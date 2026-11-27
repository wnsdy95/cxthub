package serverruntime

import (
	"fmt"
	"net/url"
)

func ValidateGitHubOrigin(origin string) error {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" || parsed.Host == "" {
		return fmt.Errorf("GitHub return origin must be an absolute origin")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && (parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "::1")) {
		return fmt.Errorf("GitHub return origin must use HTTPS except on loopback")
	}
	return nil
}
