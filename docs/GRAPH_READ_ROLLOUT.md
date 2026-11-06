# Qualified graph read rollout

The worker, qualification and read-routing gates are independent. Code being
deployed does not mean the new read path is selected. Do not enable all gates at
once or infer readiness from a successful legacy `/view` response.

## Release gates

1. Deploy the compatible API and MCP binaries before publishing larger rules-2
   bundles. This release does not change the format or weaken integrity checks;
   older binaries still reject bundles above their earlier output bounds.
2. Measure a full legacy request with the new request-scoped diagnostics. Record
   route, request ID, total time, stage times, SQL count/rows/time and cache results.
   SQL rows are command-tag rows, not database blocks or rows examined. Stage
   durations overlap; do not add nested timings together.
3. Enable `CXT_GRAPH_PROJECTION_REPOS` and `CXT_GRAPH_SHADOW_REPOS` for the same
   exact repository. Leave `CXT_GRAPH_READ_REPOS` empty. Preserve unrelated
   environment values. After the required redeploy, inspect `/graph-readiness`.
4. Require a complete qualified generation with the expected rules, matching
   source incarnation and no attention-required state. `current=false` denotes
   an explicitly lagging, complete generation, not missing graph facts. Prefer
   a current generation for the initial cutover. A failed gate is a blocker,
   not permission to edit source history, skip qualification or raise timeouts.
5. Enable `CXT_GRAPH_READ_REPOS` for the qualified repository and redeploy the API.
   Verify `selected=true`, a nonempty `read_generation`, `route=published` in
   diagnostics, and the original branch births/PR joins. Compare equivalent
   requests separately for initial/repeated reads, including authorization denial.
6. Check memory and concurrent user-read progress while workers run. Logical
   output bounds do not guarantee the 512 MiB process limit. Single-process local
   measurements are evidence, not production mixed-load certification.
   Full published-bundle reads acquire the shared bulk permit before loading
   parts, including default-view reads; lightweight readiness does not. If the
   permit is busy, the existing owned-read retry releases its snapshot and
   resources before queuing, then rechecks authorization. Borrowed transactions
   do not wait while retaining another owner's resources.

## Rollback

Remove only the affected repository from `CXT_GRAPH_READ_REPOS` and redeploy.
This explicitly restores the legacy read path without rewriting original
snapshots, conversations, memory, refs or PR records. A published-read failure
must never silently fall back to the legacy path inside a request.

Removing worker startup allowlists drains only that process; durable enablement
is shared. Do not claim workers are globally disabled unless their owners are
drained or their durable configuration has been explicitly disabled.

## Remaining boundary

Qualified serving currently reads the entire field-part bundle. It removes
repeated source/evidence projection from user requests, but is **not** a graph
range index. Frontend row virtualization is likewise not server pagination.
Genuine windows require generation-bound ordered rows and crossing-edge data,
bounded range/point reads, atomic publication and explicit generation-reset
handling. They must retain branch births, PR completion, selection targets and
edges whose endpoints are outside the window. Do not return a sliced GraphState
as a complete graph or mix generations across pages.

MCP context/memory queries and the independently projected On Hold list retain
their existing contracts. These graph flags do not activate their projections.
