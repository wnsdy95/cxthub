package federation

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

func TestProviderNetworkRejectsPrivateAndReboundAddresses(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.1.1.1", "100.64.1.1", "169.254.169.254", "172.16.0.1", "192.168.1.1", "198.18.0.1", "224.0.0.1", "240.0.0.1", "0.0.0.0", "::1", "::ffff:127.0.0.1", "fc00::1", "fe80::1", "64:ff9b::a00:1", "2002:7f00:1::1"} {
		ip := netip.MustParseAddr(raw)
		if publicIP(ip) {
			t.Fatal("private address accepted", raw)
		}
		calls := 0
		dial := publicDial(func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8"), ip}, nil
		}, func(context.Context, string, string) (net.Conn, error) {
			calls++
			return nil, errors.New("unexpected dial")
		})
		if _, err := dial(context.Background(), "tcp", "issuer.test:443"); err == nil || calls != 0 {
			t.Fatal("mixed DNS attempted connection", raw)
		}
	}
	resolved := 0
	dial := publicDial(func(context.Context, string, string) ([]netip.Addr, error) {
		resolved++
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}, func(_ context.Context, _, addr string) (net.Conn, error) {
		if addr != "8.8.8.8:443" {
			t.Fatal("unvalidated re-resolution", addr)
		}
		return nil, errors.New("fixture refused")
	})
	_, _ = dial(context.Background(), "tcp", "issuer.test:443")
	if resolved != 1 {
		t.Fatal(resolved)
	}
}

func TestProviderTransportBoundsAndRedirects(t *testing.T) {
	for _, target := range []string{"http://example.test", "https://user:pass@example.test", "https://example.test:8443", "https://example.test/#fragment"} {
		called := false
		tr := boundedTransport{roundTripFunc(func(*http.Request) (*http.Response, error) { called = true; return nil, errProvider })}
		r, _ := http.NewRequest("GET", target, nil)
		if _, err := tr.RoundTrip(r); err == nil || called {
			t.Fatal("unsafe target sent", target)
		}
	}
	tr := boundedTransport{roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", maxProviderResponse+1)))}, nil
	})}
	r, _ := http.NewRequest("GET", "https://example.test", nil)
	if _, err := tr.RoundTrip(r); err == nil {
		t.Fatal("oversized response accepted")
	}
	client := newPublicClient()
	if client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("redirects can forward client credentials")
	}
}
