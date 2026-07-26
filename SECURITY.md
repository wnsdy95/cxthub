# Security policy

cxthub stores coding-agent conversations and may process sensitive repository
context. Please treat security reports as confidential.

## Reporting a vulnerability

Do not open a public issue for a suspected vulnerability. Use GitHub's private
vulnerability reporting form:

`Security` → `Advisories` → `Report a vulnerability`

Include the affected version or commit, impact, reproduction steps, and a
minimal proof of concept. Remove real credentials, customer data, and private
agent transcripts from the report. If private reporting is unavailable, wait
for the repository owner to enable it rather than publishing exploit details.

We will acknowledge reports on a best-effort basis, investigate them, and
coordinate disclosure after a fix is available. This alpha project currently
has no paid bug bounty and makes no response-time guarantee.

## Supported versions

Security fixes target the latest release and `main`. Older binaries and
self-hosted deployments must upgrade to receive fixes.

| Version | Supported |
|---|---|
| Latest release | Yes |
| `main` | Development support |
| Older releases | No |

## Deployment responsibility

The repository includes a development authentication mode. Never expose
`CXT_AUTH=dev` to an untrusted network. Production operators are responsible
for TLS, authentication configuration, database access, backups, secret
management, rate limits, and timely upgrades. See
[`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md).

The `.cxtsecrets` scrubber is defense in depth, not a guarantee that an agent
session contains no secrets. Revoke any credential that may have entered a
session or Git history.
