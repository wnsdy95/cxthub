//go:build postgres

package store

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"testing"
	"time"
)

func checkPairingRollbackPG(t *testing.T, s *PostgresStore) {
	ctx := context.Background()
	now := time.Now().UTC()
	p := domain.DevicePairing{Code: "ROLLBACK", PollHash: domain.HashToken("rollback"), ExpiresAt: now.Add(time.Minute)}
	user := domain.User{ID: "pairing-rollback", Username: "pairing-rollback", Name: "Test", Email: "rollback@example.test"}
	if err := s.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateDevicePairing(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveDevicePairing(ctx, p.Code, user.ID, now); err != nil {
		t.Fatal(err)
	}
	sess := domain.Session{Token: domain.HashToken("duplicate-token"), UserID: user.ID, Kind: "cli", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := s.RedeemDevicePairing(ctx, p.Code, p.PollHash, sess, now); err == nil {
		t.Fatal("duplicate token insertion must fail")
	}
	if _, err := s.GetDevicePairing(ctx, p.Code, p.PollHash, now); err != nil {
		t.Fatalf("failed issuance consumed pairing: %v", err)
	}
	sess.Token = domain.HashToken("replacement-token")
	if err := s.RedeemDevicePairing(ctx, p.Code, p.PollHash, sess, now); err != nil {
		t.Fatal(err)
	}
}
