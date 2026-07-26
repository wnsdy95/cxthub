package app

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// TestCollectObjectsIncludesReachableStash ensures that push target collection excludes stash by "reachability" rather than "label". Content-hash deduplication can lead to a "(stash)" label object being placed in the commit ancestry if the same session content is in both stash and commit (same ID). Simply removing the label can lead to parent loss on the server and permanent ref advancement blockage (real case 9b23182f68).
func TestCollectObjectsIncludesReachableStash(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	svc := NewSyncRepoService(st, nil, nil)
	mk := func(label, branch, message string, parents ...domain.ContentHash) domain.ContentHash {
		h, err := st.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{
			Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, SessionOriginID: label},
			Events:   []domain.Event{{Kind: domain.EventMessage, Seq: 0, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: label}}}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.PutSnapshot(ctx, domain.Snapshot{ID: h, DocHash: h, Branch: branch, Parents: parents, Message: message}); err != nil {
			t.Fatal(err)
		}
		return h
	}

	// A(root) ← S("(stash)" label but part of commit ancestry) ← C(main tip)
	// P: A pure stash that cannot be reached from any ref — this must remain local.
	a := mk("a", "main", "")
	sMid := mk("s", domain.StashBranchLabel, "WIP on main", a)
	c := mk("c", "main", "", sMid)
	p := mk("p", domain.StashBranchLabel, "WIP on main 2", a)

	man := domain.Manifest{
		RepoID:        "repo-1",
		Refs:          []domain.Ref{{Kind: domain.RefBranch, Name: "main", RepoID: "repo-1", Target: c}},
		SnapshotIndex: []domain.ContentHash{a, sMid, c, p},
	}
	snaps, _, err := svc.collectObjects(ctx, "repo-1", man)
	if err != nil {
		t.Fatal(err)
	}
	got := map[domain.ContentHash]bool{}
	for _, s := range snaps {
		got[s.ID] = true
	}
	if !got[sMid] {
		t.Fatal("Reachable (stash) object missing from push — server parent loss (fsck corruption) recurrence")
	}
	if got[p] {
		t.Fatal("Unreachable pure stash included in push — stash must be local only")
	}
	if !got[a] || !got[c] {
		t.Fatal("General commit collection regression")
	}
}

// TestPutSnapshotPromotesStashToCommit ensures that content-hash deduplication promotes stash → commit label: If a stash (with the same ID) is first stored and then re-stored as a commit (non-stash), the branch/message is updated to the commit (hash-derived meta — ID/body immutable). The reverse (commit first, stash later) maintains the commit label.
func TestPutSnapshotPromotesStashToCommit(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	mkID := func(label string) domain.ContentHash {
		h, err := st.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{
			Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, SessionOriginID: label},
			Events:   []domain.Event{{Kind: domain.EventMessage, Seq: 0, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: label}}}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	id := mkID("d")

	// Stash first → commit re-save: Promotion.
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, Branch: domain.StashBranchLabel, Message: "WIP on main"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, Branch: "main", Message: "feat: x [git abc1234]"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSnapshot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Branch != "main" || got.Message != "feat: x [git abc1234]" {
		t.Fatalf("Promotion failure: branch=%q message=%q — (stash) label contaminates commit ancestry", got.Branch, got.Message)
	}

	// Commit first → stash re-save: commit label preserved (no-op).
	id2 := mkID("e")
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id2, DocHash: id2, Branch: "main", Message: "feat: y"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id2, DocHash: id2, Branch: domain.StashBranchLabel, Message: "WIP"}); err != nil {
		t.Fatal(err)
	}
	got2, err := st.GetSnapshot(ctx, id2)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Branch != "main" || got2.Message != "feat: y" {
		t.Fatalf("Commit label downgraded during stash re-save: branch=%q message=%q", got2.Branch, got2.Message)
	}
}
