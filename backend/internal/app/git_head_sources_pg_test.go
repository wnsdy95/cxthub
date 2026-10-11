//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type admissionSourceReader struct {
	*admissionBatchReader
	mu           sync.Mutex
	reads        []string
	sources      map[string]outbound.GitHead
	sourceErr    error
	duringSource func(context.Context) error
}

func (reader *admissionSourceReader) ReadGitHead(ctx context.Context, origin, branch string) (outbound.GitHead, error) {
	reader.mu.Lock()
	reader.reads = append(reader.reads, branch)
	reader.mu.Unlock()
	if ctx.Value(afterCommitKey{}) != nil {
		return outbound.GitHead{}, errors.New("source HTTP inside repository write")
	}
	if reader.duringSource != nil {
		if err := reader.duringSource(ctx); err != nil {
			return outbound.GitHead{}, err
		}
	}
	if reader.sourceErr != nil {
		return outbound.GitHead{}, reader.sourceErr
	}
	head, found := reader.sources[branch]
	if !found {
		return outbound.GitHead{}, domain.ErrNotFound
	}
	return head, nil
}

func crossPageAdmissionFixture(t *testing.T, count int) (*GitScans, admissionPGFixture, *admissionBatchPG, *admissionSourceReader) {
	t.Helper()
	scans, fixture, storage, paged, _ := admissionBatchReconcileFixture(t, count)
	paged.headPages = map[int][]outbound.GitHead{
		1: {{Ref: "refs/heads/main", Commit: strings.Repeat("b", 40)}},
		2: {{Ref: "refs/heads/feature", Commit: fixture.frozen.input.SourceHead}},
	}
	reader := &admissionSourceReader{admissionBatchReader: paged, sources: map[string]outbound.GitHead{
		"feature": {Ref: "refs/heads/feature", Commit: fixture.frozen.input.SourceHead},
	}}
	paged.duringRead = func(context.Context) {
		reader.mu.Lock()
		defer reader.mu.Unlock()
		if len(reader.reads) == 0 {
			t.Error("destination page was read before its source")
		}
	}
	scans.reader = reader
	scans.core.meta = &contributionWorkerPGStore{gitContributionApplyPGStore: &gitContributionApplyPGStore{admissionBatchPG: storage}, clock: time.Now().UTC()}
	return scans, fixture, storage, reader
}

func TestPGGitHeadSourcesCrossPageAutomaticAdmissionAndIntegration(t *testing.T) {
	scans, fixture, storage, reader := crossPageAdmissionFixture(t, 1)
	ctx := systemTestContext()
	if err := scans.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	admissions, err := storage.ListGitContributionAdmissions(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, "", 101)
	if err != nil || len(admissions) != 1 {
		t.Fatal("separate-page source did not admit", admissions, err)
	}
	admission := admissions[0]
	receipt, err := storage.GetGitHeadReceipt(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, admission.Destination.ReadVersion)
	if err != nil || !admission.MatchesBaseline(receipt) || len(receipt.Heads) != 1 || len(receipt.PriorSourceHeads) != 1 || receipt.PriorSourceHeads[0].Ref != "refs/heads/feature" || receipt.Heads[0].Ref != "refs/heads/main" {
		t.Fatal("ordered reads lost provenance", receipt, err)
	}
	sourceHeads, err := storage.ListGitContextHeads(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, admission.SourceBranchID, 0, 101)
	if err != nil || len(sourceHeads) != 0 {
		t.Fatal("source-only pre-read created a destination binding", sourceHeads, err)
	}
	if err := scans.ProcessContributions(ctx, 2); err != nil {
		t.Fatal(err)
	}
	proofs, err := storage.ListGitContributionProofs(ctx, fixture.repo.ID)
	if err != nil || len(proofs) != 0 {
		t.Fatal("admission became an early completed merge", proofs, err)
	}
	if err := scans.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	merged := strings.Repeat("d", 40)
	storage.trees[merged] = domain.GitCommitTree{Commit: merged, Tree: strings.Repeat("f", 40), Parents: []string{admission.Destination.DestinationHead, admission.SourceHead}}
	reader.headPages[1] = []outbound.GitHead{{Ref: "refs/heads/main", Commit: merged}}
	if err := scans.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := scans.ProcessContributions(ctx, 2); err != nil {
			t.Fatal(err)
		}
	}
	proofs, err = storage.ListGitContributionProofs(ctx, fixture.repo.ID)
	if err != nil || len(proofs) != 1 || proofs[0].Contribution.IntegrationSHA != merged || proofs[0].AdmissionID != admission.ID {
		t.Fatal("ordinary worker did not integrate exact cross-page source", proofs, err)
	}
	pins, err := storage.ListGitContributionMemories(ctx, fixture.repo.ID)
	if err != nil || len(pins) != 1 || pins[0].PublicationID != admission.PublicationID || !pins[0].MemoryPinned || pins[0].MemoryHash != "" {
		t.Fatal("original explicit empty pin was replaced", pins, err)
	}
	for _, expected := range []domain.Ref{fixture.frozen.source, fixture.frozen.destination} {
		actual, err := storage.GetRef(ctx, fixture.repo.ID, expected.Kind, expected.Name)
		if err != nil || actual != expected {
			t.Fatal("automatic cross-page application moved current context", actual, err)
		}
	}
}

func TestPGGitHeadSourcesDeduplicateBoundedReads(t *testing.T) {
	scans, fixture, storage, reader := crossPageAdmissionFixture(t, gitAdmissionBatchLimit+2)
	if err := scans.Reconcile(systemTestContext()); err != nil {
		t.Fatal(err)
	}
	reader.mu.Lock()
	reads := append([]string{}, reader.reads...)
	reader.mu.Unlock()
	if !reflect.DeepEqual(reads, []string{"feature"}) {
		t.Fatal("equivalent publications repeated exact source reads", reads)
	}
	admissions, err := storage.ListGitContributionAdmissions(systemTestContext(), fixture.repo.ID, fixture.repo.GitRemoteURL, "", 101)
	if err != nil || len(admissions) != gitAdmissionBatchLimit {
		t.Fatal("bounded match failed", len(admissions), err)
	}
}

func TestPGGitHeadSourcesFailuresAndStaleInputsDoNotInventAdmission(t *testing.T) {
	for _, scenario := range []string{"missing-source", "wrong-source", "source-timeout", "authorization-denied", "later-page-source", "changed-context", "rollback-cursor"} {
		t.Run(scenario, func(t *testing.T) {
			scans, fixture, storage, reader := crossPageAdmissionFixture(t, 1)
			ctx := systemTestContext()
			switch scenario {
			case "missing-source":
				reader.sourceErr = domain.ErrNotFound
			case "wrong-source":
				reader.sources["feature"] = outbound.GitHead{Ref: "refs/heads/feature-other", Commit: fixture.frozen.input.SourceHead}
			case "source-timeout":
				reader.sourceErr = context.DeadlineExceeded
			case "authorization-denied":
				reader.sourceErr = domain.ErrForbidden
			case "later-page-source":
				reader.headPages[1] = append(reader.headPages[1], outbound.GitHead{Ref: "refs/heads/feature", Commit: strings.Repeat("9", 40)})
			case "changed-context":
				reader.duringSource = func(context.Context) error {
					return repositoryWriteError(ctx, scans.core, fixture.repo.ID, func(context.Context) error { return nil })
				}
			case "rollback-cursor":
				storage.rejectMatch = true
			}
			if err := scans.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			admissions, err := storage.ListGitContributionAdmissions(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, "", 101)
			if err != nil || len(admissions) != 0 {
				t.Fatal("unproven source admitted", admissions, err)
			}
			receipts, err := storage.ListGitHeadReceipts(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, 0, 101)
			want := 1
			if scenario == "rollback-cursor" || scenario == "authorization-denied" {
				want = 0
			}
			if err != nil || len(receipts) != want {
				t.Fatal("head-page transaction split", receipts, err)
			}
			if want == 1 && scenario != "changed-context" && scenario != "later-page-source" && len(receipts[0].PriorSourceHeads) != 0 {
				t.Fatal("failed source read persisted as proof")
			}
			if scenario == "authorization-denied" && len(reader.headReads) != 0 {
				t.Fatal("known authorization denial proceeded to page I/O")
			}
		})
	}
}

func TestPGGitHeadSourcesOptionalBudgetPreservesPageProgress(t *testing.T) {
	scans, fixture, storage, reader := crossPageAdmissionFixture(t, 1)
	finished := make(chan struct{})
	reader.duringSource = func(ctx context.Context) error {
		deadline, exists := ctx.Deadline()
		if !exists || time.Until(deadline) > 10*time.Second {
			return errors.New("optional source has no separate bounded budget")
		}
		<-ctx.Done()
		close(finished)
		return ctx.Err()
	}
	ctx, cancel := context.WithTimeout(systemTestContext(), 20*time.Second)
	defer cancel()
	if err := scans.Reconcile(ctx); err != nil {
		t.Fatal("optional lookup exhausted ordinary reconciliation", err)
	}
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("source request did not terminate on cancellation")
	}
	if !reflect.DeepEqual(reader.headReads, []int{1}) {
		t.Fatal("source timeout blocked ordinary page", reader.headReads)
	}
	receipts, err := storage.ListGitHeadReceipts(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, 0, 101)
	if err != nil || len(receipts) != 1 || len(receipts[0].PriorSourceHeads) != 0 {
		t.Fatal("timed-out source gained receipt authority", receipts, err)
	}
	match, err := storage.GetGitAdmissionMatch(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, fixture.authorization.provider, 1)
	if err != nil || match.Version != 1 || match.ThroughPublicationID != "" {
		t.Fatal("optional failure stalled finite matching sweep", match, err)
	}
}

func TestPGGitHeadSourcesParentCancellationDoesNotReadOrPublishPage(t *testing.T) {
	scans, fixture, storage, reader := crossPageAdmissionFixture(t, 1)
	started, finished := make(chan struct{}), make(chan struct{})
	reader.duringSource = func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(finished)
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(systemTestContext())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- scans.Reconcile(ctx) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("source lookup did not start")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal("parent cancellation was ignored", err)
	}
	<-finished
	if len(reader.headReads) != 0 {
		t.Fatal("canceled reconciliation started destination I/O")
	}
	receipts, err := storage.ListGitHeadReceipts(systemTestContext(), fixture.repo.ID, fixture.repo.GitRemoteURL, 0, 101)
	if err != nil || len(receipts) != 0 {
		t.Fatal("canceled source published proof", receipts, err)
	}
}
