//go:build postgres

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type failedInviteTargetStore struct {
	*store.PostgresStore
	fail string
}

func (s *failedInviteTargetStore) AddMember(ctx context.Context, m domain.Membership) error {
	if m.RepositoryID == s.fail {
		return errors.New("second invitation target unavailable")
	}
	return s.PostgresStore.AddMember(ctx, m)
}

func TestPGLegacyInvitationAcceptsAllOriginalTargetsAtomically(t *testing.T) {
	_, st, _ := collaborationPG(t)
	ctx := context.Background()
	f := makeTeamFixture(t, st)
	other, err := f.identity.CreateOrganizationRepository(ctx, f.owner, f.organization.ID, "Second")
	if err != nil {
		t.Fatal(err)
	}
	first, second := f.repository, other
	if first.ID > second.ID {
		first, second = second, first
	}
	invite, err := f.identity.Invite(ctx, f.owner.ID, first.ID, "", domain.RolePuller, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	db, err := pgxpool.New(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The ownership migration records the original container's complete scope.
	if _, err = db.Exec(ctx, `INSERT INTO repository_invite_targets(token,repository_id) VALUES($1,$2),($1,$3)`, invite.Token, first.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	wrapper := &failedInviteTargetStore{PostgresStore: st, fail: second.ID}
	service := NewIdentityService(nil, wrapper)
	if _, err = service.AcceptInvite(ctx, f.outsider, invite.Token); err == nil {
		t.Fatal("partial acceptance succeeded")
	}
	for _, id := range []string{first.ID, second.ID} {
		if member, e := st.IsMember(ctx, id, f.outsider.ID); e != nil || member {
			t.Fatalf("partial membership survived: %t %v", member, e)
		}
	}
	if current, e := st.GetInvite(ctx, invite.Token); e != nil || current.Status != domain.InvitePending {
		t.Fatalf("invitation consumed after rollback: %+v %v", current, e)
	}
	wrapper.fail = ""
	if _, err = service.AcceptInvite(ctx, f.outsider, invite.Token); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{first.ID, second.ID} {
		if role, ok := service.RoleOf(ctx, id, f.outsider.ID); !ok || role != domain.RolePuller {
			t.Fatalf("legacy target lost: %s %v", role, ok)
		}
	}
}
