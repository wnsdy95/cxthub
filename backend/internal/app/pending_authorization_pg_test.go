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
	description, badSite := "changed", "javascript:alert(1)"
	if _, err = svc.PatchRepoProfile(ctx, repo, inbound.RepoProfilePatch{Description: &description, Website: &badSite}); err == nil {
		t.Fatal("invalid website accepted")
	}
	after, err := st.GetRepo(ctx, repo)
	if err != nil || after.Description != old.Description {
		t.Fatalf("partial configuration write: %+v %v", after, err)
	}
}
