package http

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// localAllowanceEntries bounds the per-process allowance table. An entry is one
// hashed key and two timestamps, so the bound costs a few MiB at most.
const localAllowanceEntries = 1 << 16

// errSharedAllowanceUnavailable reports a server built without a shared runtime
// store. Requests then fail closed: no instance may admit on its own.
var errSharedAllowanceUnavailable = errors.New("shared request allowance unavailable")

// localAllowance is the per-process copy of the shared GCRA allowance. It runs
// before the shared check, so a request this process already refuses costs no
// database write. When the shared store is overloaded or times out, its decision
// stands alone, which still limits each instance to the configured rate.
type localAllowance struct {
	mu      sync.Mutex
	entries map[string]localAllowanceEntry
}

type localAllowanceEntry struct {
	availableAt time.Time
	expiresAt   time.Time
}

// allow applies the rule of PostgresStore.AllowRequest: a burst of up to limit
// requests that refills at limit per window. A refused request does not move the
// schedule. A full table refuses new keys rather than growing without bound.
func (allowance *localAllowance) allow(key string, limit int, window time.Duration, now time.Time) bool {
	if limit < 1 || window <= 0 {
		return false
	}
	step := window / time.Duration(limit)
	allowance.mu.Lock()
	defer allowance.mu.Unlock()
	entry, ok := allowance.entries[key]
	if !ok || !now.Before(entry.expiresAt) {
		if !ok && !allowance.reserve(now) {
			return false
		}
		entry = localAllowanceEntry{availableAt: now}
	}
	if entry.availableAt.After(now.Add(window - step)) {
		return false
	}
	if entry.availableAt.Before(now) {
		entry.availableAt = now
	}
	entry.availableAt = entry.availableAt.Add(step)
	entry.expiresAt = now.Add(window)
	allowance.entries[key] = entry
	return true
}

// reserve makes room for one new key, dropping expired keys only when the table
// is full. The caller holds the lock.
func (allowance *localAllowance) reserve(now time.Time) bool {
	if allowance.entries == nil {
		allowance.entries = make(map[string]localAllowanceEntry)
	}
	if len(allowance.entries) < localAllowanceEntries {
		return true
	}
	for key, entry := range allowance.entries {
		if !now.Before(entry.expiresAt) {
			delete(allowance.entries, key)
		}
	}
	return len(allowance.entries) < localAllowanceEntries
}

// admitRequest reports whether one more request under key fits its allowance.
// The shared store keeps instances from multiplying the allowance. If it fails,
// for example when the database is overloaded, the local decision is used instead
// of refusing every caller. Invalid limits and a missing store still fail closed.
func (s *Server) admitRequest(ctx context.Context, key string, limit int, window time.Duration) (bool, error) {
	if s.runtime == nil {
		return false, errSharedAllowanceUnavailable
	}
	if limit < 1 || window <= 0 || window/time.Duration(limit) < time.Microsecond {
		return false, domain.ErrValidation
	}
	if !s.allowance.allow(key, limit, window, time.Now()) {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	shared, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	allowed, err := s.runtime.AllowRequest(shared, key, limit, window, time.Time{})
	switch {
	case err == nil:
		return allowed, nil
	case errors.Is(err, domain.ErrValidation), ctx.Err() != nil:
		return false, err
	default:
		return true, nil
	}
}

// writeAllowance admits the request or writes the 429 or 503 response itself.
func (s *Server) writeAllowance(w http.ResponseWriter, ctx context.Context, key string, limit int, window time.Duration) bool {
	allowed, err := s.admitRequest(ctx, key, limit, window)
	if errors.Is(err, errSharedAllowanceUnavailable) {
		s.writeError(w, http.StatusServiceUnavailable, "unavailable", err.Error())
		return false
	}
	if err != nil {
		s.writeError(w, http.StatusServiceUnavailable, "unavailable", "request allowance unavailable")
		return false
	}
	if !allowed {
		writeRateLimited(s, w, window)
		return false
	}
	return true
}

func writeRateLimited(s *Server, w http.ResponseWriter, window time.Duration) {
	w.Header().Set("Retry-After", fmt.Sprint(max(1, int(window.Seconds()))))
	s.writeError(w, http.StatusTooManyRequests, "rate_limited", "Too many requests — please retry later")
}

// allowanceSubject names whose allowance a request spends: the signed-in user
// when there is one, otherwise the connecting address. Behind the production
// proxies every anonymous caller shares one address, so anonymous routes keep a
// single service-wide allowance; forwarding headers are never trusted here.
func allowanceSubject(r *http.Request) string {
	if u, ok := userFrom(r.Context()); ok && u.ID != "" {
		return "user:" + domain.HashToken(u.ID)
	}
	address := r.RemoteAddr
	if host, _, err := net.SplitHostPort(address); err == nil {
		address = host
	}
	return "source:" + domain.HashToken(address)
}
