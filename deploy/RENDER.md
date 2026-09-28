# Vercel + Render deployment

## Services and ownership

```text
Browser / CLI / MCP client
           |
     Vercel public origin
           |
           +-- /api/* ---------------------------> Render API: cxtd :8907
           |                                         |
           +-- /api/v1/oauth/requests/* ----+          |
           +-- /mcp, /oauth/*, /.well-known/*          |
                                          |          |
                               Render MCP: cxt-mcp :8908
                                          |          |
                                          +----------+
                                           PostgreSQL
```

The consent-specific API rule precedes `/api/*`. `/connect/mcp` is a frontend
route. Browsers use the public Vercel origin throughout login and consent.
The API still validates that browser Origin behind the proxy: the Blueprint
sets `CXT_CORS_ORIGINS` from API's own `CXT_PUBLIC_URL` and fixes
`CXT_COOKIE_SECURE=1`. Keep SameSite=Lax and leave the cookie domain unset.

- `cxtd`: REST, sessions, ingestion, GitHub callbacks/webhooks and all background
  workers (promotion, Git evidence, email, notifications, maintenance).
- `cxt-mcp`: MCP queries and OAuth registration, consent, token exchange and
  revocation. No GitHub credentials or email configuration are loaded.
- Shared application/domain code and PostgreSQL are the consistency boundary;
  the services do not replicate business rules or communicate over a private
  REST hop. Each has its own connection pool and shutdown lifecycle.
- OAuth/rate-limit records are writable operational data. Read-only MCP tools
  do not imply a read-only database account. Startup migrations also need DDL.

## Provision

1. Import the root `render.yaml` as a Render Blueprint. It declares two paid
   web services (`pro`: API 4 GB; `standard`: MCP 2 GB) and a PostgreSQL 16
   `0.1c-256mb` instance in Singapore.
   Review current plans before provisioning; these are starting sizes, not a
   production load guarantee. Use the same region for services and database.
2. Enter `CXT_FIREBASE_PROJECT` and `CXT_PUBLIC_URL` on the API service. The latter
   is the HTTPS **Vercel frontend/custom domain**, without a path, not a Render
   service origin. MCP references these values from API; both must stay equal.
   After changing the public URL, sync the Blueprint so both MCP and the API's
   trusted Origin reference update. References refresh on Blueprint sync, not
   immediately when a source environment value changes. Do not replace the
   exact Origin with a wildcard or disable CSRF validation.
3. The Blueprint injects the same internal PostgreSQL connection string into
   both services. External DB access is disabled (`ipAllowList: []`). Docker
   images enforce PostgreSQL and bundle the migration directory. Concurrent
   starts serialize migrations using the existing PostgreSQL advisory lock.
4. Add GitHub App secrets and optional Resend settings **only to API**, following
   [GitHub connections](../docs/GITHUB_CONNECTIONS.md) and `.env.example`. Empty
   integration settings leave those optional features disabled. Server workers
   do not read a developer's `gh` credentials.
5. Import `frontend/web` in Vercel as Vite. Set:

   | Variable | Value |
   |---|---|
   | `CXT_API_ORIGIN` | API service HTTPS origin, e.g. `https://your-api.onrender.com` |
   | `CXT_MCP_ORIGIN` | Different MCP service HTTPS origin, e.g. `https://your-mcp.onrender.com` |
   | `VITE_FIREBASE_API_KEY` | Firebase web application API key |
   | `VITE_FIREBASE_AUTH_DOMAIN` | Firebase authorized auth domain |
   | `VITE_FIREBASE_PROJECT_ID` | Same Firebase project as both servers |

   Do not set `VITE_API_BASE` for this same-origin deployment. Vercel rejects
   missing or identical API/MCP origins. Add the public domain to Firebase's
   authorized domains and configure GitHub provider callbacks separately.
6. Wait for `/healthz` on both Render services to return 200. Each checks its
   own database pool, with 503 on database failure. Redeploy Vercel so rewrites
   contain the real Render origins. Connect clients to `https://<public>/mcp`.

The single public issuer preserves existing MCP registrations and tokens when
moving an existing deployment **with its existing database**. Changing the
public domain/resource requires reconnecting clients. Do not point MCP at an
empty database or a different Firebase project.

## Migrating from combined cxtd

Deploy the new MCP service against the existing PostgreSQL database first.
Verify discovery and readiness directly, then deploy the Vercel routing split.
Only then replace the API image with the version that removes MCP/OAuth routes.
This order avoids an OAuth outage. Upgrade both images from the same revision;
roll out backward-compatible schema changes before removing old application
versions. To roll back, restore the old combined API and then revert the proxy.

A local development FS store is not a shared production database. This change
neither migrates nor deletes existing FS data. Export/import and verify it before
moving that dataset to PostgreSQL. Never run two processes against the same FS
store. A standalone MCP server refuses startup without PostgreSQL even locally.

For an existing personal-repository FS dataset, use the offline
[FS transfer rehearsal](FS_TRANSFER.md) before planning the production cutover.

## Local verification

Use a disposable PostgreSQL database and build both binaries with `postgres`:

```bash
go build -C backend -tags postgres -o ../bin/cxtd ./cmd/cxtd
go build -C backend -tags postgres -o ../bin/cxt-mcp ./cmd/cxt-mcp
export CXT_POSTGRES_DSN='<disposable local database DSN>'
export CXT_AUTH=dev CXT_PUBLIC_URL=http://127.0.0.1:5173
export CXT_MIGRATIONS_DIR="$PWD/schemas/db/migrations"
bin/cxtd serve --addr 127.0.0.1:8907
# In a second terminal with the same variables:
bin/cxt-mcp serve --addr 127.0.0.1:8908
```

Vite defaults to API :8907 and MCP :8908. For browser tests, the harness builds
and starts both real executables on isolated ports. Use an empty test database;
fixtures are written and retained:

```bash
cd frontend/web
CXT_E2E_FULLSTACK=1 CXT_E2E_DSN='<test DSN>' npm run test:e2e
```

`TestPGIndependentMCP` additionally verifies independent pool/server restarts,
private-repository permission revocation, token revocation across services and
absence of API mutation routes on MCP. The browser test covers API login,
MCP OAuth consent, PKCE, read-only calls and account-screen disconnection.

## Operations

The [sync validation](SYNC_VALIDATION.md) reduced peak resident memory for one
163 MiB stored conversation from about 1.45 GiB to 1.08 GiB. This remains above the 512 MB
`starter` limit. The API budget also leaves room for publication/indexing and
other requests; the MCP budget covers cold index reads after data transfer.
These are initial validation budgets, not concurrency/load guarantees. Warm the
selected branch reads and measure representative concurrent traffic in staging
before launch. Separating services does not remove per-request allocation costs.

Render's current equivalent plan IDs are `2c-4g` and `1c-2g`; legacy names remain
supported. As of 2026-09-28, these service plans list at $85 and $25/month, plus
the separate PostgreSQL plan (currently $6/month), storage, traffic and any Vercel
charges. Review pricing before importing the paid Blueprint. Editing this file
does not provision resources. See [compute specifications](https://render.com/docs/compute-plans),
[plan naming compatibility](https://render.com/docs/compute-plans-update), and
[current pricing](https://render.com/pricing).

API workers need an always-running service; the Blueprint uses paid services
rather than a sleeping free instance. Keep at least one API instance alive for
retry/maintenance progress. MCP remains readable during an API restart, but new
web logins and background processing wait for API availability. A database
outage affects both services and fails readiness/authorization closed.

Budget database connections across **both** pools and all replicas. pgx accepts
`pool_max_conns` and `connect_timeout` in each DSN; configure per-service limits
when scaling. Use direct Postgres connections for startup advisory locks and
transactions; do not put migration sessions behind transaction-mode pooling.

Validate large uploads, long-lived repository event streams, MCP paging and
cold-start latency through the deployed Vercel proxy. Gateway limits are not
removed by separating servers; a local test is not a cloud load measurement.
Preserve the public domain when reconnecting a custom domain or rolling back.

References: [Render Blueprint specification](https://render.com/docs/blueprint-spec),
[Render health checks](https://render.com/docs/health-checks),
[Vercel external rewrites](https://vercel.com/docs/routing/rewrites).

## Before provisioning

Create a local virtual environment, install `deploy/requirements-preflight.txt`,
and set `CXT_PREFLIGHT_PYTHON` to that environment's Python executable.
Run `scripts/deploy-preflight.sh full` from the repository root. It checks the
Render contract, frontend routes, Go tests, real PostgreSQL migrations, both
Docker images, route isolation, independent API shutdown, and database failure
and recovery. The [sync validation plan and results](SYNC_VALIDATION.md) include
the separate large-document, concurrent-read and PostgreSQL CLI checks.
It creates only disposable local containers; it never pushes an image or creates
cloud resources. CI also exercises the two real Docker images.

With planned deployment values exported, run `scripts/deploy-preflight.sh config`.
It checks the two service origins, public origin and matching Firebase project
without printing credentials. Service URLs assigned during provisioning can be
validated again then. The legacy GCP checks are explicitly named
`cloudrun-accounts` and `cloudrun-ready`; they do not gate Render preparation.
