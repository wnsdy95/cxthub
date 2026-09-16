package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"os"
	"path/filepath"
	"time"
)

func (s *FSStore) pairingPath(code string) string {
	return filepath.Join(s.dataDir, "runtime", "pairings", opaqueName(code)+".json")
}
func (s *FSStore) CreateDevicePairing(ctx context.Context, p domain.DevicePairing) error {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	var old domain.DevicePairing
	err := readJSON(s.pairingPath(p.Code), &old)
	if err == nil && old.ExpiresAt.After(time.Now()) {
		return domain.ErrConflict
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return writeAtomic(s.pairingPath(p.Code), b)
}
func (s *FSStore) GetDevicePairing(ctx context.Context, code, poll string, now time.Time) (domain.DevicePairing, error) {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	var p domain.DevicePairing
	err := readJSON(s.pairingPath(code), &p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, domain.ErrNotFound) {
			err = domain.ErrNotFound
		}
		return p, err
	}
	if p.Code != code || p.PollHash != poll || !p.ExpiresAt.After(now) {
		return domain.DevicePairing{}, domain.ErrNotFound
	}
	return p, nil
}
func (s *FSStore) ApproveDevicePairing(ctx context.Context, code, user string, now time.Time) error {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	var p domain.DevicePairing
	if err := readJSON(s.pairingPath(code), &p); err != nil {
		return domain.ErrNotFound
	}
	if p.Code != code || !p.ExpiresAt.After(now) {
		return domain.ErrNotFound
	}
	if p.UserID != "" && p.UserID != user {
		return domain.ErrConflict
	}
	p.UserID = user
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return writeAtomic(s.pairingPath(code), b)
}
func (s *FSStore) RedeemDevicePairing(ctx context.Context, code, poll string, sess domain.Session, now time.Time) error {
	if err := domain.ValidateSessionRecord(sess); err != nil {
		return err
	}
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	var p domain.DevicePairing
	if err := readJSON(s.pairingPath(code), &p); err != nil {
		return domain.ErrNotFound
	}
	if p.Code != code || p.PollHash != poll || p.UserID == "" || p.UserID != sess.UserID || !p.ExpiresAt.After(now) {
		return domain.ErrNotFound
	}
	// Session precedes one-time consumption; no raw token is stored or returned
	// until consumption is durable. A crash may leave an undisclosed hashed session.
	if err := s.CreateSession(ctx, sess); err != nil {
		return err
	}
	return removeFileDurable(s.pairingPath(code))
}

type allowance struct {
	AvailableAt time.Time `json:"available_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func (s *FSStore) AllowRequest(ctx context.Context, key string, limit int, window time.Duration, now time.Time) (bool, error) {
	if limit < 1 || window <= 0 || window/time.Duration(limit) < time.Microsecond {
		return false, domain.ErrValidation
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	path := filepath.Join(s.dataDir, "runtime", "limits", opaqueName(key)+".json")
	var a allowance
	if err := readJSON(path, &a); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, domain.ErrNotFound) {
		return false, err
	}
	interval := window / time.Duration(limit)
	if a.AvailableAt.After(now.Add(window - interval)) {
		return false, nil
	}
	if a.AvailableAt.Before(now) {
		a.AvailableAt = now
	}
	a.AvailableAt = a.AvailableAt.Add(interval)
	a.ExpiresAt = now.Add(window)
	b, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	return true, writeAtomic(path, b)
}
func (s *FSStore) PruneRuntimeState(ctx context.Context, now time.Time) error {
	l := s.oauthLock()
	l.Lock()
	defer l.Unlock()
	for _, dir := range []string{"pairings", "limits"} {
		root := filepath.Join(s.dataDir, "runtime", dir)
		entries, err := os.ReadDir(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
				continue
			}
			var expiry struct {
				ExpiresAt time.Time `json:"expires_at"`
			}
			path := filepath.Join(root, e.Name())
			if err := readJSON(path, &expiry); err != nil {
				return err
			}
			if !expiry.ExpiresAt.After(now) {
				if err := os.Remove(path); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
