# Configuration

This project uses the v2 JSON configuration below. The v1 schema (top-level
`server` and `clients` keys) has been removed: rename them to `mcpProxy` and
`mcpServers`, and lift each client's `config` object into the server entry
itself, so that

```jsonc
{"clients": {"fetch": {"type": "stdio", "config": {"command": "uvx", "args": ["mcp-server-fetch"]}}}}
```

becomes

```jsonc
{"mcpServers": {"fetch": {"command": "uvx", "args": ["mcp-server-fetch"]}}}
```

`panicIfInvalid`, `logEnabled` and `authTokens` move into the entry's `options`,
and the proxy's `globalAuthTokens` become `mcpProxy.options.authTokens`.

- Online converter (build Claude config from your proxy): https://tbxark.github.io/mcp-proxy

## Full Example

```jsonc
{
  "mcpProxy": {
    "baseURL": "https://mcp.example.com",
    "addr": ":9090",
    "name": "MCP Proxy",
    "version": "1.0.0",
    "type": "streamable-http", // or "sse" (default)
    "options": {
      "panicIfInvalid": false,
      "logEnabled": true,
      "authTokens": ["DefaultToken"]
    }
  },
  "mcpServers": {
    "github": {
      // stdio client
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": { "GITHUB_PERSONAL_ACCESS_TOKEN": "<YOUR_TOKEN>" },
      "options": {
        "toolFilter": {
          "mode": "block",
          "list": ["create_or_update_file"]
        }
      }
    },
    "fetch": {
      // stdio client
      "command": "uvx",
      "args": ["mcp-server-fetch"],
      "timeout": "60s", // bound each request so one wedged call cannot take the server down
      "options": {
        "panicIfInvalid": true,
        "logEnabled": false,
        "authTokens": ["SpecificToken"]
      }
    },
    "amap": {
      // SSE client
      "url": "https://mcp.amap.com/sse",
      "headers": { "Authorization": "Bearer <YOUR_TOKEN>" },
      "options": {
        "disabled": true
      }
    },
    "parallel-search": {
      // Optional: sends search queries and requested URLs to Parallel.ai
      "url": "https://search.parallel.ai/mcp",
      "transportType": "streamable-http"
    },
    "notion": {
      // streamable-http client requiring interactive OAuth (no static
      // bearer token accepted) - see "oauth" below
      "url": "https://mcp.notion.com/mcp",
      "transportType": "streamable-http",
      "oauth": {
        "scopes": []
      }
    }
  }
}
```

## mcpProxy

- `baseURL`: Public URL base used to build client endpoints.
- `addr`: Bind address (e.g. `:9090`).
- `name`, `version`: Server identity for MCP handshake.
- `type`: `sse` (default) or `streamable-http`.
- `options`: Defaults inherited by `mcpServers.*.options` (can be overridden per server).
- `startupGracePeriod` (duration string, default `"30s"`): how long `/_readyz` reports
  `initializing` while clients are still connecting. After it, the proxy reports ready
  and any straggler mounts when it finishes, so one slow server cannot keep the whole
  proxy out of rotation.

## mcpServers

Each entry's key is the server name, which becomes its route
(`<baseURL path>/<serverName>/`). It must be a clean relative URL path: it
cannot be empty, contain `.`, `..` or empty segments, start or end with `/`,
or contain `\`, `{`, `}` or control characters. Multiple segments are allowed
(see the token-in-the-route trick in `USAGE.md`).

Each entry defines a downstream MCP server. Supported client types:

- `stdio` (implicit when `command` is set): run a subprocess via stdio.
- `sse` (implicit when `url` is set and `transportType` ≠ `streamable-http`): connect via Server‑Sent Events.
- `streamable-http` (requires `transportType: "streamable-http"`): connect via HTTP streaming.

Common fields:

- `command`, `args`, `env` — for `stdio` clients.
- `url`, `headers` — for `sse` and `streamable-http` clients. Prefer `headers`
  for credentials (e.g. `"Authorization": "Bearer <token>"`). Some servers only
  accept a token in the URL query (see `amap` below); the proxy redacts
  credentials from the error messages and logs it controls, but a preference for
  headers avoids relying on that and keeps tokens out of access logs.
- `timeout` — request timeout for a single downstream request. Write it as a
  duration string: `"timeout": "30s"`. A bare number means **nanoseconds**
  (`"timeout": 30` is 30ns, not 30 seconds), so anything under a millisecond is
  rejected at startup rather than silently failing every request — except on
  `stdio`, where such a value is discarded and the server runs unbounded, so a
  timeout copied in from a Claude config cannot break startup.
  On `stdio` a timeout is worth setting: one pipe carries every request, so a
  request the server accepts and never answers keeps that pipe busy, the
  keepalive probe stops getting replies, and the whole downstream is dropped as
  unhealthy for every caller until the proxy restarts. It bounds tool calls,
  resource reads, and prompt fetches alike.
- `oauth` — for `sse` and `streamable-http` clients that require interactive OAuth instead of (or in addition to) `headers` (see below).
- `options` — per‑server overrides and filters (see below).

## oauth

Some remote MCP servers (e.g. Notion's hosted MCP) require the full OAuth
2.1 authorization-code flow and reject static bearer tokens outright. Set
an `oauth` block on an `sse`/`streamable-http` server to have mcp-proxy act
as the OAuth client on the downstream connection:

- `clientId`, `clientSecret` (optional): static client credentials. Omit
  both to use RFC 7591 dynamic client registration, which is performed
  automatically the first time you authorize.
- `redirectUri` (optional): local callback URL used during the one-time
  interactive authorization. Defaults to `http://localhost:8090/oauth/callback`.
  It must use `http`, a loopback host (`localhost`, `127.0.0.1`, or `::1`), an
  explicit port, and a non-root callback path. The callback listener never
  binds to a public interface.
- `scopes` (optional): OAuth scopes to request.
- `pkceDisabled` (bool, optional): disable PKCE. PKCE is enabled by default.
- `authServerMetadataUrl` (optional): override discovery of the authorization
  server's metadata document. Needed for providers whose protected-resource
  metadata (RFC 9728) advertises an `authorization_servers` entry with a
  non-empty path (e.g. `https://mcp.example.com/v1/mcp`): this library's
  discovery always appends `/.well-known/oauth-authorization-server` after
  the full issuer URL (OpenID Connect Discovery convention), but RFC 8414
  requires inserting it *before* the path when one is present, and some
  providers (Datadog, at time of writing) only serve the document at the
  RFC 8414 location. If servers connected via `oauth` fail discovery,
  check `<issuer>/.well-known/oauth-authorization-server` vs.
  `<scheme>://<host>/.well-known/oauth-authorization-server<path>` by hand
  and set this field to whichever one responds.

Tokens are persisted to `<user config dir>/mcp-proxy/oauth/<server>.json`
(e.g. `~/.config/mcp-proxy/oauth/notion.json` on Linux) and refreshed
automatically using the stored refresh token as they expire. Before the
daemon can use an `oauth`-configured server, you must authorize it once —
see the `-authorize` flag in [USAGE.md](USAGE.md).

When `clientId` is left empty, the dynamically-registered client (RFC
7591) is also persisted, to `<user config dir>/mcp-proxy/oauth/<server>@client.json`.
This matters: registration only happens inside the one-off `-authorize`
process, so without persisting it, a freshly started daemon would build a
new OAuth handler with an empty client ID and every token refresh would
silently be rejected by the provider once the access token expires -
looking, from the logs, like a refresh failure with no obvious cause.
(Releases before this used `<server>.client.json`; that name is still read as a
fallback, so an existing registration keeps working.)

## options

- `panicIfInvalid` (bool): If true, startup fails when a client cannot initialize.
- `pingInterval` (duration string, default `"30s"`): how often the connection is probed
  to keep it alive and to notice it died. This is how quickly `/_readyz` turns
  `degraded` after a downstream goes away.
- `logEnabled` (bool): Log requests and events for this client.
- `authTokens` ([]string): Valid bearer tokens; requests must include `Authorization: <token>`.
- `toolFilter` (object): Selectively expose tools to the proxy:
  - `mode`: `allow` or `block`.
  - `list`: List of tool names.
  - `mode: "allow"` exposes only the listed tools, so an empty (or omitted) `list`
    exposes **no** tools. `mode: "block"` hides the listed tools, so an empty list
    hides nothing. The filter applies to tools only — a downstream's resources and
    prompts are always exposed.
- `Disabled` (bool): Enable or disable this server. Disabled servers are skipped at startup.
- `autoReconnect` (bool, default `false`): Keep a downstream connection alive across
  failures. When true, a server that is unreachable at startup is retried every
  `reconnectInterval` until it connects (its route is mounted then), and a connection
  that drops is rebuilt in place instead of staying `degraded` until the proxy is
  restarted. Off by default: with the health endpoints, the usual deployment contract is
  that an orchestrator restarts the proxy, so a proxy that repairs itself by default
  would hide the failure. Ignored when `panicIfInvalid` is true, which keeps failing fast.
- `reconnectInterval` (duration string, default `"15s"`): gap between attempts while a
  downstream with `autoReconnect` is unreachable. Only meaningful with `autoReconnect`.
  It applies to the **startup** retry loop, not to a connection that drops later: a drop
  is discovered by the keepalive probe and rebuilt in place, so its recovery time is
  governed by `pingInterval` (the probe period) and the consecutive-failure threshold,
  not by `reconnectInterval`. With the defaults (30s probe, three failures) a dropped
  connection recovers in roughly 90s plus connect time; shorten `pingInterval` to recover
  faster.

Notes:

- `mcpProxy.options.authTokens` serves as the default token set if a server omits `options.authTokens`.
- `mcpProxy.options.toolFilter` likewise serves as the default filter if a server omits
  `options.toolFilter`. The other `mcpProxy.options` values (`panicIfInvalid`, `logEnabled`,
  `pingInterval`, `autoReconnect`, `reconnectInterval`) are defaults for servers that omit
  them, so a fleet-wide setting can be declared once. `disabled` is per-server only and is
  not inherited.
- To discover tool names for filtering, start without a filter and check logs for lines like `<server> Adding tool <name>`.
