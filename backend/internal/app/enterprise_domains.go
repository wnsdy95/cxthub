package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// Configure before serving requests. Network resolution is outside identity
// transactions; the actor, challenge and exclusive claim are rechecked inside.
func (s *IdentityService) WithDomainResolver(r outbound.DomainTXTResolver) *IdentityService {
	s.domainResolver = r
	return s
}

type EnterpriseDomainView struct {
	domain.EnterpriseDomain
	RecordName string `json:"record_name"`
	State      string `json:"state"`
}

func domainView(d domain.EnterpriseDomain) EnterpriseDomainView {
	return EnterpriseDomainView{d, d.RecordName(), d.State(time.Now().UTC())}
}
func (s *IdentityService) domainStore(ctx context.Context, actor, id string, write bool) (outbound.EnterpriseDomainStore, error) {
	min := domain.EnterpriseAdmin
	if write {
		min = domain.EnterpriseOwner
	}
	if role, ok := s.EnterpriseRoleOf(ctx, id, actor); !ok || !role.AtLeast(min) {
		return nil, domain.ErrForbidden
	}
	st, ok := s.repositories.(outbound.EnterpriseDomainStore)
	if !ok || s.domainResolver == nil {
		return nil, domain.ErrDomainVerificationUnavailable
	}
	return st, nil
}
func (s *IdentityService) ListEnterpriseDomains(ctx context.Context, actor, id string) ([]EnterpriseDomainView, error) {
	st, err := s.domainStore(ctx, actor, id, false)
	if err != nil {
		return nil, err
	}
	rows, err := st.ListEnterpriseDomains(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]EnterpriseDomainView, 0, len(rows))
	for _, d := range rows {
		out = append(out, domainView(d))
	}
	return out, nil
}
func (s *IdentityService) RequestEnterpriseDomain(ctx context.Context, actor, id, name, expected string) (EnterpriseDomainView, error) {
	name, err := domain.NormalizeEnterpriseDomain(name)
	if err != nil {
		return EnterpriseDomainView{}, err
	}
	return identityResult(ctx, s, func(ctx context.Context) (EnterpriseDomainView, error) {
		st, err := s.domainStore(ctx, actor, id, true)
		if err != nil {
			return EnterpriseDomainView{}, err
		}
		old, err := st.GetEnterpriseDomain(ctx, id, name)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return EnterpriseDomainView{}, err
		}
		if old.Revision != expected {
			return EnterpriseDomainView{}, domain.ErrConflict
		}
		if errors.Is(err, domain.ErrNotFound) {
			list, err := st.ListEnterpriseDomains(ctx, id)
			if err != nil {
				return EnterpriseDomainView{}, err
			}
			if len(list) >= domain.MaxEnterpriseDomains {
				return EnterpriseDomainView{}, fmt.Errorf("%w: domain limit reached", domain.ErrConflict)
			}
		}
		d := domain.EnterpriseDomain{EnterpriseID: id, Domain: name, Challenge: domain.NewID("cxt-domain="), Revision: domain.NewID("dv_"), ChallengeExpiresAt: time.Now().UTC().Add(domain.EnterpriseDomainChallengeTTL), VerifiedAt: old.VerifiedAt, VerifiedUntil: old.VerifiedUntil}
		if err := st.PutEnterpriseDomain(ctx, d); err != nil {
			return EnterpriseDomainView{}, err
		}
		return domainView(d), s.enterpriseAudit(ctx, id, actor, "enterprise.domain.requested", name)
	})
}
func (s *IdentityService) VerifyEnterpriseDomain(ctx context.Context, actor, id, name, revision string) (EnterpriseDomainView, error) {
	name, err := domain.NormalizeEnterpriseDomain(name)
	if err != nil {
		return EnterpriseDomainView{}, err
	}
	st, err := s.domainStore(ctx, actor, id, true)
	if err != nil {
		return EnterpriseDomainView{}, err
	}
	observed, err := st.GetEnterpriseDomain(ctx, id, name)
	if err != nil {
		return EnterpriseDomainView{}, err
	}
	if observed.Revision != revision || !time.Now().Before(observed.ChallengeExpiresAt) {
		return EnterpriseDomainView{}, domain.ErrConflict
	}
	lookup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// An absolute DNS name must not inherit the host's resolver search suffixes.
	values, err := s.domainResolver.LookupTXT(lookup, observed.RecordName()+".")
	if err != nil {
		return EnterpriseDomainView{}, fmt.Errorf("%w: DNS lookup failed; retry verification", domain.ErrDomainVerificationUnavailable)
	}
	if !slices.Contains(values, observed.Challenge) {
		return EnterpriseDomainView{}, fmt.Errorf("%w: DNS TXT challenge did not match", domain.ErrValidation)
	}
	return identityResult(ctx, s, func(ctx context.Context) (EnterpriseDomainView, error) {
		st, err := s.domainStore(ctx, actor, id, true)
		if err != nil {
			return EnterpriseDomainView{}, err
		}
		current, err := st.GetEnterpriseDomain(ctx, id, name)
		if err != nil {
			return EnterpriseDomainView{}, err
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		if current.Revision != revision || current.Challenge != observed.Challenge || !now.Before(current.ChallengeExpiresAt) {
			return EnterpriseDomainView{}, domain.ErrConflict
		}
		other, err := st.OtherVerifiedEnterpriseDomain(ctx, name, id, now)
		if err != nil {
			return EnterpriseDomainView{}, err
		}
		if other {
			return EnterpriseDomainView{}, domain.ErrConflict
		}
		current.VerifiedAt, current.VerifiedUntil, current.Revision = now, now.Add(domain.EnterpriseDomainVerificationTTL), domain.NewID("dv_")
		if err = st.PutEnterpriseDomain(ctx, current); err != nil {
			return EnterpriseDomainView{}, err
		}
		return domainView(current), s.enterpriseAudit(ctx, id, actor, "enterprise.domain.verified", name)
	})
}
func (s *IdentityService) ReleaseEnterpriseDomain(ctx context.Context, actor, id, name, revision string) error {
	name, err := domain.NormalizeEnterpriseDomain(name)
	if err != nil {
		return err
	}
	return s.withIdentity(ctx, func(ctx context.Context) error {
		st, err := s.domainStore(ctx, actor, id, true)
		if err != nil {
			return err
		}
		current, err := st.GetEnterpriseDomain(ctx, id, name)
		if err != nil {
			return err
		}
		if current.Revision != revision {
			return domain.ErrConflict
		}
		if err = st.RemoveEnterpriseDomain(ctx, id, name); err != nil {
			return err
		}
		return s.enterpriseAudit(ctx, id, actor, "enterprise.domain.released", name)
	})
}
