package domain

import "testing"

func TestCatalogAcceptsPrivateOnlyGenerationsAndSequenceGaps(t *testing.T) {
	repo := catalogTestRepo()
	after := CatalogCheckpoint{Version: 1, RepoID: repo, Epoch: catalogTestEpoch, Sequence: 2}
	through := after
	through.Sequence = 15
	page := CatalogPage{Version: 1, RepoID: repo, Epoch: through.Epoch, Through: through.Sequence, Mode: "delta", Entries: []CatalogEntry{}, Checkpoint: &through}
	if err := ValidateCatalogPage(repo, &after, nil, page); err != nil {
		t.Fatalf("private-only generation rejected: %v", err)
	}
	page.Entries = []CatalogEntry{catalogTestSnapshotEntry(t, "first-public", 4), catalogTestSnapshotEntry(t, "second-public", 12)}
	if err := ValidateCatalogPage(repo, &after, nil, page); err != nil {
		t.Fatalf("commit-ordered sequence gaps rejected: %v", err)
	}
	page.Entries = []CatalogEntry{{Kind: "pending", Key: "private-session", Sequence: 12, Value: []byte(`{}`)}}
	if ValidateCatalogPage(repo, &after, nil, page) == nil {
		t.Fatal("private entity exposed through legacy public contract")
	}
}
