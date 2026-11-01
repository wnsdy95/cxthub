package evidencequery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app/contextprojection"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type codeApplicabilityFixture struct {
	trees     *gitEvidenceTestTrees
	deltas    *gitEvidenceTestDeltas
	selection domain.CodeSelection
	revision  domain.RepositoryRevision
}

func newCodeApplicabilityFixture() codeApplicabilityFixture {
	before := domain.GitEntry{OID: "old", Mode: "100644"}
	after := domain.GitEntry{OID: "fresh", Mode: "100755"}
	return codeApplicabilityFixture{
		trees: &gitEvidenceTestTrees{
			commits: map[string]domain.GitCommitTree{
				"selected": {Commit: "selected", Tree: "selected-tree", Parents: []string{"source"}},
				"source":   {Commit: "source", Tree: "source-tree", Parents: []string{"parent"}},
				"parent":   {Commit: "parent", Tree: "parent-tree", Parents: []string{}},
			},
			nodes: map[string]domain.GitTreeNode{
				"selected-tree": {OID: "selected-tree", Entries: map[string]domain.GitEntry{"file": after}},
				"source-tree":   {OID: "source-tree", Entries: map[string]domain.GitEntry{"file": after}},
				"parent-tree":   {OID: "parent-tree", Entries: map[string]domain.GitEntry{"file": before}},
			},
			commitErrors: map[string]error{},
			nodeErrors:   map[string]error{},
		},
		deltas: &gitEvidenceTestDeltas{
			values: map[[2]string]domain.GitCommitDelta{
				{"source", ""}: {Commit: "source", Parent: "parent", Parents: []string{"parent"}, Complete: true,
					Changes: []domain.GitPathChange{{Path: "file", Before: before, After: after}}},
			},
			errors: map[[2]string]error{},
		},
		selection: domain.CodeSelection{CodeCommit: "selected", SourceCommit: "source", Paths: []string{"file"}},
		revision:  domain.RepositoryRevision{Evidence: 9007199254740993, Graph: 7, Pending: 11},
	}
}

func (fixture codeApplicabilityFixture) owner(test testing.TB, strict bool) *GitEvidence {
	test.Helper()
	return gitEvidenceTestOwner(test, GitEvidenceConfig{
		RepoID: gitEvidenceTestRepo, Origin: gitEvidenceTestOrigin,
		Trees: fixture.trees, Deltas: fixture.deltas, RequireComplete: strict,
	})
}

func assertCodeApplicabilityHash(test testing.TB, view domain.CodeApplicability) string {
	test.Helper()
	hash := view.StateHash
	view.StateHash = ""
	raw, err := json.Marshal(view)
	if err != nil || hash != domain.HashContent(raw) {
		test.Fatal("applicability hash no longer covers the original unhashed JSON", view, hash, err)
	}
	return string(raw)
}

func TestGitEvidenceQueryOriginalJSONAndSharedOwner(test *testing.T) {
	fixture := newCodeApplicabilityFixture()
	evidence := fixture.owner(test, false)
	ctx := context.Background()
	if relation, err := evidence.Relation(ctx, "source", "selected"); relation != "ancestor" || err != nil || evidence.ReadCount() != 1 {
		test.Fatal("prime policy evidence", relation, err, evidence.ReadCount())
	}
	view, err := evidence.Query(ctx, fixture.selection, fixture.revision)
	if err != nil {
		test.Fatal(err)
	}
	wantJSON := `{"selection":{"code_commit":"selected","source_commit":"source","source_parent":"parent","paths":["file"]},"revision":{"evidence":"9007199254740993","graph":"7","pending":"11"},"state_hash":"","relation":"ancestor","paths":[{"path":"file","state":"applied","before":{"oid":"old","mode":"100644"},"after":{"oid":"fresh","mode":"100755"},"selected":{"oid":"fresh","mode":"100755"}}]}`
	if raw := assertCodeApplicabilityHash(test, view); raw != wantJSON {
		test.Fatal("original applicability JSON changed", raw)
	}
	if evidence.ReadCount() != 7 || len(fixture.trees.commitCalls) != 3 || len(fixture.trees.nodeCalls) != 3 || len(fixture.deltas.calls) != 1 {
		test.Fatal("query copied the policy cache or read budget", evidence.ReadCount(), fixture.trees, fixture.deltas)
	}
	for _, calls := range [][]gitEvidenceTestCall{fixture.trees.commitCalls, fixture.trees.nodeCalls, fixture.deltas.calls} {
		for _, call := range calls {
			if call.repo != gitEvidenceTestRepo || call.origin != gitEvidenceTestOrigin {
				test.Fatal("query substituted the bound repository or origin", call)
			}
		}
	}
	fixture.trees.nodes["selected-tree"] = domain.GitTreeNode{OID: "selected-tree"}
	for repeat := 0; repeat < 100; repeat++ {
		again, err := evidence.Query(ctx, fixture.selection, fixture.revision)
		if err != nil || !reflect.DeepEqual(view, again) || evidence.ReadCount() != 7 {
			test.Fatal("repeated claims reread or changed response-lifetime evidence", again, err, evidence.ReadCount())
		}
	}
	fresh, err := fixture.owner(test, false).Query(ctx, fixture.selection, fixture.revision)
	if err != nil || fresh.Paths[0].State != "changed" || fresh.StateHash == view.StateHash {
		test.Fatal("a new response inherited the old node cache", fresh, err)
	}
	if fixture.selection.SourceParent != "" {
		test.Fatal("inferred comparison parent mutated the caller's selection")
	}
}

func TestGitEvidenceQueryReversalReapplyAndIndependentSelections(test *testing.T) {
	fixture := newCodeApplicabilityFixture()
	before := fixture.trees.nodes["parent-tree"].Entries["file"]
	after := fixture.trees.nodes["source-tree"].Entries["file"]
	other := domain.GitEntry{OID: "other", Mode: "100644"}
	for _, selected := range []struct {
		commit  string
		parents []string
		entry   domain.GitEntry
	}{
		{"reversed", []string{"selected"}, before},
		{"reapplied", []string{"reversed"}, after},
		{"later", []string{"reapplied"}, other},
		{"sibling", []string{"parent"}, after},
		{"incomplete", []string{"missing"}, after},
	} {
		fixture.trees.commits[selected.commit] = domain.GitCommitTree{Commit: selected.commit, Tree: selected.commit + "-tree", Parents: selected.parents}
		fixture.trees.nodes[selected.commit+"-tree"] = domain.GitTreeNode{Entries: map[string]domain.GitEntry{"file": selected.entry}}
	}
	evidence := fixture.owner(test, false)
	hashes := map[string]domain.ContentHash{}
	for _, scenario := range []struct{ commit, path, state, relation, reason string }{
		{"source", "file", "applied", "ancestor", ""},
		{"selected", "file", "applied", "ancestor", ""},
		{"reversed", "file", "before", "ancestor", ""},
		{"reapplied", "file", "applied", "ancestor", ""},
		{"later", "file", "changed", "ancestor", ""},
		{"parent", "file", "not_in_history", "not_ancestor", ""},
		{"sibling", "file", "equivalent", "not_ancestor", ""},
		{"incomplete", "file", "unknown", "unknown", "incomplete_ancestry"},
		{"reapplied", "absent", "unknown", "ancestor", "path_not_changed_by_source"},
	} {
		selection := fixture.selection
		selection.CodeCommit, selection.Paths = scenario.commit, []string{scenario.path}
		view, err := evidence.Query(context.Background(), selection, fixture.revision)
		if err != nil || len(view.Paths) != 1 || view.Relation != scenario.relation || view.Paths[0].State != scenario.state || view.Paths[0].Reason != scenario.reason {
			test.Fatal("observable reversal/reapply/independent selection semantics changed", scenario, view, err)
		}
		assertCodeApplicabilityHash(test, view)
		if scenario.path == "file" {
			hashes[scenario.commit] = view.StateHash
		}
	}
	if hashes["selected"] == hashes["reapplied"] {
		test.Fatal("state hash lost the explicitly selected code commit")
	}
}

func TestGitEvidenceQueryUnavailableRemainsLazyAndUnvalidated(test *testing.T) {
	fixture := newCodeApplicabilityFixture()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, strict := range []bool{false, true} {
		for _, missing := range []string{"owner", "trees", "deltas", "origin"} {
			test.Run(fmt.Sprintf("%s/strict=%t", missing, strict), func(test *testing.T) {
				config := GitEvidenceConfig{RepoID: "opaque", Origin: gitEvidenceTestOrigin, Trees: fixture.trees, Deltas: fixture.deltas, RequireComplete: strict}
				switch missing {
				case "trees":
					config.Trees = nil
				case "deltas":
					config.Deltas = nil
				case "origin":
					config.Origin = ""
				}
				var evidence *GitEvidence
				if missing != "owner" {
					evidence = gitEvidenceTestOwner(test, config)
				}
				selection := domain.CodeSelection{Paths: []string{"file", "dir/file"}}
				view, err := evidence.Query(canceled, selection, domain.RepositoryRevision{})
				if err != nil || view.Reason != "git_evidence_unavailable" || view.Relation != "unknown" || len(view.Paths) != 2 {
					test.Fatal("unavailable compatibility acquired eager validation, cancellation, or strictness", view, err)
				}
				for index, path := range view.Paths {
					if path.Path != selection.Paths[index] || path.State != "unknown" || path.Reason != view.Reason {
						test.Fatal("unavailable fallback lost path order or reasons", view)
					}
				}
				assertCodeApplicabilityHash(test, view)
				if evidence != nil && (evidence.ReadCount() != 0 || evidence.Limited() || evidence.TraversalLimited()) {
					test.Fatal("unavailable query consumed evidence")
				}
			})
		}
	}
	if len(fixture.trees.commitCalls)+len(fixture.trees.nodeCalls)+len(fixture.deltas.calls) != 0 {
		test.Fatal("unavailable query accessed a remaining reader")
	}
	var missing *GitEvidence
	view, err := missing.Query(canceled, domain.CodeSelection{}, domain.RepositoryRevision{})
	if err != nil || assertCodeApplicabilityHash(test, view) != `{"selection":{"code_commit":"","source_commit":"","paths":null},"revision":{"graph":"0","pending":"0"},"state_hash":"","relation":"unknown","paths":[],"reason":"git_evidence_unavailable"}` {
		test.Fatal("empty unavailable JSON changed", view, err)
	}
}

func TestGitEvidenceQueryPendingEvidenceAndPathOrder(test *testing.T) {
	for _, scenario := range []struct {
		name       string
		change     func(codeApplicabilityFixture)
		reason     string
		pathReason string
		reads      int
	}{
		{"selected", func(fixture codeApplicabilityFixture) { delete(fixture.trees.commits, "selected") }, "selected_tree_pending", "selected_tree_pending", 1},
		{"source", func(fixture codeApplicabilityFixture) { delete(fixture.trees.commits, "source") }, "source_tree_pending", "source_tree_pending", 2},
		{"delta", func(fixture codeApplicabilityFixture) { delete(fixture.deltas.values, [2]string{"source", ""}) }, "source_delta_pending", "source_delta_pending", 3},
		{"comparison", func(fixture codeApplicabilityFixture) { delete(fixture.trees.commits, "parent") }, "comparison_tree_pending", "comparison_tree_pending", 4},
		{"comparison node", func(fixture codeApplicabilityFixture) { delete(fixture.trees.nodes, "parent-tree") }, "", "comparison_tree_incomplete", 5},
		{"source node", func(fixture codeApplicabilityFixture) { delete(fixture.trees.nodes, "source-tree") }, "", "source_tree_incomplete", 6},
		{"selected node", func(fixture codeApplicabilityFixture) { delete(fixture.trees.nodes, "selected-tree") }, "", "selected_tree_incomplete", 7},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newCodeApplicabilityFixture()
			scenario.change(fixture)
			evidence := fixture.owner(test, false)
			for repeat := 0; repeat < 2; repeat++ {
				view, err := evidence.Query(context.Background(), fixture.selection, fixture.revision)
				if err != nil || view.Reason != scenario.reason || len(view.Paths) != 1 || view.Paths[0].State != "unknown" || view.Paths[0].Reason != scenario.pathReason || evidence.ReadCount() != scenario.reads {
					test.Fatal("partial publication asserted applicability or repeated immutable reads", view, err, evidence.ReadCount())
				}
				assertCodeApplicabilityHash(test, view)
			}
		})
	}
	fixture := newCodeApplicabilityFixture()
	fixture.selection.Paths = []string{"unchanged", "file"}
	view, err := fixture.owner(test, false).Query(context.Background(), fixture.selection, fixture.revision)
	if err != nil || len(view.Paths) != 2 || view.Paths[0].Path != "unchanged" || view.Paths[0].Reason != "path_not_changed_by_source" || view.Paths[1].Path != "file" || view.Paths[1].State != "applied" {
		test.Fatal("query reordered paths or lost an independent result", view, err)
	}
}

func TestGitEvidenceQueryComparisonParentAndRootCommit(test *testing.T) {
	fixture := newCodeApplicabilityFixture()
	source := fixture.trees.commits["source"]
	source.Parents = []string{"parent", "other-parent"}
	fixture.trees.commits["source"] = source
	evidence := fixture.owner(test, false)
	view, err := evidence.Query(context.Background(), fixture.selection, fixture.revision)
	if err != nil || view.Reason != "comparison_parent_required" || evidence.ReadCount() != 2 || len(fixture.deltas.calls) != 0 {
		test.Fatal("merge source chose an implicit parent", view, err)
	}
	assertCodeApplicabilityHash(test, view)
	fixture.selection.SourceParent = "not-a-parent"
	view, err = evidence.Query(context.Background(), fixture.selection, fixture.revision)
	if !errors.Is(err, domain.ErrValidation) || view.StateHash != "" || evidence.ReadCount() != 2 || len(fixture.deltas.calls) != 0 {
		test.Fatal("invalid parent triggered a delta read or a completed hash", view, err)
	}
	fixture.selection.SourceParent = "parent"
	delta := fixture.deltas.values[[2]string{"source", ""}]
	delta.Parents = source.Parents
	fixture.deltas.values[[2]string{"source", "parent"}] = delta
	view, err = evidence.Query(context.Background(), fixture.selection, fixture.revision)
	if err != nil || view.Paths[0].State != "applied" || view.Selection.SourceParent != "parent" || len(fixture.deltas.calls) != 1 || fixture.deltas.calls[0].parent != "parent" {
		test.Fatal("explicit merge comparison was not preserved", view, err)
	}
	fixture = newCodeApplicabilityFixture()
	source = fixture.trees.commits["source"]
	source.Parents = []string{}
	fixture.trees.commits["source"] = source
	delta = fixture.deltas.values[[2]string{"source", ""}]
	delta.Parent, delta.Parents, delta.Changes[0].Before = "", []string{}, domain.GitEntry{}
	fixture.deltas.values[[2]string{"source", ""}] = delta
	evidence = fixture.owner(test, false)
	view, err = evidence.Query(context.Background(), fixture.selection, fixture.revision)
	if err != nil || view.Paths[0].State != "applied" || view.Selection.SourceParent != "" || evidence.ReadCount() != 5 {
		test.Fatal("root addition required a fictitious comparison tree", view, err, evidence.ReadCount())
	}
	assertCodeApplicabilityHash(test, view)
}

func TestGitEvidenceQueryRetainsFailuresAndPartialOutput(test *testing.T) {
	failure := errors.New("immutable evidence reader failure")
	for _, scenario := range []struct {
		name   string
		change func(codeApplicabilityFixture)
		err    error
	}{
		{"selected", func(fixture codeApplicabilityFixture) { fixture.trees.commitErrors["selected"] = failure }, failure},
		{"source", func(fixture codeApplicabilityFixture) { fixture.trees.commitErrors["source"] = failure }, failure},
		{"delta", func(fixture codeApplicabilityFixture) { fixture.deltas.errors[[2]string{"source", ""}] = failure }, failure},
		{"comparison", func(fixture codeApplicabilityFixture) { fixture.trees.commitErrors["parent"] = failure }, failure},
		{"comparison node", func(fixture codeApplicabilityFixture) { fixture.trees.nodeErrors["parent-tree"] = failure }, failure},
		{"source node", func(fixture codeApplicabilityFixture) { fixture.trees.nodeErrors["source-tree"] = failure }, failure},
		{"selected node", func(fixture codeApplicabilityFixture) { fixture.trees.nodeErrors["selected-tree"] = failure }, failure},
		{"ancestry", func(fixture codeApplicabilityFixture) {
			selected := fixture.trees.commits["selected"]
			selected.Parents = []string{"missing"}
			fixture.trees.commits["selected"] = selected
			fixture.trees.commitErrors["missing"] = failure
		}, failure},
		{"incomplete delta", func(fixture codeApplicabilityFixture) {
			delta := fixture.deltas.values[[2]string{"source", ""}]
			delta.Complete = false
			fixture.deltas.values[[2]string{"source", ""}] = delta
		}, domain.ErrIntegrity},
		{"wrong delta commit", func(fixture codeApplicabilityFixture) {
			delta := fixture.deltas.values[[2]string{"source", ""}]
			delta.Commit = "another"
			fixture.deltas.values[[2]string{"source", ""}] = delta
		}, domain.ErrIntegrity},
		{"wrong delta parents", func(fixture codeApplicabilityFixture) {
			delta := fixture.deltas.values[[2]string{"source", ""}]
			delta.Parents = []string{"another"}
			fixture.deltas.values[[2]string{"source", ""}] = delta
		}, domain.ErrIntegrity},
		{"wrong before entry", func(fixture codeApplicabilityFixture) {
			fixture.trees.nodes["parent-tree"].Entries["file"] = domain.GitEntry{}
		}, domain.ErrIntegrity},
		{"wrong after entry", func(fixture codeApplicabilityFixture) {
			fixture.trees.nodes["source-tree"].Entries["file"] = domain.GitEntry{}
		}, domain.ErrIntegrity},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newCodeApplicabilityFixture()
			scenario.change(fixture)
			evidence := fixture.owner(test, false)
			var first domain.CodeApplicability
			reads := 0
			for repeat := 0; repeat < 2; repeat++ {
				view, err := evidence.Query(context.Background(), fixture.selection, fixture.revision)
				if !errors.Is(err, scenario.err) || view.StateHash != "" || view.Revision != fixture.revision || view.Paths == nil || len(view.Paths) != 0 || view.Reason != "" {
					test.Fatal("reader/integrity failure was finalized or lost its partial output", view, err)
				}
				if repeat == 0 {
					first, reads = view, evidence.ReadCount()
				} else if !reflect.DeepEqual(first, view) || evidence.ReadCount() != reads {
					test.Fatal("query replaced a retained failure or reread evidence", view, evidence.ReadCount())
				}
			}
		})
	}
	fixture := newCodeApplicabilityFixture()
	fixture.selection.Paths = []string{"unchanged", "file"}
	fixture.trees.nodeErrors["selected-tree"] = failure
	view, err := fixture.owner(test, false).Query(context.Background(), fixture.selection, fixture.revision)
	if !errors.Is(err, failure) || view.StateHash != "" || len(view.Paths) != 1 || view.Paths[0].Path != "unchanged" || view.Paths[0].Reason != "path_not_changed_by_source" || view.Relation != "ancestor" || view.Selection.SourceParent != "parent" {
		test.Fatal("later path failure erased earlier results or normalized selection", view, err)
	}
}

func TestGitEvidenceQuerySharesTheOriginalReadLimit(test *testing.T) {
	for _, strict := range []bool{false, true} {
		for remaining := 0; remaining < 7; remaining++ {
			test.Run(fmt.Sprintf("remaining=%d/strict=%t", remaining, strict), func(test *testing.T) {
				fixture := newCodeApplicabilityFixture()
				evidence := fixture.owner(test, strict)
				for position := 0; position < contextprojection.MaxCodeEvidenceReads-remaining; position++ {
					if err := evidence.ConsumeRead(context.Background(), gitEvidenceTestRepo, gitEvidenceTestOrigin); err != nil {
						test.Fatal("reserve preceding policy/memory work", err)
					}
				}
				view, err := evidence.Query(context.Background(), fixture.selection, fixture.revision)
				if !evidence.Limited() || evidence.TraversalLimited() || evidence.ReadCount() != contextprojection.MaxCodeEvidenceReads || len(fixture.trees.commitCalls)+len(fixture.trees.nodeCalls)+len(fixture.deltas.calls) != remaining {
					test.Fatal("query introduced an independent budget or traversal limit", view, err, evidence.ReadCount())
				}
				if strict {
					if !errors.Is(err, domain.ErrMemoryProjectionLimit) || view.StateHash != "" {
						test.Fatal("strict exhaustion became unavailable success", view, err)
					}
					return
				}
				wantPathReason := "evidence_budget_exhausted"
				if remaining >= 4 {
					wantPathReason = []string{"comparison_tree_incomplete", "source_tree_incomplete", "selected_tree_incomplete"}[remaining-4]
				}
				if err != nil || view.Reason != "evidence_budget_exhausted" || len(view.Paths) != 1 || view.Paths[0].State != "unknown" || view.Paths[0].Reason != wantPathReason {
					test.Fatal("compatibility exhaustion lost its precise path state or global reason", view, err)
				}
				assertCodeApplicabilityHash(test, view)
			})
		}
		fixture := newCodeApplicabilityFixture()
		evidence := fixture.owner(test, strict)
		view, err := evidence.Query(context.Background(), fixture.selection, fixture.revision)
		if err != nil {
			test.Fatal(err)
		}
		for evidence.ReadCount() < contextprojection.MaxCodeEvidenceReads {
			if err := evidence.ConsumeRead(context.Background(), gitEvidenceTestRepo, gitEvidenceTestOrigin); err != nil {
				test.Fatal(err)
			}
		}
		exhaustion := domain.ErrNotFound
		if strict {
			exhaustion = domain.ErrMemoryProjectionLimit
		}
		if err := evidence.ConsumeRead(context.Background(), gitEvidenceTestRepo, gitEvidenceTestOrigin); !errors.Is(err, exhaustion) {
			test.Fatal("exhaust shared budget", err)
		}
		again, err := evidence.Query(context.Background(), fixture.selection, fixture.revision)
		if err != nil || again.Reason != "evidence_budget_exhausted" || !reflect.DeepEqual(view.Paths, again.Paths) || again.StateHash == view.StateHash || evidence.ReadCount() != contextprojection.MaxCodeEvidenceReads {
			test.Fatal("exhaustion erased cached evidence or failed to enter the response hash", again, err)
		}
		assertCodeApplicabilityHash(test, again)
	}
}

func TestGitEvidenceQueryCancellationPrecedesCachedReads(test *testing.T) {
	fixture := newCodeApplicabilityFixture()
	evidence := fixture.owner(test, false)
	if _, err := evidence.Query(context.Background(), fixture.selection, fixture.revision); err != nil {
		test.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	view, err := evidence.Query(canceled, fixture.selection, fixture.revision)
	if !errors.Is(err, context.Canceled) || view.StateHash != "" || view.Relation != "unknown" || view.Paths == nil || len(view.Paths) != 0 || view.Selection.SourceParent != "" || view.Revision != fixture.revision || evidence.ReadCount() != 7 {
		test.Fatal("cached query ignored cancellation or finalized an error response", view, err, evidence.ReadCount())
	}
	fixture = newCodeApplicabilityFixture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture.trees.cancel, fixture.trees.cancelAt = cancel, 2
	evidence = fixture.owner(test, false)
	view, err = evidence.Query(ctx, fixture.selection, fixture.revision)
	if !errors.Is(err, context.Canceled) || view.StateHash != "" || evidence.ReadCount() != 2 || len(fixture.deltas.calls) != 0 {
		test.Fatal("reader cancellation was masked or allowed later reads", view, err, evidence.ReadCount())
	}
}
