package app

import (
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type invitationTestStore interface {
	teamTestStore
	outbound.CollaborationInviteStore
	outbound.EnterpriseStore
}

func TestCollaborationInvitations(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	runCollaborationInvitations(t, st)
}
func runCollaborationInvitations(t *testing.T, st invitationTestStore) {
	for _, kind := range []string{"organization", "enterprise"} {
		t.Run(kind, func(t *testing.T) {
			ctx := systemTestContext()
			f := makeTeamFixture(t, st)
			s := f.identity
			space := f.organization.ID
			if kind == "enterprise" {
				e, err := s.CreateEnterprise(ctx, f.owner, "Group", "group-"+domain.NewID("")[:10])
				if err != nil {
					t.Fatal(err)
				}
				space = e.ID
			}
			makeInvite := func(role domain.OrganizationRole) CollaborationInvitation {
				t.Helper()
				i, e := s.CreateCollaborationInvitation(ctx, f.owner.ID, kind, space, "@"+f.outsider.Username, role, 7)
				if e != nil {
					t.Fatal(e)
				}
				return i
			}
			if _, err := s.CreateCollaborationInvitation(ctx, f.member.ID, kind, space, f.outsider.Email, domain.OrganizationOwner, 7); !errors.Is(err, domain.ErrForbidden) {
				t.Fatalf("member invitation: %v", err)
			}
			if _, err := s.CreateCollaborationInvitation(ctx, f.owner.ID, kind, space, "Name <alice@example.test>", domain.OrganizationMember, 7); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("invalid email: %v", err)
			}
			i := makeInvite(domain.OrganizationMember)
			if _, err := s.CreateCollaborationInvitation(ctx, f.owner.ID, kind, space, f.outsider.Email, domain.OrganizationMember, 7); !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("duplicate pending: %v", err)
			}
			inbox, err := s.InvitationInbox(ctx, f.outsider.ID)
			if err != nil || len(inbox) != 1 || inbox[0].ID != i.ID {
				t.Fatalf("inbox: %+v %v", inbox, err)
			}
			if _, err := s.GetCollaborationInvitation(ctx, f.member.ID, i.ID); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("invitation enumeration: %v", err)
			}
			if _, err := s.ActOnCollaborationInvitation(ctx, f.member.ID, i.ID, "accept"); !errors.Is(err, domain.ErrForbidden) {
				t.Fatalf("wrong account accepted: %v", err)
			}
			renewed, err := s.ActOnCollaborationInvitation(ctx, f.owner.ID, i.ID, "resend")
			if err != nil || renewed.ID == i.ID {
				t.Fatalf("renew: %+v %v", renewed, err)
			}
			if _, err := s.ActOnCollaborationInvitation(ctx, f.outsider.ID, i.ID, "accept"); !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("old link accepted: %v", err)
			}
			accepted, err := s.ActOnCollaborationInvitation(ctx, f.outsider.ID, renewed.ID, "accept")
			if err != nil || accepted.Status != "accepted" {
				t.Fatalf("accept: %+v %v", accepted, err)
			}
			if s.collaborationRole(ctx, f.outsider.ID, kind, space) != domain.OrganizationMember {
				t.Fatal("membership missing")
			}
			if _, err := s.ActOnCollaborationInvitation(ctx, f.outsider.ID, renewed.ID, "accept"); err != nil {
				t.Fatal("non-idempotent", err)
			}
			if kind == "organization" {
				err = s.OffboardOrganizationMember(ctx, f.owner.ID, space, f.outsider.ID, "revoke")
			} else {
				err = s.RemoveEnterpriseMember(ctx, f.owner.ID, space, f.outsider.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.ActOnCollaborationInvitation(ctx, f.outsider.ID, renewed.ID, "accept"); !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("accepted link restored revoked membership: %v", err)
			}
			i = makeInvite(domain.OrganizationMember)
			expired := i.CollaborationInvite
			expired.CreatedAt = time.Now().Add(-8 * 24 * time.Hour)
			expired.ExpiresAt = time.Now().Add(-time.Hour)
			if err := st.PutCollaborationInvite(ctx, expired); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ActOnCollaborationInvitation(ctx, f.outsider.ID, i.ID, "accept"); !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("expired link: %v", err)
			}
			i = makeInvite(domain.OrganizationMember)
			if _, err := s.ActOnCollaborationInvitation(ctx, f.outsider.ID, i.ID, "decline"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ActOnCollaborationInvitation(ctx, f.outsider.ID, i.ID, "accept"); !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("declined link: %v", err)
			}
			i = makeInvite(domain.OrganizationOwner)
			// A different owner demotes the inviter before acceptance.
			if kind == "organization" {
				err = s.UpdateOrganizationMember(ctx, f.owner.ID, space, f.member.ID, domain.OrganizationOwner)
				if err == nil {
					err = s.UpdateOrganizationMember(ctx, f.member.ID, space, f.owner.ID, domain.OrganizationAdmin)
				}
			} else {
				err = s.UpdateEnterpriseMember(ctx, f.owner.ID, space, f.member.ID, domain.EnterpriseOwner)
				if err == nil {
					err = s.UpdateEnterpriseMember(ctx, f.member.ID, space, f.owner.ID, domain.EnterpriseAdmin)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.ActOnCollaborationInvitation(ctx, f.outsider.ID, i.ID, "accept"); !errors.Is(err, domain.ErrForbidden) {
				t.Fatalf("demoted issuer granted owner: %v", err)
			}
			if _, err := s.ActOnCollaborationInvitation(ctx, f.member.ID, i.ID, "revoke"); err != nil {
				t.Fatal(err)
			}
			// Inviting an existing owner as a member must never downgrade them.
			i, err = s.CreateCollaborationInvitation(ctx, f.member.ID, kind, space, f.member.Email, domain.OrganizationMember, 7)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ActOnCollaborationInvitation(ctx, f.member.ID, i.ID, "accept"); err != nil {
				t.Fatal(err)
			}
			if s.collaborationRole(ctx, f.member.ID, kind, space) != domain.OrganizationOwner {
				t.Fatal("owner downgraded")
			}
		})
	}
}
