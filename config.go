package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	nethttp "net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/go-sphere/confstore"
	"github.com/go-sphere/confstore/codec"
	"github.com/go-sphere/confstore/provider"
	"github.com/go-sphere/confstore/provider/file"
	"github.com/go-sphere/confstore/provider/http"
	"github.com/tbxark/optional-go"
)

// Duration is a time.Duration that also accepts a JSON string like "30s". A
// bare JSON number still means nanoseconds, which is what decoding into a
// time.Duration has always done here, but that reads as seconds to almost
// everyone - so validation rejects the sub-millisecond values that mistake
// produces.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(data []byte) error {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	switch v := value.(type) {
	case string:
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", v, err)
		}
		*d = Duration(parsed)
	case float64:
		*d = Duration(v)
	case nil:
		// An explicit null means "unset", exactly like leaving the key out.
		// Decoding into a plain time.Duration used to ignore it, so failing
		// here would reject configs that have always loaded.
	default:
		return fmt.Errorf(`duration must be a string like "30s" or a number of nanoseconds, got %s`, data)
	}
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

type StdioMCPClientConfig struct {
	Command string            `json:"command"`
	Env     map[string]string `json:"env"`
	Args    []string          `json:"args"`
	Timeout Duration          `json:"timeout"`
}

type SSEMCPClientConfig struct {
	URL     string             `json:"url"`
	Headers map[string]string  `json:"headers"`
	Timeout Duration           `json:"timeout"`
	OAuth   *OAuthClientConfig `json:"oauth,omitempty"`
}

type StreamableMCPClientConfig struct {
	URL     string             `json:"url"`
	Headers map[string]string  `json:"headers"`
	Timeout Duration           `json:"timeout"`
	OAuth   *OAuthClientConfig `json:"oauth,omitempty"`
}

// OAuthClientConfig configures mcp-proxy as an OAuth client of a downstream
// remote MCP server, for servers that require interactive OAuth and don't
// accept a static bearer token (e.g. Notion's hosted MCP). ClientID/Secret
// may be left empty to use RFC 7591 dynamic client registration, which the
// authorize flow performs automatically on first run.
type OAuthClientConfig struct {
	ClientID     string   `json:"clientId,omitempty"`
	ClientSecret string   `json:"clientSecret,omitempty"`
	RedirectURI  string   `json:"redirectUri,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
	PKCEDisabled bool     `json:"pkceDisabled,omitempty"`
	// AuthServerMetadataURL overrides discovery of the authorization
	// server's metadata document. Needed when the server's
	// authorization_servers entry (RFC 9728) has a non-empty path
	// component: mcp-go's discovery always appends
	// "/.well-known/oauth-authorization-server" after the full issuer
	// URL (OpenID Connect Discovery 1.0 convention), but RFC 8414
	// requires inserting it before the path when one is present, and
	// some providers (e.g. Datadog) only serve it at the RFC 8414
	// location.
	AuthServerMetadataURL string `json:"authServerMetadataUrl,omitempty"`
}

const defaultOAuthRedirectURI = "http://localhost:8090/oauth/callback"

const (
	// defaultPingInterval is how often a downstream connection is probed when
	// options.pingInterval is unset.
	defaultPingInterval = 30 * time.Second
	// defaultStartupGracePeriod is how long readiness waits for every client to
	// connect when mcpProxy.startupGracePeriod is unset.
	defaultStartupGracePeriod = 30 * time.Second
)

// pingInterval returns the probe period for a server, falling back to the
// default when unset.
func (o *OptionsV2) pingInterval() time.Duration {
	if o != nil && o.PingInterval > 0 {
		return time.Duration(o.PingInterval)
	}
	return defaultPingInterval
}

// startupGrace returns how long the proxy waits for stragglers before it
// reports ready anyway, falling back to the default when unset.
func (c *MCPProxyConfigV2) startupGrace() time.Duration {
	if c != nil && c.StartupGracePeriod > 0 {
		return time.Duration(c.StartupGracePeriod)
	}
	return defaultStartupGracePeriod
}

type MCPClientType string

const (
	MCPClientTypeStdio      MCPClientType = "stdio"
	MCPClientTypeSSE        MCPClientType = "sse"
	MCPClientTypeStreamable MCPClientType = "streamable-http"
)

type MCPServerType string

const (
	MCPServerTypeSSE        MCPServerType = "sse"
	MCPServerTypeStreamable MCPServerType = "streamable-http"
)

// ---- V2 ----

type ToolFilterMode string

const (
	ToolFilterModeAllow ToolFilterMode = "allow"
	ToolFilterModeBlock ToolFilterMode = "block"
)

type ToolFilterConfig struct {
	Mode ToolFilterMode `json:"mode,omitempty"`
	List []string       `json:"list,omitempty"`
}

type OptionsV2 struct {
	PanicIfInvalid optional.Field[bool] `json:"panicIfInvalid"`
	LogEnabled     optional.Field[bool] `json:"logEnabled"`
	AuthTokens     []string             `json:"authTokens,omitempty"`
	ToolFilter     *ToolFilterConfig    `json:"toolFilter,omitempty"`
	Disabled       bool                 `json:"disabled,omitempty"`
	// PingInterval is how often this connection is probed to keep it alive and
	// to notice that it died; it sets how quickly /_readyz reports degraded.
	PingInterval Duration `json:"pingInterval,omitempty"`
}

type MCPProxyConfigV2 struct {
	BaseURL string        `json:"baseURL"`
	Addr    string        `json:"addr"`
	Name    string        `json:"name"`
	Version string        `json:"version"`
	Type    MCPServerType `json:"type,omitempty"`
	Options *OptionsV2    `json:"options,omitempty"`
	// StartupGracePeriod bounds how long /_readyz reports "initializing" while
	// clients are still connecting. See startupGrace.
	StartupGracePeriod Duration `json:"startupGracePeriod,omitempty"`
}

type MCPClientConfigV2 struct {
	TransportType MCPClientType `json:"transportType,omitempty"`

	// Stdio
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`

	// SSE or Streamable HTTP
	URL     string             `json:"url,omitempty"`
	Headers map[string]string  `json:"headers,omitempty"`
	Timeout Duration           `json:"timeout,omitempty"`
	OAuth   *OAuthClientConfig `json:"oauth,omitempty"`

	Options *OptionsV2 `json:"options,omitempty"`
}

func parseMCPClientConfigV2(conf *MCPClientConfigV2) (any, error) {
	if conf == nil {
		return nil, errors.New("server config is null")
	}
	if conf.Command != "" && conf.URL != "" {
		return nil, errors.New("command and url are mutually exclusive")
	}

	transportType := conf.TransportType
	if transportType == "" {
		switch {
		case conf.Command != "":
			transportType = MCPClientTypeStdio
		case conf.URL != "":
			transportType = MCPClientTypeSSE
		default:
			return nil, errors.New("command or url is required")
		}
	}

	// timeout is only read by the HTTP transports below, so it is validated
	// there: a stray "timeout" on a stdio entry (easy to copy in from a Claude
	// config) is discarded, and must not abort startup.
	switch transportType {
	case MCPClientTypeStdio:
		if conf.Command == "" {
			return nil, errors.New("command is required for stdio transport")
		}
		if conf.OAuth != nil {
			return nil, errors.New("oauth is not supported for stdio transport")
		}
		// A sub-millisecond timeout is almost always a bare number copied in
		// from a Claude config, where it means nanoseconds. Such a value is
		// discarded rather than failing startup — the historical behaviour for
		// stdio — but a usable one now bounds each request.
		stdioTimeout := conf.Timeout
		if err := validateDuration("timeout", stdioTimeout); err != nil {
			stdioTimeout = 0
		}
		return &StdioMCPClientConfig{
			Command: conf.Command,
			Env:     conf.Env,
			Args:    conf.Args,
			Timeout: stdioTimeout,
		}, nil
	case MCPClientTypeSSE:
		if conf.URL == "" {
			return nil, errors.New("url is required for sse transport")
		}
		if err := validateDuration("timeout", conf.Timeout); err != nil {
			return nil, err
		}
		return &SSEMCPClientConfig{
			URL:     conf.URL,
			Headers: conf.Headers,
			Timeout: conf.Timeout,
			OAuth:   conf.OAuth,
		}, nil
	case MCPClientTypeStreamable:
		if conf.URL == "" {
			return nil, errors.New("url is required for streamable-http transport")
		}
		if err := validateDuration("timeout", conf.Timeout); err != nil {
			return nil, err
		}
		return &StreamableMCPClientConfig{
			URL:     conf.URL,
			Headers: conf.Headers,
			Timeout: conf.Timeout,
			OAuth:   conf.OAuth,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported transportType %q", conf.TransportType)
	}
}

// validateDuration rejects the values a bare JSON number produces when the
// author meant seconds: 30 decodes to 30 nanoseconds, which used to fail every
// request with an unhelpful deadline error.
func validateDuration(field string, d Duration) error {
	if d < 0 {
		return fmt.Errorf("%s cannot be negative", field)
	}
	if d > 0 && time.Duration(d) < time.Millisecond {
		return fmt.Errorf(`%s %v is shorter than a millisecond; a bare number means nanoseconds, write a string like "30s" instead`, field, time.Duration(d))
	}
	return nil
}

func validateHTTPURL(field, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%s is invalid: %w", field, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s must be an absolute http(s) URL", field)
	}
	return nil
}

func validateOptions(field string, options *OptionsV2) error {
	if options == nil {
		return nil
	}
	if err := validateDuration(field+".pingInterval", options.PingInterval); err != nil {
		return err
	}
	for i, token := range options.AuthTokens {
		if strings.TrimSpace(token) == "" {
			return fmt.Errorf("%s.authTokens[%d] cannot be empty", field, i)
		}
	}
	if options.ToolFilter == nil {
		return nil
	}
	mode := ToolFilterMode(strings.ToLower(string(options.ToolFilter.Mode)))
	if mode != ToolFilterModeAllow && mode != ToolFilterModeBlock {
		return fmt.Errorf("%s.toolFilter.mode must be %q or %q", field, ToolFilterModeAllow, ToolFilterModeBlock)
	}
	for i, toolName := range options.ToolFilter.List {
		if strings.TrimSpace(toolName) == "" {
			return fmt.Errorf("%s.toolFilter.list[%d] cannot be empty", field, i)
		}
	}
	return nil
}

// serverNameForbiddenChars are rejected in server names: "{" and "}" are
// wildcard syntax in http.ServeMux patterns, and "\" is a separator only by
// accident.
const serverNameForbiddenChars = `\{}`

// validateServerName ensures a name is usable as a URL path. Every server is
// mounted at path.Join(baseURL.Path, name), so a name that does not survive
// path cleaning unchanged ("..", "." or empty segments) could escape the base
// path or collide with another server's route - and a route collision makes
// http.ServeMux.Handle panic once both clients connect.
//
// Whitespace is rejected for the same reason: http.ServeMux reads the text
// before the first space in a pattern as an HTTP method, so "/my server/" does
// not register a route, it panics ("invalid method").
//
// Multi-segment names stay legal: docs/USAGE.md suggests "<server>/<token>" as
// a way to carry a token in the route for clients that cannot set headers.
func validateServerName(name string) error {
	if name == "" {
		return errors.New("mcpServers contains an empty server name")
	}
	if strings.ContainsAny(name, serverNameForbiddenChars) {
		return fmt.Errorf("mcpServers[%q]: name is part of a URL route and cannot contain any of %q", name, serverNameForbiddenChars)
	}
	if path.Join("/", name) != "/"+name {
		return fmt.Errorf("mcpServers[%q]: name must be a clean relative path, without %q, %q, a leading %q, or empty segments", name, "..", ".", "/")
	}
	for _, r := range name {
		if unicode.IsSpace(r) {
			return fmt.Errorf("mcpServers[%q]: name is part of a URL route and cannot contain whitespace", name)
		}
		if unicode.IsControl(r) {
			return fmt.Errorf("mcpServers[%q]: name cannot contain control characters", name)
		}
	}
	return nil
}

func validateConfig(config *Config) error {
	if config == nil || config.McpProxy == nil {
		return errors.New("mcpProxy is required")
	}
	proxy := config.McpProxy
	if err := validateHTTPURL("mcpProxy.baseURL", proxy.BaseURL); err != nil {
		return err
	}
	if strings.TrimSpace(proxy.Addr) == "" {
		return errors.New("mcpProxy.addr is required")
	}
	if strings.TrimSpace(proxy.Name) == "" {
		return errors.New("mcpProxy.name is required")
	}
	if strings.TrimSpace(proxy.Version) == "" {
		return errors.New("mcpProxy.version is required")
	}
	if proxy.Type != MCPServerTypeSSE && proxy.Type != MCPServerTypeStreamable {
		return fmt.Errorf("mcpProxy.type must be %q or %q", MCPServerTypeSSE, MCPServerTypeStreamable)
	}
	if err := validateDuration("mcpProxy.startupGracePeriod", proxy.StartupGracePeriod); err != nil {
		return err
	}
	if err := validateOptions("mcpProxy.options", proxy.Options); err != nil {
		return err
	}

	for name, serverConfig := range config.McpServers {
		if err := validateServerName(name); err != nil {
			return err
		}
		field := fmt.Sprintf("mcpServers[%q]", name)
		if serverConfig == nil {
			return fmt.Errorf("%s cannot be null", field)
		}
		if _, err := parseMCPClientConfigV2(serverConfig); err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
		if serverConfig.URL != "" {
			if err := validateHTTPURL(field+".url", serverConfig.URL); err != nil {
				return err
			}
		}
		if err := validateOptions(field+".options", serverConfig.Options); err != nil {
			return err
		}
		if serverConfig.OAuth != nil {
			if serverConfig.OAuth.ClientID == "" && serverConfig.OAuth.ClientSecret != "" {
				return fmt.Errorf("%s.oauth.clientSecret requires clientId", field)
			}
			redirectURI := serverConfig.OAuth.RedirectURI
			if redirectURI == "" {
				redirectURI = defaultOAuthRedirectURI
			}
			if _, _, err := parseRedirectURI(redirectURI); err != nil {
				return fmt.Errorf("%s.oauth.redirectUri: %w", field, err)
			}
			if metadataURL := serverConfig.OAuth.AuthServerMetadataURL; metadataURL != "" {
				if err := validateHTTPURL(field+".oauth.authServerMetadataUrl", metadataURL); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// ---- Config ----

type Config struct {
	McpProxy   *MCPProxyConfigV2             `json:"mcpProxy"`
	McpServers map[string]*MCPClientConfigV2 `json:"mcpServers"`
}

// FullConfig is the on-the-wire shape: Config plus the two keys of the removed
// v1 schema, kept only so that loading a v1 file fails with a migration hint
// instead of the misleading "mcpProxy is required".
type FullConfig struct {
	LegacyServerV1  json.RawMessage `json:"server"`
	LegacyClientsV1 json.RawMessage `json:"clients"`

	McpProxy   *MCPProxyConfigV2             `json:"mcpProxy"`
	McpServers map[string]*MCPClientConfigV2 `json:"mcpServers"`
}

// jsonPresent reports whether a key was in the document with a value other
// than null.
func jsonPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

func newConfProvider(path string, insecure, expandEnv bool, httpHeaders string, httpTimeout int) (provider.Provider, error) {
	if http.IsRemoteURL(path) {
		var opts []http.Option
		// Never reuse nethttp.DefaultClient here: setting Timeout on it would
		// leak this flag onto every other user of the default client.
		httpClient := &nethttp.Client{}
		if insecure {
			transport := nethttp.DefaultTransport.(*nethttp.Transport).Clone()
			transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
			httpClient.Transport = transport
		}
		if httpTimeout > 0 {
			httpClient.Timeout = time.Duration(httpTimeout) * time.Second
		}
		opts = append(opts, http.WithClient(httpClient))
		if httpHeaders != "" {
			// format: 'Key1:Value1;Key2:Value2'
			headers := make(nethttp.Header)
			for kv := range strings.SplitSeq(httpHeaders, ";") {
				parts := strings.SplitN(kv, ":", 2)
				if len(parts) == 2 {
					key := strings.TrimSpace(parts[0])
					value := strings.TrimSpace(parts[1])
					if key != "" && value != "" {
						headers.Add(key, value)
					}
				}
			}
			if len(headers) > 0 {
				opts = append(opts, http.WithHeaders(headers))
			}
		}
		pro := http.New(path, opts...)
		if expandEnv {
			return provider.NewExpandEnv(pro), nil
		} else {
			return pro, nil
		}
	}
	if file.IsLocalPath(path) {
		if expandEnv {
			return provider.NewExpandEnv(file.New(path, file.WithExpandEnv())), nil
		} else {
			return file.New(path), nil
		}
	}
	return nil, errors.New("unsupported config path")
}

func load(path string, insecure, expandEnv bool, httpHeaders string, httpTimeout int) (*Config, error) {
	pro, err := newConfProvider(path, insecure, expandEnv, httpHeaders, httpTimeout)
	if err != nil {
		return nil, err
	}
	conf, err := confstore.Load[FullConfig](pro, codec.JsonCodec())
	if err != nil {
		return nil, err
	}
	if jsonPresent(conf.LegacyServerV1) || jsonPresent(conf.LegacyClientsV1) {
		return nil, errors.New(`this looks like a v1 config: the "server" and "clients" keys were removed. Rename them to "mcpProxy" and "mcpServers", and lift each client's "config" object into the server entry itself (see docs/CONFIGURATION.md)`)
	}

	if conf.McpProxy == nil {
		return nil, errors.New("mcpProxy is required")
	}
	if conf.McpProxy.Options == nil {
		conf.McpProxy.Options = &OptionsV2{}
	}
	for name, clientConfig := range conf.McpServers {
		if clientConfig == nil {
			return nil, fmt.Errorf("mcpServers[%q] cannot be null", name)
		}
		if clientConfig.Options == nil {
			clientConfig.Options = &OptionsV2{}
		}
		if clientConfig.Options.AuthTokens == nil {
			clientConfig.Options.AuthTokens = conf.McpProxy.Options.AuthTokens
		}
		if !clientConfig.Options.PanicIfInvalid.Present() {
			clientConfig.Options.PanicIfInvalid = conf.McpProxy.Options.PanicIfInvalid
		}
		if !clientConfig.Options.LogEnabled.Present() {
			clientConfig.Options.LogEnabled = conf.McpProxy.Options.LogEnabled
		}
		if clientConfig.Options.PingInterval == 0 {
			clientConfig.Options.PingInterval = conf.McpProxy.Options.PingInterval
		}
	}

	if conf.McpProxy.Type == "" {
		conf.McpProxy.Type = MCPServerTypeSSE // default to SSE
	}

	config := &Config{
		McpProxy:   conf.McpProxy,
		McpServers: conf.McpServers,
	}
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	return config, nil
}
