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
route. No CORS or cross-site cookie configuration is needed: browsers use the
public Vercel origin throughout login and consent.

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
   `starter` web services and a PostgreSQL 16 `0.1c-256mb` instance in Singapore.
   Review current plans before provisioning; these are starting sizes, not a
   production load guarantee. Use the same region for services and database.
2. Enter `CXT_FIREBASE_PROJECT` and `CXT_PUBLIC_URL` on the API service. The latter
   is the HTTPS **Vercel frontend/custom domain**, without a path, not a Render
   service origin. MCP references these values from API; both must stay equal.
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
