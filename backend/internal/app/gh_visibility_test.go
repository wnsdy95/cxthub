package app

import "testing"

// TestGithubRepoPath fixes various remote format recognition and path manipulation rejections (reviewed issues).
func TestGithubRepoPath(t *testing.T) {
	cases := map[string]string{
		"git@github.com:org/repo.git":          "org/repo",
		"https://github.com/org/repo":          "org/repo",
		"https://github.com/org/repo.git":      "org/repo",
		"http://github.com/org/repo":           "org/repo",
		"ssh://git@github.com/org/repo.git":    "org/repo", // Previously missing format
		"git://github.com/org/repo":            "org/repo",
		"https://user:tok@github.com/org/repo": "org/repo", // User info
		"https://GitHub.com/org/repo":          "org/repo", // Case-insensitive host
		"https://github.com/../secret":         "",         // Path manipulation rejection
		"https://github.com/org":               "",         // Insufficient segments
		"https://github.com/org/repo/extra":    "",         // Too many segments
		"https://gitlab.com/org/repo":          "",         // Not GitHub
		"git@github.com:../secret":             "",         // manipulate SCP path
	}
	for in, want := range cases {
		if got := githubRepoPath(in); got != want {
			t.Errorf("githubRepoPath(%q)=%q want %q", in, got, want)
		}
	}
}
