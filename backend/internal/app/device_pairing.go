package app

import (
	"context"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"log"
	"strings"
	"time"
)

func (s *IdentityService) RuntimeStore() outbound.RuntimeStore {
	st, _ := s.ws.(outbound.RuntimeStore)
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
	if _, err := s.ws.GetUser(ctx, p.UserID); err != nil {
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
func (s *IdentityService) RunRuntimeMaintenance(ctx context.Context) {
	st := s.RuntimeStore()
	if st == nil {
		return
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := st.PruneRuntimeState(prune, time.Now().UTC())
			cancel()
			if err != nil && ctx.Err() == nil {
				log.Printf("runtime state cleanup: %v", err)
			}
		}
	}
}
