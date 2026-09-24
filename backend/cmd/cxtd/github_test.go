package main

import "testing"

func TestGitHubConfigurationFailsClosed(t *testing.T) {
	for _, key := range []string{"CXT_GITHUB_APP_ID", "CXT_GITHUB_APP_CLIENT_ID", "CXT_GITHUB_APP_CLIENT_SECRET", "CXT_GITHUB_APP_PRIVATE_KEY", "CXT_GITHUB_APP_SLUG", "CXT_GITHUB_APP_WEBHOOK_SECRET"} {
		t.Setenv(key, "")
	}
	if g, c, err := configureGitHub(nil, nil, nil, "https://example.test"); err != nil || g != nil || c != nil {
		t.Fatal("unset App should remain disabled")
	}
	t.Setenv("CXT_GITHUB_APP_ID", "123")
	if _, _, err := configureGitHub(nil, nil, nil, "https://example.test"); err == nil {
		t.Fatal("partial App config accepted")
	}
	for _, origin := range []string{"http://example.test", "https://user:secret@example.test", "https://example.test/path", "https://example.test?redirect=evil", "//example.test"} {
		if err := validateGitHubOrigin(origin); err == nil {
			t.Fatalf("unsafe callback accepted: %s", origin)
		}
	}
	for _, origin := range []string{"https://example.test", "http://localhost:5173", "http://127.0.0.1:8907"} {
		if err := validateGitHubOrigin(origin); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CXT_WEB_URL", "https://frontend.example.test/")
	if got, err := githubReturnURL("https://api.example.test"); err != nil || got != "https://frontend.example.test/connect/github" {
		t.Fatalf("return URL: %s %v", got, err)
	}
}
