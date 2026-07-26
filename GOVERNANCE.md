# Governance

cxthub uses a maintainer-led governance model during its alpha stage.

## Roles

- **Contributors** propose issues, documentation, code, tests, and reviews.
- **Maintainers** triage issues, review changes, manage releases, handle
  security reports, and protect project invariants.

Current maintainers are the GitHub users with repository maintain permission.
Maintainer access is granted based on sustained, trustworthy contributions and
may be removed for inactivity, security risk, or conduct violations.

## Decisions

Routine changes are decided through pull-request review. Maintainers seek
technical consensus for protocol, persistence, compatibility, security, and
governance changes. When consensus cannot be reached, the repository owner
makes the final decision and records the rationale in the issue or pull
request.

Backward compatibility, user data integrity, and safe migration take priority
over implementation convenience. Published roadmap items are intentions, not
guarantees.

## Releases

Maintainers create signed or otherwise verifiable version tags after required
CI passes. Security releases may use a private fix branch and coordinated
disclosure. The hosted cxthub service may deploy more frequently than public
CLI releases, while protocol compatibility remains documented.

## Forks

Apache License 2.0 permits forks. Forks must follow `TRADEMARKS.md` and must not
claim to be an official cxthub service or release.
