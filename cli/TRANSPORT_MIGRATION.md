# Moving a CLI transport without changing repository identity

When the server has restored the **same existing repository rows**, authenticate
to the destination and explicitly bind its API endpoint:

```sh
cxt login --server https://api.cxthub.io
cxt remote set-endpoint origin https://api.cxthub.io/api/v1 --migrate-pending
cxt remote -v
```

Keep the original identity, for example `http://localhost:5173/acme/project`. Do not remove/re-add it with
the public URL: its normalized URL hash is the immutable RepoID used by existing
documents, snapshots and queued captures. No local objects are rewritten and no
server repository is created by `set-endpoint`.

## Verification and publication

- Endpoints must use HTTPS, or HTTP with literal loopback/localhost, and exactly
  `/api/v1` (one trailing slash is accepted). Userinfo, queries, fragments,
  escaped paths and invalid ports are rejected. Redirects are not followed.
- Migration requires a destination-host token saved by `cxt login --server`.
  It ignores `CXT_TOKEN` for this explicit operation and verifies `/me`.
- The destination must resolve the **old URL**, then return an existing
  `/repos/<old-repo-id>` row with that exact URL, RepoID and the same nonempty
  server `RepositoryID`. A missing row, alias to a new identity, or mismatch
  fails without changing config. First migration pins the server RepositoryID
  from these two authorized reads; old configs did not persist it. Subsequent
  migrations must retain the pinned RepositoryID. This trusts the explicitly
  selected authenticated server, not proof from the unavailable old server.
- The endpoint binding is stored separately in shared `.cxt/config`, keyed by
  immutable URL. Named aliases for that URL share the binding. Removing a remote
  does not erase the binding or implicitly undo migration.
- Server checks occur before taking the existing shared capture/config lock.
  Exact-byte/version CAS rejects concurrent config changes, including changes
  from linked worktrees. Retry the command to obtain fresh verification after a
  conflict. Unknown configuration fields and unrelated preferences are retained.
- An active `capture.identity` root preference requires fresh root-publication
  capability confirmation on the destination before the endpoint and preference
  are updated together. Failure preserves the old preference and endpoint.

## Consumers and credentials

Normal and named sync, connection resolution, remote-add of an existing bound
identity, login/logout, setup, capture capability checks and server repair use
the shared endpoint resolver. Passive resolution does not access the network or
write configuration. Backend request endpoints and credentials are selected as
one pair; endpoint-scoped caches do not become receipts for another destination.
Personal-settings caches are endpoint-bound too.

For an ordinary remote, existing URL-derived routing and origin `CXT_TOKEN`
behavior remain. After a cross-host migration, that environment token is still
scoped to the original URL's API address: use the saved destination login, not an
old-host token. Neither migration records nor errors contain credentials. Normal
remote-add same-host URL validation is unchanged; this is not a general cross-host
alias exception.

`repair --from-server` retains the journal's immutable RepoID, resolves the saved
transport and re-verifies its exact binding before fetching. A malformed config
now requires restoring the config first: repair cannot safely guess whether a
lost or invalid transport binding should be ignored. Missing config can still be
repaired with an explicit original repository URL at its original endpoint.

## Durable capture and merge deliveries

`--migrate-pending` explicitly authorizes historical deliveries for the observed
prior transport to use the verified destination. The same CAS stores a bounded
receipt containing the exact old identity, RepoID, prior endpoint and new endpoint
with its pinned server RepositoryID. Without this flag, changing the endpoint
does not authorize historical work. Raw endpoint config edits do not create
receipts. Existing intent IDs, evidence, capture proofs and snapshots are never
rewritten. Completion still requires the normal publication acknowledgement.

Capture and deferred merge retries share one passive receipt resolver. A receipt
applies only to its exact source fingerprint and exact currently bound destination
for that repository. The prepared worker re-verifies the server binding, selects
destination-only credentials and freezes the route for the attempt. An unmatched
or changed route retains queued work without consuming transport-mismatch retry
budgets. Server identity mismatches also preserve the budget. An explicit receipt
gives captures a separate eight-attempt budget for that exact transition, so jobs
exhausted while the source was frozen can resume. Original retry counters remain
intact; re-running the migration command does not reset the transition budget.
Server/network failures otherwise retain normal retry behavior. Merge delivery
retains its existing retry/backoff policy.

Receipts are one-hop, not transitive: a later endpoint cannot piggyback on a prior
receipt. Include the flag during the transition; repeating it at the same endpoint
only reuses an existing matching receipt and cannot reconstruct an omitted source.
API-only environment jobs with no immutable remote identity cannot be migrated by
this command. At most 64 receipts and per-job transition budgets are retained;
exceeding this fails closed.

New capture-push intents bind the immutable URL/RepoID plus the verified endpoint
and server RepositoryID. Both retries compare this binding; the prepared client
freezes its endpoint, credentials and repository for the bounded append retry.
If the endpoint changes during the attempt, append fails closed. Deferred merge
deliveries use the same destination binding and explicit receipt policy.
Ordinary non-migrated intent/hash formats remain compatible.

Stop concurrent sync/capture workers when planning an operational cutover and
review retained work explicitly. Migrating the server data, installing the CLI
and deploying services are separate operations, not effects of this command.

## Device login across API and frontend hosts

Device pairing accepts the server's optional `verification_uri_complete`, such
as `https://cxthub.io/login/device?code=...`. It must contain only the emitted
pairing code at `/login/device`, without userinfo or fragments, over HTTPS (or
loopback HTTP only when the selected API is also loopback). Older servers fall
back to the selected API host. Polling, token verification and credential storage
always stay on the explicitly selected API endpoint, never the browser host.

## Validation

Hermetic `TestEndpoint*` tests cover the localhost-to-HTTPS move, exact repository
checks, destination credentials, root preference preservation, passive resolution,
CAS races, linked-worktree capture gates, login redirects, repair routing,
endpoint-scoped caches, and frozen capture/merge retry bindings. Run from `cli/`:

```sh
go test ./cmd/cxt ./internal/adapters/remotecfg ./internal/adapters/delivery/cli -run TestEndpoint -count=1
go test ./... -timeout=20m
go vet ./...
```
