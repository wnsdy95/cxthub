#!/usr/bin/env bash
# PostgreSQL contract lanes shared by CI (every PR and main push) and CI stress (nightly).
#
# Usage (from backend/): bash ../scripts/ci-backend-contracts.sh {store|app|transports}
#   SHARD=INDEX/TOTAL  required for store and app; every shard derives the same ordered
#                      selection, so the shards are disjoint and together run every test.
#   COUNT=N            repetitions for the race-checked lanes (default 1; CI stress uses 3).
#
# --first-test order matters for sharding: tests are dealt in snake order, so the
# slowest tests are listed first to land on different shards. Projection worker tests
# stay at the front so each still runs against an almost empty shared database.
set -euo pipefail

LANE="${1:-}"
COUNT="${COUNT:-1}"
RUNNER=../scripts/test-go-batches.py

if [[ ! "$COUNT" =~ ^[1-9][0-9]*$ ]]; then
  echo "[contracts] COUNT must be a positive integer" >&2
  exit 2
fi
shard_args=()
case "$LANE" in
  store|app)
    if [[ ! "${SHARD:-}" =~ ^[1-9][0-9]*/[1-9][0-9]*$ ]]; then
      echo "[contracts] SHARD=INDEX/TOTAL is required for the $LANE lane" >&2
      exit 2
    fi
    shard_args=(--shard "$SHARD")
    ;;
  transports) ;;
  *)
    echo "Usage: $0 {store|app|transports}" >&2
    exit 2
    ;;
esac

case "$LANE" in
  store)
    python3 "$RUNNER" ./internal/adapters/store/ --tags postgres --batch-size 1 "${shard_args[@]}" \
      --first-test TestDocRangeEnqueueBudgetPGTotalTimeoutRollback \
      --first-test TestDocNativeEventsPGRawDecodeRetainsFencesAndPublishes \
      --first-test TestDocCanonicalPagesPGActiveNativePreparationRetainsFences \
      --first-test TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesOwnership \
      --first-test TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesCancellation \
      --first-test TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesArgumentFence \
      --first-test TestDocToolValuesPGColdPreparationCounts \
      --first-test TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesCancellationBeforeOwnershipFailure \
      --first-test TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesCancellationBeforeArgumentFence \
      --first-test TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesSourceVersion \
      --first-test TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesCancellationBeforeSourceConflict \
      --first-test TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesJobFence \
      --first-test TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesLeaseExpiry \
      --first-test TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesSourceIncarnation \
      --first-test TestDocCanonicalPagesPGActiveNativePreparationRetainsFencesSnapshotIdentity \
      --first-test TestNormalizationCheckpointPGPreparationSavesOnlyNewRecoveryProgress \
      --first-test TestDocToolOrdersPGColdPreparationCost \
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
    python3 "$RUNNER" ./internal/app/ --tags postgres --race --run '^TestPG' --count "$COUNT" --batch-size 1 "${shard_args[@]}" \
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
    # Single-run app contracts outside the TestPG selection go to the lightest (last) shard.
    if [ "${SHARD#*/}" = "${SHARD%/*}" ]; then
      go test -tags postgres ./internal/app/ -race -run '^Test(P[69]PG|RootStartupPG|PostgresCatalogVersion)' -count=1 -timeout=120s -v
      python3 "$RUNNER" ./internal/app/ --tags postgres --race --run '^TestActivityCommitHistoryPG$'
      python3 "$RUNNER" ./internal/app/ --tags postgres --race \
        --run '^TestDisplayMaintenanceProcessPGCollectorRestartCancellationAndPublishedReads$' \
        --require-test TestDisplayMaintenanceProcessPGCollectorRestartCancellationAndPublishedReads
    fi
    ;;

  transports)
    python3 "$RUNNER" ./internal/adapters/delivery/http/ --tags postgres --race --run '^TestPG' --count "$COUNT" --batch-size 1 \
      --require-test TestPGDocFindPreparedHTTPQueuesThenContinuesRoot \
      --require-test TestPGDocFindPreparedHTTPQueuesThenContinuesLegacy
    go test -tags postgres ./internal/adapters/federation/ -race -run '^TestPG' -count="$COUNT" -timeout=120s -v
    go test -tags postgres ./internal/mcpserver/ -race -run '^TestPG' -timeout=120s -v
    python3 "$RUNNER" ./internal/serverruntime/ --tags postgres --race --run '^Test(ExecutionProfilePostgres|BoundedExecutionProcessesPG|InfrastructurePostgres|RuntimePostgresCallersReuseInfrastructurePool)'
    python3 "$RUNNER" ./cmd/cxtd/ --tags postgres --race --run '^TestServerLifecyclePGProcess(SIGTERM|BindFailure)'
    python3 "$RUNNER" ./internal/adapters/delivery/mcp/ --tags postgres --race --count "$COUNT" --batch-size 1 \
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
    python3 "$RUNNER" ./internal/adapters/delivery/mcp/ --tags postgres --race --run '^TestPGMCPResourceCutover$'
    python3 "$RUNNER" ./cmd/cxt-admin/ --tags postgres --race --run '^TestIdentityKeyCommandRejectsSecretsWithoutEchoingConfiguration$'

    # Bounded normalization restart under the race detector (formerly the separate
    # "Normalization restart" workflow). The store lane runs the same tests without
    # -race; registration of every name is enforced by --no-batch-timeout.
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
    ;;
esac
