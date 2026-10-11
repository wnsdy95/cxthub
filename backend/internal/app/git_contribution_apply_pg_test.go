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
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type gitContributionApplyPGStore struct {
	*admissionBatchPG
	historyMu      sync.Mutex
	rejectProof    bool
	rejectRevision bool
}

func (storage *gitContributionApplyPGStore) ListHistoryEvents(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	storage.historyMu.Lock()
	defer storage.historyMu.Unlock()
	return storage.PostgresStore.ListHistoryEvents(ctx, repo)
}

func (storage *gitContributionApplyPGStore) PutGitContributionProof(ctx context.Context, proof domain.GitContributionProof) error {
	if err := storage.PostgresStore.PutGitContributionProof(ctx, proof); err != nil {
		return err
	}
	if storage.rejectProof {
		return domain.ErrIntegrity
	}
	return nil
}

func (storage *gitContributionApplyPGStore) AdvanceEvidenceRevision(ctx context.Context, repo domain.ContentHash) error {
	if err := storage.PostgresStore.AdvanceEvidenceRevision(ctx, repo); err != nil {
		return err
	}
	if storage.rejectRevision {
		return domain.ErrIntegrity
	}
	return nil
}

type gitContributionApplyPGFixture struct {
	scans       *GitScans
	basis       admissionPGFixture
	storage     *gitContributionApplyPGStore
	reader      *admissionBatchReader
	publication domain.HistoryEvent
	admission   domain.GitContributionAdmission
	head        domain.GitContextHead
}

func newGitContributionApplyPGFixture(t *testing.T, memory string) *gitContributionApplyPGFixture {
	t.Helper()
	scans, basis, original, reader, publications := admissionBatchReconcileFixture(t, 1)
	storage := &gitContributionApplyPGStore{admissionBatchPG: original}
	basis.worker.core.meta = storage
	fixture := &gitContributionApplyPGFixture{scans: scans, basis: basis, storage: storage, reader: reader, publication: publications[0]}
	ctx := systemTestContext()
	if memory != "empty" {
		publication := publications[0]
		publication.ID = strings.Repeat("a", 32)
		publication.MemoryPinned = memory == "pinned"
		if publication.MemoryPinned {
			publication.MemoryHash = fixture.memory(t, "original immutable pin")
			publication.MemorySource = publication.Target
		}
		if err := basis.worker.core.RecordHistory(ctx, publication); err != nil {
			t.Fatal(err)
		}
		fixture.publication = publication
	}
	if err := scans.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	admissions, err := storage.ListGitContributionAdmissions(ctx, basis.repo.ID, basis.repo.GitRemoteURL, "", 101)
	if err != nil {
		t.Fatal(err)
	}
	wantCount := 1
	if memory != "empty" {
		wantCount++
	}
	if len(admissions) != wantCount {
		t.Fatalf("baseline failed to admit immutable publications: %+v", admissions)
	}
	for _, admission := range admissions {
		if admission.PublicationID == fixture.publication.ID {
			fixture.admission = admission
		}
	}
	if fixture.admission.Validate() != nil {
		t.Fatal("selected original publication was not admitted")
	}
	return fixture
}

func (fixture *gitContributionApplyPGFixture) memory(t *testing.T, text string) domain.ContentHash {
	t.Helper()
	capture := fixture.publication.Target
	digest := domain.MemoryDigest{SnapshotID: capture, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{{SourceSnapshot: capture,
		Claims: []domain.MemoryClaim{{Kind: "code", Text: text, Code: &domain.MemoryCodeScope{Commit: fixture.publication.GitAfter, Paths: []string{"file"}}}},
	}}}
	hash, err := fixture.storage.PutMemory(systemTestContext(), fixture.basis.repo.ID, digest)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func (fixture *gitContributionApplyPGFixture) indexIntegration() {
	commit := strings.Repeat("d", 40)
	fixture.storage.trees[commit] = domain.GitCommitTree{Commit: commit, Tree: strings.Repeat("f", 40),
		Parents: []string{strings.Repeat("b", 40), fixture.admission.SourceHead}}
}

func (fixture *gitContributionApplyPGFixture) observeIntegration(t *testing.T, branch string, indexed bool) {
	t.Helper()
	if indexed {
		fixture.indexIntegration()
	}
	fixture.reader.headPages[1] = []outbound.GitHead{{Ref: "refs/heads/" + branch, Commit: strings.Repeat("d", 40)}}
	ctx := systemTestContext()
	if err := fixture.scans.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	heads, err := fixture.storage.ListGitContextHeads(ctx, fixture.basis.repo.ID, fixture.basis.repo.GitRemoteURL,
		fixture.admission.Destination.DestinationBranchID, fixture.admission.AfterReadVersion, 101)
	if err != nil || len(heads) != 1 {
		t.Fatalf("qualified later read was not bound to its pre-read context: %+v %v", heads, err)
	}
	fixture.head = heads[0]
	if fixture.head.Destination.DestinationBranch != branch || fixture.head.Destination.DestinationHead != strings.Repeat("d", 40) {
		t.Fatal("bound head lost provider identity", fixture.head)
	}
}

func (fixture *gitContributionApplyPGFixture) prepare(t *testing.T) (preparedGitContribution, gitAdmissionAuthorization) {
	t.Helper()
	ctx := systemTestContext()
	prepared, err := fixture.scans.prepareGitContribution(ctx, fixture.admission, fixture.head)
	if err != nil {
		t.Fatal("prepare contribution", err)
	}
	authorization, err := authorizeGitAdmission(ctx, fixture.basis.worker, fixture.basis.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	return prepared, authorization
}

type gitContributionApplyPGState struct {
	refs          []domain.Ref
	parents       map[domain.ContentHash][]domain.ContentHash
	revision      domain.RepositoryRevision
	contributions []domain.GitContribution
	memories      []domain.GitContributionMemory
	proofs        []domain.GitContributionProof
}

func (fixture *gitContributionApplyPGFixture) state(t *testing.T, ctx context.Context) gitContributionApplyPGState {
	t.Helper()
	var state gitContributionApplyPGState
	var err error
	repo := fixture.basis.repo.ID
	state.refs, err = fixture.storage.ListRefs(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := fixture.storage.ListSnapshots(ctx, repo, "")
	if err != nil {
		t.Fatal(err)
	}
	state.parents = map[domain.ContentHash][]domain.ContentHash{}
	for _, snapshot := range snapshots {
		state.parents[snapshot.ID] = snapshot.Parents
	}
	state.revision, err = fixture.storage.RepositoryRevision(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	state.contributions, err = fixture.storage.ListGitContributions(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	state.memories, err = fixture.storage.ListGitContributionMemories(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	state.proofs, err = fixture.storage.ListGitContributionProofs(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func assertGitContributionAppliedOnce(t *testing.T, before, after gitContributionApplyPGState, prepared preparedGitContribution) {
	t.Helper()
	if !reflect.DeepEqual(after.contributions, []domain.GitContribution{prepared.proof.Contribution}) || !reflect.DeepEqual(after.proofs, []domain.GitContributionProof{prepared.proof}) {
		t.Fatal("contribution and exact proof did not commit together", after.contributions, after.proofs)
	}
	if prepared.memory == nil {
		if len(after.memories) != 0 {
			t.Fatal("unpinned publication gained an invented pin", after.memories)
		}
	} else if !reflect.DeepEqual(after.memories, []domain.GitContributionMemory{*prepared.memory}) {
		t.Fatal("original publication pin did not commit with proof", after.memories)
	}
	wantRevision := before.revision
	wantRevision.Evidence++
	if after.revision != wantRevision || !reflect.DeepEqual(after.refs, before.refs) || !reflect.DeepEqual(after.parents, before.parents) {
		t.Fatal("application moved refs/parents or advanced the wrong revision", before, after)
	}
}

func TestPGGitContributionApplyAtomicOriginalPinAndReplay(t *testing.T) {
	for _, memory := range []string{"empty", "unpinned", "pinned"} {
		t.Run(memory, func(t *testing.T) {
			fixture := newGitContributionApplyPGFixture(t, memory)
			fixture.observeIntegration(t, "main", true)
			ctx := systemTestContext()
			mutable := fixture.memory(t, "new unpublished memory must not replace original")
			if err := fixture.storage.CompareAndSwapSnapshotMemory(ctx, fixture.basis.repo.ID, fixture.admission.Capture, "", mutable); err != nil {
				t.Fatal(err)
			}
			prepared, authorization := fixture.prepare(t)
			if prepared.proof.AdmissionID != fixture.admission.ID || prepared.proof.ContextHeadID != fixture.head.ID || prepared.proof.Contribution.Capture != fixture.admission.Capture || prepared.proof.Contribution.OriginalHead != fixture.admission.OriginalHead || prepared.proof.Contribution.IntegrationSHA != strings.Repeat("d", 40) || !reflect.DeepEqual(prepared.publication, fixture.publication) {
				t.Fatal("preparation relabeled original evidence", prepared)
			}
			if fixture.publication.MemoryPinned {
				if prepared.memory == nil || !prepared.memory.MemoryPinned || prepared.memory.MemoryHash != fixture.publication.MemoryHash || prepared.memory.MemorySource != fixture.publication.MemorySource || prepared.memory.PublicationID != fixture.publication.ID || prepared.memory.MemoryHash == mutable {
					t.Fatal("preparation borrowed mutable memory", prepared.memory)
				}
			} else if prepared.memory != nil {
				t.Fatal("preparation fabricated an unpinned memory")
			}
			before := fixture.state(t, ctx)
			proof, err := fixture.scans.applyGitContribution(ctx, prepared, authorization)
			if err != nil || proof != prepared.proof {
				t.Fatal("apply contribution", proof, err)
			}
			after := fixture.state(t, ctx)
			assertGitContributionAppliedOnce(t, before, after, prepared)
			replayed, err := fixture.scans.applyGitContribution(ctx, prepared, authorization)
			if err != nil || replayed != proof || !reflect.DeepEqual(fixture.state(t, ctx), after) {
				t.Fatal("lost-reply replay repeated an effect", replayed, err)
			}
		})
	}
}

func TestPGGitContributionApplyLateIndexUsesBoundReadAfterReset(t *testing.T) {
	fixture := newGitContributionApplyPGFixture(t, "pinned")
	fixture.observeIntegration(t, "main", false)
	ctx := systemTestContext()
	if _, err := fixture.scans.prepareGitContribution(ctx, fixture.admission, fixture.head); !errors.Is(err, errGitContributionEvidencePending) {
		t.Fatal("missing indexed code became an integration", err)
	}
	bound := fixture.head
	reset := fixture.basis.frozen.destination
	reset.Target = collaborationSnapshot(t, fixture.storage.PostgresStore, fixture.basis.repo.ID, "independent reset target")
	if _, err := fixture.basis.worker.core.UpdateRef(ctx, inbound.UpdateRefInput{RepoID: fixture.basis.repo.ID, Ref: reset, Force: true}); err != nil {
		t.Fatal(err)
	}
	fixture.indexIntegration()
	prepared, authorization := fixture.prepare(t)
	if prepared.head != bound || prepared.head.ContextTarget == reset.Target {
		t.Fatal("late indexing rebound the old read to today's reset")
	}
	before := fixture.state(t, ctx)
	if _, err := fixture.scans.applyGitContribution(ctx, prepared, authorization); err != nil {
		t.Fatal(err)
	}
	assertGitContributionAppliedOnce(t, before, fixture.state(t, ctx), prepared)
	current, err := fixture.storage.GetRef(ctx, fixture.basis.repo.ID, domain.RefBranch, "main")
	if err != nil || current.Target != reset.Target {
		t.Fatal("historical application undid reset", current, err)
	}
}

func TestPGGitContributionApplySourceArchivePreservesAdmission(t *testing.T) {
	fixture := newGitContributionApplyPGFixture(t, "empty")
	fixture.observeIntegration(t, "main", true)
	ctx := systemTestContext()
	archive := domain.HistoryEvent{ID: strings.Repeat("6", 32), RepoID: string(fixture.basis.repo.ID), Kind: "archive",
		Branch: fixture.admission.SourceBranch, BranchID: fixture.admission.SourceBranchID, BindingParent: fixture.admission.SourceAnchorID,
		Source: fixture.admission.Capture, Target: fixture.admission.Capture, CreatedAt: time.Now().UTC()}
	if err := fixture.basis.worker.core.RecordHistory(ctx, archive); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.basis.worker.core.readGitAdmissionSnapshot(ctx, fixture.basis.repo.ID)
	if err != nil || !snapshot.bindings.ByID[fixture.admission.SourceBranchID].Archived {
		t.Fatal("source archive fixture not active", err)
	}
	prepared, authorization := fixture.prepare(t)
	before := fixture.state(t, ctx)
	if _, err := fixture.scans.applyGitContribution(ctx, prepared, authorization); err != nil {
		t.Fatal("historical source archive invalidated accepted admission", err)
	}
	assertGitContributionAppliedOnce(t, before, fixture.state(t, ctx), prepared)
}

func TestPGGitContributionApplyRejectsLateAmbiguousSource(t *testing.T) {
	fixture := newGitContributionApplyPGFixture(t, "pinned")
	fixture.observeIntegration(t, "main", true)
	prepared, authorization := fixture.prepare(t)
	ctx := systemTestContext()
	ambiguous := domain.HistoryEvent{ID: strings.Repeat("6", 32), RepoID: string(fixture.basis.repo.ID), Kind: "position",
		Branch: "other-source", BranchID: "other-source-incarnation", Source: fixture.admission.Capture, Target: fixture.admission.Capture,
		GitAfter: fixture.admission.SourceHead, CreatedAt: time.Now().UTC()}
	if err := fixture.basis.worker.core.RecordHistory(ctx, ambiguous); err != nil {
		t.Fatal(err)
	}
	before := fixture.state(t, ctx)
	if _, err := fixture.scans.prepareGitContribution(ctx, fixture.admission, fixture.head); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("fresh preparation ignored new ambiguous ordinary ownership", err)
	}
	if _, err := fixture.scans.applyGitContribution(ctx, prepared, authorization); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("stale preparation bypassed ambiguity fence", err)
	}
	if !reflect.DeepEqual(fixture.state(t, ctx), before) {
		t.Fatal("ambiguity rejection leaked ledger effects")
	}
}

func TestPGGitContributionApplyFencesRevisionAndProvider(t *testing.T) {
	for _, scenario := range []string{"stale revision", "foreign provider", "revoked binding"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newGitContributionApplyPGFixture(t, "pinned")
			fixture.observeIntegration(t, "main", true)
			prepared, authorization := fixture.prepare(t)
			ctx := systemTestContext()
			want := domain.ErrConflict
			switch scenario {
			case "stale revision":
				if err := repositoryWriteError(evidenceWriteContext(ctx), fixture.basis.worker.core, fixture.basis.repo.ID, func(context.Context) error { return nil }); err != nil {
					t.Fatal(err)
				}
			case "foreign provider":
				authorization.provider.InstallationID++
			case "revoked binding":
				want = domain.ErrForbidden
				if err := fixture.basis.worker.id.withIdentity(ctx, func(write context.Context) error {
					connections, err := fixture.storage.ListGitHubConnections(write)
					if err != nil {
						return err
					}
					connection := connections[0]
					connection.Enabled, connection.Generation = false, connection.Generation+1
					return fixture.storage.PutGitHubConnection(write, connection)
				}); err != nil {
					t.Fatal(err)
				}
			}
			before := fixture.state(t, ctx)
			if _, err := fixture.scans.applyGitContribution(ctx, prepared, authorization); !errors.Is(err, want) {
				t.Fatal("application fence bypassed", err, want)
			}
			if !reflect.DeepEqual(fixture.state(t, ctx), before) {
				t.Fatal("rejected application changed ledger or revisions")
			}
		})
	}
}

func TestPGGitContributionApplyRollsBackProofPinAndRevision(t *testing.T) {
	for _, scenario := range []string{"outer rollback", "proof storage failure", "revision failure"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newGitContributionApplyPGFixture(t, "pinned")
			fixture.observeIntegration(t, "main", true)
			prepared, authorization := fixture.prepare(t)
			ctx := systemTestContext()
			before := fixture.state(t, ctx)
			var err error
			if scenario == "outer rollback" {
				err = fixture.storage.WithinRepository(ctx, fixture.basis.repo.ID, func(write context.Context) error {
					if _, err := fixture.scans.applyGitContribution(write, prepared, authorization); err != nil {
						return err
					}
					assertGitContributionAppliedOnce(t, before, fixture.state(t, write), prepared)
					return domain.ErrIntegrity
				})
			} else {
				fixture.storage.rejectProof = scenario == "proof storage failure"
				fixture.storage.rejectRevision = scenario == "revision failure"
				_, err = fixture.scans.applyGitContribution(ctx, prepared, authorization)
			}
			if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(fixture.state(t, ctx), before) {
				t.Fatal("failed application leaked contribution, pin, proof or revision", err)
			}
			fixture.storage.rejectProof, fixture.storage.rejectRevision = false, false
			if _, err := fixture.scans.applyGitContribution(ctx, prepared, authorization); err != nil {
				t.Fatal("rollback did not leave a retryable application", err)
			}
			assertGitContributionAppliedOnce(t, before, fixture.state(t, ctx), prepared)
		})
	}
}

func TestPGGitContributionApplyConcurrentSingleEffect(t *testing.T) {
	fixture := newGitContributionApplyPGFixture(t, "pinned")
	fixture.observeIntegration(t, "main", true)
	prepared, authorization := fixture.prepare(t)
	ctx, cancel := context.WithTimeout(systemTestContext(), 10*time.Second)
	defer cancel()
	before := fixture.state(t, ctx)
	type result struct {
		proof domain.GitContributionProof
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			proof, err := fixture.scans.applyGitContribution(ctx, prepared, authorization)
			results <- result{proof, err}
		}()
	}
	close(start)
	for range 2 {
		select {
		case got := <-results:
			if got.err != nil || got.proof != prepared.proof {
				t.Fatal("concurrent apply did not converge", got)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	assertGitContributionAppliedOnce(t, before, fixture.state(t, ctx), prepared)
}

func TestPGGitContributionApplyDestinationRenameBindingChain(t *testing.T) {
	fixture := newGitContributionApplyPGFixture(t, "empty")
	ctx := systemTestContext()
	previousName, parent := "main", fixture.admission.DestinationAnchorID
	for index, name := range []string{"release", "renamed-main"} {
		id := strings.Repeat("6", 32)
		if index == 1 {
			id = strings.Repeat("7", 32)
		}
		event := domain.HistoryEvent{ID: id, RepoID: string(fixture.basis.repo.ID), Kind: "rename", Branch: name, PreviousBranch: previousName,
			BranchID: fixture.admission.Destination.DestinationBranchID, BindingParent: parent,
			Source: fixture.basis.frozen.destination.Target, Target: fixture.basis.frozen.destination.Target, CreatedAt: time.Now().UTC()}
		if err := fixture.basis.worker.core.RecordHistory(ctx, event); err != nil {
			t.Fatal(err)
		}
		ref := fixture.basis.frozen.destination
		ref.Name = name
		if _, err := fixture.basis.worker.core.UpdateRef(ctx, inbound.UpdateRefInput{RepoID: fixture.basis.repo.ID, Ref: ref}); err != nil {
			t.Fatal(err)
		}
		previousName, parent = name, id
	}
	fixture.observeIntegration(t, previousName, true)
	prepared, authorization := fixture.prepare(t)
	if fixture.head.AnchorID != parent || fixture.admission.MatchesLaterRead(fixture.head.Destination, prepared.receipt) || !fixture.admission.MatchesLaterContextHead(fixture.head, prepared.receipt) {
		t.Fatal("rename did not use a causally bound same-incarnation head")
	}
	before := fixture.state(t, ctx)
	if _, err := fixture.scans.applyGitContribution(ctx, prepared, authorization); err != nil {
		t.Fatal("proven rename chain rejected", err)
	}
	assertGitContributionAppliedOnce(t, before, fixture.state(t, ctx), prepared)
}
