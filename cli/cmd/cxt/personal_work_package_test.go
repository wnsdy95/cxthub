package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type personalPackageHistory struct{ view domain.HistoryQueryResult }

func (f personalPackageHistory) QueryHistory(context.Context, inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
	return f.view, nil
}

// Exercise the real runtime preparer, authenticated HTTP adapter, import reader,
// and application package consumer together. The selected project snapshot can
// belong to someone else: only explicitly addressed personal sources are read.
func TestPersonalWorkRuntimeProducesOptInPackage(t *testing.T) {
	ctx := context.Background()
	cwd, a, f := personalFixture(t)
	git := gitctx.NewGitContextAdapter()
	repo, err := git.CurrentRepo(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	a.RepositoryID, f.snap.RepoID = repo.ID, repo.ID
	path := writePersonalFixture(t, cwd, a)
	selected := domain.HashContent([]byte("team project position"))
	view := domain.HistoryQueryResult{Version: 1, ServerChecked: true, Complete: true, StateHash: domain.HashContent([]byte("context")), Position: selected, Revision: &domain.RepositoryRevision{Graph: 1, Evidence: 1}, Selection: domain.HistorySelection{Branch: "main", CodeCommit: strings.Repeat("a", 40)}, Snapshots: []domain.Snapshot{{ID: selected, DocHash: selected, RepoID: repo.ID, Author: domain.TeamIdentity{Email: "teammate@example.test"}, SessionID: "team-session"}}}
	sourceReads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer authenticated-token" {
			t.Error("missing authenticated request")
		}
		var body any
		switch {
		case r.URL.Path == "/me":
			body = map[string]string{"id": f.actor, "email": f.email}
		case strings.HasSuffix(r.URL.Path, "/snapshots/"+string(f.snap.ID)):
			sourceReads++
			body = f.snap
		case strings.HasSuffix(r.URL.Path, "/docs/"+string(f.doc.Hash)):
			sourceReads++
			body = f.doc
		case strings.HasSuffix(r.URL.Path, "/effective-memory"):
			page := domain.EffectiveMemoryPage{Content: "prompt", Selection: domain.EffectiveMemorySelection{Branch: "main", SnapshotID: selected, CodeCommit: view.Selection.CodeCommit}, StateHash: domain.HashContent([]byte("memory")), LineageHash: domain.HashContent([]byte("lineage"))}
			page.Revision.Graph = 1
			page.Revision.Evidence = 1
			body = page
		default:
			t.Errorf("unexpected implicit source request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	remote := backendclient.NewBackendClient(func() string { return server.URL }, func() string { return "authenticated-token" }, domain.TeamIdentity{Email: "wrong-git-author@example.test"})
	runtime := runtimeAgentPreparer{git: git, remote: remote, history: personalPackageHistory{view: view}}
	in := inbound.PrepareAgentContextInput{RepoID: repo.ID, Cwd: cwd, Branch: "main", SnapshotID: selected, Provider: domain.ProviderCodex, ArtifactOnly: true, WorkStatePath: path}
	p, err := runtime.PrepareAgentContext(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if p.Content.PersonalWork == nil || !reflect.DeepEqual(*p.Content.PersonalWork, a.State) || sourceReads != 2 {
		t.Fatalf("personal state missing or changed: %+v reads=%d", p.Content.PersonalWork, sourceReads)
	}
	if err := p.ValidateIdentity(); err != nil {
		t.Fatal(err)
	}
	in.WorkStatePath = ""
	p, err = runtime.PrepareAgentContext(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if p.Content.PersonalWork != nil || sourceReads != 2 {
		t.Fatal("unqualified preparation inherited personal state")
	}
}
