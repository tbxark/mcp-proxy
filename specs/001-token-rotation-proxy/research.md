# Research: Token Rotation and Proxy Support

**Date**: 2026-02-19  
**Feature**: 001-token-rotation-proxy  
**Oracle Consultation**: 2 iterations completed

## Research Questions Resolved

### 1. Token Rotation Architecture

**Decision**: Per-client TokenRotator with mutex-guarded state, integrated at HTTP transport layer

**Rationale**:
- Keeps changes local to MCP Proxy wrapper (no mcp-go library modifications)
- Per-client state ensures independent rotation for each MCP server connection
- Mutex provides simple, correct concurrency control for index and failure count updates
- HTTP transport layer integration works transparently for SSE and streamable-http clients

**Alternatives considered**:
- Atomic operations: Rejected because we need coordinated updates (index + failure counts)
- Global rotation state: Rejected because different clients need independent token pools
- Integration in mcp-go library: Rejected to avoid external dependency modifications

### 2. Rotation Mode Behaviors

**Decision**: Two distinct modes with different retry semantics

**Round-Robin**:
- Rotates token on every request (per `Next()` call)
- No automatic retry on auth failure
- Records failure count but continues rotation
- Use case: Load distribution across multiple valid tokens

**On-First-Failed**:
- Reuses current token until 401/403 received
- Automatically retries request with next token
- Advances to next token only on auth failure
- Respects MaxRetries limit to prevent infinite loops (default: len(tokens), meaning try each token once)
- Use case: Automatic failover when tokens expire

**Rationale**: Two modes serve different use cases - load distribution vs high availability

### 3. Authentication Failure Detection

**Decision**: HTTP status codes 401 (Unauthorized) and 403 (Forbidden) trigger rotation

**Rationale**:
- Standard HTTP auth failure codes
- Network errors (timeouts, connection refused) should NOT rotate tokens
- 5xx server errors should NOT rotate tokens (server issue, not auth)
- 407 (Proxy Auth Required) should NOT rotate tokens (proxy issue, not upstream auth)

**Implementation**: Check `resp.StatusCode` in authRoundTripper after successful HTTP round trip

### 4. Proxy Architecture

**Decision**: Per-client http.Transport with custom Proxy function and DialContext

**HTTP/HTTPS Proxy**:
- Use `http.Transport.Proxy` field with custom function
- Support Proxy-Authorization header for proxy authentication
- Fall back to `http.ProxyFromEnvironment` when ProxyConfig is nil

**SOCKS5 Proxy**:
- Use `http.Transport.DialContext` with golang.org/x/net/proxy SOCKS5 dialer
- Support username/password authentication via proxy.Auth
- Dependency: golang.org/x/net/proxy (semi-official Go extended library, acceptable)

**Rationale**: Leverages Go's standard http.Transport proxy mechanisms, minimal custom code

### 5. Config Schema Design

**Decision**: Nested AuthConfig and ProxyConfig structs in OptionsV2

```go
type OptionsV2 struct {
    // Existing fields
    PanicIfInvalid optional.Field[bool]
    LogEnabled     optional.Field[bool]
    ToolFilter     *ToolFilterConfig
    Disabled       bool
    
    // Legacy (backward compatibility)
    AuthTokens []string `json:"authTokens,omitempty"`
    
    // New nested configs
    Auth  *AuthConfig  `json:"auth,omitempty"`
    Proxy *ProxyConfig `json:"proxy,omitempty"`
}

type AuthConfig struct {
    Tokens       []string             `json:"tokens,omitempty"`
    RotationMode string               `json:"rotationMode,omitempty"` // "round-robin" | "on-first-failed"
    MaxRetries   optional.Field[int]  `json:"maxRetries,omitempty"`   // default: len(tokens)
}

type ProxyConfig struct {
    URL    string              `json:"url,omitempty"`   // includes scheme (http, https, socks5)
    Type   string              `json:"type,omitempty"`  // optional, override derived from URL
    Auth   *ProxyAuth          `json:"auth,omitempty"`
    UseEnv optional.Field[bool] `json:"useEnv,omitempty"` // default: true if Proxy nil
}

type ProxyAuth struct {
    Username string `json:"username,omitempty"`
    Password string `json:"password,omitempty"`
}
```

**Backward Compatibility Strategy**:
- If `Auth` is nil and `AuthTokens` exists, map `AuthTokens` to `Auth.Tokens`
- If single token and no `RotationMode`, treat as static (no rotation)
- If multiple tokens and no `RotationMode`, default to "round-robin" with warning
- If both `AuthTokens` and `Auth.Tokens` set, prefer `Auth.Tokens` and warn

**Rationale**: Nested structs provide clear organization while preserving legacy field for smooth migration

### 6. Authorization Header Conflict Resolution

**Decision**: Auth.Tokens takes precedence over Headers.Authorization with one-time warning

**Implementation**:
```go
func resolveAuth(headers map[string]string, auth *AuthConfig) (map[string]string, warning string) {
    if auth != nil && len(auth.Tokens) > 0 {
        if _, has := headers["Authorization"]; has {
            warning = "headers.Authorization ignored because auth.tokens is set"
        }
        delete(headers, "Authorization")
    }
    return headers, warning
}
```

**Rationale**: 
- Avoids breaking existing configs (soft warning vs hard error)
- Clear precedence rule prevents ambiguity
- Deletion prevents duplicate Authorization headers

### 7. TokenRotator Interface Design

**Decision**: Simple interface with Next/Fail/Success methods

```go
type TokenRotator interface {
    Next() (token string, idx int, err error)
    Fail(idx int, status int)
    Success(idx int)
}

type rotatorState struct {
    tokens []string
    mu     sync.Mutex
    idx    int
    fails  []int  // per-token failure count
}
```

**Rationale**:
- `Next()` returns both token and index for tracking
- `Fail()` and `Success()` use index to update specific token state
- Mutex guards all state updates for thread safety
- Separate implementations for round-robin vs on-first-failed

### 8. HTTP RoundTripper Implementation

**Decision**: Custom authRoundTripper wraps base transport with retry logic

**Key Implementation Details**:
- Clone request for each retry using `req.Clone()` and `req.GetBody()`
- Inject token as `Authorization: Bearer <token>` header
- For on-first-failed: retry up to MaxRetries on 401/403
- For round-robin: no retry, just inject token
- Drain and close failed responses to prevent connection leaks
- Return ErrBodyNotReplayable if request body cannot be retried

**Rationale**: Transparent token injection and retry without modifying upstream code

### 9. Error Handling Strategy

**Decision**: Structured error type with sentinel for detection

```go
var ErrAllTokensFailed = errors.New("all authentication tokens failed")

type TokenExhaustedError struct {
    Attempts int
    Statuses []int
    LastErr  error
}

func (e *TokenExhaustedError) Error() string {
    return fmt.Sprintf("%v after %d attempts (statuses=%v): %v",
        ErrAllTokensFailed, e.Attempts, e.Statuses, e.LastErr)
}

func (e *TokenExhaustedError) Unwrap() error { return ErrAllTokensFailed }
```

**Security**: No token values in error messages, only status codes and attempt counts

**Rationale**: Structured error provides debugging info while preventing token leakage

### 10. SSE Connection Rotation Semantics

**Decision**: Rotation occurs on initial connection attempt only

**Behavior**:
- SSE uses same authRoundTripper for initial HTTP GET
- If initial connection returns 401/403, rotation triggers and reconnect attempts
- Once SSE stream is established, no mid-stream rotation
- If SSE disconnects and reconnects, rotation logic applies to new connection

**Testing**: Use httptest server that returns 401 on first GET, then 200 with `Content-Type: text/event-stream`

**Rationale**: SSE is connection-oriented; rotation on connect is sufficient

### 11. Test Strategy

**Unit Tests** (token_rotator_test.go):
- Round-robin cycling through tokens
- On-first-failed rotation on failure
- Failure count tracking and reset
- Thread safety with concurrent goroutines
- Token exhaustion error handling

**Integration Tests** (auth_transport_test.go):
- httptest server returning 401 then 200
- Verify correct token in Authorization header
- Verify retry behavior for on-first-failed
- Verify no retry for round-robin
- Verify all-tokens-failed error

**Proxy Tests** (proxy_transport_test.go):
- httptest proxy server
- Verify requests routed through proxy
- Verify Proxy-Authorization header
- Verify environment variable fallback
- SOCKS5 handshake verification (if implemented)

**Combined Tests** (integration_test.go):
- Token rotation through proxy
- Timeout handling with rotation
- Concurrent requests with rotation
- SSE connection with rotation

**Rationale**: Comprehensive coverage at unit, integration, and system levels

### 12. Implementation Phases

**Phase 1: Token Rotation (REQUIRED - P1)**
1. Implement TokenRotator interface and concrete types
2. Implement authRoundTripper with token injection
3. Unit tests for rotation logic
4. Integration tests with mock HTTP server
5. Update config.go to support AuthConfig
6. Add backward compatibility mapping
7. Documentation updates for token rotation

**Phase 2: Proxy Support (OPTIONAL - P2)**
1. Implement proxy transport builder
2. Add HTTP/HTTPS proxy support
3. Add SOCKS5 proxy support (golang.org/x/net/proxy)
4. Proxy integration tests
5. Update config.go to support ProxyConfig
6. Environment variable fallback
7. Documentation updates for proxy

**Phase 3: Integration (OPTIONAL - P3)**
1. Integrate token rotation with proxy
2. Combined integration tests
3. SSE connection tests with proxy
4. Concurrent request tests
5. Config validation for combined scenarios

**Rationale**: 
- Phase 1 delivers core value (token rotation) independently
- Phase 2 can be implemented later or skipped if not needed
- Phase 3 only needed if both features are implemented
- Phased approach allows incremental delivery and testing

### 13. Security Considerations

**Token Logging**:
- Never log token values
- Redact Authorization headers in debug logs
- Error messages include only token index/count

**Proxy Credentials**:
- Never log proxy passwords
- Support environment variables for sensitive values
- Sanitize error messages

**Error Messages**:
- Include status codes and attempt counts
- Exclude token values and credentials
- Provide clear failure causes without leaking secrets

**Rationale**: Security-first approach prevents credential leakage in logs and errors

## Dependencies to Add

- `golang.org/x/net/proxy` - SOCKS5 proxy support (semi-official Go extended library)

## Configuration Examples

### Round-Robin Token Rotation
```json
{
  "transportType": "sse",
  "url": "https://mcp.example.com/sse",
  "options": {
    "auth": {
      "tokens": ["tok_a", "tok_b", "tok_c"],
      "rotationMode": "round-robin"
    }
  }
}
```

### On-First-Failed with HTTP Proxy
```json
{
  "transportType": "streamable-http",
  "url": "https://mcp.example.com/api",
  "options": {
    "auth": {
      "tokens": ["tok_primary", "tok_backup"],
      "rotationMode": "on-first-failed",
      "maxRetries": 2
    },
    "proxy": {
      "url": "http://proxy.internal:8080",
      "auth": {
        "username": "proxyuser",
        "password": "proxypass"
      }
    }
  }
}
```

### SOCKS5 Proxy with Environment Fallback
```json
{
  "transportType": "sse",
  "url": "https://mcp.example.com/sse",
  "options": {
    "proxy": {
      "url": "socks5://127.0.0.1:1080",
      "useEnv": true
    }
  }
}
```

## Performance Considerations

- Token rotation adds <1ms overhead per request (mutex lock/unlock)
- Proxy routing adds network hop latency (depends on proxy location)
- Request cloning for retry adds minimal memory overhead
- Concurrent rotation is thread-safe with mutex (no data races)

## Open Questions

None - all questions resolved through Oracle consultation.
