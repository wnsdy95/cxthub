//go:build postgres

package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGInvitationEmailLifecycle(t *testing.T) {
	_, st, _ := collaborationPG(t)
	runInvitationEmailLifecycle(t, st)
}
func TestPGInvitationEmailConcurrentClaims(t *testing.T) {
	_, st, _ := collaborationPG(t)
	peer, err := store.NewPostgresStore(context.Background(), collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	runInvitationEmailConcurrentClaims(t, st, peer)
}

type failedEmailQueueStore struct{ *store.PostgresStore }

func (failedEmailQueueStore) PutInvitationEmail(context.Context, domain.InvitationEmail) error {
	return errors.New("queue unavailable")
}
func TestPGInvitationEmailQueueFailureRollsBack(t *testing.T) {
	_, st, _ := collaborationPG(t)
	f := makeTeamFixture(t, st)
	ctx := systemTestContext()
	s := NewIdentityService(nil, failedEmailQueueStore{st})
	if err := s.ConfigureInvitationEmail(&fakeInvitationMailer{}, "from@example.test", "https://example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCollaborationInvitation(ctx, f.owner.ID, "organization", f.organization.ID, f.outsider.Email, domain.OrganizationMember, 7); err == nil {
		t.Fatal("queue failure ignored")
	}
	all, err := st.ListCollaborationInvites(ctx, "organization", f.organization.ID, "")
	if err != nil || len(all) != 0 {
		t.Fatal("invite escaped rollback", all, err)
	}
	audits, err := st.ListOrganizationAudit(ctx, f.organization.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range audits {
		if a.Action == "invitation.created" {
			t.Fatal("audit escaped rollback")
		}
	}
}

func TestPGInvitationEmailStaleWorkerFence(t *testing.T) {
	_, st, _ := collaborationPG(t)
	runInvitationEmailStaleWorkerFence(t, st)
}

func TestPGInvitationEmailRenewalRollsBack(t *testing.T) {
	_, st, _ := collaborationPG(t)
	f := makeTeamFixture(t, st)
	ctx := systemTestContext()
	i, err := f.identity.CreateCollaborationInvitation(ctx, f.owner.ID, "organization", f.organization.ID, f.outsider.Email, domain.OrganizationMember, 7)
	if err != nil {
		t.Fatal(err)
	}
	s := NewIdentityService(nil, failedEmailQueueStore{st})
	if err := s.ConfigureInvitationEmail(&fakeInvitationMailer{}, "from@example.test", "https://example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActOnCollaborationInvitation(ctx, f.owner.ID, i.ID, "resend"); err == nil {
		t.Fatal("queue failure ignored")
	}
	all, err := st.ListCollaborationInvites(ctx, "organization", f.organization.ID, "")
	if err != nil || len(all) != 1 || all[0].ID != i.ID || all[0].Status != "pending" {
		t.Fatal("renewal escaped rollback", all, err)
	}
}
