package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/memory"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/session"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

const claudeFixtureWithSig = `{"type":"user","cwd":"/p","sessionId":"s","gitBranch":"main","timestamp":"2026-06-30T00:00:00Z","message":{"role":"user","content":"do it"}}
{"type":"assistant","cwd":"/p","sessionId":"s","gitBranch":"main","timestamp":"2026-06-30T00:00:01Z","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"thinking","thinking":"planning","signature":"SIG1"},{"type":"text","text":"working"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`

// seedClaudeSnapshot decodes the claude session and stores the snapshot + main branch ref.
func seedClaudeSnapshot(t *testing.T, store *storage.FileStore) domain.ContentHash {
	t.Helper()
	ctx := context.Background()
	cir, err := codec.NewClaudeCodec().Decode(ctx, []byte(claudeFixtureWithSig))
	if err != nil {
		t.Fatalf("decode seed: %v", err)
	}
	h, err := store.PutDoc(ctx, domain.SessionDoc{CIR: cir})
	if err != nil {
		t.Fatalf("PutDoc: %v", err)
	}
	if err := store.PutSnapshot(ctx, domain.Snapshot{ID: h, Branch: "main", DocHash: h, Provider: domain.ProviderClaude, Fidelity: cir.Envelope.Fidelity}); err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}
	store.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: h})
	store.PutRef(ctx, domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", Symbolic: "main"})
	return h
}

func newLoadSvc(store *storage.FileStore) *LoadSessionService {
	codecs := map[domain.ProviderKind]outbound.ProviderCodec{
		domain.ProviderClaude: codec.NewClaudeCodec(),
		domain.ProviderCodex:  codec.NewCodexCodec(),
	}
	mats := map[domain.ProviderKind]outbound.SessionMaterializer{
		domain.ProviderClaude: session.NewClaudeMaterializer(),
		domain.ProviderCodex:  session.NewCodexMaterializer(),
	}
	srcs := map[domain.ProviderKind]outbound.MemorySource{
		domain.ProviderClaude: memory.NewClaudeMemorySource(),
		domain.ProviderCodex:  memory.NewCodexMemorySource(),
	}
	sinks := map[domain.ProviderKind]outbound.MemorySink{
		domain.ProviderClaude: memory.NewClaudeMemorySink(),
		domain.ProviderCodex:  memory.NewCodexMemorySink(),
	}
	return NewLoadSessionService(store, codecs, mats, srcs, memory.NewRuleDistiller(), sinks)
}

// Same provider full load: claude → claude, signature preserved.
func TestLoadFullSameProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	seedClaudeSnapshot(t, store)
	cwd := t.TempDir()

	out, err := newLoadSvc(store).Load(ctx, inbound.LoadInput{Ref: "main", TargetProvider: domain.ProviderClaude, Mode: domain.FidelityFull, Cwd: cwd})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out.Fidelity != domain.FidelityFull {
		t.Fatalf("expected full, got %s", out.Fidelity)
	}
	if !strings.HasPrefix(out.ResumeCmd, "claude --resume ") {
		t.Fatalf("resume cmd: %q", out.ResumeCmd)
	}
	data, err := os.ReadFile(out.WrittenPath)
	if err != nil {
		t.Fatalf("written file: %v", err)
	}
	if !strings.Contains(string(data), "SIG1") {
		t.Fatalf("full same-provider must preserve signature")
	}
	restored, err := codec.NewClaudeCodec().Decode(ctx, data)
	if err != nil {
		t.Fatalf("decode restored Claude session: %v", err)
	}
	if restored.Envelope.Cwd != cwd {
		t.Fatalf("restored Claude session cwd = %q, want target %q", restored.Envelope.Cwd, cwd)
	}
}

func TestLoadFullUntrimmedCarriesPortableMemoryWithoutDuplicatingConversation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	snapshotID := seedClaudeSnapshot(t, store)
	const baseline = "PORTABLE PROVIDER BASELINE"
	memoryHash, err := store.PutMemory(ctx, domain.MemoryDigest{
		SnapshotID: snapshotID,
		Summary: domain.AppendExtractiveConversationDelta(baseline,
			domain.RenderExtractiveFallbackSummary([]string{"do it"}, []string{"working"})),
		Provider: domain.ProviderClaude,
	})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.GetSnapshot(ctx, snapshotID)
	if err != nil {
		t.Fatal(err)
	}
	snap.MemoryHash = memoryHash
	if err := store.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}

	svc := NewLoadSessionService(
		store,
		map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderClaude: codec.NewClaudeCodec()},
		map[domain.ProviderKind]outbound.SessionMaterializer{domain.ProviderClaude: session.NewClaudeMaterializer()},
		map[domain.ProviderKind]outbound.MemorySource{
			domain.ProviderClaude: stubMemSource{text: "DIFFERENT TARGET MEMORY"},
		},
		memory.NewRuleDistiller(), nil,
	)
	out, err := svc.Load(ctx, inbound.LoadInput{
		Ref: "main", TargetProvider: domain.ProviderClaude, Mode: domain.FidelityFull, Cwd: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.TrimmedEvents != 0 {
		t.Fatalf("small full replay trimmed %d events, want none", out.TrimmedEvents)
	}
	raw, err := os.ReadFile(out.WrittenPath)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := codec.NewClaudeCodec().Decode(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	var prompt strings.Builder
	seedCount := 0
	for _, ev := range restored.EffectiveContext().Events {
		if isSeedSummaryEvent(ev) {
			seedCount++
		}
		for _, block := range ev.Blocks {
			prompt.WriteString(block.Text)
		}
	}
	if got := strings.Count(prompt.String(), baseline); got != 1 {
		t.Fatalf("portable baseline count=%d, want one:\n%s", got, prompt.String())
	}
	if seedCount != 1 {
		t.Fatalf("portable seed count=%d, want one", seedCount)
	}
	for _, current := range []string{"do it", "working"} {
		if got := strings.Count(prompt.String(), current); got != 1 {
			t.Fatalf("raw conversation count(%q)=%d, want one:\n%s", current, got, prompt.String())
		}
	}
}

func TestLoadFullUntrimmedDoesNotDuplicateAncestorConversationDelta(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	cir, err := codec.NewClaudeCodec().Decode(ctx, []byte(claudeFixtureWithSig))
	if err != nil {
		t.Fatal(err)
	}
	currentID, err := store.PutDoc(ctx, domain.SessionDoc{CIR: cir})
	if err != nil {
		t.Fatal(err)
	}
	ancestorID := domain.HashContent([]byte("ancestor-conversation-already-in-current-replay"))
	const baseline = "ANCESTOR PORTABLE BASELINE"
	ancestorMemory, err := store.PutMemory(ctx, domain.MemoryDigest{
		SnapshotID: ancestorID,
		Summary: domain.AppendExtractiveConversationDelta(baseline,
			domain.RenderExtractiveFallbackSummary([]string{"do it"}, []string{"working"})),
		Provider: domain.ProviderClaude,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutSnapshot(ctx, domain.Snapshot{
		ID: ancestorID, DocHash: ancestorID, Branch: "main", Provider: domain.ProviderClaude,
		Fidelity: domain.FidelityFull, MemoryHash: ancestorMemory,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutSnapshot(ctx, domain.Snapshot{
		ID: currentID, DocHash: currentID, Branch: "main", Provider: domain.ProviderClaude,
		Fidelity: domain.FidelityFull, Parents: []domain.ContentHash{ancestorID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: currentID}); err != nil {
		t.Fatal(err)
	}

	out, err := newLoadSvc(store).Load(ctx, inbound.LoadInput{
		Ref: "main", TargetProvider: domain.ProviderClaude, Mode: domain.FidelityFull, Cwd: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out.WrittenPath)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := codec.NewClaudeCodec().Decode(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	var prompt strings.Builder
	for _, event := range restored.EffectiveContext().Events {
		for _, block := range event.Blocks {
			prompt.WriteString(block.Text)
		}
	}
	for _, want := range []string{baseline, "do it", "working"} {
		if got := strings.Count(prompt.String(), want); got != 1 {
			t.Fatalf("ancestor replay count(%q)=%d, want one:\n%s", want, got, prompt.String())
		}
	}
	if strings.Contains(prompt.String(), "[cxt conversation delta v1]") {
		t.Fatalf("private memory marker leaked into full replay:\n%s", prompt.String())
	}
}

func TestLoadFullUntrimmedDoesNotDuplicateReplayedProviderCompaction(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	const compactBaseline = "PROVIDER COMPACTION ALREADY IN RAW REPLAY"
	cir := domain.CIRDocument{
		Envelope: domain.Envelope{
			CIRVersion: domain.CIRVersionV1, SourceProvider: domain.ProviderClaude,
			Cwd: "/source", GitBranch: "main", SessionOriginID: "compact-source",
		},
		Events: []domain.Event{
			{Kind: domain.EventMessage, Role: "user", CompactSummary: true, Blocks: []domain.ContentBlock{{Type: "text", Text: compactBaseline}}},
			{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "POST COMPACTION REQUEST"}}},
			{Kind: domain.EventMessage, Role: "assistant", Blocks: []domain.ContentBlock{{Type: "text", Text: "POST COMPACTION RESULT"}}},
		},
	}
	snapshotID, err := store.PutDoc(ctx, domain.SessionDoc{CIR: cir})
	if err != nil {
		t.Fatal(err)
	}
	digest, err := memory.NewRuleDistiller().Distill(ctx, cir, nil)
	if err != nil {
		t.Fatal(err)
	}
	digest.SnapshotID = snapshotID
	digest = domain.MergeDigests(domain.MemoryDigest{}, digest)
	memoryHash, err := store.PutMemory(ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutSnapshot(ctx, domain.Snapshot{
		ID: snapshotID, DocHash: snapshotID, Branch: "main", Provider: domain.ProviderClaude,
		Fidelity: domain.FidelityFull, MemoryHash: memoryHash,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: snapshotID}); err != nil {
		t.Fatal(err)
	}

	out, err := newLoadSvc(store).Load(ctx, inbound.LoadInput{
		Ref: "main", TargetProvider: domain.ProviderClaude, Mode: domain.FidelityFull, Cwd: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out.WrittenPath)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := codec.NewClaudeCodec().Decode(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	var prompt strings.Builder
	seedCount := 0
	for _, ev := range restored.EffectiveContext().Events {
		if isSeedSummaryEvent(ev) {
			seedCount++
		}
		for _, block := range ev.Blocks {
			prompt.WriteString(block.Text)
		}
	}
	if got := strings.Count(prompt.String(), compactBaseline); got != 1 {
		t.Fatalf("replayed provider compaction count=%d, want one:\n%s", got, prompt.String())
	}
	if seedCount != 0 {
		t.Fatalf("provider compaction alone created %d redundant cxt seeds", seedCount)
	}
}

// Cross-provider full request: claude → codex, reconstructed fallback + claude signature masked.
func TestLoadCrossProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	seedClaudeSnapshot(t, store)
	cwd := t.TempDir()

	out, err := newLoadSvc(store).Load(ctx, inbound.LoadInput{Ref: "main", TargetProvider: domain.ProviderCodex, Mode: domain.FidelityFull, Cwd: cwd})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out.Fidelity != domain.FidelityReconstructed {
		t.Fatalf("cross-provider must downgrade to reconstructed, got %s", out.Fidelity)
	}
	if !strings.HasPrefix(out.ResumeCmd, "codex resume ") {
		t.Fatalf("resume cmd: %q", out.ResumeCmd)
	}
	data, err := os.ReadFile(out.WrittenPath)
	if err != nil {
		t.Fatalf("written file: %v", err)
	}
	if strings.Contains(string(data), "SIG1") {
		t.Fatalf("cross-encode leaked claude signature into codex output:\n%s", data)
	}
	if !strings.Contains(string(data), "session_meta") {
		t.Fatalf("codex output should be a rollout (session_meta)")
	}
	restored, err := codec.NewCodexCodec().Decode(ctx, data)
	if err != nil {
		t.Fatalf("decode restored Codex session: %v", err)
	}
	if restored.Envelope.Cwd != cwd {
		t.Fatalf("restored Codex session cwd = %q, want target %q", restored.Envelope.Cwd, cwd)
	}
}

func TestLoadCrossProviderUntrimmedCarriesPortableMemory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	snapshotID := seedClaudeSnapshot(t, store)
	const baseline = "CROSS PROVIDER PORTABLE BASELINE"
	memoryHash, err := store.PutMemory(ctx, domain.MemoryDigest{
		SnapshotID: snapshotID,
		Summary: domain.AppendExtractiveConversationDelta(baseline,
			domain.RenderExtractiveFallbackSummary([]string{"do it"}, []string{"working"})),
		Provider: domain.ProviderClaude,
	})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.GetSnapshot(ctx, snapshotID)
	if err != nil {
		t.Fatal(err)
	}
	snap.MemoryHash = memoryHash
	if err := store.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}

	out, err := newLoadSvc(store).Load(ctx, inbound.LoadInput{
		Ref: "main", TargetProvider: domain.ProviderCodex, Mode: domain.FidelityFull, Cwd: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Fidelity != domain.FidelityReconstructed || out.TrimmedEvents != 0 {
		t.Fatalf("cross-provider output=%+v, want untrimmed reconstructed", out)
	}
	raw, err := os.ReadFile(out.WrittenPath)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := codec.NewCodexCodec().Decode(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	var prompt strings.Builder
	for _, event := range restored.EffectiveContext().Events {
		for _, block := range event.Blocks {
			prompt.WriteString(block.Text)
		}
	}
	for _, want := range []string{baseline, "do it", "working"} {
		if got := strings.Count(prompt.String(), want); got != 1 {
			t.Fatalf("cross-provider count(%q)=%d, want one:\n%s", want, got, prompt.String())
		}
	}
}

func TestLoadPortableMemoryTriggersBoundedTurnTrimOnlyWhenCombinedSeedExceedsBudget(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	events := make([]domain.Event, 0, 20)
	for i := 0; i < 10; i++ {
		events = append(events,
			domain.Event{Kind: domain.EventMessage, Seq: len(events), Role: "user", Blocks: []domain.ContentBlock{{
				Type: "text", Text: fmt.Sprintf("BUDGET REQUEST %02d ", i) + strings.Repeat("u", 8<<10),
			}}},
			domain.Event{Kind: domain.EventMessage, Seq: len(events) + 1, Role: "assistant", Blocks: []domain.ContentBlock{{
				Type: "text", Text: fmt.Sprintf("BUDGET RESULT %02d ", i) + strings.Repeat("a", 8<<10),
			}}},
		)
	}
	cir := domain.CIRDocument{
		Envelope: domain.Envelope{
			CIRVersion: domain.CIRVersionV1, SourceProvider: domain.ProviderClaude,
			Cwd: "/source", GitBranch: "main", SessionOriginID: "portable-budget",
		},
		Events: events,
	}
	snapshotID, err := store.PutDoc(ctx, domain.SessionDoc{CIR: cir})
	if err != nil {
		t.Fatal(err)
	}
	const baselineMarker = "LARGE PORTABLE BASELINE END"
	baseline := strings.Repeat("m", 40<<10) + " " + baselineMarker
	memoryHash, err := store.PutMemory(ctx, domain.MemoryDigest{
		SnapshotID: snapshotID,
		Summary: domain.AppendExtractiveConversationDelta(baseline,
			domain.RenderExtractiveFallbackSummary([]string{"BUDGET REQUEST 09"}, []string{"BUDGET RESULT 09"})),
		Provider: domain.ProviderClaude,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutSnapshot(ctx, domain.Snapshot{
		ID: snapshotID, DocHash: snapshotID, Branch: "main", Provider: domain.ProviderClaude,
		Fidelity: domain.FidelityFull, MemoryHash: memoryHash,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: snapshotID}); err != nil {
		t.Fatal(err)
	}

	out, err := newLoadSvc(store).Load(ctx, inbound.LoadInput{
		Ref: "main", TargetProvider: domain.ProviderClaude, Mode: domain.FidelityFull, Cwd: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.TrimmedEvents == 0 {
		t.Fatal("combined replay plus portable memory exceeded the budget without trimming")
	}
	raw, err := os.ReadFile(out.WrittenPath)
	if err != nil {
		t.Fatal(err)
	}
	totalBudget, _ := seedBudgets(domain.ProviderClaude)
	if len(raw) > totalBudget+(32<<10) {
		t.Fatalf("materialized seed=%d bytes, want bounded near %d", len(raw), totalBudget)
	}
	if got := strings.Count(string(raw), baselineMarker); got != 1 {
		t.Fatalf("bounded replay baseline count=%d, want one", got)
	}
	if !strings.Contains(string(raw), "BUDGET REQUEST 09") || !strings.Contains(string(raw), "BUDGET RESULT 09") {
		t.Fatal("bounded replay lost the newest complete turn")
	}
}

func TestLoadTrimDistillationFailureCreatesNoSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	var events []domain.Event
	for i := 0; i < 20; i++ {
		events = append(events,
			domain.Event{Kind: domain.EventMessage, Seq: len(events), Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "OMITTED LOAD REQUEST " + strings.Repeat("u", 16<<10)}}},
			domain.Event{Kind: domain.EventMessage, Seq: len(events) + 1, Role: "assistant", Blocks: []domain.ContentBlock{{Type: "text", Text: "OMITTED LOAD RESULT " + strings.Repeat("a", 16<<10)}}},
		)
	}
	cir := domain.CIRDocument{
		Envelope: domain.Envelope{CIRVersion: domain.CIRVersionV1, SourceProvider: domain.ProviderClaude, Cwd: "/source", GitBranch: "main", SessionOriginID: "load-distill-failure"},
		Events:   events,
	}
	hash, err := store.PutDoc(ctx, domain.SessionDoc{CIR: cir})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutSnapshot(ctx, domain.Snapshot{ID: hash, DocHash: hash, Branch: "main", Provider: domain.ProviderClaude, Fidelity: domain.FidelityFull}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: hash}); err != nil {
		t.Fatal(err)
	}
	svc := NewLoadSessionService(
		store,
		map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderClaude: codec.NewClaudeCodec()},
		map[domain.ProviderKind]outbound.SessionMaterializer{domain.ProviderClaude: session.NewClaudeMaterializer()},
		nil, failingDistiller{}, nil,
	)
	cwd := t.TempDir()

	out, err := svc.Load(ctx, inbound.LoadInput{Ref: "main", TargetProvider: domain.ProviderClaude, Mode: domain.FidelityFull, Cwd: cwd})
	if err == nil || !strings.Contains(err.Error(), "distill omitted context") {
		t.Fatalf("Load output=%+v error=%v, want fail-closed distillation error", out, err)
	}
	var materialized []string
	if walkErr := filepath.Walk(filepath.Join(home, ".claude"), func(path string, info os.FileInfo, walkErr error) error {
		if walkErr == nil && info != nil && !info.IsDir() && strings.HasSuffix(path, ".jsonl") {
			materialized = append(materialized, path)
		}
		return nil
	}); walkErr != nil && !os.IsNotExist(walkErr) {
		t.Fatal(walkErr)
	}
	if len(materialized) != 0 {
		t.Fatalf("failed distillation materialized provider sessions: %v", materialized)
	}
}

func TestLoadPinnedReplacementDistillationFailureCreatesNoSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	replacement := []domain.Event{
		{Kind: domain.EventMessage, Seq: 0, Role: "developer", Blocks: []domain.ContentBlock{{Type: "text", Text: "PINNED RUNTIME CONTRACT"}}},
		{Kind: domain.EventCompaction, Seq: 1, Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted_content", Blob: "PINNED-STATE"}},
	}
	events := []domain.Event{
		{Kind: domain.EventMessage, Seq: 0, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "archival context"}}},
		{Kind: domain.EventCompaction, Seq: 1, Replacement: replacement, ReplacementComplete: true},
	}
	for i := 0; i < 20; i++ {
		events = append(events,
			domain.Event{Kind: domain.EventMessage, Seq: len(events), Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "CODEX OMITTED REQUEST " + strings.Repeat("u", 16<<10)}}},
			domain.Event{Kind: domain.EventMessage, Seq: len(events) + 1, Role: "assistant", Blocks: []domain.ContentBlock{{Type: "text", Text: "CODEX OMITTED RESULT " + strings.Repeat("a", 16<<10)}}},
		)
	}
	cir := domain.CIRDocument{
		Envelope: domain.Envelope{CIRVersion: domain.CIRVersionV2, SourceProvider: domain.ProviderCodex, Cwd: "/source", GitBranch: "main", SessionOriginID: "pinned-distill-failure", CompactionCount: 1},
		Events:   events,
	}
	hash, err := store.PutDoc(ctx, domain.SessionDoc{CIR: cir})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutSnapshot(ctx, domain.Snapshot{ID: hash, DocHash: hash, Branch: "main", Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: hash}); err != nil {
		t.Fatal(err)
	}
	svc := NewLoadSessionService(
		store,
		map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderCodex: codec.NewCodexCodec()},
		map[domain.ProviderKind]outbound.SessionMaterializer{domain.ProviderCodex: session.NewCodexMaterializer()},
		nil, failingDistiller{}, nil,
	)
	cwd := t.TempDir()

	out, err := svc.Load(ctx, inbound.LoadInput{Ref: "main", TargetProvider: domain.ProviderCodex, Mode: domain.FidelityFull, Cwd: cwd})
	if err == nil || !strings.Contains(err.Error(), "distill omitted context") {
		t.Fatalf("Load output=%+v error=%v, want fail-closed pinned distillation error", out, err)
	}
	var materialized []string
	if walkErr := filepath.Walk(filepath.Join(home, ".codex"), func(path string, info os.FileInfo, walkErr error) error {
		if walkErr == nil && info != nil && !info.IsDir() && strings.HasSuffix(path, ".jsonl") {
			materialized = append(materialized, path)
		}
		return nil
	}); walkErr != nil && !os.IsNotExist(walkErr) {
		t.Fatal(walkErr)
	}
	if len(materialized) != 0 {
		t.Fatalf("failed pinned distillation materialized provider sessions: %v", materialized)
	}
}

func TestLoadFullReplaysCodexReplacementInsteadOfPreCompactionArchive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	fixture := `{"timestamp":"2026-08-26T00:00:00Z","type":"session_meta","payload":{"id":"roll-load-active","cwd":"/old","model":"gpt-5-codex"}}
{"timestamp":"2026-08-26T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"archive before compaction must not replay"}]}}
{"timestamp":"2026-08-26T00:01:00Z","type":"compacted","payload":{"message":"","replacement_history":[{"type":"message","role":"user","content":[{"type":"input_text","text":"active compacted request"}]},{"type":"compaction","encrypted_content":"ACTIVE-ENC"}]}}
{"timestamp":"2026-08-26T00:02:00Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"active answer"}]}}`
	cir, err := codec.NewCodexCodec().Decode(ctx, []byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	hash, err := store.PutDoc(ctx, domain.SessionDoc{CIR: cir})
	if err != nil {
		t.Fatal(err)
	}
	memoryHash, err := store.PutMemory(ctx, domain.MemoryDigest{
		SnapshotID: hash, Summary: "CODEX PORTABLE MEMORY AFTER REPLACEMENT", Provider: domain.ProviderCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutSnapshot(ctx, domain.Snapshot{
		ID: hash, Branch: "main", DocHash: hash, Provider: domain.ProviderCodex,
		Fidelity: domain.FidelityFull, MemoryHash: memoryHash,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: hash}); err != nil {
		t.Fatal(err)
	}

	out, err := newLoadSvc(store).Load(ctx, inbound.LoadInput{
		Ref: "main", TargetProvider: domain.ProviderCodex, Mode: domain.FidelityFull, Cwd: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out.WrittenPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "archive before compaction must not replay") {
		t.Fatal("load rehydrated archival pre-compaction history")
	}
	if strings.Count(string(raw), `"type":"compacted"`) != 1 || !strings.Contains(string(raw), `"message":""`) {
		t.Fatalf("Codex replacement was not restored in native compacted shape:\n%s", raw)
	}
	if strings.Contains(string(raw), `"type":"response_item","payload":{"type":"compaction"`) {
		t.Fatalf("encrypted compaction state was flattened into an unsupported top-level response item:\n%s", raw)
	}
	for _, want := range []string{"active compacted request", "ACTIVE-ENC", "CODEX PORTABLE MEMORY AFTER REPLACEMENT", "active answer"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("load lost active replacement %q", want)
		}
	}
	restored, err := codec.NewCodexCodec().Decode(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Events) != 1 || restored.Events[0].Kind != domain.EventCompaction || !restored.Events[0].ReplacementComplete {
		t.Fatalf("restored archive does not contain one authoritative boundary: %+v", restored.Events)
	}
	active := restored.EffectiveContext()
	if len(active.Events) != 4 {
		t.Fatalf("restored active context = %+v", active.Events)
	}
	requestAt, lockedAt, memoryAt, answerAt := -1, -1, -1, -1
	for i, event := range active.Events {
		if event.Kind == domain.EventCompaction && event.Locked != nil && event.Locked.Blob == "ACTIVE-ENC" {
			lockedAt = i
		}
		for _, block := range event.Blocks {
			switch block.Text {
			case "active compacted request":
				requestAt = i
			case "active answer":
				answerAt = i
			default:
				if strings.Contains(block.Text, "CODEX PORTABLE MEMORY AFTER REPLACEMENT") {
					memoryAt = i
				}
			}
		}
	}
	if !(requestAt >= 0 && requestAt < lockedAt && lockedAt < memoryAt && memoryAt < answerAt) {
		t.Fatalf("replay order is not replacement → memory → suffix: request=%d locked=%d memory=%d answer=%d events=%+v",
			requestAt, lockedAt, memoryAt, answerAt, active.Events)
	}
}

func TestLoadFullPinsCodexReplacementWhileTrimmingLongSuffix(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	cir := domain.CIRDocument{
		Envelope: domain.Envelope{
			CIRVersion: domain.CIRVersionV2, SourceProvider: domain.ProviderCodex, SourceModel: "gpt-5-codex",
			CapturedAt: "2026-08-26T00:00:00Z", Cwd: "/old", GitBranch: "main",
			SessionOriginID: "roll-pinned", Fidelity: domain.FidelityFull, CompactionCount: 1,
		},
		Events: []domain.Event{
			{Kind: domain.EventMessage, Seq: 0, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "archival history"}}},
			{
				Kind: domain.EventCompaction, Seq: 1, ReplacementComplete: true,
				Replacement: []domain.Event{
					{Kind: domain.EventMessage, Seq: 0, Role: "user", CompactSummary: true, Blocks: []domain.ContentBlock{{Type: "text", Text: seedSummaryPrefix + " 1126 events were omitted\n" + strings.Repeat(seedSummaryPrefix+" nested inherited generation\n", 20)}}},
					{Kind: domain.EventMessage, Seq: 0, Role: "developer", Blocks: []domain.ContentBlock{{Type: "text", Text: "pinned runtime contract"}}},
					{Kind: domain.EventMessage, Seq: 1, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "pinned compacted memory"}}},
					{Kind: domain.EventCompaction, Seq: 2, Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted_content", Blob: "PINNED-ENC"}},
				},
			},
			{Kind: domain.EventMessage, Seq: 2, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "old suffix request"}}},
			{Kind: domain.EventMessage, Seq: 3, Role: "assistant", Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("oversized suffix ", 30000)}}},
			{Kind: domain.EventMessage, Seq: 4, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "recent request retained"}}},
			{Kind: domain.EventMessage, Seq: 5, Role: "assistant", Blocks: []domain.ContentBlock{{Type: "text", Text: "recent answer retained"}}},
		},
	}
	hash, err := store.PutDoc(ctx, domain.SessionDoc{CIR: cir})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutSnapshot(ctx, domain.Snapshot{ID: hash, Branch: "main", DocHash: hash, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: hash}); err != nil {
		t.Fatal(err)
	}

	out, err := newLoadSvc(store).Load(ctx, inbound.LoadInput{Ref: "main", TargetProvider: domain.ProviderCodex, Mode: domain.FidelityFull, Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if out.TrimmedEvents == 0 {
		t.Fatal("oversized post-compaction suffix was not trimmed")
	}
	raw, err := os.ReadFile(out.WrittenPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > seedBudgetBytes+(32<<10) {
		t.Fatalf("materialized Codex replay exceeds bounded allowance: %d", len(raw))
	}
	for _, want := range []string{"pinned runtime contract", "pinned compacted memory", "PINNED-ENC", "recent request retained", "recent answer retained", seedSummaryPrefix} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("bounded replay lost %q", want)
		}
	}
	restored, err := codec.NewCodexCodec().Decode(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	active := restored.EffectiveContext()
	lockedAt, digestAt, recentAt, digestCount := -1, -1, -1, 0
	for i, ev := range active.Events {
		if ev.Kind == domain.EventCompaction && ev.Locked != nil && ev.Locked.Blob == "PINNED-ENC" {
			lockedAt = i
		}
		if ev.Kind == domain.EventMessage && len(ev.Blocks) > 0 && strings.HasPrefix(ev.Blocks[0].Text, seedSummaryPrefix) {
			digestAt = i
			digestCount++
		}
		if ev.Kind == domain.EventMessage && len(ev.Blocks) > 0 && ev.Blocks[0].Text == "recent request retained" {
			recentAt = i
		}
	}
	if digestCount != 1 {
		t.Fatalf("active replay contains %d synthetic seed generations, want exactly one", digestCount)
	}
	if strings.Count(active.Events[digestAt].Blocks[0].Text, seedSummaryPrefix) != 1 {
		t.Fatalf("replacement digest recursively contains prior seed generations:\n%s", active.Events[digestAt].Blocks[0].Text)
	}
	if !(lockedAt >= 0 && lockedAt < digestAt && digestAt < recentAt) {
		t.Fatalf("replay order is not replacement → digest → recent tail: locked=%d digest=%d recent=%d events=%+v", lockedAt, digestAt, recentAt, active.Events)
	}

	// Loading the already-bounded replay again must be idempotent: no new trim
	// digest and no reintroduction of a prior synthetic generation.
	replayHash, err := store.PutDoc(ctx, domain.SessionDoc{CIR: restored})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutSnapshot(ctx, domain.Snapshot{ID: replayHash, Branch: "main", DocHash: replayHash, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		t.Fatal(err)
	}
	second, err := newLoadSvc(store).Load(ctx, inbound.LoadInput{Ref: string(replayHash), TargetProvider: domain.ProviderCodex, Mode: domain.FidelityFull, Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if second.TrimmedEvents != 0 {
		t.Fatalf("already-bounded replay was trimmed again: %d events", second.TrimmedEvents)
	}
	secondRaw, err := os.ReadFile(second.WrittenPath)
	if err != nil {
		t.Fatal(err)
	}
	secondDoc, err := codec.NewCodexCodec().Decode(ctx, secondRaw)
	if err != nil {
		t.Fatal(err)
	}
	secondSeedCount := 0
	for _, ev := range secondDoc.EffectiveContext().Events {
		if isSeedSummaryEvent(ev) {
			secondSeedCount++
			if strings.Count(ev.Blocks[0].Text, seedSummaryPrefix) != 1 {
				t.Fatalf("second load recursively expanded the seed:\n%s", ev.Blocks[0].Text)
			}
		}
	}
	if secondSeedCount != 1 {
		t.Fatalf("second load contains %d synthetic seeds, want one", secondSeedCount)
	}
}

// Memory mode: inject digest into CLAUDE.md.
func TestLoadMemoryMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CXT_CLAUDE_MEMORY_PROFILE", "v1")
	t.Setenv("CXT_CLAUDE_SETTING_SOURCES", "user,project,local")
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	seedClaudeSnapshot(t, store)
	cwd := t.TempDir()
	userInstructions := "# User-owned instructions\nPreserve this exactly."
	if err := os.WriteFile(filepath.Join(cwd, "CLAUDE.md"), []byte(userInstructions), 0o600); err != nil {
		t.Fatal(err)
	}
	nativeText := "OLDEST-NATIVE\n" + strings.Repeat("é", 80<<10) + "\nNEWEST-NATIVE"
	resolvedCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	nativePath := filepath.Join(home, ".claude", "projects", providerfs.EncodeCwd(resolvedCwd), "memory", "MEMORY.md")
	if err := os.MkdirAll(filepath.Dir(nativePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nativePath, []byte(nativeText), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CXT_CLAUDE_MEMORY_CONFIG_FINGERPRINT", memory.ClaudeMemoryConfigFingerprint(ctx, cwd))

	out, err := newLoadSvc(store).Load(ctx, inbound.LoadInput{Ref: "main", TargetProvider: domain.ProviderClaude, Mode: domain.FidelityMemory, Cwd: cwd})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out.Fidelity != domain.FidelityMemory {
		t.Fatalf("expected memory, got %s", out.Fidelity)
	}
	if !strings.HasSuffix(out.WrittenPath, "CLAUDE.md") {
		t.Fatalf("memory sink should write CLAUDE.md, got %s", out.WrittenPath)
	}
	data, err := os.ReadFile(out.WrittenPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), userInstructions) || !strings.Contains(string(data), "cxt memory") || !strings.Contains(string(data), "NEWEST-NATIVE") || !strings.Contains(string(data), "do it") {
		t.Fatalf("CLAUDE.md did not preserve user instructions and current conversation")
	}
	if strings.Contains(string(data), "OLDEST-NATIVE") {
		t.Fatalf("CLAUDE.md duplicated the safe cwd-native prefix that Claude already auto-loads")
	}
	if strings.Contains(string(data), "[cxt conversation delta v1]") {
		t.Fatal("private memory marker leaked into provider instruction file")
	}
	if len(data) > len(userInstructions)+(64<<10)+2 {
		t.Fatalf("CLAUDE.md bytes=%d exceeds user content + managed budget", len(data))
	}
	info, err := os.Stat(out.WrittenPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("CLAUDE.md mode=%v, want 0600", info.Mode().Perm())
	}
	storedNative, err := os.ReadFile(nativePath)
	if err != nil || string(storedNative) != nativeText {
		t.Fatalf("provider native memory was mutated: bytes=%d err=%v", len(storedNative), err)
	}
}

// checkout -b: branch creation (new branch ref) + cross-provider restoration.
func TestCheckoutForkAndLoad(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	head := seedClaudeSnapshot(t, store)
	cwd := t.TempDir()

	load := newLoadSvc(store)
	checkout := NewCheckoutSessionService(NewForkSessionService(store), load, store)

	co, err := checkout.Checkout(ctx, inbound.CheckoutInput{From: "main", NewBranch: "feat/x", TargetProvider: domain.ProviderCodex, Mode: domain.FidelityFull, Cwd: cwd})
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if co.Branch != "feat/x" || co.Head != head {
		t.Fatalf("checkout output: %+v (head want %s)", co, head)
	}
	if co.Fidelity != domain.FidelityReconstructed {
		t.Fatalf("cross checkout should be reconstructed, got %s", co.Fidelity)
	}
	// New branch ref must point to the head.
	ref, err := store.GetRef(ctx, "", domain.RefBranch, "feat/x")
	if err != nil || ref.Target != head {
		t.Fatalf("forked branch ref: %v %+v", err, ref)
	}
	symbolic, err := store.GetRef(ctx, "", domain.RefHEAD, "HEAD")
	if err != nil || symbolic.Symbolic != "feat/x" {
		t.Fatalf("HEAD must follow forked branch: %v %+v", err, symbolic)
	}
}

func TestCheckoutExistingBranchUpdatesHEAD(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	head := seedClaudeSnapshot(t, store)
	cwd := t.TempDir()

	if err := store.PutRef(ctx, domain.Ref{
		Kind: domain.RefBranch, Name: "other", Target: head,
	}); err != nil {
		t.Fatalf("put other branch: %v", err)
	}
	if err := store.PutRef(ctx, domain.Ref{
		Kind: domain.RefHEAD, Name: "HEAD", Symbolic: "other",
	}); err != nil {
		t.Fatalf("point HEAD at other: %v", err)
	}

	checkout := NewCheckoutSessionService(
		NewForkSessionService(store),
		newLoadSvc(store),
		store,
	)
	out, err := checkout.Checkout(ctx, inbound.CheckoutInput{
		From: "main", TargetProvider: domain.ProviderCodex, Mode: domain.FidelityFull, Cwd: cwd,
	})
	if err != nil {
		t.Fatalf("checkout main: %v", err)
	}
	if out.Branch != "main" {
		t.Fatalf("checkout branch = %q, want main", out.Branch)
	}
	symbolic, err := store.GetRef(ctx, "", domain.RefHEAD, "HEAD")
	if err != nil || symbolic.Symbolic != "main" {
		t.Fatalf("HEAD must follow existing branch: %v %+v", err, symbolic)
	}
}

func TestCheckoutTagKeepsCurrentHEAD(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	head := seedClaudeSnapshot(t, store)
	cwd := t.TempDir()

	if err := store.PutRef(ctx, domain.Ref{
		Kind: domain.RefTag, Name: "v1", Target: head,
	}); err != nil {
		t.Fatalf("put tag: %v", err)
	}
	checkout := NewCheckoutSessionService(
		NewForkSessionService(store),
		newLoadSvc(store),
		store,
	)
	out, err := checkout.Checkout(ctx, inbound.CheckoutInput{
		From: "v1", TargetProvider: domain.ProviderCodex, Mode: domain.FidelityFull, Cwd: cwd,
	})
	if err != nil {
		t.Fatalf("checkout tag: %v", err)
	}
	if out.Branch != "" {
		t.Fatalf("tag restore must be detached, got branch %q", out.Branch)
	}
	symbolic, err := store.GetRef(ctx, "", domain.RefHEAD, "HEAD")
	if err != nil || symbolic.Symbolic != "main" {
		t.Fatalf("tag restore must not move HEAD: %v %+v", err, symbolic)
	}
}
