# Fetch discovery worker liveness

This CLI-only follow-up to #494 narrows the phase 2 gaps in
`docs/AUTOMATIC_CONTEXT_LINKING.md`. It adds recovery while the existing customer
capture observer is running; it does not provide an always-on client service or
server-side reconciliation.

## Existing lifecycle and recovery

Provider lifecycle hooks already start `git-hook live-watch` for an exactly
registered local session. The observer checks pending fetch observations and
saved merge deliveries at startup and on its existing 10-second ticks, including unchanged transcripts
and failed capture/publication attempts. These checks only wake discovery; they
do not capture text, restart providers, or change Git/context selection.

A wake checks the current repository/destination binding and the durable
observation records without creating state in unconnected or empty-queue repos.
Drained, deleted, unsupported, corrupt, and no-longer-authorized observations do
not launch workers. Corrupt evidence remains on disk and is reported. A valid
pending observation writes the wake token before probing the OS worker lock, so
a busy worker can see a wake racing with its exit. An active worker suppresses
another process launch.

A durable 10-second startup lease coalesces launches before the child acquires
the worker lock. The child clears the lease when it takes ownership. A failed
start or death before ownership leaves durable intent available after lease
expiry; malformed or future-dated lease contents cannot postpone recovery
indefinitely. There is no additional ticker, foreground sleeper, or new customer
command. Each wake has a 10-second local deadline; network replay remains in the
existing bounded worker and retains its authority rechecks and durable cursors.

The worker receives progress directly from durable provider-page checkpoints
and fetch-observation transitions. Progressing passes retain a one-second pause;
only unchanged pending work backs off exponentially, up to one minute. Attempt
timestamps, failed provider reads, and failed handoffs are not progress. A later
successful checkpoint resets the pause, so long healthy PR pagination does not
consume the worker's 30-minute lifetime in accumulated backoff. Provider work
remains bounded per pass; this is not an unpaced retry loop.

## Saved merge deliveries

The same wake, startup lease, worker lock, and 30-minute worker also replay saved
`.cxt/merge-deliveries` records. A merge queue can wake the worker without any
fetch-observation directory or records, including repositories using `CXT_REMOTE`
instead of a configured context origin. No new observer or daemon is created.
The cheap wake checks the local Git branch, destination and Git-origin
fingerprints, and retained range ancestry. The worker additionally verifies the
repository and context-branch identity before preserving a briefing baseline or
claiming work. It reuses the existing merge lease and scope revalidation during
replay; it does not reconstruct a range from `ORIG_HEAD` or require new captures.

Automatic attempts retain the journal's `LastAttempt` and `Attempts` with
exponential pauses of 1, 2, 4, 8, 16, 32, then 60 seconds. Observer wakes cannot
reset this durable pacing. Explicit replay retains its existing retry behavior.
A backoff-delayed ordinary predecessor blocks successors; waiting-source and
review-required records may defer to other eligible work. Eligible deferred
work is fairly retried by last attempt, without dropping the predecessor barrier.
Only a persisted plan change or completion is progress, not a failed attempt or
its new timestamp. Existing cursors are retained, and a missing briefing cursor
is saved from the local baseline before replay can update context state.

When no fetch observations remain, unfinished in-scope merge records keep the
bounded worker alive through its existing capped pauses. A later observer wake
can restart a stopped worker once an outstanding merge lease and retry delay
expire. Unrelated branches, changed destinations, and reset-away ranges remain
saved without an automatic context write.

## Residual limits

The standalone CLI has no OS-managed customer capture/sync daemon. The launchd
`cxtd` service in this repository is backend-only and cannot scan customer disks.
The existing capture observer exits for a missing/unregistered or 30-minute-idle
transcript, has a 24-hour total limit, and can itself crash. Its ticks can also be
delayed by bounded capture/publication calls. If no observer survives or restarts
and no supported Git hook runs, queued work stays durable but is not guaranteed
to execute. This is conditional worker recovery, not crash-independent liveness.

The installed CLI sets up Git hooks, not a wrapper around the `git` executable.
An ordinary no-op `git fetch` need not invoke the remote-reference observation
hook. It is not a recovery guarantee. Existing observer startup/ticks or later
supported hooks recover the queue without another user command. Always-on
customer-independent recovery still requires lifecycle ownership beyond this
bounded CLI path and server reconciliation.

## Focused fixtures

The capture fixtures exercise startup and unchanged-transcript ticks during
capture/publication failures. Fetch fixtures use temporary Git repositories and
the generated installed reference-transaction hook, with a test executable in
place of the installed CLI. Subprocess fixtures crash or expire a real worker,
reopen the production durable PR-delivery store through observer startup, and
verify retained provenance and unchanged Git/context selection. Lease, active
worker, drained-queue, unconnected-repository, and no-op fetch checks do not
require a live provider, installation, or backend changes.

Pacing fixtures inject the worker's wait function: 45 successful pages accumulate
44 seconds of scheduled pauses without actually sleeping. Provider failures,
failed handoffs, and unchanged pending delivery still reach the one-minute
backoff cap, and resumed page progress resets pacing to one second.

Merge fixtures cover recovery without fetch records or `ORIG_HEAD`, unchanged
observer ticks restarting a stopped worker, durable retry pacing, frozen-plan
progress, baseline preservation, expired leases and stale-worker fencing,
predecessor barriers, deferred-item fairness, and cross-scope/reset rejection.
