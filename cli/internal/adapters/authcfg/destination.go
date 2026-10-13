package authcfg

import (
	"net"
	"net/url"
	"strings"
)

func TokenForDestination(target, environmentBase, environmentToken string) string {
	destination, err := url.Parse(target)
	if err != nil || destination.Opaque != "" || destination.Hostname() == "" || destination.User != nil || destination.RawQuery != "" || destination.ForceQuery || strings.Contains(target, "#") {
		return ""
	}
	address := net.ParseIP(destination.Hostname())
	loopback := strings.EqualFold(destination.Hostname(), "localhost") || address != nil && address.IsLoopback()
	if !strings.EqualFold(destination.Scheme, "https") && !(strings.EqualFold(destination.Scheme, "http") && loopback) {
		return ""
	}
	origin, err := url.Parse(environmentBase)
	if err == nil && strings.EqualFold(destination.Scheme, origin.Scheme) && strings.EqualFold(destination.Host, origin.Host) && strings.TrimRight(destination.Path, "/") == strings.TrimRight(origin.Path, "/") && environmentToken != "" {
		return environmentToken
	}
	return Token(strings.ToLower(destination.Host))
}
