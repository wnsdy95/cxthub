package app

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/memory"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

// Seal B while A is still queued, with the same original selection. All
// transcripts and native memories are synthetic and deleted before replay.
func frozenContinuationFixture(t *testing.T) (stagingFixture, domain.CaptureAttempt, domain.CaptureAttempt) {
	t.Helper()
	f, a := frozenFixture(t)
	f.svc.save.WithFrozenMemory(nil, memory.NewRuleDistiller())
	a.Outcomes[0].Input.NativeMemory = &domain.NativeMemory{Provider: domain.ProviderClaude, Text: "ANCESTOR ONLY MEMORY"}
	b := a
	b.Proof.ID, b.Proof.GitAfter = strings.Repeat("5", 32), strings.Repeat("b", 40)
	position := *a.FrozenPosition
	position.GitCommit = b.Proof.GitAfter
	b.FrozenPosition = &position
	b.Predecessor = &domain.CapturePredecessor{AttemptID: a.Proof.ID, GitCommit: a.Proof.GitAfter}
	source := f.source(t, "successor", "successor conversation")
	input, err := f.svc.save.FreezeInput(context.Background(), f.root, source.Provider, source.Path)
	if err != nil {
		t.Fatal(err)
	}
	input.NativeMemory = &domain.NativeMemory{Provider: source.Provider, Text: "SUCCESSOR ONLY MEMORY"}
	b.Outcomes = []domain.CaptureOutcome{{Provider: source.Provider, State: "pending", Input: &input}}
	if err := os.Remove(source.Path); err != nil {
		t.Fatal(err)
	}
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	return f, a, b
}

func finishFrozenContinuation(t *testing.T, f stagingFixture, p domain.CaptureAttempt) domain.CaptureAttempt {
	t.Helper()
	ctx := context.Background()
	p = prepareFrozenMemoryReceipt(t, f, p)
	out, err := f.svc.save.SaveFrozen(ctx, f.root, p, 0)
	if err != nil {
		t.Fatal(err)
	}
	p.Outcomes[0].State, p.Outcomes[0].Target = "saved", out.SnapshotID
	p.Outcomes[0].MemoryHash, p.Outcomes[0].MemorySource = out.MemoryHash, out.MemorySource
	p.Proof.Source, p.Proof.Target = out.SnapshotID, out.SnapshotID
	p.FinalMemory, err = f.svc.save.PrepareFrozenMemory(ctx, f.root, p, -1)
	if err != nil || p.FinalMemory == nil {
		t.Fatal("prepare final memory", err)
	}
	observation, err := f.svc.save.FinishFrozenMemory(ctx, f.root, p)
	if err != nil {
		t.Fatal(err)
	}
	p.Observation, p.Complete, p.MemoryFinalized = &observation, true, true
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFrozenContinuationRetainsPredecessorAndExactPinnedMemory(t *testing.T) {
	for _, liveFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "live-dedup"}[liveFirst], func(t *testing.T) {
			ctx := context.Background()
			f, a, b := frozenContinuationFixture(t)
			a = finishFrozenContinuation(t, f, a)
			b.PredecessorObservation = a.Observation
			before := readFrozenMemorySelection(t, f)
			// A's later mutable attachment must never replace its pinned result.
			later, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: a.Proof.Target,
				PreviousMemoryHash: a.FinalMemory.Memory, Summary: "LATER UNRELATED MEMORY"})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.CompareAndSwapSnapshotMemory(ctx, a.Proof.Target, a.FinalMemory.Memory, later); err != nil {
				t.Fatal(err)
			}
			b = prepareFrozenMemoryReceipt(t, f, b)
			initial, err := f.store.GetMemory(ctx, b.Outcomes[0].MemoryPlan.ExpectedMemory)
			if err != nil || initial.Summary != "ORIGINAL MEMORY" || initial.PreviousMemoryHash != "" {
				t.Fatal("predecessor changed the live-compatible initial attachment", err)
			}
			if liveFirst {
				source := f.source(t, "successor", "successor conversation")
				live, err := f.svc.save.Save(ctx, inbound.SaveInput{Cwd: f.root, Provider: source.Provider,
					SessionPath: source.Path, Pending: true, DocIdentity: b.Outcomes[0].Input.DocIdentity})
				if err != nil || live.SnapshotID != b.Outcomes[0].MemoryPlan.Snapshot {
					t.Fatal("live first creation", err)
				}
				if err := os.Remove(source.Path); err != nil {
					t.Fatal(err)
				}
			}
			pendingBefore, err := f.store.ListPendings(ctx, f.git.repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			out, err := f.svc.save.SaveFrozen(ctx, f.root, b, 0)
			if err != nil {
				t.Fatal(err)
			}
			snap, err := f.store.GetSnapshot(ctx, out.SnapshotID)
			if err != nil {
				t.Fatal(err)
			}
			if !liveFirst && !reflect.DeepEqual(snap.Parents, []domain.ContentHash{a.Proof.Target}) {
				t.Fatalf("linear continuation became a false merge: parents=%v", snap.Parents)
			}
			if liveFirst && !reflect.DeepEqual(snap.Parents, []domain.ContentHash{b.Initial}) {
				t.Fatal("dedup rewrote immutable natural parents", snap.Parents)
			}
			if contains, err := f.svc.save.frozenReachable(ctx, f.git.repo.ID, out.SnapshotID, a.Proof.Target); err != nil || !contains {
				t.Fatal("successor cut predecessor ancestry", err)
			}
			digest, err := f.store.GetMemory(ctx, out.MemoryHash)
			if err != nil {
				t.Fatal(err)
			}
			for _, text := range []string{"ORIGINAL MEMORY", "ANCESTOR ONLY MEMORY", "SUCCESSOR ONLY MEMORY"} {
				if !strings.Contains(digest.Summary, text) {
					t.Fatal("successor lost frozen contribution", text)
				}
			}
			if strings.Contains(digest.Summary, "LATER UNRELATED MEMORY") {
				t.Fatal("rediscovered predecessor memory attachment")
			}
			if again, err := f.svc.save.SaveFrozen(ctx, f.root, b, 0); err != nil || !reflect.DeepEqual(out, again) {
				t.Fatal("retry changed frozen result", err)
			}
			pendingAfter, err := f.store.ListPendings(ctx, f.git.repo.ID)
			if err != nil || !reflect.DeepEqual(pendingBefore, pendingAfter) {
				t.Fatal("historical continuation changed pending state", err)
			}
			requireFrozenMemorySelection(t, f, before)
		})
	}
}

func TestFrozenContinuationAbsentProvidersKeepPredecessorMemory(t *testing.T) {
	ctx := context.Background()
	f, a, b := frozenContinuationFixture(t)
	a = finishFrozenContinuation(t, f, a)
	b.PredecessorObservation = a.Observation
	b.Outcomes = []domain.CaptureOutcome{{Provider: domain.ProviderClaude, State: "absent"}, {Provider: domain.ProviderCodex, State: "absent"}}
	b.Proof.Source, b.Proof.Target = a.Proof.Target, a.Proof.Target
	before := readFrozenMemorySelection(t, f)
	continuation := b.Proof
	continuation.ID = domain.CaptureContinuationObservationID(b.Proof.ID)
	continuation.MemoryHash, continuation.MemorySource = a.Observation.MemoryHash, a.Observation.MemorySource
	if err := f.svc.save.RecordFrozenContinuation(ctx, f.root, b); err != nil {
		t.Fatal(err)
	}
	later, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: a.Proof.Target,
		PreviousMemoryHash: a.FinalMemory.Memory, Summary: "LATER ATTACHMENT MUST SURVIVE"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.CompareAndSwapSnapshotMemory(ctx, a.Proof.Target, a.FinalMemory.Memory, later); err != nil {
		t.Fatal(err)
	}
	filesBefore := frozenContinuationFiles(t, f.root)
	plan, err := f.svc.save.PrepareFrozenMemory(ctx, f.root, b, -1)
	if err != nil || plan != nil {
		t.Fatalf("empty successor planned an unnecessary attachment rewrite: plan=%+v err=%v", plan, err)
	}
	b.FinalMemory = plan
	if _, err := f.svc.save.FinishFrozenMemory(ctx, f.root, b); err != nil {
		t.Fatal(err)
	}
	events, err := f.store.ListHistoryEvents(ctx, f.git.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		found = found || reflect.DeepEqual(event, continuation)
	}
	if !found || continuation.Target != a.Proof.Target || continuation.MemoryHash != a.Observation.MemoryHash || continuation.GitAfter != b.Proof.GitAfter {
		t.Fatal("empty continuation lost its pinned result")
	}
	snap, err := f.store.GetSnapshot(ctx, a.Proof.Target)
	if err != nil || snap.MemoryHash != later {
		t.Fatal("empty successor changed predecessor attachment", err)
	}
	if after := frozenContinuationFiles(t, f.root); !reflect.DeepEqual(filesBefore, after) {
		t.Fatal("empty successor rewrote durable state")
	}
	requireFrozenMemorySelection(t, f, before)
}

func TestFrozenContinuationRejectsUnverifiedPredecessorBeforeEffects(t *testing.T) {
	for _, invalid := range []string{"unresolved", "missing-record", "changed-record", "corrupt-memory", "unrelated-ancestry"} {
		t.Run(invalid, func(t *testing.T) {
			ctx := context.Background()
			f, a, b := frozenContinuationFixture(t)
			if invalid != "unresolved" {
				a = finishFrozenContinuation(t, f, a)
				event := *a.Observation
				b.PredecessorObservation = &event
				switch invalid {
				case "missing-record":
					event.ID = domain.CaptureProviderObservationID(a.Proof.ID, 1)
				case "changed-record":
					event.CreatedAt = event.CreatedAt.Add(1)
				case "corrupt-memory":
					path := filepath.Join(f.root, ".cxt", "objects", "memories", strings.TrimPrefix(string(event.MemoryHash), "sha256:"))
					if err := os.WriteFile(path, []byte("corrupt synthetic memory"), 0600); err != nil {
						t.Fatal(err)
					}
				case "unrelated-ancestry":
					// The original frozen baseline is independently valid but is
					// not an ancestor of A. Never manufacture a merge to repair it.
					other := finishFrozenContinuation(t, f, func() domain.CaptureAttempt {
						copy := b
						copy.Predecessor, copy.PredecessorObservation = nil, nil
						return copy
					}())
					b.Initial, b.FrozenPosition.Snapshot = other.Proof.Target, other.Proof.Target
				}
			}
			if err := b.Validate(); err != nil {
				t.Fatal("fixture must pass structural validation", err)
			}
			before := frozenContinuationFiles(t, f.root)
			if err := f.svc.save.RecordFrozenContinuation(ctx, f.root, b); err == nil {
				t.Fatal("recorded continuation without verified predecessor")
			}
			if _, err := f.svc.save.PrepareFrozenMemory(ctx, f.root, b, 0); err == nil {
				t.Fatal("prepared memory without verified predecessor")
			}
			if _, err := f.svc.save.SaveFrozen(ctx, f.root, b, 0); err == nil {
				t.Fatal("saved without verified predecessor")
			}
			if _, err := f.svc.save.FinishFrozenMemory(ctx, f.root, b); err == nil {
				t.Fatal("finished without verified predecessor")
			}
			if err := f.svc.save.ApplyFrozen(ctx, f.root, b); err == nil {
				t.Fatal("applied without verified predecessor")
			}
			if after := frozenContinuationFiles(t, f.root); !reflect.DeepEqual(before, after) {
				t.Fatal("unverified predecessor caused durable effects")
			}
		})
	}
}

func frozenRootContinuationFixture(t *testing.T, initial string) (stagingFixture, domain.CaptureAttempt, domain.ContentHash) {
	t.Helper()
	ctx := context.Background()
	f, p := frozenFixture(t)
	owner, err := f.store.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SessionOriginID: "inherited owner"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutSnapshot(ctx, domain.Snapshot{ID: owner, DocHash: owner, RepoID: f.git.repo.ID}); err != nil {
		t.Fatal(err)
	}
	put := func(d domain.MemoryDigest) domain.ContentHash {
		t.Helper()
		hash, err := f.store.PutMemory(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}
	root := put(domain.MemoryDigest{SnapshotID: p.Initial, Summary: "FIRST OWNED ROOT"})
	final := put(domain.MemoryDigest{SnapshotID: p.Initial, PreviousMemoryHash: root, Summary: "PINNED A FINAL"})
	later := put(domain.MemoryDigest{SnapshotID: p.Initial, PreviousMemoryHash: final, Summary: "LATER ATTACHMENT"})
	snap, err := f.store.GetSnapshot(ctx, p.Initial)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.CompareAndSwapSnapshotMemory(ctx, p.Initial, snap.MemoryHash, later); err != nil {
		t.Fatal(err)
	}
	p.Proof.GitAfter = strings.Repeat("b", 40)
	p.Proof.MemoryHash, p.Proof.MemorySource = "", ""
	if initial == "inherited" {
		p.Proof.MemoryHash = put(domain.MemoryDigest{SnapshotID: owner, Summary: "INHERITED BASELINE"})
		p.Proof.MemorySource = owner
	} else if initial == "self-owned" {
		p.Proof.MemoryHash, p.Proof.MemorySource = root, p.Initial
	}
	p.Outcomes = []domain.CaptureOutcome{{Provider: domain.ProviderClaude, State: "absent"}, {Provider: domain.ProviderCodex, State: "absent"}}
	p.FrozenPosition.GitCommit = p.Proof.GitAfter
	p.FrozenPosition.MemoryHash, p.FrozenPosition.MemorySource = p.Proof.MemoryHash, p.Proof.MemorySource
	baseline := p.Proof
	baseline.ID = domain.CaptureBaselineObservationID(p.Proof.ID)
	baseline.Source, baseline.Target = p.Initial, p.Initial
	p.FrozenPosition.Selection = &baseline
	if err := f.store.PutWorkingPosition(ctx, *p.FrozenPosition); err != nil {
		t.Fatal(err)
	}
	position, err := f.store.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p.FrozenPosition = &position
	if err := f.store.PutHistoryEvent(ctx, baseline); err != nil {
		t.Fatal(err)
	}
	p.Predecessor = &domain.CapturePredecessor{AttemptID: strings.Repeat("6", 32), GitCommit: strings.Repeat("a", 40)}
	prior := baseline
	prior.ID, prior.GitAfter = domain.CaptureFinalObservationID(p.Predecessor.AttemptID), p.Predecessor.GitCommit
	prior.MemoryHash, prior.MemorySource = final, p.Initial
	if err := f.store.PutHistoryEvent(ctx, prior); err != nil {
		t.Fatal(err)
	}
	p.PredecessorObservation = &prior
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	return f, p, root
}

func TestRecordFrozenContinuationRootWitnessPreservesObjectsAndPRSelection(t *testing.T) {
	for _, initial := range []string{"empty", "inherited", "self-owned"} {
		t.Run(initial, func(t *testing.T) {
			ctx := context.Background()
			f, p, root := frozenRootContinuationFixture(t, initial)
			before := frozenContinuationFiles(t, f.root)
			selection := readFrozenMemorySelection(t, f)
			if err := f.svc.save.RecordFrozenContinuation(ctx, f.root, p); err != nil {
				t.Fatal(err)
			}
			events, err := f.store.ListHistoryEvents(ctx, f.git.repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			continuationID := domain.CaptureContinuationObservationID(p.Proof.ID)
			var continuation, witness domain.HistoryEvent
			for _, event := range events {
				if event.ID == continuationID {
					continuation = event
				}
				if event.ID == domain.CaptureBaselineObservationID(continuationID) {
					witness = event
				}
			}
			if continuation.MemoryHash != p.PredecessorObservation.MemoryHash || continuation.Target != p.Initial || continuation.GitAfter != p.Proof.GitAfter {
				t.Fatal("continuation did not retain A's exact finalized pin", continuation)
			}
			if initial == "self-owned" {
				if witness.ID != "" {
					t.Fatal("self-owned baseline acquired an unnecessary root transition")
				}
			} else if witness.MemoryHash != root || witness.MemorySelectionParent != domain.CaptureBaselineObservationID(p.Proof.ID) || witness.MemorySource != p.Initial || witness.GitAfter != p.Proof.GitAfter {
				t.Fatal("missing exact same-code root witness", witness)
			}
			receipt := domain.HistoryEvent{ID: strings.Repeat("f", 32), RepoID: f.git.repo.ID,
				Branch: "destination", BranchID: "destination-id", Kind: "pr-merge", PRCompleted: true,
				SourceBranchID: p.Proof.BranchID, Source: p.Initial, Target: domain.HashContent([]byte("merge target")),
				CreatedAt: p.Proof.CreatedAt.Add(time.Hour), PR: &domain.PullRequestMerge{Number: 1,
					BaseBranch: "destination", HeadBranch: p.Proof.Branch, HeadSHA: p.Proof.GitAfter, MergeSHA: strings.Repeat("d", 40)}}
			resolved, err := NewContextHistoryService(f.store, f.store).ResolvePRSourcePosition(ctx, receipt)
			if err != nil || resolved.MemoryHash != p.PredecessorObservation.MemoryHash {
				t.Fatalf("completed continuation cannot resolve its PR memory: %+v %v", resolved, err)
			}
			after := frozenContinuationFiles(t, f.root)
			objectPrefix := filepath.Join(f.root, ".cxt", "objects") + string(os.PathSeparator)
			objects := func(files map[string]domain.ContentHash) map[string]domain.ContentHash {
				out := map[string]domain.ContentHash{}
				for path, hash := range files {
					if strings.HasPrefix(path, objectPrefix) {
						out[path] = hash
					}
				}
				return out
			}
			if !reflect.DeepEqual(objects(before), objects(after)) {
				t.Fatal("continuation changed memory objects or snapshot attachments")
			}
			requireFrozenMemorySelection(t, f, selection)
			if err := f.svc.save.RecordFrozenContinuation(ctx, f.root, p); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, frozenContinuationFiles(t, f.root)) {
				t.Fatal("continuation retry changed durable evidence")
			}
		})
	}
}

func TestRecordFrozenContinuationRejectsMissingEvidenceBeforeHistory(t *testing.T) {
	for _, missing := range []string{"root", "baseline"} {
		t.Run(missing, func(t *testing.T) {
			f, p, root := frozenRootContinuationFixture(t, "inherited")
			if missing == "baseline" {
				// A different attempt has no baseline in either immutable history
				// or a worktree's retained selection; neither can substitute for it.
				p.Proof.ID = strings.Repeat("7", 32)
			} else {
				path := filepath.Join(f.root, ".cxt", "objects", "memories", strings.TrimPrefix(string(root), "sha256:"))
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			before := frozenContinuationFiles(t, f.root)
			if err := f.svc.save.RecordFrozenContinuation(context.Background(), f.root, p); err == nil {
				t.Fatal("unverified immutable evidence was accepted")
			}
			if !reflect.DeepEqual(before, frozenContinuationFiles(t, f.root)) {
				t.Fatal("missing evidence recorded partial continuation history")
			}
		})
	}
}

type frozenRootChainStore struct {
	*storage.FileStore
	memories map[domain.ContentHash]domain.MemoryDigest
	reads    int
}

func (s *frozenRootChainStore) GetMemory(_ context.Context, hash domain.ContentHash) (domain.MemoryDigest, error) {
	s.reads++
	return s.memories[hash], nil
}

func TestFrozenMemoryRootWalkIsBounded(t *testing.T) {
	f, p := frozenFixture(t)
	store := &frozenRootChainStore{FileStore: f.store, memories: map[domain.ContentHash]domain.MemoryDigest{}}
	var tip domain.ContentHash
	for i := 0; i <= maxMemoryAttachmentDepth; i++ {
		digest := domain.MemoryDigest{SnapshotID: p.Initial, PreviousMemoryHash: tip}
		hash, err := domain.MemoryDigestHash(digest)
		if err != nil {
			t.Fatal(err)
		}
		store.memories[hash], tip = digest, hash
	}
	f.svc.save.store = store
	if _, err := f.svc.save.frozenMemoryRoot(context.Background(), p.Initial, tip); err == nil || store.reads > maxMemoryAttachmentDepth {
		t.Fatal("immutable root walk exceeded its bound or accepted an oversized chain", store.reads, err)
	}
}

func frozenContinuationFiles(t *testing.T, root string) map[string]domain.ContentHash {
	t.Helper()
	files := map[string]domain.ContentHash{}
	err := filepath.WalkDir(filepath.Join(root, ".cxt"), func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err == nil {
			files[path] = domain.HashContent(data)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
