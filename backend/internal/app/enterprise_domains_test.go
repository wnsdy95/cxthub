package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type unusedDomainResolver struct{}

func (unusedDomainResolver) LookupTXT(context.Context, string) ([]string, error) {
	panic("unavailable storage must not query DNS")
}
func TestEnterpriseDomainsRequireSharedTransactions(t *testing.T) {
	f := makeTeamFixture(t, store.NewFSStore(t.TempDir()))
	ctx := systemTestContext()
	s := f.identity.WithDomainResolver(unusedDomainResolver{})
	e, err := s.CreateEnterprise(ctx, f.owner, "Domains", "domains")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestEnterpriseDomain(ctx, f.owner.ID, e.ID, "example.test", ""); !errors.Is(err, domain.ErrDomainVerificationUnavailable) {
		t.Fatal(err)
	}
	if _, err := s.ListEnterpriseDomains(ctx, f.owner.ID, e.ID); !errors.Is(err, domain.ErrDomainVerificationUnavailable) {
		t.Fatal(err)
	}
}
