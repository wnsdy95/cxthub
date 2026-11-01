package evidencequery

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app/contextprojection"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func statusCodeFixture() codeApplicabilityFixture {
	fixture := newCodeApplicabilityFixture()
	selected, source, parent := strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("3", 40)
	fixture.selection.CodeCommit, fixture.selection.SourceCommit = selected, source
	fixture.trees.commits = map[string]domain.GitCommitTree{
		selected: {Commit: selected, Tree: "selected-tree", Parents: []string{source}},
		source:   {Commit: source, Tree: "source-tree", Parents: []string{parent}},
		parent:   {Commit: parent, Tree: "parent-tree", Parents: []string{}},
	}
	delta := fixture.deltas.values[[2]string{"source", ""}]
	delta.Commit, delta.Parent, delta.Parents = source, parent, []string{parent}
	fixture.deltas.values = map[[2]string]domain.GitCommitDelta{{source, ""}: delta}
	return fixture
}

func statusCodeQueries(test testing.TB, config CodeConfig) *CodeQueries {
	test.Helper()
	queries, err := NewCodeQueries(config)
	if err != nil {
		test.Fatal(err)
	}
	return queries
}

func statusCodeConfig(probe *statusProbe, fixture codeApplicabilityFixture) CodeConfig {
	probe.trees, probe.deltas, probe.revision = fixture.trees, fixture.deltas, fixture.revision
	return CodeConfig{Scope: probe, Metadata: probe, Revisions: probe, Trees: probe, Deltas: probe}
}

func TestCodeQueriesRunActualEvidenceWithinOneReadAndResponseLifetime(test *testing.T) {
	fixture := statusCodeFixture()
	probe := newStatusProbe()
	queries := statusCodeQueries(test, statusCodeConfig(probe, fixture))
	wantFixture := statusCodeFixture()
	want, err := wantFixture.owner(test, false).Query(context.Background(), wantFixture.selection, wantFixture.revision)
	if err != nil {
		test.Fatal(err)
	}
	view, err := queries.QueryCodeApplicability(context.Background(), gitEvidenceTestRepo, fixture.selection)
	wantCalls := []string{"scope", "metadata", "revision", "commit", "commit", "delta", "commit", "node", "node", "node"}
	if err != nil || !reflect.DeepEqual(view, want) || !reflect.DeepEqual(probe.calls, wantCalls) {
		test.Fatal("public query changed the evidence algorithm, wire/hash, or pinned ordering", view, want, err, probe.calls)
	}
	assertCodeApplicabilityHash(test, view)
	if len(fixture.trees.commitCalls) != 3 || len(fixture.trees.nodeCalls) != 3 || len(fixture.deltas.calls) != 1 || fixture.selection.SourceParent != "" {
		test.Fatal("response cache was duplicated or caller selection mutated", fixture, probe.calls)
	}
	probe.metadata.GitRemoteURL = "https://example.invalid/new-origin.git"
	fixture.trees.nodes["selected-tree"] = domain.GitTreeNode{Entries: map[string]domain.GitEntry{"file": {OID: "changed", Mode: "100644"}}}
	again, err := queries.QueryCodeApplicability(context.Background(), gitEvidenceTestRepo, fixture.selection)
	if err != nil || again.Paths[0].State != "changed" || again.StateHash == view.StateHash || len(fixture.trees.commitCalls) != 6 || len(fixture.trees.nodeCalls) != 6 || len(fixture.deltas.calls) != 2 || fixture.trees.commitCalls[3].origin != probe.metadata.GitRemoteURL {
		test.Fatal("new response reused stale origin, evidence, or caches", again, err, probe.calls)
	}
}

func TestCodeQueriesValidateBeforeScopeOrStorage(test *testing.T) {
	valid := statusCodeFixture().selection
	for _, selection := range []domain.CodeSelection{
		{},
		{CodeCommit: "bad", SourceCommit: valid.SourceCommit, Paths: valid.Paths},
		{CodeCommit: valid.CodeCommit, SourceCommit: "bad", Paths: valid.Paths},
		{CodeCommit: valid.CodeCommit, SourceCommit: valid.SourceCommit, SourceParent: "bad", Paths: valid.Paths},
		{CodeCommit: valid.CodeCommit, SourceCommit: valid.SourceCommit},
		{CodeCommit: valid.CodeCommit, SourceCommit: valid.SourceCommit, Paths: []string{"file", "file"}},
		{CodeCommit: valid.CodeCommit, SourceCommit: valid.SourceCommit, Paths: []string{"../file"}},
		{CodeCommit: valid.CodeCommit, SourceCommit: valid.SourceCommit, Paths: make([]string, 101)},
	} {
		probe := newStatusProbe()
		probe.scopeError = domain.ErrForbidden
		queries := statusCodeQueries(test, CodeConfig{Scope: probe, Metadata: probe})
		view, err := queries.QueryCodeApplicability(context.Background(), gitEvidenceTestRepo, selection)
		if !errors.Is(err, domain.ErrValidation) || !reflect.DeepEqual(view, domain.CodeApplicability{}) || len(probe.calls) != 0 {
			test.Fatal("selection validation no longer precedes scope and storage", selection, view, err, probe.calls)
		}
	}
	probe := newStatusProbe()
	queries := statusCodeQueries(test, CodeConfig{Scope: probe, Metadata: probe})
	if view, err := queries.QueryCodeApplicability(context.Background(), domain.ContentHash("bad"), valid); !errors.Is(err, domain.ErrValidation) || !reflect.DeepEqual(view, domain.CodeApplicability{}) || len(probe.calls) != 0 {
		test.Fatal("repository validation moved behind scope", view, err, probe.calls)
	}
}

func TestCodeQueriesPreserveAbsentReadersAndRevisionFallback(test *testing.T) {
	for _, absentReader := range []string{"all", "trees", "deltas", "origin", "revision"} {
		fixture := statusCodeFixture()
		probe := newStatusProbe()
		config := statusCodeConfig(probe, fixture)
		wantRevision := fixture.revision
		wantCalls := []string{"scope", "metadata", "revision"}
		switch absentReader {
		case "all":
			config.Trees, config.Deltas, config.Revisions = nil, nil, nil
			wantRevision, wantCalls = domain.RepositoryRevision{}, []string{"scope", "metadata"}
		case "trees":
			config.Trees = nil
		case "deltas":
			config.Deltas = nil
		case "origin":
			probe.metadata.GitRemoteURL = ""
		case "revision":
			config.Revisions = nil
			wantRevision = domain.RepositoryRevision{}
		}
		queries := statusCodeQueries(test, config)
		view, err := queries.QueryCodeApplicability(context.Background(), gitEvidenceTestRepo, fixture.selection)
		if err != nil || view.Revision != wantRevision {
			test.Fatal("absent revision reader changed legacy zero revision", absentReader, view, err)
		}
		if absentReader == "revision" {
			if view.Paths[0].State != "applied" || probe.calls[2] != "commit" {
				test.Fatal("optional revision absence disabled actual evidence or rediscovered reader", view, probe.calls)
			}
		} else if view.Relation != "unknown" || view.Reason != "git_evidence_unavailable" || len(view.Paths) != 1 || view.Paths[0].Reason != view.Reason || !reflect.DeepEqual(probe.calls, wantCalls) {
			test.Fatal("missing capability was rediscovered or reported as missing immutable data", absentReader, view, probe.calls)
		}
		assertCodeApplicabilityHash(test, view)
	}
}

func TestCodeQueriesPreserveErrorOrderAndAtomicResults(test *testing.T) {
	for _, atomic := range []bool{false, true} {
		for _, stage := range []string{"scope", "metadata", "identity", "revision", "selected missing", "selected failure", "finish"} {
			fixture := statusCodeFixture()
			probe := newStatusProbe()
			config := statusCodeConfig(probe, fixture)
			config.AtomicRead = atomic
			failure := errors.New("code query failure")
			wantError := failure
			wantCalls := []string{"scope", "metadata", "revision"}
			switch stage {
			case "scope":
				probe.scopeError, wantCalls = failure, []string{"scope"}
			case "metadata":
				probe.repoError, probe.revError, wantCalls = failure, domain.ErrIntegrity, []string{"scope", "metadata"}
			case "identity":
				probe.metadata.ID = domain.HashContent([]byte("foreign repository"))
				wantError, wantCalls = domain.ErrIntegrity, []string{"scope", "metadata"}
			case "revision":
				probe.revError = failure
			case "selected missing":
				fixture.trees.commitErrors[fixture.selection.CodeCommit] = fmt.Errorf("selected: %w", domain.ErrNotFound)
				wantError, wantCalls = nil, append(wantCalls, "commit")
			case "selected failure":
				fixture.trees.commitErrors[fixture.selection.CodeCommit] = failure
				wantCalls = append(wantCalls, "commit")
			case "finish":
				probe.finishError = failure
				wantCalls = append(wantCalls, "commit", "commit", "delta", "commit", "node", "node", "node")
			}
			view, err := statusCodeQueries(test, config).QueryCodeApplicability(context.Background(), gitEvidenceTestRepo, fixture.selection)
			if !errors.Is(err, wantError) || !reflect.DeepEqual(probe.calls, wantCalls) {
				test.Fatal("error order changed or later readers were accessed", atomic, stage, view, err, probe.calls)
			}
			if stage == "selected missing" {
				if view.Reason != "selected_tree_pending" || view.Paths[0].Reason != view.Reason {
					test.Fatal("missing immutable evidence became unavailable or failure", view)
				}
				assertCodeApplicabilityHash(test, view)
			} else if atomic || stage != "selected failure" && stage != "finish" {
				if !reflect.DeepEqual(view, domain.CodeApplicability{}) {
					test.Fatal("early or atomic failure leaked a response", atomic, stage, view)
				}
			} else if stage == "selected failure" && (view.StateHash != "" || view.Paths == nil || view.Revision != fixture.revision || view.Selection.CodeCommit != fixture.selection.CodeCommit) || stage == "finish" && (view.StateHash == "" || view.Paths[0].State != "applied") {
				test.Fatal("legacy partial result changed", stage, view)
			}
		}
	}
}

func TestCodeQueriesKeepCancellationAndUnavailablePrecedence(test *testing.T) {
	for _, available := range []bool{false, true} {
		fixture := statusCodeFixture()
		probe := newStatusProbe()
		config := statusCodeConfig(probe, fixture)
		if !available {
			config.Trees, config.Deltas = nil, nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		probe.cancel = cancel
		view, err := statusCodeQueries(test, config).QueryCodeApplicability(ctx, gitEvidenceTestRepo, fixture.selection)
		cancel()
		if !reflect.DeepEqual(probe.calls, []string{"scope", "metadata", "revision"}) || view.Revision != fixture.revision {
			test.Fatal("canceled orchestration accessed immutable evidence or lost revision", view, err, probe.calls)
		}
		if available {
			if !errors.Is(err, context.Canceled) || view.StateHash != "" || view.Paths == nil || len(view.Paths) != 0 || view.Relation != "unknown" {
				test.Fatal("cancellation before owner creation changed partial result", view, err)
			}
		} else if err != nil || view.Reason != "git_evidence_unavailable" || view.StateHash == "" {
			test.Fatal("unavailable evidence now checks cancellation that it historically ignored", view, err)
		}
	}
}

func TestCodeQueriesForwardTheExistingStrictTraversalLimit(test *testing.T) {
	for _, strict := range []bool{false, true} {
		fixture := statusCodeFixture()
		selected := fixture.trees.commits[fixture.selection.CodeCommit]
		selected.Parents = []string{fmt.Sprintf("%040x", 100)}
		fixture.trees.commits[selected.Commit] = selected
		for index := 0; index < contextprojection.MaxProjectionSnapshots; index++ {
			commit, parent := fmt.Sprintf("%040x", index+100), fmt.Sprintf("%040x", index+101)
			fixture.trees.commits[commit] = domain.GitCommitTree{Commit: commit, Parents: []string{parent}}
		}
		probe := newStatusProbe()
		config := statusCodeConfig(probe, fixture)
		config.RequireComplete = strict
		view, err := statusCodeQueries(test, config).QueryCodeApplicability(context.Background(), gitEvidenceTestRepo, fixture.selection)
		if strict {
			if !errors.Is(err, domain.ErrMemoryProjectionLimit) || view.StateHash != "" {
				test.Fatal("strict traversal exhaustion was downgraded to missing success", view, err)
			}
		} else if err != nil || view.Relation != "unknown" || view.Paths[0].Reason != "incomplete_ancestry" || view.Reason != "" {
			test.Fatal("legacy traversal uncertainty changed", view, err)
		}
		if len(fixture.trees.commitCalls) != contextprojection.MaxProjectionSnapshots+2 || len(fixture.deltas.calls) != 1 {
			test.Fatal("orchestration duplicated the ancestry owner or changed traversal limit", len(fixture.trees.commitCalls), len(fixture.deltas.calls))
		}
	}
}
