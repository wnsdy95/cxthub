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

## Storage metering and plan provisioning

Migration 0044 starts namespace-scoped storage accounting. The meter counts
compressed retained CAS bytes once per namespace, plus serialized settings and
encrypted secrets. Retained uploads without a snapshot still count. It excludes
derived indexes, SQL/TOAST overhead, and bandwidth; it is not total database disk
usage. Deleting a branch or hiding a session does not remove the retained bytes.
No billing process deletes context.

Existing and new namespaces start in **metering** state with no activated price
or quota. **Current operation is free:** keep this default and do not provision
production commercial entitlements. Accounting does not create an invoice and
free-period usage must not be billed later. Free/Team/Enterprise are draft plan
names; prices and allowances will follow customer usage research. The earlier
storage-price proposals are preserved only in [Pricing status](../docs/PRICING.md).

The inactive entitlement foundation below is available for development tests
and a future, separately approved paid rollout. Its Enterprise candidate includes
50 GiB and excess metering. Workspace count is unrestricted; request and payload
limits still apply. Customer REST and MCP routes cannot assign or upgrade plans.

Operators use a separate tool with the deployment database credential. A policy
file has `plan`, `included_bytes`, `pay_as_you_go`, `max_bytes`, `grace_bytes`, and
`grace_until`. Null `max_bytes` permits uncapped PAYG; otherwise it is an absolute
stored-byte cap. Free cannot enable PAYG. `grace_until` and `grace_bytes` provide a
bounded exception to the cap. No prices are inferred from these entitlements.

```bash
cd backend
# CXT_POSTGRES_DSN must be supplied securely in the operator environment.
go run -tags postgres ./cmd/cxt-admin -namespace ns_<id> -policy /secure/policy.json
# Review the output, then use its policy revision. Reuse the same operation ID
# and payload when retrying after an uncertain outcome.
go run -tags postgres ./cmd/cxt-admin -namespace ns_<id> -policy /secure/policy.json \
  -apply -expect 0 -operation provision-<stable-id> -actor operator@example.test \
  -reason 'Approved subscription activation'
```

Policy changes are compare-and-swap guarded and append to an immutable audit
record. Lowering a quota preserves stored objects and may make the account
read-only. New writes are checked against final net growth at transaction commit,
so concurrent uploads and temporary manifest conversion cannot bypass a cap.
Idempotent uploads and all reads remain available. Reducing retained storage or
raising a cap automatically restores write capacity.

Usage changes and policy baselines form an append-only ledger. Monthly excess is
the integral of `max(bytes - included_bytes, 0)` over elapsed hours while PAYG is
active. Reports expose the decimal byte-hours, not a rounded invoice amount;
partial first months begin at the first recorded measurement. Invoice settlement
and payment-webhook processing are not enabled by this foundation.

A bounded worker reconciles the oldest accounts daily against retained payloads;
interruption is safe to retry. A claimed account keeps a five-minute retry delay
when its recount fails, allowing other accounts to proceed. Owners may also
request reconciliation, at most
twice per minute, from Storage usage. Enterprise Admins can inspect aggregate
usage without gaining private repository access; only Owners can request a
recount. Personal usage appears in account settings. Filesystem development
servers report usage as unavailable; authoritative billing requires PostgreSQL.
