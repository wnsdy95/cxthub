//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func standaloneMemoryPreparationFixturePG(test *testing.T) (*Service, *store.PostgresStore, *pgxpool.Pool, domain.ContentHash, domain.EffectiveMemoryRequest) {
	test.Helper()
	service, storage, repo := collaborationPG(test)
	ctx := systemTestContext()
	connection, err := pgxpool.New(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(connection.Close)
	snapshot := collaborationSnapshot(test, storage, repo, "standalone memory preparation")
	digest := capturedMemoryDigestPG(snapshot, "standalone source")
	digest.Summary = strings.Repeat("standalone source "+string(repo), 900)
	hash, err := service.PutMemoryDigestCAS(ctx, repo, digest)
	if err != nil {
		test.Fatal(err)
	}
	request := domain.EffectiveMemoryRequest{
		Selection: domain.EffectiveMemorySelection{SnapshotID: snapshot, MemoryHash: hash, CodeCommit: effectiveOID(3)}, Limit: 1,
	}
	return service, storage, connection, repo, request
}

func newStandaloneMemoryPreparationPG(test *testing.T, metadata outbound.MetadataStore, storage *store.PostgresStore, selected []domain.ContentHash) *MemoryPreparationWorkers {
	test.Helper()
	resources, err := NewMemoryPreparationResources(MemoryPreparationResourceConfig{
		Metadata: metadata, Blobs: storage, Repositories: storage,
	})
	if err != nil {
		test.Fatal("standalone resources", err)
	}
	workers, err := NewMemoryPreparationWorkers(MemoryPreparationWorkerConfig{
		Profile: MemoryPreparationComplete, Resources: resources, Repositories: selected,
	})
	if err != nil {
		test.Fatal("standalone workers", err)
	}
	return workers
}

func enableStandaloneMemoryPreparationPG(test *testing.T, storage *store.PostgresStore, repo domain.ContentHash) {
	test.Helper()
	if err := storage.ConfigureMemoryReadWork(systemTestContext(), repo, true); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if err := storage.ConfigureMemoryReadWork(systemTestContext(), repo, false); err != nil {
			test.Error(err)
		}
	})
}

type standaloneMemoryPreparationProbePG struct {
	*comparisonMemoryProbePG
	finishes     int
	beforeFinish func(context.Context, domain.MemoryReadWork, domain.MemoryReadQualification) error
}

func (probe *standaloneMemoryPreparationProbePG) FinishMinimumMemoryReadWork(ctx context.Context, job domain.MemoryReadWork, qualification domain.MemoryReadQualification) (domain.ContentHash, error) {
	probe.finishes++
	if probe.beforeFinish != nil {
		callback := probe.beforeFinish
		probe.beforeFinish = nil
		if err := callback(ctx, job, qualification); err != nil {
			return "", err
		}
	}
	return probe.PostgresStore.FinishMinimumMemoryReadWork(ctx, job, qualification)
}

func assertStandaloneMemoryComparisonPG(test *testing.T, storage *store.PostgresStore, connection *pgxpool.Pool, comparison domain.MemoryReadComparison) {
	test.Helper()
	ctx := systemTestContext()
	repo := comparison.Source.Graph.RepoID
	var raw []byte
	if err := connection.QueryRow(ctx, `SELECT body FROM memory_read_comparisons WHERE repo_id=$1 AND read_id=$2`, repo, comparison.ReadID).Scan(&raw); err != nil {
		test.Fatal("durable comparison", err)
	}
	var restored domain.MemoryReadComparison
	if err := domain.DecodeRepositorySourceJSON(ctx, raw, &restored); err != nil || restored != comparison {
		test.Fatal("comparison receipt changed in storage", err)
	}
	header, err := storage.ReadMemoryReadHeader(ctx, repo, comparison.ReadID)
	if err != nil {
		test.Fatal("durable read header", err)
	}
	if err := domain.ValidateMemoryReadComparison(ctx, restored, header); err != nil {
		test.Fatal("comparison lost its output/source/target binding", err)
	}
	capture, err := storage.ReadMemoryCapture(ctx, repo, comparison.CaptureID)
	if err != nil || capture.ID != comparison.CaptureID || capture.Header.Generation != comparison.Source {
		test.Fatal("comparison input was not durably captured", err)
	}
}

func TestPGMemoryPreparationWorkersStandaloneCaptureQualificationIdentityAndRestart(test *testing.T) {
	service, storage, connection, repo, request := standaloneMemoryPreparationFixturePG(test)
	ctx := inbound.WithDocumentIdentities(systemTestContext(), []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	if _, err := connection.Exec(ctx, `UPDATE repos SET required_doc_identity=$2 WHERE id=$1`, repo, domain.DocumentIdentityRootV1); err != nil {
		test.Fatal(err)
	}
	workers := newStandaloneMemoryPreparationPG(test, storage, storage, []domain.ContentHash{repo})
	if prepared, err := workers.PrepareMemoryRead(systemTestContext(), repo, request, nil); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) || prepared != nil {
		test.Fatal("standalone preparation bypassed repository identity", err)
	}
	if prepared, err := workers.PrepareMemoryRead(context.Background(), repo, request, nil); !errors.Is(err, domain.ErrForbidden) || prepared != nil {
		test.Fatal("standalone preparation manufactured worker authority", err)
	}
	captured, err := workers.CaptureEffectiveMemorySource(ctx, repo, request, nil)
	if err != nil {
		test.Fatal("standalone capture", err)
	}
	if err := workers.ValidateCapturedMemorySource(ctx, repo, captured); err != nil {
		test.Fatal("standalone source validation", err)
	}
	witnesses, err := captured.ObjectWitnesses(ctx)
	if err != nil || len(witnesses) != 1 || len(witnesses[0].Components) == 0 {
		test.Fatal("standalone capture lost chunk witnesses", err)
	}
	prepared, err := workers.PrepareMemoryRead(ctx, repo, request, nil)
	if err != nil {
		test.Fatal("standalone preparation", err)
	}
	if prepared.qualification.Source != captured.Generation() || !reflect.DeepEqual(prepared.qualification.Witnesses, witnesses) {
		test.Fatal("preparation lost captured source or witnesses")
	}
	deadline := time.Now().Add(time.Hour)
	identifier, err := workers.PublishPreparedMemoryRead(ctx, prepared, deadline)
	if err != nil || identifier == "" {
		test.Fatal("standalone publication", err)
	}
	if duplicate, err := workers.PublishPreparedMemoryRead(ctx, prepared, deadline); err != nil || duplicate != identifier {
		test.Fatal("standalone publication was not idempotent", err)
	}
	if identifier, err := workers.PublishPreparedMemoryRead(systemTestContext(), prepared, deadline); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) || identifier != "" {
		test.Fatal("standalone publication bypassed repository identity", err)
	}
	reopened, err := store.NewPostgresStore(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	defer reopened.Close()
	restarted := newStandaloneMemoryPreparationPG(test, reopened, reopened, []domain.ContentHash{repo})
	input, err := reopened.ReadMemoryCapture(ctx, repo, prepared.input.ID)
	if err != nil || !reflect.DeepEqual(input, prepared.input) {
		test.Fatal("standalone capture did not survive reopening", err)
	}
	replayed, err := BuildMemoryRead(ctx, input, reopened)
	if err != nil || !reflect.DeepEqual(replayed, prepared.output) {
		test.Fatal("durable capture changed prepared output", err)
	}
	cold, err := restarted.PrepareIncrementalMemoryRead(ctx, repo, request, deadline, nil)
	if err != nil {
		test.Fatal("standalone cold anchor discovery", err)
	}
	base, err := domain.MemoryCaptureGraphBase(cold.input)
	if err != nil || cold.input.Header.Version != domain.MemoryCaptureDeltaVersion || base.ID != prepared.input.ID {
		test.Fatal("standalone restart failed to reuse durable full capture", err)
	}
	coldID, err := restarted.PublishPreparedMemoryRead(ctx, cold, deadline)
	if err != nil {
		test.Fatal("standalone restarted publication", err)
	}
	for page := 0; ; page++ {
		want, err := service.QueryEffectiveMemory(ctx, repo, request)
		if err != nil {
			test.Fatal(err)
		}
		for _, receipt := range []domain.ContentHash{identifier, coldID} {
			actual, err := service.QueryQualifiedMemory(ctx, repo, receipt, request)
			if err != nil || !reflect.DeepEqual(actual, want) {
				test.Fatal("standalone qualified page differs from live query", page, err)
			}
		}
		if want.NextCursor == "" {
			break
		}
		if page > 30 || len(want.Items) == 0 {
			test.Fatal("standalone qualification pagination did not advance")
		}
		request.Cursor = want.NextCursor
	}
	request.Cursor = ""
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if prepared, err := restarted.PrepareIncrementalMemoryRead(bound, repo, request, deadline, nil); !errors.Is(err, domain.ErrConflict) || prepared != nil {
			test.Fatal("standalone restart borrowed an existing transaction", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}

func TestPGMemoryPreparationWorkersStandaloneMinimumComparisonMismatchAndRecovery(test *testing.T) {
	service, storage, connection, repo, request := standaloneMemoryPreparationFixturePG(test)
	ctx := systemTestContext()
	probe := &standaloneMemoryPreparationProbePG{comparisonMemoryProbePG: &comparisonMemoryProbePG{PostgresStore: storage}}
	selected := []domain.ContentHash{repo}
	workers := newStandaloneMemoryPreparationPG(test, probe, storage, selected)
	selected[0] = hh("standalone selection was copied")
	var scopes int
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM memory_read_work_scopes WHERE repo_id=$1`, repo).Scan(&scopes); err != nil || scopes != 0 {
		test.Fatal("standalone factory configured durable work", scopes, err)
	}
	enableStandaloneMemoryPreparationPG(test, storage, repo)
	requirement, err := service.ObserveMemoryReadRequirement(ctx, repo, request)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := service.RequestMemoryReadPreparation(ctx, repo, request); err != nil {
		test.Fatal(err)
	}
	if err := workers.ProcessMemoryReadPreparations(ctx, 1); err != nil {
		test.Fatal("standalone process", err)
	}
	first, err := service.QueryMinimumMemory(ctx, repo, request, requirement, "")
	if err != nil || !first.Status.Complete || first.Status.Freshness != "current" || len(probe.receipts) != 1 || !probe.receipts[0].Matched() || probe.finishes != 1 {
		test.Fatal("standalone worker did not publish a matched minimum", first.Status, err)
	}
	assertStandaloneMemoryComparisonPG(test, storage, connection, probe.receipts[0])
	assertCurrentMemoryParityPG(test, service, repo, request)
	advance := func() {
		if _, err := connection.Exec(ctx, `UPDATE snapshots SET message=message || ' standalone source advance' WHERE repo_id=$1`, repo); err != nil {
			test.Fatal(err)
		}
	}
	advance()
	probe.different = true
	newer, err := service.ObserveMemoryReadRequirement(ctx, repo, request)
	if err != nil {
		test.Fatal(err)
	}
	if err := workers.ProcessMemoryReadPreparations(ctx, 1); !errors.Is(err, domain.ErrMemoryReadComparisonMismatch) {
		test.Fatal("standalone divergent candidate was accepted", err)
	}
	status, err := service.ReadMemoryPreparationStatus(ctx, repo, request)
	if err != nil || status.State != "rejected" || status.Reason != "comparison_mismatch" || len(probe.receipts) != 2 || probe.receipts[1].Matched() || probe.finishes != 1 {
		test.Fatal("standalone mismatch reached finish or lost diagnostics", status, err)
	}
	assertStandaloneMemoryComparisonPG(test, storage, connection, probe.receipts[1])
	var qualifications int
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM memory_read_qualifications WHERE repo_id=$1`, repo).Scan(&qualifications); err != nil || qualifications != 1 {
		test.Fatal("standalone mismatch created a qualification", qualifications, err)
	}
	if page, err := service.QueryMinimumMemory(ctx, repo, request, newer, ""); !errors.Is(err, domain.ErrMemoryReadNeedsAttention) || !reflect.DeepEqual(page, domain.MemoryReadPage{}) {
		test.Fatal("standalone mismatch exposed a minimum page", err)
	}
	pinned, err := service.QueryMinimumMemory(ctx, repo, request, requirement, first.Status.Generation)
	if err != nil || !reflect.DeepEqual(pinned.Page, first.Page) {
		test.Fatal("standalone rejection damaged the accepted generation", err)
	}
	probe.different = false
	advance()
	if err := workers.ProcessMemoryReadPreparations(ctx, 1); err != nil {
		test.Fatal("standalone source recovery", err)
	}
	recovered, err := service.QueryMinimumMemory(ctx, repo, request, newer, "")
	if err != nil || recovered.Status.Generation == first.Status.Generation || recovered.Status.Freshness != "current" || len(probe.receipts) != 3 || !probe.receipts[2].Matched() || probe.finishes != 2 {
		test.Fatal("standalone recovery did not require a fresh match", recovered.Status, err)
	}
	assertStandaloneMemoryComparisonPG(test, storage, connection, probe.receipts[2])
	assertCurrentMemoryParityPG(test, service, repo, request)
}

func TestPGMemoryPreparationWorkersStandaloneSourceAndWitnessFinishFences(test *testing.T) {
	for _, mode := range []string{"logical-source", "component-witness"} {
		test.Run(mode, func(test *testing.T) {
			service, storage, connection, repo, request := standaloneMemoryPreparationFixturePG(test)
			ctx := systemTestContext()
			enableStandaloneMemoryPreparationPG(test, storage, repo)
			probe := &standaloneMemoryPreparationProbePG{comparisonMemoryProbePG: &comparisonMemoryProbePG{PostgresStore: storage}}
			workers := newStandaloneMemoryPreparationPG(test, probe, storage, []domain.ContentHash{repo})
			captured, err := workers.CaptureEffectiveMemorySource(ctx, repo, request, nil)
			if err != nil {
				test.Fatal(err)
			}
			frozen, err := captured.Project(ctx, request)
			if err != nil {
				test.Fatal(err)
			}
			if err := workers.ValidateCapturedMemorySource(ctx, repo, captured); err != nil {
				test.Fatal(err)
			}
			requirement, err := service.ObserveMemoryReadRequirement(ctx, repo, request)
			if err != nil {
				test.Fatal(err)
			}
			if _, err := service.RequestMemoryReadPreparation(ctx, repo, request); err != nil {
				test.Fatal(err)
			}
			probe.beforeFinish = func(bound context.Context, _ domain.MemoryReadWork, qualification domain.MemoryReadQualification) error {
				if mode == "logical-source" {
					_, err := connection.Exec(bound, `UPDATE snapshots SET message=message || ' standalone late write' WHERE repo_id=$1`, repo)
					return err
				}
				if len(qualification.Witnesses) != 1 || len(qualification.Witnesses[0].Components) == 0 {
					return domain.ErrIntegrity
				}
				_, err := connection.Exec(bound, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, qualification.Witnesses[0].Components[0].Hash)
				return err
			}
			err = workers.ProcessMemoryReadPreparations(ctx, 1)
			if mode == "logical-source" {
				if err != nil {
					test.Fatal("later source starved fixed-floor completion", err)
				}
				page, err := service.QueryMinimumMemory(ctx, repo, request, requirement, "")
				if err != nil || !page.Status.Complete || page.Status.Freshness != "catching_up" || page.Status.Applied != captured.Generation() {
					test.Fatal("standalone bounded result claimed exact freshness", page.Status, err)
				}
				actual, want := page.Page, frozen
				actual.NextCursor, want.NextCursor = "", ""
				if !reflect.DeepEqual(actual, want) {
					test.Fatal("standalone bounded result changed captured facts")
				}
			} else {
				if !errors.Is(err, domain.ErrConflict) {
					test.Fatal("standalone finish reused a stale component witness", err)
				}
				generation, err := storage.ReadRepositoryMemorySourceGeneration(ctx, repo)
				if err != nil || generation != captured.Generation() {
					test.Fatal("physical write changed the logical source fixture", err)
				}
				status, err := service.ReadMemoryPreparationStatus(ctx, repo, request)
				if err != nil || status.State != "retrying" || status.Reason != "source_changed" || status.QualificationID != "" {
					test.Fatal("standalone witness failure published a pointer", status, err)
				}
				var qualifications int
				if err := connection.QueryRow(ctx, `SELECT count(*) FROM memory_read_qualifications WHERE repo_id=$1`, repo).Scan(&qualifications); err != nil || qualifications != 0 {
					test.Fatal("standalone witness failure leaked qualification", qualifications, err)
				}
			}
			if len(probe.receipts) != 1 || !probe.receipts[0].Matched() {
				test.Fatal("standalone finish fence was not reached after a matching comparison")
			}
			assertStandaloneMemoryComparisonPG(test, storage, connection, probe.receipts[0])
			if err := workers.ValidateCapturedMemorySource(ctx, repo, captured); !errors.Is(err, domain.ErrConflict) {
				test.Fatal("standalone source validation reused stale proof", err)
			}
			if historical, err := captured.Project(ctx, request); err != nil || !reflect.DeepEqual(historical, frozen) {
				test.Fatal("current invalidation damaged frozen replay", err)
			}
			if _, err := service.QueryCurrentMemory(ctx, repo, request); !errors.Is(err, domain.ErrMemoryReadNotReady) {
				test.Fatal("standalone late change advertised exact-current output", err)
			}
			if _, err := service.RequestMemoryReadPreparation(ctx, repo, request); err != nil {
				test.Fatal(err)
			}
			if mode == "component-witness" {
				target, err := memoryReadTarget(repo, request)
				if err != nil {
					test.Fatal(err)
				}
				identifier, err := domain.MemoryReadTargetID(ctx, target)
				if err != nil {
					test.Fatal(err)
				}
				job, err := storage.ReadMemoryReadWork(ctx, repo, identifier)
				if err != nil || job.State != "retrying" || job.Reason != "source_changed" || job.QualificationID != "" {
					test.Fatal("standalone witness retry lost its original backoff state", job, err)
				}
				tag, err := connection.Exec(ctx, `UPDATE memory_read_work SET next_attempt=clock_timestamp()-interval '1 second'
 WHERE repo_id=$1 AND target_id=$2 AND version=$3 AND state='retrying' AND reason='source_changed'`, repo, job.ID, job.Version)
				if err != nil || tag.RowsAffected() != 1 {
					test.Fatal("could not advance the witness retry fixture to its scheduled attempt", err)
				}
			}
			if err := workers.ProcessMemoryReadPreparations(ctx, 1); err != nil {
				test.Fatal("standalone fresh-source retry", err)
			}
			page, err := service.QueryMinimumMemory(ctx, repo, request, requirement, "")
			if err != nil || page.Status.Freshness != "current" {
				test.Fatal("standalone retry did not catch up", page.Status, err)
			}
			assertCurrentMemoryParityPG(test, service, repo, request)
		})
	}
}

func TestPGMemoryPreparationWorkersStandaloneConcurrentStaleClaimFence(test *testing.T) {
	service, storage, connection, repo, request := standaloneMemoryPreparationFixturePG(test)
	ctx, cancel := context.WithTimeout(systemTestContext(), 30*time.Second)
	defer cancel()
	enableStandaloneMemoryPreparationPG(test, storage, repo)
	if _, err := service.RequestMemoryReadPreparation(ctx, repo, request); err != nil {
		test.Fatal(err)
	}
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	defer peer.Close()
	reached := make(chan domain.MemoryReadWork, 1)
	release := make(chan struct{})
	firstProbe := &standaloneMemoryPreparationProbePG{comparisonMemoryProbePG: &comparisonMemoryProbePG{PostgresStore: storage}}
	firstProbe.beforeFinish = func(bound context.Context, job domain.MemoryReadWork, _ domain.MemoryReadQualification) error {
		select {
		case reached <- job:
		case <-bound.Done():
			return bound.Err()
		}
		select {
		case <-release:
			return nil
		case <-bound.Done():
			return bound.Err()
		}
	}
	first := newStandaloneMemoryPreparationPG(test, firstProbe, storage, []domain.ContentHash{repo})
	secondProbe := &standaloneMemoryPreparationProbePG{comparisonMemoryProbePG: &comparisonMemoryProbePG{PostgresStore: peer}}
	second := newStandaloneMemoryPreparationPG(test, secondProbe, peer, []domain.ContentHash{repo})
	done := make(chan error, 1)
	exited := make(chan struct{})
	defer func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			test.Error("standalone blocked worker did not release after cancellation")
		}
	}()
	go func() {
		defer close(exited)
		done <- first.ProcessMemoryReadPreparations(ctx, 1)
	}()
	var stale domain.MemoryReadWork
	select {
	case stale = <-reached:
	case err := <-done:
		test.Fatal("standalone first worker failed before the finish fence", err)
	case <-ctx.Done():
		test.Fatal("standalone first worker did not reach the finish fence", ctx.Err())
	}
	if err := second.ProcessMemoryReadPreparations(ctx, 1); !errors.Is(err, domain.ErrResourceOverloaded) || secondProbe.finishes != 0 || len(secondProbe.receipts) != 0 {
		test.Fatal("standalone peer bypassed the shared admission limit", err)
	}
	if _, err := peer.ClaimMemoryReadWork(ctx, []domain.ContentHash{repo}, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("independent SQL claimant stole a live lease", err)
	}
	tag, err := connection.Exec(ctx, `UPDATE memory_read_work SET lease_until=clock_timestamp()-interval '1 second',next_attempt=clock_timestamp()-interval '1 second'
 WHERE repo_id=$1 AND target_id=$2 AND version=$3 AND state='running'`, repo, stale.ID, stale.Version)
	if err != nil || tag.RowsAffected() != 1 {
		test.Fatal("could not expire the held standalone claim", err)
	}
	replacement, err := peer.ClaimMemoryReadWork(ctx, []domain.ContentHash{repo}, time.Minute)
	if err != nil || replacement.State != "running" || replacement.Version <= stale.Version {
		test.Fatal("independent SQL claimant could not replace the expired lease", replacement, err)
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, domain.ErrConflict) {
			test.Fatal("stale standalone worker crossed the replacement fence", err)
		}
	case <-ctx.Done():
		test.Fatal("stale standalone worker did not return", ctx.Err())
	}
	after, err := storage.ReadMemoryReadWork(ctx, repo, stale.ID)
	if err != nil || after != replacement {
		test.Fatal("stale standalone finish/failure overwrote the replacement claim", after, err)
	}
	tag, err = connection.Exec(ctx, `UPDATE memory_read_work SET lease_until=clock_timestamp()-interval '1 second',next_attempt=clock_timestamp()-interval '1 second'
 WHERE repo_id=$1 AND target_id=$2 AND version=$3 AND state='running'`, repo, replacement.ID, replacement.Version)
	if err != nil || tag.RowsAffected() != 1 {
		test.Fatal("could not expire the independent claimant before recovery", err)
	}
	if err := second.ProcessMemoryReadPreparations(ctx, 1); err != nil {
		test.Fatal("standalone peer could not recover after the first owner released admission", err)
	}
	completed, err := storage.ReadMemoryReadWork(ctx, repo, stale.ID)
	if err != nil || completed.State != "completed" || completed.Version <= replacement.Version || completed.QualificationID == "" || secondProbe.finishes != 1 || len(secondProbe.receipts) != 1 {
		test.Fatal("standalone peer did not publish with a new claim fence", completed, err)
	}
	var qualifications int
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM memory_read_qualifications WHERE repo_id=$1`, repo).Scan(&qualifications); err != nil || qualifications != 1 {
		test.Fatal("stale standalone worker leaked a qualification", qualifications, err)
	}
	assertStandaloneMemoryComparisonPG(test, peer, connection, secondProbe.receipts[0])
	assertCurrentMemoryParityPG(test, service, repo, request)
}
