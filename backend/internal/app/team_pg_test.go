//go:build postgres

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGTeamAccessContract(t *testing.T) { _, st, _ := collaborationPG(t); runTeamContract(t, st) }
func TestPGTeamRevocationSerializesWithContextWrites(t *testing.T) {
	_, st, repo := collaborationPG(t)
	f := makeTeamFixture(t, st)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := f.identity.UpdateTeamMember(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.member.ID, domain.TeamMember); err != nil {
		t.Fatal(err)
	}
	if err := f.identity.SetTeamRepository(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.repository.ID, domain.RoleMember); err != nil {
		t.Fatal(err)
	}
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	peerIdentity := NewIdentityService(nil, peer)
	checked, release := make(chan struct{}), make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- st.WithinRepository(ctx, repo, func(ctx context.Context) error {
			if err := st.LockRepositoryAccess(ctx, f.repository.ID, f.member.ID); err != nil {
				return err
			}
			if role, ok := f.identity.RoleOf(ctx, f.repository.ID, f.member.ID); !ok || role != domain.RoleMember {
				return domain.ErrForbidden
			}
			close(checked)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-checked:
	case err := <-writeDone:
		t.Fatalf("write exited early: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	revokeDone := make(chan error, 1)
	go func() {
		revokeDone <- peerIdentity.RemoveTeamMember(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.member.ID)
	}()
	select {
	case err := <-revokeDone:
		t.Fatalf("revocation crossed in-flight authorization: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err = <-writeDone; err != nil {
		t.Fatal(err)
	}
	if err = <-revokeDone; err != nil {
		t.Fatal(err)
	}
	err = st.WithinRepository(ctx, repo, func(ctx context.Context) error {
		if _, ok := f.identity.RoleOf(ctx, f.repository.ID, f.member.ID); ok {
			return errors.New("stale team access")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

type failedTeamAuditStore struct{ *store.PostgresStore }

func (s failedTeamAuditStore) AppendOrganizationAudit(context.Context, domain.OrganizationAuditEvent) error {
	return errors.New("audit unavailable")
}
func TestPGTeamGrantRollsBackWithAuditFailure(t *testing.T) {
	_, st, _ := collaborationPG(t)
	f := makeTeamFixture(t, st)
	svc := NewIdentityService(nil, failedTeamAuditStore{st})
	err := svc.SetTeamRepository(context.Background(), f.owner.ID, f.organization.ID, f.team.ID, f.repository.ID, domain.RoleOwner)
	if err == nil {
		t.Fatal("failed audit was ignored")
	}
	grants, err := st.ListTeamRepositoryGrants(context.Background(), f.team.ID)
	if err != nil || len(grants) != 0 {
		t.Fatalf("grant escaped rollback: %+v %v", grants, err)
	}
}
