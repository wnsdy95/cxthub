package store

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"sync"
	"testing"
)

func TestAtomicAppendConflictLeavesNoLosingGraft(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte("repo"))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	base, main, a, b := domain.HashContent([]byte("base")), domain.HashContent([]byte("main")), domain.HashContent([]byte("a")), domain.HashContent([]byte("b"))
	for _, id := range []domain.ContentHash{base, main, a, b} {
		snap := domain.Snapshot{ID: id, DocHash: id, RepoID: repo}
		if id != base {
			snap.Parents = []domain.ContentHash{base}
		}
		if err := st.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: main}, ""); err != nil {
		t.Fatal(err)
	}
	type result struct {
		id  domain.ContentHash
		err error
	}
	out := make(chan result, 2)
	var wg sync.WaitGroup
	for _, id := range []domain.ContentHash{a, b} {
		wg.Add(1)
		go func(id domain.ContentHash) {
			defer wg.Done()
			out <- result{id, st.AppendRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: id}, main)}
		}(id)
	}
	wg.Wait()
	close(out)
	success := 0
	for r := range out {
		snap, err := st.GetSnapshot(ctx, repo, r.id)
		if err != nil {
			t.Fatal(err)
		}
		if r.err == nil {
			success++
			if len(snap.GraftParents) != 1 || snap.GraftParents[0] != main {
				t.Fatal("winner lost old head")
			}
		} else {
			if !errors.Is(r.err, domain.ErrRefConflict) {
				t.Fatal(r.err)
			}
			if len(snap.GraftParents) != 0 || snap.GraftSeq != 0 {
				t.Fatal("rejected append mutated graph")
			}
		}
	}
	if success != 1 {
		t.Fatalf("successes=%d", success)
	}
	if _, err := OpenFSStore(st.dataDir); err != nil {
		t.Fatalf("restart recovery: %v", err)
	}
}
