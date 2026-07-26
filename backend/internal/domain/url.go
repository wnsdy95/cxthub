package domain

import (
	"net/url"
	"strings"
	"unicode"
)

// SanitizeRemoteURL removes credentials and non-identity URL components before
// a remote is persisted or exposed. Local paths and malformed URL-like strings
// return empty so server responses cannot disclose workstation paths or tokens.
func SanitizeRemoteURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		if strings.Contains(raw, "://") {
			return ""
		}
		return sanitizeSCPRemote(raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "ssh", "git", "git+ssh":
	default:
		return ""
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	return u.String()
}

func sanitizeSCPRemote(raw string) string {
	at := strings.LastIndex(raw, "@")
	if at < 0 {
		return ""
	}
	rest := raw[at+1:]
	colon := strings.IndexByte(rest, ':')
	if colon <= 0 || colon == len(rest)-1 {
		return ""
	}
	host, path := rest[:colon], rest[colon+1:]
	if strings.ContainsAny(host, "/\\") {
		return ""
	}
	for _, r := range host + path {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return ""
		}
	}
	return "git@" + host + ":" + path
}
