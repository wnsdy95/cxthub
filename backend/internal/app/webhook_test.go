package app

import (
	"context"
	"net"
	"net/http"
	"testing"
)

// TestBlockedIP fixes SSRF block IP classification to prevent regression from review findings.
func TestBlockedIP(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":       true,  // loopback
		"::1":             true,  // loopback v6
		"10.0.0.5":        true,  // Private
		"192.168.1.1":     true,  // Private
		"172.16.0.1":      true,  // Private
		"169.254.169.254": true,  // Link-local (cloud metadata)
		"fc00::1":         true,  // IPv6 ULA
		"fe80::1":         true,  // IPv6 link-local
		"0.0.0.0":         true,  // Unspecified
		"100.64.0.1":      true,  // CGNAT
		"224.0.0.1":       true,  // multicast
		"255.255.255.255": true,  // limited broadcast
		"ff02::1":         true,  // IPv6 multicast
		"8.8.8.8":         false, // Public
		"1.1.1.1":         false, // public
	}
	for s, want := range cases {
		if got := blockedIP(net.ParseIP(s)); got != want {
			t.Errorf("blockedIP(%s)=%v want %v", s, got, want)
		}
	}
}

// TestSafeWebhookClientBlocksLoopback checks if the dialer rejects loopback connections.
// This dialer applies to both the initial request and redirects, so internal 302 redirects
// are also blocked using the same mechanism (H1). It only connects using validated IPs, preventing rebinding (H2).
func TestSafeWebhookClientBlocksLoopback(t *testing.T) {
	c := safeWebhookClient()
	req, _ := http.NewRequestWithContext(context.Background(), "POST", "http://127.0.0.1:9/hook", nil)
	if _, err := c.Do(req); err == nil {
		t.Fatal("loopback webhook connection not blocked (SSRF vulnerability)")
	}
}
