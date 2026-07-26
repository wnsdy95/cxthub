package http

import (
	"crypto/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// device flow — CLI token issuance automation (RFC 8628 pattern, gh auth login response).
//
// The authentication method remains bearer. This file handles "issuance (one-time conscious)" only:
//
//	cxt login → POST start (code+poll_token receipt, browser open)
//	          → user approves on web (login required)
//	          → CLI polling (poll) → token issuance and one-time delivery upon approval confirmation
//
// Security: Separates approval authority (short code) and receipt authority (long poll_token) — even if the code is guessed, a token cannot be received without the poll_token. The code is one-time use + 5-minute TTL. Pairing status is in-memory (temporary state — CLI will receive expiration notice upon server restart and can retry).

const (
	devicePairTTL      = 5 * time.Minute
	devicePollInterval = 3 // seconds — CLI polling interval guidance
)

// pairing is the ongoing device flow.
type pairing struct {
	pollHash  string // hash of the receipt secret (original is not stored in memory)
	userID    string // approved user ("" = pending)
	label     string // device display name sent by CLI (hostname — appended to issued token)
	expiresAt time.Time
}

// devicePairings is the server's pairing status (code → pairing).
type devicePairings struct {
	mu sync.Mutex
	m  map[string]*pairing
}

// newDeviceCode generates a short code for humans: excludes confusing characters (0/O/1/I, vowels) 6 characters, XXX-XXX.
func newDeviceCode() string {
	const alphabet = "BCDFGHJKMNPQRSTVWXYZ23456789"
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	out := make([]byte, 0, 7)
	for i, c := range b {
		if i == 3 {
			out = append(out, '-')
		}
		out = append(out, alphabet[int(c)%len(alphabet)])
	}
	return string(out)
}

// deviceStart initiates pairing (unauthenticated — no secret value returned: code is worthless, poll_token is unique to this CLI). Rate limiting is handled during route registration.
func (s *Server) deviceStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Label string `json:"label"` // optional — CLI hostname (device name, display-only)
	}
	_ = decodeLoose(r, &body) // for backward compatibility with old CLI (no body)
	poll := domain.NewID("dpoll_")
	code := newDeviceCode()
	if code == "" {
		s.writeError(w, http.StatusInternalServerError, "internal", "code generation failed")
		return
	}
	now := time.Now()
	s.device.mu.Lock()
	if s.device.m == nil {
		s.device.m = map[string]*pairing{}
	}
	// lazy cleanup (expired codes) — pairing is minimal and specific, so this is sufficient.
	for k, p := range s.device.m {
		if now.After(p.expiresAt) {
			delete(s.device.m, k)
		}
	}
	s.device.m[code] = &pairing{pollHash: domain.HashToken(poll), label: strings.TrimSpace(body.Label), expiresAt: now.Add(devicePairTTL)}
	s.device.mu.Unlock()

	s.respond(w, map[string]any{
		"code":       code,
		"poll_token": poll,
		"expires_in": int(devicePairTTL.Seconds()),
		"interval":   devicePollInterval,
	}, nil)
}

// deviceApprove is when a logged-in user approves a code.
func (s *Server) deviceApprove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	code := strings.ToUpper(strings.TrimSpace(body.Code))
	u, _ := userFrom(r.Context())
	s.device.mu.Lock()
	p := s.device.m[code]
	valid := p != nil && time.Now().Before(p.expiresAt)
	if valid {
		p.userID = u.ID
	}
	s.device.mu.Unlock()
	if !valid {
		s.writeError(w, http.StatusNotFound, "not_found", "code expired or does not exist — run cxt login again in your terminal")
		return
	}
	s.respond(w, map[string]string{"status": "approved"}, nil)
}

// devicePoll checks if the CLI has been approved and, if so, receives a token once.
func (s *Server) devicePoll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code      string `json:"code"`
		PollToken string `json:"poll_token"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	code := strings.ToUpper(strings.TrimSpace(body.Code))
	s.device.mu.Lock()
	p := s.device.m[code]
	// Consolidates existence, expiration, and reception permissions into a single 404 (information non-disclosure).
	if p == nil || time.Now().After(p.expiresAt) || domain.HashToken(body.PollToken) != p.pollHash {
		s.device.mu.Unlock()
		s.writeError(w, http.StatusNotFound, "not_found", "Expired or invalid pairing")
		return
	}
	if p.userID == "" {
		s.device.mu.Unlock()
		s.respond(w, map[string]string{"status": "pending"}, nil)
		return
	}
	userID, label := p.userID, p.label
	delete(s.device.m, code) // One-time — expires immediately upon reception
	s.device.mu.Unlock()

	sess, err := s.id.CreateCLIToken(r.Context(), userID, label)
	if err != nil {
		code, status := mapError(err)
		s.writeError(w, status, code, err.Error())
		return
	}
	s.respond(w, map[string]any{"status": "approved", "token": sess.Token, "expires_at": sess.ExpiresAt}, nil)
}
