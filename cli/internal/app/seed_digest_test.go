package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type stubDistiller struct{ d domain.MemoryDigest }

func (s stubDistiller) Distill(_ context.Context, _ domain.CIRDocument, _ *domain.NativeMemory) (domain.MemoryDigest, error) {
	return s.d, nil
}

type failingDistiller struct{}

func (failingDistiller) Distill(_ context.Context, _ domain.CIRDocument, _ *domain.NativeMemory) (domain.MemoryDigest, error) {
	return domain.MemoryDigest{}, errors.New("distillation unavailable")
}

type stubMemSource struct{ text string }

func (s stubMemSource) Provider() domain.ProviderKind { return domain.ProviderClaude }
func (s stubMemSource) ReadNative(_ context.Context, _, _ string) (domain.NativeMemory, bool, error) {
	return domain.NativeMemory{Source: "claude:MEMORY.md", Text: s.text}, s.text != "", nil
}

type recordingSessionMemSource struct {
	provider             domain.ProviderKind
	calls                int
	gotCwd, gotSessionID string
}

func (s *recordingSessionMemSource) Provider() domain.ProviderKind { return s.provider }
func (s *recordingSessionMemSource) ReadNative(_ context.Context, cwd, sessionID string) (domain.NativeMemory, bool, error) {
	s.calls++
	s.gotCwd = cwd
	s.gotSessionID = sessionID
	return domain.NativeMemory{Source: "codex:rollout_summary", Text: "exact session memory"}, true, nil
}

type recordingMemorySink struct{ provider domain.ProviderKind }

func (s recordingMemorySink) Provider() domain.ProviderKind { return s.provider }
func (s recordingMemorySink) Inject(_ context.Context, _ domain.MemoryDigest, cwd string) (string, error) {
	return cwd + "/managed-memory", nil
}

func TestMemorizeReadsNativeMemoryForExactProviderSession(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	cwd := t.TempDir()
	repo := domain.Repo{ID: "repo-exact-native-memory", LocalPath: cwd, DefaultBranch: "main"}
	docHash, err := store.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{
		Envelope: domain.Envelope{
			CIRVersion:      domain.CIRVersionV2,
			SourceProvider:  domain.ProviderCodex,
			SessionOriginID: "codex-session-exact",
		},
		Events: []domain.Event{{Kind: domain.EventMessage, Role: "user", Seq: 0}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutSnapshot(ctx, domain.Snapshot{
		ID: docHash, DocHash: docHash, RepoID: repo.ID, Branch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRef(ctx, domain.Ref{
		Kind: domain.RefBranch, Name: "main", RepoID: repo.ID, Target: docHash,
	}); err != nil {
		t.Fatal(err)
	}

	source := &recordingSessionMemSource{provider: domain.ProviderCodex}
	service := NewMemorizeService(
		branchSeedGit{repo: repo}, nil, nil,
		map[domain.ProviderKind]outbound.MemorySource{domain.ProviderCodex: source},
		stubDistiller{d: domain.MemoryDigest{Summary: "fresh exact-session memory"}}, store,
	)
	if _, err := service.Memorize(ctx, inbound.MemorizeInput{Cwd: cwd}); err != nil {
		t.Fatal(err)
	}
	if source.calls != 1 || source.gotCwd != cwd || source.gotSessionID != "codex-session-exact" {
		t.Fatalf("native memory lookup calls=%d cwd=%q session=%q", source.calls, source.gotCwd, source.gotSessionID)
	}
	mismatched := &recordingSessionMemSource{provider: domain.ProviderClaude}
	service = NewMemorizeService(
		branchSeedGit{repo: repo}, nil, nil,
		map[domain.ProviderKind]outbound.MemorySource{domain.ProviderClaude: mismatched},
		stubDistiller{d: domain.MemoryDigest{Summary: "must not be attached"}}, store,
	)
	if _, err := service.Memorize(ctx, inbound.MemorizeInput{Cwd: cwd, Provider: domain.ProviderClaude}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched provider error=%v", err)
	}
	if mismatched.calls != 0 {
		t.Fatalf("mismatched provider read unrelated native memory %d time(s)", mismatched.calls)
	}
}

func TestLoadMemoryScopesNativeLookupToSameProviderSession(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	cir := domain.CIRDocument{Envelope: domain.Envelope{
		CIRVersion:      domain.CIRVersionV2,
		SourceProvider:  domain.ProviderCodex,
		SessionOriginID: "codex-session-exact",
	}}
	snap := domain.Snapshot{ID: domain.HashContent([]byte("load-memory-session-scope"))}

	for _, tc := range []struct {
		name        string
		target      domain.ProviderKind
		wantCalls   int
		wantSession string
	}{
		{name: "same provider", target: domain.ProviderCodex, wantCalls: 1, wantSession: "codex-session-exact"},
		{name: "cross provider", target: domain.ProviderClaude, wantCalls: 0, wantSession: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &recordingSessionMemSource{provider: tc.target}
			service := NewLoadSessionService(
				storage.NewFileStore(t.TempDir()), nil, nil,
				map[domain.ProviderKind]outbound.MemorySource{tc.target: source},
				stubDistiller{d: domain.MemoryDigest{Summary: "loaded memory"}},
				map[domain.ProviderKind]outbound.MemorySink{tc.target: recordingMemorySink{provider: tc.target}},
			)
			if _, err := service.loadMemory(ctx, cir, snap, tc.target, cwd); err != nil {
				t.Fatal(err)
			}
			if source.calls != tc.wantCalls || source.gotSessionID != tc.wantSession {
				t.Fatalf("native memory lookup calls=%d session=%q, want calls=%d session=%q", source.calls, source.gotSessionID, tc.wantCalls, tc.wantSession)
			}
		})
	}
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
	out := svc.prependTrimDigest(ctx, full, seed, domain.Snapshot{}, domain.ProviderClaude, t.TempDir())

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
	out = svc.prependTrimDigest(ctx, full, seed, domain.Snapshot{}, domain.ProviderClaude, t.TempDir())
	if strings.Contains(out.Events[0].Blocks[0].Text, "MEMROOT") {
		t.Fatalf("Native memory text re-injected into seed: %q", out.Events[0].Blocks[0].Text)
	}
}

func TestPrependTrimDigestProjectsStoredMemoryWithoutSeedRecursion(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	snapshotID := domain.HashContent([]byte("snapshot-with-recursive-memory"))
	recursiveSummary := "project history before recursion\n" + seedSummaryPrefix +
		" 99 events were omitted\n" + strings.Repeat("recursive inherited seed\n", 200)
	memoryHash, err := st.PutMemory(ctx, domain.MemoryDigest{
		SnapshotID: snapshotID,
		Summary:    recursiveSummary,
		KeyFacts:   []string{"stored structured fact remains available"},
		Fragments: []domain.MemoryFragment{{
			SourceSnapshot: snapshotID,
			Summary:        recursiveSummary,
			KeyFacts:       []string{"stored structured fact remains available"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	snap := domain.Snapshot{ID: snapshotID, DocHash: snapshotID, MemoryHash: memoryHash, Branch: "main"}
	if err := st.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	svc := NewLoadSessionService(st, nil, nil, nil, stubDistiller{d: domain.MemoryDigest{
		Summary: "fresh omitted conversation",
	}}, nil)
	omitted := domain.CIRDocument{Events: []domain.Event{
		{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "real old request"}}},
	}}
	seed := domain.CIRDocument{Events: []domain.Event{
		{Kind: domain.EventMessage, Role: "user", Seq: 1, Blocks: []domain.ContentBlock{{Type: "text", Text: "latest request"}}},
	}}

	out := svc.prependTrimDigest(ctx, omitted, seed, snap, domain.ProviderCodex, t.TempDir())
	if len(out.Events) != 2 || len(out.Events[0].Blocks) == 0 {
		t.Fatalf("projected seed = %+v", out.Events)
	}
	text := out.Events[0].Blocks[0].Text
	if count := strings.Count(text, seedSummaryPrefix); count != 1 {
		t.Fatalf("trim digest contains %d cxt generations, want only its own header:\n%s", count, text)
	}
	for _, forbidden := range []string{"recursive inherited seed", "project history before recursion"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("stored recursive summary leaked into prompt: %q", forbidden)
		}
	}
	for _, want := range []string{"fresh omitted conversation", "stored structured fact remains available"} {
		if !strings.Contains(text, want) {
			t.Fatalf("prompt projection lost %q:\n%s", want, text)
		}
	}
}

func TestInsertTrimDigestKeepsExistingSeedWhenReplacementFails(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	svc := NewLoadSessionService(st, nil, nil, nil, failingDistiller{}, nil)
	oldSeed := seedSummaryPrefix + " 50 events were omitted\nsole surviving project memory"
	seed := domain.CIRDocument{Events: []domain.Event{
		{Kind: domain.EventMessage, Role: "user", CompactSummary: true, Blocks: []domain.ContentBlock{{Type: "text", Text: oldSeed}}},
		{Kind: domain.EventCompaction, Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted_content", Blob: "PINNED"}},
		{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "recent request"}}},
	}}
	omitted := domain.CIRDocument{Events: []domain.Event{
		{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "older request"}}},
	}}

	out := svc.insertTrimDigestAfterPrefix(ctx, omitted, seed, 2, domain.Snapshot{}, domain.ProviderCodex, t.TempDir())
	if len(out.Events) != len(seed.Events) || out.Events[0].Blocks[0].Text != oldSeed {
		t.Fatalf("failed replacement discarded the existing seed: %+v", out.Events)
	}
	if out.Events[1].Locked == nil || out.Events[1].Locked.Blob != "PINNED" {
		t.Fatalf("failed replacement changed provider state: %+v", out.Events[1])
	}
}

func TestInsertTrimDigestProjectsExistingSeedWithoutStoredMemory(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	svc := NewLoadSessionService(st, nil, nil, nil, stubDistiller{d: domain.MemoryDigest{
		Summary: "fresh omitted conversation",
	}}, nil)
	snapshotID := domain.HashContent([]byte("memoryless-synthetic-seed"))
	oldSeed := seedSummaryPrefix + " 50 events were omitted\nsole surviving project memory\n" +
		seedSummaryPrefix + " nested generation that must not recur\n\n" +
		"Key facts:\n- The sole-memory fallback must retain structured decisions.\n\n" +
		"Open tasks:\n- Verify the memoryless replay path."
	seed := domain.CIRDocument{Events: []domain.Event{
		{Kind: domain.EventMessage, Role: "user", CompactSummary: true, Blocks: []domain.ContentBlock{{Type: "text", Text: oldSeed}}},
		{Kind: domain.EventCompaction, Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted_content", Blob: "PINNED"}},
		{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "recent request"}}},
	}}
	omitted := domain.CIRDocument{Events: []domain.Event{
		{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "older request"}}},
	}}

	out := svc.insertTrimDigestAfterPrefix(ctx, omitted, seed, 2, domain.Snapshot{ID: snapshotID}, domain.ProviderCodex, t.TempDir())
	seedCount := 0
	seedText := ""
	for _, ev := range out.Events {
		if isSeedSummaryEvent(ev) {
			seedCount++
			seedText = ev.Blocks[0].Text
		}
	}
	if seedCount != 1 || strings.Count(seedText, seedSummaryPrefix) != 1 {
		t.Fatalf("memoryless seed was not collapsed: count=%d\n%s", seedCount, seedText)
	}
	for _, want := range []string{
		"sole surviving project memory",
		"fresh omitted conversation",
		"[prior cxt context]",
		"The sole-memory fallback must retain structured decisions.",
		"Verify the memoryless replay path.",
	} {
		if !strings.Contains(seedText, want) {
			t.Fatalf("memoryless projection lost %q:\n%s", want, seedText)
		}
	}
	if out.Events[0].Locked == nil || out.Events[0].Locked.Blob != "PINNED" {
		t.Fatalf("memoryless projection changed provider state: %+v", out.Events)
	}
}

func TestInsertTrimDigestFallsBackWhenStoredMemorySanitizesEmpty(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	svc := NewLoadSessionService(st, nil, nil, nil, stubDistiller{d: domain.MemoryDigest{
		Summary: "fresh omitted conversation",
	}}, nil)
	snapshotID := domain.HashContent([]byte("empty-sanitized-stored-memory"))
	memoryHash, err := st.PutMemory(ctx, domain.MemoryDigest{
		SnapshotID: snapshotID,
		Summary:    seedSummaryPrefix + " recursively stored without structure",
	})
	if err != nil {
		t.Fatal(err)
	}
	oldSeed := seedSummaryPrefix + " 50 events were omitted\nsole surviving fallback memory"
	seed := domain.CIRDocument{Events: []domain.Event{
		{Kind: domain.EventMessage, Role: "user", CompactSummary: true, Blocks: []domain.ContentBlock{{Type: "text", Text: oldSeed}}},
		{Kind: domain.EventCompaction, Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted_content", Blob: "PINNED"}},
		{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "recent request"}}},
	}}
	omitted := domain.CIRDocument{Events: []domain.Event{
		{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "older request"}}},
	}}
	snap := domain.Snapshot{ID: snapshotID, DocHash: snapshotID, MemoryHash: memoryHash}

	out := svc.insertTrimDigestAfterPrefix(ctx, omitted, seed, 2, snap, domain.ProviderCodex, t.TempDir())
	seedCount := 0
	for _, ev := range out.Events {
		if !isSeedSummaryEvent(ev) {
			continue
		}
		seedCount++
		if !strings.Contains(ev.Blocks[0].Text, "sole surviving fallback memory") {
			t.Fatalf("empty stored projection displaced the only usable seed:\n%s", ev.Blocks[0].Text)
		}
	}
	if seedCount != 1 {
		t.Fatalf("fallback produced %d seed events, want one", seedCount)
	}
}
