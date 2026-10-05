package storage

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestRemoteObservationBranchScopeKeepsRepairBaseline(t *testing.T) {
	ctx := context.Background()
	st := NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte("scope repository")))
	full, err := st.ReadRemoteObservation(ctx, repo, "origin")
	if err != nil {
		t.Fatal(err)
	}
	full.Refs = []domain.Ref{{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: domain.HashContent([]byte("full"))}}
	if err := st.CompareAndSwapRemoteObservation(ctx, "", full); err != nil {
		t.Fatal(err)
	}
	baseline, err := st.ReadRemoteObservation(ctx, repo, "origin")
	if err != nil {
		t.Fatal(err)
	}
	for _, branch := range []string{"main", "feature/x"} {
		scope, err := st.ReadScopedRemoteObservation(ctx, repo, "origin", branch)
		if err != nil {
			t.Fatal(err)
		}
		if scope.Revision != "" || len(scope.Refs) != 0 || scope.Branch != branch {
			t.Fatalf("scope inherited another observation: %+v", scope)
		}
		scope.Refs = []domain.Ref{{RepoID: repo, Kind: domain.RefBranch, Name: branch, Target: domain.HashContent([]byte(branch))}}
		if err := st.CompareAndSwapRemoteObservation(ctx, "", scope); err != nil {
			t.Fatal(err)
		}
		if err := st.CompareAndSwapRemoteObservation(ctx, "", scope); !errors.Is(err, domain.ErrSyncConflict) {
			t.Fatalf("stale scoped CAS: %v", err)
		}
		if err := st.CompareAndSwapRemoteObservation(ctx, baseline.Revision, scope); !errors.Is(err, domain.ErrSyncConflict) {
			t.Fatalf("full revision accepted for branch: %v", err)
		}
	}
	got, err := st.ReadRemoteObservation(ctx, repo, "origin")
	if err != nil || got.Revision != baseline.Revision || got.Refs[0].Target != baseline.Refs[0].Target {
		t.Fatalf("repair baseline changed: %+v, %v", got, err)
	}
	main, err := st.ReadScopedRemoteObservation(ctx, repo, "origin", "main")
	if err != nil || main.Refs[0].Target != domain.HashContent([]byte("main")) {
		t.Fatalf("other branch replaced main: %+v, %v", main, err)
	}
	other, err := st.ReadScopedRemoteObservation(ctx, repo, "upstream", "main")
	if err != nil || other.Revision != "" {
		t.Fatalf("endpoint isolation: %+v, %v", other, err)
	}
}

func TestRemoteObservationBranchScopeRejectsCopiedAndInvalidRecords(t *testing.T) {
	ctx := context.Background()
	st := NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte("scope integrity")))
	a, err := st.ReadScopedRemoteObservation(ctx, repo, "origin", "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CompareAndSwapRemoteObservation(ctx, "", a); err != nil {
		t.Fatal(err)
	}
	ap, _ := st.scopedRemoteObservationPath(repo, "origin", "a")
	raw, err := os.ReadFile(ap)
	if err != nil {
		t.Fatal(err)
	}
	for _, branch := range []string{"", "b"} {
		path, _ := st.scopedRemoteObservationPath(repo, "origin", branch)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := st.readRemoteObservation(ctx, repo, "origin", branch); !errors.Is(err, domain.ErrHashMismatch) {
			t.Fatalf("accepted copied scope %q: %v", branch, err)
		}
	}
	for _, branch := range []string{"", "../other", "a\x00b"} {
		if _, err := st.ReadScopedRemoteObservation(ctx, repo, "origin", branch); !errors.Is(err, domain.ErrInvalidRef) {
			t.Fatalf("invalid scope %q: %v", branch, err)
		}
	}
	legacy, _ := st.remoteObservationPath(repo, "origin")
	want := domain.HashContent([]byte(repo + "\x00origin"))
	if legacy != st.storeDir()+"/remote-observations/"+hexOf(want)+".json" {
		t.Fatal("full observation path changed")
	}
}
