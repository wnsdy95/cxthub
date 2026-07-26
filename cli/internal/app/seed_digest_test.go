package app

import (
	"context"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type stubDistiller struct{ d domain.MemoryDigest }

func (s stubDistiller) Distill(_ context.Context, _ domain.CIRDocument, _ *domain.NativeMemory) (domain.MemoryDigest, error) {
	return s.d, nil
}

type stubMemSource struct{ text string }

func (s stubMemSource) Provider() domain.ProviderKind { return domain.ProviderClaude }
func (s stubMemSource) ReadNative(_ context.Context, _ string) (domain.NativeMemory, bool, error) {
	return domain.NativeMemory{Source: "claude:MEMORY.md", Text: s.text}, s.text != "", nil
}

// TestPrependTrimDigestCompactSummary enforces a cycle-breaking contract for seed summary injection:
// (1) Synthetic events are marked with CompactSummary (viewer ◈ collapsible distillation last-wins),
// (2) Previous generation seed summaries [cxt] are removed (preventing generation accumulation),
// (3) tool names and ingestion markers in KeyFacts are excluded,
// (4) Omit summaries if they match native memory text (agent self-load — preventing double injection).
func TestPrependTrimDigestCompactSummary(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	mk := func(distilled domain.MemoryDigest, native string) *LoadSessionService {
		return NewLoadSessionService(st, nil, nil,
			map[domain.ProviderKind]outbound.MemorySource{domain.ProviderClaude: stubMemSource{text: native}},
			stubDistiller{d: distilled}, nil)
	}
	full := domain.CIRDocument{Events: []domain.Event{
		{Kind: domain.EventMessage, Role: "user", Seq: 0, Blocks: []domain.ContentBlock{{Type: "text", Text: "old work"}}},
		{Kind: domain.EventMessage, Role: "assistant", Seq: 1, Blocks: []domain.ContentBlock{{Type: "text", Text: "old answer"}}},
	}}
	seed := domain.CIRDocument{Events: []domain.Event{
		// Previous generation seed summaries 2 types: marked + legacy (unmarked) — both are removal targets.
		{Kind: domain.EventMessage, Role: "user", Seq: 2, CompactSummary: true,
			Blocks: []domain.ContentBlock{{Type: "text", Text: seedSummaryPrefix + " 5 older events were omitted..."}}},
		{Kind: domain.EventMessage, Role: "user", Seq: 3,
			Blocks: []domain.ContentBlock{{Type: "text", Text: seedSummaryPrefix + " 9 older events were omitted..."}}},
		{Kind: domain.EventMessage, Role: "user", Seq: 4, Blocks: []domain.ContentBlock{{Type: "text", Text: "recent prompt"}}},
	}}

	svc := mk(domain.MemoryDigest{
		Summary:  "did X, decided Y",
		KeyFacts: []string{"apply_patch", "unknown:Agent", "native memory: claude:MEMORY.md", "absorbed from claude:MEMORY.md", "budget is 400KB per seed"},
	}, "")
	out := svc.prependTrimDigest(ctx, full, seed, domain.Snapshot{}, domain.ProviderClaude, t.TempDir(), 2)

	if len(out.Events) != 2 {
		t.Fatalf("Event count: got %d want 2 (new summary 1 + recent 1 — old [cxt] removed)", len(out.Events))
	}
	head := out.Events[0]
	if !head.CompactSummary {
		t.Fatal("Seed summary not marked as CompactSummary")
	}
	text := head.Blocks[0].Text
	if !strings.HasPrefix(text, seedSummaryPrefix) || !strings.Contains(text, "did X, decided Y") {
		t.Fatalf("Missing summary body: %q", text)
	}
	if strings.Contains(text, "apply_patch") || strings.Contains(text, "unknown:Agent") ||
		strings.Contains(text, "native memory:") || strings.Contains(text, "absorbed from") {
		t.Fatalf("KeyFacts noise included in seed: %q", text)
	}
	if !strings.Contains(text, "budget is 400KB per seed") {
		t.Fatalf("Missing sentence-form KeyFact: %q", text)
	}
	if out.Events[1].Blocks[0].Text != "recent prompt" {
		t.Fatalf("Tail preservation failed: %+v", out.Events[1])
	}

	// (4) Omit summary of native memory text as-is.
	svc = mk(domain.MemoryDigest{Summary: "MEMROOT\nfacts"}, "MEMROOT\nfacts")
	out = svc.prependTrimDigest(ctx, full, seed, domain.Snapshot{}, domain.ProviderClaude, t.TempDir(), 2)
	if strings.Contains(out.Events[0].Blocks[0].Text, "MEMROOT") {
		t.Fatalf("Native memory text re-injected into seed: %q", out.Events[0].Blocks[0].Text)
	}
}
