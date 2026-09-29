package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/authcfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func TestNamedSyncDestinationVerifiesIdentityAndIsolatesCredentials(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	var response domain.ContentHash
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repository-connections" {
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(500)
			return
		}
		auth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(domain.RepositoryConnection{RepoID: response})
	}))
	defer server.Close()
	if err := remotecfg.Save(root, remotecfg.Remotes{"origin": "https://origin.test/acme/repo", "mirror": server.URL + "/acme/repo"}); err != nil {
		t.Fatal(err)
	}
	cfg := config{RepoRoot: root, GitDir: filepath.Join(root, ".git"), RemoteToken: "origin-private-token"}
	st, _, git := buildRepositoryAdapters(cfg)
	repo, err := git.CurrentRepo(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	resolve := namedSyncDestination(cfg, st, git)
	if _, err := resolve(context.Background(), root, "missing"); err == nil {
		t.Fatal("unknown remote accepted")
	}
	response = domain.HashContent([]byte("unrelated repository"))
	if _, err := resolve(context.Background(), root, "mirror"); err == nil {
		t.Fatal("different repository accepted")
	}
	if auth != "" {
		t.Fatal("origin token leaked to named server")
	}
	response = repo.ID
	got, err := resolve(context.Background(), root, "mirror")
	if err != nil || got.Sync == nil || got.ApplySelectedPull == nil {
		t.Fatalf("verified destination: %+v %v", got, err)
	}
	if tokenForSyncDestination(cfg, "https://origin.test/api/v1") != cfg.RemoteToken {
		t.Fatal("origin token unavailable")
	}
	if err := authcfg.Save("origin.test", "fixture-saved-origin"); err != nil {
		t.Fatal(err)
	}
	if tokenForSyncDestination(cfg, "http://origin.test/api/v1") != "" {
		t.Fatal("HTTPS token leaked across scheme downgrade")
	}
	if err := authcfg.Save("mirror.test", "fixture-saved-mirror"); err != nil {
		t.Fatal(err)
	}
	if tokenForSyncDestination(cfg, "http://mirror.test/api/v1") != "" || tokenForSyncDestination(cfg, "https://mirror.test/api/v1") != "fixture-saved-mirror" {
		t.Fatal("saved credentials did not respect secure destination transport")
	}
}

func TestSelectedPullApplicationEvidenceRevision(t *testing.T) {
	// Neither repository selectors nor host Git config/templates may install hooks
	// or start maintenance in this real-Git fixture.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") {
			t.Setenv(key, "")
			if err := os.Unsetenv(key); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	for _, tc := range []struct {
		name                      string
		driftMemoryCall           int
		terminal                  string
		contextReads, memoryReads int
	}{
		{"first_memory", 1, "", 5, 5},
		{"memory_revalidation", 2, "", 6, 6},
		{"context_revoked_after_retry", 1, "context", 2, 1},
		{"memory_revoked_after_retry", 1, "memory", 2, 2},
		{"malformed_with_revision_drift", 1, "malformed", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			const branch = "selected/code"
			selectionGit(t, root, "init", "--template=", "-q", "-b", branch)
			selectionGit(t, root, "config", "core.hooksPath", os.DevNull)
			selectionGit(t, root, "config", "gc.auto", "0")
			selectionGit(t, root, "config", "maintenance.auto", "false")
			selectionGit(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "selected pull fixture")
			code := selectionGit(t, root, "rev-parse", "HEAD")
			git := gitctx.NewGitContextAdapter()
			repo, err := git.CurrentRepo(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			store := storage.NewWorktreeFileStore(root, filepath.Join(root, ".git"), branch, code)
			id, err := store.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SessionOriginID: "selected-pull-fixture"}}})
			if err != nil {
				t.Fatal(err)
			}
			snapshot := domain.Snapshot{ID: id, DocHash: id, RepoID: repo.ID, Branch: branch}
			if err := store.PutSnapshot(ctx, snapshot); err != nil {
				t.Fatal(err)
			}
			if err := store.PutWorkingPosition(ctx, domain.WorkingPosition{RepoID: repo.ID, Branch: branch, GitCommit: code, Snapshot: id, SharedTarget: id}); err != nil {
				t.Fatal(err)
			}
			before, err := store.ReadCheckoutState(ctx, repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			selection := domain.ContextSelection{Branch: branch, Position: string(id), CodeCommit: code, Scope: "current"}
			memorySelection := domain.EffectiveMemorySelection{Branch: branch, SnapshotID: id, CodeCommit: code}
			contextHash := domain.HashContent([]byte("unchanged selected context"))
			lineage := domain.HashContent([]byte("unchanged memory lineage"))
			items := []domain.EffectiveMemoryItem{{ID: domain.HashContent([]byte("decision")), SourceSnapshot: id, Kind: "decision", Text: "Keep the original selected code.", State: "retained", Reason: "project_decision"}}
			memoryHash := func(evidence uint64) domain.ContentHash {
				// The real server includes graph/evidence revisions in this hash even
				// when the selected items and lineage do not change.
				return domain.HashContent([]byte(fmt.Sprintf("memory:graph=1:evidence=%d", evidence)))
			}
			var mu sync.Mutex
			contextReads, memoryReads := 0, 0
			evidence := uint64(1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer selected-pull-fixture" {
					t.Errorf("unauthorized or mutating projection request: %s %s", r.Method, r.URL)
					http.Error(w, "denied", http.StatusForbidden)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				var response any
				var wantQuery url.Values
				switch r.URL.Path {
				case "/api/v1/repos/" + repo.ID + "/context-query":
					contextReads++
					wantQuery = url.Values{"branch": {branch}, "position": {string(id)}, "code_commit": {code}, "scope": {"current"}}
					response = domain.ContextQueryView{Version: 1, Branch: branch, Position: id, StateHash: contextHash, Revision: domain.RepositoryRevision{Graph: 1, Evidence: evidence}, Snapshots: []domain.Snapshot{snapshot}, Inclusion: &domain.BranchContext{SnapshotID: id, CodeCommit: code, SnapshotIDs: []domain.ContentHash{id}}}
				case "/api/v1/repos/" + repo.ID + "/effective-memory":
					memoryReads++
					if memoryReads == tc.driftMemoryCall {
						evidence++
					}
					wantQuery = url.Values{"branch": {branch}, "snapshot_id": {string(id)}, "code_commit": {code}, "content": {"prompt"}, "limit": {"50"}}
					page := domain.EffectiveMemoryPage{Content: "prompt", Selection: memorySelection, StateHash: memoryHash(evidence), LineageHash: lineage, Total: len(items), Items: items}
					page.Revision.Graph, page.Revision.Evidence = 1, evidence
					if tc.terminal == "malformed" {
						page.Total++ // An incomplete terminal page must not become retryable.
					}
					response = page
				default:
					t.Errorf("unexpected callback request: %s", r.URL)
					http.NotFound(w, r)
					return
				}
				if !reflect.DeepEqual(r.URL.Query(), wantQuery) {
					t.Errorf("callback changed original selection: got %v, want %v", r.URL.Query(), wantQuery)
					http.Error(w, "wrong selection", http.StatusBadRequest)
					return
				}
				if tc.terminal == "context" && contextReads == 2 || tc.terminal == "memory" && memoryReads == 2 {
					http.Error(w, "membership revoked", http.StatusForbidden)
					return
				}
				if err := json.NewEncoder(w).Encode(response); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			remote := backendclient.NewBackendClient(func() string { return server.URL + "/api/v1" }, func() string { return "selected-pull-fixture" }, domain.TeamIdentity{})
			receipt, applyErr := selectedPullApplication(store, remote, git)(ctx, root)
			mu.Lock()
			gotContextReads, gotMemoryReads := contextReads, memoryReads
			mu.Unlock()
			if gotContextReads != tc.contextReads || gotMemoryReads != tc.memoryReads {
				t.Errorf("incomplete reauthorization or unexpected retry: context=%d memory=%d; want %d/%d (error: %v)", gotContextReads, gotMemoryReads, tc.contextReads, tc.memoryReads, applyErr)
			}
			after, err := store.ReadCheckoutState(ctx, repo.ID)
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Errorf("callback changed checkout/index: before=%+v after=%+v error=%v", before, after, err)
			}
			if selectionGit(t, root, "rev-parse", "HEAD") != code || selectionGit(t, root, "symbolic-ref", "--short", "HEAD") != branch {
				t.Error("callback moved the selected Git code or branch")
			}
			stored, storedErr := store.ReadAppliedPull(ctx, repo.ID, remote.SyncRemoteIdentity())
			if tc.terminal != "" {
				if tc.terminal == "malformed" {
					if !errors.Is(applyErr, domain.ErrHashMismatch) {
						t.Errorf("malformed page lost its integrity error: %v", applyErr)
					}
				} else {
					var denied *backendclient.HTTPError
					if !errors.As(applyErr, &denied) || denied.Status != http.StatusForbidden {
						t.Errorf("revocation lost its HTTP authorization error: %v", applyErr)
					}
				}
				if !reflect.DeepEqual(receipt, outbound.SelectedPullReceipt{}) || !errors.Is(storedErr, domain.ErrNotFound) {
					t.Fatalf("terminal failure published a receipt: returned=%+v stored=%+v error=%v", receipt, stored, storedErr)
				}
				return
			}
			if applyErr != nil {
				t.Fatalf("callback failed on revision-only drift: %v", applyErr)
			}
			plan := receipt.Plan
			if plan.RepoID != repo.ID || plan.Remote != remote.SyncRemoteIdentity() || plan.Selection != selection || !reflect.DeepEqual(plan.Expected, before) || plan.ID == "" || plan.ID != outbound.SelectedPullPlanID(plan) {
				t.Fatalf("receipt did not retain the original selection: %+v", plan)
			}
			if plan.Context.Revision.Evidence != 2 || plan.Context.StateHash != contextHash || !reflect.DeepEqual(plan.Context.Snapshots, []domain.Snapshot{snapshot}) || len(plan.Memory) != 1 {
				t.Fatalf("receipt did not use the reauthorized context: %+v", plan)
			}
			page := plan.Memory[0]
			if page.Revision.Evidence != 2 || page.StateHash != memoryHash(2) || page.StateHash == memoryHash(1) || page.Selection != memorySelection || page.LineageHash != lineage || !reflect.DeepEqual(page.Items, items) {
				t.Fatalf("receipt mixed memory revisions or changed the selected body: %+v", page)
			}
			if storedErr != nil || !reflect.DeepEqual(stored, receipt) || receipt.AppliedAt.IsZero() {
				t.Fatalf("callback receipt was not persisted exactly: %+v %v", stored, storedErr)
			}
		})
	}
}
