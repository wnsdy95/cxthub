package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type auditFailEnterpriseStore struct {
	*store.FSStore
	failUseAudit bool
}

type enterpriseTestVerifier struct{ user domain.User }

func (v enterpriseTestVerifier) Verify(context.Context, string) (domain.User, error) {
	return v.user, nil
}

func (s *auditFailEnterpriseStore) UseActiveBreakGlassGrant(ctx context.Context, enterpriseID, workspaceID, userID string, now time.Time, event domain.EnterpriseAuditEvent) (domain.BreakGlassGrant, error) {
	if s.failUseAudit {
		return domain.BreakGlassGrant{}, errors.New("audit store unavailable")
	}
	return s.FSStore.UseActiveBreakGlassGrant(ctx, enterpriseID, workspaceID, userID, now, event)
}

func TestEnterpriseRolesDoNotImplicitlyGrantWorkspaceContext(t *testing.T) {
	ctx := context.Background()
	st := &auditFailEnterpriseStore{FSStore: store.NewFSStore(t.TempDir())}
	svc := NewIdentityService(nil, st)

	owner := domain.User{ID: "user-owner", Email: "owner@example.test", Name: "Owner", Username: "owner"}
	admin := domain.User{ID: "user-admin", Email: "admin@example.test", Name: "Admin", Username: "admin-user"}
	member := domain.User{ID: "user-member", Email: "member@example.test", Name: "Member", Username: "member-user"}
	for _, user := range []domain.User{owner, admin, member} {
		if err := st.UpsertUser(ctx, user); err != nil {
			t.Fatal(err)
		}
	}

	enterprise, err := svc.CreateEnterprise(ctx, owner, "Acme Corporation", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateEnterpriseMember(ctx, owner.ID, enterprise.ID, admin.ID, domain.EnterpriseAdmin); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateEnterpriseMember(ctx, owner.ID, enterprise.ID, member.ID, domain.EnterpriseMember); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateEnterpriseMember(ctx, admin.ID, enterprise.ID, member.ID, domain.EnterpriseAdmin); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("admin promoted Enterprise member to admin: %v", err)
	}
	if err := svc.UpdateEnterpriseMember(ctx, owner.ID, enterprise.ID, member.ID, domain.EnterpriseOwner); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateEnterpriseMember(ctx, admin.ID, enterprise.ID, member.ID, domain.EnterpriseMember); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("admin demoted Enterprise owner: %v", err)
	}
	if err := svc.UpdateEnterpriseMember(ctx, owner.ID, enterprise.ID, member.ID, domain.EnterpriseMember); err != nil {
		t.Fatal(err)
	}
	updatedName := "Acme Engineering"
	logo := "data:image/png;base64,aA=="
	updatedEnterprise, err := svc.UpdateEnterpriseProfile(ctx, admin.ID, enterprise.ID, &updatedName, &logo)
	if err != nil {
		t.Fatal(err)
	}
	if updatedEnterprise.Name != updatedName || updatedEnterprise.Logo != logo || updatedEnterprise.Slug != "acme" {
		t.Fatalf("enterprise profile update = %+v", updatedEnterprise)
	}
	deniedName := "Member Rewrite"
	if _, err := svc.UpdateEnterpriseProfile(ctx, member.ID, enterprise.ID, &deniedName, nil); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("member profile update err=%v, want forbidden", err)
	}

	workspace, err := svc.CreateEnterpriseWorkspace(ctx, admin, enterprise.ID, "Platform")
	if err != nil {
		t.Fatal(err)
	}
	if role, ok := svc.RoleOf(ctx, workspace.ID, owner.ID); ok || role != "" {
		t.Fatalf("enterprise owner inherited workspace role: role=%q ok=%v", role, ok)
	}
	renamedAdmin := "renamed-admin"
	if _, err := svc.UpdateProfile(ctx, admin, &renamedAdmin, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	stableWorkspace, err := st.GetWorkspace(ctx, workspace.ID)
	if err != nil || stableWorkspace.OwnerUsername != enterprise.Slug {
		t.Fatalf("personal rename changed Enterprise URL: workspace=%+v err=%v", stableWorkspace, err)
	}
	if _, personalWorkspaces, err := svc.PublicUser(ctx, renamedAdmin, admin.ID); err != nil || len(personalWorkspaces) != 0 {
		t.Fatalf("Enterprise Workspace leaked onto creator profile: workspaces=%+v err=%v", personalWorkspaces, err)
	}
	if _, err := svc.CreateEnterpriseWorkspace(ctx, member, enterprise.ID, "MemberDenied"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("member workspace creation err=%v, want forbidden", err)
	}
	if allowed, err := svc.HasBreakGlassAccess(ctx, workspace.ID, owner.ID); err != nil || allowed {
		t.Fatal("owner had break-glass access without an explicit grant")
	}
	if _, err := svc.ReadableWorkspace(ctx, enterprise.Slug, workspace.Slug, owner.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("private Workspace leaked before grant: %v", err)
	}
	if _, err := svc.CreateBreakGlassGrant(ctx, admin.ID, enterprise.ID, workspace.ID, "investigate incident", 15); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("admin break-glass err=%v, want forbidden", err)
	}
	grant, err := svc.CreateBreakGlassGrant(ctx, owner.ID, enterprise.ID, workspace.ID, "investigate incident", 15)
	if err != nil {
		t.Fatal(err)
	}
	allowed, accessErr := svc.HasBreakGlassAccess(ctx, workspace.ID, owner.ID)
	if grant.Reason != "investigate incident" || accessErr != nil || !allowed {
		t.Fatalf("active grant not applied: %+v", grant)
	}
	if readable, err := svc.ReadableWorkspace(ctx, enterprise.Slug, workspace.Slug, owner.ID); err != nil || readable.ID != workspace.ID {
		t.Fatalf("break-glass Workspace entry unavailable: workspace=%+v err=%v", readable, err)
	}
	st.failUseAudit = true
	if allowed, err := svc.HasBreakGlassAccess(ctx, workspace.ID, owner.ID); err == nil || allowed {
		t.Fatalf("break-glass did not fail closed when use audit failed: allowed=%v err=%v", allowed, err)
	}
	st.failUseAudit = false
	if role, ok := svc.RoleOf(ctx, workspace.ID, owner.ID); ok || role != "" {
		t.Fatal("break-glass must not mutate durable workspace membership")
	}

	policy, err := svc.GetEnterprisePolicy(ctx, owner.ID, enterprise.ID)
	if err != nil {
		t.Fatal(err)
	}
	policy.WorkspaceCreation = domain.EnterpriseWorkspaceMembers
	policy.AllowPublicWorkspaces = false
	if _, err := svc.UpdateEnterprisePolicy(ctx, owner.ID, policy); err != nil {
		t.Fatal(err)
	}
	memberWorkspace, err := svc.CreateEnterpriseWorkspace(ctx, member, enterprise.ID, "MemberAllowed")
	if err != nil {
		t.Fatal(err)
	}
	public := domain.VisibilityPublic
	if _, err := svc.UpdateWorkspaceSettings(ctx, member.ID, memberWorkspace.ID, WorkspacePatch{Visibility: &public}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("public workspace policy err=%v, want forbidden", err)
	}

	audit, err := svc.ListEnterpriseAudit(ctx, owner.ID, enterprise.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	wantActions := map[string]bool{
		"enterprise.created":             false,
		"enterprise.profile.updated":     false,
		"enterprise.member.updated":      false,
		"enterprise.workspace.created":   false,
		"enterprise.break_glass.created": false,
		"enterprise.policy.updated":      false,
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

func TestEnterpriseOwnershipTransfersWithoutPinningCreator(t *testing.T) {
	ctx := context.Background()
	st := store.NewFSStore(t.TempDir())
	svc := NewIdentityService(nil, st)
	founder := domain.User{ID: "enterprise-founder", Email: "founder@example.test", Name: "Founder", Username: "enterprise-founder"}
	successor := domain.User{ID: "enterprise-successor", Email: "successor@example.test", Name: "Successor", Username: "enterprise-successor"}
	admin := domain.User{ID: "enterprise-admin", Email: "admin@example.test", Name: "Admin", Username: "enterprise-admin"}
	member := domain.User{ID: "enterprise-ordinary", Email: "ordinary@example.test", Name: "Ordinary", Username: "enterprise-ordinary"}
	for _, user := range []domain.User{founder, successor, admin, member} {
		if err := st.UpsertUser(ctx, user); err != nil {
			t.Fatal(err)
		}
	}

	enterprise, err := svc.CreateEnterprise(ctx, founder, "Transferable", "transferable")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateEnterpriseMember(ctx, founder.ID, enterprise.ID, successor.ID, domain.EnterpriseOwner); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateEnterpriseMember(ctx, founder.ID, enterprise.ID, admin.ID, domain.EnterpriseAdmin); err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveEnterpriseMember(ctx, successor.ID, enterprise.ID, founder.ID); err != nil {
		t.Fatalf("successor could not remove the original creator after ownership transfer: %v", err)
	}
	if _, err := st.GetEnterpriseMembership(ctx, enterprise.ID, founder.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("founder membership remained pinned: %v", err)
	}
	if err := svc.UpdateEnterpriseMember(ctx, successor.ID, enterprise.ID, successor.ID, domain.EnterpriseMember); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("last owner demotion = %v, want conflict", err)
	}
	if err := svc.RemoveEnterpriseMember(ctx, successor.ID, enterprise.ID, successor.ID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("last owner removal = %v, want conflict", err)
	}
	if err := svc.UpdateEnterpriseMember(ctx, admin.ID, enterprise.ID, member.ID, domain.EnterpriseAdmin); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("admin promoted a new admin: %v", err)
	}
	if err := svc.UpdateEnterpriseMember(ctx, admin.ID, enterprise.ID, member.ID, domain.EnterpriseMember); err != nil {
		t.Fatalf("admin could not add an ordinary member: %v", err)
	}
	if err := svc.RemoveEnterpriseMember(ctx, admin.ID, enterprise.ID, successor.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("admin removed an owner: %v", err)
	}
}

func TestEnterpriseSlugCannotClaimPersonalNamespace(t *testing.T) {
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
	if _, err := svc.CreateEnterprise(ctx, owner, "Acme", "acme"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("enterprise claimed personal namespace: %v", err)
	}

	workspace, err := svc.CreateWorkspace(ctx, claimed, "Personal")
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
	storedWorkspace, err := st.GetWorkspaceByNamespacePath(ctx, oldAlias.ID, workspace.Slug)
	if err != nil || storedWorkspace.ID != workspace.ID || storedWorkspace.OwnerUsername != next {
		t.Fatalf("renamed personal workspace path lost: %+v err=%v", storedWorkspace, err)
	}
	if _, err := svc.CreateEnterprise(ctx, owner, "Old Alias Claim", "acme"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("enterprise claimed historical namespace alias: %v", err)
	}
}

func TestEnterpriseNamespaceRejectsServerOwnedRouteSegments(t *testing.T) {
	ctx := context.Background()
	st := store.NewFSStore(t.TempDir())
	svc := NewIdentityService(nil, st)
	owner := domain.User{ID: "route-owner", Email: "route@example.test", Name: "Route Owner", Username: "route-owner"}
	if err := st.UpsertUser(ctx, owner); err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"pricing", "connect", "oauth", "mcp"} {
		if _, err := svc.CreateEnterprise(ctx, owner, "Reserved "+slug, slug); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("CreateEnterprise slug %q error = %v, want validation", slug, err)
		}
	}
}

func TestLegacyFSWorkspaceRemainsAddressableBeforeNamespaceBackfill(t *testing.T) {
	ctx := context.Background()
	st := store.NewFSStore(t.TempDir())
	owner := domain.User{ID: "legacy-owner", Email: "legacy@example.test", Name: "Legacy", Username: "legacy"}
	workspace := domain.Workspace{
		ID: domain.NewID("ws_"), Name: "Project", OwnerID: owner.ID,
		OwnerUsername: owner.Username, Slug: "project", Visibility: domain.VisibilityPublic,
		CreatedAt: time.Now().UTC(),
	}
	if err := st.UpsertUser(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkspace(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	identity := NewIdentityService(nil, st)
	if got, err := identity.PublicWorkspace(ctx, "legacy", "project"); err != nil || got.ID != workspace.ID {
		t.Fatalf("legacy public Workspace = %+v err=%v", got, err)
	}
	contextService := &Service{ws: st}
	if got, err := contextService.workspaceForURL(ctx, "https://cxthub.test/legacy/project/repository"); err != nil || got != workspace.ID {
		t.Fatalf("legacy repository binding = %q err=%v", got, err)
	}
}

func TestAutomaticUsernameSkipsClaimedEnterpriseNamespace(t *testing.T) {
	ctx := context.Background()
	st := store.NewFSStore(t.TempDir())
	owner := domain.User{ID: "enterprise-owner", Email: "owner@example.test", Name: "Owner", Username: "owner"}
	if err := st.UpsertUser(ctx, owner); err != nil {
		t.Fatal(err)
	}
	bootstrap := NewIdentityService(nil, st)
	if _, err := bootstrap.CreateEnterprise(ctx, owner, "Acme", "acme"); err != nil {
		t.Fatal(err)
	}
	newUser := domain.User{ID: "new-user", Email: "acme@example.test", Name: "Acme User"}
	svc := NewIdentityService(enterpriseTestVerifier{user: newUser}, st)
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
