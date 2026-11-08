#!/usr/bin/env bash
# PostgreSQL contract lanes shared by CI (required PR tier) and CI heavy (post-merge tier).
#
# Usage (from backend/): bash ../scripts/ci-backend-contracts.sh {store|app|transports}
#   TIER=fast   (default) every test not in scripts/ci-heavy-tests.txt, once, no race detector.
#               This is the required tier on every pull request and main push.
#   TIER=heavy  store: the listed heavy tests; app and transports: every test under the race
#               detector, repeated COUNT times, plus the race-only restart and admission checks.
#               Runs on main pushes, nightly, and pull requests labelled ci:full.
#   WORKERS=N   run N batches at once; each worker gets its own freshly created database
#               derived from CXT_TEST_DSN (<database>_w1 .. <database>_wN).
#   SHARD=I/N   optional; split the ordered selection across jobs (used by heavy app).
#   COUNT=N     repetitions for the race-checked heavy lanes (default 1).
#
# --first-test order matters: the slowest tests are listed first so they start first, and the
# projection worker tests stay at the front so each runs against an almost empty database.
set -euo pipefail
# ${array[@]+"${array[@]}"} keeps empty arrays safe under set -u on bash 3.2 as well.

LANE="${1:-}"
TIER="${TIER:-fast}"
COUNT="${COUNT:-1}"
WORKERS="${WORKERS:-1}"
RUNNER=../scripts/test-go-batches.py
HEAVY=../scripts/ci-heavy-tests.txt

case "$LANE" in
  store|app|transports) ;;
  *) echo "Usage: $0 {store|app|transports}" >&2; exit 2 ;;
esac
case "$TIER" in
  fast|heavy) ;;
  *) echo "[contracts] TIER must be fast or heavy" >&2; exit 2 ;;
esac
for value in "$COUNT" "$WORKERS"; do
  if [[ ! "$value" =~ ^[1-9][0-9]*$ ]]; then
    echo "[contracts] COUNT and WORKERS must be positive integers" >&2
    exit 2
  fi
done
if [ "$TIER" = fast ] && [ "$COUNT" != 1 ]; then
  echo "[contracts] the fast tier runs every test once" >&2
  exit 2
fi
shard_args=()
if [ -n "${SHARD:-}" ]; then
  if [[ ! "$SHARD" =~ ^[1-9][0-9]*/[1-9][0-9]*$ ]]; then
    echo "[contracts] SHARD must be INDEX/TOTAL" >&2
    exit 2
  fi
  shard_args=(--shard "$SHARD")
fi
last_shard=true
if [ -n "${SHARD:-}" ] && [ "${SHARD#*/}" != "${SHARD%/*}" ]; then
  last_shard=false
fi

# Per-worker databases: tests that read database-wide counters or claim leftover jobs must
# not see another worker's rows, so every worker starts from its own empty database.
worker_args=()
if [ "$WORKERS" -gt 1 ]; then
  : "${CXT_TEST_DSN:?CXT_TEST_DSN is required for parallel workers}"
  prefix="${CXT_TEST_DSN%%\?*}"
  query="${CXT_TEST_DSN#"$prefix"}"
  database="${prefix##*/}"
  if [[ ! "$database" =~ ^[a-z_][a-z0-9_]*$ ]]; then
    echo "[contracts] CXT_TEST_DSN must name a plain lowercase database" >&2
    exit 2
  fi
  for worker in $(seq 1 "$WORKERS"); do
    psql "$CXT_TEST_DSN" -v ON_ERROR_STOP=1 -q \
      -c "DROP DATABASE IF EXISTS ${database}_w${worker}" -c "CREATE DATABASE ${database}_w${worker}"
  done
  worker_args=(--workers "$WORKERS" --worker-env "CXT_TEST_DSN=${prefix%/*}/${database}_w{worker}${query}")
fi

if [ "$TIER" = fast ]; then
  race=()
  selection=(--heavy-list "$HEAVY" --tier fast)
else
  race=(--race)
  selection=(--heavy-list "$HEAVY" --tier all)
fi

case "$LANE" in
  store)
    # The store lane has never used the race detector; its heavy tier is the listed tests.
    [ "$TIER" = heavy ] && selection=(--heavy-list "$HEAVY" --tier heavy)
    python3 "$RUNNER" ./internal/adapters/store/ --tags postgres --batch-size 1 \
      "${selection[@]}" ${worker_args[@]+"${worker_args[@]}"} ${shard_args[@]+"${shard_args[@]}"} \
      --first-test TestDocRangeEnqueueBudgetPGTotalTimeoutRollback \
      --first-test TestDocNativeEventsPGRawDecodeRetainsFencesAndPublishes \
      --first-test TestDocCanonicalPagesPGActiveNativePreparationRetainsFences \
      --first-test TestNormalizationCheckpointPGPreparationSavesOnlyNewRecoveryProgress \
      --first-test TestDocToolValuesPGColdPreparationCounts \
      --first-test TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesOwnership \
      --first-test TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesCancellationBeforeOwnershipFailure \
      --first-test TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesCancellationBeforeArgumentFence \
      --first-test TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesSnapshotIdentity \
      --first-test TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesSourceVersion \
      --first-test TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesSourceIncarnation \
      --first-test TestDocToolOrdersPGColdPreparationCost \
      --first-test TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesCancellationBeforeSourceConflict \
      --first-test TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesCancellation \
      --first-test TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesArgumentFence \
      --first-test TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesJobFence \
      --first-test TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesLeaseExpiry \
      --no-batch-timeout TestDocCanonicalPagesPGActiveNativePreparationRetainsFences \
      --no-batch-timeout TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesSourceVersion \
      --no-batch-timeout TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesSourceIncarnation \
      --no-batch-timeout TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesOwnership \
      --no-batch-timeout TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesLeaseExpiry \
      --no-batch-timeout TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesJobFence \
      --no-batch-timeout TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesSnapshotIdentity \
      --no-batch-timeout TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesArgumentFence \
      --no-batch-timeout TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesCancellation \
      --no-batch-timeout TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesCancellationBeforeSourceConflict \
      --no-batch-timeout TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesCancellationBeforeOwnershipFailure \
      --no-batch-timeout TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesCancellationBeforeArgumentFence \
      --no-batch-timeout TestDocNativeBlockPGActivePagesRetainFencesAndPublish \
      --no-batch-timeout TestDocNativeEventsPGRawDecodeRetainsFencesAndPublishes \
      --no-batch-timeout TestDocToolValuesPGColdPreparationCounts \
      --no-batch-timeout TestNormalizationCheckpointPGPreparationSavesOnlyNewRecoveryProgress \
      --no-batch-timeout TestNormalizationCheckpointPGPreparingColdRestart \
      --no-batch-timeout TestNormalizationCheckpointPGStageOutputColdRestartBeforeInputConsumed \
      --require-test TestLegacyByteRecoveryPGColdChild \
      --require-test TestLegacyByteRecoveryPGColdRestartPublishesExactCanonicalSource \
      --require-test TestNormalizationCheckpointPGColdChild \
      --require-test TestNormalizationCheckpointPGColdRestartPublishes \
      --require-test TestPGRootWriterCost \
      --require-test TestPGRootWriterCostPreparationReferenceRead \
      --require-test TestPGRootWriterWait \
      --allow-skip TestLegacyProofStreamAllocationQualification \
      --allow-skip TestLegacyStreamingPGMemoryQualification \
      --allow-skip TestPGCycleValidationComparison \
      --allow-skip TestPGDocSearchStageQualification \
      --allow-skip TestPGMemoryCapturePersistenceCostD3ac \
      --allow-skip TestPGRootPublicationCost \
      --allow-skip TestPGRootPublicationCostWriterBarrier \
      --allow-skip TestRootReaderLegacyClassificationCost \
      --allow-skip TestStoredConversationReferencePerformance
    ;;

  app)
    python3 "$RUNNER" ./internal/app/ --tags postgres ${race[@]+"${race[@]}"} --run '^TestPG' --count "$COUNT" --batch-size 1 \
      "${selection[@]}" ${worker_args[@]+"${worker_args[@]}"} ${shard_args[@]+"${shard_args[@]}"} \
      --first-test TestPGGraphProjectionWorkerResumesAndMatchesLiveGitPRGraph \
      --first-test TestPGInvitationEmailWorkerDirectClaimAndCompletionRollback \
      --first-test TestPGPRPromotionWorkerDirectSharedAdmissionLeaseReplacementAndRecovery \
      --first-test TestPGPendingProjectionWorkerMatchesDirectClassificationAcrossMutations \
      --first-test TestPGWave40ProjectionWorkersCaptureRestartPublishAndQualifyWithoutService \
      --first-test TestPGWave41NotificationFactoryDurableRetryRestartAndStableEventID \
      --first-test TestPGWave41NotificationFactoryFencesStaleLeaseAndCurrentPolicy \
      --first-test TestPGDocRangeNativePagesActiveNormalizationParksAndRechecksFencesRawReplacement \
      --first-test TestPGDocRangeNativeBlockPagedNormalizationParksAndRechecksFences \
      --first-test TestPGDocRangeNativePagesActiveNormalizationParksAndRechecksFencesReplacement \
      --first-test TestPGDocRangeNativePagesActiveNormalizationParksAndRechecksFencesReplacementSourceVersion \
      --first-test TestPGDocRangeNativePagesActiveNormalizationParksAndRechecksFencesReplacementLeaseExpiry \
      --no-batch-timeout TestPGDocRangeNativeBlockPagedNormalizationParksAndRechecksFences \
      --no-batch-timeout TestPGDocRangeNativePagesActiveNormalizationParksAndRechecksFencesRawReplacement \
      --no-batch-timeout TestPGDocRangeNativePagesActiveNormalizationParksAndRechecksFencesReplacement \
      --no-batch-timeout TestPGDocRangeNativePagesActiveNormalizationParksAndRechecksFencesReplacementLeaseExpiry \
      --no-batch-timeout TestPGDocRangeNativePagesActiveNormalizationParksAndRechecksFencesReplacementSourceVersion \
      --allow-skip TestPGMemoryPreparationCostD3ab \
      --allow-skip TestPGReadModelMixedProgress
    # Single-run app contracts outside the TestPG selection (on the last shard when sharded).
    if [ "$last_shard" = true ]; then
      go test -tags postgres ./internal/app/ ${race[@]+"${race[@]}"} -run '^Test(P[69]PG|RootStartupPG|PostgresCatalogVersion)' -count=1 -timeout=120s -v
      python3 "$RUNNER" ./internal/app/ --tags postgres ${race[@]+"${race[@]}"} --run '^TestActivityCommitHistoryPG$'
      python3 "$RUNNER" ./internal/app/ --tags postgres ${race[@]+"${race[@]}"} \
        --run '^TestDisplayMaintenanceProcessPGCollectorRestartCancellationAndPublishedReads$' \
        --require-test TestDisplayMaintenanceProcessPGCollectorRestartCancellationAndPublishedReads
    fi
    ;;

  transports)
    python3 "$RUNNER" ./internal/adapters/delivery/http/ --tags postgres ${race[@]+"${race[@]}"} --run '^TestPG' --count "$COUNT" --batch-size 1 \
      "${selection[@]}" ${worker_args[@]+"${worker_args[@]}"} \
      --require-test TestPGDocFindPreparedHTTPQueuesThenContinuesRoot \
      --require-test TestPGDocFindPreparedHTTPQueuesThenContinuesLegacy
    go test -tags postgres ./internal/adapters/federation/ ${race[@]+"${race[@]}"} -run '^TestPG' -count="$COUNT" -timeout=120s -v
    go test -tags postgres ./internal/mcpserver/ ${race[@]+"${race[@]}"} -run '^TestPG' -timeout=120s -v
    python3 "$RUNNER" ./internal/serverruntime/ --tags postgres ${race[@]+"${race[@]}"} "${selection[@]}" \
      --run '^Test(ExecutionProfilePostgres|BoundedExecutionProcessesPG|InfrastructurePostgres|RuntimePostgresCallersReuseInfrastructurePool)'
    python3 "$RUNNER" ./cmd/cxtd/ --tags postgres ${race[@]+"${race[@]}"} "${selection[@]}" --run '^TestServerLifecyclePGProcess(SIGTERM|BindFailure)'
    python3 "$RUNNER" ./internal/adapters/delivery/mcp/ --tags postgres ${race[@]+"${race[@]}"} --count "$COUNT" --batch-size 1 \
      "${selection[@]}" ${worker_args[@]+"${worker_args[@]}"} \
      --run '^Test(RootProjectionPGConsumersWithoutPersistentIndex|PRContributionTransportParity|PGConversationFindMCPPreparationRoundTrip.*)$' \
      --require-test TestRootProjectionPGConsumersWithoutPersistentIndex \
      --require-test TestPRContributionTransportParity \
      --require-test TestPGConversationFindMCPPreparationRoundTrip \
      --require-test TestPGConversationFindMCPPreparationRoundTripReasoning \
      --require-test TestPGConversationFindMCPPreparationRoundTripMessage \
      --require-test TestPGConversationFindMCPPreparationRoundTripSummary \
      --require-test TestPGConversationFindMCPPreparationRoundTripSeed \
      --require-test TestPGConversationFindMCPPreparationRoundTripToolEdit \
      --require-test TestPGConversationFindMCPPreparationRoundTripToolWrite \
      --require-test TestPGConversationFindMCPPreparationRoundTripToolBash \
      --require-test TestPGConversationFindMCPPreparationRoundTripToolGeneric
    python3 "$RUNNER" ./internal/adapters/delivery/mcp/ --tags postgres ${race[@]+"${race[@]}"} "${selection[@]}" --run '^TestPGMCPResourceCutover$'
    python3 "$RUNNER" ./cmd/cxt-admin/ --tags postgres ${race[@]+"${race[@]}"} "${selection[@]}" --run '^TestIdentityKeyCommandRejectsSecretsWithoutEchoingConfiguration$'

    if [ "$TIER" = heavy ]; then
      # Race-only checks. Without -race these run in the store lane and backend-checks.
      # Bounded normalization restart (formerly the "Normalization restart" workflow):
      # registration of every name is enforced by --no-batch-timeout.
      GOMAXPROCS=2 go test -json -p 1 -parallel=1 -race -count=1 -timeout=0 ./internal/domain \
        -run '^(TestLegacyNormalizationPreparingRestart|TestLegacyNormalizationStageCheckpoint|TestLegacyNormalizationStageCapture|TestLegacyNativeEventLifecycle|TestLegacyNativeCanonicalOwnerReadyLifecycle)'
      GOMAXPROCS=2 python3 "$RUNNER" ./internal/adapters/store/ --tags postgres --race --batch-size 1 \
        --run '^(TestNormalizationCheckpointPGPreparationSavesOnlyNewRecoveryProgress|TestNormalizationCheckpointPGPreparingColdRestart|TestNormalizationCheckpointPGStageOutputColdRestartBeforeInputConsumed|TestNormalizationCheckpointPGAutomaticOutputGenerationReset|TestNormalizationCheckpointPGRootCommitFailureAndLostResponse|TestDocNormalizationWorkPGFencesEachPhase|TestNormalizationCheckpointPGPreservesLocalDamageAndRejectsDerivedProof)$' \
        --no-batch-timeout TestNormalizationCheckpointPGPreparationSavesOnlyNewRecoveryProgress \
        --no-batch-timeout TestNormalizationCheckpointPGPreparingColdRestart \
        --no-batch-timeout TestNormalizationCheckpointPGStageOutputColdRestartBeforeInputConsumed \
        --no-batch-timeout TestNormalizationCheckpointPGAutomaticOutputGenerationReset \
        --no-batch-timeout TestNormalizationCheckpointPGRootCommitFailureAndLostResponse \
        --no-batch-timeout TestDocNormalizationWorkPGFencesEachPhase \
        --no-batch-timeout TestNormalizationCheckpointPGPreservesLocalDamageAndRejectsDerivedProof
      # Admission progress on one CPU under the race detector (backend-checks runs it without).
      GOMAXPROCS=1 go test -race ./internal/app -run '^TestResourceAdmission(DispatchReservesBothClasses|MixedGraphVerification)$' -count=3 -timeout=120s
    fi
    ;;
esac
