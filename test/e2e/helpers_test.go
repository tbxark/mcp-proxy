package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

const (
	testAuthToken = "integration-test-token"
	testEnvValue  = "value-from-config"

	// Both binaries are compiled once for the whole package; building them per
	// test would dominate the runtime.
	proxyPkg   = "../.."
	fixturePkg = "../../testdata/stdio-server"

	// The proxy's own defaults are 30s. Configs here shorten them so a test can
	// watch readiness change instead of waiting half a minute.
	testPingInterval = "200ms"
	testGracePeriod  = "1s"
	// testStdioTimeout bounds a single downstream stdio request. It is short so
	// a wedged call fails fast in tests, but far longer than any fixture tool
	// legitimately takes.
	testStdioTimeout = "3s"
)

// configTemplate mirrors what a user would write by hand. The auth token comes
// from an env var so config loading's ${VAR} expansion is exercised too.
const configTemplate = `{
  "mcpProxy": {
    "baseURL": "http://%[1]s",
    "addr": "%[1]s",
    "name": "integration-proxy",
    "version": "9.9.9",
    "type": "%[2]s",
    "startupGracePeriod": "` + testGracePeriod + `",
    "options": {
      "logEnabled": true,
      "pingInterval": "` + testPingInterval + `",
      "authTokens": ["${MCP_PROXY_TEST_TOKEN}"]
    }
  },
  "mcpServers": {
    "fixture": {
      "command": "%[3]s",
      "env": {"MCP_PROXY_TEST_ENV": "%[4]s"},
      "timeout": "` + testStdioTimeout + `",
      "options": {
        "toolFilter": {"mode": "block", "list": ["blocked"]}
      }
    },
    "off": {
      "command": "%[3]s",
      "options": {"disabled": true}
    }
  }
}`

// ---- building ----

// binaries are compiled on first use and shared by every test, so they outlive
// any single t.TempDir. TestMain removes the directory at the end of the run.
var builds struct {
	sync.Mutex
	dir   string
	byPkg map[string]string
}

func TestMain(m *testing.M) {
	code := m.Run()
	if builds.dir != "" {
		_ = os.RemoveAll(builds.dir)
	}
	os.Exit(code)
}

// buildBinary compiles a package once per test run and returns its path.
func buildBinary(t *testing.T, pkg, name string) string {
	t.Helper()

	builds.Lock()
	defer builds.Unlock()
	if path, ok := builds.byPkg[pkg]; ok {
		return path
	}
	if builds.dir == "" {
		dir, err := os.MkdirTemp("", "mcp-proxy-e2e")
		if err != nil {
			t.Fatalf("temp dir: %v", err)
		}
		builds.dir = dir
	}

	binary := filepath.Join(builds.dir, name)
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	out, err := exec.Command("go", "build", "-o", binary, pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, out)
	}
	if builds.byPkg == nil {
		builds.byPkg = map[string]string{}
	}
	builds.byPkg[pkg] = binary
	return binary
}

func buildProxy(t *testing.T) string   { return buildBinary(t, proxyPkg, "mcp-proxy") }
func buildFixture(t *testing.T) string { return buildBinary(t, fixturePkg, "stdio-server") }

// ---- configs ----

// freeAddr reserves a loopback port and releases it again: the proxy binds an
// address from its config, so the port has to be chosen up front.
func freeAddr(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return addr
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// stdioConfig writes a config with the stdio fixture mounted at "fixture" and a
// disabled server at "off", and returns its path and the proxy's address.
func stdioConfig(t *testing.T, serverType string) (string, string) {
	t.Helper()

	addr := freeAddr(t)
	fixture := filepath.ToSlash(buildFixture(t))
	return writeConfig(t, fmt.Sprintf(configTemplate, addr, serverType, fixture, testEnvValue)), addr
}

// ---- running the proxy ----

// proxy is a running mcp-proxy process.
type proxy struct {
	baseURL string
	cmd     *exec.Cmd
	done    chan error
	stderr  *syncBuffer
	stopped bool
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startProxy runs the proxy binary against configPath, waits until /_readyz
// reports ready, and stops it on cleanup.
func startProxy(t *testing.T, configPath, addr string) *proxy {
	t.Helper()

	p := launchProxy(t, configPath, addr)
	p.waitForReady(t)
	return p
}

// launchProxy is startProxy without the wait, for tests that assert on a proxy
// which never becomes ready.
func launchProxy(t *testing.T, configPath, addr string) *proxy {
	t.Helper()

	cmd := exec.Command(buildProxy(t), "-config", configPath)
	cmd.Env = append(os.Environ(), "MCP_PROXY_TEST_TOKEN="+testAuthToken)
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start proxy: %v", err)
	}

	p := &proxy{baseURL: "http://" + addr, cmd: cmd, done: make(chan error, 1), stderr: stderr}
	go func() { p.done <- cmd.Wait() }()
	t.Cleanup(func() {
		if !p.stopped {
			_ = cmd.Process.Kill()
			<-p.done
		}
	})

	return p
}

// waitForReady polls /_readyz the way a container orchestrator would before
// routing traffic.
func (p *proxy) waitForReady(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-p.done:
			p.stopped = true
			t.Fatalf("proxy exited before becoming ready: %v\n%s", err, p.stderr.String())
		default:
		}
		if code, _ := p.ready(t); code == http.StatusOK {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("proxy did not become ready within 30s\n%s", p.stderr.String())
}

// ready returns the status code and body of /_readyz. A connection error
// reports 0 so callers can poll a proxy that is not listening yet.
func (p *proxy) ready(t *testing.T) (int, string) {
	t.Helper()

	resp, err := http.Get(p.baseURL + "/_readyz")
	if err != nil {
		return 0, ""
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /_readyz body: %v", err)
	}
	return resp.StatusCode, string(body)
}

// waitForReadyState polls until /_readyz returns want, and returns the body.
func (p *proxy) waitForReadyState(t *testing.T, want int) string {
	t.Helper()

	return p.waitForReadyBody(t, want, "")
}

// waitForReadyBody polls until /_readyz returns want with a body containing
// substr, so a test can wait for one 503 state rather than the first of them:
// "initializing" and "degraded" share a status code.
func (p *proxy) waitForReadyBody(t *testing.T, want int, substr string) string {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	var code int
	var body string
	for time.Now().Before(deadline) {
		if code, body = p.ready(t); code == want && strings.Contains(body, substr) {
			return body
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("/_readyz = %d %s, want %d containing %q", code, body, want, substr)
	return ""
}

func (p *proxy) get(t *testing.T, path string) int {
	t.Helper()

	resp, err := http.Get(p.baseURL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// stop sends SIGTERM and asserts the proxy exits cleanly, which is how it is
// shut down in production.
func (p *proxy) stop(t *testing.T) {
	t.Helper()

	if p.stopped {
		return
	}
	p.stopped = true
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal proxy: %v", err)
	}
	select {
	case err := <-p.done:
		if err != nil {
			t.Errorf("proxy exited with %v, want a clean exit\n%s", err, p.stderr.String())
		}
	case <-time.After(15 * time.Second):
		_ = p.cmd.Process.Kill()
		t.Fatal("proxy did not exit within 15s of SIGTERM")
	}
}

// ---- MCP client plumbing ----

// mcpPath is the route a proxied server is served at, which depends on the
// proxy's own transport.
func mcpPath(serverType, name string) string {
	if serverType == "sse" {
		return "/" + name + "/sse"
	}
	return "/" + name + "/mcp"
}

func (p *proxy) connect(t *testing.T, serverType, name string) *client.Client {
	t.Helper()

	endpoint := p.baseURL + mcpPath(serverType, name)
	headers := map[string]string{"Authorization": "Bearer " + testAuthToken}
	var (
		mcpClient *client.Client
		err       error
	)
	if serverType == "sse" {
		mcpClient, err = client.NewSSEMCPClient(endpoint, client.WithHeaders(headers))
	} else {
		mcpClient, err = client.NewStreamableHttpClient(endpoint, transport.WithHTTPHeaders(headers))
	}
	if err != nil {
		t.Fatalf("create %s client: %v", serverType, err)
	}
	t.Cleanup(func() { _ = mcpClient.Close() })

	ctx := testContext(t)
	if err := mcpClient.Start(ctx); err != nil {
		t.Fatalf("start %s client: %v", serverType, err)
	}

	initRequest := mcp.InitializeRequest{}
	initRequest.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initRequest.Params.ClientInfo = mcp.Implementation{Name: "e2e", Version: "1.0.0"}
	result, err := mcpClient.Initialize(ctx, initRequest)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if result.ServerInfo.Name != name {
		t.Errorf("server name = %q, want the proxy route name %q", result.ServerInfo.Name, name)
	}
	return mcpClient
}

func testContext(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func callTool(t *testing.T, mcpClient *client.Client, name string, args map[string]any) (*mcp.CallToolResult, error) {
	t.Helper()

	request := mcp.CallToolRequest{}
	request.Params.Name = name
	request.Params.Arguments = args
	return mcpClient.CallTool(testContext(t), request)
}

func callToolText(t *testing.T, mcpClient *client.Client, name string, args map[string]any) string {
	t.Helper()

	result, err := callTool(t, mcpClient, name, args)
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if result.IsError {
		t.Fatalf("call %s returned an error result: %s", name, resultText(t, result))
	}
	return resultText(t, result)
}

func resultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()

	if len(result.Content) == 0 {
		t.Fatal("tool result has no content")
	}
	text, ok := mcp.AsTextContent(result.Content[0])
	if !ok {
		t.Fatalf("tool content type = %T, want text", result.Content[0])
	}
	return text.Text
}

func readResourceText(t *testing.T, mcpClient *client.Client, uri string) string {
	t.Helper()

	request := mcp.ReadResourceRequest{}
	request.Params.URI = uri
	result, err := mcpClient.ReadResource(testContext(t), request)
	if err != nil {
		t.Fatalf("read resource %s: %v", uri, err)
	}
	if len(result.Contents) == 0 {
		t.Fatalf("resource %s has no contents", uri)
	}
	contents, ok := mcp.AsTextResourceContents(result.Contents[0])
	if !ok {
		t.Fatalf("resource %s content type = %T, want text", uri, result.Contents[0])
	}
	return contents.Text
}

// ---- misc ----

// runCLI runs the proxy binary and returns its stdout and stderr. A non-nil
// error means a non-zero exit status.
func runCLI(t *testing.T, args ...string) (string, string, error) {
	t.Helper()

	cmd := exec.Command(buildProxy(t), args...)
	cmd.Env = append(os.Environ(), "MCP_PROXY_TEST_TOKEN="+testAuthToken)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("run %v: %v", args, err)
	}
	return stdout.String(), stderr.String(), err
}

// assertPortReleased confirms the graceful shutdown actually closed the
// listener rather than just exiting.
func assertPortReleased(t *testing.T, addr string) {
	t.Helper()

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("port %s still bound after shutdown: %v", addr, err)
	}
	_ = listener.Close()
}

func skipShort(t *testing.T) {
	t.Helper()

	if testing.Short() {
		t.Skip("end-to-end test builds binaries and spawns processes")
	}
}
