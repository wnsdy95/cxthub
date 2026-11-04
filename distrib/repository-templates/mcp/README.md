# CXTHub MCP service

Private generated distribution for `cxt-mcp`, the independent MCP and OAuth
service. The full backend Go module is retained at `backend/` and shared
contracts, fixtures, and migrations at `schemas/`. This is not an extracted or
renamed Go module: its path remains `github.com/wnsdy95/cxthub/backend`. The root
`Dockerfile` is copied from the source `deploy/Dockerfile.mcp`.

The public [CXTHub monorepo](https://github.com/wnsdy95/cxthub) is the only source
of truth. This private repository is an allowlisted snapshot of one exact source
commit, without source Git history; the original source remains public.
`.cxthub-source.json` records the source SHA and exported file provenance.
**Never edit this mirror.** Make changes through the
[monorepo contribution flow](https://github.com/wnsdy95/cxthub/blob/main/CONTRIBUTING.md).

## Build and check

Use the Go version in `backend/go.mod` (Go 1.26.9 or later). From the repository
root:

```bash
go build -C backend ./...
go build -C backend -tags postgres ./...
go vet -C backend ./...
go vet -C backend -tags postgres ./...
go build -C backend -tags postgres -o ../bin/cxt-mcp ./cmd/cxt-mcp
docker build -f Dockerfile -t cxthub-mcp:local .
```

CI builds and vets default and `postgres` variants and builds the MCP binary and
container without publishing. **The full backend tests are not run here:** some
depend on the sibling CLI or other monorepo-only inputs. Full cross-module,
PostgreSQL, OAuth, and full-stack tests must pass in the monorepo before export.
A green packaging workflow does not prove full integration correctness and is
not a deployment authorization.

## Manual deployment only

Render uses this repository's `main`, root `Dockerfile`, and root context for
`cxthub-mcp`. Automatic deployment stays off. MCP requires PostgreSQL, including
locally; do not substitute a filesystem store. It shares the existing database,
Firebase project, and public origin with the API. OAuth records still require
database writes even though MCP tools are read-only. Startup only verifies the
current migration bundle; it never applies migrations. Run the backend mirror's
operator migration image before starting either service.

Verify all four generated repositories against the same source commit and wait
for their CI before manually deploying the exact exported MCP commit. Preserve
the existing database and public issuer to avoid changing client identity.
PostgreSQL is shared infrastructure, not a fifth source repository. Follow the
source [distribution handoff](https://github.com/wnsdy95/cxthub/blob/main/docs/REPOSITORY_DISTRIBUTION.md)
and [operations guide](https://github.com/wnsdy95/cxthub/blob/main/deploy/RENDER.md).
Production and sync secrets must never enter this mirror. Shared `LICENSE`,
`NOTICE`, and third-party notices are exported from the monorepo.
