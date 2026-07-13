# Implementation Plan: Token Rotation and Proxy Support

**Branch**: `001-token-rotation-proxy` | **Date**: 2026-02-19 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `/specs/001-token-rotation-proxy/spec.md`

**Note**: This plan follows TDD methodology with comprehensive Oracle consultation for architecture design and edge case coverage.

## Summary

Add multiple authentication token support with two rotation modes (round-robin and on-first-failed) as the core feature. Optionally add HTTP/HTTPS/SOCKS5 proxy routing for MCP client connections. Implementation uses per-client TokenRotator with mutex-guarded state, custom HTTP RoundTripper for token injection and retry logic. Proxy support (optional) uses per-client transport configuration. All features maintain backward compatibility with existing AuthTokens field and preserve 76.5% test coverage baseline.

**Core Feature**: Token rotation (required)  
**Optional Feature**: Proxy support (can be implemented independently or deferred)

## Technical Context

**Language/Version**: Go 1.24.0  
**Primary Dependencies**: 
- github.com/mark3labs/mcp-go v0.44.0 (MCP protocol client/server)
- github.com/go-sphere/confstore v0.0.4 (configuration loading)
- golang.org/x/net/proxy (SOCKS5 support - to be added)

**Storage**: N/A (stateless proxy, token rotation state is in-memory per client)  
**Testing**: Go testing framework with httptest for mock servers, go test -race for concurrency  
**Target Platform**: Linux server (Docker containers), multi-arch (amd64, arm64)  
**Project Type**: Single project (Go binary)  
**Performance Goals**: 
- <10ms p95 overhead for token injection
- Support 1000+ concurrent connections with thread-safe rotation
- No performance degradation with proxy routing

**Constraints**: 
- Maintain 76.5% test coverage baseline (enforced by pre-commit hook)
- Backward compatible with existing AuthTokens field
- No token values in logs or error messages (security requirement)
- Must work in Docker with environment variable proxy configuration

**Scale/Scope**: 
- Support 10+ tokens per client
- Handle 1000+ req/s with token rotation
- Support multiple concurrent clients each with independent rotation state

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

Verify compliance with `.specify/memory/constitution.md`:

- [x] **Test Coverage Discipline**: Plan includes unit tests (TokenRotator logic), integration tests (HTTP mock servers for auth/proxy), contract tests (MCP protocol compliance), and race detection (go test -race for concurrent rotation)
- [x] **Code Quality Gates**: Implementation will pass golangci-lint, nilaway (nil checks for rotator), go vet, go fmt
- [x] **Documentation-First**: Feature documentation in docs/CONFIGURATION.md and docs/USAGE.md will be drafted before implementation, including JSON config examples
- [x] **Backward Compatibility**: Existing AuthTokens field preserved and mapped to new Auth.Tokens; single-token configs work without rotation mode; no breaking changes to config schema
- [x] **Docker-First Deployment**: Feature supports environment variable proxy configuration (HTTP_PROXY, HTTPS_PROXY); Docker image will include golang.org/x/net/proxy dependency

**Complexity Justifications** (if any principle violations):
- None - all constitution principles are satisfied

## Project Structure

### Documentation (this feature)

```text
specs/[###-feature]/
├── plan.md              # This file (/speckit.plan command output)
├── research.md          # Phase 0 output (/speckit.plan command)
├── data-model.md        # Phase 1 output (/speckit.plan command)
├── quickstart.md        # Phase 1 output (/speckit.plan command)
├── contracts/           # Phase 1 output (/speckit.plan command)
└── tasks.md             # Phase 2 output (/speckit.tasks command - NOT created by /speckit.plan)
```

### Source Code (repository root)

```text
# Single project structure (Go binary)
/
├── client.go                    # Existing - will add token rotation integration
├── client_test.go               # Existing - will add rotation tests
├── config.go                    # Existing - will extend OptionsV2 with Auth/Proxy configs
├── config_test.go               # Existing - will add config validation tests
├── http.go                      # Existing - HTTP server implementation
├── http_test.go                 # Existing - HTTP server tests
├── integration_test.go          # Existing - will add proxy integration tests
├── token_rotator.go             # NEW - TokenRotator interface and implementations
├── token_rotator_test.go        # NEW - Unit tests for rotation logic
├── auth_transport.go            # NEW - authRoundTripper HTTP wrapper
├── auth_transport_test.go       # NEW - Integration tests for auth injection
├── proxy_transport.go           # NEW - Proxy configuration and transport builder
├── proxy_transport_test.go      # NEW - Proxy routing tests
├── errors.go                    # NEW - ErrAllTokensFailed and related errors
└── docs/
    ├── CONFIGURATION.md         # UPDATE - Add auth and proxy config examples
    └── USAGE.md                 # UPDATE - Add token rotation and proxy usage examples
```

**Structure Decision**: Single project structure maintained. New files added for token rotation (token_rotator.go), auth transport wrapper (auth_transport.go), and proxy support (proxy_transport.go). Each new file has corresponding _test.go for unit/integration tests. Existing client.go and config.go will be extended to integrate new features.

## Complexity Tracking

> **No violations** - All constitution principles satisfied

**Note on Optional Proxy Feature**:
- Proxy support is optional and can be implemented independently
- Core token rotation feature delivers value without proxy
- Proxy can be deferred to later phase if time-constrained
- Implementation phases designed for incremental delivery

## Phase 0: Research (COMPLETED)

**Oracle Consultations**: 2 iterations completed
- Architecture design for token rotation and proxy
- Interface definitions and implementation details
- Edge case identification and test strategy
- Security considerations and error handling

**Key Decisions**:
- Per-client TokenRotator with mutex-guarded state
- HTTP RoundTripper wrapper for token injection
- Two rotation modes: round-robin and on-first-failed
- Proxy support via http.Transport configuration
- Backward compatible config schema with nested Auth/Proxy

**Artifacts Generated**:
- [research.md](./research.md) - Complete research findings
- [data-model.md](./data-model.md) - Entity definitions and state diagrams
- [contracts/config-schema.json](./contracts/config-schema.json) - JSON schema
- [quickstart.md](./quickstart.md) - User guide with examples

## Phase 1: Design (COMPLETED)

**Data Model**: Defined in [data-model.md](./data-model.md)
- TokenRotator interface with Next/Fail/Success methods
- AuthConfig and ProxyConfig configuration structs
- authRoundTripper transport wrapper
- rotatorState internal state management
- TokenExhaustedError structured error type

**API Contracts**: Defined in [contracts/config-schema.json](./contracts/config-schema.json)
- JSON schema for configuration validation
- Examples for all rotation modes and proxy types
- Backward compatibility with legacy AuthTokens field

**Quickstart Guide**: Created in [quickstart.md](./quickstart.md)
- Configuration examples for all scenarios
- Testing procedures
- Troubleshooting guide
- Security best practices

## Phase 2: Implementation Planning

**Implementation Order** (from research.md):

### Phase 1: Token Rotation (REQUIRED - Core Feature)
1. Write unit tests for TokenRotator interface and concrete types (token_rotator_test.go)
2. Write integration tests with mock HTTP server (auth_transport_test.go)
3. Implement TokenRotator interface and concrete types (token_rotator.go)
4. Implement authRoundTripper with token injection (auth_transport.go)
5. Update config.go to support AuthConfig
6. Add backward compatibility mapping (AuthTokens → Auth.Tokens)
7. Update docs/CONFIGURATION.md and docs/USAGE.md

**Deliverable**: Token rotation fully functional and tested

### Phase 2: Proxy Support (OPTIONAL - Can be deferred)
1. Write proxy integration tests (proxy_transport_test.go)
2. Implement proxy transport builder (proxy_transport.go)
3. Add HTTP/HTTPS proxy support
4. Add SOCKS5 proxy support with golang.org/x/net/proxy
5. Update config.go to support ProxyConfig
6. Environment variable fallback (HTTP_PROXY, HTTPS_PROXY)
7. Update documentation

**Deliverable**: Proxy routing fully functional and tested

### Phase 3: Integration (OPTIONAL - Only if Phase 2 implemented)
1. Integrate token rotation with proxy
2. Combined integration tests
3. SSE connection tests with proxy
4. Concurrent request tests
5. Config validation for combined scenarios

**Deliverable**: Token rotation and proxy work together seamlessly

## Test Coverage Plan

**Unit Tests** (target: 80%+ coverage):
- token_rotator_test.go: Rotation logic, failure tracking, thread safety
- auth_transport_test.go: Token injection, retry logic, error handling
- proxy_transport_test.go: Proxy configuration, transport building
- config_test.go: Config validation, backward compatibility

**Integration Tests** (target: Full scenario coverage):
- HTTP mock server returning 401 then 200
- Verify correct token in Authorization header
- Verify retry behavior matches rotation mode
- Proxy routing through httptest proxy
- Environment variable fallback
- SSE connection with rotation

**Edge Cases** (from research.md):
- Empty token list
- Single token (no rotation)
- All tokens fail (exhaustion)
- Concurrent requests with rotation
- Request body not replayable
- Proxy auth failure (407)
- Timeout during rotation
- Headers.Authorization conflict
- Network errors (connection refused, timeout) - no rotation
- 5xx server errors - no rotation
- 429 rate limit errors - no rotation
- Duplicate tokens in list
- Rotation mode with empty tokens
- maxRetries exceeds token count

## Success Criteria

- [ ] Token rotation works for round-robin mode
- [ ] Token rotation works for on-first-failed mode
- [ ] Backward compatibility with AuthTokens field
- [ ] Test coverage ≥76.5% baseline maintained
- [ ] All linting passes (golangci-lint, nilaway, go vet, go fmt)
- [ ] No token values in logs or error messages
- [ ] Documentation complete (CONFIGURATION.md, USAGE.md)
- [ ] Quickstart guide validated with real examples
- [ ] (Optional) HTTP/HTTPS proxy support working
- [ ] (Optional) SOCKS5 proxy support working
- [ ] (Optional) Combined token rotation + proxy working

## Next Steps

1. **Review this plan** with stakeholders
2. **Prioritize phases**: Decide if proxy support is needed for v1
3. **Begin Phase 1 implementation**: Token rotation (core feature)
4. **Create tasks.md**: Break down implementation into specific tasks
5. **Start TDD cycle**: Write tests first, then implement
