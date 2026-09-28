//go:build postgres

package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGMCPGrantLifecycle(t *testing.T) {
	_, st, _ := collaborationPG(t)
	runMCPGrantReplay(t, st)
	runMCPGrantLifecycle(t, st)
}

func TestPGMCPGrantConcurrentReplayAndRestart(t *testing.T) {
	_, st, _ := collaborationPG(t)
	ctx := systemTestContext()
	u := grantUser(t, st)
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	s := NewIdentityService(nil, st)
	other := NewIdentityService(nil, peer)
	p, err := s.IssueMCPTokenPair(ctx, u.ID, "client")
	if err != nil {
		t.Fatal(err)
	}
	independent, err := s.IssueMCPTokenPair(ctx, u.ID, "client")
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		pair domain.OAuthTokenPair
		err  error
	}
	done := make(chan result, 2)
	start := make(chan struct{})
	for _, svc := range []*IdentityService{s, other} {
		go func(svc *IdentityService) {
			<-start
			p, e := svc.RefreshMCPAccessToken(ctx, p.RefreshToken, "client")
			done <- result{p, e}
		}(svc)
	}
	close(start)
	success := 0
	for i := 0; i < 2; i++ {
		r := <-done
		if r.err == nil {
			success++
			if _, err = other.ResolveMCPUser(ctx, r.pair.AccessToken); err != nil && !errors.Is(err, domain.ErrUnauthorized) {
				t.Fatal(err)
			}
		} else if !errors.Is(r.err, domain.ErrUnauthorized) {
			t.Fatal(r.err)
		}
	}
	if success != 1 {
		t.Fatalf("rotation successes %d", success)
	}
	sessions, err := st.ListSessionsForUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	original := tokenGrant(t, st, p.AccessToken)
	for _, sess := range sessions {
		if sess.GrantID == original.ID {
			if err = s.checkMCPGrant(ctx, sess); !errors.Is(err, domain.ErrUnauthorized) {
				t.Fatal("race left live authorization", err)
			}
		}
	}
	// A separately constructed service reads the committed database outcome.
	if _, err = NewIdentityService(nil, peer).ResolveMCPUser(ctx, independent.AccessToken); err != nil {
		t.Fatal("independent grant changed", err)
	}
	audit, err := s.AccountAudit(ctx, u.ID)
	if err != nil || len(audit) != 1 || audit[0].Action != "mcp.refresh_reused" {
		t.Fatal("replay audit", err)
	}
}

type failGrantSessionStore struct{ *store.PostgresStore }

func (s failGrantSessionStore) CreateSession(ctx context.Context, sess domain.Session) error {
	if sess.Kind == "mcp_refresh" {
		return errors.New("injected refresh persistence failure")
	}
	return s.PostgresStore.CreateSession(ctx, sess)
}

func TestPGMCPGrantFailureRollbacks(t *testing.T) {
	_, st, _ := collaborationPG(t)
	ctx := systemTestContext()
	u := grantUser(t, st)
	s := NewIdentityService(nil, st)
	p, err := s.IssueMCPTokenPair(ctx, u.ID, "client")
	if err != nil {
		t.Fatal(err)
	}
	original := tokenGrant(t, st, p.AccessToken)
	broken := NewIdentityService(nil, failGrantSessionStore{st})
	if _, err = broken.RefreshMCPAccessToken(ctx, p.RefreshToken, "client"); err == nil {
		t.Fatal("failed token issuance accepted")
	}
	sessions, err := st.ListSessionsForUser(ctx, u.ID)
	if err != nil || len(sessions) != 2 {
		t.Fatal("partial tokens escaped rollback", len(sessions), err)
	}
	if after := tokenGrant(t, st, p.AccessToken); !after.ExpiresAt.Equal(original.ExpiresAt) {
		t.Fatal("failed rotation advanced grant")
	}
	q, err := s.RefreshMCPAccessToken(ctx, p.RefreshToken, "client")
	if err != nil {
		t.Fatal("failed rotation consumed original", err)
	}
	broken = NewIdentityService(nil, failedAccountAuditStore{st})
	if _, err = broken.RefreshMCPAccessToken(ctx, p.RefreshToken, "client"); err == nil || errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("audit failure reported as committed rejection", err)
	}
	if _, err = s.ResolveMCPUser(ctx, q.AccessToken); err != nil {
		t.Fatal("revocation escaped audit rollback", err)
	}
	if _, err = s.RefreshMCPAccessToken(ctx, p.RefreshToken, "client"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, err = s.ResolveMCPUser(ctx, q.AccessToken); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("successful replay revocation rolled back", err)
	}
}

func TestPGMCPGrantTokenRevocationRacesRotation(t *testing.T) {
	_, st, _ := collaborationPG(t)
	ctx := systemTestContext()
	u := grantUser(t, st)
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	s := NewIdentityService(nil, st)
	other := NewIdentityService(nil, peer)
	p, err := s.IssueMCPTokenPair(ctx, u.ID, "client")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	done := make(chan error, 1)
	go func() { <-start; done <- other.RevokeMCPToken(ctx, p.RefreshToken, "client") }()
	close(start)
	q, err := s.RefreshMCPAccessToken(ctx, p.RefreshToken, "client")
	if err != nil && !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	for _, pair := range []domain.OAuthTokenPair{p, q} {
		if pair.AccessToken != "" {
			if _, err = s.ResolveMCPUser(ctx, pair.AccessToken); !errors.Is(err, domain.ErrUnauthorized) {
				t.Fatal("revocation raced access", err)
			}
			if _, err = s.RefreshMCPAccessToken(ctx, pair.RefreshToken, "client"); !errors.Is(err, domain.ErrUnauthorized) {
				t.Fatal("revocation raced refresh", err)
			}
		}
	}
}
