# Deployment

## Docker

Run with a local config file mounted into the container:

```bash
docker run -d \
  -p 9090:9090 \
  -v /path/to/config.json:/config/config.json \
  ghcr.io/tbxark/mcp-proxy:latest
```

Or reference a remote config URL:

```bash
docker run -d -p 9090:9090 \
  ghcr.io/tbxark/mcp-proxy:latest \
  --config https://example.com/config.json
```

The image supports launching MCP servers via `npx` and `uvx` out of the box.

## Docker Compose

Minimal compose file:

```yaml
services:
  app:
    image: ghcr.io/tbxark/mcp-proxy:latest
    pull_policy: always
    volumes:
      - ./config.json:/config/config.json
    ports:
      - "9090:9090"
    restart: always
```

Serving the config via an internal file server (no host mount into `app`):

```yaml
services:
  caddy:
    image: caddy:latest
    pull_policy: always
    expose:
      - "80"
    volumes:
      - ./config.json:/config/config.json
    command: ["caddy", "file-server", "--root", "/config"]

  app:
    image: ghcr.io/tbxark/mcp-proxy:latest
    pull_policy: always
    ports:
      - "9090:9090"
    restart: always
    depends_on:
      - caddy
    command: ["--config", "http://caddy/config.json"]
```

## Reverse proxy

When mcp-proxy listens on loopback behind nginx/Caddy, forward the public Host that matches the hostname of `mcpProxy.baseURL` (or use `localhost`): on loopback connections any other Host value is rejected with `403 Forbidden` as DNS-rebinding protection.

## Security Notes

- Prefer `authTokens` per downstream server; only use the `mcpProxy` default when appropriate.
- If an MCP client cannot set headers, you can embed a token in the route key (e.g. `fetch/<token>`) and route via that path.
- Set `options.panicIfInvalid: true` for critical servers to fail fast on misconfiguration.

