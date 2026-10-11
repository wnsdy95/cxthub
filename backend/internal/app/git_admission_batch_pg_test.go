//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type admissionBatchPG struct {
	*store.PostgresStore
	trees       map[string]domain.GitCommitTree
	rejectMatch bool
	reads       int
	clock       time.Time
}

func (storage *admissionBatchPG) ListHistoryEvents(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	storage.reads++
	return storage.PostgresStore.ListHistoryEvents(ctx, repo)
}

func (storage *admissionBatchPG) GetGitCommitTree(ctx context.Context, repo domain.ContentHash, origin, commit string) (domain.GitCommitTree, error) {
	if tree, exists := storage.trees[commit]; exists {
		return tree, nil
	}
	return storage.PostgresStore.GetGitCommitTree(ctx, repo, origin, commit)
}

func (storage *admissionBatchPG) ClaimGitHeadScan(ctx context.Context, repo domain.ContentHash, origin string, _ time.Time, lease time.Duration) (domain.GitHeadScan, error) {
	storage.clock = storage.clock.Add(10 * time.Minute)
	return storage.PostgresStore.ClaimGitHeadScan(ctx, repo, origin, storage.clock, lease)
}

func (storage *admissionBatchPG) SaveGitAdmissionMatch(ctx context.Context, next domain.GitAdmissionMatch, expected int64, receipt domain.GitHeadReceipt) error {
	if err := storage.PostgresStore.SaveGitAdmissionMatch(ctx, next, expected, receipt); err != nil {
		return err
	}
	if storage.rejectMatch {
		return domain.ErrIntegrity
	}
	return nil
}

type admissionBatchReader struct {
	*scanReader
	duringRead func(context.Context)
}

func (reader *admissionBatchReader) ListGitHeads(ctx context.Context, origin string, page int) ([]outbound.GitHead, bool, error) {
	if reader.duringRead != nil {
		reader.duringRead(ctx)
	}
	return reader.scanReader.ListGitHeads(ctx, origin, page)
}

func admissionBatchReconcileFixture(t *testing.T, count int) (*GitScans, admissionPGFixture, *admissionBatchPG, *admissionBatchReader, []domain.HistoryEvent) {
	t.Helper()
	fixture := newAdmissionPGFixture(t)
	ctx := systemTestContext()
	if err := fixture.storage.FailGitHeadScan(ctx, fixture.claim, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	root, baseline := strings.Repeat("c", 40), strings.Repeat("b", 40)
	tree := strings.Repeat("f", 40)
	storage := &admissionBatchPG{PostgresStore: fixture.storage, clock: time.Now(), trees: map[string]domain.GitCommitTree{
		root: {Commit: root, Tree: tree, Parents: []string{}}, baseline: {Commit: baseline, Tree: tree, Parents: []string{root}},
		fixture.frozen.input.SourceHead: {Commit: fixture.frozen.input.SourceHead, Tree: tree, Parents: []string{root}},
	}}
	fixture.worker.core.meta = storage
	history, err := fixture.worker.core.ListHistory(ctx, fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	var original domain.HistoryEvent
	for _, event := range history {
		if event.ID == fixture.frozen.input.PublicationID {
			original = event
		}
	}
	publications := []domain.HistoryEvent{original}
	for index := 1; index < count; index++ {
		publication := original
		publication.ID = fmt.Sprintf("a%031x", index)
		if err := fixture.worker.core.RecordHistory(ctx, publication); err != nil {
			t.Fatal(err)
		}
		publications = append(publications, publication)
	}
	reader := &admissionBatchReader{scanReader: &scanReader{headPages: map[int][]outbound.GitHead{1: {
		{Ref: "refs/heads/feature", Commit: fixture.frozen.input.SourceHead}, {Ref: "refs/heads/main", Commit: baseline},
	}}}}
	scans, err := NewGitScans(fixture.worker.core, reader)
	if err != nil {
		t.Fatal(err)
	}
	scans.SetSourceAuthorizer(fixture.worker)
	return scans, fixture, storage, reader, publications
}

func TestPGGitAdmissionBatchReconcileAutomaticAndReplay(t *testing.T) {
	scans, fixture, storage, reader, _ := admissionBatchReconcileFixture(t, 1)
	ctx := systemTestContext()
	reader.duringRead = func(read context.Context) {
		if read.Value(afterCommitKey{}) != nil {
			t.Fatal("provider read inside repository write")
		}
	}
	before, err := storage.RepositoryRevision(ctx, fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := scans.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	admissions, err := storage.ListGitContributionAdmissions(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, "", 101)
	if err != nil || len(admissions) != 1 {
		t.Fatal("ordinary reconciliation did not admit accepted publication", admissions, err)
	}
	admitted := admissions[0]
	if admitted.PublicationID != fixture.frozen.input.PublicationID || admitted.OriginalHead != fixture.frozen.input.OriginalHead || admitted.AfterReadVersion <= fixture.claim.Version {
		t.Fatal("original provenance/read watermark lost", admitted)
	}
	match, err := storage.GetGitAdmissionMatch(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, fixture.authorization.provider, 1)
	if err != nil || match.Version != 1 || match.ThroughPublicationID != "" {
		t.Fatal("finite sweep did not complete", match, err)
	}
	after, err := storage.RepositoryRevision(ctx, fixture.repo.ID)
	if err != nil || after.Graph != before.Graph || after.Evidence != before.Evidence+1 {
		t.Fatal("admission advanced wrong revisions", before, after, err)
	}
	if err := scans.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	replayed, err := storage.ListGitContributionAdmissions(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, "", 101)
	if err != nil || !reflect.DeepEqual(replayed, admissions) {
		t.Fatal("repeated head scan refreshed admission", replayed, err)
	}
	joins, err := storage.ListGitContributions(ctx, fixture.repo.ID)
	if err != nil || len(joins) != 0 {
		t.Fatal("admission marked a completed join", joins, err)
	}
	for _, expected := range []domain.Ref{fixture.frozen.source, fixture.frozen.destination} {
		actual, err := storage.GetRef(ctx, fixture.repo.ID, expected.Kind, expected.Name)
		if err != nil || actual != expected {
			t.Fatal("admission moved context ref", actual, err)
		}
	}
}

func TestPGGitAdmissionBatchRollbackIncludesReceiptAndCursor(t *testing.T) {
	scans, fixture, storage, _, _ := admissionBatchReconcileFixture(t, 1)
	ctx := systemTestContext()
	storage.rejectMatch = true
	if err := scans.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	admissions, err := storage.ListGitContributionAdmissions(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, "", 101)
	if err != nil || len(admissions) != 0 {
		t.Fatal("admission escaped rollback", admissions, err)
	}
	receipts, err := storage.ListGitHeadReceipts(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, 0, 101)
	if err != nil || len(receipts) != 0 {
		t.Fatal("head receipt escaped rollback", receipts, err)
	}
	if _, err := storage.GetGitAdmissionMatch(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, fixture.authorization.provider, 1); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("cursor escaped rollback", err)
	}
	queued, err := storage.ListGitAssociationPublications(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, "", 101)
	if err != nil || len(queued) != 1 {
		t.Fatal("failure lost earlier durable discovery", queued, err)
	}
	storage.rejectMatch = false
	if err := scans.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	admissions, err = storage.ListGitContributionAdmissions(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, "", 101)
	if err != nil || len(admissions) != 1 {
		t.Fatal("retry did not recover", admissions, err)
	}
}

func TestPGGitAdmissionBatchLateEvidenceDoesNotChangeFrozenInput(t *testing.T) {
	for _, scenario := range []string{"ordinary-arrival", "ref-move", "binding-revoked"} {
		t.Run(scenario, func(t *testing.T) {
			scans, fixture, storage, reader, _ := admissionBatchReconcileFixture(t, 1)
			ctx := systemTestContext()
			reader.duringRead = func(read context.Context) {
				switch scenario {
				case "ordinary-arrival":
					history, err := fixture.worker.core.ListHistory(read, fixture.repo.ID)
					if err != nil {
						t.Fatal(err)
					}
					for _, event := range history {
						if event.Kind == "birth" {
							event.ID, event.Kind, event.Source, event.Creation = strings.Repeat("6", 32), "position", event.Target, nil
							if err := fixture.worker.core.RecordHistory(read, event); err != nil {
								t.Fatal(err)
							}
							break
						}
					}
				case "ref-move":
					ref := fixture.frozen.destination
					ref.Target = fixture.frozen.source.Target
					if err := storage.CompareAndSwapRef(read, fixture.repo.ID, ref, fixture.frozen.destination.Target); err != nil {
						t.Fatal(err)
					}
				case "binding-revoked":
					if err := fixture.worker.id.withIdentity(read, func(write context.Context) error {
						connections, err := storage.ListGitHubConnections(write)
						if err != nil {
							return err
						}
						connection := connections[0]
						connection.Enabled, connection.Generation = false, connection.Generation+1
						return storage.PutGitHubConnection(write, connection)
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := scans.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			admissions, err := storage.ListGitContributionAdmissions(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, "", 101)
			if err != nil || len(admissions) != 0 {
				t.Fatal("stale preread input admitted", admissions, err)
			}
			receipts, err := storage.ListGitHeadReceipts(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, 0, 101)
			want := 1
			if scenario == "binding-revoked" {
				want = 0
			}
			if err != nil || len(receipts) != want {
				t.Fatal("candidate deferral altered authorized ingestion", receipts, err)
			}
		})
	}
}

func TestPGGitAdmissionBatchFiniteSweepFindsLateLowerID(t *testing.T) {
	scans, fixture, storage, _, publications := admissionBatchReconcileFixture(t, gitAdmissionBatchLimit+2)
	ctx := systemTestContext()
	storage.reads = 0
	if err := scans.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if storage.reads != 0 {
		t.Fatal("batch reloaded full history per candidate", storage.reads)
	}
	first, err := storage.GetGitAdmissionMatch(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, fixture.authorization.provider, 1)
	if err != nil || first.AfterPublicationID == "" || first.ThroughPublicationID == "" {
		t.Fatal("batch was not bounded", first, err)
	}
	late := publications[0]
	late.ID = "00000000000000000000000000000002"
	if late.ID >= first.AfterPublicationID {
		t.Fatal("fixture must arrive behind the cursor")
	}
	if err := fixture.worker.core.RecordHistory(ctx, late); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if err := scans.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	admissions, err := storage.ListGitContributionAdmissions(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, "", 101)
	if err != nil || len(admissions) != len(publications)+1 {
		t.Fatal("late lower publication was starved", len(admissions), err)
	}
	found := false
	for _, admission := range admissions {
		if admission.PublicationID == late.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("late lower-ID publication disappeared")
	}
}

func TestPGGitAdmissionBatchProviderPagesHaveIndependentSweeps(t *testing.T) {
	scans, fixture, storage, reader, _ := admissionBatchReconcileFixture(t, gitAdmissionBatchLimit+2)
	ctx := systemTestContext()
	reader.headPages[2] = reader.headPages[1]
	reader.headPages[1] = []outbound.GitHead{{Ref: "refs/heads/another", Commit: strings.Repeat("b", 40)}}
	if err := scans.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	pageOne, err := storage.GetGitAdmissionMatch(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, fixture.authorization.provider, 1)
	if err != nil || pageOne.AfterPublicationID == "" {
		t.Fatal("nonmatching page did not make bounded progress", pageOne, err)
	}
	if err := scans.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	admissions, err := storage.ListGitContributionAdmissions(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, "", 101)
	if err != nil || len(admissions) != gitAdmissionBatchLimit {
		t.Fatal("provider pages shared publication cursor", len(admissions), err)
	}
	for _, admission := range admissions {
		if admission.PublicationID > pageOne.AfterPublicationID {
			t.Fatal("page two skipped publications visited only on page one", admission.PublicationID, pageOne.AfterPublicationID)
		}
	}
	pageTwo, err := storage.GetGitAdmissionMatch(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, fixture.authorization.provider, 2)
	if err != nil || pageTwo.Version != 1 {
		t.Fatal("page two cursor missing", pageTwo, err)
	}
	unchanged, err := storage.GetGitAdmissionMatch(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, fixture.authorization.provider, 1)
	if err != nil || unchanged != pageOne {
		t.Fatal("page two changed page one cursor", unchanged, err)
	}
}
