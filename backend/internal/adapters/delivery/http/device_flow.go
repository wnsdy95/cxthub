package http

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"strings"
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
// Security: Separates approval authority (short code) and receipt authority (long poll_token) — even if the code is guessed, a token cannot be received without the poll_token. The code is one-time use + 5-minute TTL. Pairing state is shared and expires in the database; any server instance may handle each step.

const (
	devicePairTTL      = 5 * time.Minute
	devicePollInterval = 3 // seconds — CLI polling interval guidance
)

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
	if s.runtime == nil {
		s.writeError(w, 503, "unavailable", "shared device state unavailable")
		return
	}
	label := []rune(strings.TrimSpace(body.Label))
	if len(label) > 64 {
		label = label[:64]
	}
	var code string
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		code = newDeviceCode()
		if code == "" {
			s.writeError(w, 500, "internal", "code generation failed")
			return
		}
		err = s.runtime.CreateDevicePairing(r.Context(), domain.DevicePairing{Code: code, PollHash: domain.HashToken(poll), Label: string(label), ExpiresAt: time.Now().UTC().Add(devicePairTTL)})
		if !errors.Is(err, domain.ErrConflict) {
			break
		}
	}
	if err != nil {
		s.writeError(w, 503, "unavailable", "device state unavailable")
		return
	}

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
	if s.runtime == nil {
		s.writeError(w, 503, "unavailable", "shared device state unavailable")
		return
	}
	if err := s.runtime.ApproveDevicePairing(r.Context(), code, u.ID, time.Now().UTC()); err != nil {
		s.respond(w, nil, err)
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

	service, ok := s.id.(interface {
		RedeemDevicePairing(context.Context, string, string) (domain.Session, bool, error)
	})
	if !ok {
		s.writeError(w, 503, "unavailable", "shared device state unavailable")
		return
	}
	sess, approved, err := service.RedeemDevicePairing(r.Context(), code, domain.HashToken(body.PollToken))
	if err != nil {
		s.respond(w, nil, err)
		return
	}
	if !approved {
		s.respond(w, map[string]string{"status": "pending"}, nil)
		return
	}
	s.respond(w, map[string]any{"status": "approved", "token": sess.Token, "expires_at": sess.ExpiresAt}, nil)
}
