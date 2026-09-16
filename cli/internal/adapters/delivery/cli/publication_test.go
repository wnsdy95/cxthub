package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/branchjournal"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func TestCommitPublicationSurvivesFailedHistoryWrite(t *testing.T) {
	cwd, c, _, repo, target := historyFixture(t)
	ctx := context.Background()
	oid := gitOut(cwd, "rev-parse", "HEAD")
	st := storage.NewWorktreeFileStore(cwd, gitOut(cwd, "rev-parse", "--absolute-git-dir"), "main", oid)
	history := app.NewContextHistoryService(st, st)
	if err := history.SelectPosition(ctx, domain.WorkingPosition{RepoID: repo, Branch: "main", GitCommit: oid, Snapshot: target}); err != nil {
		t.Fatal(err)
	}
	c.History = unavailableRewriteHistory{history}
	pass, err := beginCommitCapture(ctx, c, cwd, []string{domain.ProviderClaude})
	if err != nil {
		t.Fatal(err)
	}
	if err := pass.recordOutcome(cwd, 0, "", "saved", inbound.SaveOutput{SnapshotID: target}, nil); err != nil {
		t.Fatal(err)
	}
	if err := recordCommitPublication(ctx, c, cwd, pass); err == nil {
		t.Fatal("acknowledged unpersisted publication")
	}
	st = storage.NewWorktreeFileStore(cwd, gitOut(cwd, "rev-parse", "--absolute-git-dir"), "main", oid)
	c.History = app.NewContextHistoryService(st, st)
	for i := 0; i < 2; i++ {
		if err := replayPublications(ctx, c, cwd); err != nil {
			t.Fatal(err)
		}
	}
	events, err := c.History.ListHistory(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range events {
		if e.Kind == "publish" {
			count++
			if e.Target != target || e.GitAfter != oid {
				t.Fatalf("wrong publication: %+v", e)
			}
		}
	}
	if count != 1 {
		t.Fatalf("publications = %d", count)
	}
}

func TestSquashFinalizationWaitsForEveryOriginal(t *testing.T) {
	a, b, x := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	first, last := domain.HashContent([]byte("first")), domain.HashContent([]byte("last"))
	p := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(domain.HashContent([]byte("repo"))), BranchID: "feature", Branch: "feature", WorktreeID: strings.Repeat("2", 32), Kind: "publish", Source: first, Target: first, GitAfter: a, MemoryPinned: true, CreatedAt: time.Unix(1, 0).UTC()}
	q := p
	q.ID, q.Source, q.Target, q.GitAfter = strings.Repeat("3", 32), last, last, b
	batch := rewriteBatch{RepoID: p.RepoID, BranchID: p.BranchID, WorktreeID: p.WorktreeID, Rewrites: map[string]string{a: x, b: x}}
	snaps := []domain.Snapshot{{ID: first}, {ID: last, Parents: []domain.ContentHash{first}}}
	ordinary := q
	ordinary.Kind = "position"
	got, err := rewrittenPublications([]domain.HistoryEvent{p, ordinary}, batch, snaps)
	if err != nil || len(got) != 0 {
		t.Fatalf("partial squash became final: %v %v", got, err)
	}
	proof := q
	proof.ID, proof.Kind, proof.GitAfter = strings.Repeat("4", 32), "position", x
	events := []domain.HistoryEvent{p, q, proof}
	got, err = rewrittenPublications(events, batch, snaps)
	if err != nil || len(got) != 1 || got[0].Target != last || got[0].GitAfter != x {
		t.Fatalf("complete squash = %v %v", got, err)
	}
	if again, err := rewrittenPublications(append(events, got...), batch, snaps); err != nil || len(again) != 0 {
		t.Fatalf("duplicate finalization: %v %v", again, err)
	}
	snaps[1].Parents = nil
	if _, err := rewrittenPublications(events, batch, snaps); err == nil {
		t.Fatal("incomparable contexts were finalized")
	}
}

func TestRewritePublicationWaitsForNativeFinalization(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	s, next := domain.HashContent([]byte("old")), domain.HashContent([]byte("native"))
	old := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(domain.HashContent([]byte("repo"))), BranchID: "feature", Branch: "feature", WorktreeID: strings.Repeat("2", 32), Kind: "publish", Source: s, Target: s, GitAfter: a, MemoryPinned: true, CreatedAt: time.Unix(1, 0).UTC()}
	native := old
	native.ID, native.Kind, native.Source, native.Target, native.GitAfter = strings.Repeat("3", 32), "position", next, next, b
	batch := rewriteBatch{RepoID: old.RepoID, BranchID: old.BranchID, WorktreeID: old.WorktreeID, Rewrites: map[string]string{a: b}}
	events := []domain.HistoryEvent{old, native}
	aliases, err := rewrittenHistory(events, batch.Rewrites, batch.BranchID, batch.WorktreeID, time.Now())
	if err != nil || len(aliases) != 0 {
		t.Fatalf("native observation should suppress old alias: %+v %v", aliases, err)
	}
	got, err := rewrittenPublications(events, batch, []domain.Snapshot{{ID: s}, {ID: next, Parents: []domain.ContentHash{s}}})
	if err != nil || len(got) != 0 {
		t.Fatalf("publication without exact destination proof: %+v %v", got, err)
	}
}

func publicationFixture(t *testing.T) (string, *Container, *storage.FileStore, string, domain.ContentHash) {
	t.Helper()
	cwd, c, _, repo, target := historyFixture(t)
	t.Setenv("HOME", t.TempDir())
	for _, key := range []string{"CXT_REMOTE", "CXT_WRAPPED", "CODEX_THREAD_ID", "CODEX_SESSION_ID"} {
		t.Setenv(key, "")
	}
	t.Setenv("CXT_WRAPPED_AGENT", "claude")
	st := storage.NewWorktreeFileStore(cwd, gitOut(cwd, "rev-parse", "--absolute-git-dir"), "main", gitOut(cwd, "rev-parse", "HEAD"))
	c.History = app.NewContextHistoryService(st, st)
	c.List, c.Memorize = app.NewListSessionsService(st), noOpMemorize{}
	if err := c.History.SelectPosition(context.Background(), domain.WorkingPosition{RepoID: repo, Branch: "main", GitCommit: gitOut(cwd, "rev-parse", "HEAD"), Snapshot: target}); err != nil {
		t.Fatal(err)
	}
	return cwd, c, st, repo, target
}

type publicationSaveFunc func(context.Context, inbound.SaveInput) (inbound.SaveOutput, error)

func (f publicationSaveFunc) Save(ctx context.Context, in inbound.SaveInput) (inbound.SaveOutput, error) {
	return f(ctx, in)
}

func publicationSnapshot(t *testing.T, st *storage.FileStore, repo, label string, parents ...domain.ContentHash) domain.ContentHash {
	t.Helper()
	ctx := context.Background()
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SessionOriginID: label}}}
	id, err := st.PutDoc(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: "main", Parents: parents}); err != nil {
		t.Fatal(err)
	}
	return id
}

func capturePasses(t *testing.T, cwd string) []commitCapturePass {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(cwd, ".cxt", "worktrees", "*", "capture-passes", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out []commitCapturePass
	for _, path := range files {
		raw, err := os.ReadFile(path)
		var p commitCapturePass
		if err != nil || json.Unmarshal(raw, &p) != nil {
			t.Fatalf("read capture pass %s: %v", path, err)
		}
		out = append(out, p)
	}
	return out
}

func assertNoPublication(t *testing.T, c *Container, repo string) {
	t.Helper()
	events, err := c.History.ListHistory(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == "publish" {
			t.Fatalf("incomplete pass published: %+v", e)
		}
	}
}

func TestCommitPublicationUsesOwnOutputsDuringConcurrentPass(t *testing.T) {
	cwd, c, st, repo, initial := publicationFixture(t)
	ctx := context.Background()
	own := publicationSnapshot(t, st, repo, "own complete pass", initial)
	other := publicationSnapshot(t, st, repo, "other partial pass", initial)
	if err := remotecfg.SetStagedProviders(cwd, []string{domain.ProviderClaude}); err != nil {
		t.Fatal(err)
	}
	read, release := make(chan struct{}), make(chan struct{})
	c.Save = publicationSaveFunc(func(_ context.Context, _ inbound.SaveInput) (inbound.SaveOutput, error) {
		// A real successful Save durably records this exact observation before
		// returning, even if another pass subsequently changes the cursor.
		if err := c.History.SelectPosition(ctx, domain.WorkingPosition{RepoID: repo, Branch: "main", GitCommit: gitOut(cwd, "rev-parse", "HEAD"), Snapshot: own}); err != nil {
			return inbound.SaveOutput{}, err
		}
		close(read)
		<-release
		return inbound.SaveOutput{SnapshotID: own, Branch: "main"}, nil
	})
	done := make(chan error, 1)
	go func() { _, err := snapshotForCommit(ctx, c, cwd, "commit"); done <- err }()
	select {
	case <-read:
	case err := <-done:
		t.Fatalf("pass stopped before Save: %v", err)
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("Save was not entered")
	}
	passes := capturePasses(t, cwd)
	if len(passes) != 1 || passes[0].Complete || passes[0].Outcomes[0].State != "pending" {
		close(release)
		<-done
		t.Fatalf("Save ran without durable intent: %+v", passes)
	}
	// A second pass gets as far as its first provider and changes the shared
	// worktree selection while the first pass's Save is still in progress.
	second, err := beginCommitCapture(ctx, c, cwd, []string{domain.ProviderClaude, domain.ProviderCodex})
	if err == nil {
		err = second.recordOutcome(cwd, 0, "", "saved", inbound.SaveOutput{SnapshotID: other}, nil)
	}
	if err == nil {
		err = c.History.SelectPosition(ctx, domain.WorkingPosition{RepoID: repo, Branch: "main", GitCommit: gitOut(cwd, "rev-parse", "HEAD"), Snapshot: other})
	}
	close(release)
	firstErr := <-done
	if err != nil || firstErr != nil {
		t.Fatalf("interleaved capture: second=%v first=%v", err, firstErr)
	}
	events, err := c.History.ListHistory(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	var published []domain.ContentHash
	for _, e := range events {
		if e.Kind == "publish" {
			published = append(published, e.Target)
		}
	}
	if !reflect.DeepEqual(published, []domain.ContentHash{own}) {
		t.Fatalf("published another pass's position: %v", published)
	}
	position, _ := c.History.CurrentPosition(ctx)
	if position.Snapshot != other {
		t.Fatal("publication rewrote the other pass's position")
	}
}

func TestCommitPublicationRejectsChangedGitIdentity(t *testing.T) {
	for _, change := range []string{"sha", "branch"} {
		t.Run(change, func(t *testing.T) {
			cwd, c, _, repo, target := publicationFixture(t)
			if err := remotecfg.SetStagedProviders(cwd, []string{domain.ProviderClaude}); err != nil {
				t.Fatal(err)
			}
			old := gitOut(cwd, "rev-parse", "HEAD")
			c.Save = publicationSaveFunc(func(_ context.Context, _ inbound.SaveInput) (inbound.SaveOutput, error) {
				if change == "sha" {
					runLifecycleGit(t, cwd, "commit", "--allow-empty", "-qm", "concurrent commit")
				} else {
					runLifecycleGit(t, cwd, "switch", "-qc", "other")
				}
				return inbound.SaveOutput{SnapshotID: target, Branch: "main"}, nil
			})
			_, err := snapshotForCommit(context.Background(), c, cwd, "commit")
			if err == nil || !strings.Contains(err.Error(), "changed during capture") {
				t.Fatalf("changed identity was finalized: %v", err)
			}
			passes := capturePasses(t, cwd)
			if len(passes) != 1 || passes[0].Complete || passes[0].Proof.GitAfter != old {
				t.Fatalf("intent was relabeled: %+v", passes)
			}
			assertNoPublication(t, c, repo)
		})
	}
}

func TestCommitSelectedTranscriptDisappearingRemainsPending(t *testing.T) {
	cwd, c, _, repo, _ := publicationFixture(t)
	id := "11111111-1111-4111-8111-111111111111"
	t.Setenv("CODEX_THREAD_ID", id)
	path := writeCodexRollout(t, os.Getenv("HOME"), cwd, id, time.Now())
	if err := remotecfg.SetStagedProviders(cwd, []string{domain.ProviderCodex}); err != nil {
		t.Fatal(err)
	}
	c.Save = publicationSaveFunc(func(_ context.Context, in inbound.SaveInput) (inbound.SaveOutput, error) {
		if in.SessionPath != path {
			t.Fatalf("exact session was not selected: %+v", in)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		return inbound.SaveOutput{}, domain.ErrNoActiveSession
	})
	_, err := snapshotForCommit(context.Background(), c, cwd, "commit")
	if !errors.Is(err, domain.ErrNoActiveSession) || !strings.Contains(err.Error(), "remains pending") {
		t.Fatalf("missing selected transcript was treated as inactive: %v", err)
	}
	passes := capturePasses(t, cwd)
	if len(passes) != 1 || passes[0].Complete || passes[0].Outcomes[0].State != "failed" {
		t.Fatalf("capture failure not retained: %+v", passes)
	}
	if err := replayPublications(context.Background(), c, cwd); err != nil {
		t.Fatalf("pending pass blocked unrelated replay: %v", err)
	}
	assertNoPublication(t, c, repo)
}

func TestCommitCaptureCrashBeforeCompletionCannotBeInferred(t *testing.T) {
	for _, outcomes := range []int{0, 1, 2} {
		t.Run(string(rune('0'+outcomes))+"-recorded-outcomes", func(t *testing.T) {
			cwd, c, st, repo, initial := publicationFixture(t)
			target := publicationSnapshot(t, st, repo, "saved but no exact observation", initial)
			ctx := context.Background()
			pass, err := beginCommitCapture(ctx, c, cwd, []string{domain.ProviderClaude, domain.ProviderCodex})
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < outcomes; i++ {
				if err := pass.recordOutcome(cwd, i, "", "saved", inbound.SaveOutput{SnapshotID: target}, nil); err != nil {
					t.Fatal(err)
				}
			}
			// Even all successful Save outcomes are insufficient if the process
			// died before confirming the frozen Git identity and final target.
			st = storage.NewWorktreeFileStore(cwd, gitOut(cwd, "rev-parse", "--absolute-git-dir"), "main", pass.Proof.GitAfter)
			c.History = app.NewContextHistoryService(st, st)
			for i := 0; i < 2; i++ {
				if err := replayPublications(ctx, c, cwd); err != nil {
					t.Fatalf("incomplete capture blocked unrelated replay: %v", err)
				}
			}
			if got := capturePasses(t, cwd); len(got) != 1 || got[0].Complete {
				t.Fatalf("incomplete capture silently recovered: %+v", got)
			}
			assertNoPublication(t, c, repo)
		})
	}
}

type acceptedPublicationHistory struct{ inbound.ContextHistory }

func (h acceptedPublicationHistory) ValidateHistorySource(context.Context, domain.HistoryEvent) (domain.HistoryEvent, error) {
	return domain.HistoryEvent{}, errors.New("accepted publication must not reconstruct transcript")
}

func TestPublicationReplaySkipsAcceptedSourceValidation(t *testing.T) {
	cwd, c, _, _, target := publicationFixture(t)
	c.Save = publicationSaveFunc(func(context.Context, inbound.SaveInput) (inbound.SaveOutput, error) {
		return inbound.SaveOutput{SnapshotID: target, Branch: "main"}, nil
	})
	ctx := context.Background()
	if _, err := snapshotForCommit(ctx, c, cwd, "commit"); err != nil {
		t.Fatal(err)
	}
	c.History = acceptedPublicationHistory{c.History}
	files, err := filepath.Glob(filepath.Join(cwd, ".cxt", "worktrees", "*", "publication-journal", "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("publication journals: %v %v", files, err)
	}
	before, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	j, err := branchjournal.Open(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	// Holding the creation lock proves replay does not reacquire it. An
	// unchanged accepted journal must also keep its original inode and mtime.
	if err := j.Transaction(ctx, func() error {
		bounded, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		for i := 0; i < 2; i++ {
			if err := replayPublications(bounded, c, cwd); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(files[0])
	if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("accepted journal was rewritten: %v", err)
	}
}

func TestCommitPublicationPreservesMemorySelection(t *testing.T) {
	for _, reuse := range []bool{true, false} {
		name := "existing-exact-observation"
		if !reuse {
			name = "dedup-new-sha-frozen-baseline"
		}
		t.Run(name, func(t *testing.T) {
			cwd, c, st, repo, target := publicationFixture(t)
			ctx := context.Background()
			memory, err := st.PutMemory(ctx, domain.MemoryDigest{SnapshotID: target, Summary: "retain original project memory"})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.RecordWorkingMemory(ctx, target, memory); err != nil {
				t.Fatal(err)
			}
			if !reuse {
				runLifecycleGit(t, cwd, "commit", "--allow-empty", "-qm", "new code, unchanged transcript")
			}
			pass, err := beginCommitCapture(ctx, c, cwd, []string{domain.ProviderClaude})
			if err != nil {
				t.Fatal(err)
			}
			if err := pass.recordOutcome(cwd, 0, "", "saved", inbound.SaveOutput{SnapshotID: target}, nil); err != nil {
				t.Fatal(err)
			}
			before, _ := c.History.ListHistory(ctx, repo)
			if err := recordCommitPublication(ctx, c, cwd, pass); err != nil {
				t.Fatal(err)
			}
			events, err := c.History.ListHistory(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			selected := contextSelectionAtCode(cwd, pass.Proof.GitAfter, "main", nil, events)
			if selected.Snapshot != target || selected.MemoryHash != memory || !selected.MemoryPinned {
				t.Fatalf("finalization erased memory: %+v", selected)
			}
			if reuse && (pass.Observation == nil || len(events) != len(before)+1) {
				t.Fatalf("finalization manufactured an extra ordinary proof: %+v", events)
			}
			if err := replayPublications(ctx, c, cwd); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCommitCaptureRecoveryUsesOnlyFrozenOutcomesAndObservations(t *testing.T) {
	cwd, c, st, repo, initial := publicationFixture(t)
	ctx := context.Background()
	first := publicationSnapshot(t, st, repo, "first provider", initial)
	last := publicationSnapshot(t, st, repo, "last provider", first)
	pass, err := beginCommitCapture(ctx, c, cwd, []string{domain.ProviderClaude, domain.ProviderCodex})
	if err != nil {
		t.Fatal(err)
	}
	for i, target := range []domain.ContentHash{first, last} {
		if err := c.History.SelectPosition(ctx, domain.WorkingPosition{RepoID: repo, Branch: "main", Snapshot: target, GitCommit: pass.Proof.GitAfter}); err != nil {
			t.Fatal(err)
		}
		if err := pass.recordOutcome(cwd, i, filepath.Join(cwd, "removed-native-transcript"), "saved", inbound.SaveOutput{SnapshotID: target}, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Crash before Complete/proof. Both Git and the mutable selection can move
	// before retry, and no native transcript is available to reread.
	runLifecycleGit(t, cwd, "switch", "-qc", "other")
	runLifecycleGit(t, cwd, "commit", "--allow-empty", "-qm", "independent code")
	if err := c.History.SelectPosition(ctx, domain.WorkingPosition{RepoID: repo, Branch: "other", Snapshot: initial, GitCommit: gitOut(cwd, "rev-parse", "HEAD")}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := replayPublications(ctx, c, cwd); err != nil {
			t.Fatal(err)
		}
	}
	got := capturePasses(t, cwd)
	if len(got) != 1 || !got[0].Complete || got[0].Proof.Target != last || got[0].Proof.GitAfter != pass.Proof.GitAfter || got[0].Observation == nil {
		t.Fatalf("frozen completed outputs were not recovered: %+v", got)
	}
	position, _ := c.History.CurrentPosition(ctx)
	if position.Branch != "other" || position.Snapshot != initial {
		t.Fatalf("replay changed current selection: %+v", position)
	}
}

func TestPublicationReplayRetainsFailedPassWithoutBlockingOtherBranch(t *testing.T) {
	cwd, c, _, repo, target := publicationFixture(t)
	ctx := context.Background()
	failed, err := beginCommitCapture(ctx, c, cwd, []string{domain.ProviderClaude})
	if err != nil {
		t.Fatal(err)
	}
	if err := failed.recordOutcome(cwd, 0, "selected.jsonl", "failed", inbound.SaveOutput{}, errors.New("selected transcript disappeared")); err != nil {
		t.Fatal(err)
	}
	runLifecycleGit(t, cwd, "switch", "-qc", "independent")
	if err := c.History.SelectPosition(ctx, domain.WorkingPosition{RepoID: repo, Branch: "independent", Snapshot: target, GitCommit: failed.Proof.GitAfter}); err != nil {
		t.Fatal(err)
	}
	completed, err := beginCommitCapture(ctx, c, cwd, []string{domain.ProviderClaude})
	if err != nil {
		t.Fatal(err)
	}
	if err := completed.recordOutcome(cwd, 0, "", "saved", inbound.SaveOutput{SnapshotID: target}, nil); err != nil {
		t.Fatal(err)
	}
	history := c.History
	c.History = unavailableRewriteHistory{history}
	if err := recordCommitPublication(ctx, c, cwd, completed); err == nil || !completed.Complete {
		t.Fatalf("expected completed pass awaiting history delivery: %v", err)
	}
	c.History = history
	for i := 0; i < 2; i++ {
		if err := replayPublications(ctx, c, cwd); err != nil {
			t.Fatalf("failed capture blocked independent completed delivery: %v", err)
		}
	}
	events, _ := c.History.ListHistory(ctx, repo)
	publications := 0
	for _, e := range events {
		if e.Kind == "publish" {
			publications++
			if e.Branch != "independent" {
				t.Fatalf("failed branch was published: %+v", e)
			}
		}
	}
	if publications != 1 {
		t.Fatalf("completed branch publications = %d", publications)
	}
	for _, p := range capturePasses(t, cwd) {
		if p.Proof.ID == failed.Proof.ID && (p.Complete || p.Outcomes[0].Error != "selected transcript disappeared") {
			t.Fatalf("failed pass was silently finalized: %+v", p)
		}
	}
}

func TestPublicationReplayRejectsCorruptPass(t *testing.T) {
	cwd, c, _, _, _ := publicationFixture(t)
	pass, err := beginCommitCapture(context.Background(), c, cwd, []string{domain.ProviderClaude})
	if err != nil {
		t.Fatal(err)
	}
	pass.Outcomes[0].State = "corrupt"
	if err := pass.write(cwd); err != nil {
		t.Fatal(err)
	}
	if err := replayPublications(context.Background(), c, cwd); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("corruption treated as pending: %v", err)
	}
}

type publicationBarrierList struct {
	inbound.ListSessions
	ready  chan<- struct{}
	resume <-chan struct{}
}

func (l publicationBarrierList) List(ctx context.Context, in inbound.ListInput) (inbound.ListOutput, error) {
	l.ready <- struct{}{}
	select {
	case <-l.resume:
		return l.ListSessions.List(ctx, in)
	case <-ctx.Done():
		return inbound.ListOutput{}, ctx.Err()
	}
}

func TestCommitPublicationLiveCompletionAndReplayAgree(t *testing.T) {
	cwd, c, _, repo, target := publicationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pass, err := beginCommitCapture(ctx, c, cwd, []string{domain.ProviderClaude})
	if err != nil {
		t.Fatal(err)
	}
	if err := pass.recordOutcome(cwd, 0, "", "saved", inbound.SaveOutput{SnapshotID: target}, nil); err != nil {
		t.Fatal(err)
	}
	ready, resume := make(chan struct{}, 2), make(chan struct{})
	c.List = publicationBarrierList{ListSessions: c.List, ready: ready, resume: resume}
	done := make(chan error, 2)
	go func() { done <- recordCommitPublication(ctx, c, cwd, pass) }()
	go func() { done <- replayPublications(ctx, c, cwd) }()
	// Both finalizers have independently loaded the same saved outcomes while
	// Complete is still false. Neither can rely on having run first.
	for i := 0; i < 2; i++ {
		select {
		case <-ready:
		case err := <-done:
			close(resume)
			t.Fatalf("finalizer stopped before barrier: %v", err)
		case <-ctx.Done():
			close(resume)
			t.Fatal(ctx.Err())
		}
	}
	close(resume)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent completion: %v", err)
		}
	}
	stored := capturePasses(t, cwd)
	if len(stored) != 1 || !stored[0].Complete || !reflect.DeepEqual(stored[0], *pass) {
		t.Fatalf("finalizers disagreed about immutable completion: %+v %+v", stored, pass)
	}
	events, _ := c.History.ListHistory(ctx, repo)
	publications := 0
	for _, e := range events {
		if e.Kind == "publish" {
			publications++
		}
	}
	if publications != 1 {
		t.Fatalf("publications = %d", publications)
	}
}

func TestCommitPublicationIntentFailurePreventsSave(t *testing.T) {
	cwd, c, _, _, _ := publicationFixture(t)
	position, err := c.History.CurrentPosition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cwd, ".cxt", "worktrees", position.WorktreeID, "capture-passes")
	if err := os.WriteFile(path, []byte("cannot create journal directory"), 0600); err != nil {
		t.Fatal(err)
	}
	c.Save = nil // A Save before durable intent would panic.
	if _, err := snapshotForCommit(context.Background(), c, cwd, "commit"); err == nil || !strings.Contains(err.Error(), "before Save") {
		t.Fatalf("Save was permitted without intent: %v", err)
	}
}

type noPublicationPosition struct{ inbound.ContextHistory }

func (noPublicationPosition) CurrentPosition(context.Context) (domain.WorkingPosition, error) {
	return domain.WorkingPosition{}, domain.ErrNotFound
}

func TestPublicationReplayWithoutPositionIsNoop(t *testing.T) {
	cwd := t.TempDir()
	if err := replayPublications(context.Background(), &Container{History: noPublicationPosition{}}, cwd); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".cxt")); !os.IsNotExist(err) {
		t.Fatalf("no-position replay created capture state: %v", err)
	}
}

func TestPostCommitSkipsOnlyDetachedOperationCapture(t *testing.T) {
	for _, operation := range []bool{true, false} {
		name := "detached-user-commit"
		if operation {
			name = "intermediate-rebase-commit"
		}
		t.Run(name, func(t *testing.T) {
			cwd, _, _, _, _ := publicationFixture(t)
			runLifecycleGit(t, cwd, "switch", "--detach", "-q", "HEAD")
			c := &Container{} // nil Save is a panic trap if the intermediate capture runs.
			calls := 0
			if operation {
				if err := os.Mkdir(filepath.Join(cwd, ".git", "rebase-merge"), 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				c.Memorize = noOpMemorize{}
				c.Save = publicationSaveFunc(func(context.Context, inbound.SaveInput) (inbound.SaveOutput, error) {
					calls++
					return inbound.SaveOutput{SnapshotID: domain.HashContent([]byte("detached")), Branch: "HEAD"}, nil
				})
			}
			if err := runGitHook(context.Background(), c, cwd, []string{"post-commit"}); err != nil {
				t.Fatal(err)
			}
			if !operation && calls != 2 {
				t.Fatalf("detached user commit lost capture: %d calls", calls)
			}
		})
	}
}
