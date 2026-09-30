package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func contextWriteFS(t *testing.T) (*FSStore, domain.Ref) {
	t.Helper()
	s := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	if _, err := s.PutRepo(context.Background(), domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableContextProtocol(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	return s, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", BranchID: "identity", Target: domain.HashContent([]byte("target"))}
}

func TestFSContextWriteSkipsUnusedState(t *testing.T) {
	for _, failedRead := range []string{"history", "current-ref"} {
		t.Run(failedRead, func(t *testing.T) {
			s, branch := contextWriteFS(t)
			ctx := context.Background()
			if failedRead == "history" {
				// A file where ReadDir expects a directory fails independently of
				// permissions (including when tests run as root).
				if err := os.WriteFile(filepath.Join(s.repoDir(branch.RepoID), "history"), []byte("unreadable history"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for _, kind := range []domain.RefKind{domain.RefTag, domain.RefSession, domain.RefHead, domain.RefBranch} {
				ref := branch
				ref.Kind = kind
				if kind == domain.RefHead {
					ref.Name = domain.HeadRefName
				}
				if failedRead == "current-ref" {
					if err := os.MkdirAll(s.refFile(ref.RepoID, ref.Kind, ref.Name), 0700); err != nil {
						t.Fatal(err)
					}
				}
				err := s.validateContextWrite(ctx, ref.RepoID, ref)
				if kind == domain.RefBranch {
					var pathErr *os.PathError
					if !errors.As(err, &pathErr) {
						t.Fatalf("branch must still read %s: %v", failedRead, err)
					}
				} else if err != nil {
					t.Fatalf("%s read unused %s: %v", kind, failedRead, err)
				}
			}
			branch.BranchID = ""
			if err := s.validateContextWrite(ctx, branch.RepoID, branch); !errors.Is(err, domain.ErrConflict) {
				t.Fatal("missing identity must fail before reads", err)
			}
			// Even nonbranches must read and reject an invalid protocol marker.
			if err := os.WriteFile(filepath.Join(s.repoDir(branch.RepoID), "context-protocol"), []byte("2\n"), 0600); err != nil {
				t.Fatal(err)
			}
			branch.Kind = domain.RefTag
			if err := s.validateContextWrite(ctx, branch.RepoID, branch); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatal("invalid protocol marker ignored", err)
			}
		})
	}
}

func TestFSContextWriteCASAndRecovery(t *testing.T) {
	s, tag := contextWriteFS(t)
	ctx := context.Background()
	tag.Kind, tag.BranchID = domain.RefTag, ""
	history := filepath.Join(s.repoDir(tag.RepoID), "history")
	if err := os.WriteFile(history, []byte("unreadable history"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.CompareAndSwapRef(ctx, tag.RepoID, tag, ""); err != nil {
		t.Fatal("nonbranch CAS loaded history", err)
	}
	if err := s.CompareAndSwapRef(ctx, tag.RepoID, tag, ""); !errors.Is(err, domain.ErrRefConflict) {
		t.Fatal("create CAS lost", err)
	}
	next := tag
	next.Target = domain.HashContent([]byte("next target"))
	if err := s.CompareAndSwapRef(ctx, tag.RepoID, next, next.Target); !errors.Is(err, domain.ErrRefConflict) {
		t.Fatal("update CAS lost", err)
	}
	// Recovery remains ahead of the preflight in the actual mutation path.
	if err := os.WriteFile(s.historyJournal(tag.RepoID), []byte("invalid journal"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.CompareAndSwapRef(ctx, tag.RepoID, next, tag.Target); err == nil {
		t.Fatal("history recovery bypassed")
	}
	if err := os.Remove(s.historyJournal(tag.RepoID)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.joinJournalPath(tag.RepoID), []byte("pending join"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.CompareAndSwapRef(ctx, tag.RepoID, next, tag.Target); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("join recovery guard bypassed", err)
	}
	got, err := s.GetRef(ctx, tag.RepoID, tag.Kind, tag.Name)
	if err != nil || got.Target != tag.Target {
		t.Fatalf("failed write changed ref: %+v %v", got, err)
	}
}
