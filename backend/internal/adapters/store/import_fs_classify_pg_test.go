//go:build postgres

package store

import "testing"

func TestFSImportTreatsTheRemoteGitCacheAsDerived(t *testing.T) {
	repo := "repos/sha256-" + "0000000000000000000000000000000000000000000000000000000000000000"
	if kind, err := classifyFSImport(repo + "/remote-git.json"); err != nil || kind != "derived" {
		t.Fatalf("the provider cache: %q %v", kind, err)
	}
	// Records the import does not carry over are still refused, never dropped.
	for _, path := range []string{repo + "/remote-git.json/extra", repo + "/branch-ledger.json", repo + "/anchored-notes.json"} {
		if kind, err := classifyFSImport(path); err == nil {
			t.Errorf("%s was accepted as %q", path, kind)
		}
	}
}
