# AGENTS.md

## Build, format, test

```bash
make build    # CGO_ENABLED=0 build to ./build/
make format   # go fix/fmt/vet/test/tidy, golangci-lint, nilaway
```

Run `make format` before proposing a change — it covers `go vet`,
`go test ./...`, and lint/nilaway in one step. Keep builds `CGO_ENABLED=0`
(already the default in the Makefile).

## Docs

Start with whichever of these matches the task:

- [docs/USAGE.md](docs/USAGE.md) — CLI flags, endpoints, health checks, auth,
  running `-authorize`.
- [docs/CONFIGURATION.md](docs/CONFIGURATION.md) — `config.json` schema,
  including the `oauth` block.
- [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) — running the proxy as a service.
- [docs/OAUTH_LIFECYCLE.md](docs/OAUTH_LIFECYCLE.md) — design notes on how
  OAuth-authorized routes are mounted and refreshed, the current
  restart-to-reauthorize gap, and the planned incremental fix. Read this
  before touching route mounting, token storage/refresh
  (`oauth.go`, `oauth_store.go`), or the startup connect loop (`http.go`).

## Working on this repo

- Small, independently-reviewable PRs over one large change
- See relevant docs when working on a section (e.g. OAUTH_LIFECYCLE when doing anything auth related)