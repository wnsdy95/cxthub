package gitctx

import (
	"net/url"
	"strings"
	"unicode"
)

// SanitizeRemoteURL strips credentials and non-identity URL components before a
// remote is persisted, displayed, or used as an identity input.
func SanitizeRemoteURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		if strings.Contains(rawURL, "://") {
			return ""
		}
		return sanitizeSCPRemote(rawURL)
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

// NormalizeRemoteURL converts a git remote URL to a provider-independent normalized form (DATA-MODEL §3.1).
//
// Normalization rules:
//  1. SSH URL(git@github.com:org/repo.git) → https://github.com/org/repo
//  2. HTTPS URL → scheme removal, lowercase host + path normalization
//  3. Remove trailing .git suffix
//  4. Remove trailing /
//  5. Lowercase host
//
// Examples:
// "git@github.com:wnsdy95/cxthub/cli.git" → "github.com/wnsdy95/cxthub/cli"
//
// "https://GITHUB.COM/wnsdy95/cxthub/cli.git" → "github.com/wnsdy95/cxthub/cli"
//
// TODO: Complete URL parsing implementation (SCP format ssh, various git hosts).
func NormalizeRemoteURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	normalized := SanitizeRemoteURL(rawURL)
	if normalized == "" {
		// Local-path remotes remain valid local identity inputs but are never
		// persisted/displayed. Malformed URL/userinfo inputs fail closed.
		if strings.Contains(rawURL, "://") || strings.Contains(rawURL, "@") {
			return ""
		}
		normalized = rawURL
	}
	// SSH SCP format: git@github.com:org/repo.git
	if strings.HasPrefix(normalized, "git@") {
		normalized = strings.TrimPrefix(normalized, "git@")
		normalized = strings.Replace(normalized, ":", "/", 1)
	} else if parsed, err := url.Parse(normalized); err == nil && parsed.Host != "" {
		normalized = parsed.Host + parsed.EscapedPath()
	}
	// Remove trailing .git
	normalized = strings.TrimSuffix(normalized, ".git")
	// Remove trailing /
	normalized = strings.TrimRight(normalized, "/")
	// Lowercase
	return strings.ToLower(normalized)
}
