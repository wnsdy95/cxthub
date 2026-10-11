# Git head read receipts

`FinishGitHeadScan` creates a receipt only for a nonempty, uniformly qualified
reconciliation page. Its version identifies one accepted scan claim, not a Git
timestamp, join, current-head assertion, or complete provider snapshot. `More`
records pagination only. Empty and providerless pages produce no receipt and
must never prove absence. Repeated identical heads retain distinct occurrences.

Reads are scoped by exact repository and origin. `GetGitHeadReceipt` selects a
positive claim version. `ListGitHeadReceipts` returns ascending versions strictly
after its cursor, with a limit of 1–101. Gaps are normal: failed, empty, and legacy
pages need not have receipts. Observation IDs remain immutable content identities.

PostgreSQL publishes observations, endpoint jobs, receipt, and checkpoint in one
transaction/savepoint. Repository policy and the live claim fence are checked
before writes. Application-owned repository revisions remain in the outer
transaction. A completed, expired, or superseded claim cannot replay its finish.

Development FS uses a process lock and durable per-file writes, not a multi-file
transaction. Observations and checkpoint precede the receipt. A failure can leave
partial observations or an advanced checkpoint without a receipt; a later fresh
scan can create a new occurrence, but the old occurrence is not reconstructed.
FS cannot supply production atomic admission/application guarantees. Production
causal consumers must require repository transactions rather than treating the
presence of this read port as an ACID capability.
