package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	nethttp "net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-sphere/confstore"
	"github.com/go-sphere/confstore/codec"
	"github.com/go-sphere/confstore/provider"
	"github.com/go-sphere/confstore/provider/file"
	"github.com/go-sphere/confstore/provider/http"
	"github.com/tbxark/optional-go"
)

type StdioMCPClientConfig struct {
	Command string            `json:"command"`
	Env     map[string]string `json:"env"`
	Args    []string          `json:"args"`
}

type SSEMCPClientConfig struct {
	URL     string             `json:"url"`
	Headers map[string]string  `json:"headers"`
	OAuth   *OAuthClientConfig `json:"oauth,omitempty"`
}

type StreamableMCPClientConfig struct {
	URL     string             `json:"url"`
	Headers map[string]string  `json:"headers"`
	Timeout time.Duration      `json:"timeout"`
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
}

type MCPProxyConfigV2 struct {
	BaseURL string        `json:"baseURL"`
	Addr    string        `json:"addr"`
	Name    string        `json:"name"`
	Version string        `json:"version"`
	Type    MCPServerType `json:"type,omitempty"`
	Options *OptionsV2    `json:"options,omitempty"`
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
	Timeout time.Duration      `json:"timeout,omitempty"`
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

	switch transportType {
	case MCPClientTypeStdio:
		if conf.Command == "" {
			return nil, errors.New("command is required for stdio transport")
		}
		if conf.OAuth != nil {
			return nil, errors.New("oauth is not supported for stdio transport")
		}
		return &StdioMCPClientConfig{
			Command: conf.Command,
			Env:     conf.Env,
			Args:    conf.Args,
		}, nil
	case MCPClientTypeSSE:
		if conf.URL == "" {
			return nil, errors.New("url is required for sse transport")
		}
		return &SSEMCPClientConfig{
			URL:     conf.URL,
			Headers: conf.Headers,
			OAuth:   conf.OAuth,
		}, nil
	case MCPClientTypeStreamable:
		if conf.URL == "" {
			return nil, errors.New("url is required for streamable-http transport")
		}
		if conf.Timeout < 0 {
			return nil, errors.New("timeout cannot be negative")
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
	if err := validateOptions("mcpProxy.options", proxy.Options); err != nil {
		return err
	}

	for name, serverConfig := range config.McpServers {
		field := fmt.Sprintf("mcpServers[%q]", name)
		if strings.TrimSpace(name) == "" {
			return errors.New("mcpServers contains an empty server name")
		}
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

type FullConfig struct {
	DeprecatedServerV1  *MCPProxyConfigV1             `json:"server"`
	DeprecatedClientsV1 map[string]*MCPClientConfigV1 `json:"clients"`

	McpProxy   *MCPProxyConfigV2             `json:"mcpProxy"`
	McpServers map[string]*MCPClientConfigV2 `json:"mcpServers"`
}

func newConfProvider(path string, insecure, expandEnv bool, httpHeaders string, httpTimeout int) (provider.Provider, error) {
	if http.IsRemoteURL(path) {
		var opts []http.Option
		httpClient := nethttp.DefaultClient
		if insecure {
			transport := nethttp.DefaultTransport.(*nethttp.Transport).Clone()
			transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
			httpClient = &nethttp.Client{Transport: transport}
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
	adaptMCPClientConfigV1ToV2(conf)

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
