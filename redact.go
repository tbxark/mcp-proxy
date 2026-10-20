package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
)

// Secret redaction for downstream results.
//
// When options.redact.enabled is set, the text a downstream returns (tool
// results, resource contents, prompt messages) is scanned for credentials
// before it reaches the MCP client, and every finding is replaced. The scan is
// pattern based: vendor token formats, PEM private keys, URL userinfo,
// .env-style assignments and password/token key-value pairs, plus an opt-in
// high-entropy heuristic. It cannot recognise a secret that has no shape (a
// password mentioned in plain prose), nor one that is encoded or split across
// messages; see docs/CONFIGURATION.md.

// RedactionSource identifies the request whose result is being redacted.
type RedactionSource struct {
	// Server is the mcpServers key of the downstream.
	Server string
	// Method is the MCP method: "tools/call", "resources/read" or "prompts/get".
	Method string
	// Name is the tool name, resource URI or prompt name.
	Name string
	// Arguments are the request arguments (tools and prompts). They must be
	// treated as read-only.
	Arguments map[string]any
}

// RedactionFinding is one detected secret.
type RedactionFinding struct {
	// Kind names the rule that matched, e.g. "github_token" or "password".
	Kind string
	// Value is the matched secret text.
	Value string
}

// RedactionHook decides what replaces each finding. The default writes a
// "[REDACTED:<kind>]" marker. A build that wants more (keeping the value
// somewhere, counting findings) can install its own hook with
// SetRedactionHook from an init function in a separate file, without
// changing this one.
//
// Replace is called synchronously while a result is being rewritten, possibly
// from many requests at once, so an implementation must be safe for concurrent
// use and should be fast.
type RedactionHook interface {
	Replace(ctx context.Context, source RedactionSource, finding RedactionFinding) string
}

// RedactionMarker is the default replacement for a finding.
func RedactionMarker(kind string) string {
	return "[REDACTED:" + kind + "]"
}

type markerHook struct{}

func (markerHook) Replace(_ context.Context, _ RedactionSource, finding RedactionFinding) string {
	return RedactionMarker(finding.Kind)
}

var redactionHook RedactionHook = markerHook{}

// SetRedactionHook installs the hook used for every redaction; nil restores
// the default. Call it during program initialisation, before the proxy serves
// requests.
func SetRedactionHook(hook RedactionHook) {
	if hook == nil {
		hook = markerHook{}
	}
	redactionHook = hook
}

// redactionRule finds one kind of secret in text.
type redactionRule struct {
	kind string
	re   *regexp.Regexp
	// groups lists the capture groups that may hold the secret; the first
	// non-empty one is used. Empty means the whole match.
	groups []int
	// prefilter holds lower-case substrings at least one of which must occur
	// in the text for the rule to run at all. Empty means always run.
	prefilter []string
	// precheck, when set, is a cheap scan that must pass for the rule to run.
	// It gets the text and its lower-cased form.
	precheck func(text, lowered string) bool
	// accept validates a candidate and may narrow it or change its kind. It
	// receives the whole text, the submatch indexes and the selected span.
	accept func(text string, match []int, start, end int) (newStart, newEnd int, kind string, ok bool)
	// optIn marks rules that only run when enabled explicitly (highEntropy).
	optIn bool
	// jsonEscaped marks a value written inside a JSON string literal; the hook
	// gets it decoded.
	jsonEscaped bool
}

// passwordKeyWords and secretKeyWords classify key names; see secretKeyKind.
var (
	passwordKeyWords = []string{"password", "passwd", "passphrase"}
	secretKeyWords   = []string{
		"secret", "clientsecret", "apikey", "apisecret", "apitoken", "accesstoken",
		"refreshtoken", "authtoken", "idtoken", "bearertoken", "sessiontoken",
		"privatekey", "secretkey", "secretaccesskey", "credential", "credentials",
	}
	// exactSecretKeys are only secret as a whole key, never as a suffix:
	// "pagetoken" or "bypass" are not credentials.
	exactPasswordKeys = []string{"pwd", "pass", "pin"}
	exactSecretKeys   = []string{"token"}
	// headerKeys hold a whole credential header value ("Bearer ...").
	headerKeys = []string{"authorization", "proxyauthorization", "cookie", "setcookie"}
)

// normalizeKey lower-cases a key name and drops separators, so "client_secret",
// "clientSecret" and "client-secret" compare equal.
func normalizeKey(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	for _, r := range key {
		switch r {
		case '_', '-', '.', ' ':
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// secretKeyKind reports whether a key name (a JSON member name, a YAML key) is
// one whose value is a credential, and which kind it is.
func secretKeyKind(key string) (string, bool) {
	normalized := normalizeKey(key)
	if normalized == "" || len(normalized) > 64 {
		return "", false
	}
	if slices.Contains(headerKeys, normalized) {
		return "auth_header", true
	}
	// "isSecret", "has_password": flags about a secret, not the secret.
	if words := splitKeyWords(key); len(words) > 1 && (words[0] == "IS" || words[0] == "HAS") {
		return "", false
	}
	if slices.Contains(exactPasswordKeys, normalized) {
		return "password", true
	}
	// "db_pass", "dbPwd": pass and pwd count as the last word of a longer
	// name, but not inside one ("bypass").
	if words := splitKeyWords(key); len(words) > 1 && slices.Contains(exactPasswordKeys, strings.ToLower(words[len(words)-1])) {
		return "password", true
	}
	if slices.Contains(exactSecretKeys, normalized) {
		return "secret", true
	}
	for _, word := range passwordKeyWords {
		if strings.HasSuffix(normalized, word) {
			return "password", true
		}
	}
	for _, word := range secretKeyWords {
		if strings.HasSuffix(normalized, word) {
			return "secret", true
		}
	}
	return "", false
}

// envSecretWords are the words of an environment-variable name that mark its
// value as a credential.
var envSecretWords = map[string]string{
	"PASSWORD": "password", "PASSWD": "password", "PASS": "password", "PASSPHRASE": "password",
	"SECRET": "env_secret", "TOKEN": "env_secret", "APIKEY": "env_secret", "KEY": "env_secret",
	"CREDENTIAL": "env_secret", "CREDENTIALS": "env_secret", "AUTH": "env_secret",
	"DSN": "env_secret", "PRIVATE": "env_secret",
}

// envNonSecretWords, as the last word of a name, mark a value that describes a
// secret rather than holding it (TOKEN_URL, PASSWORD_MIN_LENGTH, KEY_ID).
var envNonSecretWords = map[string]bool{
	"URL": true, "URI": true, "ENDPOINT": true, "HOST": true, "PATH": true, "FILE": true,
	"DIR": true, "TYPE": true, "MODE": true, "ENABLED": true, "ENABLE": true, "EXPIRY": true,
	"EXPIRES": true, "TTL": true, "LENGTH": true, "SIZE": true, "ALGORITHM": true, "ALG": true,
	"NAME": true, "ID": true, "HEADER": true, "PREFIX": true, "PORT": true, "USER": true,
	"USERNAME": true, "LIMIT": true, "COUNT": true, "TIMEOUT": true, "VERSION": true,
	"FORMAT": true, "FIELD": true, "PARAM": true, "SCOPE": true, "SCOPES": true,
	"REGION": true, "ISSUER": true, "AUDIENCE": true, "HINT": true, "METHOD": true,
	"PROVIDER": true, "STRATEGY": true, "BACKEND": true,
}

// envNonSecretKeyQualifiers, right before a final KEY, name a data key rather
// than a key credential (SORT_KEY, PRIMARY_KEY, CACHE_KEY).
var envNonSecretKeyQualifiers = map[string]bool{
	"SORT": true, "PRIMARY": true, "CACHE": true, "PARTITION": true, "FOREIGN": true,
	"ROUTING": true, "HASH": true, "LOOKUP": true, "MAP": true, "OBJECT": true,
	"IDEMPOTENCY": true, "DEDUP": true, "GROUP": true, "SHORTCUT": true, "HOT": true,
}

// splitKeyWords splits FOO_BAR, foo.bar, foo-bar and fooBar into upper-case
// words.
func splitKeyWords(key string) []string {
	words := make([]string, 0, 4)
	var current []rune
	flush := func() {
		if len(current) > 0 {
			words = append(words, strings.ToUpper(string(current)))
			current = current[:0]
		}
	}
	runes := []rune(key)
	for i, r := range runes {
		if r == '_' || r == '.' || r == '-' {
			flush()
			continue
		}
		if i > 0 && unicode.IsUpper(r) && unicode.IsLower(runes[i-1]) {
			flush()
		}
		current = append(current, r)
	}
	flush()
	return words
}

// envKeyKind classifies an environment-style variable name.
func envKeyKind(key string) (string, bool) {
	words := splitKeyWords(key)
	if len(words) == 0 {
		return "", false
	}
	if upper := strings.ToUpper(key); upper == "PWD" || upper == "OLDPWD" {
		return "", false
	}
	if envNonSecretWords[words[len(words)-1]] || slices.Contains(words, "PUBLIC") {
		return "", false
	}
	if n := len(words); n > 1 && words[n-1] == "KEY" && envNonSecretKeyQualifiers[words[n-2]] {
		return "", false
	}
	for _, word := range words {
		if kind, ok := envSecretWords[word]; ok {
			return kind, true
		}
	}
	return "", false
}

// placeholderValues are values that stand for a credential without being one.
var placeholderValues = map[string]bool{
	"none": true, "null": true, "nil": true, "true": true, "false": true, "undefined": true,
	"empty": true, "n/a": true, "na": true, "redacted": true, "hidden": true, "required": true,
	"optional": true, "unknown": true, "yes": true, "no": true, "not": true, "set": true,
	"unset": true, "expired": true, "invalid": true, "missing": true, "string": true,
	"example": true, "placeholder": true, "your_token": true, "your_password": true,
	"changed": true, "reset": true, "updated": true, "valid": true, "sent": true,
	"enabled": true, "disabled": true, "generated": true, "rotated": true, "revoked": true,
}

// placeholderReference matches a variable reference or a named slot:
// "$TOKEN", "${DB_PASSWORD}", "<your-token>". "$ecretP4ss" is a password.
var placeholderReference = regexp.MustCompile(`^(?:\$\{?[A-Z_][A-Z0-9_]*\}?|\$\{[A-Za-z_][A-Za-z0-9_.]*\}|<[A-Za-z_ \-]+>)$`)

// isPlaceholderValue reports whether a candidate value is obviously not a real
// secret: too short, a variable reference, a template, a mask, or a marker this
// proxy wrote itself.
func isPlaceholderValue(value string) bool {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) < 3 {
		return true
	}
	lower := strings.ToLower(value)
	if placeholderValues[lower] {
		return true
	}
	if strings.Contains(value, "[REDACTED") {
		return true
	}
	if placeholderReference.MatchString(value) || strings.HasPrefix(value, "{{") || strings.HasPrefix(value, "%(") {
		return true
	}
	first, _ := utf8.DecodeRuneInString(value)
	if strings.Trim(value, string(first)) == "" && strings.ContainsRune("*x.•#-_", unicode.ToLower(first)) {
		return true // "*****", "xxxx", "...."
	}
	return false
}

// trimTrailingPunctuation drops sentence punctuation that a greedy unquoted
// value picked up ("password: hunter2." or "(token: abc)").
func trimTrailingPunctuation(text string, start, end int) int {
	for end > start && strings.ContainsRune(".,;:)]}", rune(text[end-1])) {
		end--
	}
	return end
}

// keyValueAccept validates a key-value match whose key is in group 1 and
// whose value is the selected span.
func keyValueAccept(classify func(string) (string, bool), quotedGroups int) func(string, []int, int, int) (int, int, string, bool) {
	return func(text string, match []int, start, end int) (int, int, string, bool) {
		key := text[match[2]:match[3]]
		kind, ok := classify(key)
		if !ok {
			return 0, 0, "", false
		}
		// Unquoted values (the last group) can carry trailing punctuation.
		last := len(match)/2 - 1
		if quotedGroups < last && match[2*last] == start {
			end = trimTrailingPunctuation(text, start, end)
		}
		value := text[start:end]
		if isPlaceholderValue(value) {
			return 0, 0, "", false
		}
		// "pwd" is also the working directory (PWD=/home/user).
		if normalizeKey(key) == "pwd" && (strings.HasPrefix(value, "/") || strings.HasPrefix(value, "~")) {
			return 0, 0, "", false
		}
		return start, end, kind, true
	}
}

// proseKeyKind classifies the key of a "key: value" pair found in free text.
// It is narrower than secretKeyKind: pagination cursors and similar
// "*_token" names are common in API output and are not credentials.
func proseKeyKind(key string) (string, bool) {
	normalized := normalizeKey(key)
	for _, word := range []string{"page", "cursor", "sync", "next", "csrf", "continuation"} {
		if strings.Contains(normalized, word) {
			return "", false
		}
	}
	return secretKeyKind(key)
}

// valueAccept rejects placeholders for rules whose whole span is the value.
func valueAccept(_ string, _ []int, start, end int) (int, int, string, bool) {
	return start, end, "", true
}

func placeholderAccept(text string, _ []int, start, end int) (int, int, string, bool) {
	if isPlaceholderValue(text[start:end]) {
		return 0, 0, "", false
	}
	return start, end, "", true
}

// zeroWidth matches the invisible characters sometimes used to break up a
// label ("pass\u200bword:").
const zeroWidth = `[\x{200B}-\x{200D}\x{2060}\x{FEFF}]*`

// builtinRedactionRules run in order; when two findings overlap, the earlier
// rule wins. Specific formats therefore come before generic key-value rules,
// and the entropy heuristic is last.
var builtinRedactionRules = []*redactionRule{
	{
		kind:      "private_key",
		re:        regexp.MustCompile(`-----BEGIN[ A-Z0-9]*PRIVATE KEY(?: BLOCK)?-----(?:[\s\S]*?-----END[ A-Z0-9]*PRIVATE KEY(?: BLOCK)?-----|[\s\S]*)`),
		prefilter: []string{"private key"},
	},
	{
		kind:      "aws_access_key",
		re:        regexp.MustCompile(`\b((?:AKIA|ASIA|ABIA|ACCA)[A-Z0-9]{16})\b`),
		groups:    []int{1},
		prefilter: []string{"akia", "asia", "abia", "acca"},
	},
	{
		kind:      "aws_secret_key",
		re:        regexp.MustCompile(`(?i)aws_?secret_?(?:access_?)?key["']?\s*[:=]\s*["']?([A-Za-z0-9/+=]{40})(?:[^A-Za-z0-9/+=]|$)`),
		groups:    []int{1},
		prefilter: []string{"aws"},
	},
	{
		kind:      "github_token",
		re:        regexp.MustCompile(`\b((?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})\b`),
		groups:    []int{1},
		prefilter: []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"},
	},
	{
		kind:      "gitlab_token",
		re:        regexp.MustCompile(`\b((?:glpat|gloas|gldt|glrt|glrtr|glcbt|glptt|glft|glimt|glagent|glsoat|glffct|glwt)-[A-Za-z0-9_.\-]{20,}|GR1348941[A-Za-z0-9_\-]{20,})`),
		groups:    []int{1},
		prefilter: []string{"glpat-", "gloas-", "gldt-", "glrt-", "glrtr-", "glcbt-", "glptt-", "glft-", "glimt-", "glagent-", "glsoat-", "glffct-", "glwt-", "gr1348941"},
	},
	{
		kind:      "anthropic_api_key",
		re:        regexp.MustCompile(`\b(sk-ant-[A-Za-z0-9]{2,12}-[A-Za-z0-9_\-]{20,})`),
		groups:    []int{1},
		prefilter: []string{"sk-ant-"},
	},
	{
		kind:      "openai_api_key",
		re:        regexp.MustCompile(`\b(sk-(?:proj|svcacct|admin)-[A-Za-z0-9_\-]{20,}|sk-[A-Za-z0-9]{32,})\b`),
		groups:    []int{1},
		prefilter: []string{"sk-"},
	},
	{
		kind:      "google_api_key",
		re:        regexp.MustCompile(`\b(AIza[0-9A-Za-z_\-]{35})`),
		groups:    []int{1},
		prefilter: []string{"aiza"},
	},
	{
		kind:      "google_oauth_token",
		re:        regexp.MustCompile(`\b(ya29\.[A-Za-z0-9_\-]{20,}|GOCSPX-[A-Za-z0-9_\-]{20,}|1//0[A-Za-z0-9_\-]{40,})`),
		groups:    []int{1},
		prefilter: []string{"ya29.", "gocspx-", "1//0"},
	},
	{
		kind:      "slack_token",
		re:        regexp.MustCompile(`\b(xox[abposre]-[A-Za-z0-9\-]{10,}|xapp-\d-[A-Za-z0-9\-]{10,})`),
		groups:    []int{1},
		prefilter: []string{"xox", "xapp-"},
	},
	{
		kind:      "slack_webhook",
		re:        regexp.MustCompile(`(https://hooks\.slack\.com/(?:services|workflows|triggers)/[A-Za-z0-9_/\-]{20,})`),
		groups:    []int{1},
		prefilter: []string{"hooks.slack.com"},
	},
	{
		kind:     "telegram_bot_token",
		re:       regexp.MustCompile(`\b(?:bot)?(\d{6,16}:[A-Za-z0-9_\-]{35})(?:[^A-Za-z0-9_\-]|$)`),
		groups:   []int{1},
		precheck: hasDigitColonRun,
	},
	{
		kind:      "stripe_key",
		re:        regexp.MustCompile(`\b((?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,})`),
		groups:    []int{1},
		prefilter: []string{"_live_", "_test_"},
	},
	{
		kind:      "jwt",
		re:        regexp.MustCompile(`\b(eyJ[A-Za-z0-9_\-]{8,}\.eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,})`),
		groups:    []int{1},
		prefilter: []string{"eyj"},
	},
	{
		kind:      "auth_header",
		re:        regexp.MustCompile(`(?i)\b(?:proxy-)?authorization["']?\s*[:=]\s*["']?(?:bearer|basic|token|digest)\s+([A-Za-z0-9._~+/=\-]{8,})`),
		groups:    []int{1},
		prefilter: []string{"authorization"},
	},
	{
		kind:     "url_password",
		re:       regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9+.\-]{1,30}://[^\s:/?#@"'<>]*:([^\s/?#"'<>]+)@`),
		groups:   []int{1},
		precheck: hasURLUserinfo,
		accept:   placeholderAccept,
	},
	{
		// KEY=value lines of .env files and shell exports.
		kind:      "env_secret",
		re:        regexp.MustCompile(`(?m)^[ \t]*(?:export[ \t]+)?([A-Za-z_][A-Za-z0-9_.\-]{0,127})[ \t]*=[ \t]*(?:"((?:[^"\\\r\n]|\\.)*)"|'([^'\r\n]*)'|([^\s"'#][^\r\n]*?))[ \t]*(?:[ \t]#[^\r\n]*)?\r?$`),
		groups:    []int{2, 3, 4},
		prefilter: []string{"pass", "secret", "token", "key", "auth", "dsn", "credential", "private"},
		precheck:  func(text, _ string) bool { return strings.Contains(text, "=") },
		accept:    keyValueAccept(envKeyKind, 3),
	},
	{
		// "password: x", "api_key = x", "token=x", "password := x" in prose,
		// YAML, INI, code and query strings. The key may carry any prefix
		// (MYSQL_ROOT_PASSWORD, userPassword); proseKeyKind classifies it.
		kind: "secret",
		re: regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_.\-])([A-Za-z0-9_.\-]{0,64}?(?:password|passwd|pwd|passphrase|pass|secret|api[_\-. ]?key|apikey|token|private[_\-]?key|secret[_\-]?key))` +
			zeroWidth + `["']?[ \t\x{00A0}]*(?::=|=>|[:=]|：)[ \t\x{00A0}]*(?:"([^"\r\n]{1,})"|'([^'\r\n]{1,})'|([^\s"'<>&]{1,}))`),
		groups:    []int{2, 3, 4},
		prefilter: []string{"pass", "pwd", "secret", "key", "token"},
		precheck:  func(text, _ string) bool { return strings.ContainsAny(text, ":=：") },
		accept:    keyValueAccept(proseKeyKind, 3),
	},
	{
		// Command-line flags: --password x, --token=x.
		kind:      "secret",
		re:        regexp.MustCompile(`(?:^|\s)--?((?:db-|mysql-|admin-)?(?:password|passwd|pass|token|api-key|apikey|secret|client-secret))(?:=|[ \t]+)([^\s"'-][^\s"']*)`),
		groups:    []int{2},
		prefilter: []string{"--", "-pass", "-token", "-secret", "-api"},
		accept:    keyValueAccept(proseKeyKind, 0),
	},
	{
		// "key": "value" fragments in text that is not itself valid JSON.
		kind: "secret",
		// Also matches escaped JSON inside a string or log line
		// ({\"password\":\"x\"}); the value then stops at the next backslash.
		re:          regexp.MustCompile(`\\?"([A-Za-z0-9_.\- ]{1,64})\\?"\s*:\s*(?:\\"([^"\\\r\n]+)|"((?:[^"\\\r\n]|\\.)+)")`),
		groups:      []int{2, 3},
		prefilter:   []string{"\":", "\": ", "\" :"},
		accept:      keyValueAccept(proseKeyKind, 3),
		jsonEscaped: true,
	},
	{
		kind:     "high_entropy",
		re:       regexp.MustCompile(`[A-Za-z0-9+/_\-]{24,}={0,2}`),
		precheck: hasLongTokenRun,
		accept:   highEntropyAccept,
		optIn:    true,
	},
}

func isTokenByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// hasDigitColonRun reports whether text has a digit, a colon and 35 token
// characters in a row, the shape of a Telegram bot token.
func hasDigitColonRun(text, _ string) bool {
	for i := 1; i < len(text); i++ {
		if text[i] != ':' || text[i-1] < '0' || text[i-1] > '9' {
			continue
		}
		run := 0
		for j := i + 1; j < len(text) && isTokenByte(text[j]); j++ {
			run++
		}
		if run >= 35 {
			return true
		}
	}
	return false
}

// hasURLUserinfo reports whether some "://" is followed by an "@" before the
// URL's host part ends.
func hasURLUserinfo(text, _ string) bool {
	for offset := 0; ; {
		i := strings.Index(text[offset:], "://")
		if i < 0 {
			return false
		}
		start := offset + i + 3
		for j := start; j < len(text); j++ {
			c := text[j]
			if c == '@' {
				return true
			}
			if c == '/' || c == ' ' || c == '\n' || c == '\t' || c == '"' || c == '?' || c == '#' {
				break
			}
		}
		offset = start
	}
}

// hasLongTokenRun reports whether text has 24 base64-ish characters in a row.
func hasLongTokenRun(text, _ string) bool {
	run := 0
	for i := 0; i < len(text); i++ {
		if c := text[i]; isTokenByte(c) || c == '+' || c == '/' {
			run++
			if run >= 24 {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}

// entropyContextWindow bounds how far highEntropyAccept looks around a token.
const entropyContextWindow = 256

// highEntropyAccept keeps a token that looks randomly generated: mixed case
// and digits, enough Shannon entropy, and not part of a path or URL (resource
// names such as "spaces/AAAA/messages/BBBB" are long and random too).
func highEntropyAccept(text string, _ []int, start, end int) (int, int, string, bool) {
	token := text[start:end]
	if start > 0 && strings.ContainsRune("/.@\\", rune(text[start-1])) {
		return 0, 0, "", false
	}
	if end < len(text) && strings.ContainsRune("/@\\", rune(text[end])) {
		return 0, 0, "", false
	}
	// A lower-case word between slashes marks a path or resource name
	// ("spaces/AAAA/messages/BBBB"), which base64 almost never produces.
	if strings.Contains(token, "/") {
		for segment := range strings.SplitSeq(token, "/") {
			if len(segment) >= 3 && strings.IndexFunc(segment, func(r rune) bool { return r < 'a' || r > 'z' }) < 0 {
				return 0, 0, "", false
			}
		}
	}
	// Look around only within a bounded window, so text without whitespace
	// cannot make each candidate scan the whole input.
	windowStart := max(0, start-entropyContextWindow)
	windowEnd := min(len(text), end+entropyContextWindow)
	wordStart := windowStart + strings.LastIndexAny(text[windowStart:start], " \t\r\n\"'") + 1
	wordEnd := windowEnd
	if i := strings.IndexAny(text[end:windowEnd], " \t\r\n\"'"); i >= 0 {
		wordEnd = end + i
	}
	if strings.Contains(text[wordStart:wordEnd], "://") {
		return 0, 0, "", false
	}
	// SSH public keys ("ssh-ed25519 AAAAC3...") are long base64 but public.
	before := strings.TrimRight(text[windowStart:start], " \t")
	if i := strings.LastIndexAny(before, " \t\r\n\"'") + 1; strings.HasPrefix(before[i:], "ssh-") ||
		strings.HasPrefix(before[i:], "ecdsa-sha2-") || strings.HasPrefix(before[i:], "sk-ssh-") || strings.HasPrefix(before[i:], "sk-ecdsa-") {
		return 0, 0, "", false
	}
	var upper, lower, digit bool
	for _, r := range token {
		switch {
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= '0' && r <= '9':
			digit = true
		}
	}
	if !upper || !lower || !digit {
		return 0, 0, "", false // hex digests, lower-case ids, words
	}
	if shannonEntropy(token) < 4.0 {
		return 0, 0, "", false
	}
	return start, end, "", true
}

func shannonEntropy(s string) float64 {
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	entropy := 0.0
	n := float64(len(s))
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		entropy -= p * math.Log2(p)
	}
	return entropy
}

// redactionRuleKinds lists every built-in rule kind, for config validation.
func redactionRuleKinds() []string {
	kinds := make([]string, 0, len(builtinRedactionRules))
	for _, rule := range builtinRedactionRules {
		if !slices.Contains(kinds, rule.kind) {
			kinds = append(kinds, rule.kind)
		}
	}
	// Kinds that rules emit through accept rather than declare.
	for _, extra := range []string{"password"} {
		if !slices.Contains(kinds, extra) {
			kinds = append(kinds, extra)
		}
	}
	return kinds
}

// redactor applies the enabled rules for one downstream.
type redactor struct {
	server   string
	rules    []*redactionRule
	disabled map[string]bool
}

// newRedactor returns nil when redaction is off for this server, so callers
// can use a nil check (every method is nil-safe).
func newRedactor(server string, options *OptionsV2) *redactor {
	if options == nil || options.Redact == nil || !options.Redact.Enabled {
		return nil
	}
	r := &redactor{server: server, disabled: make(map[string]bool)}
	for _, kind := range options.Redact.DisableRules {
		r.disabled[kind] = true
	}
	for _, rule := range builtinRedactionRules {
		if rule.optIn && !options.Redact.HighEntropy {
			continue
		}
		r.rules = append(r.rules, rule)
	}
	return r
}

type finding struct {
	start, end  int
	kind        string
	priority    int
	jsonEscaped bool
}

// value returns the secret as the hook should see it: decoded when it was
// written inside a JSON string literal.
func (f finding) value(text string) string {
	raw := text[f.start:f.end]
	if f.jsonEscaped && strings.Contains(raw, "\\") {
		var decoded string
		if json.Unmarshal([]byte(`"`+raw+`"`), &decoded) == nil {
			return decoded
		}
	}
	return raw
}

// maskEscapedBreaks returns text with each escaped line break or tab (\n, \r,
// \t written as a backslash and a letter, as in JSON, a log or a repr that was
// not decoded) replaced by two NUL bytes, so it has the same length. NUL is no
// word character, so the rules see a boundary before what follows ("API:\n123:AA…"
// left none between n and the digit), and it is no line break, quote or
// backslash, so a quoted value that contains one still ends at its quote. An
// escaped backslash (\\n) is left alone.
func maskEscapedBreaks(text string) string {
	if !strings.Contains(text, "\\") {
		return text
	}
	b := []byte(text)
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' {
			continue
		}
		j := i
		for j < len(b) && b[j] == '\\' {
			j++
		}
		if (j-i)%2 == 1 && j < len(b) && (b[j] == 'n' || b[j] == 'r' || b[j] == 't') {
			b[j-1], b[j] = 0, 0
		}
		i = j - 1
	}
	return string(b)
}

// find returns the non-overlapping findings in text, ordered by position. The
// rules run on text with its escaped line breaks masked (see maskEscapedBreaks),
// which keeps every position, so the findings cut the original text.
func (r *redactor) find(text string) []finding {
	if text == "" {
		return nil
	}
	text = maskEscapedBreaks(text)
	var lowered string
	var kept []finding
	for priority, rule := range r.rules {
		if len(rule.prefilter) > 0 {
			if lowered == "" {
				lowered = strings.ToLower(text)
			}
			hit := false
			for _, needle := range rule.prefilter {
				if strings.Contains(lowered, needle) {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
		}
		if rule.precheck != nil {
			if lowered == "" {
				lowered = strings.ToLower(text)
			}
			if !rule.precheck(text, lowered) {
				continue
			}
		}
		var ruleFindings []finding
		for _, match := range rule.re.FindAllStringSubmatchIndex(text, -1) {
			start, end := match[0], match[1]
			if len(rule.groups) > 0 {
				start, end = -1, -1
				for _, group := range rule.groups {
					if 2*group+1 < len(match) && match[2*group] >= 0 && match[2*group+1] > match[2*group] {
						start, end = match[2*group], match[2*group+1]
						break
					}
				}
				if start < 0 {
					continue
				}
			}
			kind := rule.kind
			accept := rule.accept
			if accept == nil {
				accept = valueAccept
			}
			newStart, newEnd, acceptedKind, ok := accept(text, match, start, end)
			if !ok || newEnd <= newStart {
				continue
			}
			if acceptedKind != "" {
				kind = acceptedKind
			}
			if r.disabled[kind] {
				continue
			}
			ruleFindings = append(ruleFindings, finding{start: newStart, end: newEnd, kind: kind, priority: priority, jsonEscaped: rule.jsonEscaped})
		}
		kept = mergeFindings(kept, ruleFindings)
	}
	return kept
}

// mergeFindings adds a lower-priority rule's findings to kept, dropping those
// that overlap a finding already kept. Both inputs are sorted by position and
// free of overlaps, so one linear pass suffices.
func mergeFindings(kept, added []finding) []finding {
	if len(added) == 0 {
		return kept
	}
	if len(kept) == 0 {
		return added
	}
	merged := make([]finding, 0, len(kept)+len(added))
	i := 0
	for _, f := range added {
		for i < len(kept) && kept[i].end <= f.start {
			merged = append(merged, kept[i])
			i++
		}
		if i < len(kept) && kept[i].start < f.end {
			continue // overlaps a higher-priority finding
		}
		if n := len(merged); n > 0 && merged[n-1].end > f.start {
			continue
		}
		merged = append(merged, f)
	}
	return append(merged, kept[i:]...)
}

// redactText replaces every finding in text. It reports whether anything
// changed and how many findings there were, by kind.
func (r *redactor) redactText(ctx context.Context, source RedactionSource, text string, stats map[string]int) string {
	findings := r.find(text)
	if len(findings) == 0 {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	last := 0
	for _, f := range findings {
		b.WriteString(text[last:f.start])
		b.WriteString(redactionHook.Replace(ctx, source, RedactionFinding{Kind: f.kind, Value: f.value(text)}))
		stats[f.kind]++
		last = f.end
	}
	b.WriteString(text[last:])
	return b.String()
}

// maxJSONDepth bounds how far JSON nested inside JSON strings is followed.
const maxJSONDepth = 5

// redactString redacts one string. Text that is a JSON document is rewritten
// value by value, so escaped content ("\n", "\u043f") is matched in its
// decoded form and member names give context ("password": "...").
func (r *redactor) redactString(ctx context.Context, source RedactionSource, text string, stats map[string]int, depth int) string {
	if depth < maxJSONDepth && looksLikeJSON(text) {
		if out, ok := r.rewriteJSON(ctx, source, []byte(text), stats, depth+1, ""); ok {
			return string(out)
		}
	}
	return r.redactText(ctx, source, text, stats)
}

func looksLikeJSON(text string) bool {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) < 2 {
		return false
	}
	first, last := trimmed[0], trimmed[len(trimmed)-1]
	return ((first == '{' && last == '}') || (first == '[' && last == ']')) && json.Valid([]byte(trimmed))
}

// redactJSONValue redacts one decoded JSON string value. secretKind is set
// when the value sits under a secret member (directly or nested): it is then
// replaced as a whole, or, when it is itself JSON, rewritten with everything
// inside treated as secret.
func (r *redactor) redactJSONValue(ctx context.Context, source RedactionSource, secretKind, value string, stats map[string]int, depth int) string {
	if secretKind != "" && !r.disabled[secretKind] {
		if looksLikeJSON(value) && depth < maxJSONDepth {
			if out, ok := r.rewriteJSON(ctx, source, []byte(value), stats, depth+1, secretKind); ok {
				return string(out)
			}
		}
		if !isPlaceholderValue(value) {
			stats[secretKind]++
			return redactionHook.Replace(ctx, source, RedactionFinding{Kind: secretKind, Value: value})
		}
		return value
	}
	return r.redactString(ctx, source, value, stats, depth)
}

// nestedSafeKeys are members inside a secret object that describe the secret
// rather than hold it ({"token": {"type": "Bearer", "expires_in": 3600}}).
var nestedSafeKeys = []string{"user", "username", "login", "email", "type", "tokentype", "scope", "scopes",
	"expires", "expiresin", "expiresat", "expiry", "issuer", "name", "id", "kind"}

// redactJSON rewrites the string values of a valid JSON document in place,
// leaving everything else (key order, numbers, whitespace) byte-for-byte
// intact. ok is false if data is not valid JSON.
func (r *redactor) redactJSON(ctx context.Context, source RedactionSource, data []byte, stats map[string]int, depth int) ([]byte, bool) {
	if !json.Valid(data) {
		return nil, false
	}
	return r.rewriteJSON(ctx, source, data, stats, depth, "")
}

// rewriteJSON is redactJSON for data already known to be valid JSON.
// inherited is the secret kind of the member the whole document sits under,
// or "" at the top level.
func (r *redactor) rewriteJSON(ctx context.Context, source RedactionSource, data []byte, stats map[string]int, depth int, inherited string) ([]byte, bool) {
	type frame struct {
		object    bool
		expectKey bool
		key       string
		// inherited is the secret kind of an enclosing secret member: every
		// value below it is secret unless its own name says otherwise.
		inherited string
	}
	stack := make([]frame, 0, 8)
	currentKey := func() string {
		if len(stack) == 0 {
			return ""
		}
		return stack[len(stack)-1].key
	}
	// secretKind is the kind of the value at the current position, or "".
	secretKind := func() string {
		if len(stack) == 0 {
			return inherited
		}
		top := stack[len(stack)-1]
		if kind, ok := secretKeyKind(top.key); ok {
			return kind
		}
		if top.inherited != "" && !slices.Contains(nestedSafeKeys, normalizeKey(top.key)) {
			return top.inherited
		}
		return ""
	}
	var out bytes.Buffer
	last := 0
	for i := 0; i < len(data); {
		switch data[i] {
		case '{':
			stack = append(stack, frame{object: true, expectKey: true, inherited: secretKind()})
			i++
		case '[':
			// Elements of an array inherit the member name it is stored under.
			stack = append(stack, frame{key: currentKey(), inherited: secretKind()})
			i++
		case '}', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			i++
		case ',':
			if len(stack) > 0 && stack[len(stack)-1].object {
				stack[len(stack)-1].expectKey = true
			}
			i++
		case ':':
			if len(stack) > 0 {
				stack[len(stack)-1].expectKey = false
			}
			i++
		case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			// A number under a secret member (a numeric PIN or password) is
			// replaced by the marker string; the member's type changes, but
			// the value does not leak.
			end := i + 1
			for end < len(data) && strings.IndexByte("0123456789+-.eE", data[end]) >= 0 {
				end++
			}
			if kind := secretKind(); kind != "" && !r.disabled[kind] && end-i >= 3 {
				stats[kind]++
				out.Write(data[last:i])
				out.Write(encodeJSONString(redactionHook.Replace(ctx, source, RedactionFinding{Kind: kind, Value: string(data[i:end])})))
				last = end
			}
			i = end
		case '"':
			end := jsonStringEnd(data, i)
			literal := data[i:end]
			decoded, err := decodeJSONString(literal)
			if err != nil {
				return nil, false
			}
			if len(stack) > 0 && stack[len(stack)-1].object && stack[len(stack)-1].expectKey {
				stack[len(stack)-1].key = decoded
			} else {
				redacted := r.redactJSONValue(ctx, source, secretKind(), decoded, stats, depth)
				if redacted != decoded {
					out.Write(data[last:i])
					out.Write(encodeJSONString(redacted))
					last = end
				}
			}
			i = end
		default:
			i++
		}
	}
	if last == 0 {
		return data, true
	}
	out.Write(data[last:])
	return out.Bytes(), true
}

// jsonStringEnd returns the index just past the closing quote of the string
// literal starting at data[start]. data is known to be valid JSON.
func jsonStringEnd(data []byte, start int) int {
	for i := start + 1; i < len(data); i++ {
		switch data[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return len(data)
}

func decodeJSONString(literal []byte) (string, error) {
	if !bytes.ContainsRune(literal, '\\') {
		return string(literal[1 : len(literal)-1]), nil
	}
	var s string
	err := json.Unmarshal(literal, &s)
	return s, err
}

func encodeJSONString(s string) []byte {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(s)
	return bytes.TrimRight(b.Bytes(), "\n")
}

// maxRedactedBlobBytes bounds how large a base64 blob with a text MIME type
// is decoded for scanning. Larger blobs pass through unscanned.
const maxRedactedBlobBytes = 8 << 20

// textMIMEType reports whether a blob of this type is text worth scanning.
func textMIMEType(mimeType string) bool {
	mimeType = strings.ToLower(strings.TrimSpace(strings.SplitN(mimeType, ";", 2)[0]))
	if strings.HasPrefix(mimeType, "text/") || strings.HasSuffix(mimeType, "+json") || strings.HasSuffix(mimeType, "+xml") {
		return true
	}
	switch mimeType {
	case "application/json", "application/xml", "application/yaml", "application/x-yaml",
		"application/toml", "application/x-sh", "application/javascript", "application/x-pem-file",
		"application/x-www-form-urlencoded":
		return true
	}
	return false
}

func (r *redactor) redactBlob(ctx context.Context, source RedactionSource, blob, mimeType string, stats map[string]int) string {
	if !textMIMEType(mimeType) || base64.StdEncoding.DecodedLen(len(blob)) > maxRedactedBlobBytes {
		return blob
	}
	decoded, err := base64.StdEncoding.DecodeString(blob)
	if err != nil || !utf8.Valid(decoded) {
		return blob
	}
	redacted := r.redactString(ctx, source, string(decoded), stats, 0)
	if redacted == string(decoded) {
		return blob
	}
	return base64.StdEncoding.EncodeToString([]byte(redacted))
}

func (r *redactor) redactResourceContents(ctx context.Context, source RedactionSource, contents mcp.ResourceContents, stats map[string]int) mcp.ResourceContents {
	switch v := contents.(type) {
	case mcp.TextResourceContents:
		v.Text = r.redactString(ctx, source, v.Text, stats, 0)
		return v
	case *mcp.TextResourceContents:
		if v != nil {
			v.Text = r.redactString(ctx, source, v.Text, stats, 0)
		}
		return v
	case mcp.BlobResourceContents:
		v.Blob = r.redactBlob(ctx, source, v.Blob, v.MIMEType, stats)
		return v
	case *mcp.BlobResourceContents:
		if v != nil {
			v.Blob = r.redactBlob(ctx, source, v.Blob, v.MIMEType, stats)
		}
		return v
	}
	return contents
}

func (r *redactor) redactContent(ctx context.Context, source RedactionSource, content mcp.Content, stats map[string]int) mcp.Content {
	switch v := content.(type) {
	case mcp.TextContent:
		v.Text = r.redactString(ctx, source, v.Text, stats, 0)
		return v
	case *mcp.TextContent:
		if v != nil {
			v.Text = r.redactString(ctx, source, v.Text, stats, 0)
		}
		return v
	case mcp.EmbeddedResource:
		v.Resource = r.redactResourceContents(ctx, source, v.Resource, stats)
		return v
	case *mcp.EmbeddedResource:
		if v != nil {
			v.Resource = r.redactResourceContents(ctx, source, v.Resource, stats)
		}
		return v
	case mcp.ResourceLink:
		r.redactLink(ctx, source, &v, stats)
		return v
	case *mcp.ResourceLink:
		if v != nil {
			r.redactLink(ctx, source, v, stats)
		}
		return v
	case mcp.ToolResultContent:
		for i, nested := range v.Content {
			v.Content[i] = r.redactContent(ctx, source, nested, stats)
		}
		return v
	case *mcp.ToolResultContent:
		if v != nil {
			for i, nested := range v.Content {
				v.Content[i] = r.redactContent(ctx, source, nested, stats)
			}
		}
		return v
	case mcp.ToolUseContent:
		v.Input = r.redactAny(ctx, source, v.Input, stats)
		return v
	case *mcp.ToolUseContent:
		if v != nil {
			v.Input = r.redactAny(ctx, source, v.Input, stats)
		}
		return v
	}
	return content
}

func (r *redactor) redactLink(ctx context.Context, source RedactionSource, link *mcp.ResourceLink, stats map[string]int) {
	link.URI = r.redactText(ctx, source, link.URI, stats)
	link.Title = r.redactText(ctx, source, link.Title, stats)
	link.Description = r.redactText(ctx, source, link.Description, stats)
}

// redactAny redacts an arbitrary decoded JSON value by rewriting its
// encoding. A value that cannot be re-encoded is dropped rather than passed on
// unscanned.
func (r *redactor) redactAny(ctx context.Context, source RedactionSource, value any, stats map[string]int) any {
	if value == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	redacted, ok := r.redactJSON(ctx, source, raw, stats, 1)
	if !ok {
		return nil
	}
	if bytes.Equal(redacted, raw) {
		return value
	}
	var decoded any
	if json.Unmarshal(redacted, &decoded) != nil {
		return nil
	}
	return decoded
}

// redactError rewrites an error message from the downstream. The original
// error stays reachable through Unwrap for classification.
func (r *redactor) redactError(ctx context.Context, source RedactionSource, err error) error {
	if r == nil || err == nil {
		return err
	}
	stats := make(map[string]int)
	message := r.redactText(ctx, source, err.Error(), stats)
	if len(stats) == 0 {
		return err
	}
	r.logFindings(source, stats)
	return &redactedError{msg: message, err: err}
}

// redactStructured rewrites structuredContent. mcp-go serialises the raw bytes
// it received when they are present, so those are what gets rewritten; the
// decoded value is refreshed from the result to stay consistent.
func (r *redactor) redactStructured(ctx context.Context, source RedactionSource, result *mcp.CallToolResult, stats map[string]int) {
	raw := []byte(result.RawStructuredContent)
	if len(raw) == 0 {
		if result.StructuredContent == nil {
			return
		}
		encoded, err := json.Marshal(result.StructuredContent)
		if err != nil {
			return
		}
		raw = encoded
	}
	redacted, ok := r.redactJSON(ctx, source, raw, stats, 1)
	if !ok || bytes.Equal(redacted, raw) {
		return
	}
	var decoded any
	if err := json.Unmarshal(redacted, &decoded); err != nil {
		// Never fall back to the unredacted value.
		result.StructuredContent = nil
		result.RawStructuredContent = nil
		return
	}
	result.StructuredContent = decoded
	result.RawStructuredContent = json.RawMessage(redacted)
}

// logFindings records what was redacted, never the values.
func (r *redactor) logFindings(source RedactionSource, stats map[string]int) {
	if len(stats) == 0 {
		return
	}
	kinds := make([]string, 0, len(stats))
	total := 0
	for kind, count := range stats {
		kinds = append(kinds, kind)
		total += count
	}
	slices.Sort(kinds)
	slog.Info("Redacted secrets from downstream result", "client", r.server, "method", source.Method, "name", source.Name, "count", total, "kinds", kinds)
}

func (r *redactor) toolSource(request mcp.CallToolRequest) RedactionSource {
	return RedactionSource{Server: r.server, Method: "tools/call", Name: request.Params.Name, Arguments: request.GetArguments()}
}

// toolError redacts an error returned for a tool call.
func (r *redactor) toolError(ctx context.Context, request mcp.CallToolRequest, err error) error {
	if r == nil {
		return err
	}
	return r.redactError(ctx, r.toolSource(request), err)
}

// resourceError redacts an error returned for a resource read.
func (r *redactor) resourceError(ctx context.Context, request mcp.ReadResourceRequest, err error) error {
	if r == nil {
		return err
	}
	return r.redactError(ctx, RedactionSource{Server: r.server, Method: "resources/read", Name: request.Params.URI}, err)
}

// promptError redacts an error returned for a prompt.
func (r *redactor) promptError(ctx context.Context, request mcp.GetPromptRequest, err error) error {
	if r == nil {
		return err
	}
	return r.redactError(ctx, RedactionSource{Server: r.server, Method: "prompts/get", Name: request.Params.Name}, err)
}

// toolResult redacts a tool call result in place.
func (r *redactor) toolResult(ctx context.Context, request mcp.CallToolRequest, result *mcp.CallToolResult) *mcp.CallToolResult {
	if r == nil || result == nil {
		return result
	}
	source := r.toolSource(request)
	stats := make(map[string]int)
	for i, content := range result.Content {
		result.Content[i] = r.redactContent(ctx, source, content, stats)
	}
	r.redactStructured(ctx, source, result, stats)
	r.logFindings(source, stats)
	return result
}

// resourceContents redacts the contents of a resource read in place.
func (r *redactor) resourceContents(ctx context.Context, request mcp.ReadResourceRequest, contents []mcp.ResourceContents) []mcp.ResourceContents {
	if r == nil {
		return contents
	}
	source := RedactionSource{Server: r.server, Method: "resources/read", Name: request.Params.URI, Arguments: request.Params.Arguments}
	stats := make(map[string]int)
	for i, content := range contents {
		contents[i] = r.redactResourceContents(ctx, source, content, stats)
	}
	r.logFindings(source, stats)
	return contents
}

// promptResult redacts the messages of a prompt in place.
func (r *redactor) promptResult(ctx context.Context, request mcp.GetPromptRequest, result *mcp.GetPromptResult) *mcp.GetPromptResult {
	if r == nil || result == nil {
		return result
	}
	arguments := make(map[string]any, len(request.Params.Arguments))
	for key, value := range request.Params.Arguments {
		arguments[key] = value
	}
	source := RedactionSource{Server: r.server, Method: "prompts/get", Name: request.Params.Name, Arguments: arguments}
	stats := make(map[string]int)
	result.Description = r.redactText(ctx, source, result.Description, stats)
	for i := range result.Messages {
		result.Messages[i].Content = r.redactContent(ctx, source, result.Messages[i].Content, stats)
	}
	r.logFindings(source, stats)
	return result
}
