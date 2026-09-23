//go:build postgres

package app

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"testing"
	"time"
)

func TestPGOrganizationOwnerAccessContract(t *testing.T) {
	_, st, _ := collaborationPG(t)
	runOrganizationOwnerAccess(t, st)
}
func TestPGOrganizationOwnerCommands(t *testing.T) {
	_, st, _ := collaborationPG(t)
	runOrganizationOwnerCommands(t, st)
}
func TestPGOrganizationOwnerDemotionSerializesWithContextWrites(t *testing.T) {
	_, st, repo := collaborationPG(t)
	f := makeTeamFixture(t, st)
	ctx, cancel := context.WithTimeout(systemTestContext(), 10*time.Second)
	defer cancel()
	if err := f.identity.UpdateOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID, domain.OrganizationOwner); err != nil {
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
			if role, ok := f.identity.RoleOf(ctx, f.repository.ID, f.member.ID); !ok || role != domain.RoleOwner {
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
		revokeDone <- peerIdentity.UpdateOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID, domain.OrganizationMember)
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
			return errors.New("stale organization owner access")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
