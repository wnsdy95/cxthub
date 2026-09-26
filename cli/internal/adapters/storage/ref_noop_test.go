package storage

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestRepeatedPullRefsAvoidReplacementWithoutBypassingLifecycle(t *testing.T) {
	ctx := context.Background()
	s := NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte(t.Name())))
	first, second := domain.HashContent([]byte("first")), domain.HashContent([]byte("second"))
	for kind, dir := range map[domain.RefKind]string{domain.RefBranch: "heads", domain.RefSession: "sessions", domain.RefTag: "tags"} {
		ref := domain.Ref{RepoID: repo, Kind: kind, Name: "example", Target: first}
		if err := s.PutRef(ctx, ref); err != nil {
			t.Fatal(err)
		}
		path := s.refPath(dir, ref.Name)
		old := time.Unix(100, 0)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.PutRef(ctx, ref); err != nil {
			t.Fatal(err)
		}
		after, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(before, after) || !after.ModTime().Equal(old) {
			t.Fatal("equal ref rewritten")
		}
		ref.Target = second
		if err := s.PutRef(ctx, ref); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetRef(ctx, repo, kind, ref.Name)
		if err != nil || got.Target != second {
			t.Fatal("changed ref not persisted", got, err)
		}
	}
	archived, err := domain.NewBranchLifecycleRef(repo, "example", second, 1, domain.BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutRef(ctx, archived); err != nil {
		t.Fatal(err)
	}
	// The physical branch file is identical, but the logical branch is archived.
	if err := s.PutRef(ctx, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "example", Target: second}); !errors.Is(err, domain.ErrBranchArchived) {
		t.Fatal("equality bypassed lifecycle policy", err)
	}
}

func TestMatchingTagRefsChecksCurrentValuesAndRejectsBranches(t *testing.T) {
	ctx := context.Background()
	s := NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte(t.Name())))
	a, b := domain.HashContent([]byte("a")), domain.HashContent([]byte("b"))
	first := domain.Ref{RepoID: repo, Kind: domain.RefTag, Name: "retained", Target: a}
	missing := first
	missing.Name = "absent"
	changed := first
	changed.Name = "changed"
	if err := s.PutRef(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRef(ctx, changed); err != nil {
		t.Fatal(err)
	}
	changed.Target = b
	matched, err := s.MatchingTagRefs(ctx, []domain.Ref{first, missing, changed})
	if err != nil || len(matched) != 1 || !matched[first.Name] {
		t.Fatal(matched, err)
	}
	// A later writer is never overwritten by the completed no-op recognition.
	first.Target = b
	if err := s.PutRef(ctx, first); err != nil {
		t.Fatal(err)
	}
	matched, err = s.MatchingTagRefs(ctx, []domain.Ref{first})
	if err != nil || !matched[first.Name] {
		t.Fatal(matched, err)
	}
	first.Kind = domain.RefBranch
	if _, err := s.MatchingTagRefs(ctx, []domain.Ref{first}); !errors.Is(err, domain.ErrInvalidRef) {
		t.Fatal("accepted branch", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.MatchingTagRefs(ctx, []domain.Ref{missing}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored", err)
	}
}
