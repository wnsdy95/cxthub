//go:build postgres

package app

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGCollaborationInvitations(t *testing.T) {
	_, st, _ := collaborationPG(t)
	runCollaborationInvitations(t, st)
}
func TestPGInvitationConcurrentAcceptAndCancellation(t *testing.T) {
	_, st, _ := collaborationPG(t)
	ctx := systemTestContext()
	f := makeTeamFixture(t, st)
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	other := NewIdentityService(nil, peer)
	i, err := f.identity.CreateCollaborationInvitation(ctx, f.owner.ID, "organization", f.organization.ID, f.outsider.Email, domain.OrganizationMember, 7)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 16)
	var wg sync.WaitGroup
	for n := 0; n < 16; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			svc, actor, action := f.identity, f.outsider.ID, "accept"
			if n%2 == 0 {
				svc, actor, action = other, f.owner.ID, "revoke"
			}
			_, err := svc.ActOnCollaborationInvitation(ctx, actor, i.ID, action)
			results <- err
		}(n)
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil && !errors.Is(err, domain.ErrConflict) {
			t.Fatal(err)
		}
	}
	final, err := st.GetCollaborationInvite(ctx, i.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, member := f.identity.OrganizationRoleOf(ctx, f.organization.ID, f.outsider.ID)
	if member != (final.Status == "accepted") {
		t.Fatalf("membership and decision disagree: %+v member=%v", final, member)
	}
	audits, err := st.ListOrganizationAudit(ctx, f.organization.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	decisions := 0
	for _, a := range audits {
		if a.TargetID == i.ID && (a.Action == "invitation.accept" || a.Action == "invitation.revoke") {
			decisions++
		}
	}
	if decisions != 1 {
		t.Fatalf("terminal decisions=%d", decisions)
	}
}

type failedInvitationAuditStore struct{ *store.PostgresStore }

func (s failedInvitationAuditStore) AppendOrganizationAudit(context.Context, domain.OrganizationAuditEvent) error {
	return errors.New("audit unavailable")
}
func TestPGInvitationAcceptanceRollsBackWithAudit(t *testing.T) {
	_, st, _ := collaborationPG(t)
	ctx := systemTestContext()
	f := makeTeamFixture(t, st)
	i, err := f.identity.CreateCollaborationInvitation(ctx, f.owner.ID, "organization", f.organization.ID, f.outsider.Email, domain.OrganizationMember, 7)
	if err != nil {
		t.Fatal(err)
	}
	broken := NewIdentityService(nil, failedInvitationAuditStore{st})
	if _, err = broken.ActOnCollaborationInvitation(ctx, f.outsider.ID, i.ID, "accept"); err == nil {
		t.Fatal("missing audit accepted")
	}
	if _, ok := f.identity.OrganizationRoleOf(ctx, f.organization.ID, f.outsider.ID); ok {
		t.Fatal("membership escaped rollback")
	}
	current, err := st.GetCollaborationInvite(ctx, i.ID)
	if err != nil || current.Status != "pending" {
		t.Fatalf("invitation escaped rollback: %+v %v", current, err)
	}
}
