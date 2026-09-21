package app

import (
	"context"
	"fmt"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// Notifications contain event metadata only, never secrets ciphertext or plaintext.
func enqueueRepositoryNotification(ctx context.Context, store any, repositoryRecord domain.Repository, kind, text string) error {
	if repositoryRecord.WebhookURL == "" || repositoryRecord.Archived {
		return nil
	}
	outbox, ok := store.(outbound.NotificationStore)
	if !ok {
		return fmt.Errorf("durable notification storage unavailable")
	}
	now := time.Now().UTC()
	return outbox.EnqueueNotification(ctx, outbound.NotificationDelivery{Destination: repositoryRecord.WebhookURL, Job: domain.NotificationJob{
		ID: domain.NewID("evt_"), RepositoryID: repositoryRecord.ID, Kind: kind, Text: text, State: "pending", CreatedAt: now, UpdatedAt: now, NextAttempt: now,
	}})
}

func (s *Service) notifyRefUpdate(ctx context.Context, repoID domain.ContentHash, ref domain.Ref, forced, created bool) error {
	if s.repositories == nil || ref.Kind != domain.RefBranch {
		return nil
	}
	repo, err := s.meta.GetRepo(ctx, repoID)
	if err != nil {
		return err
	}
	if repo.RepositoryID == "" {
		return nil
	}
	repositoryRecord, err := s.repositories.GetRepository(ctx, repo.RepositoryID)
	if err != nil {
		return err
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
	return enqueueRepositoryNotification(ctx, s.meta, repositoryRecord, "ref_updated", fmt.Sprintf("cxthub: %s — branch %q %s → %s", name, ref.Name, verb, shortHash(ref.Target)))
}

func (s *Service) notifySecretsChanged(ctx context.Context, repoID domain.ContentHash) error {
	if s.repositories == nil {
		return nil
	}
	repo, err := s.meta.GetRepo(ctx, repoID)
	if err != nil {
		return err
	}
	if repo.RepositoryID == "" {
		return nil
	}
	repositoryRecord, err := s.repositories.GetRepository(ctx, repo.RepositoryID)
	if err != nil {
		return err
	}
	name := repo.RemoteURL
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return enqueueRepositoryNotification(ctx, s.meta, repositoryRecord, "secrets_updated", fmt.Sprintf("cxthub: %s — secrets updated (team members run cxt secrets pull)", name))
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
		// Do not forward event content to a different URL or accept a redirect's
		// GET response as an acknowledgment of the original POST.
		CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
	}
}
