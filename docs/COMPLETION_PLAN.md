# Reliability and Enterprise completion

Baseline: `0f9ffcf`, 2026-09-28. Tracking: #144 and #288.
Cloud provisioning/deployment and paid billing activation are excluded. The
existing repository, organization and Enterprise authority boundaries remain.

## Delivery order and acceptance

1. **Retained capture recovery.** Inspect attempts across worktrees without
   changing them. Distinguish exact replay, independently completed publication
   at the same code position, and an unprovable historical gap. Persist recovery
   decisions separately with immutable evidence and concurrency checks. Never
   substitute a live transcript or a newer worktree position for an old capture.
   Validate the three retained development cases through supported commands.
2. **Synchronization cost.** Measure selection, transfer, validation and indexing
   independently. Separate foreground prerequisites from retained historical
   backfill without losing either. Reuse verified chunks/index components where
   their identity proves equivalence; preserve whole-document identity checks.
   Persist deferred work, bound retries and demonstrate progress under capture,
   cancellation, restart and concurrent publication.
3. **PostgreSQL development cutover.** Back up and freeze current FS writers at
   cutover, import a fresh verified copy into an empty local database, compare
   identities/authority/history/memory and rehearse restore. Run independent API
   and MCP against that database. Preserve the original FS backup and rollback
   route; no external resources or production transfer.
4. **Progress and large graphs.** Expose transfer/verification/publication phases,
   actionable recovery and retained gaps consistently. Render only the visible
   graph rows while retaining the full server-provided graph evidence, lane
   coordinates, sticky labels, selection and keyboard access. Verify scrolling,
   folding, live updates and long labels with browser regressions.
5. **Enterprise identity and security.** Add interoperable SAML/OIDC connection
   and verified-domain identity binding, SCIM with source-aware deprovisioning,
   enforced MFA/IP/session policies and last-owner recovery. Add auditable
   retention/deletion/restore controls and security audit delivery. Configuration
   alone must never claim enforcement. Existing users, manual grants and unrelated
   organizations must survive identity changes. Local protocol/IdP fixtures are
   required; actual customer/provider credentials remain a separate verification.

Each logical change gets focused failure-path tests, affected full checks, a
signed PR and green CI. Record measured limitations rather than treating local
tests as cloud certification. Preserve user data and existing Git/provider hooks.

## Design review constraints

- Capture recovery from the current transcript is rejected: it falsely assigns
  later conversations to an earlier commit. Immutable evidence plus distinct gap
  and replacement states is the selected approach. Repeated warnings alone do
  not provide an operator recovery workflow.
- Incremental work must not weaken canonical hashes or classify retained history
  as disposable. A queue acknowledgement must imply durable retriable work.
- Graph virtualization is a presentation optimization. Business inclusion and
  publication facts stay in the server query contract.
- Enterprise membership is not private repository access. Identity federation
  cannot merge accounts merely because emails match, or silently remove manual
  grants. A tenant must not lock out its last recoverable owner.
- Retention defaults preserve data. Policy controls and deletion mechanisms are
  tested with synthetic data; enabling deletion for live data requires a concrete
  policy selected by the administrator. Billing counters never authorize deletion.

## Status

- Capture attempt investigation: two failed attempts have a later complete pass
  and publication at the same Git/worktree/branch position; one has no successful
  capture result. The original journals remain unchanged during investigation.
- Enterprise IdP: use local protocol fixtures unless a customer IdP is supplied.
- Capture recovery CLI and failure-path tests implemented. Two retained failures
  were resolved through verified replacement evidence; all three original attempt
  files remained byte-for-byte unchanged. One unprovable gap remains unresolved
  pending an explicit operator acknowledgment. CLI suite/vet/race passed; real
  Git-hook sync E2E and PR checks are in progress.
- All other phases pending implementation and validation; this document is a plan,
  not a completion claim.
