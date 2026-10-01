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
| `github.com/coder/websocket` | ISC |
| `github.com/jackc/pgx/v5` and `github.com/jackc/puddle/v2` | MIT |
| `github.com/jackc/pgpassfile` and `github.com/jackc/pgservicefile` | MIT |
| `golang.org/x/mod`, `x/sync`, `x/text`, and `x/tools` | BSD-3-Clause |

Test-only dependencies listed in the Go module files are not linked into the
release binaries.

### coder/websocket

Copyright (c) 2025 Coder

Permission to use, copy, modify, and distribute this software for any
purpose with or without fee is hereby granted, provided that the above
copyright notice and this permission notice appear in all copies.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.

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
