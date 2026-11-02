package domain

import (
	"net/url"
	"strings"
)

func NormalizeGitRemoteURL(raw string) string {
	remote := SanitizeRemoteURL(raw)
	if remote == "" {
		return ""
	}
	if strings.HasPrefix(remote, "git@") {
		remote = strings.Replace(strings.TrimPrefix(remote, "git@"), ":", "/", 1)
	} else if parsed, err := url.Parse(remote); err == nil && parsed.Host != "" {
		remote = parsed.Host + parsed.EscapedPath()
	}
	remote = strings.TrimSuffix(remote, ".git")
	remote = strings.TrimRight(remote, "/")
	return strings.ToLower(remote)
}
