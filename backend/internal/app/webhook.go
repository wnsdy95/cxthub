package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// Alert webhook — Slack incoming webhook compatibility (POST {"text": "..."}).
//
// On successful ref update (push/branch creation/force), asynchronously notifies the repo's workspace webhook_url. Alert is best-effort: failure has no impact on synchronization (goroutine + 5s timeout, errors ignored).

// notifyRefUpdate notifies the workspace webhook of ref movement success (only if configured).
func (s *Service) notifyRefUpdate(ctx context.Context, repoID domain.ContentHash, ref domain.Ref, forced, created bool) {
	if s.ws == nil || ref.Kind != domain.RefBranch {
		return
	}
	repo, err := s.meta.GetRepo(ctx, repoID)
	if err != nil || repo.WorkspaceID == "" {
		return
	}
	wsp, err := s.ws.GetWorkspace(ctx, repo.WorkspaceID)
	if err != nil || wsp.WebhookURL == "" {
		return
	}
	name := repo.RemoteURL
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	verb := "update"
	if created {
		verb = "new branch"
	}
	if forced {
		verb += "(force)"
	}
	text := fmt.Sprintf("cxthub: %s — branch %q %s → %s", name, ref.Name, verb, shortHash(ref.Target))
	go postWebhook(wsp.WebhookURL, text)
}

// notifyWorkspace sends text to the workspace webhook (only if configured, best-effort).
// Entry point for common events (member join, secret change) outside ref update.
func notifyWorkspace(wsp domain.Workspace, text string) {
	if wsp.WebhookURL == "" {
		return
	}
	go postWebhook(wsp.WebhookURL, text)
}

// notifySecretsChanged notifies the workspace webhook of repo secret envelope replacement.
// Content is an E2E encrypted message, only "changed" is communicated (team member pull encouraged + audit signal).
func (s *Service) notifySecretsChanged(ctx context.Context, repoID domain.ContentHash) {
	if s.ws == nil {
		return
	}
	repo, err := s.meta.GetRepo(ctx, repoID)
	if err != nil || repo.WorkspaceID == "" {
		return
	}
	wsp, err := s.ws.GetWorkspace(ctx, repo.WorkspaceID)
	if err != nil {
		return
	}
	name := repo.RemoteURL
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	notifyWorkspace(wsp, fmt.Sprintf("cxthub: %s — secrets updated (team members run cxt secrets pull)", name))
}

func shortHash(h domain.ContentHash) string {
	s := strings.TrimPrefix(string(h), "sha256:")
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

// webhookSchemeOK is a scheme pre-check (http/s only). Actual SSRF defense is done by the dialer below.
func webhookSchemeOK(raw string) bool {
	u, err := neturl.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != ""
}

// blockedIP determines if an IP is an internal network IP (loopback/private/link-local/unknown — including cloud metadata 169.254, IPv6 ULA·CGNAT). It is a single determination function for SSRF target IP classification.
//
// Note: This determination is enforced only by the dialer of safeWebhookClient (webhook outbound only).
// Other outbound (ghRepoPublic·Firebase certs) do not have user control over the destination, so they are currently not applicable — adding a new outbound path that accepts user-controlled URLs must go through safeWebhookClient (or this determination) via the dialer.
func blockedIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	// CGNAT 100.64.0.0/10 (treated as private).
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return true
	}
	return false
}

// webhookClientOnce ensures that safeWebhookClient is created only once per process (connection reuse).
// Creating a new Client+Transport on each call prevents keep-alive connections from being reused, leading to repeated TCP+TLS handshakes for the same webhook URL. The SSRF check by the dialer runs on each connection, so it maintains the same security characteristics even as a singleton.
var (
	webhookClientOnce sync.Once
	webhookClient     *http.Client
)

// safeWebhookClient is a shared HTTP client that enforces SSRF in the dialer (singleton).
//
// Core: Before connecting, it resolves the destination host to **the actual IP to connect to** and directly connects (pinning) only to the passed IP. This check happens on every connection, so it blocks two SSRFs:
//   - DNS rebinding: Uses the same IP for the check and connection (no re-resolution).
//   - Redirect: All redirects pass through the same dialer, so internal addresses are blocked by a 302.
//
// CXT_ALLOW_PRIVATE_WEBHOOK=1 skips the internal network check (exception for self-hosted).
func safeWebhookClient() *http.Client {
	webhookClientOnce.Do(func() { webhookClient = buildWebhookClient() })
	return webhookClient
}

func buildWebhookClient() *http.Client {
	allowPrivate := os.Getenv("CXT_ALLOW_PRIVATE_WEBHOOK") == "1"
	base := &net.Dialer{Timeout: 5 * time.Second}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("webhook: host resolution failed")
		}
		// Tries in order with validated IPs (prevents rebinding — fixed by this list without re-resolution).
		// Trying only ips[0] for a dual-stack host can lead to connection failures in an IPv4-only environment (container/Cloud Run) when the first record is AAAA — restores happy-eyeballs (tries all validated IPs) while maintaining SSRF pinning.
		var lastErr error
		for _, ip := range ips {
			if !allowPrivate && blockedIP(ip) {
				return nil, fmt.Errorf("webhook: internal network address rejected (%s)", ip)
			}
		}
		for _, ip := range ips {
			conn, derr := base.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if derr == nil {
				return conn, nil
			}
			lastErr = derr
		}
		return nil, lastErr
	}
	return &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{DialContext: dial},
		// Redirects are re-validated by the dialer, but excessive redirects are capped separately.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("webhook: too many redirects")
			}
			if !webhookSchemeOK(req.URL.String()) {
				return fmt.Errorf("webhook: disallowed redirect scheme")
			}
			return nil
		},
	}
}

// postWebhook sends {"text": ...} via POST. Failures are silently ignored (best-effort).
// SSRF defense is enforced by the safeWebhookClient's dialer (schemes are pre-blocked here).
func postWebhook(url, text string) {
	if !webhookSchemeOK(url) {
		return
	}
	body, _ := json.Marshal(map[string]string{"text": text})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if resp, err := safeWebhookClient().Do(req); err == nil {
		resp.Body.Close()
	}
}

// _ = inbound reference to maintain file compilation clarity
var _ = inbound.RefForced
