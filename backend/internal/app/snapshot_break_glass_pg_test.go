//go:build postgres

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type snapshotBreakGlassStore struct {
	*store.PostgresStore
	blockInherited bool
	failAudit      bool
}

func (storage *snapshotBreakGlassStore) RepositoryOrganizationAccess(ctx context.Context, repository, user string) (domain.OrganizationRepositoryAccess, error) {
	if storage.blockInherited {
		return domain.OrganizationRepositoryAccess{}, nil
	}
	return storage.PostgresStore.RepositoryOrganizationAccess(ctx, repository, user)
}

func (storage *snapshotBreakGlassStore) UseActiveBreakGlassGrant(ctx context.Context, organization, repository, user string, now time.Time, event domain.OrganizationAuditEvent) (domain.BreakGlassGrant, error) {
	grant, err := storage.PostgresStore.UseActiveBreakGlassGrant(ctx, organization, repository, user, now, event)
	if err == nil && storage.failAudit {
		return domain.BreakGlassGrant{}, errors.New("injected outer audit transaction failure")
	}
	return grant, err
}

func TestPGSnapshotBreakGlassRequiresExactCommittedUse(test *testing.T) {
	_, postgres, _ := collaborationPG(test)
	ctx := context.Background()
	connection, err := pgxpool.New(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	defer connection.Close()
	for _, scenario := range []string{"unchanged", "newer grant", "deleted grant", "changed grant", "disabled policy", "demoted owner", "unaudited new grant", "audit failure"} {
		test.Run(scenario, func(test *testing.T) {
			storage := &snapshotBreakGlassStore{PostgresStore: postgres}
			identity := NewIdentityService(auth.NewDevVerifier(), storage)
			owner, err := identity.Authenticate(ctx, "dev:"+domain.NewID("bgowner")+"@example.test")
			if err != nil {
				test.Fatal(err)
			}
			admin, err := identity.Authenticate(ctx, "dev:"+domain.NewID("bgadmin")+"@example.test")
			if err != nil {
				test.Fatal(err)
			}
			organization, err := identity.CreateOrganization(ctx, owner, "Snapshot", domain.NewID("snapshot"))
			if err != nil {
				test.Fatal(err)
			}
			if err := identity.UpdateOrganizationMember(ctx, owner.ID, organization.ID, admin.ID, domain.OrganizationAdmin); err != nil {
				test.Fatal(err)
			}
			repository, err := identity.CreateOrganizationRepository(ctx, admin, organization.ID, "snapshot")
			if err != nil {
				test.Fatal(err)
			}
			if scenario == "demoted owner" {
				if err := identity.UpdateOrganizationMember(ctx, owner.ID, organization.ID, admin.ID, domain.OrganizationOwner); err != nil {
					test.Fatal(err)
				}
			}
			grant, err := identity.CreateBreakGlassGrant(ctx, owner.ID, organization.ID, repository.ID, "test emergency access", 5)
			if err != nil {
				test.Fatal(err)
			}
			storage.blockInherited = scenario != "unaudited new grant"
			storage.failAudit = scenario == "audit failure"
			permit, err := identity.AdmitRepositoryRead(ctx, repository.ID, owner.ID)
			if scenario == "audit failure" {
				if err == nil || permit != nil {
					test.Fatal("failed audit issued a permit")
				}
			} else if err != nil {
				test.Fatal("admit", err)
			}
			auditCount := func() int {
				test.Helper()
				events, err := storage.ListOrganizationAudit(ctx, organization.ID, 100)
				if err != nil {
					test.Fatal(err)
				}
				count := 0
				for _, event := range events {
					if event.Action == "organization.break_glass.used" {
						count++
					}
				}
				return count
			}
			wantAudits := 1
			if scenario == "audit failure" || scenario == "unaudited new grant" {
				wantAudits = 0
			}
			if count := auditCount(); count != wantAudits {
				test.Fatalf("committed use audits=%d want=%d", count, wantAudits)
			}
			if scenario == "audit failure" {
				return
			}
			switch scenario {
			case "newer grant", "unaudited new grant":
				if _, err := identity.CreateBreakGlassGrant(ctx, owner.ID, organization.ID, repository.ID, "separate later approval", 10); err != nil {
					test.Fatal(err)
				}
				storage.blockInherited = true
			case "deleted grant":
				_, err = connection.Exec(ctx, `DELETE FROM organization_break_glass_grants WHERE id=$1`, grant.ID)
			case "changed grant":
				_, err = connection.Exec(ctx, `UPDATE organization_break_glass_grants SET reason='changed purpose' WHERE id=$1`, grant.ID)
			case "disabled policy":
				policy, readErr := storage.GetOrganizationPolicy(ctx, organization.ID)
				if readErr != nil {
					test.Fatal(readErr)
				}
				policy.BreakGlassEnabled = false
				err = storage.PutOrganizationPolicy(ctx, policy)
			case "demoted owner":
				_, err = connection.Exec(ctx, `UPDATE organization_memberships SET role=$3 WHERE organization_id=$1 AND user_id=$2`, organization.ID, owner.ID, domain.OrganizationMember)
			}
			if err != nil {
				test.Fatal(err)
			}
			wantAllowed := scenario == "unchanged" || scenario == "newer grant"
			for range 2 {
				if err := postgres.WithinReadSnapshot(ctx, func(bound context.Context) error {
					err := permit.AuthorizeSnapshot(bound, repository.ID, owner.ID)
					if wantAllowed && err != nil || !wantAllowed && !errors.Is(err, domain.ErrForbidden) {
						test.Fatalf("snapshot permission=%v allowed=%t", err, wantAllowed)
					}
					return nil
				}); err != nil {
					test.Fatal(err)
				}
			}
			if auditCount() != wantAudits {
				test.Fatal("snapshot predicate wrote another use audit")
			}
		})
	}
}
