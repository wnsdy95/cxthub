package app

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/memory"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
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
			CIRVersion:     domain.CIRVersionForEvents(events),
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

func TestBranchSeedSummarizesTrimmedPostCompactionConversation(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repo := domain.Repo{ID: "repo-post-compaction-bridge", DefaultBranch: "main", LocalPath: t.TempDir()}
	const (
		compactBase = "PROVIDER COMPACT BASELINE MUST SURVIVE"
		middle      = "MIDDLE POST-COMPACTION DECISION MUST SURVIVE"
		latest      = "LATEST POST-COMPACTION REQUEST MUST SURVIVE"
	)
	events := []domain.Event{{
		Kind: domain.EventMessage, Role: "user", Seq: 0, CompactSummary: true,
		Blocks: []domain.ContentBlock{{Type: "text", Text: compactBase}},
	}}
	for i := 0; i < 20; i++ {
		user := fmt.Sprintf("post-compaction request %02d %s", i, strings.Repeat("u", 16<<10))
		if i == 10 {
			user = middle + " " + strings.Repeat("m", 16<<10)
		}
		if i == 19 {
			user = latest + " " + strings.Repeat("l", 16<<10)
		}
		events = append(events,
			seedMessage("user", user, len(events)),
			seedMessage("assistant", fmt.Sprintf("post-compaction answer %02d %s", i, strings.Repeat("a", 16<<10)), len(events)+1),
		)
	}

	head := putBranchSeedSnapshot(t, ctx, store, repo.ID, "main", events, nil, nil)
	putBranchSeedRef(t, ctx, store, repo.ID, "main", head)
	service := NewBranchSeedService(
		branchSeedGit{repo: repo}, store, memory.NewRuleDistiller(), nil, nil, nil,
	)
	out, err := service.Seed(ctx, inbound.SeedInput{
		Cwd: repo.LocalPath, FromBranch: "main", NewBranch: "feature/post-compaction-bridge", Provider: domain.ProviderCodex,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	seed, err := store.GetDoc(ctx, out.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	totalBudget, _ := seedBudgets(domain.ProviderCodex)
	if got := eventsJSONBytes(seed.CIR.Events); got > totalBudget {
		t.Fatalf("seed events = %d bytes, want <= %d", got, totalBudget)
	}
	var prompt strings.Builder
	middleIsVerbatim := false
	for i, event := range seed.CIR.Events {
		for _, block := range event.Blocks {
			prompt.WriteString(block.Text)
			if i > 0 && strings.Contains(block.Text, middle) {
				middleIsVerbatim = true
			}
		}
	}
	if middleIsVerbatim {
		t.Fatal("fixture did not trim the middle post-compaction decision")
	}
	if !strings.Contains(seed.CIR.Events[0].Blocks[0].Text, middle) {
		t.Fatal("trimmed post-compaction decision was not bridged into the summary layer")
	}
	if strings.Contains(seed.CIR.Events[0].Blocks[0].Text, latest) {
		t.Fatal("verbatim latest request was redundantly copied into the bridge summary")
	}
	for _, want := range []string{compactBase, middle, latest} {
		if !strings.Contains(prompt.String(), want) {
			t.Fatalf("branch seed prompt lost %q", want)
		}
	}
	if count := strings.Count(prompt.String(), latest); count != 1 {
		t.Fatalf("latest request appears %d times, want one verbatim copy", count)
	}
	seedSnapshot, err := store.GetSnapshot(ctx, out.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	seedMemory, err := store.GetMemory(ctx, seedSnapshot.MemoryHash)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seedMemory.Summary, middle) {
		t.Fatal("branch seed memory lost the trimmed post-compaction decision")
	}
}

func TestBranchSeedDoesNotDuplicateCurrentConversationInSummary(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repo := domain.Repo{ID: "repo-seed-no-current-dup", DefaultBranch: "main", LocalPath: t.TempDir()}
	events := []domain.Event{
		{Kind: domain.EventMessage, Role: "user", Seq: 0, CompactSummary: true, Blocks: []domain.ContentBlock{{Type: "text", Text: "BRANCH PROVIDER BASELINE"}}},
		seedMessage("user", "BRANCH CURRENT DECISION ONCE", 1),
		seedMessage("assistant", "BRANCH CURRENT RESULT ONCE", 2),
	}
	fullMemory, err := memory.NewRuleDistiller().Distill(context.Background(), domain.CIRDocument{
		Envelope: domain.Envelope{SourceProvider: domain.ProviderCodex}, Events: events,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	head := putBranchSeedSnapshot(t, ctx, store, repo.ID, "main", events, nil, &fullMemory)
	putBranchSeedRef(t, ctx, store, repo.ID, "main", head)
	service := NewBranchSeedService(branchSeedGit{repo: repo}, store, memory.NewRuleDistiller(), nil, nil, nil)

	out, err := service.Seed(ctx, inbound.SeedInput{
		Cwd: repo.LocalPath, FromBranch: "main", NewBranch: "feature/no-current-dup", Provider: domain.ProviderCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := store.GetDoc(ctx, out.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	var prompt strings.Builder
	for _, event := range doc.CIR.Events {
		for _, block := range event.Blocks {
			prompt.WriteString(block.Text)
		}
	}
	for _, marker := range []string{"BRANCH CURRENT DECISION ONCE", "BRANCH CURRENT RESULT ONCE"} {
		if count := strings.Count(prompt.String(), marker); count != 1 {
			t.Fatalf("branch prompt contains %q %d times, want one verbatim copy:\n%s", marker, count, prompt.String())
		}
	}
	if strings.Contains(prompt.String(), "[cxt conversation delta v1]") {
		t.Fatalf("private memory marker leaked into branch prompt:\n%s", prompt.String())
	}
	seedSnap, err := store.GetSnapshot(ctx, out.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	seedMemory, err := store.GetMemory(ctx, seedSnap.MemoryHash)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"BRANCH CURRENT DECISION ONCE", "BRANCH CURRENT RESULT ONCE"} {
		if !strings.Contains(seedMemory.Summary, marker) {
			t.Fatalf("attached seed memory lost %q:\n%s", marker, seedMemory.Summary)
		}
	}
	if strings.Contains(seedMemory.Summary, "[cxt conversation delta v1]") {
		t.Fatalf("private memory marker leaked into rendered seed memory:\n%s", seedMemory.Summary)
	}
}

type failBridgeDistiller struct{ calls int }

func (d *failBridgeDistiller) Distill(context.Context, domain.CIRDocument, *domain.NativeMemory) (domain.MemoryDigest, error) {
	d.calls++
	if d.calls > 1 {
		return domain.MemoryDigest{}, fmt.Errorf("bridge distillation failed")
	}
	return domain.MemoryDigest{Summary: "fresh branch memory", Provider: domain.ProviderCodex}, nil
}

func TestBranchSeedBridgeFailureDoesNotCreateLossyBranch(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repo := domain.Repo{ID: "repo-bridge-fail-closed", DefaultBranch: "main", LocalPath: t.TempDir()}
	var events []domain.Event
	for i := 0; i < 20; i++ {
		events = append(events,
			seedMessage("user", strings.Repeat("large request ", 1400), len(events)),
			seedMessage("assistant", strings.Repeat("large answer ", 1400), len(events)+1),
		)
	}
	head := putBranchSeedSnapshot(t, ctx, store, repo.ID, "main", events, nil, &domain.MemoryDigest{
		Summary: "stored project memory", Provider: domain.ProviderCodex,
	})
	putBranchSeedRef(t, ctx, store, repo.ID, "main", head)
	distiller := &failBridgeDistiller{}
	service := NewBranchSeedService(branchSeedGit{repo: repo}, store, distiller, nil, nil, nil)
	_, err := service.Seed(ctx, inbound.SeedInput{
		Cwd: repo.LocalPath, FromBranch: "main", NewBranch: "feature/bridge-failure", Provider: domain.ProviderCodex,
	})
	if err == nil || !strings.Contains(err.Error(), "bridge distillation failed") {
		t.Fatalf("seed error=%v", err)
	}
	if _, err := store.GetRef(ctx, repo.ID, domain.RefBranch, "feature/bridge-failure"); err == nil {
		t.Fatal("lossy branch ref was created after bridge distillation failed")
	}
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
	seedSnapshot, err := store.GetSnapshot(ctx, out.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	seedMemory, err := store.GetMemory(ctx, seedSnapshot.MemoryHash)
	if err != nil {
		t.Fatal(err)
	}
	walker := memoryProjectionWalker{fingerprinter: newMemoryProjectionFingerprinter(ctx, store)}
	if !walker.memoryDigestCoversLineage(seedMemory, seedSnapshot) {
		t.Fatal("seed coverage omitted the departure parent's attached memory pointer")
	}
}

func TestBranchSeedProjectsClaudeCwdNativeOnlyFromPrompt(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repo := domain.Repo{ID: "repo-claude-native-seed", DefaultBranch: "main", LocalPath: t.TempDir()}
	const nativeBaseline = "CLAUDE AUTO-LOADED PROJECT MEMORY"
	head := putBranchSeedSnapshot(t, ctx, store, repo.ID, "main", []domain.Event{
		seedMessage("user", "RAW DEPARTURE REQUEST", 0),
		seedMessage("assistant", "RAW DEPARTURE RESULT", 1),
	}, nil, &domain.MemoryDigest{Summary: nativeBaseline})
	putBranchSeedRef(t, ctx, store, repo.ID, "main", head)

	service := NewBranchSeedService(
		branchSeedGit{repo: repo}, store,
		stubDistiller{d: domain.MemoryDigest{Summary: "FRESH LINEAGE SUMMARY"}}, nil, nil,
		map[domain.ProviderKind]outbound.MemorySource{
			domain.ProviderClaude: stubMemSource{text: nativeBaseline},
		},
	)
	out, err := service.Seed(ctx, inbound.SeedInput{
		Cwd: repo.LocalPath, FromBranch: "main", NewBranch: "feature/claude-native", Provider: domain.ProviderClaude,
	})
	if err != nil {
		t.Fatal(err)
	}
	seedDoc, err := store.GetDoc(ctx, out.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	var prompt strings.Builder
	for _, event := range seedDoc.CIR.Events {
		for _, block := range event.Blocks {
			prompt.WriteString(block.Text)
		}
	}
	if strings.Contains(prompt.String(), nativeBaseline) {
		t.Fatalf("branch prompt duplicated Claude cwd memory:\n%s", prompt.String())
	}
	for _, want := range []string{"FRESH LINEAGE SUMMARY", "RAW DEPARTURE REQUEST", "RAW DEPARTURE RESULT"} {
		if !strings.Contains(prompt.String(), want) {
			t.Fatalf("branch prompt lost %q:\n%s", want, prompt.String())
		}
	}
	seedSnap, err := store.GetSnapshot(ctx, out.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	attached, err := store.GetMemory(ctx, seedSnap.MemoryHash)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(attached.Summary, nativeBaseline) {
		t.Fatalf("prompt-only projection mutated portable seed memory:\n%s", attached.Summary)
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

	seedSnapshot, err := store.GetSnapshot(ctx, out.SnapshotID)
	if err != nil {
		t.Fatalf("get seed snapshot: %v", err)
	}
	seedMemory, err := store.GetMemory(ctx, seedSnapshot.MemoryHash)
	if err != nil {
		t.Fatalf("get seed memory: %v", err)
	}
	if seedMemory.GraftCoverage == nil || !seedMemory.GraftCoverage.ProjectionComplete {
		t.Fatal("cross-lineage seed did not record memory coverage")
	}
	pinned := map[domain.ContentHash]bool{}
	for _, source := range seedMemory.GraftCoverage.PinnedSources {
		pinned[source] = true
	}
	if !pinned[mainHead] || !pinned[featureHead] || len(pinned) != 2 {
		t.Fatalf("seed pinned sources = %v, want main %s and memoryless departure %s", seedMemory.GraftCoverage.PinnedSources, mainHead, featureHead)
	}
	walker := memoryProjectionWalker{fingerprinter: newMemoryProjectionFingerprinter(ctx, store)}
	if !walker.memoryDigestCoversLineage(seedMemory, seedSnapshot) {
		t.Fatal("synthetic seed coverage does not match its stored parent lineage")
	}

	// The seed coverage is now stale: a graft appears below its natural parent
	// after the seed memory was attached. Reprojection must add that late branch
	// without dropping the explicitly imported main memory or the departure
	// snapshot's on-the-fly distillation.
	lateGraft := putBranchSeedSnapshot(t, ctx, store, repo.ID, "feature/late", []domain.Event{
		seedMessage("user", "late graft conversation", 0),
	}, nil, &domain.MemoryDigest{Summary: "LATE GRAFT MEMORY", Provider: domain.ProviderCodex})
	featureSnapshot, err := store.GetSnapshot(ctx, featureHead)
	if err != nil {
		t.Fatalf("get feature snapshot: %v", err)
	}
	featureSnapshot.Grafted = true
	featureSnapshot.GraftSeq = 1
	featureSnapshot.GraftParents = []domain.ContentHash{lateGraft}
	if err := store.PutSnapshot(ctx, featureSnapshot); err != nil {
		t.Fatalf("add late graft: %v", err)
	}
	seedSnapshot, err = store.GetSnapshot(ctx, out.SnapshotID)
	if err != nil {
		t.Fatalf("reload seed snapshot: %v", err)
	}
	projected, ok := snapshotMemoryProjection(ctx, store, seedSnapshot)
	if !ok {
		t.Fatal("stale cross-lineage seed memory was not reprojected")
	}
	for _, want := range []string{
		"MAIN PROJECT MEMORY FOR EVERY NEW BRANCH",
		"feature lineage digest",
		"LATE GRAFT MEMORY",
	} {
		if count := strings.Count(projected.Summary, want); count != 1 {
			t.Fatalf("reprojected seed summary contains %q %d times, want exactly once: %q", want, count, projected.Summary)
		}
	}

	// Memorizing the seed snapshot itself replaces its fresh own contribution,
	// but must not replace the imported memory owned only by that seed digest.
	memorizer := NewMemorizeService(
		branchSeedGit{repo: repo}, nil, nil, nil,
		stubDistiller{d: domain.MemoryDigest{Summary: "RE-MEMORIZED SEED OWN MEMORY"}}, store,
	)
	memorized, err := memorizer.Memorize(ctx, inbound.MemorizeInput{
		Cwd: repo.LocalPath, Provider: domain.ProviderCodex, Ref: string(out.SnapshotID),
	})
	if err != nil {
		t.Fatalf("re-memorize seed: %v", err)
	}
	rememorized, err := store.GetMemory(ctx, memorized.MemoryHash)
	if err != nil {
		t.Fatalf("get re-memorized seed: %v", err)
	}
	for _, want := range []string{
		"MAIN PROJECT MEMORY FOR EVERY NEW BRANCH",
		"feature lineage digest",
		"LATE GRAFT MEMORY",
		"RE-MEMORIZED SEED OWN MEMORY",
	} {
		if count := strings.Count(rememorized.Summary, want); count != 1 {
			t.Fatalf("re-memorized seed contains %q %d times, want exactly once: %q", want, count, rememorized.Summary)
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
	fullMainSummary := "MAIN COMPACT MEMORY START\n" + strings.Repeat("project memory ", 12000) + "\nMAIN COMPACT MEMORY NEWEST TAIL"

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
		Summary:  fullMainSummary,
		Provider: domain.ProviderCodex,
	})
	putBranchSeedRef(t, ctx, store, repo.ID, "main", head)

	service := NewBranchSeedService(
		branchSeedGit{repo: repo},
		store,
		stubDistiller{d: domain.MemoryDigest{Summary: "bounded lineage summary"}},
		nil,
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
	// The prompt keeps the newest tail of an oversized summary (#31); the full
	// digest including the head stays reachable via the seed MemoryHash below.
	if !strings.Contains(seed.CIR.Events[0].Blocks[0].Text, "MAIN COMPACT MEMORY NEWEST TAIL") {
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
	seedSnapshot, err := store.GetSnapshot(ctx, out.SnapshotID)
	if err != nil {
		t.Fatalf("get seed snapshot: %v", err)
	}
	if seedSnapshot.MemoryHash == "" {
		t.Fatal("seed snapshot lost the full inherited memory digest")
	}
	seedMemory, err := store.GetMemory(ctx, seedSnapshot.MemoryHash)
	if err != nil {
		t.Fatalf("get seed memory: %v", err)
	}
	if seedMemory.SnapshotID != out.SnapshotID {
		t.Fatalf("seed memory snapshot = %s, want %s", seedMemory.SnapshotID, out.SnapshotID)
	}
	if seedMemory.GraftCoverage == nil || seedMemory.GraftCoverage.ProjectionVersion != domain.MemoryProjectionVersion || !seedMemory.GraftCoverage.ProjectionComplete ||
		seedMemory.GraftCoverage.LineageFingerprint == "" || seedMemory.GraftCoverage.GraftSeq != 0 || len(seedMemory.GraftCoverage.GraftParents) != 0 {
		t.Fatalf("seed memory graft coverage = %+v, want explicit empty register", seedMemory.GraftCoverage)
	}
	if !strings.Contains(seedMemory.Summary, fullMainSummary) {
		t.Fatalf("attached seed memory lost full main digest: got %d bytes, want at least %d", len(seedMemory.Summary), len(fullMainSummary))
	}
	if !strings.Contains(seedMemory.Summary, "bounded lineage summary") {
		t.Fatal("attached seed memory lost the fresh departure-lineage digest")
	}
}

// TestRenderSeedTextKeepsBulletsAndNewestSummaryTail fixes the seed-prompt
// budgeting contract (#31): MergeDigests places the oldest summary generations
// first, so whole-text prefix truncation used to keep only stale summary head
// and drop key facts, open tasks, the Layer2 digest, and the trailer whenever
// the main summary outgrew the digest budget.
func TestRenderSeedTextKeepsBulletsAndNewestSummaryTail(t *testing.T) {
	mainMem := &domain.MemoryDigest{
		Summary:   "OLDEST-GENERATION-HEAD\n" + strings.Repeat("m", 4*seedDigestBudgetBytes) + "\nNEWEST-MAIN-TAIL",
		KeyFacts:  []string{"main fact alpha beta"},
		OpenTasks: []string{"main task gamma delta"},
	}
	branchMem := domain.MemoryDigest{
		Summary:   "OLD-BRANCH-HEAD\n" + strings.Repeat("b", 2*seedDigestBudgetBytes) + "\nNEWEST-BRANCH-TAIL",
		KeyFacts:  []string{"branch fact epsilon zeta"},
		OpenTasks: []string{"branch task eta theta"},
	}
	out := renderSeedText("main", "fix/x", mainMem, branchMem, seedDigestBudgetBytes)
	if len(out) > seedDigestBudgetBytes {
		t.Fatalf("seed text = %d bytes, want <= %d", len(out), seedDigestBudgetBytes)
	}
	for _, want := range []string{
		"## Project understanding (main)",
		"- main fact alpha beta",
		"- ☐ main task gamma delta",
		"NEWEST-MAIN-TAIL",
		"## Work summary of this lineage (main)",
		"- branch fact epsilon zeta",
		"- ☐ branch task eta theta",
		"NEWEST-BRANCH-TAIL",
		"## Recent context (verbatim)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("seed text missing %q", want)
		}
	}
	for _, unwanted := range []string{"OLDEST-GENERATION-HEAD", "OLD-BRANCH-HEAD"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("seed text kept stale summary head %q instead of the newest tail", unwanted)
		}
	}
}

// TestBranchSeedCarriedMemoryIsBounded (#33): a seed born under an oversized
// main memory stores a bounded carried digest (newest tail), while the
// ancestor's full memory object stays untouched and reachable via the parent.
func TestBranchSeedCarriedMemoryIsBounded(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repo := domain.Repo{ID: "repo-mem-cap", DefaultBranch: "main", LocalPath: t.TempDir()}
	oversized := "OLDEST-GENERATION-HEAD\n" + strings.Repeat("project memory ", 2*memoryCarryBudgetBytes/15) + "\nNEWEST-GENERATION-TAIL"
	head := putBranchSeedSnapshot(t, ctx, store, repo.ID, "main",
		[]domain.Event{seedMessage("user", "latest main request", 0)}, nil,
		&domain.MemoryDigest{Summary: oversized, Provider: domain.ProviderCodex})
	putBranchSeedRef(t, ctx, store, repo.ID, "main", head)

	service := NewBranchSeedService(
		branchSeedGit{repo: repo},
		store,
		stubDistiller{d: domain.MemoryDigest{Summary: "fresh lineage summary"}},
		nil,
		nil,
		nil,
	)
	out, err := service.Seed(ctx, inbound.SeedInput{
		Cwd: repo.LocalPath, FromBranch: "main", NewBranch: "feature/cap", Provider: domain.ProviderCodex,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	seedSnap, err := store.GetSnapshot(ctx, out.SnapshotID)
	if err != nil || seedSnap.MemoryHash == "" {
		t.Fatalf("seed snapshot missing memory: %+v err=%v", seedSnap, err)
	}
	carried, err := store.GetMemory(ctx, seedSnap.MemoryHash)
	if err != nil {
		t.Fatal(err)
	}
	if len(carried.Summary) > memoryCarryBudgetBytes+len("\n\nfresh lineage summary") {
		t.Fatalf("carried seed memory = %d bytes, inherited carry exceeded %d", len(carried.Summary), memoryCarryBudgetBytes)
	}
	if !strings.Contains(carried.Summary, "NEWEST-GENERATION-TAIL") || !strings.Contains(carried.Summary, "fresh lineage summary") {
		t.Fatal("carried seed memory lost the newest generations")
	}
	mainSnap, err := store.GetSnapshot(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	full, err := store.GetMemory(ctx, mainSnap.MemoryHash)
	if err != nil {
		t.Fatal(err)
	}
	if full.Summary != oversized {
		t.Fatal("ancestor full memory object changed — history must stay recoverable")
	}
}

func TestRepeatedBranchSeedsDoNotCarryPriorSyntheticPrompts(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repo := domain.Repo{ID: "repo-seed-replay-clean", DefaultBranch: "main", LocalPath: t.TempDir()}
	head := putBranchSeedSnapshot(t, ctx, store, repo.ID, "main", []domain.Event{
		seedMessage("user", "[cxt seed] Branch-switch context: old → main\nold synthetic prompt", 0),
		seedMessage("developer", "old runtime instructions", 1),
		seedMessage("user", "<environment_context>\n<cwd>/old</cwd>\n</environment_context>", 2),
		seedMessage("user", "real request", 3),
		seedMessage("assistant", "real answer", 4),
	}, nil, &domain.MemoryDigest{Summary: "clean project memory", Provider: domain.ProviderCodex})
	putBranchSeedRef(t, ctx, store, repo.ID, "main", head)

	service := NewBranchSeedService(
		branchSeedGit{repo: repo}, store,
		stubDistiller{d: domain.MemoryDigest{Summary: "fresh lineage memory"}}, nil, nil, nil,
	)
	first, err := service.Seed(ctx, inbound.SeedInput{
		Cwd: repo.LocalPath, FromBranch: "main", NewBranch: "feature/one", Provider: domain.ProviderCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Seed(ctx, inbound.SeedInput{
		Cwd: repo.LocalPath, FromBranch: "feature/one", NewBranch: "feature/two", Provider: domain.ProviderCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []domain.ContentHash{first.SnapshotID, second.SnapshotID} {
		doc, err := store.GetDoc(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		seedCount := 0
		for _, ev := range doc.CIR.Events {
			if ev.Kind != domain.EventMessage || len(ev.Blocks) == 0 {
				continue
			}
			text := ev.Blocks[0].Text
			if strings.HasPrefix(text, "[cxt seed] Branch-switch context:") {
				seedCount++
			}
			for _, forbidden := range []string{"old synthetic prompt", "old runtime instructions", "<environment_context>"} {
				if strings.Contains(text, forbidden) {
					t.Fatalf("seed %s carried replay control %q: %q", id, forbidden, text)
				}
			}
		}
		if seedCount != 1 {
			t.Fatalf("seed %s contains %d synthetic seed prompts, want its own one", id, seedCount)
		}
	}
}
