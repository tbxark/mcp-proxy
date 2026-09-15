# OAuth & route lifecycle: design notes

This document describes how OAuth-authorized routes behave today, the gap
that causes the [known 404 failure mode](USAGE.md#oauth-authorizing-a-downstream-server),
and the target design for closing it. It's written as a reference for anyone
(human or agent) picking up related work, and as a map for the stack of
incremental changes that implement it — see [Delivery plan](#delivery-plan).

## Current behavior

- Every configured server is connected to exactly once, at daemon startup.
  Only on a successful connect does its HTTP route get registered; a server
  that fails to connect (most commonly: an `oauth`-configured server with no
  valid token yet) never gets a route at all, so requests to it 404 rather
  than failing with anything that explains why.
- `-authorize` runs as a separate, one-shot process. It has no way to signal
  a running daemon — no pidfile, no socket, no reload signal — so the only
  way for a newly-authorized server to start working is a full restart
  (documented today in [USAGE.md](USAGE.md#oauth-authorizing-a-downstream-server)).
- For a server whose route *is* already mounted, token rotation already
  works with no daemon involvement: the token store does a fresh disk read
  on every outgoing request, with no in-memory cache to go stale. This part
  isn't broken and this design doesn't change it.

## Problem statement

Restart-to-pick-up-authorization is the actual operational pain: running
`-authorize` should be enough to make a server work, without an operator
remembering (or automating) a restart afterward. And a 404 on a
not-yet-authorized route reads as "this route doesn't exist," not "this
route exists but needs authorization" — the failure should say what's
actually wrong.

The goal is closing both gaps without introducing new failure modes —
no crash loops on ordinary token refresh, no corrupted token files from a
lost write race, no leaked connections from repeated retries.

## Target design

1. **Every configured server's route is mounted at startup, unconditionally.**
   Connect failure no longer means "no route" — it means "route exists,
   currently failing," which is a state a request can report clearly.
2. **No credential on disk yet → fail immediately, locally, with no network
   call.** This is a plain disk check (the same one `-auth-status` already
   does), not an attempt to connect. It's what makes retrying on every
   request to a still-unauthorized server free instead of expensive: there's
   nothing to open, so there's nothing to tear down.
3. **A credential exists:**
   - **3a.** Access token still valid → use it. This is the existing,
     already-correct happy path — no change.
   - **3b.** Access token invalid → refresh it. No in-process lock around
     this — see [Known limitation](#known-limitation-concurrent-refresh-token-use)
     below for why not. The one rule that's load-bearing here: a failed
     refresh must never write anything to disk; only a successful token
     exchange writes. That's what makes a race between two refresh
     attempts (the daemon's own, a separately-run `-authorize`, or two
     concurrent requests both finding an expired token) safe without any
     coordination at all: whichever attempt actually succeeds is the only
     write that lands, and a losing attempt (e.g. `invalid_grant` from an
     already-rotated refresh token) simply produces no write to conflict
     with it. The loser just fails its own request.
4. **Disk is the sole source of truth, on every request, always.** No
   in-process cache of secret values at any point in this design.

## Explicitly rejected approaches

Worth recording so these don't get re-proposed and re-litigated later:

- **fsnotify / filesystem watching** to detect credential changes —
  unnecessary once every request already re-reads fresh from disk on demand
  (point 4). Also would have added a dependency and per-platform watcher
  semantics (inotify/kqueue/ReadDirectoryChangesW behave differently enough
  around debouncing and directory-vs-file watches to be worth avoiding) for
  no behavioral gain over "just read it when you need it."
- **Content-hash or mtime tracking** to distinguish the daemon's own writes
  from an external process's writes — solves a problem that doesn't exist
  once nothing reacts to file-change *events* in the first place; only
  point 3b's write-on-success-only rule matters, and that's enforced at the
  point of writing, not by inspecting the file afterward.
- **An in-memory token cache** — the reads involved are small, infrequent
  files; OS page caching already makes repeat reads cheap, and adding an
  explicit cache only reintroduces the invalidation problem this design
  avoids by not having one.
- **Crash-on-file-change as a reload mechanism** — a hedge against
  staleness that a no-cache design has no need for.
- **`config.json` / environment-variable hot-reload** — explicitly out of
  scope. Config changes require a restart; that's an accepted constraint,
  not a gap this document is trying to close.

## Known limitation: concurrent refresh-token use

Checked against `mcp-go` v0.56.0's actual source (`client/transport/oauth.go`):
`OAuthHandler` has no guard around token refresh. Its only `sync.Once`
dedupes OAuth server-metadata discovery, not the refresh-token grant;
`getValidToken`/`refreshToken` take no lock at all. Two concurrent requests
that both find the access token expired will independently POST to the
token endpoint with the same `refresh_token`. Against a provider that
rotates refresh tokens on use, the losing POST comes back `invalid_grant`
and that caller's request fails with a spurious "needs re-authorization" —
even though the other request's refresh just saved a good token a moment
earlier (`mcp-go`'s own test, `TestOAuthHandler_RefreshToken_SingleUseRefreshToken`,
demonstrates this exact response shape).

This can't be fixed from mcp-proxy's own code without either forking
`mcp-go` or serializing an entire server's traffic (not just the refresh):
the automatic per-request refresh path is invoked transparently, internally,
by the transport, and the only surface mcp-proxy controls — the injected
`TokenStore` — sits on the wrong side of the race (it's read *before* the
decision to refresh, and written *after* the network call, never in a
position to guard the network call itself). Making `TokenStore.GetToken`
proactively refresh so it never hands back an expired token was considered
and rejected — it would mean reimplementing a meaningful chunk of
`refreshToken`'s already-correct, already-tested logic inside mcp-proxy
just to wrap a lock around it.

**Decision: accept this as-is, not planned work.** Concurrent requests to
the same downstream service are rare in practice, and callers (MCP clients,
agent tool-call loops) generally retry a failed call on their own. The
existing write discipline (§3b: only a successful refresh ever writes)
already guarantees the failure is contained — the winner's token lands
correctly, the loser just fails its one request, nothing gets corrupted or
lost. The right long-term fix is a small upstream `mcp-go` change (a mutex
or `singleflight.Group` around `getValidToken`/`refreshToken` — it'd affect
every consumer of its OAuth support, not just this proxy), but it isn't
blocking anything in the delivery plan below and isn't being built here.

## Delivery plan

Landing as a stack of small, independently reviewable changes rather than
one large rewrite:

1. This document.
2. Extract the per-server connect/mount logic used at startup into a
   reusable function, with no behavior change — groundwork for calling it
   more than once.
3. Always-mount + fail-fast-locally for servers with no credential yet
   (design points 1–2). User-visible improvement on its own: a clear error
   instead of a bare 404, even before anything reloads without a restart.
4. Lazy connect-on-request for a not-yet-ready route, guarded by a
   per-server single-flight lock (design point 3a, still restart-free for
   the case that matters most: point 4's "run `-authorize` and it just
   works").

Refresh-token concurrency (design point 3b) is not a planned item — see
[Known limitation](#known-limitation-concurrent-refresh-token-use) above.
