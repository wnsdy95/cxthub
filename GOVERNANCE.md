# Governance

CXTHub uses a maintainer-led governance model during its alpha stage.

## Roles

### Contributors

Contributors report issues, participate in discussions, submit focused pull
requests, review changes, and improve documentation or tests.

### Maintainers

Maintainers triage issues, review and merge changes, protect compatibility and
data integrity, manage security reports, and prepare releases. Maintainer
access is granted by the project owner after sustained, trustworthy
contributions and may be removed for inactivity, security risk, or conduct
violations.

### Project owner

The project owner and lead maintainer is [@wnsdy95](https://github.com/wnsdy95).
The owner appoints maintainers, resolves decisions that lack consensus,
administers repository rules, and has final release authority.

## Decision making

Routine decisions happen in issues and pull requests. Maintainers seek
technical consensus for changes involving:

- snapshot identity and graph semantics;
- wire formats, persistence, and migrations;
- synchronization and provider compatibility;
- security, privacy, authentication, and authorization;
- governance and release policy.

When consensus cannot be reached, the project owner decides and records the
rationale in the relevant public issue or pull request unless a confidential
security process prevents immediate disclosure.

Backward compatibility, recoverability, and user-data integrity take priority
over implementation convenience. Roadmap statements are intentions, not
guarantees.

## Repository administration

The protected-branch rules apply to maintainers and contributors. The project
owner has an explicit ruleset bypass for emergency security work, release
recovery, and authorized history maintenance. It is not the normal contribution
path and should retain a public audit trail whenever confidentiality permits.

The branching and review policy is documented in
[docs/BRANCHING.md](docs/BRANCHING.md).

## Releases

Maintainers publish signed, immutable version tags after required checks pass.
The project owner is the final release approver. Security releases may use a
private fix branch and coordinated disclosure.

Open-source releases and hosted-service deployments are independent. See
[docs/RELEASING.md](docs/RELEASING.md).

## Conduct and disputes

All project spaces follow [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md). Conduct
reports are handled privately. Technical disagreements should focus on
evidence, user impact, and project invariants.

## Forks

The Apache License 2.0 permits forks. Forks must follow
[TRADEMARKS.md](TRADEMARKS.md) and must not imply that they are official
CXTHub services or releases.
