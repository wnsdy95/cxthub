package app

import (
	"errors"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"testing"
)

func TestSyncRefSelectionKeepsOnlyRequestedPointerAndLifecycle(t *testing.T) {
	repo := string(domain.HashContent([]byte("scoped-sync")))
	h := domain.HashContent([]byte("source"))
	birth, err := domain.NewBranchLifecycleRef(repo, "feature", h, 1, domain.BranchActive)
	if err != nil {
		t.Fatal(err)
	}
	refs := []domain.Ref{{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: h}, {Kind: domain.RefBranch, Name: "feature", RepoID: repo, Target: h}, birth, {Kind: domain.RefTag, Name: "tag", RepoID: repo, Target: h}}
	got, err := selectSyncRefs(refs, "feature")
	if err != nil || len(got) != 2 || got[0].Name != "feature" || got[1] != birth {
		t.Fatalf("scoped refs: %+v %v", got, err)
	}
	if _, err = selectSyncRefs(refs, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown branch silently ignored: %v", err)
	}
	if _, err = selectSyncRefs(refs, "main:other"); !errors.Is(err, domain.ErrInvalidRef) {
		t.Fatalf("refspec silently ignored: %v", err)
	}
}
