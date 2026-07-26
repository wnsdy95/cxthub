package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// TestInviteNoPrivilegeEscalation ensures invitations cannot bypass owner-only rules:
// maintainers may invite only their own role or lower, owner invitations are rejected,
// invalid roles return ErrValidation instead of silently falling back, and owners may invite owners.
func TestInviteNoPrivilegeEscalation(t *testing.T) {
	ctx := context.Background()
	fs := store.NewFSStore(t.TempDir())
	svc := NewIdentityService(nil, fs)

	owner := domain.User{ID: "u_owner", Email: "o@t.io", Username: "owner"}
	maint := domain.User{ID: "u_maint", Email: "m@t.io", Username: "maint"}
	for _, u := range []domain.User{owner, maint} {
		if err := fs.UpsertUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	ws := domain.Workspace{ID: domain.NewID("ws_"), Name: "T", OwnerID: owner.ID, Slug: "t", OwnerUsername: "owner", CreatedAt: time.Now().UTC()}
	if err := fs.CreateWorkspace(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if err := fs.AddMember(ctx, domain.Membership{WorkspaceID: ws.ID, UserID: owner.ID, Role: domain.RoleOwner, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := fs.AddMember(ctx, domain.Membership{WorkspaceID: ws.ID, UserID: maint.ID, Role: domain.RoleMaintainer, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	// Maintainer attempts to invite owner → rejected (privilege escalation block — critical regression).
	if _, err := svc.Invite(ctx, maint.ID, ws.ID, "", domain.RoleOwner, 0); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("maintainer successfully invited owner (privilege escalation): err=%v", err)
	}
	// Maintainer invites member of equal or lower rank → allowed.
	if _, err := svc.Invite(ctx, maint.ID, ws.ID, "", domain.RoleMember, 0); err != nil {
		t.Fatalf("member invitation by maintainer rejected: %v", err)
	}
	// Invalid role → silent member downgrade is replaced by ErrValidation.
	if _, err := svc.Invite(ctx, owner.ID, ws.ID, "", domain.MemberRole("superadmin"), 0); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid role not rejected: err=%v", err)
	}
	// Owners can invite other owners (co-owner assignment requires owner privileges).
	if _, err := svc.Invite(ctx, owner.ID, ws.ID, "", domain.RoleOwner, 0); err != nil {
		t.Fatalf("owner invitation by owner was rejected: %v", err)
	}

	// Expired: if ttl > 0, set ExpiresAt; if expired, reject AcceptInvite. Negative ttl rejects.
	if _, err := svc.Invite(ctx, owner.ID, ws.ID, "", domain.RoleMember, -time.Hour); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("negative ttl not rejected: err=%v", err)
	}
	inv, err := svc.Invite(ctx, owner.ID, ws.ID, "", domain.RoleMember, time.Millisecond)
	if err != nil || inv.ExpiresAt == nil {
		t.Fatalf("ttl invitation ExpiresAt not set: inv=%+v err=%v", inv, err)
	}
	time.Sleep(5 * time.Millisecond)
	joiner := domain.User{ID: "u_late", Email: "l@t.io", Username: "late"}
	if err := fs.UpsertUser(ctx, joiner); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AcceptInvite(ctx, joiner, inv.Token); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expired invitation accept not rejected: err=%v", err)
	}
}

func TestAcceptInviteNeverDowngradesExistingMember(t *testing.T) {
	ctx := context.Background()
	fs := store.NewFSStore(t.TempDir())
	svc := NewIdentityService(nil, fs)
	owner := domain.User{ID: "u_owner", Email: "owner@example.com", Username: "owner"}
	member := domain.User{ID: "u_member", Email: "member@example.com", Username: "member"}
	for _, user := range []domain.User{owner, member} {
		if err := fs.UpsertUser(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	ws := domain.Workspace{ID: domain.NewID("ws_"), Name: "Roles", OwnerID: owner.ID, Slug: "roles", OwnerUsername: owner.Username}
	if err := fs.CreateWorkspace(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if err := fs.AddMember(ctx, domain.Membership{WorkspaceID: ws.ID, UserID: owner.ID, Role: domain.RoleOwner}); err != nil {
		t.Fatal(err)
	}
	if err := fs.AddMember(ctx, domain.Membership{WorkspaceID: ws.ID, UserID: member.ID, Role: domain.RoleMaintainer}); err != nil {
		t.Fatal(err)
	}
	oldViewerInvite, err := svc.Invite(ctx, owner.ID, ws.ID, member.Email, domain.RoleViewer, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AcceptInvite(ctx, member, oldViewerInvite.Token); err != nil {
		t.Fatal(err)
	}
	if role, ok := svc.RoleOf(ctx, ws.ID, member.ID); !ok || role != domain.RoleMaintainer {
		t.Fatalf("old invite downgraded existing member: %q, %v", role, ok)
	}
}

func TestRevokeInviteCannotCrossWorkspaceBoundary(t *testing.T) {
	ctx := context.Background()
	fs := store.NewFSStore(t.TempDir())
	svc := NewIdentityService(nil, fs)
	ownerA := domain.User{ID: "u_owner_a", Email: "a@example.com", Username: "a"}
	ownerB := domain.User{ID: "u_owner_b", Email: "b@example.com", Username: "b"}
	maintA := domain.User{ID: "u_maint_a", Email: "maint@example.com", Username: "maint"}
	for _, user := range []domain.User{ownerA, ownerB, maintA} {
		if err := fs.UpsertUser(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	wsA := domain.Workspace{ID: domain.NewID("ws_"), Name: "A", OwnerID: ownerA.ID, Slug: "a", OwnerUsername: ownerA.Username}
	wsB := domain.Workspace{ID: domain.NewID("ws_"), Name: "B", OwnerID: ownerB.ID, Slug: "b", OwnerUsername: ownerB.Username}
	for _, ws := range []domain.Workspace{wsA, wsB} {
		if err := fs.CreateWorkspace(ctx, ws); err != nil {
			t.Fatal(err)
		}
	}
	if err := fs.AddMember(ctx, domain.Membership{WorkspaceID: wsA.ID, UserID: maintA.ID, Role: domain.RoleMaintainer}); err != nil {
		t.Fatal(err)
	}
	invB, err := svc.Invite(ctx, ownerB.ID, wsB.ID, "", domain.RoleMember, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RevokeInvite(ctx, maintA.ID, wsA.ID, invB.Token); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-workspace revoke error = %v", err)
	}
	stored, err := fs.GetInvite(ctx, invB.Token)
	if err != nil || stored.Status != domain.InvitePending {
		t.Fatalf("foreign invite was changed: %+v, %v", stored, err)
	}
}
