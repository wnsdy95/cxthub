// Package federation contains identity-provider protocol and network adapters.
package federation

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const maxProviderResponse = 1 << 20

var errProvider = errors.New("identity provider request failed")

func providerURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return publicIP(ip)
	}
	return true
}

var restrictedNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001::/32"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
}

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range restrictedNetworks {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

type resolveIPs func(context.Context, string, string) ([]netip.Addr, error)
type connectIP func(context.Context, string, string) (net.Conn, error)

// Resolve once and connect to the checked address, never to the original host.
// Every answer must be public; mixed public/private DNS cannot select a bypass.
func publicDial(resolve resolveIPs, connect connectIP) connectIP {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != "443" {
			return nil, errProvider
		}
		ips, err := resolve(ctx, "ip", host)
		if err != nil || len(ips) == 0 {
			return nil, errProvider
		}
		for _, ip := range ips {
			if !publicIP(ip) {
				return nil, errProvider
			}
		}
		for _, ip := range ips {
			if c, err := connect(ctx, network, net.JoinHostPort(ip.String(), port)); err == nil {
				return c, nil
			}
		}
		return nil, errProvider
	}
}

type boundedTransport struct{ base http.RoundTripper }

func (t boundedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !providerURL(req.URL.String()) {
		return nil, errProvider
	}
	res, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, errProvider
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, maxProviderResponse+1))
	if err != nil || len(b) > maxProviderResponse {
		return nil, errProvider
	}
	res.Body = io.NopCloser(bytes.NewReader(b))
	return res, nil
}

func newPublicClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: boundedTransport{&http.Transport{
			// No environment proxy or private-network exception for tenant URLs.
			DialContext:         publicDial(net.DefaultResolver.LookupNetIP, dialer.DialContext),
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second,
			IdleConnTimeout: time.Minute, MaxIdleConns: 20, MaxIdleConnsPerHost: 2,
		}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
