package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type selectionHistoryFunc func(context.Context, inbound.HistoryQueryInput) (domain.HistoryQueryResult, error)

func (f selectionHistoryFunc) QueryHistory(ctx context.Context, in inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
	return f(ctx, in)
}

type agentSelectionFixture struct {
	runtime         runtimeAgentPreparer
	cwd, repo, code string
	position        domain.WorkingPosition
	owner           domain.ContentHash
	mu              sync.Mutex
	requests        []domain.EffectiveMemorySelection
	historyRequests []inbound.HistoryQueryInput
	during          func()
}

func selectionGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}

func newAgentSelectionFixture(t *testing.T) *agentSelectionFixture {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	f := &agentSelectionFixture{cwd: t.TempDir()}
	selectionGit(t, f.cwd, "init", "-q", "-b", "main")
	selectionGit(t, f.cwd, "config", "core.hooksPath", "/dev/null")
	selectionGit(t, f.cwd, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "base")
	f.code = selectionGit(t, f.cwd, "rev-parse", "HEAD")
	git := gitctx.NewGitContextAdapter()
	repo, err := git.CurrentRepo(context.Background(), f.cwd)
	if err != nil {
		t.Fatal(err)
	}
	f.repo = repo.ID
	store := storage.NewWorktreeFileStore(f.cwd, filepath.Join(f.cwd, ".git"), "main", f.code)
	put := func(label string) domain.ContentHash {
		id, err := store.PutDoc(context.Background(), domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex}, Events: []domain.Event{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: label}}}}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.PutSnapshot(context.Background(), domain.Snapshot{ID: id, DocHash: id, RepoID: f.repo, Provider: domain.ProviderCodex, Branch: "main"}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	f.owner = put("ancestor memory owner")
	f.position = domain.WorkingPosition{RepoID: f.repo, Branch: "main", GitCommit: f.code, Snapshot: put("selected source"), MemoryHash: domain.HashContent([]byte("old memory")), MemoryPinned: true, Rewound: true}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/effective-memory") {
			t.Errorf("unexpected source read: %s", req.URL.Path)
			http.NotFound(w, req)
			return
		}
		q := req.URL.Query()
		selection := domain.EffectiveMemorySelection{SnapshotID: domain.ContentHash(q.Get("snapshot_id")), MemoryHash: domain.ContentHash(q.Get("memory_hash")), Branch: q.Get("branch"), CodeCommit: q.Get("code_commit")}
		f.mu.Lock()
		f.requests = append(f.requests, selection)
		f.mu.Unlock()
		text := "historical memory at selected time"
		lineage := selection.MemoryHash
		if lineage == "" {
			lineage = domain.HashContent([]byte("new memory"))
			text = "FUTURE mutable attachment"
		}
		page := domain.EffectiveMemoryPage{Content: q.Get("content"), Selection: selection, StateHash: domain.HashContent([]byte("assessment")), LineageHash: lineage, Total: 1, Items: []domain.EffectiveMemoryItem{{ID: domain.HashContent([]byte(text)), SourceSnapshot: selection.SnapshotID, Kind: "decision", Text: text, State: "retained", Reason: "project_decision"}}}
		page.Revision.Graph, page.Revision.Evidence = 1, 1
		if err := json.NewEncoder(w).Encode(page); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	remote := backendclient.NewBackendClient(func() string { return server.URL }, func() string { return "fixture-token" }, domain.TeamIdentity{})
	f.runtime = runtimeAgentPreparer{git: git, store: store, remote: remote}
	f.runtime.history = selectionHistoryFunc(func(_ context.Context, in inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
		f.historyRequests = append(f.historyRequests, in)
		if len(f.historyRequests) == 2 && f.during != nil {
			f.during()
		}
		return domain.HistoryQueryResult{Version: 1, ServerChecked: true, Complete: true, Position: in.Position, StateHash: domain.HashContent([]byte("context")), Revision: &domain.RepositoryRevision{Graph: 1, Evidence: 1}, Selection: domain.HistorySelection{Branch: in.Branch, CodeCommit: f.code}, Snapshots: []domain.Snapshot{{ID: in.Position, DocHash: in.Position, RepoID: f.repo, MemoryHash: domain.HashContent([]byte("new memory"))}, {ID: f.owner, DocHash: f.owner, RepoID: f.repo}}}, nil
	})
	return f
}

func (f *agentSelectionFixture) writePosition(t *testing.T) {
	t.Helper()
	f.position.Selection = nil
	if err := f.runtime.store.PutWorkingPosition(context.Background(), f.position); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeAgentContextPreservesHistoricalPins(t *testing.T) {
	for _, kind := range []string{"selected owner", "ancestor owner", "empty"} {
		t.Run(kind, func(t *testing.T) {
			f := newAgentSelectionFixture(t)
			want := &domain.AgentMemoryPin{SnapshotID: f.position.Snapshot, MemoryHash: f.position.MemoryHash}
			if kind == "ancestor owner" {
				f.position.MemorySource, want.SnapshotID = f.owner, f.owner
			} else if kind == "empty" {
				f.position.MemoryHash = ""
				want = &domain.AgentMemoryPin{}
			}
			f.writePosition(t)
			p, err := f.runtime.PrepareAgentContext(context.Background(), inbound.PrepareAgentContextInput{Cwd: f.cwd, Provider: domain.ProviderCodex})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(p.Content.Selection.MemoryPin, want) || p.Content.Selection.WorktreeStateHash == "" {
				t.Fatal("lost selected pin or worktree fence", p.Content.Selection)
			}
			if err := p.ValidateIdentity(); err != nil {
				t.Fatal(err)
			}
			prompt, _ := p.Prompt()
			if strings.Contains(prompt, "FUTURE") {
				t.Fatal("newer memory imported into historical selection")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if kind == "empty" && len(f.requests) != 0 {
				t.Fatal("empty pin queried current memory")
			}
			if kind != "empty" && len(f.requests) != 2 {
				t.Fatal("pin not reauthorized", f.requests)
			}
			for _, request := range f.requests {
				if request.SnapshotID != want.SnapshotID || request.MemoryHash != want.MemoryHash || request.Branch != "" {
					t.Fatal("wrong cloud identity", request)
				}
			}
		})
	}
}

func TestRuntimeAgentContextExplicitHashDoesNotInheritBranch(t *testing.T) {
	f := newAgentSelectionFixture(t)
	f.writePosition(t)
	p, err := f.runtime.PrepareAgentContext(context.Background(), inbound.PrepareAgentContextInput{Cwd: f.cwd, SnapshotID: f.position.Snapshot, Provider: domain.ProviderCodex})
	if err != nil {
		t.Fatal(err)
	}
	if p.Content.Selection.Branch != "" || p.Content.Selection.MemoryPin == nil {
		t.Fatal("hash borrowed HEAD branch or lost its historical pin", p.Content.Selection)
	}
	for _, req := range f.historyRequests {
		if req.Branch != "" || req.Position != f.position.Snapshot {
			t.Fatal("hash inherited HEAD query", req)
		}
	}
}

func TestRuntimeAgentContextRejectsPinChangeDuringPreparation(t *testing.T) {
	f := newAgentSelectionFixture(t)
	f.writePosition(t)
	f.during = func() {
		f.position.MemoryHash = domain.HashContent([]byte("changed pin"))
		f.writePosition(t)
	}
	if _, err := f.runtime.PrepareAgentContext(context.Background(), inbound.PrepareAgentContextInput{Cwd: f.cwd, Provider: domain.ProviderCodex}); !errors.Is(err, domain.ErrSelectionChanged) {
		t.Fatal("concurrent pin change accepted", err)
	}
}

func TestRuntimeAgentDeliveryValidatesCodeBranchAndPinsAfterWarm(t *testing.T) {
	for _, change := range []string{"pin", "pin owner", "snapshot", "same SHA branch", "code"} {
		t.Run(change, func(t *testing.T) {
			f := newAgentSelectionFixture(t)
			f.writePosition(t)
			p, err := f.runtime.PrepareAgentContext(context.Background(), inbound.PrepareAgentContextInput{Cwd: f.cwd, Provider: domain.ProviderCodex})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.runtime.validateAgentDelivery(context.Background(), f.cwd, p.Content.Selection); err != nil {
				t.Fatal("unchanged delivery rejected", err)
			}
			switch change {
			case "pin":
				f.position.MemoryHash = "" // empty is a different selection, too
				f.writePosition(t)
			case "pin owner":
				f.position.MemorySource = f.owner
				f.writePosition(t)
			case "snapshot":
				f.position.Snapshot = f.owner
				f.writePosition(t)
			case "same SHA branch":
				selectionGit(t, f.cwd, "checkout", "-qb", "other")
			case "code":
				selectionGit(t, f.cwd, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "next")
			}
			if err := f.runtime.validateAgentDelivery(context.Background(), f.cwd, p.Content.Selection); !errors.Is(err, domain.ErrSelectionChanged) {
				t.Fatalf("stale %s delivery accepted: %v", change, err)
			}
		})
	}
}

func TestRuntimeAgentContextOrphanNeverSelectsUnrelatedBranchTip(t *testing.T) {
	f := newAgentSelectionFixture(t)
	f.position.Snapshot, f.position.Orphan = "", true
	f.writePosition(t)
	if _, err := f.runtime.PrepareAgentContext(context.Background(), inbound.PrepareAgentContextInput{Cwd: f.cwd, Provider: domain.ProviderCodex}); !errors.Is(err, domain.ErrAgentContextUnavailable) || len(f.historyRequests) != 0 {
		t.Fatal("orphan silently selected a branch tip", err, f.historyRequests)
	}
}

func TestRuntimeAgentContextPreservesPinAfterNamedCheckout(t *testing.T) {
	for _, kind := range []string{"selected owner", "ancestor owner", "empty"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f := newAgentSelectionFixture(t)
			owner := f.position.Snapshot
			if kind == "ancestor owner" {
				owner = f.owner
			}
			hash, err := f.runtime.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: owner, Summary: "historical memory"})
			if err != nil {
				t.Fatal(err)
			}
			pin := domain.AgentMemoryPin{SnapshotID: owner, MemoryHash: hash}
			if kind == "empty" {
				pin = domain.AgentMemoryPin{}
			}
			f.position.MemoryHash, f.position.MemorySource = pin.MemoryHash, pin.SnapshotID
			f.writePosition(t)
			before, err := f.runtime.store.ReadCheckoutState(ctx, f.repo)
			if err != nil {
				t.Fatal(err)
			}
			err = f.runtime.store.CommitCheckout(ctx, outbound.CheckoutTransition{
				RepoID: f.repo, Expected: before, MemoryPin: &pin, CreateBranch: true,
				Head:   domain.Ref{RepoID: f.repo, Kind: domain.RefHEAD, Name: "HEAD", Symbolic: "restored"},
				Branch: &domain.Ref{RepoID: f.repo, Kind: domain.RefBranch, Name: "restored", Target: f.position.Snapshot},
			})
			if err != nil {
				t.Fatal(err)
			}
			selected, err := f.runtime.store.ReadWorkingPosition(ctx, f.repo)
			if err != nil || !selected.MemoryPinned || !selected.Rewound {
				t.Fatalf("expected a named historical selection: %+v %v", selected, err)
			}
			p, err := f.runtime.PrepareAgentContext(ctx, inbound.PrepareAgentContextInput{Cwd: f.cwd, Provider: domain.ProviderCodex})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(p.Content.Selection.MemoryPin, &pin) {
				t.Fatal("named checkout lost the historical pin on fresh preparation", p.Content.Selection)
			}
			prompt, err := p.Prompt()
			if err != nil || strings.Contains(prompt, "FUTURE") {
				t.Fatal("named checkout imported a newer attachment", prompt, err)
			}
		})
	}
}

func TestRuntimeAgentCurrentSelectionUsesIntegratedServerMemory(t *testing.T) {
	f := newAgentSelectionFixture(t)
	f.position.Rewound = false
	f.writePosition(t)
	p, err := f.runtime.PrepareAgentContext(context.Background(), inbound.PrepareAgentContextInput{Cwd: f.cwd, Provider: domain.ProviderCodex})
	if err != nil {
		t.Fatal(err)
	}
	if p.Content.Selection.MemoryPin != nil || len(p.Content.ProjectMemory) != 1 {
		t.Fatal("ordinary selection suppressed integrated server memory", p.Content)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 2 {
		t.Fatal("server projection was not read and revalidated", f.requests)
	}
	for _, q := range f.requests {
		if q.Branch != "main" || q.SnapshotID != f.position.Snapshot || q.CodeCommit != f.code || q.MemoryHash != "" {
			t.Fatal("ordinary selection bypassed code-scoped branch projection", q)
		}
	}
}
