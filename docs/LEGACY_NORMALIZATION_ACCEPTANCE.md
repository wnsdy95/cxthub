# Legacy normalization acceptance

Decision: 2026-11-03. A complete legacy-document conversion has no 30-second
product deadline. Correctness, recoverability and bounded resource use are the
acceptance criteria; elapsed time is reported separately.

Range jobs continue while their lease renews and the caller remains active;
they no longer inherit the ten-minute whole-job limit. Lease renewal still has
its bounded operation deadline, and lease loss or caller cancellation stops
the worker. Other document workers retain their existing work limits.

## Scope

This is background preparation of an existing document for verified range reads,
not the normal incremental upload of a new conversation chunk. The same source
and derived-format version can reuse a completed publication. Failed attempts,
new legacy input or a changed derived format may require more work.

## Required guarantees

- Preserve the original stored bytes and document identity. The final canonical
  bytes and hash must match the independent oracle.
- Preserve all messages, stable ordering, source/ownership/lease fences,
  cancellation, per-quantum work limits and memory/IO bounds.
- Keep incomplete results private. A fresh process must recover durable progress
  and finish without duplicate publication.
- Yield resources so foreground reads can complete while conversion is paused.
  A read deadline starts when that read is requested, not when conversion starts.
- After publication, range reads must retain their bounded-read assertions and
  must not fall back to reading the entire original document.

## Test interpretation

`TestPGDocRangeNativePagesActiveNormalizationParksAndRechecksFences*` and
`TestPGDocRangeNativeBlockPagedNormalizationParksAndRechecksFences*` verify
same-process parking, foreground progress, fences and exact publication. Their
full conversion no longer inherits a 30-second deadline; the foreground request
retains a separate 30-second test guard and the production admission policy.

`TestDocCanonicalPagesPGActiveNativePreparationRetainsFences*` uses a cancellable
test-lifetime context rather than the shared 30-second fixture context. Other
users of that shared fixture retain their existing timeout.

Deletion of legacy fixture snapshots, ownership and source blobs is also a
completion check, not a five-second latency requirement. Cleanup retains its
error assertions and relies on the enclosing runner/job watchdog instead of a
separate five-second deadline that can interrupt cascading checkpoint cleanup.

Fresh-process recovery is tested separately. None of the ordering, byte/hash,
publication, fence or work/resource assertions is removed by this decision.

Checkpoint serialization freezes the decoder. Tests distinguish those steps
from semantic preparation: serialization must acknowledge a valid continuation
without moving the decoder, and preparation must advance without restarting it.
Checkpoint steps do not consume the semantic-step budget. The raw 8192-child
fixture applies the existing checkpoint bound to each save, not a cumulative
bound borrowed from a smaller document, and reports the total saves and steps.
The per-step IO/memory limits and the outer preparation-progress bound remain.

The test-runner watchdog is an infrastructure hang guard, not a conversion
latency SLA. The full-conversion cases explicitly listed in CI use
`--no-batch-timeout` with isolated single-test batches, relying on the enclosing
CI job cancellation/watchdog rather than the default 120-second binary limit.
Other batches retain that limit. Test selection, repetitions and strict outcome
accounting remain unchanged; unknown or duplicate exception names are rejected.
A watchdog termination must still be reported as incomplete, never as a pass.
Whole-conversion elapsed times are measurements, not evidence of the latency of
an individual chunk upload or foreground page read.
