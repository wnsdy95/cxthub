package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestFSWorkspaceStoreRejectsTraversalKeys(t *testing.T) {
	st := NewFSStore(t.TempDir())
	ctx := context.Background()

	if err := st.CreateWorkspace(ctx, domain.Workspace{ID: "../outside", OwnerID: "owner"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("workspace traversal: %v", err)
	}
	if _, err := st.GetWorkspace(ctx, "../outside"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("workspace get traversal: %v", err)
	}
	if err := st.AddMember(ctx, domain.Membership{WorkspaceID: "../outside", UserID: "user", Role: domain.RoleMember}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("membership traversal: %v", err)
	}
	if err := st.CreateInvite(ctx, domain.Invite{Token: "../outside", WorkspaceID: domain.NewID("ws_")}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invite traversal: %v", err)
	}
	if _, err := st.GetInvite(ctx, "../outside"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invite get traversal: %v", err)
	}
	if err := st.CreateSession(ctx, domain.Session{Token: "../outside", UserID: "user"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("session traversal: %v", err)
	}
	if _, err := st.GetSession(ctx, "../outside"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("session get traversal: %v", err)
	}
}

func TestFSWorkspaceStoreExternalIDsAreCollisionFree(t *testing.T) {
	st := NewFSStore(t.TempDir())
	ctx := context.Background()
	wsID := domain.NewID("ws_")
	if err := st.CreateWorkspace(ctx, domain.Workspace{
		ID: wsID, Name: "Collision", OwnerID: "owner", Slug: "collision", OwnerUsername: "owner", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	// In legacy safeName, both values become firebase_a.
	users := []domain.User{
		{ID: "firebase/a", Email: "slash@example.com", Username: "slash"},
		{ID: "firebase_a", Email: "underscore@example.com", Username: "underscore"},
	}
	for _, user := range users {
		if err := st.UpsertUser(ctx, user); err != nil {
			t.Fatal(err)
		}
		if err := st.AddMember(ctx, domain.Membership{WorkspaceID: wsID, UserID: user.ID, Role: domain.RoleMember}); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range users {
		got, err := st.GetUser(ctx, want.ID)
		if err != nil || got.Email != want.Email {
			t.Fatalf("GetUser(%q) = %+v, %v", want.ID, got, err)
		}
	}
	members, err := st.ListMembers(ctx, wsID)
	if err != nil || len(members) != 2 {
		t.Fatalf("members = %+v, %v", members, err)
	}
	if err := st.RemoveMember(ctx, wsID, users[0].ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.IsMember(ctx, wsID, users[0].ID); ok {
		t.Fatal("removed collision member still present")
	}
	if ok, _ := st.IsMember(ctx, wsID, users[1].ID); !ok {
		t.Fatal("removing one collision member removed the other")
	}
}

func TestFSWorkspaceStoreLegacyUserReadChecksEmbeddedID(t *testing.T) {
	st := NewFSStore(t.TempDir())
	want := domain.User{ID: "firebase_a", Email: "underscore@example.com"}
	raw, _ := json.Marshal(want)
	if err := writeAtomic(filepath.Join(st.usersDir(), safeName("firebase/a")+".json"), raw); err != nil {
		t.Fatal(err)
	}

	if _, err := st.GetUser(context.Background(), "firebase/a"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("legacy collision returned another user: %v", err)
	}
	got, err := st.GetUser(context.Background(), want.ID)
	if err != nil || got.ID != want.ID {
		t.Fatalf("legacy exact read = %+v, %v", got, err)
	}
}

func TestFSWorkspaceStoreOpaqueMembershipOverridesLegacyRole(t *testing.T) {
	st := NewFSStore(t.TempDir())
	ctx := context.Background()
	wsID := domain.NewID("ws_")
	userID := "firebase/legacy-user"
	legacy := domain.Membership{WorkspaceID: wsID, UserID: userID, Role: domain.RoleOwner}
	raw, _ := json.Marshal(legacy)
	legacyPath := filepath.Join(st.membersDir(), wsID, safeName(userID)+".json")
	if err := writeAtomic(legacyPath, raw); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMember(ctx, domain.Membership{WorkspaceID: wsID, UserID: userID, Role: domain.RoleViewer}); err != nil {
		t.Fatal(err)
	}

	members, err := st.ListMembers(ctx, wsID)
	if err != nil || len(members) != 1 || members[0].Role != domain.RoleViewer {
		t.Fatalf("members = %+v, err=%v; stale legacy role won", members, err)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("exact legacy membership was not removed: %v", err)
	}
}

func TestFSWorkspaceStoreCorruptRoleDoesNotGrantMembership(t *testing.T) {
	st := NewFSStore(t.TempDir())
	ctx := context.Background()
	wsID := domain.NewID("ws_")
	userID := "firebase/corrupt-role"
	corrupt := domain.Membership{WorkspaceID: wsID, UserID: userID, Role: domain.MemberRole("root")}
	raw, _ := json.Marshal(corrupt)
	if err := writeAtomic(filepath.Join(st.membersDir(), wsID, opaqueName(userID)+".json"), raw); err != nil {
		t.Fatal(err)
	}

	member, err := st.IsMember(ctx, wsID, userID)
	if err != nil {
		t.Fatal(err)
	}
	if member {
		t.Fatal("corrupt role granted private workspace membership")
	}
	if _, err := st.ListMembers(ctx, wsID); err != nil {
		t.Fatalf("corrupt list entry should be ignored fail-closed: %v", err)
	}
}

func TestFSWorkspaceStoreRejectsCorruptInviteOnRead(t *testing.T) {
	st := NewFSStore(t.TempDir())
	token := domain.NewID("inv_")
	inv := domain.Invite{
		Token: token, WorkspaceID: domain.NewID("ws_"), CreatedBy: "firebase/owner",
		Role: domain.RoleMember, Status: domain.InviteStatus("reopened"),
	}
	raw, _ := json.Marshal(inv)
	if err := writeAtomic(filepath.Join(st.invitesDir(), token+".json"), raw); err != nil {
		t.Fatal(err)
	}

	if _, err := st.GetInvite(context.Background(), token); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("corrupt invite read = %v, want ErrIntegrity", err)
	}
}
