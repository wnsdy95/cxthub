//go:build postgres

package app

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestPGOrganizationAuditPages(t *testing.T) {
	_, st, _ := collaborationPG(t)
	runOrganizationAuditPages(t, st)
}
func TestPGRepositoryWriteAndNamespaceTransferRequireAudit(t *testing.T) {
	_, st, _ := collaborationPG(t)
	f := makeTeamFixture(t, st)
	ctx := systemTestContext()
	repo := domain.Repo{ID: hh(f.repository.ID), RepositoryID: f.repository.ID, DefaultBranch: "main"}
	if _, err := st.PutRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	broken := failedInvitationAuditStore{st}
	svc := NewService(broken, broken, nil, gitengine.NewEngine(broken), broken)
	actor := inbound.WithCorrelation(inbound.WithRepositoryActor(context.Background(), f.owner.ID), "req_write")
	next := "next"
	if err := svc.UpdateRepoConfig(actor, repo.ID, &next, nil); err == nil {
		t.Fatal("audit failure accepted")
	}
	current, err := st.GetRepo(ctx, repo.ID)
	if err != nil || current.DefaultBranch != "main" {
		t.Fatalf("write escaped rollback: %+v %v", current, err)
	}
	dest, err := f.identity.CreateOrganization(ctx, f.owner, "Other", "audit-"+domain.NewID("")[:8])
	if err != nil {
		t.Fatal(err)
	}
	identity := NewIdentityService(nil, broken)
	if _, err = identity.TransferRepositoryNamespace(ctx, f.owner.ID, f.repository.ID, f.repository.OwnerNamespaceID, f.repository.Slug, dest.Slug); err == nil {
		t.Fatal("transfer accepted audit failure")
	}
	currentRepo, err := st.GetRepository(ctx, f.repository.ID)
	if err != nil || currentRepo.OwnerNamespaceID != f.repository.OwnerNamespaceID {
		t.Fatal("transfer escaped rollback")
	}
	svc = NewService(st, st, nil, gitengine.NewEngine(st), st)
	if err = svc.UpdateRepoConfig(actor, repo.ID, &next, nil); err != nil {
		t.Fatal(err)
	}
	events, err := st.ListOrganizationAudit(ctx, f.organization.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, e := range events {
		if e.Action == "repository.config.updated" {
			found++
			if e.CorrelationID != "req_write" || e.TargetID != f.repository.ID || e.Reason != "" {
				t.Fatalf("bad event %+v", e)
			}
		}
	}
	if found != 1 {
		t.Fatalf("events=%d", found)
	}
}
