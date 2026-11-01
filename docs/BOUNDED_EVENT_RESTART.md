# Bounded restart of unpublished event preparation

## Contract

When the current native event has emitted zero bytes, normalization can save its
source using the existing Stage v1 `Initial` entry. Recovery restarts only that
event's preparation. Completed event occurrences, output, and the decoder input
position remain intact. Equal sequence numbers do not identify the same event.

An event with partially emitted bytes continues through the existing prepared
checkpoint. Restoring a source-only event alongside a nonzero written offset is
rejected. Source generation, ownership, lease, hash, and atomic publication
checks remain mandatory. This change adds no wire format or database migration.

Preparation does not mutate the native source. Its map keys and replacement
ordering are derived structures, so restarting from the source produces the
same canonical bytes. The cost of preparing the current event again is an
explicit tradeoff; arbitrary runtime frames and iterators are not serialized.

## Verification

`TestNormalizationCheckpointPGPreparingColdRestart` runs a real producer
subprocess against isolated local PostgreSQL. After one event is completed and
the next event has begun preparation, it saves a durable checkpoint. The parent
then verifies a SIGKILL exit and removes the producer's temporary files.

A separate consumer process obtains a new claim, restores the exact saved
control, verifies the canonical result, and publishes it once. A second publish
is rejected. The test also checks original context, ref, and memory preservation.
Its fixture includes a 5,001-element array and repeated event sequence numbers.

Domain tests additionally retain the original 5,000-entry nested-map and
1,200-replacement cases. Existing partial-output manual/automatic recovery,
commit-response loss, corruption, ownership, generation, and lease regressions
remain enabled. Their resource and fixture deadlines are unchanged.

The `Normalization restart` workflow checks this bounded scope independently of
the unfinished foundation's broader CI. It does not replace those required
checks or make the foundation ready to merge.

## Limits

- Restart inside an event repeats its unpublished preparation, not its exact
  internal instruction cursor.
- The new hard-stop cut uses an explicit checkpoint. It does not claim every
  automatic zero-output checkpoint cut has been fault-injected.
- Accounted heap limits are not operating-system RSS measurements.
- Passing this scope does not establish full rollout, old-binary compatibility,
  production capacity, or completion of the broader refactoring plan.
