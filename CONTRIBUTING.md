# Contributing

Thanks for helping. A few things keep eze what it is:

- **A small, fixed set of capabilities.** Static files plus the browser SDK (database, files,
  rooms, AI, identity). Proposals for custom backends, cron jobs or per-site secrets are usually
  a "no"; open an issue to discuss before writing code for a new capability.
- **One binary, no build step for sites.** No new runtime dependencies without a good reason.
- **Secure by default.** eze never runs without authentication, and new endpoints must keep
  the cross-origin and per-site isolation guarantees described in SECURITY.md.

## Development

```sh
go test ./...             # needs Go 1.26+; Node 22+ also runs the browser code checks
go test -race ./...
gofmt -l .                # should print nothing

go build -o eze ./cmd/eze
EZE_DOMAIN=localhost:8080 EZE_SCHEME=http EZE_AUTH=dev EZE_ADDR=127.0.0.1:8080 ./eze serve
```

Open http://localhost:8080 and drop a folder, or deploy with `./eze login http://localhost:8080`.
Sites are served at `http://<name>.localhost:8080` (Chrome and Firefox resolve `*.localhost`).

## Layout

| Path | What |
|---|---|
| `cmd/eze` | The binary: `serve` plus the CLI commands |
| `internal/server` | HTTP routing, auth middleware, every API, the dashboard |
| `internal/server/web` | Embedded browser code: `sdk.js` (served to sites), `app.js` and templates (dashboard) |
| `internal/sites` | Deploy archives (tar.gz, zip) and serving static files |
| `internal/store` | SQLite: sites, deploys, tokens, documents, file records, AI usage |
| `internal/uploads` | Uploaded file bytes on disk |
| `internal/auth` | OIDC, auth-proxy headers, dev mode, API tokens |
| `internal/ai` | The Claude provider behind `eze.ai` |
| `internal/cli` | The CLI commands, `eze mcp` (the MCP server), and the AGENTS.md written by `eze init` |

When you change the SDK, update `internal/cli/templates/AGENTS.md`: it is how coding agents
learn the API.
