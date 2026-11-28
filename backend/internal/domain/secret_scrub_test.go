package domain

import (
	"strings"
	"testing"
)

func TestScrubSecretPatternsMasksKnownFormatsAndLeavesTheRest(t *testing.T) {
	// Every sample is synthetic and assembled here, so no token-shaped
	// literal sits in the source.
	github := "ghp_" + strings.Repeat("a1B2", 9)
	jwt := strings.Join([]string{"eyJ" + strings.Repeat("a", 12), "eyJ" + strings.Repeat("b", 12), strings.Repeat("c", 16)}, ".")
	for name, sample := range map[string]struct{ text, secret, kind string }{
		"a cloud access key":   {"key " + "AKIA" + strings.Repeat("Q", 16), "AKIA" + strings.Repeat("Q", 16), "aws-key"},
		"a hosting token":      {"token: " + github, github, "github-token"},
		"a fine-grained token": {"github_pat_" + strings.Repeat("x9", 14), strings.Repeat("x9", 14), "github-token"},
		"an API key":           {"use sk-" + strings.Repeat("k", 24), strings.Repeat("k", 24), "api-key"},
		"a chat token":         {"xoxb-" + strings.Repeat("7", 12), strings.Repeat("7", 12), "slack-token"},
		"a signed web token":   {"jwt " + jwt, strings.Repeat("c", 16), "jwt"},
		"a bearer header":      {"Authorization: Bearer " + strings.Repeat("t", 20), strings.Repeat("t", 20), "bearer"},
		"a password in a URL":  {"clone https://user:" + "synthetic-pass" + "@example.test/x.git", "synthetic-pass", "password"},
		"a private key block":  {"-----BEGIN RSA " + "PRIVATE KEY-----\nsynthetic\n-----END RSA " + "PRIVATE KEY-----", "synthetic", "private-key"},
	} {
		masked := ScrubSecretPatterns(sample.text)
		if strings.Contains(masked, sample.secret) || !strings.Contains(masked, "«redacted:"+sample.kind+"»") {
			t.Errorf("%s: masked as %q", name, masked)
		}
	}
	if masked := ScrubSecretPatterns("clone https://user:" + "synthetic-pass" + "@example.test/x.git"); masked != "clone https://user:«redacted:password»@example.test/x.git" {
		t.Errorf("only the password is masked: %q", masked)
	}
	for _, plain := range []string{
		"", "The parser skips blank lines; see commit " + strings.Repeat("a", 40) + ".", "https://example.test/path?query=1", "sha256:" + strings.Repeat("0", 64),
	} {
		if masked := ScrubSecretPatterns(plain); masked != plain {
			t.Errorf("ordinary text changed: %q -> %q", plain, masked)
		}
	}
}
