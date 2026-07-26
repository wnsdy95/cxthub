# Third-party notices

cxthub is licensed under Apache License 2.0, but it depends on components under
their own licenses. This file is an attribution index, not a replacement for
those licenses. Exact versions are locked in `cli/go.sum`, `backend/go.sum`, and
`frontend/web/package-lock.json`.

## Go binaries

The `cxt` and `cxtd` binaries include the Go runtime and may include the
following libraries:

| Component | License |
|---|---|
| The Go standard library | BSD-3-Clause |
| `github.com/klauspost/compress` | BSD-3-Clause |
| `github.com/jackc/pgx/v5` and `github.com/jackc/puddle/v2` | MIT |
| `github.com/jackc/pgpassfile` and `github.com/jackc/pgservicefile` | MIT |
| `golang.org/x/mod`, `x/sync`, `x/text`, and `x/tools` | BSD-3-Clause |

Test-only dependencies listed in the Go module files are not linked into the
release binaries.

## Web application

The web application directly uses these packages:

| Component | License |
|---|---|
| React and React DOM | MIT |
| TanStack Query | MIT |
| Zustand | MIT |
| Firebase JavaScript SDK | Apache-2.0 |
| Vite and `@vitejs/plugin-react` | MIT |
| TypeScript | Apache-2.0 |
| esbuild | MIT |

Transitive web dependencies use MIT, Apache-2.0, BSD-3-Clause, ISC, 0BSD, or
MPL-2.0 licenses as recorded in `frontend/web/package-lock.json`. In
particular, `lightningcss` and its platform packages are distributed under
MPL-2.0. Their unmodified source and license are available from the package
coordinates and exact version recorded in that lockfile.

When dependencies change, update this file and verify the lockfiles before a
release. Release archives include `LICENSE`, `NOTICE`, and this file.
