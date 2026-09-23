# Collaboration completion plan

This is the remaining scope after #257, #259 and #261, tracked by #144.
Unchecked work is not complete. Local validation is not cloud validation.

## Invariants

- Cloud PostgreSQL owns shared state. CLI files are recoverable working replicas.
- Effective authority is checked in the same transaction as each write. An
  organization owner inherits repository owner access; Enterprise administration
  alone never grants access to organization content.
- Revocation and writes serialize. Already committed writes remain historical
  facts; revoked credentials cannot authorize subsequent writes.
- Context objects, natural parents, PR completion evidence and memory provenance
  remain intact. Failed delivery is resumable, never silently discarded.
- Free metering remains enabled. Pricing and paid billing remain deferred.

## Delivery and acceptance

- [x] Enumerate mutation paths and expand real PostgreSQL authorization races,
  including membership, archive and operation-policy changes. Denied writes must
  leave objects, pointers, revisions and notification jobs unchanged.
- [x] Scope foreground branch replay to the current Git ref and make document/chunk
  pull resumable. Full explicit synchronization still covers retained history. Preserve durable retries, integrity and final publication order.
- [x] Team name/description editing through application, both stores, API and UI.
- [x] Organization default repository access composed with direct/team access,
  owner inheritance and removal; authorization/listing must agree.
- [x] Organization/Enterprise invites: intended recipient, verified acceptance,
  expiry, cancellation, resend, idempotency and audit. No implicit membership.
- [x] Repository transfer and namespace renaming: authority on both sides,
  stable IDs, old URL resolution, collision protection and retained audit.
- [x] Remove obsolete organization owner break-glass UI without expanding
  Enterprise content access.
- [x] Authorized MCP applications: inventory, revocation and audit.
- [x] Organization audit: ordinary repository/asset mutations, cursor/export
  and correlation identifiers without secret content.
- [x] Reproducible multi-process PostgreSQL load, tenant isolation, cancellation,
  restart, backup/restore and migration checks; record results separately.
- [ ] Run deployment verification against an explicitly supplied staging project
  and API. Public webhook/OAuth verification requires that environment.

SSO/SCIM, internal visibility/nested teams and retention/deletion are separate
product contracts in #144, not implied by removing the defects above.

## Review decisions

Keep the application transaction as the shared authorization boundary. Repeating
only HTTP checks leaves body-read races open; adding a global process mutex
would not coordinate cloud replicas. Database identity and repository locks must
use one documented order, with no external network I/O while held.

Do not increase the Git hook timeout to hide sync cost. Capture/publication
dependencies take priority over retained historical replication. A deadline must
preserve recoverable work and must not mark partial delivery as complete.

## Delivery evidence

See COLLABORATION_TRANSACTIONS.md for the exact authorization matrix, separate
server-process workload and restore procedure. Invitation delivery is inbox/link
only, per the selected product scope. No email provider or staging project/API
has been supplied. Public deployment verification remains open; no cloud resource
has been created and no instance limit has been raised.

Broader query paging/virtualization, archive-wide initial sync cost and capture
recovery tooling remain separately tracked under #144. They are not solved by
this collaboration administration change.
