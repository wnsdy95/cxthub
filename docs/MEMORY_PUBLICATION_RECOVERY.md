# Prepared memory archive publication

Tracking: #268.

## Failure and contract

A retained memory can refer to an old hook snapshot that was collected before its
first memory pointer reached the server. Recovery must recreate the snapshot and
attach the root memory atomically. It must not move a branch, rewrite natural
parents, lose typed claims, or overwrite a newer memory pointer.

The previous recovery request resubmitted the complete document or its staged
chunk manifest. `PublishMemoryArchive` called the ordinary object commit inside
the repository write transaction. That reconstructed and domain-validated the
archive again, even when durable document preparation had already completed.
Large archives could exceed the ordinary 30-second HTTP deadline repeatedly.

## Protocol

1. Negotiate `prepared_memory_archives_supported`, bounded chunks, supported
   chunk format and asynchronous document finalization.
2. Upload missing chunks through bounded requests.
3. Submit the document to the existing durable document queue and wait for its
   completed receipt. Cancellation stops the caller's wait, not accepted work.
4. Publish only snapshot metadata and root memory. The server verifies current
   repository-owned document content inside the write transaction, then creates
   the snapshot and CAS-attaches memory in that same transaction.

A completed job is not sufficient proof of current ownership or integrity. A
missing/corrupt document fails publication; a document owned only by another
repository cannot be attached. Retrying preparation recreates a collected body.
Conflicting memory still returns conflict. Permission checks run at publication
as well as preparation. PostgreSQL rolls back snapshot, memory and revision on
failure while retaining a document prepared by an earlier completed transaction.

Legacy clients can still send a body or manifest. New clients refuse chunked
recovery against servers that do not advertise the prepared protocol; they do
not silently downgrade to a slow or non-atomic sequence. Envelope-only documents
retain the small inline path. No timeout, hash, snapshot ID, or storage migration
changes are required.

Current stored-document verification still reads/hash-checks owned bytes and may
perform domain validation on a cold verification cache. This change removes
redundant transfer, reconstruction and writes from recovery; it does not claim
constant-cost document verification or solve all archive-wide push costs.

## Verification

- Client protocol: preparation precedes publication; no body/manifest is replayed;
  unsupported servers, rejected jobs, cancellation and incorrect acknowledgements
  fail without publishing.
- Application: replay, current missing/corrupt proof, foreign repository isolation,
  preserved memory and no full decoder call on the prepared reference path.
- PostgreSQL: publication rollback and replay from another store/connection,
  alongside existing collection races and stale-memory conflict tests.

## Local operational measurement

The previously failing retained archive was replayed through the public client
API against the installed backend: 166,003,658 canonical bytes (44,991 events).
Publication completed in 16.34 seconds with ordinary HTTP limits, and reading
back the root memory produced the exact expected content hash. No refs were
moved by this targeted recovery. This is one local measurement, not a cloud
latency guarantee or proof that archive-wide synchronization has completed.

## Remaining GitHub rollout work

Real GitHub/Firebase registration and HTTPS callbacks require operator settings;
fixture tests cannot substitute for real personal/organization installation,
login, webhook delivery, revocation and reconnect verification.

Fork PR context import needs both immutable source/destination repository IDs
and a separate source-context sharing contract. A code merge alone does not
prove authorization to transfer private conversations. Do not remove the current
cross-repository exclusion before that contract and transactional import exist.
