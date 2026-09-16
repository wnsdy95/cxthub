# Deploying CXTHub

The production template runs the web application on Vercel and `cxtd` on
Cloud Run with PostgreSQL. It is an operator-run template, not an automatic
deployment performed by the release workflow.

## Runtime secrets

Create these Google Secret Manager secrets before applying Terraform:

| Default secret ID | Runtime environment variable | Value |
|---|---|---|
| `cxt-postgres-dsn` | `CXT_POSTGRES_DSN` | PostgreSQL connection string |
| `cxt-github-webhook-secret` | `CXT_GITHUB_WEBHOOK_SECRET` | Random GitHub webhook HMAC secret |

Every externally bound `cxtd` process requires PostgreSQL. Cloud Run also sets
`CXT_REQUIRE_POSTGRES=1` as an explicit deployment assertion. Production
therefore refuses to start if the PostgreSQL build tag, DSN, live database
connection, or migration directory is missing; it never falls back to the
container filesystem.
It also sets `CXT_PUBLIC_URL=https://<domain>`, which is the OAuth issuer and
protected-resource origin for the remote read-only MCP connector.

Secret versions are intentionally managed outside Terraform so their plaintext
does not enter Terraform state. The Terraform stack looks up only secret
metadata, grants the Cloud Run service account access, and injects `latest`.
Use `postgres_secret_id` or `github_webhook_secret_id` to override the default
IDs.

## Remote MCP routing

Codex app and Claude app connect to `https://<domain>/mcp`. Vercel keeps that
public same-origin URL and proxies these server-owned paths to Cloud Run:

- `/mcp`
- `/oauth/*`
- `/.well-known/*`
- `/api/*`

The web-owned `/connect/mcp` path is intentionally not proxied; it renders the
login and consent screen. OAuth client/request/code state and access/refresh
token hashes live in PostgreSQL. The remote server exposes no write tools and
MCP access tokens are rejected by the ordinary REST authorization boundary.
Device pairing and request allowances also live in PostgreSQL. Pairing consumption
and CLI token insertion commit atomically. Rate admission uses a shared GCRA
bucket (configured burst size and continuous refill); rejection does not extend
the bucket. Store failures return 503 instead of disabling the limit. Expired
rows are pruned in bounded batches. FS mode is for one development host only.

`max_instances` defaults to 1 until staging is measured. Before raising it, verify
the database connection budget across all replicas and enforce per-source limits
at the gateway. Application gates deliberately do not trust forwarded IP headers;
HTTP peer limits behind a proxy may group multiple users. MCP/OAuth additionally
have aggregate shared limits. These remain protection backstops, not tenant quotas.

Reproduce the two-instance PostgreSQL load probe on a **disposable test database**:

```bash
cd backend
CXT_LOAD_DSN='<test database DSN>' go test -tags postgres ./internal/adapters/delivery/http -run TestPostgresMultiInstanceLoad -count=1 -v -timeout=10m
```

The probe creates its own user/repository, a 100 MiB conversation and 1,000 small
snapshots, then uses 16 concurrent readers across independent server/pool pairs.
It records REST/MCP p50/p95/p99 and maximum response bytes, verifies authorization
and paging, and restarts one endpoint during login pairing. It is opt-in and
writes fixtures; never point it at production. Local results are not evidence of
Cloud Run latency. Repeat on staging with its real network and database tier,
then load the public gateway separately before increasing the replica cap.

Run the read-only readiness check before applying:

```bash
scripts/deploy-preflight.sh ready
```

It fails closed when either secret is absent or has no enabled version.

## GitHub pull-request webhook

Each GitHub repository whose context is hosted by this deployment needs one
repository webhook:

- Payload URL: `https://<domain>/api/v1/hooks/github`
- Content type: `application/json`
- Secret: the exact value stored in `cxt-github-webhook-secret`
- Events: select **Pull requests**
- Active: enabled

After the API deployment and same-origin `/api` rewrite are live, reconcile the
hook without exposing its secret on the command line:

```bash
scripts/github-webhook.sh apply
```

The command fails before changing GitHub when the public health endpoint or
Secret Manager version is unavailable. It then creates or reconciles exactly
one hook and requires a successful signed GitHub ping. Use `check` for a
read-only drift check. `TF_VAR_gcp_project` and `TF_VAR_domain` are required;
`CXT_GITHUB_REPOSITORY=owner/repo` overrides the current `gh` repository.

On a same-repository PR `closed` event with `merged: true`, `cxtd` appends the
head context branch tip to the base context branch. This preserves the source
branch and immutable natural snapshot parents; the append is represented by
the existing graft overlay and is idempotent.

Fork PR heads are ignored because a fork's Git branch cannot safely identify a
branch ref in the base CXTHub repository. If webhook delivery is unavailable,
the installed local `post-merge` hook performs a best-effort GitHub API
fallback after the merged base branch is pulled. Public repositories need no
GitHub token; private repositories may explicitly provide
`CXT_GITHUB_TOKEN`, `GH_TOKEN`, or `GITHUB_TOKEN` with pull-request read access.

The receiver returns `404` when `CXT_GITHUB_WEBHOOK_SECRET` is absent and
`401` when `X-Hub-Signature-256` does not match. Never configure a webhook
without the shared secret.
