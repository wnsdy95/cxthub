//go:build postgres

package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func projectionCachePGConnection(t *testing.T) *pgx.Conn {
	t.Helper()
	connection, err := pgx.Connect(systemTestContext(), os.Getenv("CXT_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(context.Background()) })
	return connection
}

func TestPGProjectionCachesSourceOnlyMutationsAndEpochReset(t *testing.T) {
	service, storage, repo := collaborationPG(t)
	ctx := systemTestContext()
	connection := projectionCachePGConnection(t)
	first := collaborationSnapshot(t, storage, repo, "first")
	second := collaborationSnapshot(t, storage, repo, "second")
	ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: first, BranchID: "main-id"}
	if err := storage.CompareAndSwapRef(ctx, repo, ref, ""); err != nil {
		t.Fatal(err)
	}
	if err := storage.AdvanceRepositoryRevision(ctx, repo, false); err != nil {
		t.Fatal(err)
	}
	in := domain.ContextSelection{SegmentLimit: 10}
	before, err := service.QueryContext(ctx, repo, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.QueryGraphState(ctx, repo, ""); err != nil {
		t.Fatal(err)
	}
	if service.segmentCache.Stats().Entries == 0 || service.branchCache.Stats().Entries != 1 || service.graphCache.Stats().Entries != 1 {
		t.Fatal("production graph source did not populate all process caches")
	}
	mutations := []struct {
		name string
		run  func() error
	}{
		{"staging", func() error { collaborationSnapshot(t, storage, repo, "unpublished"); return nil }},
		{"graft", func() error { return storage.AddGraftParents(ctx, repo, first, []domain.ContentHash{second}) }},
		{"snapshot metadata", func() error {
			_, err := connection.Exec(ctx, `UPDATE snapshots SET message='changed source only' WHERE repo_id=$1 AND id=$2`, repo, first)
			return err
		}},
		{"standalone reflog", func() error {
			_, err := connection.Exec(ctx, `INSERT INTO reflog(repo_id,kind,name,old,new,created_at) VALUES($1,'branch','topic',$2,$2,$3)`, repo, second, time.Unix(100, 0).UTC())
			return err
		}},
		{"ref", func() error {
			ref.Target = second
			return storage.CompareAndSwapRef(ctx, repo, ref, first)
		}},
		{"history", func() error {
			return storage.ApplyHistoryEvent(ctx, domain.HistoryEvent{ID: fmt.Sprintf("%032x", 1), RepoID: string(repo), Branch: "main", BranchID: ref.BranchID, Kind: "position", Source: second, Target: second, GitAfter: effectiveOID(3), CreatedAt: time.Unix(101, 0).UTC()})
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			oldSource := service.graphCache.Keys()[0].Source
			if err := mutation.run(); err != nil {
				t.Fatal(err)
			}
			current, err := service.QueryContext(ctx, repo, in)
			if err != nil || current.Revision != before.Revision {
				t.Fatalf("source-only mutation changed public revisions: %+v %v", current.Revision, err)
			}
			graph, err := service.QueryGraphState(ctx, repo, "")
			if err != nil || graph.Revision != before.Revision {
				t.Fatalf("source-only graph read failed: %+v %v", graph.Revision, err)
			}
			newSource := service.graphCache.Keys()[0].Source
			if newSource == oldSource || service.branchCache.Keys()[0].Generation != newSource {
				t.Fatal("source-only mutation reused an outer or branch generation")
			}
			fresh := NewService(storage, storage, nil, nil, nil)
			want, err := fresh.QueryContext(ctx, repo, in)
			if err != nil || want.StateHash != current.StateHash || want.Segments.PageHash != current.Segments.PageHash {
				t.Fatalf("warm context disagrees with cold source: %v", err)
			}
			wantGraph, err := fresh.QueryGraphState(ctx, repo, "")
			actualBytes, _ := json.Marshal(graph)
			expectedBytes, _ := json.Marshal(wantGraph)
			if err != nil || string(actualBytes) != string(expectedBytes) {
				t.Fatalf("warm graph disagrees with cold source: %v", err)
			}
			if current.StateHash != before.StateHash {
				stale := in
				stale.SegmentStateHash = before.StateHash
				if _, err := service.QueryContext(ctx, repo, stale); !errors.Is(err, domain.ErrConflict) {
					t.Fatalf("stale continuation accepted: %v", err)
				}
			}
			before = current
		})
	}
	if _, err := connection.Exec(ctx, `SELECT cxt_reset_catalog($1)`, repo); err != nil {
		t.Fatal(err)
	}
	before, err = service.QueryContext(ctx, repo, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.QueryGraphState(ctx, repo, ""); err != nil {
		t.Fatal(err)
	}
	oldSource := service.graphCache.Keys()[0].Source
	transaction, err := connection.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback(ctx)
	if _, err := transaction.Exec(ctx, `UPDATE snapshots SET message='new incarnation' WHERE repo_id=$1 AND id=$2`, repo, first); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Exec(ctx, `SELECT cxt_reset_catalog($1)`, repo); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	stale := in
	stale.SegmentStateHash = before.StateHash
	if _, err := service.QueryContext(ctx, repo, stale); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("same-sequence new epoch accepted stale continuation: %v", err)
	}
	if _, err := service.QueryGraphState(ctx, repo, ""); err != nil {
		t.Fatal(err)
	}
	newSource := service.graphCache.Keys()[0].Source
	if oldSource.Sequence != newSource.Sequence || oldSource.Epoch == newSource.Epoch || service.branchCache.Keys()[0].Generation != newSource {
		t.Fatalf("epoch reset fence: before=%+v after=%+v", oldSource, newSource)
	}
}

func TestPGProjectionCachesRefreshEvidenceOutsideViewFingerprint(t *testing.T) {
	service, storage, repo := collaborationPG(t)
	ctx := systemTestContext()
	connection := projectionCachePGConnection(t)
	base := collaborationSnapshot(t, storage, repo, "base")
	source := collaborationSnapshot(t, storage, repo, "merged source")
	target := collaborationSnapshot(t, storage, repo, "continued main")
	ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", BranchID: "main-id", Target: target}
	if err := storage.CompareAndSwapRef(ctx, repo, ref, ""); err != nil {
		t.Fatal(err)
	}
	receipt := domain.HistoryEvent{ID: fmt.Sprintf("%032x", 10), RepoID: string(repo), Branch: "main", BranchID: ref.BranchID, SourceBranchID: "feature-id", Kind: "pr-merge", Source: source, Target: source, PR: &domain.PullRequestMerge{Number: 10, BaseBranch: "main", HeadBranch: "feature", HeadSHA: effectiveOID(8), MergeSHA: effectiveOID(2)}, CreatedAt: time.Unix(100, 0).UTC()}
	completion := receipt
	completionID := sha256.Sum256([]byte(receipt.ID + ":completed"))
	completion.ID, completion.PRCompleted, completion.SharedTarget = fmt.Sprintf("%x", completionID[:16]), true, base
	position := domain.HistoryEvent{ID: fmt.Sprintf("%032x", 11), RepoID: string(repo), Branch: "main", BranchID: ref.BranchID, Kind: "position", Source: target, Target: target, GitAfter: effectiveOID(3), CreatedAt: time.Unix(101, 0).UTC()}
	for _, event := range []domain.HistoryEvent{receipt, completion, position} {
		if err := storage.ApplyHistoryEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.AdvanceRepositoryRevision(ctx, repo, false); err != nil {
		t.Fatal(err)
	}
	in := domain.ContextSelection{Branch: "main", Position: "main", Scope: "current", CodeCommit: effectiveOID(3), SegmentLimit: 10}
	before, err := service.QueryContext(ctx, repo, in)
	if err != nil || before.Segments.Total != 1 {
		t.Fatalf("missing-evidence context: %+v %v", before, err)
	}
	graph, err := service.QueryGraphState(ctx, repo, "")
	if err != nil || len(graph.BranchContexts["main"].SnapshotIDs) != 1 {
		t.Fatalf("missing-evidence branch: %+v %v", graph.BranchContexts, err)
	}
	oldKey := service.graphCache.Keys()[0]
	metadata, err := storage.GetRepo(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	transaction, err := connection.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback(ctx)
	for number := 1; number <= 3; number++ {
		commit := domain.GitCommitTree{Commit: effectiveOID(number), Tree: effectiveOID(100), Parents: []string{}}
		if number > 1 {
			commit.Parents = []string{effectiveOID(number - 1)}
		}
		payload, err := json.Marshal(commit)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := transaction.Exec(ctx, `INSERT INTO git_commit_trees(repo_id,origin,commit_oid,payload) VALUES($1,$2,$3,$4)`, repo, metadata.GitRemoteURL, commit.Commit, payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	graph, err = service.QueryGraphState(ctx, repo, "")
	if err != nil || len(graph.BranchContexts["main"].SnapshotIDs) != 3 || graph.Revision != before.Revision {
		t.Fatalf("late Git evidence reused branch: %+v %v", graph.BranchContexts, err)
	}
	newKey := service.graphCache.Keys()[0]
	if oldKey.Inputs != newKey.Inputs || oldKey.Revision != newKey.Revision || oldKey.Source == newKey.Source || service.branchCache.Keys()[0].Generation != newKey.Source {
		t.Fatalf("outer cache did not fence evidence outside fingerprint: before=%+v after=%+v", oldKey, newKey)
	}
	stale := in
	stale.SegmentStateHash = before.StateHash
	if _, err := service.QueryContext(ctx, repo, stale); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("late Git evidence accepted stale context continuation: %v", err)
	}
	after, err := service.QueryContext(ctx, repo, in)
	if err != nil || after.Segments.Total != 3 || after.Revision != before.Revision {
		t.Fatalf("late Git evidence did not refresh current selection: %+v %v", after, err)
	}
	if _, err := connection.Exec(ctx, `DELETE FROM git_commit_trees WHERE repo_id=$1 AND commit_oid=$2`, repo, effectiveOID(3)); err != nil {
		t.Fatal(err)
	}
	graph, err = service.QueryGraphState(ctx, repo, "")
	if err != nil || len(graph.BranchContexts["main"].SnapshotIDs) != 1 {
		t.Fatalf("removed evidence retained cached inclusion: %+v %v", graph.BranchContexts, err)
	}
}
