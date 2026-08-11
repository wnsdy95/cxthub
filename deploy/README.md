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

Secret versions are intentionally managed outside Terraform so their plaintext
does not enter Terraform state. The Terraform stack looks up only secret
metadata, grants the Cloud Run service account access, and injects `latest`.
Use `postgres_secret_id` or `github_webhook_secret_id` to override the default
IDs.

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
