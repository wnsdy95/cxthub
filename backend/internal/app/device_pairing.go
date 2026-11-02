package app

import (
	"context"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/app/systemmaintenance"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"log"
	"strings"
	"time"
)

func (s *IdentityService) RuntimeStore() outbound.RuntimeStore {
	st, _ := s.repositories.(outbound.RuntimeStore)
	return st
}
func (s *IdentityService) RedeemDevicePairing(ctx context.Context, code, pollHash string) (domain.Session, bool, error) {
	st := s.RuntimeStore()
	if st == nil {
		return domain.Session{}, false, fmt.Errorf("shared device state unavailable")
	}
	now := time.Now().UTC()
	p, err := st.GetDevicePairing(ctx, code, pollHash, now)
	if err != nil {
		return domain.Session{}, false, err
	}
	if p.UserID == "" {
		return domain.Session{}, false, nil
	}
	if _, err := s.repositories.GetUser(ctx, p.UserID); err != nil {
		return domain.Session{}, false, domain.ErrUnauthorized
	}
	label := []rune(strings.TrimSpace(p.Label))
	if len(label) > 64 {
		label = label[:64]
	}
	raw := domain.NewID("sess_cli_")
	sess := domain.Session{Token: domain.HashToken(raw), UserID: p.UserID, Hint: domain.TokenHint(raw), Kind: "cli", Label: string(label), CreatedAt: now, ExpiresAt: now.Add(cliTokenTTL)}
	if err := st.RedeemDevicePairing(ctx, code, pollHash, sess, now); err != nil {
		return domain.Session{}, false, err
	}
	sess.Token = raw
	return sess, true, nil
}
func (service *IdentityService) RunRuntimeMaintenance(ctx context.Context) {
	worker, err := systemmaintenance.NewRuntime(systemmaintenance.RuntimeConfig{
		Profile: systemmaintenance.Compatibility, Store: service.RuntimeStore(),
	})
	if err != nil {
		log.Printf("runtime maintenance: %v", err)
		return
	}
	worker.Run(ctx)
}
