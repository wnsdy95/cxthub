//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type comparisonMemoryProbePG struct {
	*store.PostgresStore
	different    bool
	afterCapture func()
	afterClaim   func()
	receipts     []domain.MemoryReadComparison
}

func (probe *comparisonMemoryProbePG) ClaimMemoryReadWork(ctx context.Context, repos []domain.ContentHash, lease time.Duration) (domain.MemoryReadWork, error) {
	job, err := probe.PostgresStore.ClaimMemoryReadWork(ctx, repos, lease)
	if err == nil && probe.afterClaim != nil {
		callback := probe.afterClaim
		probe.afterClaim = nil
		callback()
	}
	return job, err
}

type comparisonMemoryEvidencePG struct {
	outbound.GraphEvidenceReader
}

func (reader comparisonMemoryEvidencePG) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	value, err := reader.GraphEvidenceReader.GetRepo(ctx, repo)
	value.GitRemoteURL = "https://example.invalid/different/captured-origin"
	return value, err
}

func (probe *comparisonMemoryProbePG) BuildGraphSourceReader(ctx context.Context, checkpoint domain.RepositoryViewSourceCheckpoint, repo domain.Repo, entries []domain.RepositoryViewSourceEntry) (outbound.GraphEvidenceReader, error) {
	reader, err := probe.PostgresStore.BuildGraphSourceReader(ctx, checkpoint, repo, entries)
	if err != nil {
		return nil, err
	}
	if probe.afterCapture != nil {
		callback := probe.afterCapture
		probe.afterCapture = nil
		callback()
	}
	if probe.different {
		return comparisonMemoryEvidencePG{reader}, nil
	}
	return reader, nil
}

func (probe *comparisonMemoryProbePG) RecordMemoryReadComparison(ctx context.Context, job domain.MemoryReadWork, comparison domain.MemoryReadComparison) error {
	if err := probe.PostgresStore.RecordMemoryReadComparison(ctx, job, comparison); err != nil {
		return err
	}
	probe.receipts = append(probe.receipts, comparison)
	return nil
}

func TestPGMemoryComparisonRejectsMismatchPreservesPriorAndRecovers(test *testing.T) {
	service, storage, connection, repo, request, _ := qualifiedMemoryFixturePG(test)
	ctx := systemTestContext()
	memoryWorkConfigurePG(test, service, storage, repo)
	probe := &comparisonMemoryProbePG{PostgresStore: storage}
	service.meta = probe
	requirement, err := service.ObserveMemoryReadRequirement(ctx, repo, request)
	if err != nil {
		test.Fatal(err)
	}
	legacy, err := service.QueryEffectiveMemory(ctx, repo, request)
	if err != nil {
		test.Fatal(err)
	}
	service.deliveryCache = effectiveMemoryDeliveryCache{}
	service.deliveryCache.Store(legacy.StateHash, domain.HashContent([]byte("poisoned process cache")))
	if _, err := service.RequestMemoryReadPreparation(ctx, repo, request); err != nil {
		test.Fatal(err)
	}
	if err := service.ProcessMemoryReadPreparations(ctx, 1); err != nil {
		test.Fatal("uncached reference", err)
	}
	first, err := service.QueryMinimumMemory(ctx, repo, request, requirement, "")
	if err != nil || len(probe.receipts) != 1 || !probe.receipts[0].Matched() {
		test.Fatal("matching complete comparison missing", err)
	}
	advance := func() {
		if _, err := connection.Exec(ctx, `UPDATE snapshots SET message=message || ' next source' WHERE repo_id=$1`, repo); err != nil {
			test.Fatal(err)
		}
	}
	advance()
	probe.different = true
	newer, err := service.ObserveMemoryReadRequirement(ctx, repo, request)
	if err != nil {
		test.Fatal(err)
	}
	if err := service.ProcessMemoryReadPreparations(ctx, 1); !errors.Is(err, domain.ErrMemoryReadComparisonMismatch) {
		test.Fatal("divergent frozen source was published", err)
	}
	status, err := service.ReadMemoryPreparationStatus(ctx, repo, request)
	if err != nil || status.State != "rejected" || status.Reason != "comparison_mismatch" || len(probe.receipts) != 2 || probe.receipts[1].Matched() {
		test.Fatal("mismatch lost its durable diagnostic", status, err)
	}
	if page, err := service.QueryMinimumMemory(ctx, repo, request, newer, ""); !errors.Is(err, domain.ErrMemoryReadNeedsAttention) || !reflect.DeepEqual(page, domain.MemoryReadPage{}) {
		test.Fatal("mismatch exposed a page or retry loop", page, err)
	}
	pinned, err := service.QueryMinimumMemory(ctx, repo, request, requirement, first.Status.Generation)
	if err != nil || !reflect.DeepEqual(pinned.Page, first.Page) {
		test.Fatal("comparison failure damaged previously accepted output", err)
	}
	var durable int
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM memory_read_comparisons WHERE repo_id=$1`, repo).Scan(&durable); err != nil || durable != 2 {
		test.Fatal("comparison not persisted", durable, err)
	}
	probe.different = false
	advance()
	if err := service.ProcessMemoryReadPreparations(ctx, 1); err != nil {
		test.Fatal("new source did not recover", err)
	}
	recovered, err := service.QueryMinimumMemory(ctx, repo, request, newer, "")
	if err != nil || recovered.Status.Generation == first.Status.Generation || len(probe.receipts) != 3 || !probe.receipts[2].Matched() {
		test.Fatal("recovery without a fresh match", err)
	}
}

func TestPGMemoryComparisonReferenceSharesCaptureSnapshot(test *testing.T) {
	service, storage, connection, repo, request, _ := qualifiedMemoryFixturePG(test)
	ctx := systemTestContext()
	memoryWorkConfigurePG(test, service, storage, repo)
	requirement, err := service.ObserveMemoryReadRequirement(ctx, repo, request)
	if err != nil {
		test.Fatal(err)
	}
	probe := &comparisonMemoryProbePG{PostgresStore: storage, afterCapture: func() {
		if _, err := connection.Exec(ctx, `UPDATE snapshots SET message=message || ' after capture before reference' WHERE repo_id=$1`, repo); err != nil {
			test.Fatal(err)
		}
	}}
	service.meta = probe
	if _, err := service.RequestMemoryReadPreparation(ctx, repo, request); err != nil {
		test.Fatal(err)
	}
	if err := service.ProcessMemoryReadPreparations(ctx, 1); err != nil {
		test.Fatal("legitimate later source was reported as mismatch", err)
	}
	page, err := service.QueryMinimumMemory(ctx, repo, request, requirement, "")
	if err != nil || page.Status.Freshness != "catching_up" || len(probe.receipts) != 1 || !probe.receipts[0].Matched() || probe.receipts[0].Source != requirement.Minimum {
		test.Fatal("reference escaped the capture transaction", page.Status, err)
	}
	if _, err := service.memoryReadReference(ctx, repo, request); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("reference ran without an owned read snapshot", err)
	}
}

func TestPGMemoryComparisonQuietLegacyCompletionAutomaticallyRecovers(test *testing.T) {
	service, storage, connection, repo, request, prepared := qualifiedMemoryFixturePG(test)
	ctx := systemTestContext()
	memoryWorkConfigurePG(test, service, storage, repo)
	if _, err := service.RequestMemoryReadPreparation(ctx, repo, request); err != nil {
		test.Fatal(err)
	}
	job, err := storage.ClaimMemoryReadWork(ctx, []domain.ContentHash{repo}, time.Minute)
	if err != nil {
		test.Fatal(err)
	}
	if err := service.persistPreparedMemoryRead(ctx, prepared, time.Now().Add(time.Hour)); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.FinishMemoryReadWork(ctx, job, prepared.qualification); err != nil {
		test.Fatal("legacy completion", err)
	}
	if _, err := service.QueryCurrentMemory(ctx, repo, request); err != nil {
		test.Fatal("strict contract changed", err)
	}
	requirement, err := service.ObserveMemoryReadRequirement(ctx, repo, request)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := service.QueryMinimumMemory(ctx, repo, request, requirement, ""); !errors.Is(err, domain.ErrMemoryReadNotReady) {
		test.Fatal("legacy output served without comparison", err)
	}
	if err := service.ProcessMemoryReadPreparations(ctx, 1); err != nil {
		test.Fatal("automatic re-comparison", err)
	}
	if _, err := service.QueryMinimumMemory(ctx, repo, request, requirement, ""); err != nil {
		test.Fatal("quiet repository stranded after receipt gate", err)
	}
	status, err := service.ReadMemoryPreparationStatus(ctx, repo, request)
	if err != nil || status.State != "completed" || status.Version <= job.Version {
		test.Fatal("re-comparison did not acquire a new work fence", status, err)
	}
	before, err := storage.ReadRepositoryMemorySourceGeneration(ctx, repo)
	if err != nil || before != requirement.Minimum {
		test.Fatal("re-comparison required a business source change", err)
	}
	if err := service.ProcessMemoryReadPreparations(ctx, 1); err != nil {
		test.Fatal(err)
	}
	after, err := service.ReadMemoryPreparationStatus(ctx, repo, request)
	if err != nil || !reflect.DeepEqual(status, after) {
		test.Fatal("matched quiet target was repeatedly prepared", err)
	}
	var receipts int
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM memory_read_comparisons WHERE repo_id=$1`, repo).Scan(&receipts); err != nil || receipts != 1 {
		test.Fatal("automatic re-comparison failed to persist one receipt", receipts, err)
	}
}

func TestPGMemoryComparisonNewSourceRetryStopsWhenSourceIsQuiet(test *testing.T) {
	service, storage, connection, repo, request, _ := qualifiedMemoryFixturePG(test)
	ctx := systemTestContext()
	memoryWorkConfigurePG(test, service, storage, repo)
	probe := &comparisonMemoryProbePG{PostgresStore: storage}
	service.meta = probe
	if _, err := service.RequestMemoryReadPreparation(ctx, repo, request); err != nil {
		test.Fatal(err)
	}
	if err := service.ProcessMemoryReadPreparations(ctx, 1); err != nil {
		test.Fatal(err)
	}
	prior, err := service.ReadMemoryPreparationStatus(ctx, repo, request)
	if err != nil {
		test.Fatal(err)
	}
	advance := func() {
		if _, err := connection.Exec(ctx, `UPDATE snapshots SET message=message || ' comparison source advance' WHERE repo_id=$1`, repo); err != nil {
			test.Fatal(err)
		}
	}
	advance()
	probe.different, probe.afterClaim = true, advance
	if err := service.ProcessMemoryReadPreparations(ctx, 1); !errors.Is(err, domain.ErrMemoryReadComparisonMismatch) {
		test.Fatal("advanced mismatching candidate was accepted", err)
	}
	retrying, err := service.ReadMemoryPreparationStatus(ctx, repo, request)
	if err != nil || retrying.State != "retrying" || retrying.Reason != "source_changed" || retrying.QualificationID != prior.QualificationID {
		test.Fatal("new source retry lost prior pointer", retrying, err)
	}
	if err := service.ProcessMemoryReadPreparations(ctx, 1); !errors.Is(err, domain.ErrMemoryReadComparisonMismatch) {
		test.Fatal("quiet mismatch did not stop", err)
	}
	rejected, err := service.ReadMemoryPreparationStatus(ctx, repo, request)
	if err != nil || rejected.State != "rejected" || rejected.Reason != "comparison_mismatch" || rejected.QualificationID != prior.QualificationID || len(probe.receipts) != 3 {
		test.Fatal("same-source mismatch became a retry loop", rejected, err)
	}
	for _, receipt := range probe.receipts[1:] {
		if receipt.Matched() {
			test.Fatal("mismatch diagnostics changed")
		}
	}
}
