package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// Test secrets are assembled at run time so that no token-shaped literal sits
// in the source, where secret scanners would (rightly) flag it.
func fake(prefix string, n int) string {
	const alphabet = "a1B2c3D4e5F6g7H8i9J0kLmNoPqRsTuVwXyZ"
	var b strings.Builder
	b.WriteString(prefix)
	for i := 0; i < n; i++ {
		b.WriteByte(alphabet[(i*7+3)%len(alphabet)])
	}
	return b.String()
}

func testRedactor(highEntropy bool, disable ...string) *redactor {
	return newRedactor("test", &OptionsV2{Redact: &RedactConfig{Enabled: true, HighEntropy: highEntropy, DisableRules: disable}})
}

func redactForTest(r *redactor, text string) string {
	return r.redactString(context.Background(), RedactionSource{Server: "test"}, text, map[string]int{}, 0)
}

func TestRedactFindsKnownSecretFormats(t *testing.T) {
	t.Parallel()

	pem := "-----BEGIN " + "RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA" + fake("", 40) + "\n-----END RSA PRIVATE KEY-----"
	jwt := "eyJ" + fake("", 20) + ".eyJ" + fake("", 30) + "." + fake("", 43)
	tests := []struct {
		name   string
		text   string
		secret string
		kind   string
	}{
		{name: "pem private key", text: "key:\n" + pem + "\nthanks", secret: "MIIEowIBAAKCAQEA", kind: "private_key"},
		{name: "truncated pem", text: "-----BEGIN OPENSSH " + "PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA" + fake("", 30), secret: "b3BlbnNzaC1rZXktdjEAAAAA", kind: "private_key"},
		{name: "aws access key", text: "id " + fake("AKIA", 0) + "IOSFODNN7EXAMPLE ok", secret: "IOSFODNN7EXAMPLE", kind: "aws_access_key"},
		{name: "aws secret key", text: "aws_secret_access_key = " + fake("", 40), secret: fake("", 40), kind: "aws_secret_key"},
		{name: "github classic", text: "use " + fake("ghp_", 36), secret: fake("ghp_", 36), kind: "github_token"},
		{name: "github fine-grained", text: fake("github_pat_", 60), secret: fake("github_pat_", 60), kind: "github_token"},
		{name: "gitlab", text: "token " + fake("glpat-", 20), secret: fake("glpat-", 20), kind: "gitlab_token"},
		{name: "anthropic", text: fake("sk-ant-api03-", 80), secret: fake("sk-ant-api03-", 80), kind: "anthropic_api_key"},
		{name: "openai project", text: "key " + fake("sk-proj-", 64), secret: fake("sk-proj-", 64), kind: "openai_api_key"},
		{name: "google api key", text: fake("AIza", 35), secret: fake("AIza", 35), kind: "google_api_key"},
		{name: "google oauth access", text: fake("ya29.", 60), secret: fake("ya29.", 60), kind: "google_oauth_token"},
		{name: "slack bot", text: fake("xoxb-", 40), secret: fake("xoxb-", 40), kind: "slack_token"},
		{name: "slack webhook", text: "post to https://hooks.slack.com/services/" + fake("T", 10) + "/" + fake("B", 10) + "/" + fake("", 24), secret: fake("", 24), kind: "slack_webhook"},
		{name: "telegram bot", text: "bot token 123456789:" + fake("AA", 33) + " here", secret: fake("AA", 33), kind: "telegram_bot_token"},
		{name: "telegram bot after an escaped newline", text: `Use this token to access the HTTP API:\n7123456789:` + fake("AA", 33) + `\nKeep it secure`, secret: fake("AA", 33), kind: "telegram_bot_token"},
		{name: "second of two telegram bots", text: "A=7123456789:" + fake("AA", 33) + " B=8123456789:" + fake("BB", 33), secret: fake("BB", 33), kind: "telegram_bot_token"},
		{name: "github token after an escaped newline", text: `see:\n` + fake("ghp_", 36), secret: fake("ghp_", 36), kind: "github_token"},
		{name: "aws key after an escaped tab", text: `id:\t` + fake("AKIA", 0) + "IOSFODNN7EXAMPLE", secret: "IOSFODNN7EXAMPLE", kind: "aws_access_key"},
		{name: "token label after an escaped newline", text: `user\nToken: hunter22secret`, secret: "hunter22secret", kind: "secret"},
		{name: "quoted value ending in an escaped newline", text: `password: "hunter22secret\n"`, secret: "hunter22secret", kind: "password"},
		{name: "repr value ending in an escaped newline", text: `{'password': 'hunter22secret\n', 'user': 'bob'}`, secret: "hunter22secret", kind: "password"},
		{name: "json fragment ending in an escaped newline", text: `log {"client_secret": "hunter22secret\n"} tail`, secret: "hunter22secret", kind: "secret"},
		{name: "env value ending in an escaped newline", text: `DB_PASSWORD="hunter22secret\n"`, secret: "hunter22secret", kind: "password"},
		{name: "quoted value with an escaped newline inside", text: `password: "Xk9\nP2qZ7"`, secret: "P2qZ7", kind: "password"},
		{name: "unquoted value with an escaped tab inside", text: `password: Qw\terty77`, secret: "erty77", kind: "password"},
		{name: "bearer after an escaped crlf", text: `hdr\r\nAuthorization: Bearer ` + fake("", 30), secret: fake("", 30), kind: "auth_header"},
		{name: "jwt", text: "Cookie: session=" + jwt, secret: jwt, kind: "jwt"},
		{name: "authorization header", text: "Authorization: Bearer " + fake("", 30), secret: fake("", 30), kind: "auth_header"},
		{name: "url credentials", text: "DATABASE=postgres://app:s3cr3t-Pa55@db.internal:5432/app", secret: "s3cr3t-Pa55", kind: "url_password"},
		{name: "env password", text: "DB_HOST=db\nDB_PASSWORD=hunter2!x\n", secret: "hunter2!x", kind: "password"},
		{name: "env export quoted", text: `export MY_API_KEY="abc 123 def"`, secret: "abc 123 def", kind: "env_secret"},
		{name: "env lower-case", text: "smtp_pass=Pa55word-1 # rotated", secret: "Pa55word-1", kind: "password"},
		{name: "prose password", text: "the password: Summer-Blue-77! ok", secret: "Summer-Blue-77!", kind: "password"},
		{name: "prose pwd", text: "login admin pwd=hunter22.", secret: "hunter22", kind: "password"},
		{name: "prose api key", text: "set api_key = zz9plural now", secret: "zz9plural", kind: "secret"},
		{name: "prose token", text: "token: abcdef0123456789", secret: "abcdef0123456789", kind: "secret"},
		{name: "fullwidth colon", text: "password：fullwidth1", secret: "fullwidth1", kind: "password"},
		{name: "zero-width before colon", text: "password\u200b: hunter22", secret: "hunter22", kind: "password"},
		{name: "yaml", text: "db:\n  password: \"quoted secret\"\n", secret: "quoted secret", kind: "password"},
		{name: "query string", text: "GET /cb?code=1&access_token=qwertyuiop&x=1", secret: "qwertyuiop", kind: "secret"},
		{name: "json fragment in prose", text: `config was {"client_secret": "cs-123456"} oops`, secret: "cs-123456", kind: "secret"},
		{name: "escaped json in a log line", text: `log: {\"password\":\"esc-aped-1\"}`, secret: "esc-aped-1", kind: "password"},
		{name: "prefixed yaml key", text: "  MYSQL_ROOT_PASSWORD: rootpw-77", secret: "rootpw-77", kind: "password"},
		{name: "camel case key", text: "userPassword: camel-pw-1", secret: "camel-pw-1", kind: "password"},
		{name: "inline env", text: "run PGPASSWORD=pgpw-123 psql", secret: "pgpw-123", kind: "password"},
		{name: "go assignment", text: `password := "go-assign-1"`, secret: "go-assign-1", kind: "password"},
		{name: "php arrow", text: `'password' => 'arrow-pw-1'`, secret: "arrow-pw-1", kind: "password"},
		{name: "spaced key", text: "API key: spaced-key-123", secret: "spaced-key-123", kind: "secret"},
		{name: "cli flag", text: "mysql --password hunter-cli-2 db", secret: "hunter-cli-2", kind: "password"},
		{name: "redis empty user", text: "redis://:redis-pw@cache:6379", secret: "redis-pw", kind: "url_password"},
		{name: "at sign in url password", text: "postgres://u:p@ss-word@db/x", secret: "p@ss-word", kind: "url_password"},
		{name: "dollar password", text: "password=$ecretP4ss", secret: "$ecretP4ss", kind: "password"},
		{name: "nbsp before colon", text: "password\u00a0: nbsp-pw-1", secret: "nbsp-pw-1", kind: "password"},
	}
	r := testRedactor(false)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactForTest(r, tt.text)
			if strings.Contains(got, tt.secret) {
				t.Fatalf("secret survived: %q", got)
			}
			if !strings.Contains(got, RedactionMarker(tt.kind)) {
				t.Fatalf("want marker %s in %q", RedactionMarker(tt.kind), got)
			}
		})
	}
}

func TestRedactLeavesOrdinaryTextAlone(t *testing.T) {
	t.Parallel()

	texts := []string{
		"commit 8f14e45fceea167a5a36dedd4bea2543 merged",
		"sha256 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"550e8400-e29b-41d4-a716-446655440000",
		"spaces/AAAAm3bFj9E/messages/kPq7QzX1aB0.kPq7QzX1aB0",
		"Build 123456 completed on 2026-08-31",
		"token count is 123",
		`path C:\\new\\tokens\\list.txt`,
		"the password was changed yesterday",
		"Token: none",
		"password: ********",
		"PASSWORD=${DB_PASSWORD}",
		"TOKEN_URL=https://auth.example.com/token",
		"PASSWORD_MIN_LENGTH=12",
		"KEYBOARD=us",
		"PWD=/home/user/project",
		"API_KEY_ID=12345",
		"https://example.com/path?page=2&sort=asc",
		"mail user@example.com about it",
		"bypass: yes",
		"compass: north",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl user@host",
		`{"nextPageToken":"CAESBggBEAEYAQ","pageSize":50}`,
		"next_page_token: CAESBggBEAEYAQ",
		"Привет, как дела?",
		"SORT_KEY=created_at",
		"PRIMARY_KEY=id",
		"AUTH_METHOD=oauth",
		`{"isSecret":"maybe","has_password":"yes"}`,
		"see the API reference: https://example.com/api",
		"Password: changed yesterday",
		"Token: valid until Friday",
	}
	for _, highEntropy := range []bool{false, true} {
		r := testRedactor(highEntropy)
		for _, text := range texts {
			if got := redactForTest(r, text); got != text {
				t.Errorf("highEntropy=%v: %q was changed to %q", highEntropy, text, got)
			}
		}
	}
}

func TestRedactHighEntropyIsOptIn(t *testing.T) {
	t.Parallel()

	text := "here is " + fake("", 32) + " for you"
	if got := redactForTest(testRedactor(false), text); got != text {
		t.Fatalf("highEntropy off changed %q to %q", text, got)
	}
	got := redactForTest(testRedactor(true), text)
	if strings.Contains(got, fake("", 32)) || !strings.Contains(got, RedactionMarker("high_entropy")) {
		t.Fatalf("highEntropy on: got %q", got)
	}
}

func TestRedactDisableRules(t *testing.T) {
	t.Parallel()

	jwt := "eyJ" + fake("", 20) + ".eyJ" + fake("", 30) + "." + fake("", 43)
	text := "a " + jwt + " password: hunter22"
	got := redactForTest(testRedactor(false, "jwt", "password"), text)
	if !strings.Contains(got, jwt) || !strings.Contains(got, "hunter22") {
		t.Fatalf("disabled rules still applied: %q", got)
	}
}

func TestRedactJSONTextIsRewrittenValueByValue(t *testing.T) {
	t.Parallel()

	r := testRedactor(false)
	tests := []struct {
		name    string
		text    string
		secrets []string
		keep    []string
	}{
		{
			name:    "message text with escaped newline",
			text:    `{"messages":[{"id":42,"text":"password: hunter22\nsee you"}]}`,
			secrets: []string{"hunter22"},
			keep:    []string{`"id":42`, `\nsee you`},
		},
		{
			name:    "unicode-escaped label",
			text:    `{"text":"\u0070assword: hunter22"}`,
			secrets: []string{"hunter22"},
		},
		{
			name:    "secret member names",
			text:    `{"client_secret":"abc123def","dbPassword":"zzz999","nextPageToken":"CAESBggB","token_count":5}`,
			secrets: []string{"abc123def", "zzz999"},
			keep:    []string{`"nextPageToken":"CAESBggB"`, `"token_count":5`},
		},
		{
			name:    "array under secret member",
			text:    `{"token":["one-secret","two-secret"]}`,
			secrets: []string{"one-secret", "two-secret"},
		},
		{
			name:    "json nested in a string",
			text:    `{"result":"{\"password\":\"hunter22\"}"}`,
			secrets: []string{"hunter22"},
		},
		{
			name:    "authorization header member",
			text:    `{"headers":{"Authorization":"Bearer abcdefgh12345678","Accept":"*/*"}}`,
			secrets: []string{"abcdefgh12345678"},
			keep:    []string{`"Accept":"*/*"`},
		},
		{
			name:    "numeric secret member",
			text:    `{"password": 12345678, "count": 12345678}`,
			secrets: []string{`"password": 12345678`},
			keep:    []string{`"count": 12345678`},
		},
		{
			name:    "dollar value under secret member",
			text:    `{"password":"$ecretP4ss"}`,
			secrets: []string{"$ecretP4ss"},
		},
		{
			name:    "object under a secret member",
			text:    `{"token":{"access":"abcdef123","refresh":"ghijkl456","type":"Bearer","expires_in":3600},"credentials":"{\"user\":\"bob\",\"value\":\"hunter2secret\"}"}`,
			secrets: []string{"abcdef123", "ghijkl456", "hunter2secret"},
			keep:    []string{`"type":"Bearer"`, `"expires_in":3600`, `bob`},
		},
		{
			name:    "json array string under a secret member",
			text:    `{"password":"[\"hunter2secret\"]"}`,
			secrets: []string{"hunter2secret"},
		},
		{
			name: "key order and spacing preserved",
			text: "{\n  \"z\": 1,\n  \"a\": \"password: hunter22\"\n}",
			keep: []string{"{\n  \"z\": 1,\n  \"a\": \"password: [REDACTED:password]\"\n}"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactForTest(r, tt.text)
			if !json.Valid([]byte(got)) {
				t.Fatalf("result is not valid JSON: %s", got)
			}
			for _, secret := range tt.secrets {
				if strings.Contains(got, secret) {
					t.Errorf("secret %q survived: %s", secret, got)
				}
			}
			for _, keep := range tt.keep {
				if !strings.Contains(got, keep) {
					t.Errorf("want %q kept in %s", keep, got)
				}
			}
		})
	}
}

func TestRedactToolResultCoversEveryTextField(t *testing.T) {
	t.Parallel()

	r := testRedactor(false)
	secret := fake("ghp_", 36)
	result := &mcp.CallToolResult{
		Content: []mcp.Content{
			mcp.NewTextContent("plain " + secret),
			&mcp.TextContent{Type: mcp.ContentTypeText, Text: "pointer " + secret},
			mcp.NewEmbeddedResource(mcp.TextResourceContents{URI: "x://1", Text: "embedded " + secret}),
			mcp.NewEmbeddedResource(mcp.BlobResourceContents{URI: "x://2", MIMEType: "text/plain", Blob: base64.StdEncoding.EncodeToString([]byte("blob " + secret))}),
			mcp.NewImageContent(base64.StdEncoding.EncodeToString([]byte(secret)), "image/png"),
		},
		RawStructuredContent: json.RawMessage(`{"b":2,"note":"` + secret + `","a":1}`),
		StructuredContent:    map[string]any{"note": secret},
	}
	request := mcp.CallToolRequest{}
	request.Params.Name = "read"
	r.toolResult(context.Background(), request, result)

	for i, content := range result.Content[:4] {
		raw, err := json.Marshal(content)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		if i == 3 {
			blob := content.(mcp.EmbeddedResource).Resource.(mcp.BlobResourceContents)
			decoded, _ := base64.StdEncoding.DecodeString(blob.Blob)
			text = string(decoded)
		}
		if strings.Contains(text, secret) || !strings.Contains(text, "[REDACTED:github_token]") {
			t.Errorf("content[%d] not redacted: %s", i, text)
		}
	}
	if image := result.Content[4].(mcp.ImageContent); image.Data != base64.StdEncoding.EncodeToString([]byte(secret)) {
		t.Error("binary image content must pass through unchanged")
	}
	if got := string(result.RawStructuredContent); got != `{"b":2,"note":"[REDACTED:github_token]","a":1}` {
		t.Errorf("raw structured content = %s", got)
	}
	if note := result.StructuredContent.(map[string]any)["note"]; note != "[REDACTED:github_token]" {
		t.Errorf("structured content note = %v", note)
	}
	wire, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), secret) {
		t.Errorf("serialized result still holds the secret: %s", wire)
	}
}

type recordingHook struct {
	mu       sync.Mutex
	findings []RedactionFinding
	sources  []RedactionSource
}

func (h *recordingHook) Replace(_ context.Context, source RedactionSource, finding RedactionFinding) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.findings = append(h.findings, finding)
	h.sources = append(h.sources, source)
	return fmt.Sprintf("<%s #%d>", finding.Kind, len(h.findings))
}

// Not parallel: it swaps the process-wide hook. Parallel tests only start
// once the sequential ones have finished, so none of them observe it.
func TestRedactionHookReceivesFindings(t *testing.T) {
	hook := &recordingHook{}
	SetRedactionHook(hook)
	t.Cleanup(func() { SetRedactionHook(nil) })

	r := testRedactor(false)
	request := mcp.CallToolRequest{}
	request.Params.Name = "get_messages"
	request.Params.Arguments = map[string]any{"chat_id": 7}
	result := &mcp.CallToolResult{Content: []mcp.Content{mcp.NewTextContent("password: hunter22 and " + fake("ghp_", 36))}}
	r.toolResult(context.Background(), request, result)

	text := result.Content[0].(mcp.TextContent).Text
	if text != "password: <password #1> and <github_token #2>" {
		t.Fatalf("text = %q", text)
	}
	if len(hook.findings) != 2 || hook.findings[0].Value != "hunter22" || hook.findings[1].Value != fake("ghp_", 36) {
		t.Fatalf("findings = %+v", hook.findings)
	}
	source := hook.sources[0]
	if source.Server != "test" || source.Method != "tools/call" || source.Name != "get_messages" || source.Arguments["chat_id"] != 7 {
		t.Fatalf("source = %+v", source)
	}
}

func TestRedactDisabledIsNil(t *testing.T) {
	t.Parallel()

	for _, options := range []*OptionsV2{nil, {}, {Redact: &RedactConfig{}}} {
		if r := newRedactor("x", options); r != nil {
			t.Fatalf("newRedactor(%+v) = %v, want nil", options, r)
		}
	}
	var r *redactor
	result := &mcp.CallToolResult{Content: []mcp.Content{mcp.NewTextContent("password: hunter22")}}
	if got := r.toolResult(context.Background(), mcp.CallToolRequest{}, result); got.Content[0].(mcp.TextContent).Text != "password: hunter22" {
		t.Fatal("nil redactor must not change results")
	}
}

func TestRedactConfigValidationAndInheritance(t *testing.T) {
	t.Parallel()

	if err := validateOptions("o", &OptionsV2{Redact: &RedactConfig{Enabled: true, DisableRules: []string{"nope"}}}); err == nil || !strings.Contains(err.Error(), "o.redact.disableRules[0]") {
		t.Fatalf("unknown rule: err = %v", err)
	}
	if err := validateOptions("o", &OptionsV2{Redact: &RedactConfig{Enabled: true, DisableRules: []string{"jwt", "password"}}}); err != nil {
		t.Fatalf("known rules rejected: %v", err)
	}

	dir := t.TempDir()
	path := dir + "/config.json"
	writeFile(t, path, `{
		"mcpProxy": {"baseURL": "http://localhost:1", "addr": ":1", "name": "p", "version": "1",
			"options": {"redact": {"enabled": true}}},
		"mcpServers": {
			"inherits": {"command": "/bin/true"},
			"overrides": {"command": "/bin/true", "options": {"redact": {"enabled": false}}}
		}
	}`)
	config, err := load(path, false, false, "", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if newRedactor("inherits", config.McpServers["inherits"].Options) == nil {
		t.Error("server without redact must inherit mcpProxy.options.redact")
	}
	if newRedactor("overrides", config.McpServers["overrides"].Options) != nil {
		t.Error("server redact block must override the inherited one")
	}
}

func TestRedactThroughProxy(t *testing.T) {
	t.Parallel()

	secret := fake("ghp_", 36)
	downstream := newRawDownstream(t)
	downstream.setTools("read")
	downstream.setResources("test://doc")
	downstream.setMethodResult("tools/call", `{"content":[{"type":"text","text":"{\"text\":\"`+secret+`\"}"}],"structuredContent":{"text":"`+secret+`"}}`)
	downstream.setMethodResult("resources/read", `{"contents":[{"uri":"test://doc","mimeType":"text/plain","text":"API_KEY=`+secret+`"}]}`)

	mcpClient, err := newMCPClient("test", &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           downstream.url,
		Options:       &OptionsV2{Redact: &RedactConfig{Enabled: true}},
	})
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()
	proxyServer := newProxyServerForTest(t)
	if err := mcpClient.addToMCPServer(t.Context(), mcp.Implementation{Name: "test"}, proxyServer); err != nil {
		t.Fatalf("addToMCPServer: %v", err)
	}

	for _, request := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"resources/read","params":{"uri":"test://doc"}}`,
	} {
		raw, err := json.Marshal(proxyServer.HandleMessage(context.Background(), []byte(request)))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), secret) || !strings.Contains(string(raw), "[REDACTED:github_token]") {
			t.Errorf("response not redacted: %s", raw)
		}
	}
}

func BenchmarkRedactLargeMessageList(b *testing.B) {
	var sb strings.Builder
	sb.WriteString(`{"messages":[`)
	for i := 0; i < 5000; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `{"id":%d,"sender":"user %d","date":"2026-10-14T10:00:00Z","text":"ordinary message number %d, see https://example.com/x/%d"}`, i, i, i, i)
	}
	sb.WriteString(`]}`)
	text := sb.String()
	r := testRedactor(true)
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = redactForTest(r, text)
	}
}

func TestRedactHookValueIsDecoded(t *testing.T) {
	hook := &recordingHook{}
	SetRedactionHook(hook)
	t.Cleanup(func() { SetRedactionHook(nil) })

	r := testRedactor(false)
	_ = redactForTest(r, `prefix {"token": "a\"b\\c-12345"} suffix`)
	if len(hook.findings) != 1 || hook.findings[0].Value != `a"b\c-12345` {
		t.Fatalf("findings = %+v", hook.findings)
	}
}

func TestRedactErrorsAndResourceLinks(t *testing.T) {
	t.Parallel()

	r := testRedactor(false)
	secret := fake("ghp_", 36)
	request := mcp.CallToolRequest{}
	request.Params.Name = "x"
	err := r.toolError(context.Background(), request, fmt.Errorf("bad credentials for %s", secret))
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error not redacted: %v", err)
	}
	result := &mcp.CallToolResult{Content: []mcp.Content{
		mcp.NewResourceLink("postgres://app:link-pw-9@db/x", "db", "token: "+secret, "text/plain"),
		mcp.ToolUseContent{Type: "tool_use", ID: "1", Name: "n", Input: map[string]any{"api_key": "in-put-key-1"}},
	}}
	r.toolResult(context.Background(), request, result)
	wire, _ := json.Marshal(result)
	for _, leaked := range []string{"link-pw-9", secret, "in-put-key-1"} {
		if strings.Contains(string(wire), leaked) {
			t.Errorf("%q survived: %s", leaked, wire)
		}
	}
}

// Overlapping findings from several rules and whitespace-free input must stay
// linear; both used to be quadratic.
func TestRedactLargeInputsStayFast(t *testing.T) {
	t.Parallel()
	if testing.Short() || raceEnabled {
		t.Skip("timing test")
	}
	inputs := []string{
		strings.Repeat("password: hunter2secret\nAKIAABCDEFGHIJKLMNOP\n", 40000),
		strings.Repeat("Ab1Cd2Ef3Gh4Ij5Kl6Mn7Op8Qr9St0,", 40000),
	}
	r := testRedactor(true)
	for _, text := range inputs {
		start := time.Now()
		_ = redactForTest(r, text)
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("%d bytes took %v", len(text), elapsed)
		}
	}
}
