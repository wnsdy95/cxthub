# CXTHub API backend

Private generated distribution for the `cxtd` API service. The complete backend
Go module remains at `backend/`; shared contracts, fixtures, and migrations
remain at `schemas/`. The root `Dockerfile` is copied from the source
`deploy/Dockerfile`. The module path is still `github.com/wnsdy95/cxthub/backend`.

The public [CXTHub monorepo](https://github.com/wnsdy95/cxthub) is the only source
of truth. This repository contains an allowlisted snapshot of one exact source
commit, without source Git history. `.cxthub-source.json` records the source SHA
and exported file provenance. Private distribution does not make the
public source confidential. **Never edit this mirror.** Submit changes and
reviews through the [monorepo](https://github.com/wnsdy95/cxthub/blob/main/CONTRIBUTING.md).

## Build and check

Use the Go version declared in `backend/go.mod` (Go 1.26.9 or later). From the
repository root:

```bash
go build -C backend ./...
go build -C backend -tags postgres ./...
go vet -C backend ./...
go vet -C backend -tags postgres ./...
go build -C backend -tags postgres -o ../bin/cxtd ./cmd/cxtd
docker build -f Dockerfile -t cxthub-api:local .
docker build -f Dockerfile.migrate -t cxthub-migrate:local .
```

CI builds and vets both default and `postgres` variants and builds the API
binary and container without pushing an image. **It does not run the full
backend test suite:** cross-module tests require files intentionally absent
from this export. Full cross-module, PostgreSQL, and end-to-end tests remain
mandatory in the monorepo before publishing. Green mirror CI is a packaging
check, not independent proof of integration correctness or deployment approval.

## Manual deployment only

Render uses this repository's `main`, root `Dockerfile`, and root build context
for `cxthub-api`. The source Blueprint keeps automatic deployment off. Both API
and MCP must use the same existing PostgreSQL database, Firebase project, and
public frontend origin. PostgreSQL is shared infrastructure, not another source
repository. Do not create a new database to switch code repositories.

Before API/MCP/worker startup, explicitly apply and verify the exact schema with
the separate `cxthub-migrate:local` image and privileged operator credentials.
Its default is check-only; applying requires `--apply`. Runtime startup is
verification-only and must not have migration authority. See
[`docs/DATABASE_MIGRATIONS.md`](docs/DATABASE_MIGRATIONS.md) for coordinated
drain, public namespace, history/checksum checks, and rollback limits.

After all four generated repositories are verified against the same source
commit and their CI succeeds, deploy the exact exported API commit manually.
Follow the source [distribution handoff](https://github.com/wnsdy95/cxthub/blob/main/docs/REPOSITORY_DISTRIBUTION.md)
and [operations guide](https://github.com/wnsdy95/cxthub/blob/main/deploy/RENDER.md).
Keep production secrets in the existing service configuration, never in Git or
the export. No sync credential belongs in this repository. Shared `LICENSE`,
`NOTICE`, and third-party notices are exported from the monorepo.
