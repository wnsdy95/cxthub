package domain

import "testing"

func TestNormalizeGitRemoteURLGolden(test *testing.T) {
	for _, golden := range []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", ""},
		{"sanitized first", " \thttps://oauth-user:secret@GitHub.com/Acme/Orders.git?token=secret#fragment \n", "github.com/acme/orders"},
		{"ssh port", "ssh://git@GitHub.com:2222/Acme/Orders.git", "github.com:2222/acme/orders"},
		{"scp credentials", "deploy-token@GitLab.Example:Acme/Orders.git", "gitlab.example/acme/orders"},
		{"git ssh", "git+ssh://git@GitHub.com/Acme/Orders.git", "github.com/acme/orders"},
		{"escaped slash", "https://GitHub.com/Acme/Name%2FPart.git", "github.com/acme/name%2fpart"},
		{"escaped space", "https://GitHub.com/Acme/Two Words.git", "github.com/acme/two%20words"},
		{"escaped suffix", "https://GitHub.com/Acme/Orders%2Egit", "github.com/acme/orders%2egit"},
		{"uppercase suffix before lowercase", "https://GitHub.com/Acme/Orders.GIT", "github.com/acme/orders.git"},
		{"suffix before trailing slashes", "https://GitHub.com/Acme/Orders.git///", "github.com/acme/orders.git"},
		{"single suffix removal", "https://GitHub.com/Acme/Orders.git.git", "github.com/acme/orders.git"},
		{"local path rejected", "/Users/alice/private/repo", ""},
		{"file scheme rejected", "file:///Users/alice/private/repo", ""},
		{"missing host rejected", "https://token@", ""},
		{"malformed escaping rejected", "https://GitHub.com/Acme/Orders%zz.git", ""},
	} {
		test.Run(golden.name, func(test *testing.T) {
			if normalized := NormalizeGitRemoteURL(golden.raw); normalized != golden.want {
				test.Fatalf("NormalizeGitRemoteURL(%q) = %q, want %q", golden.raw, normalized, golden.want)
			}
		})
	}
}
