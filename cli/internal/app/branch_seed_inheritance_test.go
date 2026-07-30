package app

import (
	"context"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func putBranchSeedSnapshot(
	t *testing.T,
	ctx context.Context,
	store *storage.FileStore,
	repoID, branch string,
	events []domain.Event,
	parents []domain.ContentHash,
	memory *domain.MemoryDigest,
) domain.ContentHash {
	t.Helper()
	docHash, err := store.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{
		Envelope: domain.Envelope{
			CIRVersion:     "1",
			SourceProvider: domain.ProviderCodex,
			GitBranch:      branch,
			Fidelity:       domain.FidelityFull,
		},
		Events: events,
	}})
	if err != nil {
		t.Fatalf("put doc: %v", err)
	}
	var memoryHash domain.ContentHash
	if memory != nil {
		digest := *memory
		digest.SnapshotID = docHash
		memoryHash, err = store.PutMemory(ctx, digest)
		if err != nil {
			t.Fatalf("put memory: %v", err)
		}
	}
	if err := store.PutSnapshot(ctx, domain.Snapshot{
		ID:         docHash,
		RepoID:     repoID,
		Branch:     branch,
		Parents:    parents,
		DocHash:    docHash,
		MemoryHash: memoryHash,
		Provider:   domain.ProviderCodex,
		Fidelity:   domain.FidelityFull,
	}); err != nil {
		t.Fatalf("put snapshot: %v", err)
	}
	return docHash
}

func putBranchSeedRef(
	t *testing.T,
	ctx context.Context,
	store *storage.FileStore,
	repoID, branch string,
	target domain.ContentHash,
) {
	t.Helper()
	if err := store.PutRef(ctx, domain.Ref{
		Kind: domain.RefBranch, Name: branch, RepoID: repoID, Target: target,
	}); err != nil {
		t.Fatalf("put ref: %v", err)
	}
}

func TestBranchSeedFromMainIncludesStoredMemoryAndFullSession(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repo := domain.Repo{ID: "repo-main-seed", DefaultBranch: "main", LocalPath: t.TempDir()}

	parentEvents := []domain.Event{
		seedMessage("user", "main session opening request", 0),
		seedMessage("assistant", "main session opening answer", 1),
	}
	parent := putBranchSeedSnapshot(t, ctx, store, repo.ID, "main", parentEvents, nil, nil)
	headEvents := append(append([]domain.Event{}, parentEvents...),
		seedMessage("user", "main session latest request", 2),
		seedMessage("assistant", "main session latest answer", 3),
	)
	head := putBranchSeedSnapshot(t, ctx, store, repo.ID, "main", headEvents, []domain.ContentHash{parent}, &domain.MemoryDigest{
		Summary:   "MAIN COMPACT MEMORY: preserve architecture and current decisions.",
		KeyFacts:  []string{"main compact fact"},
		OpenTasks: []string{"main compact task"},
		Provider:  domain.ProviderCodex,
	})
	putBranchSeedRef(t, ctx, store, repo.ID, "main", head)

	service := NewBranchSeedService(
		branchSeedGit{repo: repo},
		store,
		stubDistiller{d: domain.MemoryDigest{Summary: "fresh main lineage summary"}},
		nil,
		nil,
	)
	out, err := service.Seed(ctx, inbound.SeedInput{
		Cwd: repo.LocalPath, FromBranch: "main", NewBranch: "feature/main-inheritance", Provider: domain.ProviderCodex,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	seed, err := store.GetDoc(ctx, out.SnapshotID)
	if err != nil {
		t.Fatalf("get seed: %v", err)
	}
	if len(seed.CIR.Events) != len(headEvents)+1 {
		t.Fatalf("seed events = %d, want summary + full main session = %d", len(seed.CIR.Events), len(headEvents)+1)
	}
	summary := seed.CIR.Events[0].Blocks[0].Text
	for _, want := range []string{
		"## Project understanding (main)",
		"MAIN COMPACT MEMORY",
		"main compact fact",
		"main compact task",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("seed summary missing %q: %s", want, summary)
		}
	}
	for i, want := range headEvents {
		got := seed.CIR.Events[i+1]
		if len(got.Blocks) == 0 || got.Blocks[0].Text != want.Blocks[0].Text {
			t.Fatalf("main conversation event %d = %+v, want %+v", i, got, want)
		}
	}
}

func TestBranchSeedFromMainDistillsMemoryWhenHeadHasNoStoredDigest(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repo := domain.Repo{ID: "repo-main-fallback", DefaultBranch: "main", LocalPath: t.TempDir()}
	head := putBranchSeedSnapshot(t, ctx, store, repo.ID, "main", []domain.Event{
		seedMessage("user", "fallback main conversation", 0),
	}, nil, nil)
	putBranchSeedRef(t, ctx, store, repo.ID, "main", head)

	service := NewBranchSeedService(
		branchSeedGit{repo: repo},
		store,
		stubDistiller{d: domain.MemoryDigest{Summary: "DISTILLED MAIN COMPACT MEMORY"}},
		nil,
		nil,
	)
	out, err := service.Seed(ctx, inbound.SeedInput{
		Cwd: repo.LocalPath, FromBranch: "main", NewBranch: "feature/fallback", Provider: domain.ProviderCodex,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	seed, err := store.GetDoc(ctx, out.SnapshotID)
	if err != nil {
		t.Fatalf("get seed: %v", err)
	}
	summary := seed.CIR.Events[0].Blocks[0].Text
	if !strings.Contains(summary, "## Project understanding (main)") ||
		!strings.Contains(summary, "DISTILLED MAIN COMPACT MEMORY") {
		t.Fatalf("distilled main memory was not inherited explicitly: %s", summary)
	}
}

func TestBranchSeedFromOtherLineageIncludesMainMemoryAndLineageSession(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repo := domain.Repo{ID: "repo-other-lineage", DefaultBranch: "main", LocalPath: t.TempDir()}

	mainHead := putBranchSeedSnapshot(t, ctx, store, repo.ID, "main", []domain.Event{
		seedMessage("user", "main conversation should not replace feature conversation", 0),
	}, nil, &domain.MemoryDigest{
		Summary:  "MAIN PROJECT MEMORY FOR EVERY NEW BRANCH",
		Provider: domain.ProviderCodex,
	})
	putBranchSeedRef(t, ctx, store, repo.ID, "main", mainHead)

	featureEvents := []domain.Event{
		seedMessage("user", "feature lineage opening request", 0),
		seedMessage("assistant", "feature lineage current answer", 1),
	}
	featureHead := putBranchSeedSnapshot(t, ctx, store, repo.ID, "feature/source", featureEvents, nil, nil)
	putBranchSeedRef(t, ctx, store, repo.ID, "feature/source", featureHead)

	service := NewBranchSeedService(
		branchSeedGit{repo: repo},
		store,
		stubDistiller{d: domain.MemoryDigest{Summary: "feature lineage digest"}},
		nil,
		nil,
	)
	out, err := service.Seed(ctx, inbound.SeedInput{
		Cwd: repo.LocalPath, FromBranch: "feature/source", NewBranch: "feature/child", Provider: domain.ProviderCodex,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	seed, err := store.GetDoc(ctx, out.SnapshotID)
	if err != nil {
		t.Fatalf("get seed: %v", err)
	}
	if !strings.Contains(seed.CIR.Events[0].Blocks[0].Text, "MAIN PROJECT MEMORY FOR EVERY NEW BRANCH") {
		t.Fatal("non-main branch seed lost main project memory")
	}
	if len(seed.CIR.Events) != len(featureEvents)+1 {
		t.Fatalf("seed events = %d, want summary + feature session = %d", len(seed.CIR.Events), len(featureEvents)+1)
	}
	for i, want := range featureEvents {
		if got := seed.CIR.Events[i+1].Blocks[0].Text; got != want.Blocks[0].Text {
			t.Fatalf("feature conversation event %d = %q, want %q", i, got, want.Blocks[0].Text)
		}
	}
}

func TestBranchSeedUsesStoredMainMemoryWhenMainDocIsNotLocal(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repo := domain.Repo{ID: "repo-partial-main", DefaultBranch: "main", LocalPath: t.TempDir()}

	mainID := domain.HashContent([]byte("partial-main-snapshot"))
	mainMemoryHash, err := store.PutMemory(ctx, domain.MemoryDigest{
		SnapshotID: mainID,
		Summary:    "STORED MAIN MEMORY SURVIVES PARTIAL LOCAL DOCS",
		Provider:   domain.ProviderCodex,
	})
	if err != nil {
		t.Fatalf("put main memory: %v", err)
	}
	if err := store.PutSnapshot(ctx, domain.Snapshot{
		ID: mainID, RepoID: repo.ID, Branch: "main", DocHash: mainID, MemoryHash: mainMemoryHash,
	}); err != nil {
		t.Fatalf("put main snapshot: %v", err)
	}
	putBranchSeedRef(t, ctx, store, repo.ID, "main", mainID)

	featureHead := putBranchSeedSnapshot(t, ctx, store, repo.ID, "feature/source", []domain.Event{
		seedMessage("user", "feature conversation remains available", 0),
	}, nil, nil)
	putBranchSeedRef(t, ctx, store, repo.ID, "feature/source", featureHead)

	service := NewBranchSeedService(
		branchSeedGit{repo: repo},
		store,
		stubDistiller{d: domain.MemoryDigest{Summary: "feature digest"}},
		nil,
		nil,
	)
	out, err := service.Seed(ctx, inbound.SeedInput{
		Cwd: repo.LocalPath, FromBranch: "feature/source", NewBranch: "feature/partial-main", Provider: domain.ProviderCodex,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	seed, err := store.GetDoc(ctx, out.SnapshotID)
	if err != nil {
		t.Fatalf("get seed: %v", err)
	}
	if !strings.Contains(seed.CIR.Events[0].Blocks[0].Text, "STORED MAIN MEMORY SURVIVES PARTIAL LOCAL DOCS") {
		t.Fatal("stored main memory was discarded because the main doc was unavailable")
	}
}

func TestBranchSeedMainMemoryAndConversationStayWithinBudget(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repo := domain.Repo{ID: "repo-main-bounded", DefaultBranch: "main", LocalPath: t.TempDir()}

	events := make([]domain.Event, 0, 122)
	for i := 0; i < 60; i++ {
		events = append(events,
			seedMessage("user", strings.Repeat("older main request ", 300), i*2),
			seedMessage("assistant", strings.Repeat("older main answer ", 300), i*2+1),
		)
	}
	events = append(events,
		seedMessage("user", "LATEST MAIN USER REQUEST MUST SURVIVE", len(events)),
		seedMessage("assistant", "latest main answer", len(events)+1),
	)
	head := putBranchSeedSnapshot(t, ctx, store, repo.ID, "main", events, nil, &domain.MemoryDigest{
		Summary:  "MAIN COMPACT MEMORY START\n" + strings.Repeat("project memory ", 12000),
		Provider: domain.ProviderCodex,
	})
	putBranchSeedRef(t, ctx, store, repo.ID, "main", head)

	service := NewBranchSeedService(
		branchSeedGit{repo: repo},
		store,
		stubDistiller{d: domain.MemoryDigest{Summary: "bounded lineage summary"}},
		nil,
		nil,
	)
	out, err := service.Seed(ctx, inbound.SeedInput{
		Cwd: repo.LocalPath, FromBranch: "main", NewBranch: "feature/bounded", Provider: domain.ProviderCodex,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	seed, err := store.GetDoc(ctx, out.SnapshotID)
	if err != nil {
		t.Fatalf("get seed: %v", err)
	}
	if got := eventsJSONBytes(seed.CIR.Events); got > seedBudgetBytes {
		t.Fatalf("seed size = %d, want <= %d", got, seedBudgetBytes)
	}
	if !strings.Contains(seed.CIR.Events[0].Blocks[0].Text, "MAIN COMPACT MEMORY START") {
		t.Fatal("bounded seed lost main compact memory")
	}
	foundLatest := false
	for _, event := range seed.CIR.Events {
		if len(event.Blocks) > 0 && event.Blocks[0].Text == "LATEST MAIN USER REQUEST MUST SURVIVE" {
			foundLatest = true
			break
		}
	}
	if !foundLatest {
		t.Fatal("bounded seed lost latest main user request")
	}
}
