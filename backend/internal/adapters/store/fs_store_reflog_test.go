package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func rlHash(c byte) domain.ContentHash {
	return domain.ContentHash("sha256:" + strings.Repeat(string(c), 64))
}

// TestReflogRecordsRefMoves verifies that ref creation→move is append-only, returned in latest-first order, and the previous tip (old) is preserved before the move. This old value serves as evidence for recovering tips that cannot be reached via gc/graft (not forced reconnection, but evidence preservation).
func TestReflogRecordsRefMoves(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	repo := rlHash('0')
	tip1, tip2 := rlHash('a'), rlHash('b')

	// New creation(expected="") → old is empty.
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: tip1}, ""); err != nil {
		t.Fatalf("create ref: %v", err)
	}
	// tip1 → tip2 move → old is tip1 (moved tip).
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: tip2}, tip1); err != nil {
		t.Fatalf("move ref: %v", err)
	}

	log, err := st.ReadReflog(ctx, repo)
	if err != nil {
		t.Fatalf("ReadReflog: %v", err)
	}
	if len(log) != 2 {
		t.Fatalf("Expected 2, got %d", len(log))
	}
	// Latest first: move (tip1→tip2) first.
	if log[0].Old != tip1 || log[0].New != tip2 {
		t.Fatalf("Latest item old/new mismatch: old=%s new=%s", log[0].Old, log[0].New)
	}
	if log[0].Name != "main" || log[0].Kind != domain.RefBranch {
		t.Fatalf("Latest item kind/name mismatch: %s/%s", log[0].Kind, log[0].Name)
	}
	// Next: new creation (no old, new=tip1).
	if log[1].Old != "" || log[1].New != tip1 {
		t.Fatalf("Mismatch in created item old/new: old=%s new=%s", log[1].Old, log[1].New)
	}
}

// TestReflogSkipsSymbolicHEAD verifies that symbolic HEAD moves are not recorded in the reflog (only actual lineage tip moves are restored).
func TestReflogSkipsSymbolicHEAD(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	repo := rlHash('1')

	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefHead, Name: "HEAD", RepoID: repo, Symbolic: "refs/heads/main"}, ""); err != nil {
		t.Fatalf("set HEAD: %v", err)
	}
	log, err := st.ReadReflog(ctx, repo)
	if err != nil {
		t.Fatalf("ReadReflog: %v", err)
	}
	if len(log) != 0 {
		t.Fatalf("HEAD move should not be recorded, actual %d", len(log))
	}
}

func TestCompareAndSwapRefIsAtomicWithinProcess(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	first := NewFSStore(dataDir)
	second := NewFSStore(dataDir) // Both instances should share the same dataDir lock.
	repo := rlHash('2')
	base, left, right := rlHash('a'), rlHash('b'), rlHash('c')
	if err := first.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: base}, ""); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	move := func(st *FSStore, target domain.ContentHash) {
		<-start
		results <- st.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: target}, base)
	}
	go move(first, left)
	go move(second, right)
	close(start)

	succeeded, conflicted := 0, 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, domain.ErrRefConflict):
			conflicted++
		default:
			t.Fatalf("unexpected CAS result: %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("CAS outcomes success/conflict = %d/%d", succeeded, conflicted)
	}
	ref, err := first.GetRef(ctx, repo, domain.RefBranch, "main")
	if err != nil || (ref.Target != left && ref.Target != right) {
		t.Fatalf("final ref = %+v, %v", ref, err)
	}
	log, err := first.ReadReflog(ctx, repo)
	if err != nil || len(log) != 2 || log[0].Old != base || log[0].New != ref.Target {
		t.Fatalf("reflog after concurrent CAS = %+v, %v", log, err)
	}
}
