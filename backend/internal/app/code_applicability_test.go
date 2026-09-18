package app

import (
	"context"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestCodeApplicabilityReversalReapplyAndIndependentSelections(t *testing.T) {
	ctx := context.Background()
	core, st := newFsckSvc(t)
	repo := hh(t.Name())
	origin := "https://github.com/example/applicability"
	if _, e := st.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: origin}); e != nil {
		t.Fatal(e)
	}
	id := func(s string) string { return strings.Repeat(s, 40) }
	r, a, b, rev, again, sibling, later := id("1"), id("2"), id("3"), id("4"), id("5"), id("6"), id("7")
	old := domain.GitEntry{OID: id("a"), Mode: "100644"}
	fresh := domain.GitEntry{OID: id("b"), Mode: "100755"}
	other := domain.GitEntry{OID: id("c"), Mode: "100644"}
	d := func(sha, parent string, changes ...domain.GitPathChange) domain.GitCommitDelta {
		parents := []string{}
		if parent != "" {
			parents = append(parents, parent)
		}
		return domain.GitCommitDelta{Commit: sha, Parent: parent, Parents: parents, Complete: true, Changes: changes}
	}
	reader := &scanReader{deltas: map[string]domain.GitCommitDelta{
		r:       d(r, "", domain.GitPathChange{Path: "a", After: old}),
		a:       d(a, r, domain.GitPathChange{Path: "a", Before: old, After: fresh}),
		b:       d(b, a, domain.GitPathChange{Path: "b", After: other}),
		rev:     d(rev, b, domain.GitPathChange{Path: "a", Before: fresh, After: old}),
		again:   d(again, rev, domain.GitPathChange{Path: "a", Before: old, After: fresh}),
		sibling: d(sibling, r, domain.GitPathChange{Path: "a", Before: old, After: fresh}),
		later:   d(later, again, domain.GitPathChange{Path: "a", Before: fresh, After: other}),
	}}
	scans, e := NewGitScans(core, reader)
	if e != nil {
		t.Fatal(e)
	}
	query := func(code, source, path string) domain.CodeApplicability {
		t.Helper()
		v, e := core.QueryCodeApplicability(ctx, repo, domain.CodeSelection{CodeCommit: code, SourceCommit: source, Paths: []string{path}})
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	missing := query(later, a, "a")
	if missing.Paths[0].State != "unknown" || missing.Reason != "selected_tree_pending" {
		t.Fatal(missing)
	}
	for _, tip := range []string{later, sibling} {
		if _, e = scans.ObservePush(ctx, origin, "refs/heads/main", "", tip, false, tip); e != nil {
			t.Fatal(e)
		}
	}
	if e = scans.Process(ctx, 100); e != nil {
		t.Fatal(e)
	}
	for _, tt := range []struct{ code, source, path, state, relation string }{
		{a, a, "a", "applied", "ancestor"}, {rev, a, "a", "before", "ancestor"},
		{rev, b, "b", "applied", "ancestor"}, {again, a, "a", "applied", "ancestor"},
		{later, a, "a", "changed", "ancestor"}, {b, a, "a", "applied", "ancestor"},
		{r, a, "a", "not_in_history", "not_ancestor"}, {sibling, a, "a", "equivalent", "not_ancestor"},
		{again, a, "absent", "unknown", "ancestor"},
	} {
		v := query(tt.code, tt.source, tt.path)
		if v.Relation != tt.relation || v.Paths[0].State != tt.state {
			t.Fatalf("%+v -> %+v", tt, v)
		}
	}
	if query(later, a, "a").StateHash == missing.StateHash {
		t.Fatal("cache did not reflect arriving evidence")
	}
	if query(b, a, "a").StateHash == query(again, a, "a").StateHash {
		t.Fatal("cache lost explicit selected code")
	}
	if refs, e := core.ListRefs(ctx, repo); e != nil || len(refs) != 0 {
		t.Fatal("read/discovery changed shared position", refs, e)
	}
	if history, e := core.ListHistory(ctx, repo); e != nil || len(history) != 0 {
		t.Fatal("created fictitious merge history", history, e)
	}
	revision, e := core.RepositoryRevision(ctx, repo)
	if e != nil || revision.Graph != 0 || revision.Pending != 0 || revision.Evidence == 0 {
		t.Fatal("evidence discovery invalidated context graph", revision, e)
	}
	reader.offline = true
	before := reader.reads
	query(again, a, "a")
	if reader.reads != before {
		t.Fatal("read query fetched provider")
	}
}
