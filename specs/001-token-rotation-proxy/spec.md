# Feature Specification: Token Rotation and Proxy Support

**Feature Branch**: `001-token-rotation-proxy`  
**Created**: 2026-02-19  
**Status**: Draft  
**Input**: User description: "I want to added multiple token handling and the rotation mode, like round robin or on first failed. ALSO add the functionalities to route these through sock / http proxy as well."

**Note**: Token rotation is the core feature (required). Proxy support is optional and can be implemented independently or deferred to a later phase.

## User Scenarios & Testing *(mandatory per constitution)*

### User Story 1 - Multiple Token Authentication with Round-Robin (Priority: P1)

As an MCP Proxy operator, I need to distribute authentication load across multiple API tokens using round-robin rotation, so that I can avoid rate limits and maximize throughput across multiple MCP server connections.

**Why this priority**: Core functionality that enables load distribution and rate limit avoidance - essential for production deployments with high request volumes.

**Independent Test**: Can be fully tested by configuring multiple tokens, sending sequential requests, and verifying that tokens cycle A→B→C→A. Delivers immediate value for load distribution without requiring proxy or failure handling.

**Acceptance Scenarios**:

1. **Given** a client configured with tokens ["tok_a", "tok_b", "tok_c"] and mode "round-robin", **When** sending 5 sequential requests, **Then** tokens are used in order: tok_a, tok_b, tok_c, tok_a, tok_b
2. **Given** a client with round-robin mode, **When** one token receives 401 response, **Then** the failure is recorded but rotation continues normally (no retry)
3. **Given** a client with single token and no rotation mode, **When** sending requests, **Then** the same token is used for all requests (backward compatible)

---

### User Story 2 - Automatic Token Failover (Priority: P1)

As an MCP Proxy operator, I need automatic token failover when authentication fails, so that my service remains available even when individual tokens expire or are revoked.

**Why this priority**: Critical for reliability - prevents service outages when tokens fail. Must be implemented alongside P1 for complete token management.

**Independent Test**: Can be tested by configuring on-first-failed mode with a mock server that returns 401 for first token and 200 for second. Verifies automatic retry behavior works correctly.

**Acceptance Scenarios**:

1. **Given** a client configured with tokens ["tok_a", "tok_b"] and mode "on-first-failed", **When** server returns 401 for tok_a, **Then** request is automatically retried with tok_b
2. **Given** a client with on-first-failed mode, **When** all tokens return 401/403, **Then** client returns ErrAllTokensFailed with attempt count but no token values in error message
3. **Given** a client with on-first-failed mode, **When** a token succeeds after previous failure, **Then** failure count for that token is reset to zero

---

### User Story 3 - HTTP/HTTPS Proxy Support (Priority: P2)

As an MCP Proxy operator, I need to route MCP client connections through HTTP/HTTPS proxies, so that I can comply with corporate network policies and access MCP servers behind firewalls.

**Why this priority**: Important for enterprise deployments but not blocking for basic token rotation functionality. Can be developed in parallel with P1/P2.

**Independent Test**: Can be tested by configuring HTTP proxy URL, sending requests through httptest proxy server, and verifying proxy receives requests with correct headers and authentication.

**Acceptance Scenarios**:

1. **Given** a client configured with proxy URL "http://proxy.internal:8080", **When** connecting to MCP server, **Then** all requests are routed through the specified proxy
2. **Given** a proxy requiring authentication, **When** proxy credentials are configured, **Then** Proxy-Authorization header is sent correctly
3. **Given** no proxy configuration, **When** HTTP_PROXY environment variable is set, **Then** proxy is used automatically (environment fallback)

---

### User Story 4 - SOCKS5 Proxy Support (Priority: P3)

As an MCP Proxy operator, I need to route MCP client connections through SOCKS5 proxies, so that I can use advanced proxy configurations and tunnel protocols for secure remote access.

**Why this priority**: Nice-to-have for advanced use cases. HTTP proxy covers most enterprise scenarios. Can be deferred if time-constrained.

**Independent Test**: Can be tested by configuring SOCKS5 proxy URL, connecting through test SOCKS5 server, and verifying handshake and target address are correct.

**Acceptance Scenarios**:

1. **Given** a client configured with proxy URL "socks5://127.0.0.1:1080", **When** connecting to MCP server, **Then** SOCKS5 handshake is performed and connection is tunneled
2. **Given** a SOCKS5 proxy requiring authentication, **When** proxy credentials are configured, **Then** SOCKS5 username/password authentication is performed
3. **Given** SOCKS5 proxy connection failure, **When** timeout occurs, **Then** clear error message is returned without exposing credentials

---

### User Story 5 - Combined Token Rotation Through Proxy (Priority: P2)

As an MCP Proxy operator, I need token rotation to work correctly when routing through proxies, so that I can use both features together for maximum reliability and compliance.

**Why this priority**: Integration scenario ensuring P1/P2 and P3/P4 work together. Critical for real-world deployments but depends on individual features being complete.

**Independent Test**: Can be tested by configuring both token rotation and proxy, using mock servers for both, and verifying tokens rotate correctly through proxy with proper error handling.

**Acceptance Scenarios**:

1. **Given** a client with token rotation and HTTP proxy configured, **When** first token fails with 401, **Then** retry with next token is routed through same proxy
2. **Given** a client with proxy returning 407 (proxy auth required), **When** request is sent, **Then** proxy error is returned without triggering token rotation
3. **Given** a client with token rotation through proxy, **When** request timeout occurs, **Then** timeout error is returned without exhausting all tokens

---

### Edge Cases

- What happens when all tokens fail (401/403)? → Return ErrAllTokensFailed with attempt count, no token values leaked
- How does system handle concurrent requests with round-robin? → Thread-safe rotation using mutex, no race conditions
- What happens when proxy returns 407 (proxy auth required)? → Return proxy auth error, do not rotate tokens
- How does system handle request timeout during token rotation? → Return timeout error immediately, do not retry with next token
- What happens when Headers.Authorization conflicts with Auth.Tokens? → Auth.Tokens takes precedence, log warning once
- How does SSE connection handle token rotation? → Rotate on initial connection 401/403, reconnect with next token
- What happens when request body is not replayable (no GetBody)? → Return error for on-first-failed mode, cannot retry
- How does system handle empty token list? → Return ErrNoTokens immediately
- What happens when proxy URL is malformed? → Return config validation error at startup
- How does system handle SOCKS5 proxy timeout? → Return timeout error with clear message, no credential leakage
- How does system handle network errors (connection refused, timeout)? → Return error immediately, do not rotate tokens
- How does system handle 5xx server errors? → Return error immediately, do not rotate tokens (server issue, not auth)
- How does system handle 429 rate limit errors? → Return error immediately, do not rotate tokens (rate limit, not auth failure)
- What happens with duplicate tokens in list? → System allows duplicates, rotates through them normally
- What happens when rotation mode specified but tokens list is empty? → Return ErrNoTokens at startup validation
- What happens when maxRetries exceeds token count? → Use token count as effective limit (cycle through all tokens once)

## Requirements *(mandatory per constitution)*

### Functional Requirements

- **FR-001**: System MUST support multiple authentication tokens per MCP client configuration
- **FR-002**: System MUST support "round-robin" rotation mode that cycles through tokens sequentially per request
- **FR-003**: System MUST support \"on-first-failed\" rotation mode that retries with next token on 401/403 responses, up to maxRetries limit (default: number of tokens)
- **FR-004**: System MUST detect authentication failures by HTTP status codes 401 (Unauthorized) and 403 (Forbidden)
- **FR-005**: System MUST track failure counts per token and reset on successful authentication
- **FR-006**: System MUST return ErrAllTokensFailed when all tokens are exhausted without leaking token values
- **FR-007**: System SHOULD support HTTP/HTTPS proxy configuration per MCP client (optional feature)
- **FR-008**: System SHOULD support SOCKS5 proxy configuration per MCP client (optional feature)
- **FR-009**: System SHOULD support proxy authentication (username/password) for both HTTP and SOCKS5 proxies (if proxy feature implemented)
- **FR-010**: System SHOULD fall back to HTTP_PROXY/HTTPS_PROXY environment variables when no proxy config is specified (if proxy feature implemented)
- **FR-011**: System MUST inject authentication tokens as "Authorization: Bearer <token>" header
- **FR-012**: System MUST handle conflicts between Headers.Authorization and Auth.Tokens by preferring Auth.Tokens with warning
- **FR-013**: System MUST be thread-safe for concurrent requests with token rotation
- **FR-014**: System MUST preserve backward compatibility with existing AuthTokens field
- **FR-015**: System MUST validate configuration at startup and return clear errors for invalid proxy URLs or conflicting auth settings

### Key Entities *(include if feature involves data)*

- **TokenRotator**: Manages token rotation state (current index, failure counts), provides Next/Fail/Success methods
- **AuthConfig**: Configuration for authentication (tokens list, rotation mode, max retries)
- **ProxyConfig**: Configuration for proxy routing (URL, type, authentication, environment fallback)
- **authRoundTripper**: HTTP transport wrapper that injects tokens and handles retry logic
- **rotatorState**: Shared state for token rotation (tokens, index, failure counts, mutex)

## Success Criteria *(mandatory per constitution)*

### Measurable Outcomes

- **SC-001**: Token rotation correctly cycles through all configured tokens in specified mode (verified by integration tests)
- **SC-002**: Authentication failures (401/403) trigger automatic retry with next token in on-first-failed mode (verified by mock server tests)
- **SC-003**: All requests are correctly routed through configured proxy when proxy is configured (verified by proxy server logs)
- **SC-004**: Test coverage remains ≥76.5% baseline after implementation (enforced by pre-commit hook)
- **SC-005**: No authentication tokens or proxy credentials appear in logs or error messages (verified by security audit)
- **SC-006**: Concurrent requests with round-robin rotation show no race conditions (verified by go test -race)
- **SC-007**: Existing configurations without token rotation continue to work unchanged (backward compatibility verified by regression tests)
- **SC-008**: Configuration validation catches invalid proxy URLs and conflicting auth settings at startup (verified by validation tests)
