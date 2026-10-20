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
- `redact` (object, default off): Scan what this server returns for credentials and
  replace each one before it reaches the client. See [redact](#redact) below.
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

- `mcpProxy.options.redact` is inherited by every server that has no `redact` block of
  its own. A server's block replaces the inherited one as a whole (no field merging), so
  `"redact": {"enabled": false}` turns it off for one server.
- `mcpProxy.options.authTokens` serves as the default token set if a server omits `options.authTokens`.
- `mcpProxy.options.toolFilter` likewise serves as the default filter if a server omits
  `options.toolFilter`. The other `mcpProxy.options` values (`panicIfInvalid`, `logEnabled`,
  `pingInterval`, `autoReconnect`, `reconnectInterval`) are defaults for servers that omit
  them, so a fleet-wide setting can be declared once. `disabled` is per-server only and is
  not inherited.
- To discover tool names for filtering, start without a filter and check logs for lines like `<server> Adding tool <name>`.

## redact

Tool results, resource contents and prompt messages can carry credentials that a
downstream read from somewhere else: a chat message with a password, a pasted `.env`
file, a log line with a token. With `redact` enabled, the proxy scans that text and
replaces every finding with `[REDACTED:<kind>]` before the response leaves the proxy.

```jsonc
"options": {
  "redact": {
    "enabled": true,
    "highEntropy": false,        // optional, see below
    "disableRules": ["jwt"]      // optional, finding kinds to keep visible
  }
}
```

`disableRules` works on the kind a finding is reported as: `["password"]` leaves every
`password` finding visible, whichever rule found it.

What is scanned:

- `text` content of a tool result, the text of embedded resources, resource links
  (`uri`, `title`, `description`), nested `tool_result` content, `tool_use` input, and
  `structuredContent` (every string value, and numbers under a secret member name);
- `resources/read` contents with `text`;
- blobs (embedded or read) whose `mimeType` is textual (`text/*`, JSON, XML, YAML and
  similar), up to 8 MiB decoded; they are decoded, scanned and re-encoded;
- the description and text content of `prompts/get` results;
- the message of an error the downstream returns for any of these calls.

Images, audio and other binary blobs pass through untouched, and so does `_meta`.

When a text value is itself a JSON document (most MCP servers return JSON as text), the
proxy rewrites it string by string: escapes such as `\n` or `\u0070` are decoded before
matching, member names give context (`"password": "..."`), and everything else in the
document, including key order and spacing, is left byte-for-byte as it was. A number
stored under a secret member (`"pin": 123456`) becomes the marker string, which can
make the result fail a strict `outputSchema` check in a client that validates it.
Everything inside an object, array or embedded JSON string stored under a secret member
counts as secret (`{"token": {"access": "...", "refresh": "..."}}`), except members that
describe it such as `type`, `scope`, `expires_in` or `username`. JSON nested
inside JSON strings is followed five levels deep; deeper levels are scanned as plain
text.

Rule kinds:

| Kind | Matches |
|---|---|
| `private_key` | PEM private key blocks (RSA, EC, OpenSSH, PGP, PKCS#8), including a block cut off before its END line |
| `aws_access_key`, `aws_secret_key` | `AKIA…`/`ASIA…` key ids; a 40-character secret after `aws_secret_access_key` |
| `github_token`, `gitlab_token` | `ghp_`/`gho_`/`ghu_`/`ghs_`/`ghr_`, `github_pat_`; `glpat-` and the other GitLab prefixes |
| `anthropic_api_key`, `openai_api_key` | `sk-ant-…`; `sk-proj-…`, `sk-svcacct-…`, `sk-admin-…`, legacy `sk-…` |
| `google_api_key`, `google_oauth_token` | `AIza…`; `ya29.…` access tokens, `GOCSPX-…` client secrets, `1//0…` refresh tokens |
| `slack_token`, `slack_webhook` | `xoxb-`/`xoxp-`/… and `xapp-` tokens; `hooks.slack.com` webhook URLs |
| `telegram_bot_token` | `<bot id>:<35 characters>` |
| `stripe_key` | `sk_live_`, `sk_test_`, `rk_live_`, `rk_test_` |
| `jwt` | three base64url segments starting with `eyJ` |
| `auth_header` | the credential in `Authorization: Bearer/Basic/Token/Digest …` |
| `url_password` | the password in `scheme://user:password@host` (the user name stays) |
| `env_secret`, `password` | `NAME=value` lines whose name contains a secret word (`PASSWORD`, `SECRET`, `TOKEN`, `KEY`, `AUTH`, `DSN`, …); password-like names report `password` |
| `password`, `secret` | `password: …`, `pwd=…`, `api_key = …`, `token: …` and similar pairs in prose, YAML, INI and query strings, and JSON members such as `"client_secret": "…"` |
| `high_entropy` | only with `highEntropy: true`: tokens of 24+ characters mixing upper case, lower case and digits with high Shannon entropy |

Trade-offs, chosen to keep ordinary output readable:

- Values that are obviously not secrets are kept: shorter than 3 characters, variable
  references (`${DB_PASSWORD}`, `$TOKEN`; `$ecretP4ss` still counts as a password),
  templates (`{{ token }}`, `<your-token>`), masks (`****`) and words such as `none`,
  `null`, `expired`.
- Names that describe a secret rather than hold it are skipped: `TOKEN_URL`,
  `PASSWORD_MIN_LENGTH`, `API_KEY_ID`, `PUBLIC_KEY`, `PWD=/path`. A bare `token` member
  counts, but pagination names such as `nextPageToken` or `page_token` do not, and
  neither do flags such as `isSecret` or `has_password`, or data keys such as `SORT_KEY`.
- Known false positives: a member whose last word is `pass` (`boardingPass`), and on a
  `NAME=value` line the whole rest of the line, so `client_secret=abc&x=1` hides `&x=1`
  too.
- Hex digests, UUIDs, numbers and dates on their own are not matched by the specific
  rules (after a label such as `token:` they are).
  `highEntropy` also skips them (they lack mixed case), and skips tokens inside paths and
  URLs (`spaces/AAAA/messages/BBBB`) and SSH public keys, but it does match other opaque
  ids such as Telegram file ids. A tool that takes such an id back as an argument (a
  media download) then stops working, which is why it is off by default.

Limits: redaction finds secrets by their shape. It does not catch a password written as
plain prose ("the code is the name of my cat"), a secret that is base64-encoded,
reversed or split across several messages, a token format it does not know (unless
`highEntropy` happens to catch it), or a name and a value stored in separate members
(`{"name": "DB_PASSWORD", "value": "..."}`). Treat it as a guard against accidental exposure, not
as a boundary against a downstream that tries to leak data.

Each response with findings is logged at info level with the server, method, tool or
resource name, count and kinds. Values are never logged.

### Custom replacement

The replacement text comes from a `RedactionHook`
(`Replace(ctx, RedactionSource, RedactionFinding) string`). The default returns
`[REDACTED:<kind>]`. A custom build can install its own hook with `SetRedactionHook`
from an `init` function in an extra file, for example to keep the value in a vault and
return a reference to it, without changing `redact.go`. The hook receives the server
name, MCP method, tool name or resource URI, the request arguments and the matched value.
The value is the secret as written, decoded when it sat inside a JSON string. The hook
runs on request goroutines, concurrently, so it must be safe for concurrent use.
