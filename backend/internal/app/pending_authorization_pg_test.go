//go:build postgres

package app

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestPGRepoProfileRejectsPartialPatch(t *testing.T) {
	svc, st, repo := collaborationPG(t)
	ctx := systemTestContext()
	old, err := st.GetRepo(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	branch, badSite := "changed", "javascript:alert(1)"
	if _, err = svc.PatchRepoProfile(ctx, repo, inbound.RepoProfilePatch{DefaultBranch: &branch, Website: &badSite}); err == nil {
		t.Fatal("invalid website accepted")
	}
	after, err := st.GetRepo(ctx, repo)
	if err != nil || after.DefaultBranch != old.DefaultBranch {
		t.Fatalf("partial configuration write: %+v %v", after, err)
	}
}
