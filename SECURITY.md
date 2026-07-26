# Security policy

CXTHub stores coding-agent conversations and may process sensitive repository
context. Treat suspected vulnerabilities as confidential.

## Reporting a vulnerability

Do not open a public issue. Use GitHub private vulnerability reporting:

`Security` → `Advisories` → `Report a vulnerability`

Include the affected version or commit, impact, reproduction steps, and a
minimal proof of concept. Remove real credentials, private source code,
personal data, and agent transcripts.

The maintainers will acknowledge reports on a best-effort basis, investigate
them, and coordinate disclosure after a fix is available. This alpha project
has no paid bug bounty and makes no response-time guarantee.

## Supported versions

Security fixes target the latest release and `main`.

| Version | Supported |
|---|---|
| Latest release | Yes |
| `main` | Development support |
| Older releases | No |

## Operator responsibility

The repository includes a filesystem store and development authentication mode
for trusted local use. Never expose `CXT_AUTH=dev` to an untrusted network.

Internet-facing operators are responsible for TLS, production authentication,
database access, backups, secret management, network policy, rate limits,
monitoring, and timely upgrades.

The `.cxtsecrets` scrubber and pattern filters are defense in depth, not a
guarantee that a session contains no secrets. Revoke any credential that may
have entered a session or Git history.

Published fixes follow the immutable release process in
[docs/RELEASING.md](docs/RELEASING.md).
