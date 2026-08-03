package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

const doctorLiveTimeout = 15 * time.Second

// doctorResult describes one configured MCP server's authentication state,
// as reported by the -doctor command.
type doctorResult struct {
	name      string
	transport string
	auth      string
	ok        bool
	status    string
	live      string
}

// runDoctor loads the config and reports, for every configured MCP server,
// its transport, its authentication mechanism, and whether its credentials
// are currently valid. Backs both the -auth-status and -doctor flags.
//
// With live=false (-auth-status) this is local-only: it reads config.json
// and the cached oauth/<name>.json token files (comparing expires_at
// against now), the same fields AGENTS.md already documents checking by
// hand. With live=true (-doctor) it additionally connects to each remote
// server (the same Start+Initialize the daemon performs at startup, run
// concurrently across servers) to confirm the credentials are accepted
// right now rather than just unexpired locally; this can incidentally
// refresh an expired OAuth token via its refresh token, same as the daemon
// would, but it never performs interactive (browser-based) authorization.
//
// Returns whether every server checked out OK, so main can set a non-zero
// exit code for scripting.
func runDoctor(configPath string, insecure, expandEnv bool, httpHeaders string, httpTimeout int, live bool) (bool, error) {
	config, err := load(configPath, insecure, expandEnv, httpHeaders, httpTimeout)
	if err != nil {
		return false, fmt.Errorf("failed to load config: %w", err)
	}

	names := make([]string, 0, len(config.McpServers))
	for name := range config.McpServers {
		names = append(names, name)
	}
	sort.Strings(names)

	results := make([]doctorResult, len(names))
	for i, name := range names {
		results[i] = checkServerAuth(name, config.McpServers[name])
	}

	if live {
		var wg sync.WaitGroup
		for i, name := range names {
			clientConfig := config.McpServers[name]
			if results[i].transport == "stdio" || (clientConfig.Options != nil && clientConfig.Options.Disabled) {
				continue
			}
			wg.Add(1)
			go func(i int, name string, clientConfig *MCPClientConfigV2) {
				defer wg.Done()
				slog.Info("doctor: checking live", "server", name)
				checkServerLive(&results[i], clientConfig)
				slog.Info("doctor: checked live", "server", name, "result", results[i].live)
			}(i, name, clientConfig)
		}
		wg.Wait()
	}

	allOK := true
	for i, name := range names {
		clientConfig := config.McpServers[name]
		if clientConfig.Options != nil && clientConfig.Options.Disabled {
			results[i].status = "disabled (skipped)"
			results[i].ok = true
		}
		if !results[i].ok {
			allOK = false
		}
	}

	printDoctorReport(results, live)
	return allOK, nil
}

// checkServerAuth classifies a server's transport and auth mechanism and
// performs the local (no-network) validity check appropriate to it.
func checkServerAuth(name string, conf *MCPClientConfigV2) doctorResult {
	res := doctorResult{name: name}
	clientInfo, err := parseMCPClientConfigV2(conf)
	if err != nil {
		res.transport, res.auth = "?", "?"
		res.status = fmt.Sprintf("invalid config: %v", err)
		return res
	}

	switch v := clientInfo.(type) {
	case *StdioMCPClientConfig:
		res.transport = "stdio"
		res.auth = "none"
		res.ok = true
		res.status = "ok (local process, no auth)"
	case *SSEMCPClientConfig:
		res.transport = "sse"
		checkRemoteAuth(&res, name, v.OAuth, v.Headers)
	case *StreamableMCPClientConfig:
		res.transport = "streamable-http"
		checkRemoteAuth(&res, name, v.OAuth, v.Headers)
	default:
		res.transport, res.auth = "?", "?"
		res.status = "unrecognized client type"
	}
	return res
}

func checkRemoteAuth(res *doctorResult, name string, oauthConf *OAuthClientConfig, headers map[string]string) {
	if oauthConf != nil {
		res.auth = "oauth"
		checkOAuthToken(res, name)
		return
	}
	if len(headers) == 0 {
		res.auth = "none"
		res.ok = true
		res.status = "ok (no auth configured)"
		return
	}
	res.auth = "static header"
	checkStaticHeaders(res, headers)
}

// checkStaticHeaders flags headers whose value is empty once env vars have
// been expanded (confstore expands an unset ${VAR} to ""), which is the
// static-auth equivalent of an OAuth token being missing.
func checkStaticHeaders(res *doctorResult, headers map[string]string) {
	var empty []string
	for key, value := range headers {
		if strings.TrimSpace(strings.TrimPrefix(value, "Bearer")) == "" {
			empty = append(empty, key)
		}
	}
	if len(empty) > 0 {
		sort.Strings(empty)
		res.ok = false
		res.status = fmt.Sprintf("MISSING (%s empty after env expansion; check the referenced env var is set)", strings.Join(empty, ", "))
		return
	}
	res.ok = true
	res.status = "ok (header value present)"
}

func checkOAuthToken(res *doctorResult, name string) {
	path, err := oauthTokenPath(name)
	if err != nil {
		res.status = fmt.Sprintf("error: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	token, err := NewFileTokenStore(path).GetToken(ctx)
	if errors.Is(err, transport.ErrNoToken) {
		res.status = fmt.Sprintf("NOT AUTHORIZED (no token on file; run: mcp-proxy -authorize %s -config <path>)", name)
		return
	}
	if err != nil {
		res.status = fmt.Sprintf("error reading token: %v", err)
		return
	}

	hasRefresh := token.RefreshToken != ""
	switch {
	case token.IsExpired():
		age := time.Since(token.ExpiresAt).Round(time.Minute)
		if hasRefresh {
			res.status = fmt.Sprintf("EXPIRED %s ago (has refresh token; daemon restart or -doctor may refresh it)", age)
		} else {
			res.status = fmt.Sprintf("EXPIRED %s ago (no refresh token; re-run: mcp-proxy -authorize %s -config <path>)", age, name)
		}
	case token.ExpiresAt.IsZero():
		res.ok = true
		res.status = "ok (token does not expire)"
	default:
		res.ok = true
		res.status = fmt.Sprintf("ok (expires in %s)", time.Until(token.ExpiresAt).Round(time.Minute))
	}
}

// checkServerLive attempts an actual Start+Initialize against the remote
// server, without ever falling back to interactive (browser-based)
// authorization - it only reports whether one would be needed.
func checkServerLive(res *doctorResult, conf *MCPClientConfigV2) {
	mcpClient, err := newMCPClient(res.name, conf)
	if err != nil {
		res.ok = false
		res.live = fmt.Sprintf("FAILED: %v", err)
		return
	}
	defer mcpClient.Close()

	ctx, cancel := context.WithTimeout(context.Background(), doctorLiveTimeout)
	defer cancel()

	if mcpClient.needManualStart {
		if err := mcpClient.client.Start(ctx); err != nil {
			res.ok = false
			res.live = liveFailureMessage(res.name, err)
			return
		}
	}

	initRequest := mcp.InitializeRequest{}
	initRequest.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initRequest.Params.ClientInfo = mcp.Implementation{Name: "mcp-proxy-doctor", Version: BuildVersion}
	if _, err := mcpClient.client.Initialize(ctx, initRequest); err != nil {
		res.ok = false
		res.live = liveFailureMessage(res.name, err)
		return
	}
	res.live = "ok (connected)"
}

func liveFailureMessage(name string, err error) string {
	if client.IsOAuthAuthorizationRequiredError(err) {
		return fmt.Sprintf("NOT AUTHORIZED (run: mcp-proxy -authorize %s -config <path>)", name)
	}
	return fmt.Sprintf("FAILED: %v", err)
}

func printDoctorReport(results []doctorResult, live bool) {
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	if live {
		fmt.Fprintln(w, "NAME\tTRANSPORT\tAUTH\tSTATUS\tLIVE")
	} else {
		fmt.Fprintln(w, "NAME\tTRANSPORT\tAUTH\tSTATUS")
	}
	for _, res := range results {
		if live {
			liveStatus := res.live
			if liveStatus == "" {
				liveStatus = "skipped"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", res.name, res.transport, res.auth, res.status, liveStatus)
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", res.name, res.transport, res.auth, res.status)
		}
	}
	_ = w.Flush()
}
