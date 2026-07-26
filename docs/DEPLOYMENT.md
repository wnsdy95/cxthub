# cxthub Deployment Design (Vercel + Cloud Run + Neon)

**Status: Platform constraints validated; not deployed.** This document and `deploy/terraform/` define the intended deployment structure.
Do not run Terraform apply until the accounts, database, credentials, and remote state described in §4 are prepared.

---

## 1. Architecture

```
                        ┌─ User Browser ────────── https://<domain>
                        │      │
                        │      ▼
   Team Member CLI (cxt)       │  [Vercel]  frontend/web (SPA, vite build)
        │               │      │
        │ https://<domain>/api/v1/…        rewrite /api/* ─┐  (vercel.mjs — same-origin proxy)
        └───────────────┴──────────────────────────────────┤
                                                           ▼
                                              [Cloud Run]  cxtd (single instance)
                                                *.run.app · CXT_AUTH=firebase
                                                auto migration on startup
                                                           │ Secret Manager → CXT_POSTGRES_DSN
                                                           ▼
                                                 [Neon]  Postgres (cxthub DB)
```

| Component | Owner | Terraform Resources |
|---|---|---|
| Web (SPA) | Vercel | `vercel_project`, `vercel_project_domain` |
| API(cxtd) | Cloud Run (Seoul) | `google_cloud_run_v2_service`, Artifact Registry |
| DB | Neon (Singapore — Seoul not supported, nearest) | manual bootstrap (PITR/role included) |
| DB Secret | GCP Secret Manager | secret metadata lookup + runtime SA IAM managed by TF |
| DNS | Domain Registration Authority (manual apex A record) | Vercel domain inspect + output `dns_records` for guidance |

## 2. Core Design Decisions and Justifications

**① cxtd cannot be deployed on Vercel.** cxtd is a long-running daemon — in-memory state (device flow pairing, rate limit window), background goroutines (webhook invocation, GitHub lazy synchronization). It does not coexist with serverless functions (request-based start/stop). Therefore, the web runs on Vercel, while the API requires a persistent runtime.

**② Same-origin proxy (the axis of this design).** If the web and API have different origins, HttpOnly cookies (SameSite=Lax) will not be sent across cross-site XHR requests, causing login to fail. By using `vercel.mjs` rewrite to proxy `/api/*` to the Cloud Run default `run.app` URI, the browser perceives everything as `https://<domain>` same-origin:
- Cookies work as expected (SameSite=None fallback unnecessary)
- CORS requests do not occur. However, the upstream Host could be `run.app`, so for server-side CSRF Origin validation, inject `CXT_CORS_ORIGINS=https://<domain>` exactly once.
- CLI remote URL also unifies to a single domain: `https://<domain>/<user>/<ws>`
  (remotecfg derives `/api/v1` from the host → Vercel proxy → Cloud Run)
- Device flow approval URL (`https://<domain>/login/device?...`) also naturally becomes the same domain.

**③ Fixed single instance for Cloud Run (min=max=1).** Since device flow pairing and rate limiting are in-memory, multiple instances would lead to polling/counting discrepancies. With the current scale (small teams), one instance is sufficient and eliminates cold starts. Scaling out preparation is in §9.

**④ DB is Neon, credentials are managed outside Terraform.** cxtd requires only `-tags postgres` and a DSN; the schema migrates automatically at startup. Create the Neon project and minimal privilege role during the initial bootstrap, and inject the DSN directly from Secret Manager version. Avoid using Neon provider's computed password or Terraform's `secret_data` as they store secrets in tfstate, which is not intentional.

**⑤ Large sessions use bounded chunk transport.** Cloud Run HTTP/1 request/response limits are 32 MiB, and Vercel external rewrite has a 120-second limit per request. Store the doc in chunks in the repository and recombine into a single JSON over HTTP. The first push/pull exceeds this boundary, so the chunk body is sent in chunks of up to 32, uncompressed 2 MiB (`/push/chunks`, `/pull/chunks`) per request. Staging is content-addressed and idempotent, excluding already negotiated chunks after a break, so it does not resend from the beginning. The integrity DocHash and snapshot/ref publication order remain unchanged. If a single event exceeds 2 MiB, it automatically falls back to the existing monolithic doc path in the v1 manifest event boundary to maintain backward compatibility. Platform limits:
[Cloud Run quotas](https://cloud.google.com/run/quotas),
[Vercel limits](https://vercel.com/docs/limits).

Cloud Run URI is directly inserted into Vercel's `CXT_API_ORIGIN` build environment variable by Terraform. There are no static placeholders or separate API DNS, and `vercel.mjs` only allows HTTPS `*.run.app` origins, rejecting incorrect origin settings at the build step. Cloud Run custom-domain mapping is not supported in the Seoul region and is discouraged for Preview and production, so it is not used.

**⑥ Rejected Alternatives.**
- *Entirely on Vercel*: Not possible as per requirement ①.
- *Single VM (cxtd serving web as well)*: Minimal configuration but completely eliminates TLS/patch/backup operational burden. Valid as a self-hosting option, so recorded as Alternative B.
- *Browser directly calling run.app*: Lowering cookies to SameSite=None and managing CORS whitelist required — security downgrade + increased configuration. Temporarily allowed in domain-less staging environments.

## 3. Environment Variable Manifest (Production Source of Truth)

| Variable | Production Value | Justification |
|---|---|---|
| `CXT_AUTH` | `firebase` (required) | Dev authentication guard: Denies startup if unset or non-loopback binding |
| `CXT_FIREBASE_PROJECT` | `<firebase-project-id>` (required) | ID token RS256 validation |
| `CXT_POSTGRES_DSN` | Refer to Secret Manager `cxt-postgres-dsn:latest` | Prohibits plain text env/tfstate, requires `sslmode=require` |
| `CXT_MIGRATIONS_DIR` | `/app/migrations` | Bundles SQL migrations in container, automatically applied at startup |
| `CXT_COOKIE_SECURE` | `1` | Secure cookies for HTTPS |
| `CXT_CORS_ORIGINS` | `https://<domain>` | Allows exactly one browser Origin in CSRF trusted list, transmitted by Vercel proxy |
| `CXT_ALLOW_PRIVATE_WEBHOOK` | Unset | Maintains SSRF guard activation |
| `CXT_GH_API_BASE` | Unset | Uses actual GitHub API |

Web (Vercel): `VITE_API_BASE` **Unset** — Same-origin relative path (`/api/v1`) used in production configuration.

| Vercel Build Variable | Value | Notes |
|---|---|---|
| `CXT_API_ORIGIN` | Cloud Run `uri` | Terraform calculated value, only `https://*.run.app` allowed |
| `VITE_FIREBASE_API_KEY` | Firebase Web App SDK config | Identifier exposed to browser. API/domain restrictions required |
| `VITE_FIREBASE_AUTH_DOMAIN` | `<firebase-project>.firebaseapp.com` | Scheme/path forbidden |
| `VITE_FIREBASE_PROJECT_ID` | Same as `CXT_FIREBASE_PROJECT` | Prohibits project mismatch between web and server |

These values are injected by Terraform into the production target. If any are missing or incorrectly formatted, `vercel.mjs` will fail the build. Until a separate staging DB/API is established, these values are not injected into the preview target, thus not opening the path for preview to send write requests to the production API.

## 4. Deployment Preparation and Unchanged Verification

This section performs a local build and resource **query only**. Terraform apply, image push, Vercel project creation, and DNS changes are not performed.

```bash
# Fast configuration check / Full local check (PG 16 temporary container and cxtd image are created locally only)
make deploy-check
make deploy-check-full

# Actual input files. terraform.tfvars is gitignored and only example is committed.
cp deploy/terraform/terraform.tfvars.example deploy/terraform/terraform.tfvars
# gcp_project, firebase_project, and firebase_web_api_key are filled with actual values.
# The Web API key placeholder is rejected by Terraform, Vercel, and ready checks;
# project placeholders fail the account-access checks.
# image_tag is not fixed in the file and is overwritten with the full SHA every deployment.
export TF_VAR_image_tag="$(git rev-parse HEAD)"
```

First, verify the following in the Firebase console.

1. Project `<firebase-project-id>` has a Web App; obtain apiKey, authDomain, and projectId from its SDK config.
2. Enable Email/Password and Google provider in Authentication.
3. Add the deployment's `<domain>` to Authentication → Authorized domains.
4. Restrict the Web API key in Google Cloud Credentials to the necessary APIs and the actual web referrer for Firebase Auth.

The Vercel Terraform provider uses a `VERCEL_API_TOKEN` instead of a CLI login
session. The token must be issued from the intended personal or team account
and allow GitHub App access to the public `wnsdy95/cxthub` repository.
When using a team, `vercel_team_id` is not the username/slug but the `team_…` ID.

In Neon, create a project/database in the Singapore region and assign a cxtd-specific role to own the database/schema so that migration DDL can be executed. Do not use the Neon management role as a runtime DSN. This configuration is for a single always-on instance, starting with a direct endpoint DSN (`sslmode=require`). A pooler switch is not needed until the number of connections increases.

Run a read-only account check with the following values. If any account name is different, stop immediately.

```bash
export CXT_EXPECTED_GCP_ACCOUNT='<gcp-email>'
# If using Terraform ADC as a separate subject (e.g., impersonated service account), specify it. If omitted, it should be the same as the above account.
# export CXT_EXPECTED_GCP_ADC_ACCOUNT='<adc-principal-email>'
export CXT_EXPECTED_VERCEL_ACCOUNT='<vercel-username>'
export VERCEL_API_TOKEN='<token>'                  # Don't leave in shell history, inject safely
export TF_VAR_gcp_project='<gcp-project-id>'
export TF_VAR_firebase_project='<firebase-project-id>'
# If team deployment, set actual team_… ID to verify token team access. Skip for individual accounts.
# export TF_VAR_vercel_team_id='team_...'
make deploy-check-accounts
```

`deploy-check-accounts` compares the GCP CLI account and Terraform's actual ADC subject, and checks the project/billing and Vercel token owner account (team access if applicable) without making any changes. The state bucket, DB secret, and image are checked in the `ready` inspection following the bootstrap.

## 5. Initial Provisioning Process (One-time)

Preparation: 1 domain · Payment-connected GCP project · Vercel/Neon/Firebase setup · Local installation of terraform ≥1.7 / gcloud / docker · `VERCEL_API_TOKEN` · `gcloud auth application-default login`. Do not provide Neon API key and DSN as Terraform variables.

```bash
# ① Prepare GCP API and remote tfstate bucket (Uniform access + versioning, allow only operator IAM)
gcloud services enable run.googleapis.com artifactregistry.googleapis.com secretmanager.googleapis.com iam.googleapis.com \
  --project="$TF_VAR_gcp_project"
gcloud storage buckets create gs://<TF_STATE_BUCKET> --project="$TF_VAR_gcp_project" \
  --location=asia-northeast3 --uniform-bucket-level-access
gcloud storage buckets update gs://<TF_STATE_BUCKET> --versioning

# ② Create project/database and cxtd-specific minimal permission role in Neon console, then inject DSN into Secret Manager
gcloud secrets create cxt-postgres-dsn --project="$TF_VAR_gcp_project" --replication-policy=automatic
read -rsp "Neon DSN: " POSTGRES_DSN; echo
printf %s "$POSTGRES_DSN" | gcloud secrets versions add cxt-postgres-dsn \
  --project="$TF_VAR_gcp_project" --data-file=-
unset POSTGRES_DSN

# ③ Initialize Terraform and create Artifact Registry first. Initialization fails if backend configuration is missing.
cd deploy/terraform
terraform init -backend-config="bucket=<TF_STATE_BUCKET>" -backend-config="prefix=cxthub/prod"
export TF_VAR_image_tag="$(git rev-parse HEAD)"
terraform apply -target=google_artifact_registry_repository.cxthub
cd ../..

# ④ Initial image build and push (from repo root, using full git SHA for immutable source)
gcloud auth configure-docker asia-northeast3-docker.pkg.dev
docker build -f deploy/Dockerfile -t asia-northeast3-docker.pkg.dev/<PROJ>/cxthub/cxtd:$(git rev-parse HEAD) .
docker push  asia-northeast3-docker.pkg.dev/<PROJ>/cxthub/cxtd:$(git rev-parse HEAD)

# ⑤ Pre-deployment read-only check. Verifies clean HEAD=origin=image tag and API/bucket/secret/image.
export CXT_TF_STATE_BUCKET='<TF_STATE_BUCKET>'
export TF_VAR_domain='<domain>'
export TF_VAR_gcp_project='<PROJ>'
export TF_VAR_firebase_web_api_key='<web-api-key>'
export TF_VAR_image_tag="$(git rev-parse HEAD)"
# Only set if past exposed PAT has been revoked directly in GitHub
export CXT_CONFIRMED_LEAKED_PAT_REVOKED=true
make deploy-check-ready

# ⑥ Full plan/apply — Cloud Run URI is injected into Vercel CXT_API_ORIGIN and a Git deployment is created.
cd deploy/terraform
terraform plan -out=cxthub.tfplan
terraform apply cxthub.tfplan

# ⑦ Verify apex A record for project in Vercel and replace existing A record of the registrar.
vercel domains inspect <domain>
# Default value is A @ 76.76.21.21, but use the result from the inspection as the source of truth. No separate API subdomain exists.

# ⑧ Validation: health → login → cxt remote/device flow → actual commit/push
curl -fsS https://<domain>/api/v1/health
# {"status":"ok"}
# Log in to <domain> → cxt remote add origin https://<domain>/<u>/<ws>
#         → cxt login(device flow) → git commit·push → verify snapshot on web
```

**Note:** The DB secret version must exist before the Cloud Run resource. The container includes all SQL (currently `0001`–`0035`) and applies them in order upon cxtd startup. The `run.app` URI is a public origin address and is not called directly by the browser; normal traffic always goes through Vercel's `/api/*` rewrite. The production Cloud Run service is protected by the provider's `deletion_protection`, and Artifact Registry and Vercel projects are protected by Terraform `prevent_destroy`. For intentional deletion, protection is changed and reviewed separately before performing the two-step process.

Past exposed PAT strings may remain in the local `.cxt/` and `cxt-data/` derived contexts. Both paths are excluded from Git ignore and Docker deny-all allowlist, so they do not enter the deployment input. However, this does not replace credential revocation. `deploy-check-ready` fails until the operator explicitly sets `CXT_CONFIRMED_LEAKED_PAT_REVOKED=true` in GitHub. Local derived data deletion and scrubbing are destructive operations that change the immutable context and are not automatically executed in this deployment process.

The bounded chunk capability is advertised by the server in a negotiate/pull manifest response. The initial public order is **server image deployment → smoke → new cxt deployment**. The old cxt continues to work with the existing inline path on the new server, but a storage with a single payload exceeding 32 MiB must be updated to the new cxt. The new cxt falls back to a whole/inline chunk on the old server without bounded capability, maintaining rollback compatibility.

## 6. Routine Deployment (Update) Process

```bash
# Server change deployment = apply with new image tag (Cloud Run performs an uninterrupted replacement)
docker build -f deploy/Dockerfile -t …/cxtd:<new git-sha> . && docker push …/cxtd:<new git-sha>
terraform apply -var image_tag=<new git-sha> …

# Web change deployment = git push only (Vercel automatically builds and deploys via GitHub integration)
# DB migration = none (new SQL files are bundled with the image and applied automatically upon startup with history recorded)
# Deployment confirmation (read-only) = scripts/deploy-smoke.sh <domain>
```

**Rollback**: The server runs `terraform apply -var image_tag=<previous sha>`. For the web, promote the previous deployment from the Vercel dashboard. For the DB migration, follow the rollback principle (no down scripts) — if there is an issue, add a new migration with a new number.

## 7. Maintenance Runner

| Frequency | Task |
|---|---|
| Per deployment | `make deploy-check-full` → image push → plan review/apply → `deploy-smoke.sh` |
| Weekly | Check for 5xx/`denied:`/`warning:` patterns in Cloud Run logs, and Neon dashboard storage trends |
| Monthly | Perform `pg_dump` offsite 1st part (Neon PITR backup), and recommend cleaning up unused CLI tokens/web sessions |
| Quarterly | Update dependencies (go/npm), automatically reflect Firebase key rollovers, and check for Terraform plan drift |

**Incident Response Key Points**
- *Login screen unavailable*: Check Cloud Run logs for `denied: dev auth` = env loss (CXT_AUTH) → re-run apply.
- *DB connection unavailable*: Verify Neon console status → add new DSN to Secret Manager version → new Cloud Run
  revision deployment. Do not directly insert DSN into Terraform plan/output/log.
- *Session/token leak suspicion*: DB stores only hashes, no original leak. Web sessions/CLI tokens are account
  settings for individual deletion, bulk deletion with `DELETE FROM sessions` followed by a power re-login notice.
- *Secret envelope leak*: Only the ciphertext, so no passphrase leak is safe. Suspect team passphrase
  replace → re-encrypt in web and store → team members run `cxt secrets pull --remember` again.

## 8. Known Constraints (Accepted Trade-offs)

- **Rate limiting IP identification**: `RemoteAddr` groups behind Vercel/Cloud Run proxy → effectively operates globally (not a security downgrade, just an impact on UX). To improve, add trusted parsing of `X-Forwarded-For` first hop (along with scaling work).
- **Single instance**: §2-③. Allow singletons (Cloud Run handles restart management).
- **Neon region**: Seoul not supported → Singapore (round-trip ~70ms). Queries are few, so the impact is minimal.
- **Terraform secret boundaries**: The state contains only secret IDs and IAM, not DSNs. GCS bucket maintains uniform access/versioning and does not grant IAM to Terraform operators.
- **Interrupted chunk staging**: Content-addressed chunks saved before ref/doc manifest are reused on retry. The preservation/GC of permanently interrupted staging chunks is a subsequent operational policy and does not appear in the public graph.

## 9. Extension path (if scale grows)

1. **Multiple instances**: device flow pairing to in-memory → store (single table), rate limiting
   X-Forwarded-For based previous → `max_instance_count` disabled.
2. **CI Deployment**: GitHub Actions pipeline for test→build→push→apply (manual approval gate,
   Workload Identity Federation usage — disallow long-term GCP keys).
3. **Observation**: Cloud Run alert policies (5xx ratio, instance restarts), self-monitoring via webhook.

## Alternative B — Self-hosted Single Server (Reference)

For running on a single VM instead of a managed stack: build cxtd with `-tags postgres` (or FS store).
Reverse proxy (Caddy — auto TLS) for `/:*→dist`, `/api/*→cxtd`, systemd unit to run always.
Same-origin principle (§2-②) applies here as well. This configuration is the cheapest if there is no external exposure by the team.
