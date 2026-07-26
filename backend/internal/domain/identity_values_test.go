package domain

import (
	"encoding/base64"
	"errors"
	"testing"
)

func TestIdentityStorageKeyValidation(t *testing.T) {
	if err := ValidateWorkspaceID(NewID("ws_")); err != nil {
		t.Fatal(err)
	}
	if err := ValidateInviteToken(NewID("inv_")); err != nil {
		t.Fatal(err)
	}
	if err := ValidateStoredSessionToken(HashToken(NewID("sess_"))); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "../outside", "ws_ABC", "ws_1234"} {
		if err := ValidateWorkspaceID(bad); !errors.Is(err, ErrValidation) {
			t.Fatalf("ValidateWorkspaceID(%q) = %v", bad, err)
		}
	}
	if err := ValidateExternalID("firebase/user:123"); err != nil {
		t.Fatalf("external provider ID should remain opaque: %v", err)
	}
	if err := ValidateExternalID("bad\nuid"); !errors.Is(err, ErrValidation) {
		t.Fatalf("control character accepted: %v", err)
	}
}

func TestValidateAvatarDataURL(t *testing.T) {
	valid := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString([]byte("jpeg bytes"))
	if err := ValidateAvatarDataURL(valid); err != nil {
		t.Fatalf("valid raster avatar rejected: %v", err)
	}
	for _, bad := range []string{
		"data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte("<svg/>")),
		"data:image/png,not-base64",
		"https://tracker.example/avatar.png",
		"data:image/png;base64,%%%",
	} {
		if err := ValidateAvatarDataURL(bad); !errors.Is(err, ErrValidation) {
			t.Fatalf("ValidateAvatarDataURL(%q) = %v", bad, err)
		}
	}
}

func TestIdentityRecordValidationRejectsAuthorityCorruption(t *testing.T) {
	wsID := NewID("ws_")
	userID := "firebase/user:123"

	if err := ValidateWorkspaceRecord(Workspace{ID: wsID, OwnerID: userID}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorkspaceRecord(Workspace{ID: wsID, OwnerID: userID, PublicRole: "owner"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("unsafe public role accepted: %v", err)
	}
	if err := ValidateMembershipRecord(Membership{WorkspaceID: wsID, UserID: userID, Role: MemberRole("root")}); !errors.Is(err, ErrValidation) {
		t.Fatalf("unknown membership role accepted: %v", err)
	}
	if err := ValidateInviteRecord(Invite{
		Token: NewID("inv_"), WorkspaceID: wsID, CreatedBy: userID,
		Role: RoleMember, Status: InviteStatus("reopened"),
	}); !errors.Is(err, ErrValidation) {
		t.Fatalf("unknown invite status accepted: %v", err)
	}
	if err := ValidateSessionRecord(Session{Token: HashToken("raw"), UserID: "bad\nuid"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid session user accepted: %v", err)
	}
}
