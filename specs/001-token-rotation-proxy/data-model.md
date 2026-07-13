# Data Model: Token Rotation and Proxy Support

**Date**: 2026-02-19  
**Feature**: 001-token-rotation-proxy

## Core Entities

### TokenRotator (Interface)

**Purpose**: Manages token rotation state and provides thread-safe token selection

**Methods**:
- `Next() (token string, idx int, err error)` - Returns next token to use and its index
- `Fail(idx int, status int)` - Records authentication failure for token at index
- `Success(idx int)` - Records successful authentication, resets failure count

**Implementations**:
- `roundRobinRotator` - Cycles through tokens sequentially per request
- `onFirstFailedRotator` - Reuses current token until failure, then advances

**State**:
- `tokens []string` - List of authentication tokens
- `idx int` - Current token index
- `fails []int` - Per-token failure counts
- `mu sync.Mutex` - Protects concurrent access

**Lifecycle**: Created per MCP client, lives for client lifetime

---

### AuthConfig (Configuration)

**Purpose**: Configuration for authentication token rotation

**Fields**:
- `Tokens []string` - List of authentication tokens (required if using rotation)
- `RotationMode string` - Rotation strategy: "round-robin" or "on-first-failed"
- `MaxRetries optional.Field[int]` - Maximum retry attempts (default: len(Tokens))

**Validation Rules**:
- If `Tokens` is empty, no auth injection occurs
- If `Tokens` has one element and no `RotationMode`, treat as static token
- If `Tokens` has multiple elements and no `RotationMode`, default to "round-robin" with warning
- `RotationMode` must be "round-robin" or "on-first-failed" if specified
- `MaxRetries` must be > 0 if specified

**Relationships**:
- Embedded in `OptionsV2`
- Maps from legacy `AuthTokens []string` field for backward compatibility

---

### ProxyConfig (Configuration)

**Purpose**: Configuration for proxy routing

**Fields**:
- `URL string` - Proxy URL including scheme (http://, https://, socks5://)
- `Type string` - Optional proxy type override ("http", "https", "socks5")
- `Auth *ProxyAuth` - Optional proxy authentication credentials
- `UseEnv optional.Field[bool]` - Whether to fall back to environment variables (default: true)

**Validation Rules**:
- `URL` must be valid URL if specified
- Scheme must be http, https, or socks5
- If `Type` is specified, must match URL scheme or override it
- If `Auth` is specified, `URL` must be specified

**Relationships**:
- Embedded in `OptionsV2`
- Used to build per-client `http.Transport`

---

### ProxyAuth (Configuration)

**Purpose**: Proxy authentication credentials

**Fields**:
- `Username string` - Proxy username
- `Password string` - Proxy password

**Security**:
- Never logged or included in error messages
- Stored in memory only during client lifetime
- Should be loaded from environment variables in production

---

### authRoundTripper (Transport Wrapper)

**Purpose**: HTTP transport wrapper that injects authentication tokens and handles retries

**Fields**:
- `base http.RoundTripper` - Underlying transport (default: http.DefaultTransport)
- `rotator TokenRotator` - Token rotation manager
- `mode string` - Rotation mode ("round-robin" or "on-first-failed")
- `maxRetries int` - Maximum retry attempts

**Behavior**:
- Clones incoming request
- Calls `rotator.Next()` to get token
- Injects `Authorization: Bearer <token>` header
- Executes request via base transport
- On 401/403: calls `rotator.Fail()` and retries if mode is "on-first-failed"
- On success: calls `rotator.Success()`
- On network error: returns immediately without rotation

**Lifecycle**: Created per MCP client, wraps client's HTTP transport

---

### rotatorState (Internal State)

**Purpose**: Shared state for token rotation implementations

**Fields**:
- `tokens []string` - Token list (immutable after creation)
- `mu sync.Mutex` - Protects mutable fields
- `idx int` - Current token index (0 to len(tokens)-1)
- `fails []int` - Failure count per token (same length as tokens)

**Thread Safety**: All access to `idx` and `fails` must be protected by `mu`

**State Transitions**:
- `Next()`: Returns token at current index, advances index (round-robin only)
- `Fail()`: Increments failure count for token, advances index (on-first-failed only)
- `Success()`: Resets failure count for token to zero

---

### TokenExhaustedError (Error Type)

**Purpose**: Structured error when all tokens fail authentication

**Fields**:
- `Attempts int` - Number of authentication attempts made
- `Statuses []int` - HTTP status codes received (401, 403)
- `LastErr error` - Last error received (may be nil)

**Methods**:
- `Error() string` - Returns formatted error message
- `Unwrap() error` - Returns `ErrAllTokensFailed` sentinel

**Security**: Does not include token values, only metadata

---

## State Diagrams

### Round-Robin Token Rotation

```
[Start] → Next() → Token A (idx=0) → Request → Success/Fail
                                                    ↓
                   Next() → Token B (idx=1) → Request → Success/Fail
                                                    ↓
                   Next() → Token C (idx=2) → Request → Success/Fail
                                                    ↓
                   Next() → Token A (idx=0) → ... (cycle continues)
```

**Key**: Index advances on every `Next()` call, regardless of success/failure

---

### On-First-Failed Token Rotation

```
[Start] → Next() → Token A (idx=0) → Request → Success → Next() → Token A (reuse)
                                              ↓
                                            401/403
                                              ↓
                                         Fail(idx=0)
                                              ↓
                                         Next() → Token B (idx=1) → Request → Success
                                                                            ↓
                                                                      Next() → Token B (reuse)
```

**Key**: Index advances only on authentication failure (401/403)

---

### Token Exhaustion Flow

```
[Start] → Token A → 401 → Token B → 401 → Token C → 401 → ErrAllTokensFailed
          (idx=0)         (idx=1)         (idx=2)         (attempts=3, statuses=[401,401,401])
```

**Key**: After all tokens fail, return structured error with metadata

---

## Configuration Schema Evolution

### V2 (Current - Before This Feature)

```go
type OptionsV2 struct {
    PanicIfInvalid optional.Field[bool]
    LogEnabled     optional.Field[bool]
    AuthTokens     []string  // Unused field
    ToolFilter     *ToolFilterConfig
    Disabled       bool
}
```

### V2 (After This Feature - Backward Compatible)

```go
type OptionsV2 struct {
    PanicIfInvalid optional.Field[bool]
    LogEnabled     optional.Field[bool]
    AuthTokens     []string          // Legacy - mapped to Auth.Tokens
    Auth           *AuthConfig       // NEW
    Proxy          *ProxyConfig      // NEW
    ToolFilter     *ToolFilterConfig
    Disabled       bool
}
```

**Migration Path**:
- Existing configs with `AuthTokens` continue to work (mapped to `Auth.Tokens`)
- New configs should use `Auth` and `Proxy` nested structs
- If both `AuthTokens` and `Auth.Tokens` are set, `Auth.Tokens` takes precedence with warning

---

## Validation Rules Summary

### AuthConfig Validation

1. If `Tokens` is empty, no validation needed (no auth injection)
2. If `RotationMode` is specified, must be "round-robin" or "on-first-failed"
3. If `MaxRetries` is specified, must be > 0
4. If `MaxRetries` is not specified, defaults to `len(Tokens)`
5. `MaxRetries` semantics: Maximum total authentication attempts across all tokens (not per-token)
6. If `Tokens` contains duplicate values, log warning (may indicate misconfiguration)
7. If `RotationMode` is specified but `Tokens` is empty, return error (invalid config)

### ProxyConfig Validation (Optional Feature)

1. If `URL` is specified, must be valid URL with scheme (http, https, socks5)
2. If `Type` is specified, must be "http", "https", or "socks5"
3. If `Auth` is specified, `URL` must also be specified
4. If `UseEnv` is false and `URL` is empty, no proxy is used (proxy is optional)
5. Proxy configuration is independent of auth configuration (can be used separately)

### Header Conflict Validation

1. If `Headers["Authorization"]` exists and `Auth.Tokens` is non-empty, log warning and delete `Headers["Authorization"]`
2. `Auth.Tokens` always takes precedence over `Headers["Authorization"]`

### Edge Case Validation

1. **Non-replayable request bodies**: If request has body without `GetBody`, return `ErrBodyNotReplayable` on retry
2. **Proxy auth failure (407)**: Return immediately without token rotation
3. **Network errors**: Return immediately without token rotation (connection refused, timeout, DNS failure)
4. **5xx server errors**: Return immediately without token rotation (not auth failures)
5. **SSE auth failure**: Detect 401/403 in SSE stream, trigger rotation for next request
6. **Empty token in list**: Skip empty tokens, log warning
7. **All tokens empty**: Treat as no auth configured

---

## Memory and Performance Characteristics

### Memory Usage

- `rotatorState`: ~100 bytes + (len(tokens) * ~50 bytes per token)
- `authRoundTripper`: ~200 bytes + base transport size
- Request cloning: Temporary allocation during retry (freed after request completes)

**Example**: 10 tokens = ~600 bytes per client

### Performance Impact

- Token rotation: <1ms per request (mutex lock/unlock + array access)
- Request cloning: <1ms for typical requests (depends on body size)
- Proxy routing: Network latency (depends on proxy location, typically 1-50ms)

### Concurrency

- Thread-safe: Multiple goroutines can call `Next()`, `Fail()`, `Success()` concurrently
- No data races: All shared state protected by mutex
- No deadlocks: Mutex held only during state updates (no nested locks)

---

## Error Handling

### Error Types

1. `ErrNoTokens` - Token list is empty
2. `ErrAllTokensFailed` - All tokens returned 401/403
3. `ErrBodyNotReplayable` - Request body cannot be retried (no GetBody)
4. `ErrInvalidRotationMode` - Unknown rotation mode in config
5. `ErrInvalidProxyURL` - Malformed proxy URL

### Error Propagation

- Network errors: Returned immediately without rotation
- Auth errors (401/403): Trigger rotation, return after exhaustion
- Proxy errors (407): Returned immediately without rotation
- Timeout errors: Returned immediately without rotation

---

## Testing Considerations

### Unit Test Coverage

- Token rotation logic (round-robin, on-first-failed)
- Failure count tracking and reset
- Thread safety (concurrent Next/Fail/Success calls)
- Token exhaustion error generation

### Integration Test Coverage

- HTTP mock server returning 401 then 200
- Verify correct token in Authorization header
- Verify retry behavior matches rotation mode
- Proxy routing through httptest proxy
- Environment variable fallback

### Edge Cases

- Empty token list
- Single token (no rotation)
- All tokens fail
- Concurrent requests with rotation
- Request body not replayable
- Proxy auth failure (407)
- Timeout during rotation
- Headers.Authorization conflict
