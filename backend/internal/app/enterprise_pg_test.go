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

func TestPGEnterpriseHierarchyAndPolicyComposition(t *testing.T) {
	_, st, _ := collaborationPG(t)
	runEnterpriseContract(t, st)
}

func TestPGOrganizationOffboarding(t *testing.T) {
	_, st, _ := collaborationPG(t)
	runOrganizationOffboard(t, st)
}

type failingOffboardAudit struct{ *store.PostgresStore }

func (s failingOffboardAudit) RemoveOrganizationMemberWithAudit(context.Context, string, string, domain.OrganizationAuditEvent) error {
	return errors.New("audit unavailable")
}
func TestPGOrganizationOffboardingRollsBackAllDirectGrants(t *testing.T) {
	_, st, _ := collaborationPG(t)
	ctx := context.Background()
	f := makeTeamFixture(t, st)
	if err := st.AddMember(ctx, domain.Membership{RepositoryID: f.repository.ID, UserID: f.member.ID, Role: domain.RolePuller}); err != nil {
		t.Fatal(err)
	}
	s := NewIdentityService(nil, failingOffboardAudit{st})
	if err := s.OffboardOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID, "revoke"); err == nil {
		t.Fatal("missing audit accepted")
	}
	if role, ok := s.RoleOf(ctx, f.repository.ID, f.member.ID); !ok || role != domain.RolePuller {
		t.Fatal("direct grant lost despite rollback")
	}
	if _, ok := s.OrganizationRoleOf(ctx, f.organization.ID, f.member.ID); !ok {
		t.Fatal("membership lost despite rollback")
	}
}

func TestPGEnterpriseConcurrentOwnersAndOrganizationLink(t *testing.T) {
	_, st, _ := collaborationPG(t)
	ctx := context.Background()
	f := makeTeamFixture(t, st)
	e, err := f.identity.CreateEnterprise(ctx, f.owner, "Group", "group-"+domain.NewID("")[:12])
	if err != nil {
		t.Fatal(err)
	}
	if err = f.identity.UpdateEnterpriseMember(ctx, f.owner.ID, e.ID, f.member.ID, domain.EnterpriseOwner); err != nil {
		t.Fatal(err)
	}
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	other := NewIdentityService(nil, peer)
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; results <- f.identity.RemoveEnterpriseMember(ctx, f.owner.ID, e.ID, f.member.ID) }()
	go func() { <-start; results <- other.RemoveEnterpriseMember(ctx, f.member.ID, e.ID, f.owner.ID) }()
	close(start)
	success := 0
	for i := 0; i < 2; i++ {
		if <-results == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("concurrent removals succeeded %d times", success)
	}
	members, err := st.ListEnterpriseMembers(ctx, e.ID)
	if err != nil || len(members) != 1 || members[0].Role != domain.EnterpriseOwner {
		t.Fatalf("final owner: %+v %v", members, err)
	}

	// Two servers cannot attach the same organization to different parents.
	first, err := f.identity.CreateEnterprise(ctx, f.owner, "First", "first-"+domain.NewID("")[:12])
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.identity.CreateEnterprise(ctx, f.owner, "Second", "second-"+domain.NewID("")[:12])
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	outcomes := make(chan error, 2)
	for index, enterprise := range []domain.Enterprise{first, second} {
		go func() {
			defer wg.Done()
			service := f.identity
			if index == 1 {
				service = other
			}
			outcomes <- service.LinkEnterpriseOrganization(ctx, f.owner.ID, enterprise.ID, f.organization.ID, true)
		}()
	}
	wg.Wait()
	close(outcomes)
	success = 0
	for result := range outcomes {
		if result == nil {
			success++
		} else if !errors.Is(result, domain.ErrConflict) {
			t.Fatal(result)
		}
	}
	if success != 1 {
		t.Fatalf("organization acquired %d parent enterprises", success)
	}
}
