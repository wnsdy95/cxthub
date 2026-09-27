package main

import (
	"github.com/wnsdy95/cxthub/backend/internal/adapters/resend"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/serverruntime"
	"os"
	"strings"
)

func loadServerEnv() error { return serverruntime.LoadEnv() }

func configureInvitationEmail(s *app.IdentityService, addr, publicURL string) error {
	key := strings.TrimSpace(os.Getenv("RESEND_API_KEY"))
	if key == "" {
		return nil
	}
	from := strings.TrimSpace(os.Getenv("RESEND_FROM"))
	if from == "" {
		from = "CXTHub <noreply@cxthub.com>"
	}
	origin := strings.TrimSpace(os.Getenv("CXT_WEB_URL"))
	if origin == "" {
		origin = publicURL
		if isLoopback(addr) {
			origin = "http://localhost:5173"
		}
	}
	return s.ConfigureInvitationEmail(resend.New(key), from, origin)
}
