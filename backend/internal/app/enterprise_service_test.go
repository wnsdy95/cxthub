package app

import (
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"testing"
)

type enterpriseTestStore interface {
	teamTestStore
	outbound.EnterpriseStore
}

func runEnterpriseContract(t *testing.T, st enterpriseTestStore) {
	t.Helper()
	ctx := systemTestContext()
	f := makeTeamFixture(t, st)
	s := f.identity
	enterprise, err := s.CreateEnterprise(ctx, f.owner, "Acme Group", "group-"+domain.NewID("")[:10])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetEnterprise(ctx, f.outsider.ID, enterprise.Slug); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("enterprise leaked: %v", err)
	}
	if err = s.LinkEnterpriseOrganization(ctx, f.member.ID, enterprise.ID, f.organization.ID, true); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("member linked organization: %v", err)
	}
	if err = s.LinkEnterpriseOrganization(ctx, f.owner.ID, enterprise.ID, f.organization.ID, true); err != nil {
		t.Fatal(err)
	}
	if list, err := s.ListEnterprises(ctx, f.member.ID); err != nil || len(list) != 1 || list[0].ID != enterprise.ID {
		t.Fatalf("organization member enterprise listing: %+v %v", list, err)
	}
	if _, ok := s.RoleOf(ctx, f.repository.ID, f.member.ID); ok {
		t.Fatal("enterprise linkage granted private repository access")
	}
	if err = s.UpdateEnterpriseMember(ctx, f.owner.ID, enterprise.ID, f.outsider.ID, domain.EnterpriseAdmin); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.RoleOf(ctx, f.repository.ID, f.outsider.ID); ok {
		t.Fatal("enterprise administrator gained repository access")
	}
	if err = s.UpdateEnterpriseMember(ctx, f.outsider.ID, enterprise.ID, f.member.ID, domain.EnterpriseOwner); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("enterprise admin promoted owner: %v", err)
	}
	if err = s.RemoveEnterpriseMember(ctx, f.owner.ID, enterprise.ID, f.owner.ID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("last enterprise owner removed: %v", err)
	}
	if err = s.UpdateEnterpriseMember(ctx, f.owner.ID, enterprise.ID, f.outsider.ID, domain.EnterpriseOwner); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.RoleOf(ctx, f.repository.ID, f.outsider.ID); ok {
		t.Fatal("upper Enterprise Owner inherited Organization repository access")
	}
	policy, err := s.GetOrganizationPolicy(ctx, f.owner.ID, f.organization.ID)
	if err != nil {
		t.Fatal(err)
	}
	policy.RepositoryCreation = domain.OrganizationRepositoryMembers
	policy.BreakGlassEnabled = true
	policy.AllowPublicRepositories = true
	if _, err = s.UpdateOrganizationPolicy(ctx, f.owner.ID, policy); err != nil {
		t.Fatal(err)
	}
	restrictions := domain.EnterprisePolicy{RepositoryCreation: domain.OrganizationRepositoryAdmins, AllowPublicRepositories: false, AllowBreakGlass: false}
	if _, err = s.UpdateEnterprise(ctx, f.owner.ID, enterprise.ID, EnterprisePatch{Policy: &restrictions}); err != nil {
		t.Fatal(err)
	}
	effective, err := s.EffectiveOrganizationPolicy(ctx, f.member.ID, f.organization.ID)
	if err != nil || effective.AllowPublicRepositories || effective.BreakGlassEnabled || effective.RepositoryCreation != domain.OrganizationRepositoryAdmins {
		t.Fatalf("parent restrictions: %+v %v", effective, err)
	}
	if _, err = s.CreateOrganizationRepository(ctx, f.member, f.organization.ID, "Denied"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("parent repository-creation restriction bypass: %v", err)
	}
	public := domain.VisibilityPublic
	if _, err = s.UpdateRepositorySettings(ctx, f.owner.ID, f.repository.ID, RepositoryPatch{Visibility: &public}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("public repository restriction bypass: %v", err)
	}
	if _, err = s.CreateBreakGlassGrant(ctx, f.owner.ID, f.organization.ID, f.repository.ID, "test policy", 10); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("parent break-glass restriction bypass: %v", err)
	}
	if err = s.LinkEnterpriseOrganization(ctx, f.owner.ID, enterprise.ID, f.organization.ID, false); err != nil {
		t.Fatal(err)
	}
	effective, err = s.EffectiveOrganizationPolicy(ctx, f.member.ID, f.organization.ID)
	if err != nil || !effective.AllowPublicRepositories || !effective.BreakGlassEnabled || effective.RepositoryCreation != domain.OrganizationRepositoryMembers {
		t.Fatalf("unlink destroyed organization preferences: %+v %v", effective, err)
	}
	if _, err = s.UpdateRepositorySettings(ctx, f.owner.ID, f.repository.ID, RepositoryPatch{Visibility: &public}); err != nil {
		t.Fatal(err)
	}
	if err = s.LinkEnterpriseOrganization(ctx, f.owner.ID, enterprise.ID, f.organization.ID, true); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("restricted enterprise accepted existing public repository: %v", err)
	}
}
func TestEnterpriseHierarchyAndPolicyComposition(t *testing.T) {
	runEnterpriseContract(t, store.NewFSStore(t.TempDir()))
}
