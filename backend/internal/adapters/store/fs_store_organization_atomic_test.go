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

func TestFSOrganizationMutationRollsBackWhenAuditCannotPersist(t *testing.T) {
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
	organization := domain.Organization{
		ID: domain.NewID("ent_"), NamespaceID: domain.NewID("ns_"), Name: "Atomic",
		Slug: "atomic", CreatedBy: owner.ID, CreatedAt: now,
	}
	namespace := domain.Namespace{
		ID: organization.NamespaceID, Slug: organization.Slug, Kind: domain.NamespaceOrganization,
		OrganizationID: organization.ID, CreatedAt: now,
	}
	ownerMembership := domain.OrganizationMembership{
		OrganizationID: organization.ID, UserID: owner.ID, Role: domain.OrganizationOwner, CreatedAt: now,
	}
	policy := domain.DefaultOrganizationPolicy(organization.ID)
	policy.UpdatedBy, policy.UpdatedAt = owner.ID, now
	createdAudit := domain.OrganizationAuditEvent{
		ID: domain.NewID("aud_"), OrganizationID: organization.ID, ActorID: owner.ID,
		Action: "organization.created", TargetType: "organization", TargetID: organization.ID, CreatedAt: now,
	}
	if err := st.CreateOrganization(ctx, organization, namespace, ownerMembership, policy, createdAudit); err != nil {
		t.Fatal(err)
	}
	demotedOnlyOwner := ownerMembership
	demotedOnlyOwner.Role = domain.OrganizationAdmin
	if err := st.AddOrganizationMember(ctx, demotedOnlyOwner); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("FS store allowed the last Organization owner to be demoted: %v", err)
	}
	if err := st.RemoveOrganizationMember(ctx, organization.ID, owner.ID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("FS store allowed the last Organization owner to be removed: %v", err)
	}

	// Replace the audit directory with a file so every subsequent audit append
	// fails after the mutation's first write. The atomic adapter must restore
	// the exact pre-mutation state before returning the error.
	auditDir := filepath.Join(st.organizationAuditDir(), organization.ID)
	if err := os.RemoveAll(auditDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auditDir, []byte("block audit writes"), 0o600); err != nil {
		t.Fatal(err)
	}

	member := domain.OrganizationMembership{
		OrganizationID: organization.ID, UserID: memberUser.ID, Role: domain.OrganizationMember, CreatedAt: now,
	}
	memberAudit := domain.OrganizationAuditEvent{
		ID: domain.NewID("aud_"), OrganizationID: organization.ID, ActorID: owner.ID,
		Action: "organization.member.updated", TargetType: "user", TargetID: memberUser.ID, CreatedAt: now,
	}
	if err := st.AddOrganizationMemberWithAudit(ctx, member, memberAudit); err == nil {
		t.Fatal("member mutation unexpectedly succeeded with unavailable audit storage")
	}
	if _, err := st.GetOrganizationMembership(ctx, organization.ID, memberUser.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("member mutation was not rolled back: %v", err)
	}

	repository := domain.Repository{
		ID: domain.NewID("ws_"), Name: "Atomic Repository", OwnerID: owner.ID,
		OwnerUsername: organization.Slug, OwnerNamespaceID: organization.NamespaceID,
		Slug: "atomic-repository", Visibility: domain.VisibilityPrivate, CreatedAt: now,
	}
	repositoryOwner := domain.Membership{
		RepositoryID: repository.ID, UserID: owner.ID, Role: domain.RoleOwner, CreatedAt: now,
	}
	repositoryAudit := domain.OrganizationAuditEvent{
		ID: domain.NewID("aud_"), OrganizationID: organization.ID, ActorID: owner.ID,
		Action: "organization.repository.created", TargetType: "repository", TargetID: repository.ID, CreatedAt: now,
	}
	if err := st.CreateOrganizationRepositoryWithAudit(ctx, repository, repositoryOwner, repositoryAudit); err == nil {
		t.Fatal("Repository mutation unexpectedly succeeded with unavailable audit storage")
	}
	if _, err := st.GetRepository(ctx, repository.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Repository mutation was not rolled back: %v", err)
	}
	if member, err := st.IsMember(ctx, repository.ID, owner.ID); err != nil || member {
		t.Fatalf("Repository owner membership was not rolled back: member=%v err=%v", member, err)
	}
}
