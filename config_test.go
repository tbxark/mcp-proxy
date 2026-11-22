package main

import (
	"encoding/json"
	nethttp "net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validTestConfig() *Config {
	return &Config{
		McpProxy: &MCPProxyConfigV2{
			BaseURL: "http://localhost:9090",
			Addr:    ":9090",
			Name:    "test-proxy",
			Version: "test",
			Type:    MCPServerTypeStreamable,
			Options: &OptionsV2{},
		},
		McpServers: map[string]*MCPClientConfigV2{
			"stdio": {
				Command: "test-server",
				Options: &OptionsV2{},
			},
		},
	}
}

func TestParseMCPClientConfigV2(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		config  *MCPClientConfigV2
		wantErr string
		want    any
	}{
		{name: "infer stdio", config: &MCPClientConfigV2{Command: "server"}, want: &StdioMCPClientConfig{}},
		{name: "infer streamable", config: &MCPClientConfigV2{URL: "https://example.com/mcp"}, want: &StreamableMCPClientConfig{}},
		{name: "explicit sse", config: &MCPClientConfigV2{TransportType: MCPClientTypeSSE, URL: "https://example.com/sse"}, want: &SSEMCPClientConfig{}},
		{name: "explicit streamable", config: &MCPClientConfigV2{TransportType: MCPClientTypeStreamable, URL: "https://example.com/mcp"}, want: &StreamableMCPClientConfig{}},
		{name: "null", config: nil, wantErr: "server config is null"},
		{name: "ambiguous", config: &MCPClientConfigV2{Command: "server", URL: "https://example.com"}, wantErr: "mutually exclusive"},
		{name: "missing endpoint", config: &MCPClientConfigV2{}, wantErr: "command or url is required"},
		{name: "unknown transport", config: &MCPClientConfigV2{TransportType: "websocket", URL: "https://example.com"}, wantErr: "unsupported transportType"},
		{name: "stdio missing command", config: &MCPClientConfigV2{TransportType: MCPClientTypeStdio}, wantErr: "command is required"},
		{name: "oauth on stdio", config: &MCPClientConfigV2{Command: "server", OAuth: &OAuthClientConfig{}}, wantErr: "oauth is not supported"},
		{name: "negative timeout", config: &MCPClientConfigV2{TransportType: MCPClientTypeStreamable, URL: "https://example.com", Timeout: -1}, wantErr: "timeout cannot be negative"},
		{name: "negative stdio timeout", config: &MCPClientConfigV2{Command: "server", Timeout: -1}, wantErr: "timeout cannot be negative"},
		{name: "stdio keeps timeout", config: &MCPClientConfigV2{Command: "server", Timeout: Duration(30 * time.Second)}, want: &StdioMCPClientConfig{Timeout: Duration(30 * time.Second)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMCPClientConfigV2(tt.config)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse config: %v", err)
			}
			switch tt.want.(type) {
			case *StdioMCPClientConfig:
				stdio, ok := got.(*StdioMCPClientConfig)
				if !ok {
					t.Fatalf("type = %T, want *StdioMCPClientConfig", got)
				}
				if want := tt.want.(*StdioMCPClientConfig); stdio.Timeout != want.Timeout {
					t.Errorf("Timeout = %v, want %v", time.Duration(stdio.Timeout), time.Duration(want.Timeout))
				}
			case *SSEMCPClientConfig:
				if _, ok := got.(*SSEMCPClientConfig); !ok {
					t.Fatalf("type = %T, want *SSEMCPClientConfig", got)
				}
			case *StreamableMCPClientConfig:
				if _, ok := got.(*StreamableMCPClientConfig); !ok {
					t.Fatalf("type = %T, want *StreamableMCPClientConfig", got)
				}
			}
		})
	}
}

func TestValidateConfig(t *testing.T) {
	t.Parallel()

	t.Run("valid", func(t *testing.T) {
		if err := validateConfig(validTestConfig()); err != nil {
			t.Fatalf("validate config: %v", err)
		}
	})

	t.Run("public OAuth redirect with callbackListen", func(t *testing.T) {
		config := validTestConfig()
		config.McpServers["remote"] = &MCPClientConfigV2{
			URL:     "https://example.com/mcp",
			OAuth:   &OAuthClientConfig{RedirectURI: "https://gateway.example.net/oauth/callback", CallbackListen: "127.0.0.1:8090"},
			Options: &OptionsV2{},
		}
		if err := validateConfig(config); err != nil {
			t.Fatalf("validate config: %v", err)
		}
	})

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "invalid base URL", mutate: func(c *Config) { c.McpProxy.BaseURL = "localhost:9090" }, wantErr: "absolute http(s) URL"},
		{name: "unknown proxy type", mutate: func(c *Config) { c.McpProxy.Type = "websocket" }, wantErr: "mcpProxy.type"},
		{name: "empty token", mutate: func(c *Config) { c.McpServers["stdio"].Options.AuthTokens = []string{""} }, wantErr: "authTokens[0]"},
		{name: "unknown filter mode", mutate: func(c *Config) {
			c.McpServers["stdio"].Options.ToolFilter = &FilterConfig{Mode: "permit", List: []string{"tool"}}
		}, wantErr: "toolFilter.mode"},
		{name: "empty allow list", mutate: func(c *Config) {
			c.McpServers["stdio"].Options.ToolFilter = &FilterConfig{Mode: FilterModeAllow}
		}, wantErr: "toolFilter.list cannot be empty in allow mode"},
		{name: "proxy-level disabled", mutate: func(c *Config) {
			c.McpProxy.Options = &OptionsV2{Disabled: true}
		}, wantErr: "disabled is only valid per server"},
		{name: "invalid remote URL", mutate: func(c *Config) {
			c.McpServers["stdio"] = &MCPClientConfigV2{URL: "://bad", Options: &OptionsV2{}}
		}, wantErr: "is invalid"},
		{name: "non-loopback OAuth redirect", mutate: func(c *Config) {
			c.McpServers["stdio"] = &MCPClientConfigV2{
				URL:     "https://example.com/mcp",
				OAuth:   &OAuthClientConfig{RedirectURI: "http://0.0.0.0:8090/oauth/callback"},
				Options: &OptionsV2{},
			}
		}, wantErr: "loopback"},
		{name: "public OAuth redirect without callbackListen", mutate: func(c *Config) {
			c.McpServers["stdio"] = &MCPClientConfigV2{
				URL:     "https://example.com/mcp",
				OAuth:   &OAuthClientConfig{RedirectURI: "https://gateway.example.net/oauth/callback"},
				Options: &OptionsV2{},
			}
		}, wantErr: `.oauth.redirectUri: redirect URI "https://gateway.example.net/oauth/callback" must use http`},
		{name: "callbackListen without redirectUri", mutate: func(c *Config) {
			c.McpServers["stdio"] = &MCPClientConfigV2{
				URL:     "https://example.com/mcp",
				OAuth:   &OAuthClientConfig{CallbackListen: "127.0.0.1:8090"},
				Options: &OptionsV2{},
			}
		}, wantErr: "callbackListen requires redirectUri"},
		{name: "public callbackListen", mutate: func(c *Config) {
			c.McpServers["stdio"] = &MCPClientConfigV2{
				URL:     "https://example.com/mcp",
				OAuth:   &OAuthClientConfig{RedirectURI: "https://gateway.example.net/oauth/callback", CallbackListen: "0.0.0.0:8090"},
				Options: &OptionsV2{},
			}
		}, wantErr: ".oauth.callbackListen: callback listen address must use localhost or a loopback IP"},
		{name: "secret without client ID", mutate: func(c *Config) {
			c.McpServers["stdio"] = &MCPClientConfigV2{
				URL:     "https://example.com/mcp",
				OAuth:   &OAuthClientConfig{ClientSecret: "secret"},
				Options: &OptionsV2{},
			}
		}, wantErr: "clientSecret requires clientId"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := validTestConfig()
			tt.mutate(config)
			err := validateConfig(config)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateServerName(t *testing.T) {
	t.Parallel()

	// "<server>/<token>" is documented in docs/USAGE.md as a way to carry a
	// token in the route, so multi-segment names have to keep working.
	valid := []string{"fetch", "amap-maps", "my_server", "server.v1", "笔记", "fetch/secret-token"}
	for _, name := range valid {
		if err := validateServerName(name); err != nil {
			t.Errorf("validateServerName(%q) = %v, want nil", name, err)
		}
	}

	// A space is not merely ugly: http.ServeMux reads the text before it as an
	// HTTP method, so registering the route panics.
	invalid := []string{
		"", ".", "..", "../etc", "a/../b", "a//b", "/leading", "trailing/",
		`a\b`, "{id}", "tab\there", "my server", "trailing ", "no\u00a0break",
	}
	for _, name := range invalid {
		if err := validateServerName(name); err == nil {
			t.Errorf("validateServerName(%q) = nil, want error", name)
		}
	}
}

// A name with a path separator escapes the proxy's base path, so validation
// has to reject it before the route is ever built.
func TestValidateConfigRejectsRouteEscapingName(t *testing.T) {
	t.Parallel()

	config := validTestConfig()
	config.McpServers["../escape"] = &MCPClientConfigV2{Command: "server", Options: &OptionsV2{}}
	err := validateConfig(config)
	if err == nil || !strings.Contains(err.Error(), "../escape") {
		t.Fatalf("validate config error = %v, want rejection of escaping name", err)
	}
}

// newConfProvider used to set Timeout on http.DefaultClient, leaking the
// -http-timeout flag onto every other user of the default client.
func TestNewConfProviderLeavesDefaultClientUntouched(t *testing.T) {
	timeout := nethttp.DefaultClient.Timeout
	if _, err := newConfProvider("https://example.com/config.json", false, false, "", 10); err != nil {
		t.Fatalf("new conf provider: %v", err)
	}
	if nethttp.DefaultClient.Timeout != timeout {
		t.Fatalf("http.DefaultClient.Timeout = %v, want unchanged %v", nethttp.DefaultClient.Timeout, timeout)
	}
}

// Defaults: an omitted mcpProxy.type means streamable-http, and a url-only
// server means a streamable-http downstream.
func TestLoadDefaultsToStreamableHTTP(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{
		"mcpProxy": {
			"baseURL": "http://localhost:9090",
			"addr": ":9090",
			"name": "test",
			"version": "test"
		},
		"mcpServers": {
			"remote": {"url": "https://example.com/mcp"}
		}
	}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	config, err := load(path, false, false, "", 10)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if config.McpProxy.Type != MCPServerTypeStreamable {
		t.Errorf("mcpProxy.type = %q, want %q", config.McpProxy.Type, MCPServerTypeStreamable)
	}
	parsed, err := parseMCPClientConfigV2(config.McpServers["remote"])
	if err != nil {
		t.Fatalf("parse remote server: %v", err)
	}
	if _, ok := parsed.(*StreamableMCPClientConfig); !ok {
		t.Errorf("url-only server parsed as %T, want *StreamableMCPClientConfig", parsed)
	}
}

func TestLoadRejectsInvalidServerConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{
		"mcpProxy": {
			"baseURL": "http://localhost:9090",
			"addr": ":9090",
			"name": "test",
			"version": "test",
			"type": "streamable-http"
		},
		"mcpServers": {
			"broken": {"transportType": "streamable-http"}
		}
	}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	_, err := load(path, false, false, "", 10)
	if err == nil || !strings.Contains(err.Error(), `mcpServers["broken"]`) {
		t.Fatalf("load error = %v, want broken server context", err)
	}
}

func TestDurationUnmarshal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		json    string
		want    time.Duration
		wantErr bool
	}{
		{name: "duration string", json: `"30s"`, want: 30 * time.Second},
		{name: "minutes", json: `"1m30s"`, want: 90 * time.Second},
		{name: "null is unset", json: `null`, want: 0},
		// A bare number reads as seconds to most people but decodes as
		// nanoseconds, so it is rejected rather than silently misapplied.
		{name: "bare number rejected", json: `30000000000`, wantErr: true},
		{name: "zero number rejected", json: `0`, wantErr: true},
		{name: "bad string", json: `"soon"`, wantErr: true},
		{name: "bad type", json: `true`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got Duration
			err := json.Unmarshal([]byte(tt.json), &got)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Unmarshal(%s) = %v, want error", tt.json, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal(%s): %v", tt.json, err)
			}
			if time.Duration(got) != tt.want {
				t.Errorf("Unmarshal(%s) = %v, want %v", tt.json, time.Duration(got), tt.want)
			}
		})
	}
}

func TestValidateDuration(t *testing.T) {
	t.Parallel()

	if err := validateDuration("timeout", -1); err == nil {
		t.Fatal("validateDuration(-1) = nil, want an error")
	}
	for _, ok := range []Duration{0, Duration(time.Millisecond), Duration(30 * time.Second)} {
		if err := validateDuration("timeout", ok); err != nil {
			t.Errorf("validateDuration(%v) = %v, want nil", time.Duration(ok), err)
		}
	}
}

// The keepalive period and the readiness grace period are configurable, and
// both fall back to a sane default.
func TestDurationOptionDefaults(t *testing.T) {
	t.Parallel()

	if got := (&OptionsV2{}).pingInterval(); got != defaultPingInterval {
		t.Errorf("unset pingInterval = %v, want %v", got, defaultPingInterval)
	}
	if got := (&OptionsV2{PingInterval: Duration(5 * time.Second)}).pingInterval(); got != 5*time.Second {
		t.Errorf("configured pingInterval = %v, want 5s", got)
	}
	if got := (&MCPProxyConfigV2{}).startupGrace(); got != defaultStartupGracePeriod {
		t.Errorf("unset startupGracePeriod = %v, want %v", got, defaultStartupGracePeriod)
	}
	if got := (&MCPProxyConfigV2{StartupGracePeriod: Duration(time.Second)}).startupGrace(); got != time.Second {
		t.Errorf("configured startupGracePeriod = %v, want 1s", got)
	}
	if got := (&OptionsV2{}).reconnectInterval(); got != defaultReconnectInterval {
		t.Errorf("unset reconnectInterval = %v, want %v", got, defaultReconnectInterval)
	}
	if got := (&OptionsV2{ReconnectInterval: Duration(time.Second)}).reconnectInterval(); got != time.Second {
		t.Errorf("configured reconnectInterval = %v, want 1s", got)
	}
}

// Auto-reconnect is opt-in: the default has to stay fail-once, because the
// health endpoints exist precisely so an orchestrator can restart the proxy.
func TestAutoReconnectDefaultsOff(t *testing.T) {
	t.Parallel()

	if (&OptionsV2{}).AutoReconnect.OrElse(false) {
		t.Error("autoReconnect must default to false")
	}
}

// The timeout used to be parsed but never applied to sse clients.
func TestSSEConfigCarriesTimeout(t *testing.T) {
	t.Parallel()

	conf := &MCPClientConfigV2{
		TransportType: MCPClientTypeSSE,
		URL:           "https://example.com/sse",
		Timeout:       Duration(7 * time.Second),
	}
	parsed, err := parseMCPClientConfigV2(conf)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	sse, ok := parsed.(*SSEMCPClientConfig)
	if !ok {
		t.Fatalf("type = %T, want *SSEMCPClientConfig", parsed)
	}
	if time.Duration(sse.Timeout) != 7*time.Second {
		t.Errorf("sse timeout = %v, want 7s", time.Duration(sse.Timeout))
	}
	if got := len(sseClientOptions(sse)); got != 2 {
		t.Errorf("sse client options = %d, want 2 (headers and timeout)", got)
	}
}

// Per-server options fall back to the mcpProxy defaults, which is how a global
// auth token or keepalive period is applied to every server.
func TestLoadInheritsProxyOptions(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{
		"mcpProxy": {
			"baseURL": "http://localhost:9090",
			"addr": ":9090",
			"name": "test",
			"version": "test",
			"type": "streamable-http",
			"options": {
				"authTokens": ["shared"],
				"logEnabled": true,
				"pingInterval": "5s",
				"autoReconnect": true,
				"reconnectInterval": "1s"
			}
		},
		"mcpServers": {
			"inherits": {"command": "server"},
			"overrides": {
				"command": "server",
				"options": {"authTokens": ["own"], "pingInterval": "1s", "autoReconnect": false, "reconnectInterval": "2s"}
			}
		}
	}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	config, err := load(path, false, false, "", 10)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	inheritsServer := config.McpServers["inherits"]
	if inheritsServer == nil {
		t.Fatal("mcpServers[inherits] missing from loaded config")
	}
	inherits := inheritsServer.Options
	if inherits == nil {
		t.Fatal("mcpServers[inherits].options missing from loaded config")
	}
	if len(inherits.AuthTokens) != 1 || inherits.AuthTokens[0] != "shared" {
		t.Errorf("inherited authTokens = %v, want [shared]", inherits.AuthTokens)
	}
	if !inherits.LogEnabled.OrElse(false) {
		t.Error("inherited logEnabled = false, want true")
	}
	if got := inherits.pingInterval(); got != 5*time.Second {
		t.Errorf("inherited pingInterval = %v, want 5s", got)
	}

	overridesServer := config.McpServers["overrides"]
	if overridesServer == nil || overridesServer.Options == nil {
		t.Fatal("mcpServers[overrides].options missing from loaded config")
	}
	overrides := overridesServer.Options
	if len(overrides.AuthTokens) != 1 || overrides.AuthTokens[0] != "own" {
		t.Errorf("overridden authTokens = %v, want [own]", overrides.AuthTokens)
	}
	if got := overrides.pingInterval(); got != time.Second {
		t.Errorf("overridden pingInterval = %v, want 1s", got)
	}

	// autoReconnect and reconnectInterval inherit like the other options, and
	// an explicit per-server value wins.
	if !inherits.AutoReconnect.Present() || !inherits.AutoReconnect.OrElse(false) {
		t.Error("inherited autoReconnect = false, want true")
	}
	if got := inherits.reconnectInterval(); got != time.Second {
		t.Errorf("inherited reconnectInterval = %v, want 1s", got)
	}
	if overrides.AutoReconnect.OrElse(true) {
		t.Error("overridden autoReconnect = true, want false")
	}
	if got := overrides.reconnectInterval(); got != 2*time.Second {
		t.Errorf("overridden reconnectInterval = %v, want 2s", got)
	}
}

func TestToolFilterIsInheritedFromProxyOptions(t *testing.T) {
	t.Parallel()

	// A server with no toolFilter of its own inherits mcpProxy.options' one;
	// a server that sets one keeps it (no merging).
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
			"inherits": {"command": "/bin/true"},
			"own": {"command": "/bin/true", "options": {"toolFilter": {"mode": "block", "list": ["other"]}}}
		}
	}`)

	config, err := load(path, false, true, "", 10)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	inherits := config.McpServers["inherits"]
	if inherits == nil || inherits.Options == nil || inherits.Options.ToolFilter == nil {
		t.Fatal("server without a toolFilter should inherit mcpProxy.options.toolFilter")
	}
	if got := toolFilterFunc("inherits", inherits.Options)("blocked"); got {
		t.Error("inherited block list must block the listed tool")
	}

	own := config.McpServers["own"]
	if own == nil || own.Options == nil || own.Options.ToolFilter == nil {
		t.Fatal("per-server toolFilter should survive load")
	}
	if got := toolFilterFunc("own", own.Options)("blocked"); !got {
		t.Error("a server's own filter replaces the inherited one, so 'blocked' stays exposed")
	}
	if got := toolFilterFunc("own", own.Options)("other"); got {
		t.Error("the server's own block list must block the listed tool")
	}
}

// A malformed downstream URL in config is reported by load(); the parse
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

func TestValidateConfigRejectsBadResourceAndPromptFilters(t *testing.T) {
	t.Parallel()

	for _, field := range []string{"resourceFilter", "promptFilter"} {
		for _, tc := range []struct {
			filter  *FilterConfig
			wantErr string
		}{
			{filter: &FilterConfig{Mode: "permit"}, wantErr: field + ".mode"},
			{filter: &FilterConfig{Mode: FilterModeAllow}, wantErr: field + ".list cannot be empty in allow mode"},
			{filter: &FilterConfig{Mode: FilterModeBlock, List: []string{" "}}, wantErr: field + ".list[0]"},
		} {
			options := &OptionsV2{}
			if field == "resourceFilter" {
				options.ResourceFilter = tc.filter
			} else {
				options.PromptFilter = tc.filter
			}
			err := validateOptions("mcpServers[\"s\"].options", options)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err = %v, want it to mention %q", field, err, tc.wantErr)
			}
		}
	}
}
