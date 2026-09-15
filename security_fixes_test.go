package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/tbxark/optional-go"
)

// Regression tests for the security-review fixes. Each test names the review
// item it covers; see code_review/security-audit/zcode.md.

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// RF-001: a downstream resource template without a uriTemplate used to reach
// mcp-go's SetResourceTemplates, which dereferences the nil URITemplate and
// panics the shared process. It must be skipped instead.
func TestResourceTemplateWithoutURITemplateIsSkipped(t *testing.T) {
	t.Parallel()

	downstream := newRawDownstream(t)
	downstream.setTools("alpha")
	downstream.setResourceTemplates(
		map[string]any{"name": "missing", "description": "no uriTemplate"},
		map[string]any{"name": "explicit-null", "uriTemplate": nil},
		map[string]any{"name": "good", "uriTemplate": "test://echo/{word}"},
	)

	mcpClient, err := newMCPClient("test", &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           downstream.url,
		Options:       &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	proxyServer := newProxyServerForTest(t)
	if err := mcpClient.addToMCPServer(t.Context(), mcp.Implementation{Name: "test"}, proxyServer); err != nil {
		t.Fatalf("addToMCPServer: %v", err)
	}

	got := serverResourceTemplateNames(t, proxyServer)
	if !slices.Equal(got, []string{"good"}) {
		t.Fatalf("registered resource templates = %v, want only [good]", got)
	}
}

// serverResourceTemplateNames reads back the resource templates registered on
// the proxy-side server.
func serverResourceTemplateNames(t *testing.T, mcpServer *server.MCPServer) []string {
	t.Helper()

	resp := mcpServer.HandleMessage(context.Background(), []byte(`{
		"jsonrpc": "2.0", "id": 1, "method": "resources/templates/list", "params": {}
	}`))
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal templates/list response: %v", err)
	}
	var decoded struct {
		Result struct {
			ResourceTemplates []struct {
				Name string `json:"name"`
			} `json:"resourceTemplates"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal templates/list response: %v", err)
	}
	names := make([]string, 0, len(decoded.Result.ResourceTemplates))
	for _, template := range decoded.Result.ResourceTemplates {
		names = append(names, template.Name)
	}
	slices.Sort(names)
	return names
}

func TestToolFilterFuncPreservesCompatibility(t *testing.T) {
	t.Parallel()

	// These expectations intentionally encode the PRE-EXISTING behavior: an
	// upgrade must not start hiding tools a deployment relies on. Do not tighten
	// any of these without a major release.
	tests := []struct {
		name    string
		options *OptionsV2
		tool    string
		want    bool
	}{
		{name: "nil options allows", options: nil, tool: "any", want: true},
		{name: "nil filter allows", options: &OptionsV2{}, tool: "any", want: true},
		{name: "empty allow list still allows (legacy)", options: &OptionsV2{ToolFilter: &ToolFilterConfig{Mode: ToolFilterModeAllow}}, tool: "any", want: true},
		{name: "allow list membership", options: &OptionsV2{ToolFilter: &ToolFilterConfig{Mode: ToolFilterModeAllow, List: []string{"a"}}}, tool: "a", want: true},
		{name: "allow list exclusion", options: &OptionsV2{ToolFilter: &ToolFilterConfig{Mode: ToolFilterModeAllow, List: []string{"a"}}}, tool: "b", want: false},
		{name: "empty block list allows", options: &OptionsV2{ToolFilter: &ToolFilterConfig{Mode: ToolFilterModeBlock}}, tool: "any", want: true},
		{name: "block list excludes", options: &OptionsV2{ToolFilter: &ToolFilterConfig{Mode: ToolFilterModeBlock, List: []string{"b"}}}, tool: "b", want: false},
		{name: "unknown mode still allows (legacy)", options: &OptionsV2{ToolFilter: &ToolFilterConfig{Mode: ToolFilterMode("nonsense")}}, tool: "any", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toolFilterFunc("test", tt.options)(tt.tool); got != tt.want {
				t.Errorf("toolFilterFunc(%s)(%s) = %v, want %v", tt.name, tt.tool, got, tt.want)
			}
		})
	}
}

func TestEmptyAllowListStillExposesAllTools(t *testing.T) {
	t.Parallel()

	// Compatibility guard: an empty allow list historically meant "no filtering"
	// and still does, so an existing deployment keeps seeing every tool. The
	// surprising case is logged as a warning instead of changing behavior.
	downstream := newRawDownstream(t)
	downstream.setTools("alpha", "beta")

	mcpClient, err := newMCPClient("test", &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           downstream.url,
		Options:       &OptionsV2{ToolFilter: &ToolFilterConfig{Mode: ToolFilterModeAllow}},
	})
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	proxyServer := newProxyServerForTest(t)
	if err := mcpClient.addToMCPServer(t.Context(), mcp.Implementation{Name: "test"}, proxyServer); err != nil {
		t.Fatalf("addToMCPServer: %v", err)
	}
	if got := serverToolNames(t, proxyServer); !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Fatalf("tools with an empty allow list = %v, want [alpha beta] (legacy behavior)", got)
	}
}

func TestDownstreamExecutionMetadataIsStripped(t *testing.T) {
	t.Parallel()

	downstream := newRawDownstream(t)
	downstream.setTools("danger")
	downstream.setToolExecution(map[string]any{"taskSupport": "required"})

	mcpClient, err := newMCPClient("test", &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           downstream.url,
		Options:       &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	proxyServer := newProxyServerForTest(t)
	if err := mcpClient.addToMCPServer(t.Context(), mcp.Implementation{Name: "test"}, proxyServer); err != nil {
		t.Fatalf("addToMCPServer: %v", err)
	}

	resp := proxyServer.HandleMessage(context.Background(), []byte(`{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": {}
	}`))
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal tools/list response: %v", err)
	}
	if strings.Contains(string(raw), "taskSupport") || strings.Contains(string(raw), `"execution"`) {
		t.Fatalf("republished tool kept downstream execution metadata: %s", raw)
	}
	if !strings.Contains(string(raw), "danger") {
		t.Fatalf("the tool itself should still be advertised: %s", raw)
	}
}

// RF-003: the configured per-request bound must apply to resource reads, not
// only tool calls. A downstream that accepts a read and never answers used to
// hold the shared channel for the caller's whole deadline.
func TestResourceReadHonorsRequestTimeout(t *testing.T) {
	t.Parallel()

	downstream := newRawDownstream(t)
	downstream.setTools("alpha")
	downstream.setResources("test://hang")
	downstream.hangMethodsNamed("resources/read")

	mcpClient, err := newMCPClient("test", &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           downstream.url,
		Options:       &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	proxyServer := newProxyServerForTest(t)
	ctx := t.Context()
	if err := mcpClient.addToMCPServer(ctx, mcp.Implementation{Name: "test"}, proxyServer); err != nil {
		t.Fatalf("addToMCPServer: %v", err)
	}

	// Bound is normally set only for stdio; the forwarding logic it drives is
	// shared, so exercising it on a streamable client keeps the test in-process.
	mcpClient.requestTimeout = 250 * time.Millisecond

	callerCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err = mcpClient.readResource(callerCtx, mcp.ReadResourceRequest{
		Params: mcp.ReadResourceParams{URI: "test://hang"},
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("readResource succeeded against a downstream that never answers")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("readResource took %v; it ignored the configured request timeout", elapsed)
	}
}

// RF-003: the prompt path must be bounded the same way.
func TestPromptGetHonorsRequestTimeout(t *testing.T) {
	t.Parallel()

	downstream := newRawDownstream(t)
	downstream.setTools("alpha")
	downstream.setPrompts("hang")
	downstream.hangMethodsNamed("prompts/get")

	mcpClient, err := newMCPClient("test", &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           downstream.url,
		Options:       &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	proxyServer := newProxyServerForTest(t)
	ctx := t.Context()
	if err := mcpClient.addToMCPServer(ctx, mcp.Implementation{Name: "test"}, proxyServer); err != nil {
		t.Fatalf("addToMCPServer: %v", err)
	}
	mcpClient.requestTimeout = 250 * time.Millisecond

	callerCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp := proxyServer.HandleMessage(callerCtx, []byte(`{
		"jsonrpc": "2.0", "id": 1, "method": "prompts/get",
		"params": {"name": "hang"}
	}`))
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal prompts/get response: %v", err)
	}
	// The handler is bounded, so the call must have produced a JSON-RPC error
	// rather than blocking until the caller's 30s deadline.
	if !strings.Contains(string(raw), `"error"`) {
		t.Fatalf("prompts/get against a hanging downstream should error, got %s", raw)
	}
}

// RF-004: a credential carried in a downstream URL query must not survive into
// an error message that reaches a caller or the log.
func TestRedactURLCredentials(t *testing.T) {
	t.Parallel()

	credentialed := `Post "https://mcp.example.com/mcp?key=SECRET&token=OTHER": dial tcp: refused`
	_ = credentialed
	err := &url.Error{Op: "Post", URL: "https://mcp.example.com/mcp?key=SECRET&token=OTHER", Err: errors.New("dial tcp: refused")}

	redacted := redactURLCredentials(err)
	message := redacted.Error()
	for _, secret := range []string{"SECRET", "OTHER", "key=", "token="} {
		if strings.Contains(message, secret) {
			t.Errorf("redacted message still contains %q: %s", secret, message)
		}
	}
	if !strings.Contains(message, "mcp.example.com") {
		t.Errorf("redaction dropped too much, the host is not diagnostic: %s", message)
	}
	if !errors.Is(redacted, err) {
		t.Error("redaction must preserve the original error in the chain for errors.As/Is")
	}

	// A non-URL error is returned unchanged.
	plain := errors.New("boom")
	if got := redactURLCredentials(plain); got != plain {
		t.Errorf("plain error should pass through, got %v", got)
	}
	if got := redactURLCredentials(nil); got != nil {
		t.Errorf("nil error should stay nil, got %v", got)
	}
}

// RF-004 end to end: the error a caller receives after a downstream transport
// failure must not contain the configured query credential.
func TestToolCallErrorRedactsURLCredential(t *testing.T) {
	t.Parallel()

	const secret = "SECRETTOKEN"
	downstream := newRawDownstream(t)
	downstream.setTools("alpha")

	mcpClient, err := newMCPClient("test", &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		// The credential the docs recommend putting in the URL query.
		URL:     downstream.url + "?key=" + secret,
		Options: &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	proxyServer := newProxyServerForTest(t)
	ctx := t.Context()
	if err := mcpClient.addToMCPServer(ctx, mcp.Implementation{Name: "test"}, proxyServer); err != nil {
		t.Fatalf("addToMCPServer: %v", err)
	}

	// Take the downstream away, then call the registered tool: the transport
	// failure embeds the credentialed URL in a *url.Error.
	downstream.close()

	resp := proxyServer.HandleMessage(ctx, []byte(`{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": {"name": "alpha", "arguments": {}}
	}`))
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal tools/call response: %v", err)
	}
	message := string(raw)
	if strings.Contains(message, secret) {
		t.Fatalf("caller-visible error disclosed the URL query credential: %s", message)
	}
	if !strings.Contains(message, `"error"`) {
		t.Fatalf("a call to a dead downstream should return an error, got %s", message)
	}
}

func TestGlobalToolFilterIsNotInherited(t *testing.T) {
	t.Parallel()

	// Compatibility guard: mcpProxy.options.toolFilter is NOT applied to
	// servers. Enforcing it would newly hide tools on an existing deployment,
	// so it remains a no-op reported via a warning. This test pins that
	// behavior; changing it is a breaking change needing a major release.
	dir := t.TempDir()
	path := dir + "/config.json"
	writeFile(t, path, `{
		"mcpProxy": {
			"baseURL": "http://127.0.0.1:9090",
			"addr": ":9090",
			"name": "proxy",
			"version": "1.0.0",
			"type": "streamable-http",
			"options": {"toolFilter": {"mode": "block", "list": ["blocked"]}}
		},
		"mcpServers": {
			"inherits": {"command": "/bin/true"}
		}
	}`)

	config, err := load(path, false, true, "", 10)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	server := config.McpServers["inherits"]
	if server == nil || server.Options == nil {
		t.Fatal("server options should be defaulted after load")
	}
	if server.Options.ToolFilter != nil {
		t.Fatalf("global toolFilter must not be inherited, got %+v", server.Options.ToolFilter)
	}
	// The practical consequence: the globally "blocked" tool stays exposed.
	if got := toolFilterFunc("inherits", server.Options)("blocked"); !got {
		t.Error("legacy behavior exposes 'blocked' even with a global block list")
	}

	// A per-server filter is the supported way to restrict tools, and works.
	writeFile(t, path, `{
		"mcpProxy": {"baseURL": "http://127.0.0.1:9090","addr": ":9090","name": "proxy","version": "1.0.0","type": "streamable-http"},
		"mcpServers": {"restricted": {"command": "/bin/true", "options": {"toolFilter": {"mode": "block", "list": ["blocked"]}}}}
	}`)
	config, err = load(path, false, true, "", 10)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	restricted := config.McpServers["restricted"]
	if restricted == nil || restricted.Options == nil || restricted.Options.ToolFilter == nil {
		t.Fatal("per-server toolFilter should survive load")
	}
	if got := toolFilterFunc("restricted", restricted.Options)("blocked"); got {
		t.Error("per-server block list must still block the excluded tool")
	}
}

func TestOAuthTokenAndClientPathsDoNotAlias(t *testing.T) {
	isolateUserConfigDir(t)
	const serverName = "notion"
	const collidingName = serverName + ".client"

	tokenPath, err := oauthTokenPath(collidingName)
	if err != nil {
		t.Fatalf("oauthTokenPath: %v", err)
	}
	clientPath, err := oauthClientPath(serverName)
	if err != nil {
		t.Fatalf("oauthClientPath: %v", err)
	}
	if tokenPath == clientPath {
		t.Fatalf("token path of %q aliases the client path of %q: %s", collidingName, serverName, tokenPath)
	}

	// Distinct servers never share a file, in either direction.
	for _, name := range []string{serverName, collidingName, "a.b", "server-abc"} {
		path, err := oauthTokenPath(name)
		if err != nil {
			t.Fatalf("oauthTokenPath(%q): %v", name, err)
		}
		client, err := oauthClientPath(name)
		if err != nil {
			t.Fatalf("oauthClientPath(%q): %v", name, err)
		}
		if path == client {
			t.Errorf("token and client paths alias for %q: %s", name, path)
		}
	}

	// Round trip: each schema is written and read back intact, so neither write
	// clobbered the other's file.
	if err := saveRegisteredClient(serverName, "cid-1", "sec-1"); err != nil {
		t.Fatalf("saveRegisteredClient: %v", err)
	}
	if err := NewFileTokenStore(tokenPath).SaveToken(context.Background(), &transport.Token{AccessToken: "tok", RefreshToken: "r"}); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	clientID, secret, ok, err := loadRegisteredClient(serverName)
	if err != nil || !ok || clientID != "cid-1" || secret != "sec-1" {
		t.Fatalf("registered client after token write = (%q,%q,%v,%v), want (cid-1,sec-1,true,nil)", clientID, secret, ok, err)
	}
	token, err := NewFileTokenStore(tokenPath).GetToken(context.Background())
	if err != nil || token.AccessToken != "tok" {
		t.Fatalf("token after client write = (%v,%v), want token tok", token, err)
	}
}

// RF-007 compatibility: a client registered under the pre-fix file name is
// still found, so an existing deployment is not forced to re-authorize.
func TestLoadRegisteredClientReadsLegacyPath(t *testing.T) {
	isolateUserConfigDir(t)
	const serverName = "reviewfix-legacy"

	legacyPath, err := oauthLegacyClientPath(serverName)
	if err != nil {
		t.Fatalf("oauthLegacyClientPath: %v", err)
	}
	writeFile(t, legacyPath, `{"clientId":"legacy-id","clientSecret":"legacy-secret"}`)

	clientID, secret, ok, err := loadRegisteredClient(serverName)
	if err != nil {
		t.Fatalf("loadRegisteredClient: %v", err)
	}
	if !ok || clientID != "legacy-id" || secret != "legacy-secret" {
		t.Fatalf("legacy client = (%q,%q,%v), want (legacy-id,legacy-secret,true)", clientID, secret, ok)
	}
}

// RF-004: a URL that fails to parse must still be redacted. The first cut used
// url.Parse and silently returned the original text when parsing failed, which
// leaked the credential in exactly the malformed-URL case.
func TestRedactURLCredentialsHandlesUnparseableURL(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"http://ho st/mcp?key=PARSE_SECRET",
		"://?key=PARSE_SECRET",
		"http://user:pw@ho st/x?key=PARSE_SECRET#frag",
	} {
		urlErr := &url.Error{Op: "Post", URL: raw, Err: errors.New("boom")}
		got := redactURLCredentials(urlErr).Error()
		if strings.Contains(got, "PARSE_SECRET") {
			t.Errorf("unparseable URL %q leaked its query credential: %s", raw, got)
		}
		if strings.Contains(got, "pw@") {
			t.Errorf("unparseable URL %q leaked its userinfo: %s", raw, got)
		}
	}

	// A URL with no credential is left untouched, and the original error stays
	// in the chain so classification is preserved.
	clean := &url.Error{Op: "Post", URL: "https://mcp.example.com/mcp", Err: errors.New("boom")}
	if got := redactURLCredentials(clean); got != error(clean) {
		t.Errorf("a credential-free URL should pass through unchanged, got %v", got)
	}
}

// RF-004: the fatal startup path returns the transport error out of the startup
// errorGroup, where main logs it. It must be redacted there too.
func TestFatalStartupErrorIsRedacted(t *testing.T) {
	t.Parallel()

	urlErr := &url.Error{Op: "Post", URL: "https://mcp.example.com/mcp?key=FATAL_SECRET", Err: errors.New("refused")}

	panicIfInvalid := optional.NewField(true)

	fatal := fatalStartupError(&MCPClientConfigV2{Options: &OptionsV2{PanicIfInvalid: panicIfInvalid}}, urlErr)
	if fatal == nil {
		t.Fatal("panicIfInvalid should make the failure fatal")
	}
	if strings.Contains(fatal.Error(), "FATAL_SECRET") {
		t.Fatalf("the fatal startup error still carries the credential: %s", fatal)
	}
	var asURLErr *url.Error
	if !errors.As(fatal, &asURLErr) {
		t.Error("redaction must preserve the url.Error in the chain")
	}

	// Without panicIfInvalid the failure is not fatal at all.
	if got := fatalStartupError(&MCPClientConfigV2{Options: &OptionsV2{}}, urlErr); got != nil {
		t.Errorf("a non-fatal startup error should return nil, got %v", got)
	}
}

// RF-007 read-side: the legacy fallback must not read ANOTHER server's token
// file. The legacy client path of "X" is the token path of "X.client", so with
// no legacy client file present the token of "X.client" must be ignored rather
// than parsed as a client registration (which fails with a confusing clientId
// error and leaves "X" unable to start).
func TestLoadRegisteredClientIgnoresOtherServersToken(t *testing.T) {
	isolateUserConfigDir(t)
	const serverName = "reviewfix-owner"
	const collidingName = serverName + ".client"

	// The colliding server has a token at what is the legacy client path of
	// serverName. No legacy client file exists for serverName.
	tokenPath, err := oauthTokenPath(collidingName)
	if err != nil {
		t.Fatalf("oauthTokenPath: %v", err)
	}
	legacyPath, err := oauthLegacyClientPath(serverName)
	if err != nil {
		t.Fatalf("oauthLegacyClientPath: %v", err)
	}
	if tokenPath != legacyPath {
		t.Fatalf("precondition: expected %q and %q to coincide", tokenPath, legacyPath)
	}
	if err := NewFileTokenStore(tokenPath).SaveToken(context.Background(), &transport.Token{AccessToken: "tok"}); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}

	clientID, secret, ok, err := loadRegisteredClient(serverName)
	if err != nil {
		t.Fatalf("loadRegisteredClient must not fail on another server's token file: %v", err)
	}
	if ok || clientID != "" || secret != "" {
		t.Fatalf("loadRegisteredClient = (%q,%q,%v), want (\"\",\"\",false)", clientID, secret, ok)
	}
}

// RF-007 read-side: ignoring a token file must not also swallow a genuinely
// corrupt client file, which should still report why it could not be used.
func TestLoadRegisteredClientReportsCorruptLegacyFile(t *testing.T) {
	isolateUserConfigDir(t)
	const serverName = "reviewfix-corrupt"

	legacyPath, err := oauthLegacyClientPath(serverName)
	if err != nil {
		t.Fatalf("oauthLegacyClientPath: %v", err)
	}
	writeFile(t, legacyPath, `{"clientId":`)

	if _, _, _, err := loadRegisteredClient(serverName); err == nil {
		t.Fatal("a malformed legacy client file should surface a parse error")
	}
}

// RF-007: a server's token read must not accept another server's client
// registration. The path is shared with the pre-fix name, so a client JSON has
// no access or refresh token and must read as "no token", not as an empty one.
func TestGetTokenRejectsNonTokenFile(t *testing.T) {
	isolateUserConfigDir(t)
	// The legacy client path of "reviewfix-owner" is the token path of
	// "reviewfix-owner.client": legacyClientPath(D) == tokenPath(D+".client").
	const ownerName = "reviewfix-owner"
	const serverName = ownerName + ".client"

	path, err := oauthTokenPath(serverName)
	if err != nil {
		t.Fatalf("oauthTokenPath: %v", err)
	}
	legacyOwner, err := oauthLegacyClientPath(ownerName)
	if err != nil {
		t.Fatalf("oauthLegacyClientPath: %v", err)
	}
	if path != legacyOwner {
		t.Fatalf("precondition: expected token(%q)=%q to equal legacyClient(%q)=%q", serverName, path, ownerName, legacyOwner)
	}
	writeFile(t, path, `{"clientId":"someone-elses-client","clientSecret":"s"}`)

	if _, err := NewFileTokenStore(path).GetToken(context.Background()); !errors.Is(err, transport.ErrNoToken) {
		t.Fatalf("GetToken on a client-registration file = %v, want ErrNoToken", err)
	}
}

// RF-004: the reconnect log and the -doctor live column are further sinks for a
// transport error, which embeds the credentialed downstream URL.
func TestURLRedactionCoversReconnectAndDoctorSinks(t *testing.T) {
	t.Parallel()

	urlErr := func() error {
		return &url.Error{Op: "Post", URL: "https://mcp.example.com/mcp?key=SINK_SECRET", Err: errors.New("refused")}
	}

	// Reconnect path: the error that reaches the log must be redacted.
	downstream := newRawDownstream(t)
	downstream.setTools("alpha")
	mcpClient, err := newMCPClient("test", &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           downstream.url + "?key=SINK_SECRET",
		Options:       &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()
	if err := mcpClient.addToMCPServer(t.Context(), mcp.Implementation{Name: "test"}, newProxyServerForTest(t)); err != nil {
		t.Fatalf("addToMCPServer: %v", err)
	}
	downstream.close()
	reconnectErr := mcpClient.connect(t.Context(), mcp.Implementation{Name: "test"}, mcpClient.mcpServer)
	if reconnectErr == nil {
		t.Fatal("expected the reconnect to fail after the downstream closed")
	}
	if strings.Contains(redactURLCredentials(reconnectErr).Error(), "SINK_SECRET") {
		t.Error("the reconnect error still carries the URL query credential into the log")
	}

	// Doctor path: liveFailureMessage is printed to the report.
	if message := liveFailureMessage("test", urlErr()); strings.Contains(message, "SINK_SECRET") {
		t.Errorf("-doctor live column disclosed the URL query credential: %s", message)
	}
	if message := liveFailureMessage("test", urlErr()); !strings.Contains(message, "mcp.example.com") {
		t.Errorf("-doctor message lost the diagnostic host: %s", message)
	}
}

// RF-004: url.Error renders its URL with %q, so a credential containing a quote
// or backslash does not appear verbatim in the message. Replacing the raw URL
// silently did nothing and leaked it; redaction must rebuild the rendered form.
func TestRedactURLCredentialsHandlesQuotedSecret(t *testing.T) {
	t.Parallel()

	for _, secret := range []string{`SEC"RET`, `SEC\RET`, `SEC\"RET`, "SEC`RET"} {
		urlErr := &url.Error{Op: "Post", URL: "https://mcp.example.com/mcp?key=" + secret, Err: errors.New("refused")}
		got := redactURLCredentials(urlErr).Error()
		if strings.Contains(got, secret) {
			t.Errorf("secret %q survived redaction: %s", secret, got)
		}
		// The failure-classification chain must survive too.
		var asURLErr *url.Error
		if !errors.As(redactURLCredentials(urlErr), &asURLErr) {
			t.Errorf("errors.As(*url.Error) failed after redaction of %q", secret)
		}
	}
}

// RF-004: a transport error nested inside a wrapper (as the transports produce)
// must also be redacted, including a quoted credential.
func TestRedactURLCredentialsNestedWrapper(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("transport error: %w", &url.Error{Op: "Post", URL: `http://127.0.0.1:1/mcp?key=NEST"ED`, Err: errors.New("refused")})
	got := redactURLCredentials(wrapped).Error()
	if strings.Contains(got, `NEST"ED`) || strings.Contains(got, "key=") {
		t.Fatalf("nested transport error leaked the credential: %s", got)
	}
	if !strings.Contains(got, "transport error") || !strings.Contains(got, "127.0.0.1") {
		t.Fatalf("redaction lost the diagnostic context: %s", got)
	}
}

// RF-004: -authorize returns the first Start/Initialize failure unchanged when
// it is not an auth-required error, so main.go logs it. It must be redacted.
func TestAuthorizeInteractivelyRedactsNonAuthError(t *testing.T) {
	t.Parallel()

	urlErr := &url.Error{Op: "Post", URL: "https://mcp.example.com/mcp?key=AUTH_SECRET", Err: errors.New("refused")}
	got := authorizeInteractively(context.Background(), urlErr, "server", "http://localhost:8090/oauth/callback")
	if got == nil {
		t.Fatal("expected the non-auth error to be returned")
	}
	if strings.Contains(got.Error(), "AUTH_SECRET") {
		t.Fatalf("-authorize pass-through leaked the credential: %s", got)
	}
}

// RF-004: a malformed downstream URL in config is reported by load(); the parse
// error embeds the raw URL, so it must be redacted before it is logged/printed.
func TestValidateHTTPURLRedactsMalformedURLCredential(t *testing.T) {
	t.Parallel()

	// The second shape is the one that defeated two earlier redactor attempts:
	// net/url truncates it at the '#', leaving the credential as bare userinfo
	// with no '@' to detect.
	for _, raw := range []string{
		"http://ho st/mcp?key=CFG_SECRET",
		"https://user:SUPERSECRET#frag@example.com/mcp",
		"https://user:SUPERSECRET?@example.com/mcp",
		"//user:NOSCHEMESECRET@ho st/mcp?key=Q",
		"https://user:SUPERSECRET",
	} {
		err := validateHTTPURL("mcpServers[\"bad\"].url", raw)
		if err == nil {
			t.Errorf("%q: expected a malformed URL to be rejected", raw)
			continue
		}
		for _, secret := range []string{"CFG_SECRET", "SUPERSECRET", "NOSCHEMESECRET"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%q leaked %q: %s", raw, secret, err)
			}
		}
	}
}

// RF-004: the redactor itself must survive the truncated-userinfo shape that
// net/url produces for an unparseable URL.
func TestRedactURLStringTruncatedUserinfo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw         string
		mustNotHave []string
	}{
		{"https://user:SUPERSECRET", []string{"SUPERSECRET"}},
		{"//user:NOSCHEMESECRET@ho st/mcp?key=Q", []string{"NOSCHEMESECRET", "Q"}},
		{"https://user:SEC#frag@example.com/mcp", []string{"SEC"}},
		{"scheme://user:pass@host:8080/path?k=v#f", []string{"pass", "k=v", "f"}},
	}
	for _, tt := range tests {
		got := redactURLString(tt.raw)
		for _, secret := range tt.mustNotHave {
			if strings.Contains(got, secret) {
				t.Errorf("redactURLString(%q) = %q still contains %q", tt.raw, got, secret)
			}
		}
	}

	// A credential-free URL is returned byte-identical, and the port survives
	// when there is a path, so diagnostics stay useful.
	for _, clean := range []string{"http://127.0.0.1:9/mcp", "https://mcp.example.com/sse"} {
		if got := redactURLString(clean); got != clean {
			t.Errorf("credential-free URL changed: %q -> %q", clean, got)
		}
	}
}

// RF-004: a hostile matrix over every credential location. The redactor must
// not echo a secret from userinfo, query, or fragment - including for inputs
// that do not parse as URLs at all, which is where two earlier attempts leaked.
func TestRedactURLCredentialsDropsContaminatedReason(t *testing.T) {
	t.Parallel()

	// A malformed URL can contaminate the parser's REASON, not just the URL.
	// net/url truncates at the first '#' and then reports the credential as a
	// bad port. This mirrors the exact shape observed from
	// url.Parse("https://user:<secret>#frag@example.com/mcp"):
	//   parse "https://user:SUPERSECRET": invalid port ":SUPERSECRET" after host
	// Redacting only the URL would leave the secret in the reason, so the whole
	// reason is dropped. Built directly so the test does not depend on the
	// exact wording net/url happens to produce.
	urlErr := &url.Error{
		Op:  "parse",
		URL: "https://user:SUPERSECRET",
		Err: errors.New(`invalid port ":SUPERSECRET" after host`),
	}

	message := redactURLCredentials(urlErr).Error()
	if strings.Contains(message, "SUPERSECRET") {
		t.Fatalf("credential survived in the parse reason: %s", message)
	}
	if !strings.Contains(message, "redacted-url") {
		t.Fatalf("expected the URL to be replaced with the placeholder: %s", message)
	}
	// The parse error must still be reachable for callers that inspect it.
	var asURLErr *url.Error
	if !errors.As(redactURLCredentials(urlErr), &asURLErr) {
		t.Error("errors.As(*url.Error) must still succeed")
	}
}

func TestRedactURLStringCredentialLocations(t *testing.T) {
	t.Parallel()

	secrets := []string{"S3CRET", `S3"C`, `S3\C`, "S3\nC", "S3%C", "S3@C", "S3#C", "S3?C", "ключ", "S3 C"}
	// Only userinfo, query and fragment are credential locations; the path is
	// preserved on purpose because it is diagnostic and not a secret slot.
	templates := []string{
		"https://h/p?key=%s",
		"https://u:%s@h/p",
		"https://h/p?key=%s&key=y",
		"https://u:%s",
		"//u:%s@ho st/p?k=1",
		"https://h/p?a=%s#%s",
		"https://%s@h",
		"https://h:%s/p",
		"https://h/p#%s",
		"https://h/p?%s",
	}
	for _, secret := range secrets {
		for _, template := range templates {
			raw := strings.ReplaceAll(template, "%s", secret)
			if got := redactURLString(raw); strings.Contains(got, secret) {
				t.Errorf("template %q leaked secret %q -> %q", template, secret, got)
			}
		}
	}

	// Inputs that do not parse are replaced wholesale, never echoed.
	for _, raw := range []string{"not a url at all", ":://", "user:SUPERSECRET", "ho st", "//u:S@ho st/p"} {
		got := redactURLString(raw)
		if strings.Contains(got, "SUPERSECRET") || strings.Contains(got, "S@") {
			t.Errorf("unparseable %q leaked: %q", raw, got)
		}
		if got != redactedURLPlaceholder {
			t.Errorf("unparseable %q should become the placeholder, got %q", raw, got)
		}
	}

	// Credential-free URLs stay byte-identical so diagnostics remain useful.
	for _, clean := range []string{"http://127.0.0.1:9/mcp", "https://mcp.example.com/sse", "https://h:8443/p"} {
		if got := redactURLString(clean); got != clean {
			t.Errorf("clean URL changed: %q -> %q", clean, got)
		}
	}
}

// RF-008: an empty access token is not usable, and the local diagnostic must
// not report it as healthy (the daemon's getValidToken would refuse it).
func TestCheckOAuthTokenRejectsEmptyAccessToken(t *testing.T) {
	isolateUserConfigDir(t)
	const refreshOnly = "reviewfix-refresh-only"
	const emptyAccess = "reviewfix-empty-access"
	const valid = "reviewfix-valid"

	conf := &MCPClientConfigV2{URL: "https://mcp.example.com/mcp", OAuth: &OAuthClientConfig{}}

	writeTestToken(t, refreshOnly, &transport.Token{RefreshToken: "r"})
	res := checkServerAuth(refreshOnly, conf)
	if res.ok {
		t.Errorf("a token with no access token must not be reported ok, got %+v", res)
	}
	if !strings.Contains(res.status, "MISSING ACCESS TOKEN") {
		t.Errorf("status should explain the missing access token, got %q", res.status)
	}

	writeTestToken(t, emptyAccess, &transport.Token{AccessToken: "", ExpiresAt: time.Now().Add(time.Hour)})
	res = checkServerAuth(emptyAccess, conf)
	if res.ok {
		t.Errorf("a token with an empty access token must not be reported ok, got %+v", res)
	}

	// Control: a real unexpired token is still ok.
	writeTestToken(t, valid, &transport.Token{AccessToken: "a", ExpiresAt: time.Now().Add(time.Hour)})
	res = checkServerAuth(valid, conf)
	if !res.ok {
		t.Errorf("an unexpired token with an access token should be ok, got %+v", res)
	}
}
