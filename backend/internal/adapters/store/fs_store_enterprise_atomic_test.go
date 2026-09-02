package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestFSEnterpriseMutationRollsBackWhenAuditCannotPersist(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	now := time.Now().UTC()
	owner := domain.User{ID: "atomic-owner", Email: "owner@example.test", Name: "Owner", Username: "atomic-owner"}
	memberUser := domain.User{ID: "atomic-member", Email: "member@example.test", Name: "Member", Username: "atomic-member"}
	for _, user := range []domain.User{owner, memberUser} {
		if err := st.UpsertUser(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	enterprise := domain.Enterprise{
		ID: domain.NewID("ent_"), NamespaceID: domain.NewID("ns_"), Name: "Atomic",
		Slug: "atomic", CreatedBy: owner.ID, CreatedAt: now,
	}
	namespace := domain.Namespace{
		ID: enterprise.NamespaceID, Slug: enterprise.Slug, Kind: domain.NamespaceEnterprise,
		EnterpriseID: enterprise.ID, CreatedAt: now,
	}
	ownerMembership := domain.EnterpriseMembership{
		EnterpriseID: enterprise.ID, UserID: owner.ID, Role: domain.EnterpriseOwner, CreatedAt: now,
	}
	policy := domain.DefaultEnterprisePolicy(enterprise.ID)
	policy.UpdatedBy, policy.UpdatedAt = owner.ID, now
	createdAudit := domain.EnterpriseAuditEvent{
		ID: domain.NewID("aud_"), EnterpriseID: enterprise.ID, ActorID: owner.ID,
		Action: "enterprise.created", TargetType: "enterprise", TargetID: enterprise.ID, CreatedAt: now,
	}
	if err := st.CreateEnterprise(ctx, enterprise, namespace, ownerMembership, policy, createdAudit); err != nil {
		t.Fatal(err)
	}
	demotedOnlyOwner := ownerMembership
	demotedOnlyOwner.Role = domain.EnterpriseAdmin
	if err := st.AddEnterpriseMember(ctx, demotedOnlyOwner); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("FS store allowed the last Enterprise owner to be demoted: %v", err)
	}
	if err := st.RemoveEnterpriseMember(ctx, enterprise.ID, owner.ID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("FS store allowed the last Enterprise owner to be removed: %v", err)
	}

	// Replace the audit directory with a file so every subsequent audit append
	// fails after the mutation's first write. The atomic adapter must restore
	// the exact pre-mutation state before returning the error.
	auditDir := filepath.Join(st.enterpriseAuditDir(), enterprise.ID)
	if err := os.RemoveAll(auditDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auditDir, []byte("block audit writes"), 0o600); err != nil {
		t.Fatal(err)
	}

	member := domain.EnterpriseMembership{
		EnterpriseID: enterprise.ID, UserID: memberUser.ID, Role: domain.EnterpriseMember, CreatedAt: now,
	}
	memberAudit := domain.EnterpriseAuditEvent{
		ID: domain.NewID("aud_"), EnterpriseID: enterprise.ID, ActorID: owner.ID,
		Action: "enterprise.member.updated", TargetType: "user", TargetID: memberUser.ID, CreatedAt: now,
	}
	if err := st.AddEnterpriseMemberWithAudit(ctx, member, memberAudit); err == nil {
		t.Fatal("member mutation unexpectedly succeeded with unavailable audit storage")
	}
	if _, err := st.GetEnterpriseMembership(ctx, enterprise.ID, memberUser.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("member mutation was not rolled back: %v", err)
	}

	workspace := domain.Workspace{
		ID: domain.NewID("ws_"), Name: "Atomic Workspace", OwnerID: owner.ID,
		OwnerUsername: enterprise.Slug, OwnerNamespaceID: enterprise.NamespaceID,
		Slug: "atomic-workspace", Visibility: domain.VisibilityPrivate, CreatedAt: now,
	}
	workspaceOwner := domain.Membership{
		WorkspaceID: workspace.ID, UserID: owner.ID, Role: domain.RoleOwner, CreatedAt: now,
	}
	workspaceAudit := domain.EnterpriseAuditEvent{
		ID: domain.NewID("aud_"), EnterpriseID: enterprise.ID, ActorID: owner.ID,
		Action: "enterprise.workspace.created", TargetType: "workspace", TargetID: workspace.ID, CreatedAt: now,
	}
	if err := st.CreateEnterpriseWorkspaceWithAudit(ctx, workspace, workspaceOwner, workspaceAudit); err == nil {
		t.Fatal("Workspace mutation unexpectedly succeeded with unavailable audit storage")
	}
	if _, err := st.GetWorkspace(ctx, workspace.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Workspace mutation was not rolled back: %v", err)
	}
	if member, err := st.IsMember(ctx, workspace.ID, owner.ID); err != nil || member {
		t.Fatalf("Workspace owner membership was not rolled back: member=%v err=%v", member, err)
	}
}
