//go:build postgres

package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGOwnerRecoveryConfirmationRotationAndScopedRedemption(t *testing.T) {
	f, cli, mcp, _ := verifiedAssuranceFixture(t)
	s, ctx, ep, actor := f.team.identity, systemTestContext(), f.enterprise.ID, f.team.owner.ID
	initial, err := s.GetOwnerRecovery(ctx, actor, ep, f.session.Token)
	if err != nil || initial.Ready {
		t.Fatal(initial, err)
	}
	prepared, err := s.PrepareOwnerRecovery(ctx, actor, ep, f.session.Token, initial.Revision)
	if err != nil || prepared.Code == "" {
		t.Fatal("prepare", err)
	}
	pending, err := s.GetOwnerRecovery(ctx, actor, ep, f.session.Token)
	if err != nil || pending.Ready {
		t.Fatal("unconfirmed code is ready", pending, err)
	}
	if _, err = s.RedeemOwnerRecovery(ctx, actor, ep, f.session.Token, prepared.Code); err == nil {
		t.Fatal("unconfirmed redemption")
	}
	peer, err := s.issueSession(ctx, actor, "sess_", "web", "peer", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmOwnerRecovery(ctx, actor, ep, peer.Token, prepared.Revision, prepared.Code); err == nil {
		t.Fatal("other browser confirmed")
	}
	if err = s.ConfirmOwnerRecovery(ctx, actor, ep, f.session.Token, prepared.Revision, prepared.Code); err != nil {
		t.Fatal(err)
	}
	ready, _ := s.GetOwnerRecovery(ctx, actor, ep, f.session.Token)
	if !ready.Ready {
		t.Fatal("confirmation missing")
	}
	// Preparing a replacement does not destroy the owner's saved working code.
	replacement, err := s.PrepareOwnerRecovery(ctx, actor, ep, f.session.Token, ready.Revision)
	if err != nil {
		t.Fatal(err)
	}
	ready, _ = s.GetOwnerRecovery(ctx, actor, ep, f.session.Token)
	if !ready.Ready {
		t.Fatal("pending rotation invalidated recovery")
	}
	if _, err = s.RedeemOwnerRecovery(ctx, actor, ep, cli.Token, prepared.Code); err == nil {
		t.Fatal("CLI used recovery")
	}
	// Recovery survives an unavailable identity connection without issuing SSO evidence.
	if err = s.DisableOIDC(ctx, actor, ep, f.input.Revision); err != nil {
		t.Fatal(err)
	}
	lease, err := s.RedeemOwnerRecovery(ctx, actor, ep, f.session.Token, prepared.Code)
	if err != nil || lease.RepairUntil == nil || lease.Ready {
		t.Fatal(lease, err)
	}
	if _, err = s.RedeemOwnerRecovery(ctx, actor, ep, f.session.Token, prepared.Code); err == nil {
		t.Fatal("code replay accepted")
	}
	if err = s.ConfirmOwnerRecovery(ctx, actor, ep, f.session.Token, replacement.Revision, replacement.Code); err == nil {
		t.Fatal("stale pending survived redemption")
	}
	own, _ := s.GetOwnerRecovery(ctx, actor, ep, f.session.Token)
	other, _ := s.GetOwnerRecovery(ctx, actor, ep, peer.Token)
	if own.RepairUntil == nil || other.RepairUntil != nil {
		t.Fatal("repair escaped browser")
	}
	for _, token := range []string{f.session.Token, cli.Token, mcp.AccessToken} {
		assessment, e := s.AssessEnterpriseCredential(ctx, actor, ep, token)
		if e != nil || assessment.State == "verified" {
			t.Fatal("recovery became SSO", assessment, e)
		}
	}
	stored, _ := f.st.GetOwnerRecovery(ctx, ep, actor)
	if strings.Contains(stored.ActiveHash+stored.PendingHash, prepared.Code) {
		t.Fatal("raw code persisted")
	}
}

func TestPGOwnerRecoveryConcurrentRedemptionAndAuditRollback(t *testing.T) {
	for _, failAudit := range []bool{false, true} {
		t.Run(map[bool]string{false: "concurrent", true: "audit_failure"}[failAudit], func(t *testing.T) {
			f, _, _, _ := verifiedAssuranceFixture(t)
			s, ctx, ep, actor := f.team.identity, systemTestContext(), f.enterprise.ID, f.team.owner.ID
			v, _ := s.GetOwnerRecovery(ctx, actor, ep, f.session.Token)
			p, err := s.PrepareOwnerRecovery(ctx, actor, ep, f.session.Token, v.Revision)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.ConfirmOwnerRecovery(ctx, actor, ep, f.session.Token, p.Revision, p.Code); err != nil {
				t.Fatal(err)
			}
			if failAudit {
				broken := NewIdentityService(nil, domainAuditFailure{f.st})
				if _, err = broken.RedeemOwnerRecovery(ctx, actor, ep, f.session.Token, p.Code); err == nil {
					t.Fatal("audit failure ignored")
				}
				v, _ = s.GetOwnerRecovery(ctx, actor, ep, f.session.Token)
				if !v.Ready || v.RepairUntil != nil {
					t.Fatal("failed transaction changed recovery")
				}
			}
			db, err := store.NewPostgresStore(ctx, collaborationDSN(t))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			restart := NewIdentityService(nil, db)
			start, results := make(chan struct{}), make(chan error, 2)
			for _, svc := range []*IdentityService{s, restart} {
				go func(svc *IdentityService) {
					<-start
					_, e := svc.RedeemOwnerRecovery(ctx, actor, ep, f.session.Token, p.Code)
					results <- e
				}(svc)
			}
			close(start)
			successes := 0
			for range 2 {
				if e := <-results; e == nil {
					successes++
				} else if !errors.Is(e, domain.ErrUnauthorized) {
					t.Error(e)
				}
			}
			if successes != 1 {
				t.Fatalf("redemption successes=%d", successes)
			}
		})
	}
}

func TestPGOwnerRecoveryLeaseInvalidation(t *testing.T) {
	for _, change := range []string{"logout", "expired", "demote", "revoke"} {
		t.Run(change, func(t *testing.T) {
			f, _, _, _ := verifiedAssuranceFixture(t)
			s, ctx, ep, actor := f.team.identity, systemTestContext(), f.enterprise.ID, f.team.owner.ID
			if err := s.UpdateEnterpriseMember(ctx, actor, ep, f.team.outsider.ID, domain.EnterpriseOwner); err != nil {
				t.Fatal(err)
			}
			v, _ := s.GetOwnerRecovery(ctx, actor, ep, f.session.Token)
			p, err := s.PrepareOwnerRecovery(ctx, actor, ep, f.session.Token, v.Revision)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.ConfirmOwnerRecovery(ctx, actor, ep, f.session.Token, p.Revision, p.Code); err != nil {
				t.Fatal(err)
			}
			v, err = s.RedeemOwnerRecovery(ctx, actor, ep, f.session.Token, p.Code)
			if err != nil || v.RepairUntil == nil {
				t.Fatal("no repair evidence", err)
			}
			switch change {
			case "logout":
				err = s.Logout(ctx, f.session.Token)
			case "expired":
				err = s.withIdentity(ctx, func(ctx context.Context) error {
					lease, err := f.st.GetOwnerRepairSession(ctx, ep, domain.HashToken(f.session.Token))
					if err != nil {
						return err
					}
					lease.CreatedAt = time.Now().Add(-time.Hour)
					lease.ExpiresAt = lease.CreatedAt.Add(domain.OwnerRecoveryWindow)
					return f.st.PutOwnerRepairSession(ctx, lease)
				})
			case "demote":
				err = s.UpdateEnterpriseMember(ctx, f.team.outsider.ID, ep, actor, domain.EnterpriseMember)
				if err == nil {
					err = s.UpdateEnterpriseMember(ctx, f.team.outsider.ID, ep, actor, domain.EnterpriseOwner)
				}
			case "revoke":
				err = s.RevokeOwnerRecovery(ctx, actor, ep, f.session.Token, v.Revision)
			}
			if err != nil {
				t.Fatal(err)
			}
			v, err = s.GetOwnerRecovery(ctx, actor, ep, f.session.Token)
			if err == nil && v.RepairUntil != nil {
				t.Fatal("stale repair evidence survived", change)
			}
			if change != "logout" && err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPGOwnerRecoveryRedemptionRacesOwnershipLoss(t *testing.T) {
	f, _, _, _ := verifiedAssuranceFixture(t)
	s, ctx, ep, actor := f.team.identity, systemTestContext(), f.enterprise.ID, f.team.owner.ID
	if err := s.UpdateEnterpriseMember(ctx, actor, ep, f.team.outsider.ID, domain.EnterpriseOwner); err != nil {
		t.Fatal(err)
	}
	v, _ := s.GetOwnerRecovery(ctx, actor, ep, f.session.Token)
	p, err := s.PrepareOwnerRecovery(ctx, actor, ep, f.session.Token, v.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmOwnerRecovery(ctx, actor, ep, f.session.Token, p.Revision, p.Code); err != nil {
		t.Fatal(err)
	}
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	other := NewIdentityService(nil, peer)
	start, used, changed := make(chan struct{}), make(chan error, 1), make(chan error, 1)
	go func() { <-start; _, err := s.RedeemOwnerRecovery(ctx, actor, ep, f.session.Token, p.Code); used <- err }()
	go func() {
		<-start
		changed <- other.UpdateEnterpriseMember(ctx, f.team.outsider.ID, ep, actor, domain.EnterpriseMember)
	}()
	close(start)
	if err := <-changed; err != nil {
		t.Fatal(err)
	}
	if err := <-used; err != nil && !errors.Is(err, domain.ErrForbidden) {
		t.Fatal(err)
	}
	if _, err := s.GetOwnerRecovery(ctx, actor, ep, f.session.Token); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("former owner retained recovery", err)
	}
	if err := other.UpdateEnterpriseMember(ctx, f.team.outsider.ID, ep, actor, domain.EnterpriseOwner); err != nil {
		t.Fatal(err)
	}
	v, err = s.GetOwnerRecovery(ctx, actor, ep, f.session.Token)
	if err != nil || v.Ready || v.RepairUntil != nil {
		t.Fatal("role regrant revived old recovery", v, err)
	}
}
