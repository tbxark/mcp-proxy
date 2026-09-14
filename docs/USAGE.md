# Usage

## CLI

```text
-config string         path to config file or a http(s) url (default "config.json")
-expand-env            expand environment variables in config file (default true)
-http-headers string   optional headers for config URL: 'Key1:Value1;Key2:Value2'
-http-timeout int      timeout (seconds) for remote config fetch (default 10)
-insecure              skip TLS verification for remote config
-authorize string      run a one-time interactive OAuth authorization for the
                        named mcpServers entry, then exit
-check-config          load and validate the config, then exit
-log-level value       log level: debug, info, warn, or error (default info)
-version               print version and exit
-help                  print help and exit
```

## Validating configuration

Use `-check-config` in CI, init containers, or deployment scripts to validate
the proxy settings and every downstream server without binding the HTTP port:

```bash
mcp-proxy -config config.json -check-config
# Config OK: 3 MCP server(s) configured
```

Validation includes transport requirements, absolute HTTP URLs, OAuth callback
safety, authentication tokens, and tool-filter modes. Invalid configuration
exits non-zero with the affected field or server name.

## Endpoints

Given `mcpProxy.baseURL = https://mcp.example.com` and a server key `fetch`:

- For `type: sse`: `https://mcp.example.com/fetch/sse`
- For `type: streamable-http`: `https://mcp.example.com/fetch/mcp`

## Health checks

Two unauthenticated endpoints are always served for liveness/readiness probes
(Docker, reverse proxies, dashboards, monitoring):

- `/_healthz` (liveness) returns `200` as soon as the process serves requests. It
  is about this process only, so it stays `200` even when a downstream is down.
- `/_readyz` (readiness) returns `503` with `"status":"initializing"` until every
  enabled server has finished connecting and mounting its route, then `200`.
- `/_readyz` returns `503` again with `"status":"degraded"` if a downstream that
  had connected later stops answering: each connection is probed every 30s, and
  the servers that failed are listed in `unhealthy`. It takes three failed probes
  in a row, so a busy single-threaded server that misses one probe does not take
  the proxy out of rotation. Servers speaking the modern protocol (2026-07-28,
  which removed ping) are probed with a real `tools/list` request; legacy
  connections are still pinged.
- `/_readyz` returns `503` with `"status":"unavailable"` if no enabled server is
  mounted at all. Nothing can be named as broken in that case, but every MCP
  route would `404`.
- `GET` returns a JSON status document; `HEAD` returns the same code with an
  empty body. `serverCount` counts enabled servers only.

```bash
curl http://127.0.0.1:9090/_healthz
# {"name":"MCP Proxy","serverCount":3,"status":"ok","version":"1.0.0"}

curl http://127.0.0.1:9090/_readyz
# {"name":"MCP Proxy","serverCount":3,"status":"degraded","unhealthy":["notion"],"version":"1.0.0"}
```

A server that never connected at startup is *not* reported as unhealthy: it has
no route, and keeping the whole proxy out of rotation would take the working
servers down with it. Use `-doctor` or the startup logs to find those.

By default a server that is down at startup stays down until the proxy is
restarted, and a connection that drops stays `degraded`. Set `autoReconnect: true`
on a server (or in `mcpProxy.options` for all of them) to have it repair itself:
the proxy retries an unreachable backend every `reconnectInterval` (default `15s`)
and mounts its route once it connects, and it rebuilds a connection that drops.
This trades the external-restart contract for in-process recovery, which suits a
host where the proxy outlives the app it fronts (e.g. started at login).

The two cases use different timings. A backend that was never up is retried on
`reconnectInterval`. A connection that drops after being healthy is noticed by the
keepalive probe instead, so its recovery is governed by `pingInterval` and the
three-consecutive-failure threshold — about 90s with the defaults. Lower
`pingInterval` to detect and rebuild a drop sooner.

These endpoints never require the proxy auth token, which also means the
`unhealthy` list exposes your server names to anyone who can reach the port.
Bind the proxy to an internal address, or keep the health endpoints on an
internal route in your reverse proxy, if those names are sensitive.

## Auth

If `options.authTokens` is set for a server, requests must include the token in
the `Authorization` header. Both forms are accepted, and the scheme name is
case-insensitive:

```
Authorization: Bearer <token>
Authorization: <token>
```

If your client cannot set headers, embed the token in the route key (e.g. `fetch/<token>`) and call that path instead.

## OAuth-authorizing a downstream server

For servers configured with an `oauth` block (see [CONFIGURATION.md](CONFIGURATION.md#oauth)),
run the authorization flow once, by hand, before starting (or restarting)
the daemon:

```bash
mcp-proxy -authorize notion -config path/to/config.json
```

This opens your default browser to the provider's consent screen, waits
for the local redirect callback, exchanges the code for a token, and saves
it to disk. Run it interactively, in a session with a real browser -
never from an unattended service/container, since it requires you to log
in and approve access.

Once authorized, (re)start the daemon. A server's HTTP route is only
mounted on a successful connect at startup, so if the daemon was already
running when you authorized, restart it now - the new token won't be
picked up otherwise. After that, tokens refresh automatically as they
expire with no further restarts needed. Re-run `-authorize` only if the
server reports the token is no longer valid (e.g. access was revoked).
