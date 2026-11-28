# Deploying CXTHub

The production topology is **Vercel frontend + Render API (`cxtd`) + Render
MCP (`cxt-mcp`) + shared PostgreSQL**. Start with [Render deployment](RENDER.md)
and the repository-root `render.yaml`. These templates do not create resources
until an operator imports/applies them.

The Terraform configuration below is retained for existing Cloud Run API
installations. It requires `mcp_origin` for a separately deployed MCP service;
`cxtd` no longer hosts MCP. New installations should use the Render guide.

## Runtime secrets

Create these Google Secret Manager secrets before applying Terraform:

| Default secret ID | Runtime environment variable | Value |
|---|---|---|
| `cxt-postgres-dsn` | `CXT_POSTGRES_DSN` | PostgreSQL connection string |

The [collaboration transaction contract](../docs/COLLABORATION_TRANSACTIONS.md)
defines atomic writes, retry behavior and failover limits.

Every externally bound `cxtd` process requires PostgreSQL. Cloud Run also sets
`CXT_REQUIRE_POSTGRES=1` as an explicit deployment assertion. Production
therefore refuses to start if the PostgreSQL build tag, DSN, live database
connection, or migration directory is missing; it never falls back to the
container filesystem.
All PostgreSQL API/MCP/worker startup is now schema-verification-only. Run the
separate operator image with explicit `--apply` before starting a compatible
release; do not give its migration credential to runtime services. Follow
[database migration ownership](../docs/DATABASE_MIGRATIONS.md) before rollout.
It also sets `CXT_PUBLIC_URL=https://<domain>`, which is the OAuth issuer and
protected-resource origin for the remote read-only MCP connector.

Secret versions are intentionally managed outside Terraform so their plaintext
does not enter Terraform state. The Terraform stack looks up only secret
metadata, grants the Cloud Run service account access, and injects `latest`.
Use `postgres_secret_id` to override the default ID.

## Remote MCP routing

Codex app and Claude app connect to `https://<domain>/mcp`. Vercel keeps that
public same-origin URL and proxies these paths to the independent MCP service:

- `/mcp`
- `/oauth/*`
- `/.well-known/*`
- `/api/v1/oauth/requests/*` (before the broad API rule)

Other `/api/*` requests go to the API service.

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

Run the read-only readiness check before applying:

```bash
scripts/deploy-preflight.sh cloudrun-ready
```

It fails closed when the PostgreSQL secret is absent or has no enabled version.

## GitHub App connections

The normal account/organization installation flow is documented in
[GitHub connections](../docs/GITHUB_CONNECTIONS.md). It uses a separate App
webhook at `/api/v1/github/webhook` and installation-scoped credentials. Enable
Terraform's optional `github_app` input with existing Secret Manager IDs after
registering the App. Firebase GitHub sign-in is configured separately.

## Storage metering and plan provisioning

Migration 0044 starts namespace-scoped storage accounting. The meter counts
compressed retained CAS bytes once per namespace, plus serialized settings and
encrypted secrets. It excludes derived indexes, SQL/TOAST overhead, and
bandwidth; it is not total database disk usage. Archiving a session does not
remove the retained bytes. No billing process deletes sessions.

Existing and new namespaces start in **metering** state with no activated price
or quota. **Current operation is free:** keep this default and do not provision
production commercial entitlements. Accounting does not create an invoice and
free-period usage must not be billed later. Free/Team/Enterprise are draft plan
names; prices and allowances will follow customer usage research. The earlier
storage-price proposals are preserved only in [Pricing status](../docs/PRICING.md).

The inactive entitlement foundation below is available for development tests
and a future, separately approved paid rollout. Its Enterprise candidate includes
50 GiB and excess metering. Repository count is unrestricted; request and payload
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
  -apply -expect 0 -operation provision-<stable-id> -actor '<operator identity>' \
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


### Background workers

`cxtd` runs its background workers alongside HTTP: the read of the Git hosting
provider, GitHub App work where an App is configured, document finalization,
notifications, invitation email and maintenance. The Cloud Run configuration
keeps at least one instance and sets `cpu_idle = false` so jobs continue
without incoming requests. This uses **instance-based billing**:
CPU and memory are billed for the instance lifetime, including idle time.
See [Cloud Run billing settings](https://docs.cloud.google.com/run/docs/configuring/billing-settings).
A request-only or scale-to-zero deployment needs a separately scheduled worker;
otherwise persisted jobs remain safe but cannot make progress while suspended.

Migration `0045_notification_outbox.sql` adds the notification outbox. Deploy
the backend, web and CLI update together: secrets writes require the revision
read before editing (`expected_revision`, HTTP 428 if missing). Existing encrypted
data stays readable. Notification delivery status is in repository Settings;
maintainers and owners can retry an undelivered event with the current webhook.
External delivery is at least once; receivers can deduplicate using
`X-CXTHub-Event-ID`. A secrets update and a member joining send a
notification; a branch moving does not. No cloud resources are created by the
local test commands.


### Remote Git worker

`cxtd` reads what the Git hosting provider holds for every repository that has
an origin: the default branch, branch heads, the commits that lead to them and
pull requests with their commit lists. Sessions open to other readers from
this read, never from what a CLI says. One pass makes at most 40 provider
requests, so a first read of a large repository takes several passes, and
provider rate limits can delay completion.

With a GitHub App configured, a repository is read only with the token of the
installation it is bound to; a repository that no installation was told to
read is not read, and `cxt setup`, `cxt doctor` and the Sessions tab say where
a maintainer connects it. A server without a GitHub App reads public GitHub
repositories without a credential, at GitHub's anonymous limit. No operator
token stands in for an installation. Server workers do not read a developer's
`gh` keychain. HTTP redirects are not followed. Only GitHub.com origins are
read; another host is an explicit validation failure.

The cache this worker fills holds commit numbers, parents, subjects, author
data, branch heads and pull-request metadata. It holds no source file
contents. Its database storage is separate from retained conversation objects
and should be included in database capacity monitoring.

### Optional Resend invitation email

Local servers may read a backend `.env`; Cloud Run uses Secret Manager instead.
Create a Resend key and verify the sender domain, then store the key as an enabled
version of an existing secret outside Terraform. Set `resend_secret_id` to that
secret's ID, and optionally `resend_from` for another verified domain. Terraform
only looks up metadata, grants the runtime access and injects `RESEND_API_KEY`;
the key never enters tfstate or frontend builds. The default empty secret ID
preserves inbox/link invitations without email. No paid resource or email is
created merely by adding the configuration to this repository.

All replicas must use the same settings and Resend account. `CXT_WEB_URL` points
to the public frontend domain; invitation URLs never use the private API host.
The existing warm instance/CPU setting also keeps the email outbox worker alive
without an incoming request. Restart/redeploy after key or sender changes.
