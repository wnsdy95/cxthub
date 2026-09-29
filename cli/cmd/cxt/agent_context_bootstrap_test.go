package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	delivcli "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/cli"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type emptyBootstrapFixture struct {
	t                *testing.T
	root, home, repo string
	cfg              config
	runtime          runtimeAgentPreparer
	hooks            delivcli.ProviderLaunchHooks
	server           *httptest.Server
	mu               sync.Mutex
	mode             string
	reads            int
}

func newEmptyBootstrapFixture(t *testing.T, unborn bool) *emptyBootstrapFixture {
	t.Helper()
	f := &emptyBootstrapFixture{t: t, root: t.TempDir(), home: t.TempDir()}
	t.Setenv("HOME", f.home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(f.home, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(f.home, "codex"))
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	selectionGit(t, f.root, "init", "-q", "-b", "main")
	selectionGit(t, f.root, "config", "core.hooksPath", "/dev/null")
	if !unborn {
		selectionGit(t, f.root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "base")
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reads++
		mode := f.mode
		f.mu.Unlock()
		if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, "/context-query") || r.URL.RawQuery != "scope=all" || r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Errorf("unexpected bootstrap request: %s %s", r.Method, r.URL)
		}
		switch mode {
		case "403":
			http.Error(w, "denied", 403)
			return
		case "404":
			http.Error(w, "missing", 404)
			return
		case "500":
			http.Error(w, "failed", 500)
			return
		case "malformed":
			_, _ = w.Write([]byte("{"))
			return
		}
		revision := map[string]any{"graph": "0", "pending": "0", "evidence": "9007199254740993"}
		view := map[string]any{"version": 1, "state_hash": domain.HashContent([]byte("empty catalog")), "revision": revision, "snapshots": []any{}, "history": []any{}}
		switch mode {
		case "nonempty":
			view["snapshots"] = []any{map[string]any{"id": domain.HashContent([]byte("unreachable"))}}
		case "history":
			view["history"] = []any{map[string]any{"kind": "birth"}}
		case "graph":
			revision["graph"] = "1"
		case "pending":
			revision["pending"] = "1"
		case "evidence":
			revision["evidence"] = "9007199254740994"
		case "state":
			view["state_hash"] = domain.HashContent([]byte("changed"))
		case "missing-history":
			delete(view, "history")
		case "missing-revision":
			delete(view, "revision")
		case "missing-graph":
			delete(revision, "graph")
		case "bad-hash":
			view["state_hash"] = "invalid"
		case "position":
			view["position"] = domain.HashContent([]byte("position"))
		case "filtered":
			view["branch"] = "main"
		}
		_ = json.NewEncoder(w).Encode(view)
	}))
	t.Cleanup(f.server.Close)
	f.cfg = config{RepoRoot: f.root, RemoteEndpoint: f.server.URL, RemoteToken: "fixture-token"}
	var err error
	f.cfg, err = runtimeConfigAt(context.Background(), f.cfg, f.root)
	if err != nil {
		t.Fatal(err)
	}
	f.runtime, _ = runtimeAgentLoader(f.cfg)
	repo, err := f.runtime.git.CurrentRepo(context.Background(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	f.repo = repo.ID
	if _, err := app.NewInitRepoService(f.runtime.git, f.runtime.store).Init(context.Background(), inbound.InitInput{Cwd: f.root}); err != nil {
		t.Fatal(err)
	}
	f.hooks = providerLaunchHooks(f.cfg)
	return f
}

func (f *emptyBootstrapFixture) setMode(mode string) { f.mu.Lock(); defer f.mu.Unlock(); f.mode = mode }
func (f *emptyBootstrapFixture) readCount() int      { f.mu.Lock(); defer f.mu.Unlock(); return f.reads }
func (f *emptyBootstrapFixture) request(provider domain.ProviderKind) delivcli.ProviderLaunchRequest {
	return delivcli.ProviderLaunchRequest{Cwd: f.root, Intent: delivcli.LaunchIntent{Provider: provider, ProviderArgs: []string{"first task"}}}
}

func TestEmptyBootstrapFirstConversationCommittedAndUnborn(t *testing.T) {
	for _, unborn := range []bool{false, true} {
		for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
			t.Run(string(provider)+map[bool]string{false: "-committed", true: "-unborn"}[unborn], func(t *testing.T) {
				f := newEmptyBootstrapFixture(t, unborn)
				ctx := context.Background()
				before, err := f.runtime.store.ReadCheckoutState(ctx, f.repo)
				if err != nil {
					t.Fatal(err)
				}
				p, err := f.hooks.Prepare(ctx, f.request(provider))
				if err != nil {
					t.Fatal(err)
				}
				if p.Bootstrap == nil || p.Bootstrap.Unborn != unborn || p.Bootstrap.Branch != "main" || p.Bootstrap.Server.RepositoryID != f.repo || p.Bootstrap.Server.Revision.Evidence != 9007199254740993 || p.Capability != "verified_empty_repository" || p.Validate == nil {
					t.Fatalf("missing honest proof: %+v", p)
				}
				if unborn {
					if p.CodeCommit != "" {
						t.Fatalf("fabricated unborn SHA: %q", p.CodeCommit)
					}
				} else if p.CodeCommit != selectionGit(t, f.root, "rev-parse", "HEAD") {
					t.Fatal("wrong actual code")
				}
				if err := p.Validate(ctx); err != nil {
					t.Fatal(err)
				}
				if f.readCount() != 3 {
					t.Fatalf("wanted initial + before materialization + before launch checks: %d", f.readCount())
				}
				// The real receipt writer and package/materializer side effects must
				// remain outside the pristine-worktree test.
				if err := f.hooks.Record(ctx, delivcli.ProviderLaunchReceipt{Version: 1, Provider: provider, Mode: "empty_bootstrap", Bootstrap: p.Bootstrap, PackageHash: p.PackageHash, State: "prepared", Acceptance: "unknown"}); err != nil {
					t.Fatal(err)
				}
				if err := p.Validate(ctx); err != nil {
					t.Fatalf("own receipt/package blocked final validation: %v", err)
				}
				raw, err := os.ReadFile(filepath.Join(f.root, ".cxt", "input-packages", strings.TrimPrefix(string(p.PackageHash), "sha256:")+".json"))
				if err != nil {
					t.Fatal(err)
				}
				if domain.HashContent(raw) != p.PackageHash || !strings.Contains(string(raw), `"kind":"verified_empty_repository"`) || strings.Contains(string(raw), `"snapshot_id"`) {
					t.Fatalf("bootstrap artifact fabricates source or wrong hash: %s", raw)
				}
				after, err := f.runtime.store.ReadCheckoutState(ctx, f.repo)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("bootstrap mutated selection: %+v %v", after, err)
				}
				snaps, err := f.runtime.store.ListSnapshots(ctx, f.repo, "")
				if err != nil || len(snaps) != 0 {
					t.Fatalf("bootstrap fabricated snapshot: %v %v", snaps, err)
				}
			})
		}
	}
}

func TestEmptyBootstrapLocalReadsDoNotCreateOrRecoverFiles(t *testing.T) {
	f := newEmptyBootstrapFixture(t, false)
	tree := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		if err := filepath.WalkDir(filepath.Join(f.root, ".cxt"), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				out[path] = "directory"
				return nil
			}
			raw, err := os.ReadFile(path)
			out[path] = string(raw)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := tree()
	for i := 0; i < 3; i++ {
		if _, ok, err := f.runtime.bootstrapLocal(context.Background(), f.cfg, f.repo); err != nil || !ok {
			t.Fatalf("first-run local state rejected: %v %v", ok, err)
		}
	}
	if after := tree(); !reflect.DeepEqual(before, after) {
		t.Fatal("local read initialized lock/index state or mutated files")
	}
	journal := filepath.Join(f.root, ".cxt", "checkout-transition.json")
	if err := os.WriteFile(journal, []byte("unfinished checkout; do not replay"), 0600); err != nil {
		t.Fatal(err)
	}
	before = tree()
	if _, ok, err := f.runtime.bootstrapLocal(context.Background(), f.cfg, f.repo); err == nil || ok {
		t.Fatalf("journal ignored: %v %v", ok, err)
	}
	if after := tree(); !reflect.DeepEqual(before, after) {
		t.Fatal("eligibility read recovered unfinished journal")
	}
}

func TestEmptyBootstrapRejectsUnverifiedOrNonemptyServer(t *testing.T) {
	for _, mode := range []string{"403", "404", "500", "malformed", "nonempty", "history", "missing-history", "missing-revision", "missing-graph", "bad-hash", "position", "filtered", "network"} {
		t.Run(mode, func(t *testing.T) {
			f := newEmptyBootstrapFixture(t, false)
			f.setMode(mode)
			if mode == "network" {
				f.server.Close()
			}
			p, err := f.hooks.Prepare(context.Background(), f.request(domain.ProviderClaude))
			if err == nil || p.SessionID != "" || p.Bootstrap != nil {
				t.Fatalf("unverified bootstrap accepted: %+v %v", p, err)
			}
			if _, err := os.Stat(filepath.Join(f.root, ".cxt", "input-packages")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed check materialized artifact: %v", err)
			}
		})
	}
}

func TestEmptyBootstrapRechecksEveryServerRevisionAndAuthorization(t *testing.T) {
	for _, mode := range []string{"graph", "pending", "evidence", "state", "nonempty", "history", "403", "404", "network"} {
		t.Run(mode, func(t *testing.T) {
			f := newEmptyBootstrapFixture(t, false)
			p, err := f.hooks.Prepare(context.Background(), f.request(domain.ProviderClaude))
			if err != nil {
				t.Fatal(err)
			}
			f.setMode(mode)
			if mode == "network" {
				f.server.Close()
			}
			if err := p.Validate(context.Background()); err == nil {
				t.Fatal("changed or unauthorized server allowed launch")
			}
		})
	}
}

func TestEmptyBootstrapRechecksActualGitAndWorktree(t *testing.T) {
	for _, mode := range []string{"branch", "commit", "first-commit", "position", "journal", "corrupt-head"} {
		t.Run(mode, func(t *testing.T) {
			f := newEmptyBootstrapFixture(t, mode == "first-commit")
			p, err := f.hooks.Prepare(context.Background(), f.request(domain.ProviderClaude))
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "branch":
				selectionGit(t, f.root, "checkout", "-qb", "other")
			case "commit", "first-commit":
				selectionGit(t, f.root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "later")
			case "position":
				state, err := f.runtime.store.ReadCheckoutState(context.Background(), f.repo)
				if err != nil {
					t.Fatal(err)
				}
				state.Position.MemoryPinned = true
				if err := f.runtime.store.PutWorkingPosition(context.Background(), *state.Position); err != nil {
					t.Fatal(err)
				}
			case "journal":
				if err := os.WriteFile(filepath.Join(f.root, ".cxt", "checkout-transition.json"), []byte("unfinished"), 0600); err != nil {
					t.Fatal(err)
				}
			case "corrupt-head":
				if err := os.WriteFile(filepath.Join(f.root, ".git", "refs", "heads", "main"), []byte(strings.Repeat("f", 40)+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.Validate(context.Background()); err == nil {
				t.Fatal("changed Git/worktree accepted")
			}
		})
	}
}

func TestEmptyBootstrapNeverHandlesExplicitOrPinnedSelection(t *testing.T) {
	for _, mode := range []string{"history", "work-state", "transition", "native", "pin", "ref", "snapshot", "other-worktree-index"} {
		t.Run(mode, func(t *testing.T) {
			f := newEmptyBootstrapFixture(t, false)
			ctx := context.Background()
			req := f.request(domain.ProviderClaude)
			switch mode {
			case "history":
				req.Intent.Pull = true
			case "work-state":
				req.Intent.WorkStatePath = "explicit.json"
			case "transition":
				req.Transition = &delivcli.ProviderLaunchTransition{Branch: "other"}
			case "native":
				req.Intent.ProviderArgs = []string{"--resume", "11111111-1111-4111-8111-111111111111"}
			case "pin":
				state, err := f.runtime.store.ReadCheckoutState(ctx, f.repo)
				if err != nil {
					t.Fatal(err)
				}
				state.Position.MemoryPinned = true
				if err := f.runtime.store.PutWorkingPosition(ctx, *state.Position); err != nil {
					t.Fatal(err)
				}
			case "ref":
				if err := f.runtime.store.PutRef(ctx, domain.Ref{RepoID: f.repo, Kind: domain.RefBranch, Name: "missing", Target: domain.HashContent([]byte("missing source"))}); err != nil {
					t.Fatal(err)
				}
			case "snapshot":
				id, err := f.runtime.store.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{}})
				if err != nil {
					t.Fatal(err)
				}
				if err := f.runtime.store.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: f.repo}); err != nil {
					t.Fatal(err)
				}
			case "other-worktree-index":
				dir := filepath.Join(f.root, ".cxt", "worktrees", strings.Repeat("a", 32))
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte("pinned"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			p, handled, err := f.runtime.prepareEmptyBootstrap(ctx, f.cfg, req)
			if err != nil || handled || p.Bootstrap != nil || f.readCount() != 0 {
				t.Fatalf("non-bootstrap selection handled: %+v handled=%v err=%v reads=%d", p, handled, err, f.readCount())
			}
		})
	}
}
