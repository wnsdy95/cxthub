# Third-party notices

cxthub is licensed under Apache License 2.0, but it depends on components under
their own licenses. This file is an attribution index, not a replacement for
those licenses. Exact versions are locked in `backend/go.sum` and
`frontend/web/package-lock.json`; the CLI module has no third-party Go
dependencies.

## Go binaries

The `cxt` binary contains only the Go runtime and standard library. The server
binaries (`cxtd`, `cxt-mcp`, `cxt-worker` and `cxt-admin`) include the Go
runtime and may include the following libraries:

| Component | License |
|---|---|
| The Go standard library | BSD-3-Clause |
| `github.com/klauspost/compress` | BSD-3-Clause |
| `github.com/jackc/pgx/v5` and `github.com/jackc/puddle/v2` | MIT |
| `github.com/jackc/pgpassfile` and `github.com/jackc/pgservicefile` | MIT |
| `github.com/beevik/etree` | BSD-2-Clause |
| `github.com/crewjam/saml` | BSD-2-Clause |
| `github.com/coreos/go-oidc/v3` | Apache-2.0 |
| `github.com/go-jose/go-jose/v4` | Apache-2.0 (its `json` subpackage: BSD-3-Clause) |
| `github.com/jonboulle/clockwork` | Apache-2.0 |
| `github.com/mattermost/xml-roundtrip-validator` | Apache-2.0 |
| `github.com/russellhaering/goxmldsig` | Apache-2.0 |
| `golang.org/x/crypto`, `x/oauth2`, `x/sync`, and `x/text` | BSD-3-Clause |

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

## API and MCP native numeric conversion

The API/MCP legacy native binary64 converter adapts Go 1.26.10 standard-library
code and tables, distributed under the Go Authors' BSD-style license. Complete
original notices, license, disclaimer and source-profile provenance are retained
in `backend/internal/domain/legacy_native_float64/NOTICE.md` and `SOURCE.json`.
These files travel with the backend source export; the API/MCP Dockerfiles also
copy them into `/usr/share/doc/cxthub/legacy-native-float64/` in the runtime image.
