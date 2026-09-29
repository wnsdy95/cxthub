package app

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type OwnerRecoveryView struct {
	Available    bool       `json:"available"`
	Revision     string     `json:"revision"`
	State        string     `json:"state"`
	Ready        bool       `json:"ready"`
	PendingUntil *time.Time `json:"pending_until,omitempty"`
	RepairUntil  *time.Time `json:"repair_until,omitempty"`
}
type PreparedOwnerRecovery struct {
	Revision     string    `json:"revision"`
	Code         string    `json:"code"`
	PendingUntil time.Time `json:"pending_until"`
}

func (s *IdentityService) recoveryStore() (outbound.OwnerRecoveryStore, error) {
	st, ok := s.repositories.(outbound.OwnerRecoveryStore)
	if !ok {
		return nil, domain.ErrFederationUnavailable
	}
	return st, nil
}
func (s *IdentityService) recoveryOwner(ctx context.Context, st outbound.OwnerRecoveryStore, actor, ep, token string, fresh bool) (domain.Session, error) {
	// Raw membership is deliberate: recovery must remain available during IdP
	// failure, but never to a former owner or to an organization-only owner.
	if role, ok := s.EnterpriseRoleOf(ctx, ep, actor); !ok || role != domain.EnterpriseOwner {
		return domain.Session{}, domain.ErrForbidden
	}
	if !strings.HasPrefix(token, "sess_") {
		return domain.Session{}, domain.ErrUnauthorized
	}
	sess, err := st.LockFederationSession(ctx, domain.HashToken(token))
	if err != nil {
		return domain.Session{}, err
	}
	if sess.UserID != actor || sess.Kind != "web" || !time.Now().Before(sess.ExpiresAt) {
		return domain.Session{}, domain.ErrUnauthorized
	}
	if fresh {
		proof, err := s.assessEnterpriseCredential(ctx, actor, ep, token)
		if err != nil {
			return domain.Session{}, err
		}
		if proof.State != "verified" || proof.AuthenticatedAt == nil || !time.Now().Before(proof.AuthenticatedAt.Add(domain.CredentialApprovalFreshness)) {
			return domain.Session{}, domain.ErrRecentIdentityLogin
		}
	}
	return sess, nil
}
func (s *IdentityService) ownerRecoveryView(ctx context.Context, st outbound.OwnerRecoveryStore, sess domain.Session, r domain.OwnerRecovery) (OwnerRecoveryView, error) {
	v := OwnerRecoveryView{Available: true, Revision: r.Revision, State: r.State, Ready: r.State == "ready" && r.ActiveHash != ""}
	now := time.Now().UTC()
	if r.PendingUntil != nil && now.Before(*r.PendingUntil) {
		v.PendingUntil = r.PendingUntil
	}
	lease, err := st.GetOwnerRepairSession(ctx, r.EnterpriseID, sess.Token)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return OwnerRecoveryView{}, err
	}
	if err == nil && lease.UserID == sess.UserID && lease.RecoveryRevision == r.Revision && now.Before(lease.ExpiresAt) && r.State == "consumed" {
		deadline := lease.ExpiresAt
		if sess.ExpiresAt.Before(deadline) {
			deadline = sess.ExpiresAt
		}
		v.RepairUntil = &deadline
	}
	return v, nil
}
func (s *IdentityService) GetOwnerRecovery(ctx context.Context, actor, ep, token string) (OwnerRecoveryView, error) {
	st, err := s.recoveryStore()
	if errors.Is(err, domain.ErrFederationUnavailable) {
		return OwnerRecoveryView{State: "unprepared"}, nil
	}
	if err != nil {
		return OwnerRecoveryView{}, err
	}
	return identityResult(ctx, s, func(ctx context.Context) (OwnerRecoveryView, error) {
		sess, err := s.recoveryOwner(ctx, st, actor, ep, token, false)
		if err != nil {
			return OwnerRecoveryView{}, err
		}
		r, err := st.GetOwnerRecovery(ctx, ep, actor)
		if err != nil {
			return OwnerRecoveryView{}, err
		}
		return s.ownerRecoveryView(ctx, st, sess, r)
	})
}
func (s *IdentityService) PrepareOwnerRecovery(ctx context.Context, actor, ep, token, expected string) (PreparedOwnerRecovery, error) {
	st, err := s.recoveryStore()
	if err != nil {
		return PreparedOwnerRecovery{}, err
	}
	return identityResult(ctx, s, func(ctx context.Context) (PreparedOwnerRecovery, error) {
		sess, err := s.recoveryOwner(ctx, st, actor, ep, token, true)
		if err != nil {
			return PreparedOwnerRecovery{}, err
		}
		r, err := st.GetOwnerRecovery(ctx, ep, actor)
		if err != nil {
			return PreparedOwnerRecovery{}, err
		}
		if r.Revision != expected {
			return PreparedOwnerRecovery{}, domain.ErrConflict
		}
		secret := make([]byte, 32)
		if _, err = rand.Read(secret); err != nil {
			return PreparedOwnerRecovery{}, err
		}
		code := "cxt-recovery-" + base64.RawURLEncoding.EncodeToString(secret)
		until := time.Now().UTC().Add(domain.OwnerRecoveryWindow)
		if sess.ExpiresAt.Before(until) {
			until = sess.ExpiresAt
		}
		r.Revision = domain.NewID("or_")
		r.PendingHash = domain.OwnerRecoveryHash(ep, actor, code)
		r.PendingSession = sess.Token
		r.PendingUntil = &until
		if err = st.PutOwnerRecovery(ctx, r); err != nil {
			return PreparedOwnerRecovery{}, err
		}
		if err = s.enterpriseAudit(ctx, ep, actor, "enterprise.recovery.prepared", r.Revision); err != nil {
			return PreparedOwnerRecovery{}, err
		}
		return PreparedOwnerRecovery{r.Revision, code, until}, nil
	})
}
func recoveryCodeMatches(expected, ep, actor, code string) bool {
	if expected == "" || len(code) != 56 || !strings.HasPrefix(code, "cxt-recovery-") {
		return false
	}
	actual := domain.OwnerRecoveryHash(ep, actor, code)
	return subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) == 1
}
func (s *IdentityService) ConfirmOwnerRecovery(ctx context.Context, actor, ep, token, revision, code string) error {
	st, err := s.recoveryStore()
	if err != nil {
		return err
	}
	return s.withIdentity(ctx, func(ctx context.Context) error {
		sess, err := s.recoveryOwner(ctx, st, actor, ep, token, true)
		if err != nil {
			return err
		}
		r, err := st.GetOwnerRecovery(ctx, ep, actor)
		if err != nil {
			return err
		}
		if r.Revision != revision {
			return domain.ErrConflict
		}
		if r.PendingSession != sess.Token || r.PendingUntil == nil || !time.Now().Before(*r.PendingUntil) || !recoveryCodeMatches(r.PendingHash, ep, actor, code) {
			return domain.ErrUnauthorized
		}
		r.ActiveHash = r.PendingHash
		r.State = "ready"
		r.Revision = domain.NewID("or_")
		r.PendingHash = ""
		r.PendingSession = ""
		r.PendingUntil = nil
		if err = st.PutOwnerRecovery(ctx, r); err != nil {
			return err
		}
		return s.enterpriseAudit(ctx, ep, actor, "enterprise.recovery.confirmed", r.Revision)
	})
}
func (s *IdentityService) RevokeOwnerRecovery(ctx context.Context, actor, ep, token, revision string) error {
	st, err := s.recoveryStore()
	if err != nil {
		return err
	}
	return s.withIdentity(ctx, func(ctx context.Context) error {
		if _, err := s.recoveryOwner(ctx, st, actor, ep, token, true); err != nil {
			return err
		}
		r, err := st.GetOwnerRecovery(ctx, ep, actor)
		if err != nil {
			return err
		}
		if r.Revision != revision {
			return domain.ErrConflict
		}
		r = domain.DefaultOwnerRecovery(ep, actor)
		r.State = "revoked"
		r.Revision = domain.NewID("or_")
		if err = st.PutOwnerRecovery(ctx, r); err != nil {
			return err
		}
		return s.enterpriseAudit(ctx, ep, actor, "enterprise.recovery.revoked", r.Revision)
	})
}
func (s *IdentityService) RedeemOwnerRecovery(ctx context.Context, actor, ep, token, code string) (OwnerRecoveryView, error) {
	st, err := s.recoveryStore()
	if err != nil {
		return OwnerRecoveryView{}, err
	}
	return identityResult(ctx, s, func(ctx context.Context) (OwnerRecoveryView, error) {
		sess, err := s.recoveryOwner(ctx, st, actor, ep, token, false)
		if err != nil {
			return OwnerRecoveryView{}, err
		}
		r, err := st.GetOwnerRecovery(ctx, ep, actor)
		if err != nil {
			return OwnerRecoveryView{}, err
		}
		if !recoveryCodeMatches(r.ActiveHash, ep, actor, code) {
			return OwnerRecoveryView{}, domain.ErrUnauthorized
		}
		r = domain.DefaultOwnerRecovery(ep, actor)
		r.State = "consumed"
		r.Revision = domain.NewID("or_")
		now := time.Now().UTC()
		until := now.Add(domain.OwnerRecoveryWindow)
		if sess.ExpiresAt.Before(until) {
			until = sess.ExpiresAt
		}
		lease := domain.OwnerRepairSession{EnterpriseID: ep, UserID: actor, SessionHash: sess.Token, RecoveryRevision: r.Revision, CreatedAt: now, ExpiresAt: until}
		if err = st.PutOwnerRecovery(ctx, r); err != nil {
			return OwnerRecoveryView{}, err
		}
		if err = st.PutOwnerRepairSession(ctx, lease); err != nil {
			return OwnerRecoveryView{}, err
		}
		if err = s.enterpriseAudit(ctx, ep, actor, "enterprise.recovery.redeemed", r.Revision); err != nil {
			return OwnerRecoveryView{}, err
		}
		return s.ownerRecoveryView(ctx, st, sess, r)
	})
}

// Losing and later regaining ownership must not revive old saved codes or
// repair sessions. The membership mutation and this invalidation commit together.
func (s *IdentityService) invalidateOwnerRecovery(ctx context.Context, ep, user string) error {
	st, ok := s.repositories.(outbound.OwnerRecoveryStore)
	if !ok {
		return nil
	}
	r, err := st.GetOwnerRecovery(ctx, ep, user)
	if err != nil {
		return err
	}
	if r.State == "unprepared" && r.PendingHash == "" {
		return nil
	}
	r = domain.DefaultOwnerRecovery(ep, user)
	r.State = "revoked"
	r.Revision = domain.NewID("or_")
	return st.PutOwnerRecovery(ctx, r)
}
