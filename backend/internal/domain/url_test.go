package domain

import "testing"

func TestSanitizeRemoteURL(t *testing.T) {
	raw := "https://oauth-user:super-secret@GitHub.com/Acme/Orders.git?token=also-secret#fragment"
	if got, want := SanitizeRemoteURL(raw), "https://GitHub.com/Acme/Orders.git"; got != want {
		t.Fatalf("SanitizeRemoteURL() = %q, want %q", got, want)
	}
	if got, want := SanitizeRemoteURL("git@github.com:Acme/Orders.git"), "git@github.com:Acme/Orders.git"; got != want {
		t.Fatalf("SCP remote changed: %q", got)
	}
	if got, want := SanitizeRemoteURL("deploy-token@gitlab.example:Acme/Orders.git"), "git@gitlab.example:Acme/Orders.git"; got != want {
		t.Fatalf("SCP credential not sanitized: %q", got)
	}
	for _, raw := range []string{"/Users/alice/private/repo", "file:///Users/alice/private/repo", "https://token@", "token@host:"} {
		if got := SanitizeRemoteURL(raw); got != "" {
			t.Errorf("SanitizeRemoteURL(%q) = %q, want empty", raw, got)
		}
	}
}
