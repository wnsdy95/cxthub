package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func joinRecoveryFixture(t *testing.T) (*FSStore, domain.ContentHash, domain.ContentHash, domain.ContentHash, []byte, []byte) {
	t.Helper()
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte("join-recovery-repo"))
	h := domain.HashContent([]byte("head"))
	x := domain.HashContent([]byte("session-branch"))
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: h, DocHash: h, RepoID: repo, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	beforeSnap := domain.Snapshot{ID: x, DocHash: x, RepoID: repo, Branch: "main"}
	if err := st.PutSnapshot(ctx, beforeSnap); err != nil {
		t.Fatal(err)
	}
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: h}, ""); err != nil {
		t.Fatal(err)
	}
	afterSnap := beforeSnap
	afterSnap.Grafted = true
	afterSnap.GraftParents = []domain.ContentHash{h}
	afterSnap.GraftSeq = 1
	before, _ := json.Marshal(beforeSnap)
	after, _ := json.Marshal(afterSnap)
	return st, repo, h, x, before, after
}

func TestFSStorePreparedJoinJournalRollsBack(t *testing.T) {
	ctx := context.Background()
	st, repo, h, x, before, after := joinRecoveryFixture(t)
	fork := "fork/recovery"
	created := time.Now().UTC()
	j := fsJoinJournal{
		Version: fsJoinJournalVersion, Phase: fsJoinPrepared, RepoID: repo,
		Branch: "main", ExpectedHead: h, NewHead: x, ForkName: fork, ForkTip: x,
		CreatedAt: created, Snapshots: []fsJoinSnapshot{{ID: x, Before: before, After: after}},
	}
	// prepared state where the process is dying at an intermediate point: even if some or
	// all of snapshot/fork/ref are visible, the API success point has passed, so restart must go back to before-state.
	if err := writeAtomic(st.snapshotPath(repo, x), after); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(st.refFile(repo, domain.RefSession, fork), []byte(string(x)+"\n")); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(st.refFile(repo, domain.RefBranch, "main"), []byte(string(x)+"\n")); err != nil {
		t.Fatal(err)
	}
	if err := st.writeJoinJournal(st.joinJournalPath(repo), j); err != nil {
		t.Fatal(err)
	}

	recovered, err := OpenFSStore(st.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := recovered.GetRef(ctx, repo, domain.RefBranch, "main")
	if err != nil || ref.Target != h {
		t.Fatalf("prepared ref not rolled back: %+v %v", ref, err)
	}
	snap, err := recovered.GetSnapshot(ctx, repo, x)
	if err != nil || snap.GraftSeq != 0 || len(snap.GraftParents) != 0 {
		t.Fatalf("prepared graft not rolled back: %+v %v", snap, err)
	}
	if _, err := recovered.GetRef(ctx, repo, domain.RefSession, fork); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("prepared fork remains: %v", err)
	}
	if _, err := os.Stat(recovered.joinJournalPath(repo)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rolled-back journal remains: %v", err)
	}
	logs, err := recovered.ReadReflog(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range logs {
		if (entry.Name == "main" && entry.Old == h && entry.New == x) || entry.Name == fork {
			t.Fatalf("prepared join left reflog: %+v", entry)
		}
	}
}

func TestFSStoreCommittedJoinJournalRollsForwardWithReflog(t *testing.T) {
	ctx := context.Background()
	st, repo, h, x, before, after := joinRecoveryFixture(t)
	fork := "fork/recovery"
	created := time.Now().UTC()
	j := fsJoinJournal{
		Version: fsJoinJournalVersion, Phase: fsJoinCommitted, RepoID: repo,
		Branch: "main", ExpectedHead: h, NewHead: x, ForkName: fork, ForkTip: x,
		CreatedAt: created, Snapshots: []fsJoinSnapshot{{ID: x, Before: before, After: after}},
	}
	if err := st.writeJoinJournal(st.joinJournalPath(repo), j); err != nil {
		t.Fatal(err)
	}

	recovered, err := OpenFSStore(st.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := recovered.GetRef(ctx, repo, domain.RefBranch, "main")
	if err != nil || ref.Target != x {
		t.Fatalf("committed ref not recovered: %+v %v", ref, err)
	}
	snap, err := recovered.GetSnapshot(ctx, repo, x)
	if err != nil || snap.GraftSeq != 1 || len(snap.GraftParents) != 1 || snap.GraftParents[0] != h {
		t.Fatalf("committed graft not recovered: %+v %v", snap, err)
	}
	forkRef, err := recovered.GetRef(ctx, repo, domain.RefSession, fork)
	if err != nil || forkRef.Target != x {
		t.Fatalf("committed fork not recovered: %+v %v", forkRef, err)
	}
	logs, err := recovered.ReadReflog(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	seenMain, seenFork := 0, 0
	for _, entry := range logs {
		if entry.Name == "main" && entry.Old == h && entry.New == x && entry.CreatedAt.Equal(created) {
			seenMain++
		}
		if entry.Kind == domain.RefSession && entry.Name == fork && entry.Old == "" && entry.New == x && entry.CreatedAt.Equal(created) {
			seenFork++
		}
	}
	if seenMain != 1 || seenFork != 1 {
		t.Fatalf("join reflog mismatch: main=%d fork=%d logs=%+v", seenMain, seenFork, logs)
	}
}

func TestOpenFSStoreFailsClosedOnInvalidJoinJournal(t *testing.T) {
	st, repo, _, _, _, _ := joinRecoveryFixture(t)
	path := st.joinJournalPath(repo)
	if err := writeAtomic(path, []byte(`{"version":1,"phase":"committed","repo_id":"broken"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFSStore(st.dataDir); err == nil {
		t.Fatal("invalid join journal did not fail startup")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("invalid journal was removed: %v", err)
	}
}

func TestOpenFSStoreRejectsJoinJournalChangingNonGraftMetadata(t *testing.T) {
	st, repo, h, x, before, after := joinRecoveryFixture(t)
	var corrupted domain.Snapshot
	if err := json.Unmarshal(after, &corrupted); err != nil {
		t.Fatal(err)
	}
	corrupted.Message = "journal must not rewrite this"
	after, _ = json.Marshal(corrupted)
	j := fsJoinJournal{
		Version: fsJoinJournalVersion, Phase: fsJoinPrepared, RepoID: repo,
		Branch: "main", ExpectedHead: h, NewHead: x, CreatedAt: time.Now().UTC(),
		Snapshots: []fsJoinSnapshot{{ID: x, Before: before, After: after}},
	}
	path := st.joinJournalPath(repo)
	if err := st.writeJoinJournal(path, j); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFSStore(st.dataDir); err == nil {
		t.Fatal("join journal changing non-graft metadata did not fail startup")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("rejected journal was removed: %v", err)
	}
}

func TestOpenFSStoreRejectsJoinJournalOverUnrelatedCurrentProjection(t *testing.T) {
	ctx := context.Background()
	st, repo, h, x, before, after := joinRecoveryFixture(t)
	j := fsJoinJournal{
		Version: fsJoinJournalVersion, Phase: fsJoinPrepared, RepoID: repo,
		Branch: "main", ExpectedHead: h, NewHead: x, CreatedAt: time.Now().UTC(),
		Snapshots: []fsJoinSnapshot{{ID: x, Before: before, After: after}},
	}
	if err := st.writeJoinJournal(st.joinJournalPath(repo), j); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetSnapshot(ctx, repo, x)
	if err != nil {
		t.Fatal(err)
	}
	current.Message = "independent mutation"
	raw, _ := json.Marshal(current)
	if err := writeAtomic(st.snapshotPath(repo, x), raw); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFSStore(st.dataDir); err == nil {
		t.Fatal("journal overwrote an unrelated current projection")
	}
}

func TestOpenFSStoreRejectsJoinJournalWithMissingRefTarget(t *testing.T) {
	st, repo, h, x, before, after := joinRecoveryFixture(t)
	ghost := domain.HashContent([]byte("journal-missing-head"))
	j := fsJoinJournal{
		Version: fsJoinJournalVersion, Phase: fsJoinCommitted, RepoID: repo,
		Branch: "main", ExpectedHead: h, NewHead: ghost, CreatedAt: time.Now().UTC(),
		Snapshots: []fsJoinSnapshot{{ID: x, Before: before, After: after}},
	}
	path := st.joinJournalPath(repo)
	if err := st.writeJoinJournal(path, j); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFSStore(st.dataDir); err == nil {
		t.Fatal("join journal with missing ref target did not fail startup")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("rejected journal was removed: %v", err)
	}
}

func TestFSStoreApplyJoinRejectsMissingTargetsWithoutMutation(t *testing.T) {
	ctx := context.Background()
	ghost := domain.HashContent([]byte("missing-join-target"))
	tests := []struct {
		name   string
		mutate func(outbound.JoinMutation) outbound.JoinMutation
	}{
		{
			name: "new head",
			mutate: func(m outbound.JoinMutation) outbound.JoinMutation {
				m.NewHead = ghost
				m.Segment = append(m.Segment, ghost)
				return m
			},
		},
		{
			name: "fork tip",
			mutate: func(m outbound.JoinMutation) outbound.JoinMutation {
				m.ForkName = domain.SessionRefPrefix("main") + "missing"
				m.ForkTip = ghost
				m.Segment = append(m.Segment, ghost)
				return m
			},
		},
		{
			name: "graft parent",
			mutate: func(m outbound.JoinMutation) outbound.JoinMutation {
				m.Grafts[0].Parents = append(m.Grafts[0].Parents, ghost)
				return m
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, repo, h, x, _, _ := joinRecoveryFixture(t)
			m := outbound.JoinMutation{
				RepoID: repo, Branch: "main", Source: x, Segment: []domain.ContentHash{x}, ExpectedHead: h, NewHead: x,
				Grafts: []outbound.GraftPatch{{SnapshotID: x, ExpectedSeq: 0, Parents: []domain.ContentHash{h}}},
			}
			err := st.ApplyJoin(ctx, tt.mutate(m))
			if !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("err=%v, want ErrNotFound", err)
			}
			main, rerr := st.GetRef(ctx, repo, domain.RefBranch, "main")
			if rerr != nil || main.Target != h {
				t.Fatalf("failed join moved main: %+v %v", main, rerr)
			}
			snap, serr := st.GetSnapshot(ctx, repo, x)
			if serr != nil || snap.GraftSeq != 0 || len(snap.GraftParents) != 0 {
				t.Fatalf("failed join changed graft: %+v %v", snap, serr)
			}
			if _, statErr := os.Stat(st.joinJournalPath(repo)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failed join left journal: %v", statErr)
			}
			if _, ferr := st.GetRef(ctx, repo, domain.RefSession, domain.SessionRefPrefix("main")+"missing"); !errors.Is(ferr, domain.ErrNotFound) {
				t.Fatalf("failed join left fork ref: %v", ferr)
			}
		})
	}
}

func TestFSStoreGraftRejectsMissingParentWithoutMutation(t *testing.T) {
	ctx := context.Background()
	st, repo, _, x, _, _ := joinRecoveryFixture(t)
	ghost := domain.HashContent([]byte("missing-graft-parent"))
	if err := st.AddGraftParentsCAS(ctx, repo, x, []domain.ContentHash{ghost}, 0); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err=%v, want ErrNotFound", err)
	}
	snap, err := st.GetSnapshot(ctx, repo, x)
	if err != nil || snap.GraftSeq != 0 || len(snap.GraftParents) != 0 {
		t.Fatalf("missing-parent graft changed snapshot: %+v %v", snap, err)
	}
}

func TestFSStoreGraftSequenceNeverWraps(t *testing.T) {
	ctx := context.Background()
	st, repo, h, _, _, _ := joinRecoveryFixture(t)
	x := domain.HashContent([]byte("max-graft-sequence"))
	if err := st.PutSnapshot(ctx, domain.Snapshot{
		ID: x, DocHash: x, RepoID: repo, Branch: "main", GraftSeq: domain.MaxGraftSeq,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddGraftParents(ctx, repo, x, []domain.ContentHash{h}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("max-seq add err=%v, want ErrConflict", err)
	}
	got, err := st.GetSnapshot(ctx, repo, x)
	if err != nil || got.GraftSeq != domain.MaxGraftSeq || len(got.GraftParents) != 0 {
		t.Fatalf("max-seq graft wrapped/mutated: %+v err=%v", got, err)
	}
}

func TestFSStoreApplyJoinNeverOverwritesUnfinishedJournal(t *testing.T) {
	ctx := context.Background()
	st, repo, h, x, _, _ := joinRecoveryFixture(t)
	path := st.joinJournalPath(repo)
	want := []byte("unfinished-operation-must-survive")
	if err := writeAtomic(path, want); err != nil {
		t.Fatal(err)
	}
	err := st.ApplyJoin(ctx, outbound.JoinMutation{
		RepoID: repo, Branch: "main", Source: x, Segment: []domain.ContentHash{x}, ExpectedHead: h, NewHead: x,
		Grafts: []outbound.GraftPatch{{SnapshotID: x, ExpectedSeq: 0, Parents: []domain.ContentHash{h}}},
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("err=%v, want ErrConflict", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != string(want) {
		t.Fatalf("unfinished journal overwritten: %q %v", got, readErr)
	}
	main, refErr := st.GetRef(ctx, repo, domain.RefBranch, "main")
	if refErr != nil || main.Target != h {
		t.Fatalf("blocked join moved main: %+v %v", main, refErr)
	}
}

func TestFSStoreApplyJoinRechecksSourceAttachment(t *testing.T) {
	ctx := context.Background()
	mutation := func(repo, h, x domain.ContentHash) outbound.JoinMutation {
		return outbound.JoinMutation{
			RepoID: repo, Branch: "main", Source: x, Segment: []domain.ContentHash{x}, ExpectedHead: h, NewHead: x,
			Grafts: []outbound.GraftPatch{{SnapshotID: x, ExpectedSeq: 0, Parents: []domain.ContentHash{h}}},
		}
	}

	t.Run("detached object is rejected", func(t *testing.T) {
		st, repo, h, x, _, _ := joinRecoveryFixture(t)
		err := st.ApplyJoin(ctx, mutation(repo, h, x))
		if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("err=%v, want ErrConflict", err)
		}
		main, _ := st.GetRef(ctx, repo, domain.RefBranch, "main")
		snap, _ := st.GetSnapshot(ctx, repo, x)
		if main.Target != h || snap.GraftSeq != 0 || len(snap.GraftParents) != 0 {
			t.Fatalf("detached join changed state: main=%s snap=%+v", main.Target, snap)
		}
	})

	t.Run("session ref keeps a partial branch joinable", func(t *testing.T) {
		st, repo, h, x, _, _ := joinRecoveryFixture(t)
		if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{
			Kind: domain.RefSession, Name: domain.SessionRefPrefix("main") + "source", RepoID: repo, Target: x,
		}, ""); err != nil {
			t.Fatal(err)
		}
		if err := st.ApplyJoin(ctx, mutation(repo, h, x)); err != nil {
			t.Fatalf("session-attached join: %v", err)
		}
		main, _ := st.GetRef(ctx, repo, domain.RefBranch, "main")
		if main.Target != x {
			t.Fatalf("main=%s want=%s", main.Target, x)
		}
	})

	t.Run("another git branch session ref does not authorize main", func(t *testing.T) {
		st, repo, h, x, _, _ := joinRecoveryFixture(t)
		if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{
			Kind: domain.RefSession, Name: domain.SessionRefPrefix("feature") + "source", RepoID: repo, Target: x,
		}, ""); err != nil {
			t.Fatal(err)
		}
		err := st.ApplyJoin(ctx, mutation(repo, h, x))
		if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("cross-branch session attachment err=%v, want ErrConflict", err)
		}
		main, _ := st.GetRef(ctx, repo, domain.RefBranch, "main")
		if main.Target != h {
			t.Fatalf("cross-branch session attachment moved main: %s", main.Target)
		}
	})

	t.Run("nested git branch session scope does not prefix-match its parent branch", func(t *testing.T) {
		st, repo, h, x, _, _ := joinRecoveryFixture(t)
		if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{
			Kind: domain.RefSession, Name: domain.SessionRefPrefix("main/topic") + "source", RepoID: repo, Target: x,
		}, ""); err != nil {
			t.Fatal(err)
		}
		err := st.ApplyJoin(ctx, mutation(repo, h, x))
		if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("nested branch session attachment err=%v, want ErrConflict", err)
		}
		main, _ := st.GetRef(ctx, repo, domain.RefBranch, "main")
		if main.Target != h {
			t.Fatalf("nested branch session attachment moved main: %s", main.Target)
		}
	})

	t.Run("foreign scoped session ref blocks an otherwise main-attached source", func(t *testing.T) {
		st, repo, h, x, _, _ := joinRecoveryFixture(t)
		for _, ref := range []domain.Ref{
			{Kind: domain.RefSession, Name: domain.SessionRefPrefix("main") + "source", RepoID: repo, Target: x},
			{Kind: domain.RefSession, Name: domain.SessionRefPrefix("feature") + "source", RepoID: repo, Target: x},
		} {
			if err := st.CompareAndSwapRef(ctx, repo, ref, ""); err != nil {
				t.Fatal(err)
			}
		}
		err := st.ApplyJoin(ctx, mutation(repo, h, x))
		if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("foreign scoped session reachability err=%v, want ErrConflict", err)
		}
		main, _ := st.GetRef(ctx, repo, domain.RefBranch, "main")
		snap, _ := st.GetSnapshot(ctx, repo, x)
		if main.Target != h || snap.GraftSeq != 0 || len(snap.GraftParents) != 0 {
			t.Fatalf("foreign session rejection changed state: main=%s snap=%+v", main.Target, snap)
		}
	})

	t.Run("detached descendant cannot be promoted through an attached source", func(t *testing.T) {
		st, repo, h, x, _, _ := joinRecoveryFixture(t)
		tip := domain.HashContent([]byte("detached-segment-tip"))
		if err := st.PutSnapshot(ctx, domain.Snapshot{
			ID: tip, DocHash: tip, RepoID: repo, Branch: "main", Parents: []domain.ContentHash{x},
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{
			Kind: domain.RefSession, Name: domain.SessionRefPrefix("main") + "source-only", RepoID: repo, Target: x,
		}, ""); err != nil {
			t.Fatal(err)
		}
		err := st.ApplyJoin(ctx, outbound.JoinMutation{
			RepoID: repo, Branch: "main", Source: x, Segment: []domain.ContentHash{x, tip},
			ExpectedHead: h, NewHead: tip,
			Grafts: []outbound.GraftPatch{{SnapshotID: x, ExpectedSeq: 0, Parents: []domain.ContentHash{h}}},
		})
		if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("err=%v, want ErrConflict", err)
		}
		main, _ := st.GetRef(ctx, repo, domain.RefBranch, "main")
		snap, _ := st.GetSnapshot(ctx, repo, x)
		if main.Target != h || snap.GraftSeq != 0 || len(snap.GraftParents) != 0 {
			t.Fatalf("detached segment join changed state: main=%s snap=%+v", main.Target, snap)
		}
	})

	t.Run("segment that gained an attached child after planning is rejected", func(t *testing.T) {
		st, repo, h, x, _, _ := joinRecoveryFixture(t)
		tip := domain.HashContent([]byte("planned-segment-tip"))
		later := domain.HashContent([]byte("concurrent-attached-child"))
		for _, snap := range []domain.Snapshot{
			{ID: tip, DocHash: tip, RepoID: repo, Branch: "main", Parents: []domain.ContentHash{x}},
			{ID: later, DocHash: later, RepoID: repo, Branch: "main", Parents: []domain.ContentHash{tip}},
		} {
			if err := st.PutSnapshot(ctx, snap); err != nil {
				t.Fatal(err)
			}
		}
		// Service calculates [X,T] and then reproduces the contention on U attached to the same branch-scoped session ref.
		if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{
			Kind: domain.RefSession, Name: domain.SessionRefPrefix("main") + "later", RepoID: repo, Target: later,
		}, ""); err != nil {
			t.Fatal(err)
		}
		err := st.ApplyJoin(ctx, outbound.JoinMutation{
			RepoID: repo, Branch: "main", Source: x, Segment: []domain.ContentHash{x, tip},
			ExpectedHead: h, NewHead: tip,
			Grafts: []outbound.GraftPatch{{SnapshotID: x, ExpectedSeq: 0, Parents: []domain.ContentHash{h}}},
		})
		if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("stale segment err=%v, want ErrConflict", err)
		}
		main, _ := st.GetRef(ctx, repo, domain.RefBranch, "main")
		snap, _ := st.GetSnapshot(ctx, repo, x)
		if main.Target != h || snap.GraftSeq != 0 || len(snap.GraftParents) != 0 {
			t.Fatalf("rejected stale segment changed state: main=%s snap=%+v", main.Target, snap)
		}
	})
}

func TestFSStoreApplyJoinRejectsLossyMutationShape(t *testing.T) {
	ctx := context.Background()
	st, repo, h, x, _, _ := joinRecoveryFixture(t)
	tip := domain.HashContent([]byte("lossy-shape-tip"))
	if err := st.PutSnapshot(ctx, domain.Snapshot{
		ID: tip, DocHash: tip, RepoID: repo, Branch: "main", Parents: []domain.ContentHash{x},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{
		Kind: domain.RefSession, Name: domain.SessionRefPrefix("main") + "shape", RepoID: repo, Target: tip,
	}, ""); err != nil {
		t.Fatal(err)
	}
	base := outbound.JoinMutation{
		RepoID: repo, Branch: "main", Source: x, Segment: []domain.ContentHash{x, tip},
		ExpectedHead: h, NewHead: x,
		ForkName: domain.SessionRefPrefix("main") + "tip", ForkTip: tip,
		Grafts: []outbound.GraftPatch{{SnapshotID: x, ExpectedSeq: 0, Parents: []domain.ContentHash{h}}},
	}
	foreignParent := domain.HashContent([]byte("foreign-graft-parent"))
	if err := st.PutSnapshot(ctx, domain.Snapshot{
		ID: foreignParent, DocHash: foreignParent, RepoID: repo, Branch: "feature",
	}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(outbound.JoinMutation) outbound.JoinMutation
	}{
		{"partial join without residual ref", func(m outbound.JoinMutation) outbound.JoinMutation {
			m.ForkName, m.ForkTip = "", ""
			return m
		}},
		{"session ref outside target branch scope", func(m outbound.JoinMutation) outbound.JoinMutation {
			m.ForkName = domain.SessionRefPrefix("main/topic") + "tip"
			return m
		}},
		{"source patch drops previous head", func(m outbound.JoinMutation) outbound.JoinMutation {
			m.Grafts[0].Parents = nil
			return m
		}},
		{"source patch imports parent outside target branch scope", func(m outbound.JoinMutation) outbound.JoinMutation {
			m.Grafts[0].Parents = append(m.Grafts[0].Parents, foreignParent)
			return m
		}},
		{"disconnected natural segment", func(m outbound.JoinMutation) outbound.JoinMutation {
			disconnected := domain.HashContent([]byte("disconnected-tip"))
			if err := st.PutSnapshot(ctx, domain.Snapshot{ID: disconnected, DocHash: disconnected, RepoID: repo, Branch: "main"}); err != nil {
				t.Fatal(err)
			}
			if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{
				Kind: domain.RefSession, Name: domain.SessionRefPrefix("main") + "disconnected", RepoID: repo, Target: disconnected,
			}, ""); err != nil {
				t.Fatal(err)
			}
			m.Segment[1], m.ForkTip = disconnected, disconnected
			return m
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mutation := base
			mutation.Segment = append([]domain.ContentHash{}, base.Segment...)
			mutation.Grafts = append([]outbound.GraftPatch{}, base.Grafts...)
			for i := range mutation.Grafts {
				mutation.Grafts[i].Parents = append([]domain.ContentHash{}, base.Grafts[i].Parents...)
			}
			err := st.ApplyJoin(ctx, tt.mutate(mutation))
			if !errors.Is(err, domain.ErrValidation) && !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("err=%v, want validation/conflict", err)
			}
			main, _ := st.GetRef(ctx, repo, domain.RefBranch, "main")
			snap, _ := st.GetSnapshot(ctx, repo, x)
			if main.Target != h || snap.GraftSeq != 0 || len(snap.GraftParents) != 0 {
				t.Fatalf("rejected lossy plan changed state: main=%s snap=%+v", main.Target, snap)
			}
		})
	}
}

func TestFSStoreApplyJoinRejectsCycleInIntermediatePatchOrder(t *testing.T) {
	ctx := context.Background()
	st, repo, h, x, _, _ := joinRecoveryFixture(t)
	if err := st.AddGraftParents(ctx, repo, h, []domain.ContentHash{x}); err != nil {
		t.Fatal(err)
	}
	// The final graph removes H's X edge and adds X→H, so it's a DAG, but if X→H is written first in this order,
	// a temporary H↔X cycle occurs. The repository must reject writes before applying them if it doesn't strictly follow the service patch order.
	err := st.ApplyJoin(ctx, outbound.JoinMutation{
		RepoID: repo, Branch: "main", Source: x, Segment: []domain.ContentHash{x}, ExpectedHead: h, NewHead: x,
		Grafts: []outbound.GraftPatch{
			{SnapshotID: x, ExpectedSeq: 0, Parents: []domain.ContentHash{h}},
			{SnapshotID: h, ExpectedSeq: 1, Parents: nil},
		},
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("err=%v, want ErrConflict", err)
	}
	main, _ := st.GetRef(ctx, repo, domain.RefBranch, "main")
	hSnap, _ := st.GetSnapshot(ctx, repo, h)
	xSnap, _ := st.GetSnapshot(ctx, repo, x)
	if main.Target != h || hSnap.GraftSeq != 1 || len(hSnap.GraftParents) != 1 || hSnap.GraftParents[0] != x || xSnap.GraftSeq != 0 {
		t.Fatalf("rejected intermediate cycle changed state: main=%s h=%+v x=%+v", main.Target, hSnap, xSnap)
	}
	if _, statErr := os.Stat(st.joinJournalPath(repo)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rejected intermediate cycle left journal: %v", statErr)
	}
}

func TestFSStorePutSnapshotPromotionDoesNotLoseConcurrentGraft(t *testing.T) {
	ctx := context.Background()
	st, repo, h, x, _, _ := joinRecoveryFixture(t)
	stash := domain.Snapshot{ID: x, DocHash: x, RepoID: repo, Branch: domain.StashBranchLabel, Message: "WIP"}
	// joinRecoveryFixture first saved the main label, so it creates a stash-first state with a separate ID.
	x = domain.HashContent([]byte("stash-promotion-with-graft"))
	stash.ID, stash.DocHash = x, x
	if err := st.PutSnapshot(ctx, stash); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		if err := st.PutSnapshot(ctx, domain.Snapshot{ID: x, DocHash: x, RepoID: repo, Branch: "main", Message: "commit"}); err != nil {
			t.Errorf("promote: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		if err := st.AddGraftParents(ctx, repo, x, []domain.ContentHash{h}); err != nil {
			t.Errorf("graft: %v", err)
		}
	}()
	close(start)
	wg.Wait()
	got, err := st.GetSnapshot(ctx, repo, x)
	if err != nil || got.Branch != "main" || got.Message != "commit" || got.GraftSeq != 1 || len(got.GraftParents) != 1 || got.GraftParents[0] != h {
		t.Fatalf("concurrent promotion/graft lost metadata: %+v err=%v", got, err)
	}
}
