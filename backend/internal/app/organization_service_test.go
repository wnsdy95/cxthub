package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type auditFailOrganizationStore struct {
	*store.FSStore
	failUseAudit bool
}

type organizationTestVerifier struct{ user domain.User }

func (v organizationTestVerifier) Verify(context.Context, string) (domain.User, error) {
	return v.user, nil
}

func (s *auditFailOrganizationStore) UseActiveBreakGlassGrant(ctx context.Context, organizationID, repositoryID, userID string, now time.Time, event domain.OrganizationAuditEvent) (domain.BreakGlassGrant, error) {
	if s.failUseAudit {
		return domain.BreakGlassGrant{}, errors.New("audit store unavailable")
	}
	return s.FSStore.UseActiveBreakGlassGrant(ctx, organizationID, repositoryID, userID, now, event)
}

func TestOrganizationOwnerAccessAndExplicitBreakGlassAudit(t *testing.T) {
	ctx := context.Background()
	st := &auditFailOrganizationStore{FSStore: store.NewFSStore(t.TempDir())}
	svc := NewIdentityService(nil, st)

	owner := domain.User{ID: "user-owner", Email: "owner@example.test", Name: "Owner", Username: "owner"}
	admin := domain.User{ID: "user-admin", Email: "admin@example.test", Name: "Admin", Username: "admin-user"}
	member := domain.User{ID: "user-member", Email: "member@example.test", Name: "Member", Username: "member-user"}
	for _, user := range []domain.User{owner, admin, member} {
		if err := st.UpsertUser(ctx, user); err != nil {
			t.Fatal(err)
		}
	}

	organization, err := svc.CreateOrganization(ctx, owner, "Acme Corporation", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateOrganizationMember(ctx, owner.ID, organization.ID, admin.ID, domain.OrganizationAdmin); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateOrganizationMember(ctx, owner.ID, organization.ID, member.ID, domain.OrganizationMember); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateOrganizationMember(ctx, admin.ID, organization.ID, member.ID, domain.OrganizationAdmin); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("admin promoted Organization member to admin: %v", err)
	}
	if err := svc.UpdateOrganizationMember(ctx, owner.ID, organization.ID, member.ID, domain.OrganizationOwner); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateOrganizationMember(ctx, admin.ID, organization.ID, member.ID, domain.OrganizationMember); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("admin demoted Organization owner: %v", err)
	}
	if err := svc.UpdateOrganizationMember(ctx, owner.ID, organization.ID, member.ID, domain.OrganizationMember); err != nil {
		t.Fatal(err)
	}
	updatedName := "Acme Engineering"
	logo := "data:image/png;base64,aA=="
	updatedOrganization, err := svc.UpdateOrganizationProfile(ctx, admin.ID, organization.ID, &updatedName, &logo)
	if err != nil {
		t.Fatal(err)
	}
	if updatedOrganization.Name != updatedName || updatedOrganization.Logo != logo || updatedOrganization.Slug != "acme" {
		t.Fatalf("organization profile update = %+v", updatedOrganization)
	}
	deniedName := "Member Rewrite"
	if _, err := svc.UpdateOrganizationProfile(ctx, member.ID, organization.ID, &deniedName, nil); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("member profile update err=%v, want forbidden", err)
	}

	repository, err := svc.CreateOrganizationRepository(ctx, admin, organization.ID, "Platform")
	if err != nil {
		t.Fatal(err)
	}
	if role, ok := svc.RoleOf(ctx, repository.ID, owner.ID); !ok || role != domain.RoleOwner {
		t.Fatalf("organization owner missing inherited repository role: role=%q ok=%v", role, ok)
	}
	renamedAdmin := "renamed-admin"
	if _, err := svc.UpdateProfile(ctx, admin, &renamedAdmin, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	stableRepository, err := st.GetRepository(ctx, repository.ID)
	if err != nil || stableRepository.OwnerUsername != organization.Slug {
		t.Fatalf("personal rename changed Organization URL: repository=%+v err=%v", stableRepository, err)
	}
	if _, personalRepositories, err := svc.PublicUser(ctx, renamedAdmin, admin.ID); err != nil || len(personalRepositories) != 0 {
		t.Fatalf("Organization Repository leaked onto creator profile: repositories=%+v err=%v", personalRepositories, err)
	}
	if _, err := svc.CreateOrganizationRepository(ctx, member, organization.ID, "MemberDenied"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("member repository creation err=%v, want forbidden", err)
	}
	if allowed, err := svc.HasBreakGlassAccess(ctx, repository.ID, owner.ID); err != nil || allowed {
		t.Fatal("owner had break-glass access without an explicit grant")
	}
	if _, err := svc.ReadableRepository(ctx, organization.Slug, repository.Slug, owner.ID); err != nil {
		t.Fatalf("organization owner cannot read private Repository: %v", err)
	}
	if _, err := svc.CreateBreakGlassGrant(ctx, admin.ID, organization.ID, repository.ID, "investigate incident", 15); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("admin break-glass err=%v, want forbidden", err)
	}
	grant, err := svc.CreateBreakGlassGrant(ctx, owner.ID, organization.ID, repository.ID, "investigate incident", 15)
	if err != nil {
		t.Fatal(err)
	}
	allowed, accessErr := svc.HasBreakGlassAccess(ctx, repository.ID, owner.ID)
	if grant.Reason != "investigate incident" || accessErr != nil || !allowed {
		t.Fatalf("active grant not applied: %+v", grant)
	}
	if readable, err := svc.ReadableRepository(ctx, organization.Slug, repository.Slug, owner.ID); err != nil || readable.ID != repository.ID {
		t.Fatalf("break-glass Repository entry unavailable: repository=%+v err=%v", readable, err)
	}
	st.failUseAudit = true
	if allowed, err := svc.HasBreakGlassAccess(ctx, repository.ID, owner.ID); err == nil || allowed {
		t.Fatalf("break-glass did not fail closed when use audit failed: allowed=%v err=%v", allowed, err)
	}
	st.failUseAudit = false
	if direct, err := st.IsMember(ctx, repository.ID, owner.ID); err != nil || direct {
		t.Fatal("inherited access and break-glass must not create durable repository membership")
	}

	policy, err := svc.GetOrganizationPolicy(ctx, owner.ID, organization.ID)
	if err != nil {
		t.Fatal(err)
	}
	policy.RepositoryCreation = domain.OrganizationRepositoryMembers
	policy.AllowPublicRepositories = false
	if _, err := svc.UpdateOrganizationPolicy(ctx, owner.ID, policy); err != nil {
		t.Fatal(err)
	}
	memberRepository, err := svc.CreateOrganizationRepository(ctx, member, organization.ID, "MemberAllowed")
	if err != nil {
		t.Fatal(err)
	}
	public := domain.VisibilityPublic
	if _, err := svc.UpdateRepositorySettings(ctx, member.ID, memberRepository.ID, RepositoryPatch{Visibility: &public}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("public repository policy err=%v, want forbidden", err)
	}

	audit, err := svc.ListOrganizationAudit(ctx, owner.ID, organization.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	wantActions := map[string]bool{
		"organization.created":             false,
		"organization.profile.updated":     false,
		"organization.member.updated":      false,
		"organization.repository.created":  false,
		"organization.break_glass.created": false,
		"organization.policy.updated":      false,
	}
	for _, event := range audit {
		if _, ok := wantActions[event.Action]; ok {
			wantActions[event.Action] = true
		}
	}
	for action, found := range wantActions {
		if !found {
			t.Errorf("audit action %q missing: %+v", action, audit)
		}
	}
}

func TestOrganizationOwnershipTransfersWithoutPinningCreator(t *testing.T) {
	ctx := context.Background()
	st := store.NewFSStore(t.TempDir())
	svc := NewIdentityService(nil, st)
	founder := domain.User{ID: "organization-founder", Email: "founder@example.test", Name: "Founder", Username: "organization-founder"}
	successor := domain.User{ID: "organization-successor", Email: "successor@example.test", Name: "Successor", Username: "organization-successor"}
	admin := domain.User{ID: "organization-admin", Email: "admin@example.test", Name: "Admin", Username: "organization-admin"}
	member := domain.User{ID: "organization-ordinary", Email: "ordinary@example.test", Name: "Ordinary", Username: "organization-ordinary"}
	for _, user := range []domain.User{founder, successor, admin, member} {
		if err := st.UpsertUser(ctx, user); err != nil {
			t.Fatal(err)
		}
	}

	organization, err := svc.CreateOrganization(ctx, founder, "Transferable", "transferable")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateOrganizationMember(ctx, founder.ID, organization.ID, successor.ID, domain.OrganizationOwner); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateOrganizationMember(ctx, founder.ID, organization.ID, admin.ID, domain.OrganizationAdmin); err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveOrganizationMember(ctx, successor.ID, organization.ID, founder.ID); err != nil {
		t.Fatalf("successor could not remove the original creator after ownership transfer: %v", err)
	}
	if _, err := st.GetOrganizationMembership(ctx, organization.ID, founder.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("founder membership remained pinned: %v", err)
	}
	if err := svc.UpdateOrganizationMember(ctx, successor.ID, organization.ID, successor.ID, domain.OrganizationMember); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("last owner demotion = %v, want conflict", err)
	}
	if err := svc.RemoveOrganizationMember(ctx, successor.ID, organization.ID, successor.ID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("last owner removal = %v, want conflict", err)
	}
	if err := svc.UpdateOrganizationMember(ctx, admin.ID, organization.ID, member.ID, domain.OrganizationAdmin); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("admin promoted a new admin: %v", err)
	}
	if err := svc.UpdateOrganizationMember(ctx, admin.ID, organization.ID, member.ID, domain.OrganizationMember); err != nil {
		t.Fatalf("admin could not add an ordinary member: %v", err)
	}
	if err := svc.RemoveOrganizationMember(ctx, admin.ID, organization.ID, successor.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("admin removed an owner: %v", err)
	}
}

func TestOrganizationSlugCannotClaimPersonalNamespace(t *testing.T) {
	ctx := context.Background()
	st := store.NewFSStore(t.TempDir())
	svc := NewIdentityService(nil, st)
	owner := domain.User{ID: "owner", Email: "owner@example.test", Name: "Owner", Username: "owner"}
	claimed := domain.User{ID: "claimed", Email: "claimed@example.test", Name: "Claimed", Username: "acme"}
	for _, user := range []domain.User{owner, claimed} {
		if err := st.UpsertUser(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.ensurePersonalNamespace(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateOrganization(ctx, owner, "Acme", "acme"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("organization claimed personal namespace: %v", err)
	}

	repository, err := svc.CreateRepository(ctx, claimed, "Personal")
	if err != nil {
		t.Fatal(err)
	}
	next := "renamed"
	if _, err := svc.UpdateProfile(ctx, claimed, &next, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	oldAlias, err := st.GetNamespaceBySlug(ctx, "acme")
	if err != nil || oldAlias.UserID != claimed.ID || oldAlias.Slug != next {
		t.Fatalf("old namespace alias did not resolve to renamed namespace: %+v err=%v", oldAlias, err)
	}
	storedRepository, err := st.GetRepositoryByNamespacePath(ctx, oldAlias.ID, repository.Slug)
	if err != nil || storedRepository.ID != repository.ID || storedRepository.OwnerUsername != next {
		t.Fatalf("renamed personal repository path lost: %+v err=%v", storedRepository, err)
	}
	if _, err := svc.CreateOrganization(ctx, owner, "Old Alias Claim", "acme"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("organization claimed historical namespace alias: %v", err)
	}
}

func TestOrganizationNamespaceRejectsServerOwnedRouteSegments(t *testing.T) {
	ctx := context.Background()
	st := store.NewFSStore(t.TempDir())
	svc := NewIdentityService(nil, st)
	owner := domain.User{ID: "route-owner", Email: "route@example.test", Name: "Route Owner", Username: "route-owner"}
	if err := st.UpsertUser(ctx, owner); err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"pricing", "connect", "oauth", "mcp"} {
		if _, err := svc.CreateOrganization(ctx, owner, "Reserved "+slug, slug); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("CreateOrganization slug %q error = %v, want validation", slug, err)
		}
	}
}

func TestLegacyFSRepositoryRemainsAddressableBeforeNamespaceBackfill(t *testing.T) {
	ctx := context.Background()
	st := store.NewFSStore(t.TempDir())
	owner := domain.User{ID: "legacy-owner", Email: "legacy@example.test", Name: "Legacy", Username: "legacy"}
	repository := domain.Repository{
		ID: domain.NewID("ws_"), Name: "Project", OwnerID: owner.ID,
		OwnerUsername: owner.Username, Slug: "project", Visibility: domain.VisibilityPublic,
		CreatedAt: time.Now().UTC(),
	}
	if err := st.UpsertUser(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRepository(ctx, repository); err != nil {
		t.Fatal(err)
	}
	identity := NewIdentityService(nil, st)
	if got, err := identity.PublicRepository(ctx, "legacy", "project"); err != nil || got.ID != repository.ID {
		t.Fatalf("legacy public Repository = %+v err=%v", got, err)
	}
	contextService := &Service{repositories: st}
	if got, err := contextService.repositoryForURL(ctx, "https://cxthub.test/legacy/project"); err != nil || got != repository.ID {
		t.Fatalf("legacy repository binding = %q err=%v", got, err)
	}
}

func TestAutomaticUsernameSkipsClaimedOrganizationNamespace(t *testing.T) {
	ctx := context.Background()
	st := store.NewFSStore(t.TempDir())
	owner := domain.User{ID: "organization-owner", Email: "owner@example.test", Name: "Owner", Username: "owner"}
	if err := st.UpsertUser(ctx, owner); err != nil {
		t.Fatal(err)
	}
	bootstrap := NewIdentityService(nil, st)
	if _, err := bootstrap.CreateOrganization(ctx, owner, "Acme", "acme"); err != nil {
		t.Fatal(err)
	}
	newUser := domain.User{ID: "new-user", Email: "acme@example.test", Name: "Acme User"}
	svc := NewIdentityService(organizationTestVerifier{user: newUser}, st)
	got, err := svc.Authenticate(ctx, "idp-token")
	if err != nil {
		t.Fatal(err)
	}
	if got.Username != "acme-2" {
		t.Fatalf("generated username = %q, want acme-2", got.Username)
	}
	namespace, err := st.GetNamespaceBySlug(ctx, got.Username)
	if err != nil || namespace.Kind != domain.NamespaceUser || namespace.UserID != got.ID {
		t.Fatalf("personal namespace = %+v err=%v", namespace, err)
	}
}
