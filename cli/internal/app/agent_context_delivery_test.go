package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/memory"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type agentPackageFixture struct {
	calls int
	err   error
	seen  inbound.PrepareAgentContextInput
}

func (f *agentPackageFixture) PrepareAgentContext(ctx context.Context, in inbound.PrepareAgentContextInput) (domain.AgentContextPackage, error) {
	f.calls++
	f.seen = in
	provider := in.Provider
	if provider == "" {
		provider = domain.ProviderCodex
	}
	p := domain.AgentContextPackage{Version: 1, Provider: provider, Policy: domain.MemoryInputPolicy(), Delivery: "prepared", Content: domain.AgentContextContent{Notice: "bounded B memory fixture", Selection: domain.AgentContextSelection{RepositoryID: in.RepoID, Branch: in.Branch, SnapshotID: in.SnapshotID, CodeCommit: strings.Repeat("a", 40)}}}
	if in.LatestMain {
		p.Content.Selection = latestMainSelection(in.RepoID, in.Branch)
	}
	p.ID, _ = p.Digest()
	return p, f.err
}

func latestMainSelection(repo, branch string) domain.AgentContextSelection {
	return domain.AgentContextSelection{RepositoryID: repo, Branch: "main", SnapshotID: agentHash("latest main"), CodeCommit: strings.Repeat("b", 40), SourcePolicy: domain.AgentSourceLatestMain, WorktreeStateHash: agentHash("working position"), WorkingPosition: &domain.AgentWorkingPosition{Branch: branch, CodeCommit: strings.Repeat("a", 40)}}
}
func (*agentPackageFixture) ValidateAgentContextDelivery(context.Context, string, domain.AgentContextSelection) error {
	return nil
}

type agentMaterializerFixture struct {
	calls  int
	raw    []byte
	err    error
	during func()
}

func (f *agentMaterializerFixture) Provider() domain.ProviderKind { return domain.ProviderCodex }
func (f *agentMaterializerFixture) Materialize(ctx context.Context, raw []byte, cwd string) (string, string, error) {
	f.calls++
	f.raw = append([]byte{}, raw...)
	if f.during != nil {
		f.during()
	}
	return "/fixture/session.jsonl", "codex resume " + domain.NewSessionID(), f.err
}

func TestAgentContextDefaultLoadMaterializesMemoryWithoutRawReplay(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	id := putBranchSeedSnapshot(t, ctx, store, "repo", "main", []domain.Event{agentMessage("user", "PRIVATE RAW TAIL", 0)}, nil, &domain.MemoryDigest{Summary: "project memory"})
	p := &agentPackageFixture{}
	mat := &agentMaterializerFixture{}
	load := NewLoadSessionService(store, map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderCodex: codec.NewCodexCodec()}, map[domain.ProviderKind]outbound.SessionMaterializer{domain.ProviderCodex: mat}, nil, nil, nil).WithAgentContext(p).WithAgentCodePosition(agentCodeFixture{})
	out, err := load.Load(ctx, inbound.LoadInput{Ref: string(id), Cwd: t.TempDir(), TargetProvider: domain.ProviderCodex})
	if err != nil {
		t.Fatal(err)
	}
	if out.Fidelity != domain.FidelityMemory || out.ResumeCmd == "" || mat.calls != 1 || p.calls != 1 {
		t.Fatal(out, mat.calls, p.calls)
	}
	if strings.Contains(string(mat.raw), "PRIVATE RAW TAIL") || !strings.Contains(string(mat.raw), "bounded B memory fixture") {
		t.Fatal("new session is not B-only")
	}
	p.err = errors.New("server refused")
	if _, err = load.Load(ctx, inbound.LoadInput{Ref: string(id), TargetProvider: domain.ProviderCodex}); err == nil {
		t.Fatal("silently fell back")
	}
	if mat.calls != 1 {
		t.Fatal("materialized after package failure")
	}
}

func TestAgentContextSeedKeepsOriginalMemoryAndParents(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repo := domain.Repo{ID: "repo", LocalPath: t.TempDir(), DefaultBranch: "main"}
	id := putBranchSeedSnapshot(t, ctx, store, repo.ID, "main", []domain.Event{agentMessage("user", "RAW SHARED TAIL", 0)}, nil, &domain.MemoryDigest{Summary: "full stored source memory"})
	putBranchSeedRef(t, ctx, store, repo.ID, "main", id)
	seed := NewBranchSeedService(agentSeedGit{branchSeedGit{repo: repo}}, store, memory.NewRuleDistiller(), nil, nil, nil).WithAgentContext(&agentPackageFixture{})
	out, err := seed.Seed(ctx, inbound.SeedInput{Cwd: repo.LocalPath, FromBranch: "main", NewBranch: "feature/b", Provider: domain.ProviderCodex, SkipMaterialize: true})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.GetSnapshot(ctx, out.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Parents) != 1 || snap.Parents[0] != id {
		t.Fatal("original source not retained")
	}
	doc, _ := store.GetDoc(ctx, snap.DocHash)
	if len(doc.CIR.Events) != 1 || strings.Contains(doc.CIR.Events[0].Blocks[0].Text, "RAW SHARED TAIL") {
		t.Fatal("seed injected raw tail")
	}
	digest, _ := store.GetMemory(ctx, snap.MemoryHash)
	if !strings.Contains(digest.Summary, "full stored source memory") {
		t.Fatal("bounded B destroyed archival memory")
	}
	original, _ := store.GetSnapshot(ctx, id)
	if len(original.Parents) != 0 {
		t.Fatal("changed source parents")
	}
}

func TestAgentContextSeedFailureDoesNotPublishActiveBranch(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repo := domain.Repo{ID: "repo", LocalPath: t.TempDir(), DefaultBranch: "main"}
	id := putBranchSeedSnapshot(t, ctx, store, repo.ID, "main", []domain.Event{agentMessage("user", "tail", 0)}, nil, &domain.MemoryDigest{Summary: "memory"})
	putBranchSeedRef(t, ctx, store, repo.ID, "main", id)
	mat := &agentMaterializerFixture{err: errors.New("disk full")}
	seed := NewBranchSeedService(agentSeedGit{branchSeedGit{repo: repo}}, store, memory.NewRuleDistiller(), map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderCodex: codec.NewCodexCodec()}, map[domain.ProviderKind]outbound.SessionMaterializer{domain.ProviderCodex: mat}, nil).WithAgentContext(&agentPackageFixture{})
	if _, err := seed.Seed(ctx, inbound.SeedInput{Cwd: repo.LocalPath, FromBranch: "main", NewBranch: "feature/failed", Provider: domain.ProviderCodex}); err == nil {
		t.Fatal("accepted failed materialization")
	}
	if _, err := store.GetRef(ctx, repo.ID, domain.RefBranch, "feature/failed"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("active branch survived failed preparation", err)
	}
}

func TestAgentContextExplicitLegacyReplayPreservesNativeOpaque(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	seedClaudeSnapshot(t, store)
	prepared := &agentPackageFixture{}
	load := newLoadSvc(store).WithAgentContext(prepared)
	out, err := load.Load(ctx, inbound.LoadInput{Ref: "main", TargetProvider: domain.ProviderClaude, Mode: domain.FidelityFull, Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.calls != 0 || out.Fidelity != domain.FidelityFull {
		t.Fatal("legacy replay turned into B/history injection")
	}
	raw, err := os.ReadFile(out.WrittenPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "SIG1") {
		t.Fatal("opaque native state was removed")
	}
}

func TestAgentContextArtifactCannotUseNativeMaterializer(t *testing.T) {
	prepared := &agentPackageFixture{}
	load := NewLoadSessionService(nil, nil, nil, nil, nil, nil).WithAgentContext(prepared)
	if _, err := load.LoadAgentContext(context.Background(), inbound.PrepareAgentContextInput{ArtifactOnly: true}); !errors.Is(err, domain.ErrProviderCapabilityUnknown) {
		t.Fatal(err)
	}
	if prepared.calls != 0 {
		t.Fatal("artifact attempted native preparation")
	}
}

type agentCodeFixture struct{ changed bool }

func (f agentCodeFixture) CurrentCommit(context.Context, string) (string, error) {
	if f.changed {
		return strings.Repeat("b", 40), nil
	}
	return strings.Repeat("a", 40), nil
}

type agentSeedGit struct{ branchSeedGit }

func (agentSeedGit) CurrentCommit(context.Context, string) (string, error) {
	return strings.Repeat("a", 40), nil
}

func TestAgentContextDeliveryReceiptMatchesOnePreparation(t *testing.T) {
	prepared := &agentPackageFixture{}
	mat := &agentMaterializerFixture{}
	load := NewLoadSessionService(nil, map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderCodex: codec.NewCodexCodec()}, map[domain.ProviderKind]outbound.SessionMaterializer{domain.ProviderCodex: mat}, nil, nil, nil).WithAgentContext(prepared).WithAgentCodePosition(agentCodeFixture{})
	input := inbound.PrepareAgentContextInput{RepoID: "repo", Provider: domain.ProviderCodex, Cwd: t.TempDir()}
	receipt, out, err := load.PrepareAgentDelivery(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.calls != 1 || mat.calls != 1 || out.WrittenPath == "" {
		t.Fatal("preparation was repeated or not delivered")
	}
	cir, err := codec.NewCodexCodec().Decode(context.Background(), mat.raw)
	if err != nil {
		t.Fatal(err)
	}
	prompt, _ := receipt.Prompt()
	found := false
	for _, event := range cir.Events {
		for _, block := range event.Blocks {
			if block.Text == prompt {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("materialized bytes differ from the returned package")
	}
	load.WithAgentCodePosition(agentCodeFixture{changed: true})
	if _, _, err = load.PrepareAgentDelivery(context.Background(), input); !errors.Is(err, domain.ErrSelectionChanged) {
		t.Fatal(err)
	}
	if mat.calls != 1 {
		t.Fatal("code changed but session was still materialized")
	}
}

func TestAgentContextSeedConcurrentSelectionPreserved(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repo := domain.Repo{ID: "repo", LocalPath: t.TempDir(), DefaultBranch: "main"}
	id := putBranchSeedSnapshot(t, ctx, store, repo.ID, "main", []domain.Event{agentMessage("user", "tail", 0)}, nil, &domain.MemoryDigest{Summary: "memory"})
	putBranchSeedRef(t, ctx, store, repo.ID, "main", id)
	mat := &agentMaterializerFixture{during: func() {
		if err := store.PutRef(ctx, domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", RepoID: repo.ID, Target: id}); err != nil {
			t.Fatal(err)
		}
	}}
	seed := NewBranchSeedService(agentSeedGit{branchSeedGit{repo: repo}}, store, memory.NewRuleDistiller(), map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderCodex: codec.NewCodexCodec()}, map[domain.ProviderKind]outbound.SessionMaterializer{domain.ProviderCodex: mat}, nil).WithAgentContext(&agentPackageFixture{})
	if _, err := seed.Seed(ctx, inbound.SeedInput{Cwd: repo.LocalPath, FromBranch: "main", NewBranch: "feature/race", Provider: domain.ProviderCodex}); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatal(err)
	}
	if _, err := store.GetRef(ctx, repo.ID, domain.RefBranch, "feature/race"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("published a branch despite stale selection", err)
	}
	head, _ := store.GetRef(ctx, repo.ID, domain.RefHEAD, "HEAD")
	if head.Target != id || head.Symbolic != "" {
		t.Fatal("replaced concurrent worker selection")
	}
}
