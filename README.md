# MCP Proxy Server

[![Go Report Card](https://goreportcard.com/badge/github.com/tbxark/mcp-proxy)](https://goreportcard.com/report/github.com/tbxark/mcp-proxy)
[![Go Coverage](https://img.shields.io/badge/coverage-76.5%25-brightgreen)](coverage_badge.go)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

An MCP proxy that aggregates multiple MCP servers behind a single HTTP entrypoint.

## Features

- Proxy multiple MCP clients: aggregate tools, prompts, and resources from many servers.
- SSE and streamable HTTP: serve via Server‑Sent Events or streamable HTTP.
- Flexible config: supports `stdio`, `sse`, and `streamable-http` client types.

## Documentation

- Configuration: [docs/configuration.md](docs/CONFIGURATION.md)
- Usage: [docs/usage.md](docs/USAGE.md)
- Deployment: [docs/deployment.md](docs/DEPLOYMENT.md)
- Claude config converter: https://tbxark.github.io/mcp-proxy

## Quick Start

### Build from source

```bash
git clone https://github.com/tbxark/mcp-proxy.git
cd mcp-proxy
make build
./build/mcp-proxy --config path/to/config.json
```

### Install via Go

```bash
go install github.com/tbxark/mcp-proxy@latest
```

### Docker

The image includes support for launching MCP servers via `npx` and `uvx`.

```bash
docker run -d -p 9090:9090 -v /path/to/config.json:/config/config.json ghcr.io/tbxark/mcp-proxy:latest
# or provide a remote config
docker run -d -p 9090:9090 ghcr.io/tbxark/mcp-proxy:latest --config https://example.com/config.json
```

More deployment options (including docker‑compose) are in [docs/deployment.md](docs/DEPLOYMENT.md).

## Configuration

See full configuration reference and examples in [docs/configuration.md](docs/CONFIGURATION.md).
An online Claude config converter is available at: https://tbxark.github.io/mcp-proxy


## Usage

Command‑line flags, endpoints, and auth examples are documented in [docs/usage.md](docs/USAGE.md).

## Thanks

- This project was inspired by the [adamwattis/mcp-proxy-server](https://github.com/adamwattis/mcp-proxy-server) project
- If you have any questions about deployment, you can refer to  [《在 Docker 沙箱中运行 MCP Server》](https://miantiao.me/posts/guide-to-running-mcp-server-in-a-sandbox/)([@ccbikai](https://github.com/ccbikai))

## License

This project is licensed under the MIT License. See the [LICENSE](LICENSE) file for details.
