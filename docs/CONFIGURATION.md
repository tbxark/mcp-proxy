# Configuration

This project uses the JSON configuration below.

- Online converter (build Claude config from your proxy): https://tbxark.github.io/mcp-proxy

## Full Example

```jsonc
{
  "mcpProxy": {
    "baseURL": "https://mcp.example.com",
    "addr": ":9090",
    "name": "MCP Proxy",
    "version": "1.0.0",
    "type": "streamable-http", // (default) or "sse"
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
        },
        "resourceFilter": {
          "mode": "allow",
          // a template and a concrete resource; every other resource is hidden
          "list": ["repo://{owner}/{repo}/contents{/path*}", "repo://README"]
        },
        "promptFilter": {
          "mode": "block",
          "list": ["issue_triage"]
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
      // SSE client with the token in the URL query (amap only accepts a key this way)
      "transportType": "sse",
      "url": "https://mcp.amap.com/sse?key=<YOUR_TOKEN>"
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
- `type`: `streamable-http` (default) or `sse`.
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
- `streamable-http` (implicit when `url` is set and `transportType` ≠ `sse`): connect via HTTP streaming.
- `sse` (requires `transportType: "sse"`): connect via Server‑Sent Events.

Common fields:

- `command`, `args`, `env` — for `stdio` clients.
- `url`, `headers` — for `sse` and `streamable-http` clients. Prefer `headers`
  for credentials (e.g. `"Authorization": "Bearer <token>"`). Some servers only
  accept a token in the URL query (see `amap` below); the proxy redacts
  credentials from the error messages and logs it controls, but a preference for
  headers avoids relying on that and keeps tokens out of access logs.
- `timeout` — request timeout for a single downstream request. Write it as a
  duration string: `"timeout": "30s"`. A bare number is rejected — the same
  rule applies on `stdio` — because it reads as seconds to most people while
  decoding as nanoseconds.
  On `stdio` a timeout is worth setting: one pipe carries every request, so a
  request the server accepts and never answers keeps that pipe busy, the
  keepalive probe stops getting replies, and the whole downstream is dropped as
  unhealthy for every caller until the proxy restarts. It bounds tool calls,
  resource reads, and prompt fetches alike.
- `oauth` — for `sse` and `streamable-http` clients that require interactive OAuth instead of (or in addition to) `headers` (see below).
- `options` — per‑server overrides and filters (see below).

When a downstream server announces that its tools, prompts, or resources
changed (`notifications/*/list_changed`), the proxy re-lists and re-publishes
that server's catalog, so downstream changes are picked up live without a
restart. Two limitations apply: a legacy `streamable-http` downstream only
delivers notifications that ride on an in-flight request's response stream,
and mcp-go-based downstreams older than v1.2.0 never send list_changed to
modern streamable sessions, so for those the catalog still only refreshes on
reconnect. Progress and logging
notifications, and server-initiated sampling, elicitation, and roots requests,
are not forwarded.

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
  explicit port, and a non-root callback path (see `callbackListen` for a
  public `https` URL). The callback listener never binds to a public
  interface.
- `callbackListen` (optional): loopback `host:port` the callback listener
  binds to, instead of the host and port of `redirectUri`. For a machine
  without a browser, such as a headless server. `redirectUri` is then
  required, and may also be an `https` URL on any host, with or without a
  port: a reverse proxy on the same machine serves it and forwards the
  callback path, unchanged, to `callbackListen`. See
  [USAGE.md](USAGE.md#authorizing-on-a-machine-without-a-browser).
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

The client is registered for one `redirectUri`, which is saved with it. The
provider refuses a sign-in that uses the client with any other, and it does so
in the browser, where `-authorize` only sees a timeout. So when `redirectUri`
has changed, or the client was saved by a version that did not record it yet,
`-authorize` signs in with a newly registered client, and saves it in place of
the old one once the sign-in is approved.

## options

- `panicIfInvalid` (bool): If true, startup fails when a client cannot initialize.
- `pingInterval` (duration string, default `"30s"`): how often the connection is probed
  to keep it alive and to notice it died. This is how quickly `/_readyz` turns
  `degraded` after a downstream goes away.
- `logEnabled` (bool): Log each incoming HTTP request (method and path) for this server.
- `authTokens` ([]string): Valid bearer tokens; requests must include
  `Authorization: Bearer <token>` (a bare token is also accepted).
- `toolFilter` (object): Selectively expose tools to the proxy:
  - `mode`: `allow` or `block`.
  - `list`: List of tool names.
  - `mode: "allow"` exposes only the listed tools and requires a non-empty `list`.
    `mode: "block"` hides the listed tools. The filter applies to tools only;
    use `resourceFilter` and `promptFilter` for the rest of the catalog.
- `resourceFilter` (object): Selectively expose resources and resource templates.
  Same shape as `toolFilter` (`mode` and `list`). A list entry matches a resource by
  its `uri` or `name`, and a resource template by its `uriTemplate` or `name`.
  In `block` mode a read is also refused when it targets a hidden resource (listed by
  URI, or hidden by name once the resource list has been fetched) through an exposed
  template that matches it. Blocking a template hides only that template: a URI it
  would produce stays readable through another exposed template that matches it. The check
  compares normalized URIs (fragment dropped, scheme and host lower-cased,
  percent-encoded unreserved characters decoded), but a downstream that maps other
  spellings to the same resource can still serve it; use `allow` mode when the filter
  is a security boundary. In `allow` mode, allowing a template allows every URI it
  matches, including resources that are not allowed individually.
- `promptFilter` (object): Selectively expose prompts by name. Same shape as
  `toolFilter`.
- `disabled` (bool): Enable or disable this server. Disabled servers are skipped at startup.
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
  One kind of drop does not wait for the threshold: a `streamable-http` downstream that
  restarts forgets the session the proxy holds, and that is final - no later probe can
  succeed on it. With `autoReconnect` the connection is rebuilt as soon as the lost session
  is noticed, by the keepalive probe or by a forwarded request; such a request is then
  retried once on the new connection, which is safe because the server refused it before
  any handler ran. Without `autoReconnect` the client is reported unhealthy right away and
  stays unhealthy until the proxy is restarted.

Notes:

- `mcpProxy.options.authTokens` serves as the default token set if a server omits `options.authTokens`.
- `mcpProxy.options.toolFilter`, `resourceFilter` and `promptFilter` likewise serve as the
  default filters if a server omits them; a per-server filter replaces the inherited one
  entirely. The other `mcpProxy.options` values (`panicIfInvalid`, `logEnabled`,
  `pingInterval`, `autoReconnect`, `reconnectInterval`) are defaults for servers that omit
  them, so a fleet-wide setting can be declared once. `disabled` is per-server only;
  setting it on `mcpProxy.options` is rejected.
- To discover tool names for filtering, start without a filter and check logs for lines
  like `<server> Adding tool <name>`. Resources, templates and prompts are logged the
  same way (`Adding resource`, `Adding resource template`, `Adding prompt`) at debug level.
