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

type domainTXTFunc func(context.Context, string) ([]string, error)

func (f domainTXTFunc) LookupTXT(ctx context.Context, name string) ([]string, error) {
	return f(ctx, name)
}

func domainPGFixture(t *testing.T) (*store.PostgresStore, teamFixture, domain.Enterprise) {
	t.Helper()
	_, st, _ := collaborationPG(t)
	f := makeTeamFixture(t, st)
	f.identity.WithDomainResolver(domainTXTFunc(func(context.Context, string) ([]string, error) { return nil, nil }))
	e, err := f.identity.CreateEnterprise(systemTestContext(), f.owner, "Domains", "domain-"+domain.NewID("")[:12])
	if err != nil {
		t.Fatal(err)
	}
	return st, f, e
}
func TestPGEnterpriseDomainChallengeLifecycle(t *testing.T) {
	st, f, e := domainPGFixture(t)
	ctx := systemTestContext()
	s := f.identity
	name := domain.NewID("") + ".example.test"
	if _, err := s.RequestEnterpriseDomain(ctx, f.member.ID, e.ID, name, ""); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("member requested domain", err)
	}
	d, err := s.RequestEnterpriseDomain(ctx, f.owner.ID, e.ID, name, "")
	if err != nil {
		t.Fatal(err)
	}
	if d.State != "pending" {
		t.Fatal(d.State)
	}
	if _, err = s.VerifyEnterpriseDomain(ctx, f.owner.ID, e.ID, name, d.Revision); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("wrong DNS accepted", err)
	}
	s.WithDomainResolver(domainTXTFunc(func(_ context.Context, record string) ([]string, error) {
		if record != d.RecordName+"." {
			t.Error(record)
		}
		return []string{d.Challenge}, nil
	}))
	v, err := s.VerifyEnterpriseDomain(ctx, f.owner.ID, e.ID, name, d.Revision)
	if err != nil || v.State != "verified" {
		t.Fatal(v.State, err)
	}
	if _, err = s.VerifyEnterpriseDomain(ctx, f.owner.ID, e.ID, name, d.Revision); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("stale proof replayed", err)
	}
	if _, ok := s.EnterpriseRoleOf(ctx, e.ID, f.member.ID); ok {
		t.Fatal("domain granted membership")
	}
	if _, err = s.RequestEnterpriseDomain(ctx, f.owner.ID, e.ID, name, ""); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("stale renewal rotated proof", err)
	}
	r, err := s.RequestEnterpriseDomain(ctx, f.owner.ID, e.ID, name, v.Revision)
	if err != nil || r.Challenge == d.Challenge || r.VerifiedUntil != v.VerifiedUntil {
		t.Fatal("renewal lost existing observation", err)
	}
	if err = s.ReleaseEnterpriseDomain(ctx, f.owner.ID, e.ID, name, v.Revision); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("stale release", err)
	}
	if err = s.ReleaseEnterpriseDomain(ctx, f.owner.ID, e.ID, name, r.Revision); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListEnterpriseDomains(ctx, f.owner.ID, e.ID)
	if err != nil || len(list) != 0 {
		t.Fatal(list, err)
	}
	audit, err := st.ListEnterpriseAudit(ctx, e.ID, 100)
	if err != nil || audit[0].Action != "enterprise.domain.released" {
		t.Fatal("missing audit", err)
	}
}

func TestPGEnterpriseDomainVerificationRaces(t *testing.T) {
	for _, action := range []string{"rotate", "release", "revoke"} {
		t.Run(action, func(t *testing.T) {
			st, f, e := domainPGFixture(t)
			ctx := systemTestContext()
			s := f.identity
			name := domain.NewID("") + ".example.test"
			if err := s.UpdateEnterpriseMember(ctx, f.owner.ID, e.ID, f.outsider.ID, domain.EnterpriseOwner); err != nil {
				t.Fatal(err)
			}
			d, err := s.RequestEnterpriseDomain(ctx, f.owner.ID, e.ID, name, "")
			if err != nil {
				t.Fatal(err)
			}
			arrived, release := make(chan struct{}), make(chan struct{})
			s.WithDomainResolver(domainTXTFunc(func(ctx context.Context, _ string) ([]string, error) {
				close(arrived)
				select {
				case <-release:
					return []string{d.Challenge}, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}))
			result := make(chan error, 1)
			go func() { _, err := s.VerifyEnterpriseDomain(ctx, f.owner.ID, e.ID, name, d.Revision); result <- err }()
			select {
			case <-arrived:
			case <-time.After(5 * time.Second):
				t.Fatal("DNS never started")
			}
			other := NewIdentityService(nil, st).WithDomainResolver(domainTXTFunc(func(context.Context, string) ([]string, error) { return nil, nil }))
			switch action {
			case "rotate":
				_, err = other.RequestEnterpriseDomain(ctx, f.outsider.ID, e.ID, name, d.Revision)
			case "release":
				err = other.ReleaseEnterpriseDomain(ctx, f.outsider.ID, e.ID, name, d.Revision)
			case "revoke":
				err = other.RemoveEnterpriseMember(ctx, f.outsider.ID, e.ID, f.owner.ID)
			}
			close(release)
			if err != nil {
				t.Fatal(err)
			}
			if err := <-result; err == nil {
				t.Fatal("in-flight proof survived", action)
			}
			current, err := st.GetEnterpriseDomain(ctx, e.ID, name)
			if err == nil && current.Verified(time.Now()) {
				t.Fatal("stale DNS observation published")
			}
		})
	}
}

func TestPGEnterpriseDomainConcurrentClaimsAcrossServers(t *testing.T) {
	st, f, first := domainPGFixture(t)
	ctx := systemTestContext()
	second, err := f.identity.CreateEnterprise(ctx, f.owner, "Other", "other-"+domain.NewID("")[:12])
	if err != nil {
		t.Fatal(err)
	}
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	name := domain.NewID("") + ".example.test"
	a, err := f.identity.RequestEnterpriseDomain(ctx, f.owner.ID, first.ID, name, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.identity.RequestEnterpriseDomain(ctx, f.owner.ID, second.ID, name, "")
	if err != nil {
		t.Fatal(err)
	}
	arrived, release := make(chan struct{}, 2), make(chan struct{})
	resolve := domainTXTFunc(func(ctx context.Context, _ string) ([]string, error) {
		arrived <- struct{}{}
		select {
		case <-release:
			return []string{a.Challenge, b.Challenge}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	s1, s2 := NewIdentityService(nil, st).WithDomainResolver(resolve), NewIdentityService(nil, peer).WithDomainResolver(resolve)
	results := make(chan error, 2)
	go func() {
		_, err := s1.VerifyEnterpriseDomain(ctx, f.owner.ID, first.ID, name, a.Revision)
		results <- err
	}()
	go func() {
		_, err := s2.VerifyEnterpriseDomain(ctx, f.owner.ID, second.ID, name, b.Revision)
		results <- err
	}()
	for range 2 {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("DNS held identity lock")
		}
	}
	close(release)
	success := 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else if !errors.Is(err, domain.ErrConflict) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatal("global domain acquired by", success, "enterprises")
	}
}

type domainAuditFailure struct{ *store.PostgresStore }

func (domainAuditFailure) AppendEnterpriseAudit(context.Context, domain.EnterpriseAuditEvent) error {
	return errors.New("synthetic audit failure")
}
func TestPGEnterpriseDomainAuditRollbackAndExpiredChallenge(t *testing.T) {
	st, f, e := domainPGFixture(t)
	ctx := systemTestContext()
	name := domain.NewID("") + ".example.test"
	s := NewIdentityService(nil, domainAuditFailure{st}).WithDomainResolver(domainTXTFunc(func(context.Context, string) ([]string, error) { return nil, nil }))
	if _, err := s.RequestEnterpriseDomain(ctx, f.owner.ID, e.ID, name, ""); err == nil {
		t.Fatal("missing audit accepted")
	}
	if _, err := st.GetEnterpriseDomain(ctx, e.ID, name); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("failed request persisted", err)
	}
	d, err := f.identity.RequestEnterpriseDomain(ctx, f.owner.ID, e.ID, name, "")
	if err != nil {
		t.Fatal(err)
	}
	s.WithDomainResolver(domainTXTFunc(func(context.Context, string) ([]string, error) { return []string{d.Challenge}, nil }))
	if _, err := s.VerifyEnterpriseDomain(ctx, f.owner.ID, e.ID, name, d.Revision); err == nil {
		t.Fatal("verification without audit accepted")
	}
	if err := s.ReleaseEnterpriseDomain(ctx, f.owner.ID, e.ID, name, d.Revision); err == nil {
		t.Fatal("release without audit accepted")
	}
	retained, err := st.GetEnterpriseDomain(ctx, e.ID, name)
	if err != nil || retained.Revision != d.Revision || !retained.VerifiedAt.IsZero() {
		t.Fatal("failed verification/release changed claim", err)
	}
	expired := d.EnterpriseDomain
	expired.ChallengeExpiresAt = time.Now().Add(-time.Second)
	if err := st.WithinIdentity(ctx, func(ctx context.Context) error { return st.PutEnterpriseDomain(ctx, expired) }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.identity.VerifyEnterpriseDomain(ctx, f.owner.ID, e.ID, name, d.Revision); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("expired challenge accepted", err)
	}
}

func TestPGEnterpriseDomainExpiredClaimCanBeReverifiedByAnotherEnterprise(t *testing.T) {
	st, f, first := domainPGFixture(t)
	ctx := systemTestContext()
	second, err := f.identity.CreateEnterprise(ctx, f.owner, "New holder", "holder-"+domain.NewID("")[:12])
	if err != nil {
		t.Fatal(err)
	}
	name := domain.NewID("") + ".example.test"
	a, err := f.identity.RequestEnterpriseDomain(ctx, f.owner.ID, first.ID, name, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.identity.RequestEnterpriseDomain(ctx, f.owner.ID, second.ID, name, "")
	if err != nil {
		t.Fatal(err)
	}
	s := f.identity.WithDomainResolver(domainTXTFunc(func(context.Context, string) ([]string, error) { return []string{a.Challenge, b.Challenge}, nil }))
	a, err = s.VerifyEnterpriseDomain(ctx, f.owner.ID, first.ID, name, a.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyEnterpriseDomain(ctx, f.owner.ID, second.ID, name, b.Revision); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("active claim displaced", err)
	}
	expired := a.EnterpriseDomain
	expired.VerifiedAt = time.Now().Add(-2 * time.Hour)
	expired.VerifiedUntil = time.Now().Add(-time.Hour)
	if err := st.WithinIdentity(ctx, func(ctx context.Context) error { return st.PutEnterpriseDomain(ctx, expired) }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyEnterpriseDomain(ctx, f.owner.ID, second.ID, name, b.Revision); err != nil {
		t.Fatal("expired claim blocked new proof", err)
	}
	if _, err := s.VerifyEnterpriseDomain(ctx, f.owner.ID, first.ID, name, a.Revision); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("previous holder displaced new claim", err)
	}
}
