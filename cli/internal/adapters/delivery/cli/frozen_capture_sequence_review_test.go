package cli

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/memory"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func frozenSequenceReviewProcess(t *testing.T, f frozenDriver) frozenDriver {
	t.Helper()
	st := storage.NewWorktreeFileStore(f.cwd, gitOut(f.cwd, "rev-parse", "--absolute-git-dir"), "main", gitOut(f.cwd, "rev-parse", "HEAD"))
	capt := capture.NewSessionCapture(st)
	save := app.NewSaveSessionService(gitctx.NewGitContextAdapter(),
		map[domain.ProviderKind]outbound.CaptureSource{domain.ProviderClaude: capture.NewClaudeCapture(), domain.ProviderCodex: capture.NewCodexCapture()},
		map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderClaude: codec.NewClaudeCodec(), domain.ProviderCodex: codec.NewCodexCodec()}, st, capt, nil)
	save.WithFrozenMemory(nil, memory.NewRuleDistiller())
	container := *f.c
	container.History = app.NewContextHistoryService(st, st)
	container.CommitCapture, container.List = save, app.NewListSessionsService(st)
	return frozenDriver{cwd: f.cwd, c: &container, store: st, repo: f.repo, save: save, capture: capt}
}

// Reproduce consecutive commits whose capture receipts are sealed before the
// first worker finishes. All Git/native input is synthetic; no hooks, provider
// processes, network clients or model-backed distillers are involved.
func TestFrozenCaptureSequenceReview(t *testing.T) {
	for _, identity := range []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1} {
		for _, pendingA := range []bool{false, true} {
			mode := "serial-control"
			if pendingA {
				mode = "pending-A-before-freeze-B"
			}
			t.Run(string(identity)+"/"+mode, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				f := newFrozenDriver(t)
				f.save.WithFrozenMemory(nil, memory.NewRuleDistiller())
				var release func()
				if pendingA {
					var err error
					release, err = claimCaptureWorker(f.cwd)
					if err != nil || release == nil {
						t.Fatal("hold capture worker during both freezes", err)
					}
					t.Cleanup(func() {
						if release != nil {
							release()
						}
					})
				}
				const nativeA = "ancestor-only frozen native decision"
				const nativeB = "successor-only frozen native decision"

				seal := func(driver frozenDriver, text, native string) *commitCapturePass {
					t.Helper()
					p := driver.attempt(t, text, identity, false)
					next := *p
					next.Outcomes = append([]domain.CaptureOutcome(nil), p.Outcomes...)
					input := *p.Outcomes[0].Input
					var err error
					input.SessionID, err = driver.capture.FrozenSessionID(ctx, driver.cwd, input)
					if err != nil || input.SessionID == "" {
						t.Fatal("verified frozen session identity", err)
					}
					input.NativeMemory = &domain.NativeMemory{Provider: input.Provider, Source: "synthetic sequence fixture", Text: native}
					next.Outcomes[0].Input = &input
					next.Outcomes[0].SessionID = input.SessionID
					next.InputsReady = true
					if err := p.replace(ctx, driver.cwd, next); err != nil {
						t.Fatal("seal sequence receipt", err)
					}
					return p
				}
				process := func(driver frozenDriver, p *commitCapturePass) {
					t.Helper()
					if err := processFrozenCapture(ctx, driver.c, driver.cwd, driver.cwd, p); err != nil {
						t.Fatal("process frozen sequence", err)
					}
					stored := driver.readAttempt(t, p)
					if !stored.Complete || !stored.MemoryFinalized || stored.FinalMemory == nil {
						t.Fatal("sequence attempt did not finalize")
					}
					*p = stored
				}

				a := seal(f, "ancestor-conversation", nativeA)
				gitA := a.Proof.GitAfter
				if !pendingA {
					process(f, a)
				}
				runLifecycleGit(t, f.cwd, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "sequence successor B")
				gitB := gitOut(f.cwd, "rev-parse", "HEAD")
				if gitA == gitB || gitOut(f.cwd, "rev-parse", "HEAD^") != gitA {
					t.Fatal("fixture must have an exact linear Git A -> B edge")
				}

				// Model the next hook/worker process, including its current-SHA
				// storage fence; reusing A's process-scoped store would hide Apply B.
				worker := frozenSequenceReviewProcess(t, f)
				st := worker.store
				b := seal(worker, "successor-conversation", nativeB)
				if b.Proof.GitAfter != gitB || b.Proof.BranchID != a.Proof.BranchID {
					t.Fatal("B receipt lost code/branch identity")
				}
				if pendingA {
					if a.Complete || b.Initial != a.Initial || b.Proof.MemoryHash != a.Proof.MemoryHash {
						t.Fatal("fixture did not freeze both receipts from the still-selected baseline")
					}
					release()
					release = nil
					process(worker, a)
					position, err := st.GetWorkingPosition(ctx)
					if err != nil || position.Snapshot == a.Proof.Target {
						t.Fatal("historical A incorrectly replaced B's active position", err)
					}
				}
				process(worker, b)

				position, err := st.GetWorkingPosition(ctx)
				if err != nil || position.Snapshot != b.Proof.Target || position.GitCommit != gitB {
					t.Fatal("B did not become the exact active Git/context position", err)
				}
				aMemory, err := st.GetMemory(ctx, a.FinalMemory.Memory)
				if err != nil || !strings.Contains(aMemory.Summary, nativeA) {
					t.Fatal("A never captured its unique frozen memory", err)
				}
				bMemory, err := st.GetMemory(ctx, b.FinalMemory.Memory)
				if err != nil || !strings.Contains(bMemory.Summary, nativeB) {
					t.Fatal("B never captured its own frozen memory", err)
				}
				seen := map[domain.ContentHash]bool{}
				queue := []domain.ContentHash{b.Proof.Target}
				for len(queue) > 0 {
					id := queue[len(queue)-1]
					queue = queue[:len(queue)-1]
					if seen[id] {
						continue
					}
					seen[id] = true
					snapshot, err := st.GetSnapshot(ctx, id)
					if err != nil {
						t.Fatal("read causal context ancestry", err)
					}
					queue = append(queue, snapshot.ReachabilityParents()...)
				}
				if !seen[a.Proof.Target] {
					t.Errorf("B context excludes preceding Git A capture: A=%s B=%s frozen B.Initial=%s", a.Proof.Target, b.Proof.Target, b.Initial)
				}
				if !strings.Contains(bMemory.Summary, nativeA) {
					t.Errorf("B memory dropped preceding A's frozen native contribution: B summary=%q", bMemory.Summary)
				}
				inheritedA := false
				for _, fragment := range bMemory.Fragments {
					inheritedA = inheritedA || fragment.SourceSnapshot == a.Proof.Target && strings.Contains(fragment.Summary, nativeA)
				}
				if !inheritedA {
					t.Error("B memory lacks A's exact source fragment despite the linear Git ancestry")
				}
			})
		}
	}
}

func TestFrozenCaptureSequenceReviewRejectsSiblingPredecessor(t *testing.T) {
	f := newFrozenDriver(t)
	base := gitOut(f.cwd, "rev-parse", "HEAD")
	runLifecycleGit(t, f.cwd, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "sibling A")
	first := frozenSequenceReviewProcess(t, f)
	a := first.attempt(t, "sibling-a", domain.DocumentIdentityRootV1, true)
	runLifecycleGit(t, f.cwd, "reset", "--hard", base)
	runLifecycleGit(t, f.cwd, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "sibling B")
	second := frozenSequenceReviewProcess(t, f)
	b := second.attempt(t, "sibling-b", domain.DocumentIdentityRootV1, true)
	selectionA, selectionB := *a.FrozenPosition, *b.FrozenPosition
	selectionA.GitCommit, selectionB.GitCommit = "", ""
	if gitOut(f.cwd, "rev-parse", "HEAD^") != base || a.Proof.GitAfter == b.Proof.GitAfter || !reflect.DeepEqual(selectionA, selectionB) {
		t.Fatal("fixture must have sibling commits with the same frozen selection")
	}
	if b.Predecessor != nil || b.PredecessorObservation != nil {
		t.Fatal("same worktree/branch/cursor adopted a sibling instead of the exact Git first parent")
	}
}

func TestFrozenCaptureSequenceReviewMemoryRepinBreaksPredecessorSelection(t *testing.T) {
	ctx := context.Background()
	f := newFrozenDriver(t)
	a := f.attempt(t, "before-memory-repin", domain.DocumentIdentityRootV1, true)
	position, err := f.store.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	old := position
	repinned, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: position.Snapshot, Provider: domain.ProviderClaude,
		PreviousMemoryHash: position.MemoryHash, Summary: "explicitly selected replacement memory"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.CompareAndSwapSnapshotMemory(ctx, position.Snapshot, position.MemoryHash, repinned); err != nil {
		t.Fatal(err)
	}
	event := a.Proof
	event.ID = strings.Repeat("d", 32)
	event.Source, event.Target = position.Snapshot, position.Snapshot
	event.MemoryHash, event.MemorySource = repinned, position.Snapshot
	event.CreatedAt = a.Proof.CreatedAt.Add(time.Minute)
	if _, err := f.c.History.ValidateHistorySource(ctx, event); err != nil {
		t.Fatal(err)
	}
	if err := f.c.History.RecordHistory(ctx, event); err != nil {
		t.Fatal(err)
	}
	position.MemoryHash, position.MemorySource, position.MemoryPinned = repinned, position.Snapshot, true
	position.Selection = &event
	if err := f.store.PutWorkingPosition(ctx, position); err != nil {
		t.Fatal(err)
	}
	if position.Snapshot != old.Snapshot || position.SharedTarget != old.SharedTarget || position.GitCommit != old.GitCommit || position.BranchID != old.BranchID {
		t.Fatal("memory repin must leave code and context target unchanged")
	}
	runLifecycleGit(t, f.cwd, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "child after explicit memory repin")
	worker := frozenSequenceReviewProcess(t, f)
	b := worker.attempt(t, "after-memory-repin", domain.DocumentIdentityRootV1, true)
	if gitOut(f.cwd, "rev-parse", "HEAD^") != a.Proof.GitAfter || b.Initial != a.Initial || b.Proof.MemoryHash != repinned {
		t.Fatal("fixture must preserve the exact Git-parent edge and select only new memory")
	}
	if b.Predecessor != nil || b.PredecessorObservation != nil {
		t.Fatal("memory-only cursor change inherited pending A across an explicit selection boundary")
	}
}

func TestFrozenCaptureSequenceReviewIncompletePredecessorBlocksEffects(t *testing.T) {
	for _, sealed := range []bool{false, true} {
		name := "unsealed"
		if sealed {
			name = "sealed"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			f := newFrozenDriver(t)
			a := f.attempt(t, "incomplete-parent", domain.DocumentIdentityRootV1, sealed)
			runLifecycleGit(t, f.cwd, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "child of incomplete capture")
			worker := frozenSequenceReviewProcess(t, f)
			b := worker.attempt(t, "blocked-child", domain.DocumentIdentityRootV1, true)
			if b.Predecessor == nil || b.Predecessor.AttemptID != a.Proof.ID || b.Predecessor.GitCommit != a.Proof.GitAfter {
				t.Fatal("exact pending predecessor was not durably selected")
			}
			if a.Complete || a.MemoryFinalized || b.PredecessorObservation != nil {
				t.Fatal("fixture must retain an unresolved predecessor")
			}
			before := domain.CaptureAttempt(*b).Fingerprint()
			beforeHistory := worker.history(t)
			beforePosition, err := worker.store.GetWorkingPosition(ctx)
			if err != nil {
				t.Fatal(err)
			}
			wrapped := &frozenDriverCapture{CommitCapture: worker.save,
				beforePrepare: func(context.Context, domain.CaptureAttempt, int) error { return context.Canceled }}
			worker.c.CommitCapture = wrapped
			if err := processFrozenCapture(ctx, worker.c, worker.cwd, worker.cwd, b); err == nil {
				t.Error("successor proceeded without finalized predecessor evidence")
			}
			if len(wrapped.prepared) != 0 || wrapped.saves != 0 {
				t.Errorf("incomplete predecessor allowed expensive/effectful application work: prepares=%v saves=%d", wrapped.prepared, wrapped.saves)
			}
			stored := worker.readAttempt(t, b)
			if domain.CaptureAttempt(stored).Fingerprint() != before || !reflect.DeepEqual(worker.history(t), beforeHistory) {
				t.Error("blocked successor changed its durable receipt or immutable history")
			}
			afterPosition, err := worker.store.GetWorkingPosition(ctx)
			if err != nil || !reflect.DeepEqual(afterPosition, beforePosition) {
				t.Fatal("blocked successor changed the selected position", err)
			}
		})
	}
}

func frozenSequenceReviewSeal(t *testing.T, f frozenDriver, identity domain.DocumentIdentity, text string, absent bool) *commitCapturePass {
	t.Helper()
	ctx := context.Background()
	if absent {
		p, err := beginCommitCapture(ctx, f.c, f.cwd, []string{domain.ProviderClaude, domain.ProviderCodex})
		if err != nil || p == nil {
			t.Fatal("begin empty sequence capture", err)
		}
		next := *p
		next.Outcomes = []domain.CaptureOutcome{{Provider: domain.ProviderClaude, State: "absent"}, {Provider: domain.ProviderCodex, State: "absent"}}
		next.InputsReady = true
		if err := p.replace(ctx, f.cwd, next); err != nil {
			t.Fatal("seal explicit provider absence", err)
		}
		baseline := p.Proof
		baseline.ID = domain.CaptureBaselineObservationID(p.Proof.ID)
		baseline.Source, baseline.Target = p.Initial, p.Initial
		if err := f.c.History.RecordHistory(ctx, baseline); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := f.attempt(t, text, identity, false)
	next := *p
	next.Outcomes = append([]domain.CaptureOutcome(nil), p.Outcomes...)
	input := *p.Outcomes[0].Input
	var err error
	input.SessionID, err = f.capture.FrozenSessionID(ctx, f.cwd, input)
	if err != nil || input.SessionID == "" {
		t.Fatal("verify frozen sequence session", err)
	}
	input.NativeMemory = &domain.NativeMemory{Provider: input.Provider, Source: "synthetic sequence fixture", Text: text + " unique frozen native memory"}
	next.Outcomes[0].Input = &input
	next.Outcomes[0].SessionID = input.SessionID
	next.InputsReady = true
	if err := p.replace(ctx, f.cwd, next); err != nil {
		t.Fatal("seal sequence memory", err)
	}
	return p
}

func TestFrozenCaptureSequenceReviewMultihopAndEmptyProviders(t *testing.T) {
	for _, identity := range []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1} {
		for _, mode := range []string{"all-inputs", "empty-middle", "empty-final", "empty-first"} {
			t.Run(string(identity)+"/"+mode, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				f := newFrozenDriver(t)
				f.save.WithFrozenMemory(nil, memory.NewRuleDistiller())
				release, err := claimCaptureWorker(f.cwd)
				if err != nil || release == nil {
					t.Fatal("hold worker while freezing all three commits", err)
				}
				t.Cleanup(func() {
					if release != nil {
						release()
					}
				})
				var passes []*commitCapturePass
				var originalPositions []domain.WorkingPosition
				var originalMemory []domain.ContentHash
				names := []string{"sequence-A", "sequence-B", "sequence-C"}
				absent := []bool{mode == "empty-first", mode == "empty-middle", mode == "empty-final"}
				worker := f
				for i, name := range names {
					if i > 0 {
						runLifecycleGit(t, f.cwd, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", name)
						worker = frozenSequenceReviewProcess(t, f)
					}
					p := frozenSequenceReviewSeal(t, worker, identity, name, absent[i])
					if i > 0 {
						previous := passes[i-1]
						if p.Initial != previous.Initial || p.Proof.MemoryHash != previous.Proof.MemoryHash || gitOut(f.cwd, "rev-parse", "HEAD^") != previous.Proof.GitAfter {
							t.Fatal("all receipts must freeze the same baseline along exact Git first-parent edges")
						}
						if i == 1 && absent[0] {
							if p.Predecessor != nil {
								t.Fatal("empty first capture supplied a spurious dependency")
							}
						} else if p.Predecessor == nil || p.Predecessor.AttemptID != previous.Proof.ID || p.Predecessor.GitCommit != previous.Proof.GitAfter {
							t.Fatal("multihop receipt lost its exact immediate predecessor")
						}
					}
					passes = append(passes, p)
					originalPositions = append(originalPositions, *p.FrozenPosition)
					originalMemory = append(originalMemory, p.Proof.MemoryHash)
				}
				release()
				release = nil
				// The chain must gate an out-of-order attempt before deriving any
				// memory or recording a continuation from an incomplete receipt.
				beforeC := domain.CaptureAttempt(*passes[2]).Fingerprint()
				if err := processFrozenCapture(ctx, worker.c, worker.cwd, worker.cwd, passes[2]); err == nil {
					t.Fatal("C ran before its pending B predecessor")
				}
				storedC := worker.readAttempt(t, passes[2])
				if domain.CaptureAttempt(storedC).Fingerprint() != beforeC {
					t.Fatal("blocked C changed its durable receipt")
				}
				for i, p := range passes {
					if err := processFrozenCapture(ctx, worker.c, worker.cwd, worker.cwd, p); err != nil {
						t.Fatalf("process %s: %v", names[i], err)
					}
					*p = worker.readAttempt(t, p)
					if !p.Complete || !p.MemoryFinalized || p.Observation == nil || !reflect.DeepEqual(*p.FrozenPosition, originalPositions[i]) || p.Proof.MemoryHash != originalMemory[i] {
						t.Fatal("completion changed original frozen evidence or lost its exact memory receipt")
					}
					if p.Predecessor != nil {
						if !reflect.DeepEqual(p.PredecessorObservation, passes[i-1].Observation) {
							t.Fatal("continuation did not pin the predecessor's exact completed observation")
						}
						if absent[i] && (p.FinalMemory != nil || p.Proof.Target != passes[i-1].Proof.Target || p.Observation.MemoryHash != passes[i-1].Observation.MemoryHash || p.Observation.MemorySource != passes[i-1].Observation.MemorySource || p.Observation.ID != domain.CaptureContinuationObservationID(p.Proof.ID)) {
							t.Fatal("empty provider continuation lost the exact predecessor target/memory")
						}
					}
					if i < 2 {
						position, err := worker.store.GetWorkingPosition(ctx)
						if err != nil || position.Snapshot != passes[2].Initial {
							t.Fatal("historical A/B moved the current C position", err)
						}
					}
				}
				c := passes[2]
				position, err := worker.store.GetWorkingPosition(ctx)
				if err != nil || position.Snapshot != c.Proof.Target || position.GitCommit != c.Proof.GitAfter || position.MemoryHash != c.Observation.MemoryHash || position.MemorySource != c.Observation.MemorySource {
					t.Fatal("latest C did not select its exact completed memory/context", err)
				}
				digest, err := worker.store.GetMemory(ctx, c.Observation.MemoryHash)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(digest.Summary, "frozen selected memory") {
					t.Fatal("multihop result lost the original frozen baseline memory")
				}
				seen := map[domain.ContentHash]bool{}
				queue := []domain.ContentHash{c.Proof.Target}
				for len(queue) > 0 {
					tip := queue[len(queue)-1]
					queue = queue[:len(queue)-1]
					if seen[tip] {
						continue
					}
					seen[tip] = true
					snapshot, err := worker.store.GetSnapshot(ctx, tip)
					if err != nil {
						t.Fatal(err)
					}
					queue = append(queue, snapshot.ReachabilityParents()...)
				}
				for i, p := range passes {
					if !seen[p.Proof.Target] {
						t.Errorf("C ancestry excludes %s target %s", names[i], p.Proof.Target)
					}
					if absent[i] {
						continue
					}
					text := names[i] + " unique frozen native memory"
					found := false
					for _, fragment := range digest.Fragments {
						found = found || fragment.SourceSnapshot == p.Proof.Target && strings.Contains(fragment.Summary, text)
					}
					if !strings.Contains(digest.Summary, text) || !found {
						t.Errorf("C lost %s's frozen contribution or exact source fragment", names[i])
					}
				}
				beforeHistory := worker.history(t)
				beforeReceipt := domain.CaptureAttempt(*c).Fingerprint()
				if err := processFrozenCapture(ctx, worker.c, worker.cwd, worker.cwd, c); err != nil {
					t.Fatal("retry completed continuation", err)
				}
				storedC = worker.readAttempt(t, c)
				if domain.CaptureAttempt(storedC).Fingerprint() != beforeReceipt || !reflect.DeepEqual(worker.history(t), beforeHistory) {
					t.Fatal("retry changed completed receipt or immutable history")
				}
			})
		}
	}
}

func TestFrozenCaptureSequenceReviewEmptyContinuationRootSelection(t *testing.T) {
	for _, initial := range []string{"inherited", "empty"} {
		t.Run(initial, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			f := newFrozenDriver(t)
			const text = "dedup-root-transition"
			// Seed a snapshot with an explicitly inherited working-memory pin.
			// A then revisits these exact document bytes with new frozen memory.
			seed := f.attempt(t, text, domain.DocumentIdentityRootV1, true)
			if err := processFrozenCapture(ctx, f.c, f.cwd, f.cwd, seed); err != nil {
				t.Fatal("seed inherited selection", err)
			}
			position, err := f.store.GetWorkingPosition(ctx)
			if err != nil || position.MemoryHash == "" || position.MemorySource == position.Snapshot {
				t.Fatal("fixture did not produce inherited memory", err)
			}
			if initial == "empty" {
				position.MemoryHash, position.MemorySource = "", ""
				position.Selection = nil
				if err := f.store.PutWorkingPosition(ctx, position); err != nil {
					t.Fatal("select explicit empty memory", err)
				}
			}
			runLifecycleGit(t, f.cwd, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "dedup A with fresh memory")
			worker := frozenSequenceReviewProcess(t, f)
			a := frozenSequenceReviewSeal(t, worker, domain.DocumentIdentityRootV1, text, false)
			runLifecycleGit(t, f.cwd, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "empty B after dedup A")
			worker = frozenSequenceReviewProcess(t, f)
			b := frozenSequenceReviewSeal(t, worker, domain.DocumentIdentityRootV1, "empty-successor", true)
			if b.Predecessor == nil || b.Predecessor.AttemptID != a.Proof.ID {
				t.Fatal("missing pending dedup predecessor")
			}
			for _, p := range []*commitCapturePass{a, b} {
				if err := processFrozenCapture(ctx, worker.c, worker.cwd, worker.cwd, p); err != nil {
					t.Fatal("complete dedup/empty sequence", err)
				}
				*p = worker.readAttempt(t, p)
				if !p.Complete || !p.MemoryFinalized || p.Observation == nil || p.Proof.Target != seed.Proof.Target {
					t.Fatal("dedup sequence did not retain the original snapshot")
				}
			}
			if b.FinalMemory != nil || b.Observation.MemoryHash != a.Observation.MemoryHash {
				t.Fatal("empty B changed the exact predecessor memory")
			}
			// Completion alone is insufficient: the retained ledger must still
			// yield one causal memory selection when a PR freezes this code.
			history := app.NewContextHistoryService(worker.store, worker.store)
			for i, p := range []*commitCapturePass{a, b} {
				receipt := domain.HistoryEvent{ID: strings.Repeat("f", 32), RepoID: f.repo.ID, Branch: "destination", BranchID: "destination-id",
					Kind: "pr-merge", PRCompleted: true, SourceBranchID: p.Proof.BranchID, Source: p.Proof.Target,
					Target: domain.HashContent([]byte("synthetic unrelated merge target")), CreatedAt: p.Proof.CreatedAt.Add(time.Hour),
					PR: &domain.PullRequestMerge{Number: 1, BaseBranch: "destination", HeadBranch: p.Proof.Branch, HeadSHA: p.Proof.GitAfter, MergeSHA: strings.Repeat("a", 40)}}
				selected, err := history.ResolvePRSourcePosition(ctx, receipt)
				if err != nil || selected.MemoryHash != p.Observation.MemoryHash {
					t.Errorf("PR source for sequence step %d cannot recover completed memory %s: selected=%+v err=%v", i, p.Observation.MemoryHash, selected, err)
				}
			}
		})
	}
}

func TestFrozenCaptureSequenceReviewLongLivedWorkerAppliesLatestCode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// This container/store represents the already-running worker at Git A.
	workerA := newFrozenDriver(t)
	workerA.save.WithFrozenMemory(nil, memory.NewRuleDistiller())
	a := frozenSequenceReviewSeal(t, workerA, domain.DocumentIdentityRootV1, "long-worker-A", false)
	runLifecycleGit(t, workerA.cwd, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "B admitted while A worker lives")
	// The B hook is a fresh process, while replay still belongs to worker A.
	hookB := frozenSequenceReviewProcess(t, workerA)
	b := frozenSequenceReviewSeal(t, hookB, domain.DocumentIdentityRootV1, "long-worker-B", false)
	if b.Predecessor == nil || b.Predecessor.AttemptID != a.Proof.ID {
		t.Fatal("long-lived worker fixture lost predecessor")
	}
	for _, p := range []*commitCapturePass{a, b} {
		if err := processFrozenCapture(ctx, workerA.c, workerA.cwd, workerA.cwd, p); err != nil {
			t.Fatal("existing worker could not complete frozen input", err)
		}
		*p = workerA.readAttempt(t, p)
		if !p.Complete || !p.MemoryFinalized || p.Observation == nil {
			t.Fatal("existing worker did not finalize the attempt")
		}
	}
	publication := domain.CaptureAttempt(*b).Publication()
	if _, ok := workerA.history(t)[publication.ID]; !ok {
		t.Fatal("fixture did not reach the published-attempt worker skip condition")
	}
	position, err := workerA.store.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := workerA.store.GetRef(ctx, workerA.repo.ID, domain.RefBranch, b.Proof.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if position.Snapshot != b.Proof.Target || position.GitCommit != b.Proof.GitAfter || ref.Target != b.Proof.Target {
		t.Errorf("worker at A completed/published B but left active target behind: selected=%s shared=%s want=%s selectedGit=%s wantGit=%s", position.Snapshot, ref.Target, b.Proof.Target, position.GitCommit, b.Proof.GitAfter)
	}
	// Show that B's frozen CAS evidence is still valid: the identical completed
	// attempt applies through a fresh process. Automatic replay normally skips
	// this published receipt, so it cannot rely on that later repair occurring.
	if err := processFrozenCapture(ctx, hookB.c, hookB.cwd, hookB.cwd, b); err != nil {
		t.Fatal("fresh-process control failed", err)
	}
	position, err = hookB.store.GetWorkingPosition(ctx)
	if err != nil || position.Snapshot != b.Proof.Target || position.GitCommit != b.Proof.GitAfter {
		t.Fatal("B's original selection should permit exact application in a current process", err)
	}
}
