# Configuration

This project supports a v2 JSON configuration. v1 configs are automatically migrated at load time.

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
      "options": {
        "panicIfInvalid": true,
        "logEnabled": false,
        "authTokens": ["SpecificToken"]
      }
    },
    "amap": {
      // SSE client
      "url": "https://mcp.amap.com/sse?key=<YOUR_TOKEN>",
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

## mcpServers

Each entry defines a downstream MCP server. Supported client types:

- `stdio` (implicit when `command` is set): run a subprocess via stdio.
- `sse` (implicit when `url` is set and `transportType` ≠ `streamable-http`): connect via Server‑Sent Events.
- `streamable-http` (requires `transportType: "streamable-http"`): connect via HTTP streaming.

Common fields:

- `command`, `args`, `env` — for `stdio` clients.
- `url`, `headers` — for `sse` and `streamable-http` clients.
- `timeout` — request timeout for `streamable-http`.
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
  Must include an explicit port.
- `scopes` (optional): OAuth scopes to request.
- `pkceDisabled` (bool, optional): disable PKCE. PKCE is enabled by default.

Tokens are persisted to `<user config dir>/mcp-proxy/oauth/<server>.json`
(e.g. `~/.config/mcp-proxy/oauth/notion.json` on Linux) and refreshed
automatically using the stored refresh token as they expire. Before the
daemon can use an `oauth`-configured server, you must authorize it once —
see the `-authorize` flag in [USAGE.md](USAGE.md).

## options

- `panicIfInvalid` (bool): If true, startup fails when a client cannot initialize.
- `logEnabled` (bool): Log requests and events for this client.
- `authTokens` ([]string): Valid bearer tokens; requests must include `Authorization: <token>`.
- `toolFilter` (object): Selectively expose tools to the proxy:
  - `mode`: `allow` or `block`.
  - `list`: List of tool names.
- `Disabled` (bool): Enable or disable this server. Disabled servers are skipped at startup.

Notes:

- `mcpProxy.options.authTokens` serves as the default token set if a server omits `options.authTokens`.
- To discover tool names for filtering, start without a filter and check logs for lines like `<server> Adding tool <name>`.

