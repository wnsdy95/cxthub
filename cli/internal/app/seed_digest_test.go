package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/memory"
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

type recordingNativeDistiller struct {
	native *domain.NativeMemory
}

func (d *recordingNativeDistiller) Distill(_ context.Context, _ domain.CIRDocument, native *domain.NativeMemory) (domain.MemoryDigest, error) {
	d.native = native
	return domain.MemoryDigest{Summary: "loaded memory"}, nil
}

type stubMemSource struct {
	text             string
	autoLoadedPrefix string
}

func (s stubMemSource) Provider() domain.ProviderKind { return domain.ProviderClaude }
func (s stubMemSource) ReadNative(_ context.Context, _, _ string) (domain.NativeMemory, bool, error) {
	return domain.NativeMemory{
		Provider: domain.ProviderClaude, Source: "claude:MEMORY.md",
		Scope: domain.NativeMemoryScopeWorkingTree, Text: s.text, AutoLoadedPrefix: s.autoLoadedPrefix,
	}, s.text != "", nil
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
	return domain.NativeMemory{
		Provider: s.provider, Source: "codex:rollout_summary",
		Scope: domain.NativeMemoryScopeSession, Text: "exact session memory",
	}, true, nil
}

type recordingMemorySink struct{ provider domain.ProviderKind }

func (s recordingMemorySink) Provider() domain.ProviderKind { return s.provider }
func (s recordingMemorySink) Inject(_ context.Context, _ domain.MemoryDigest, cwd string) (string, error) {
	return cwd + "/managed-memory", nil
}

type recordingDigestSink struct {
	provider domain.ProviderKind
	digest   domain.MemoryDigest
}

func (s *recordingDigestSink) Provider() domain.ProviderKind { return s.provider }
func (s *recordingDigestSink) Inject(_ context.Context, digest domain.MemoryDigest, cwd string) (string, error) {
	s.digest = digest
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

func TestLoadMemorySeparatesTargetProjectionLookupFromSourceDistillation(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	cir := domain.CIRDocument{Envelope: domain.Envelope{
		CIRVersion:      domain.CIRVersionV2,
		SourceProvider:  domain.ProviderCodex,
		SessionOriginID: "codex-session-exact",
	}}
	snap := domain.Snapshot{ID: domain.HashContent([]byte("load-memory-session-scope"))}

	for _, tc := range []struct {
		name       string
		target     domain.ProviderKind
		wantNative bool
	}{
		{name: "same provider", target: domain.ProviderCodex, wantNative: true},
		{name: "cross provider", target: domain.ProviderClaude, wantNative: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &recordingSessionMemSource{provider: tc.target}
			distiller := &recordingNativeDistiller{}
			service := NewLoadSessionService(
				storage.NewFileStore(t.TempDir()), nil, nil,
				map[domain.ProviderKind]outbound.MemorySource{tc.target: source},
				distiller,
				map[domain.ProviderKind]outbound.MemorySink{tc.target: recordingMemorySink{provider: tc.target}},
			)
			if _, err := service.loadMemory(ctx, cir, snap, tc.target, cwd); err != nil {
				t.Fatal(err)
			}
			if source.calls != 1 || source.gotSessionID != "codex-session-exact" {
				t.Fatalf("target projection lookup calls=%d session=%q", source.calls, source.gotSessionID)
			}
			if got := distiller.native != nil; got != tc.wantNative {
				t.Fatalf("native memory passed to source distillation=%v, want %v", got, tc.wantNative)
			}
		})
	}
}

func TestLoadMemoryProjectsNativeBaselineByScope(t *testing.T) {
	ctx := context.Background()
	const currentDecision = "CURRENT MEMORY-MODE DECISION"
	for _, tc := range []struct {
		name         string
		provider     domain.ProviderKind
		source       outbound.MemorySource
		nativeText   string
		wantBaseline bool
	}{
		{
			name: "Claude working-tree memory stays portable without runtime attestation", provider: domain.ProviderClaude,
			source: stubMemSource{text: "CLAUDE MEMORY MODE BASELINE"}, nativeText: "CLAUDE MEMORY MODE BASELINE", wantBaseline: true,
		},
		{
			name: "Codex thread memory must cross into the new session", provider: domain.ProviderCodex,
			source: &recordingSessionMemSource{provider: domain.ProviderCodex}, nativeText: "exact session memory", wantBaseline: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			sink := &recordingDigestSink{provider: tc.provider}
			service := NewLoadSessionService(
				storage.NewFileStore(t.TempDir()), nil, nil,
				map[domain.ProviderKind]outbound.MemorySource{tc.provider: tc.source},
				memory.NewRuleDistiller(),
				map[domain.ProviderKind]outbound.MemorySink{tc.provider: sink},
			)
			cir := domain.CIRDocument{Envelope: domain.Envelope{
				SourceProvider: tc.provider, SessionOriginID: "codex-session-exact",
			}, Events: []domain.Event{seedMessage("user", currentDecision, 0)}}
			if _, err := service.loadMemory(ctx, cir, domain.Snapshot{}, tc.provider, cwd); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(sink.digest.Summary, tc.nativeText); got != tc.wantBaseline {
				t.Fatalf("managed memory baseline present=%v, want %v:\n%s", got, tc.wantBaseline, sink.digest.Summary)
			}
			if !strings.Contains(sink.digest.Summary, currentDecision) {
				t.Fatalf("managed memory lost current conversation:\n%s", sink.digest.Summary)
			}
		})
	}
}

// TestPrependTrimDigestCompactSummary enforces a cycle-breaking contract for seed summary injection:
// (1) Synthetic events are marked with CompactSummary (viewer ◈ collapsible distillation last-wins),
// (2) Previous generation seed summaries [cxt] are removed (preventing generation accumulation),
// (3) tool names and ingestion markers in KeyFacts are excluded,
// (4) Keep native memory portable when the target runtime did not attest an exact load.
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
		KeyFacts: []string{"apply_patch", "unknown:Agent", "native memory: claude:MEMORY.md", "absorbed from claude:MEMORY.md", "ingested from codex:memories_1.sqlite", "budget is 400KB per seed"},
	}, "")
	out, err := svc.prependTrimDigest(ctx, full, seed, domain.Snapshot{}, domain.ProviderClaude, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

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
		strings.Contains(text, "native memory:") || strings.Contains(text, "absorbed from") || strings.Contains(text, "ingested from") {
		t.Fatalf("KeyFacts noise included in seed: %q", text)
	}
	if !strings.Contains(text, "budget is 400KB per seed") {
		t.Fatalf("Missing sentence-form KeyFact: %q", text)
	}
	if out.Events[1].Blocks[0].Text != "recent prompt" {
		t.Fatalf("Tail preservation failed: %+v", out.Events[1])
	}

	// (4) Keep native memory text as a portable fallback.
	svc = mk(domain.MemoryDigest{Summary: "MEMROOT\nfacts"}, "MEMROOT\nfacts")
	out, err = svc.prependTrimDigest(ctx, full, seed, domain.Snapshot{}, domain.ProviderClaude, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Events[0].Blocks[0].Text, "MEMROOT") {
		t.Fatalf("Portable native memory missing from seed: %q", out.Events[0].Blocks[0].Text)
	}
}

func TestPrependTrimDigestKeepsOneClaudeNativeBaselineInsideMergedProjection(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	snapshotID := domain.HashContent([]byte("claude-native-current-snapshot"))
	ancestorID := domain.HashContent([]byte("claude-native-ancestor-snapshot"))
	const nativeBaseline = "CLAUDE CWD NATIVE BASELINE"
	ancestor := domain.MemoryDigest{
		SnapshotID: ancestorID,
		Summary: domain.AppendExtractiveConversationDelta(nativeBaseline,
			domain.RenderExtractiveFallbackSummary([]string{"ANCESTOR DECISION MUST REMAIN"}, nil)),
	}
	ancestor = domain.MergeDigests(domain.MemoryDigest{}, ancestor)
	ancestorMemoryHash, err := st.PutMemory(ctx, ancestor)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{
		ID: ancestorID, DocHash: ancestorID, MemoryHash: ancestorMemoryHash,
	}); err != nil {
		t.Fatal(err)
	}
	stored := domain.MemoryDigest{
		SnapshotID: snapshotID,
		Fragments: []domain.MemoryFragment{
			{
				SourceSnapshot: snapshotID,
				Summary: domain.AppendExtractiveConversationDelta(nativeBaseline,
					domain.RenderExtractiveFallbackSummary([]string{"CURRENT RAW TURN"}, nil)),
			},
		},
	}
	stored = domain.MergeDigests(domain.MemoryDigest{}, stored)
	memoryHash, err := st.PutMemory(ctx, stored)
	if err != nil {
		t.Fatal(err)
	}
	snap := domain.Snapshot{
		ID: snapshotID, DocHash: snapshotID, MemoryHash: memoryHash,
		Parents: []domain.ContentHash{ancestorID},
	}
	if err := st.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}

	svc := NewLoadSessionService(st, nil, nil,
		map[domain.ProviderKind]outbound.MemorySource{
			domain.ProviderClaude: stubMemSource{text: nativeBaseline},
		},
		stubDistiller{d: domain.MemoryDigest{Summary: "FRESH OMITTED DECISION"}}, nil,
	)
	omitted := domain.CIRDocument{Envelope: domain.Envelope{
		SourceProvider: domain.ProviderClaude, SessionOriginID: "claude-source-session",
	}, Events: []domain.Event{seedMessage("user", "old request", 0)}}
	seed := domain.CIRDocument{Events: []domain.Event{seedMessage("user", "RECENT RAW TURN", 1)}}

	out, err := svc.prependTrimDigest(ctx, omitted, seed, snap, domain.ProviderClaude, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var prompt strings.Builder
	for _, event := range out.Events {
		for _, block := range event.Blocks {
			prompt.WriteString(block.Text)
		}
	}
	if got := strings.Count(prompt.String(), nativeBaseline); got != 1 {
		t.Fatalf("portable cwd-native baseline count=%d, want one copy:\n%s", got, prompt.String())
	}
	for _, want := range []string{"ANCESTOR DECISION MUST REMAIN", "FRESH OMITTED DECISION", "RECENT RAW TURN"} {
		if !strings.Contains(prompt.String(), want) {
			t.Fatalf("native baseline projection lost %q:\n%s", want, prompt.String())
		}
	}
	if strings.Contains(prompt.String(), "CURRENT RAW TURN") {
		t.Fatalf("current source delta was duplicated into the summary:\n%s", prompt.String())
	}
}

func TestPrependTrimDigestKeepsCodexThreadNativeMemoryForNewSession(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	source := &recordingSessionMemSource{provider: domain.ProviderCodex}
	svc := NewLoadSessionService(st, nil, nil,
		map[domain.ProviderKind]outbound.MemorySource{domain.ProviderCodex: source},
		stubDistiller{d: domain.MemoryDigest{Summary: "exact session memory"}}, nil,
	)
	omitted := domain.CIRDocument{Envelope: domain.Envelope{
		SourceProvider: domain.ProviderCodex, SessionOriginID: "codex-session-exact",
	}, Events: []domain.Event{seedMessage("user", "old request", 0)}}
	seed := domain.CIRDocument{Events: []domain.Event{seedMessage("user", "recent request", 1)}}

	out, err := svc.prependTrimDigest(ctx, omitted, seed, domain.Snapshot{}, domain.ProviderCodex, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if source.calls != 1 || source.gotSessionID != "codex-session-exact" {
		t.Fatalf("native lookup calls=%d session=%q", source.calls, source.gotSessionID)
	}
	if len(out.Events) == 0 || len(out.Events[0].Blocks) == 0 ||
		!strings.Contains(out.Events[0].Blocks[0].Text, "exact session memory") {
		t.Fatalf("thread-scoped Codex memory was dropped before materializing a new thread: %+v", out.Events)
	}
}

func TestPortableReplaySeedCarriesExistingSyntheticMemoryBeforeReplacement(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	snapshotID := domain.HashContent([]byte("portable-replay-existing-seed"))
	memoryHash, err := st.PutMemory(ctx, domain.MemoryDigest{
		SnapshotID: snapshotID,
		Summary:    "PORTABLE PROVIDER BASELINE",
	})
	if err != nil {
		t.Fatal(err)
	}
	snap := domain.Snapshot{ID: snapshotID, DocHash: snapshotID, MemoryHash: memoryHash}
	if err := st.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	oldSeed := seedSummaryPrefix + " 50 events were omitted\n" +
		"SOLE SURVIVING PROJECT MEMORY\n\n" +
		"Key facts:\n- Preserve the only prior decision.\n\n" +
		"Open tasks:\n- Verify replacement without recursion."
	seed := domain.CIRDocument{Events: []domain.Event{
		{Kind: domain.EventMessage, Role: "user", CompactSummary: true, Blocks: []domain.ContentBlock{{Type: "text", Text: oldSeed}}},
		{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "CURRENT RAW REQUEST"}}},
	}}
	svc := NewLoadSessionService(st, nil, nil, nil, nil, nil)

	event, ok := svc.portableReplaySeed(ctx, seed, snap, domain.ProviderClaude, t.TempDir(), seedDigestBudgetBytes)
	if !ok || len(event.Blocks) == 0 {
		t.Fatal("portable replay seed was not produced")
	}
	text := event.Blocks[0].Text
	for _, want := range []string{
		"PORTABLE PROVIDER BASELINE",
		"SOLE SURVIVING PROJECT MEMORY",
		"Preserve the only prior decision.",
	} {
		if got := strings.Count(text, want); got != 1 {
			t.Fatalf("portable replacement count(%q)=%d, want one:\n%s", want, got, text)
		}
	}
	if got := strings.Count(text, seedSummaryPrefix); got != 1 {
		t.Fatalf("portable replacement retained %d recursive seed markers, want one:\n%s", got, text)
	}
	if strings.Contains(text, "Verify replacement without recursion.") {
		t.Fatalf("unmarked legacy seed task was re-attested:\n%s", text)
	}
}

func TestPortableReplaySeedOmitsExactlyAttestedWorkingTreeBaseline(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	snapshotID := domain.HashContent([]byte("portable-replay-attested-baseline"))
	const baseline = "EXACTLY ATTESTED WORKING TREE BASELINE"
	memoryHash, err := st.PutMemory(ctx, domain.MemoryDigest{
		SnapshotID: snapshotID,
		Summary:    baseline,
	})
	if err != nil {
		t.Fatal(err)
	}
	snap := domain.Snapshot{ID: snapshotID, DocHash: snapshotID, MemoryHash: memoryHash}
	if err := st.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	svc := NewLoadSessionService(st, nil, nil,
		map[domain.ProviderKind]outbound.MemorySource{
			domain.ProviderClaude: stubMemSource{text: baseline, autoLoadedPrefix: baseline},
		}, nil, nil,
	)

	if event, ok := svc.portableReplaySeed(
		ctx, domain.CIRDocument{}, snap, domain.ProviderClaude, t.TempDir(), seedDigestBudgetBytes,
	); ok {
		t.Fatalf("exact auto-load attestation produced redundant seed: %+v", event)
	}
}

func TestPortableReplaySeedKeepsReplayedAuthoritativeTaskTombstone(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	snapshotID := domain.HashContent([]byte("portable-replay-task-tombstone"))
	const compactSummary = "CURRENT PROVIDER SUMMARY WITH AUTHORITATIVE TASKS"
	digest := domain.MergeDigests(domain.MemoryDigest{}, domain.MemoryDigest{
		SnapshotID:         snapshotID,
		Summary:            compactSummary,
		TasksAuthoritative: true,
	})
	memoryHash, err := st.PutMemory(ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	snap := domain.Snapshot{ID: snapshotID, DocHash: snapshotID, MemoryHash: memoryHash}
	if err := st.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	oldSeed := seedSummaryPrefix + " old generation\n" +
		"OLDER PROJECT NARRATIVE MUST REMAIN\n\n" +
		"Open tasks:\n- Superseded task must not revive."
	seed := domain.CIRDocument{Events: []domain.Event{
		{Kind: domain.EventMessage, Role: "user", CompactSummary: true, Blocks: []domain.ContentBlock{{Type: "text", Text: compactSummary}}},
		{Kind: domain.EventMessage, Role: "user", CompactSummary: true, Blocks: []domain.ContentBlock{{Type: "text", Text: oldSeed}}},
		{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "CURRENT RAW REQUEST"}}},
	}}
	svc := NewLoadSessionService(st, nil, nil, nil, nil, nil)

	event, ok := svc.portableReplaySeed(ctx, seed, snap, domain.ProviderClaude, t.TempDir(), seedDigestBudgetBytes)
	if !ok || len(event.Blocks) == 0 {
		t.Fatal("portable replay seed was not produced")
	}
	text := event.Blocks[0].Text
	if !strings.Contains(text, "OLDER PROJECT NARRATIVE MUST REMAIN") {
		t.Fatalf("portable replay lost prior narrative:\n%s", text)
	}
	if strings.Contains(text, "Superseded task must not revive.") {
		t.Fatalf("replayed authoritative task tombstone was lost:\n%s", text)
	}
	if strings.Contains(text, compactSummary) {
		t.Fatalf("provider summary already in raw replay was duplicated:\n%s", text)
	}
}

func TestPrependTrimDigestPreservesPostCompactionOmittedSpan(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	svc := NewLoadSessionService(st, nil, nil, nil, memory.NewRuleDistiller(), nil)
	omitted := domain.CIRDocument{Envelope: domain.Envelope{SourceProvider: domain.ProviderClaude}, Events: []domain.Event{
		{Kind: domain.EventMessage, Role: "user", CompactSummary: true, Blocks: []domain.ContentBlock{{Type: "text", Text: "LOAD COMPACT BASELINE"}}},
		{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "LOAD OMITTED POST-COMPACT DECISION"}}},
		{Kind: domain.EventMessage, Role: "assistant", Blocks: []domain.ContentBlock{{Type: "text", Text: "LOAD OMITTED POST-COMPACT RESULT"}}},
	}}
	seed := domain.CIRDocument{Events: []domain.Event{
		{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "LOAD RECENT RAW REQUEST"}}},
	}}

	out, err := svc.prependTrimDigest(ctx, omitted, seed, domain.Snapshot{ID: domain.HashContent([]byte("load-omitted-span"))}, domain.ProviderClaude, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Events) != 2 || len(out.Events[0].Blocks) == 0 {
		t.Fatalf("trimmed seed = %+v", out.Events)
	}
	summary := out.Events[0].Blocks[0].Text
	for _, want := range []string{"LOAD COMPACT BASELINE", "LOAD OMITTED POST-COMPACT DECISION", "LOAD OMITTED POST-COMPACT RESULT"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("omitted-span digest lost %q:\n%s", want, summary)
		}
	}
	if strings.Contains(summary, "LOAD RECENT RAW REQUEST") || out.Events[1].Blocks[0].Text != "LOAD RECENT RAW REQUEST" {
		t.Fatalf("recent raw tail was duplicated or changed: %+v", out.Events)
	}
	if strings.Contains(summary, "[cxt conversation delta v1]") {
		t.Fatalf("private memory marker leaked into load prompt:\n%s", summary)
	}
}

func TestPrependTrimDigestDoesNotRepeatStoredCurrentConversation(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	id := domain.HashContent([]byte("load-current-delta-source"))
	full := domain.CIRDocument{Envelope: domain.Envelope{SourceProvider: domain.ProviderClaude}, Events: []domain.Event{
		{Kind: domain.EventMessage, Role: "user", CompactSummary: true, Blocks: []domain.ContentBlock{{Type: "text", Text: "LOAD STORED BASELINE"}}},
		{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "LOAD OLD OMITTED DECISION"}}},
		{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "LOAD CURRENT RAW DECISION ONCE"}}},
	}}
	digest, err := memory.NewRuleDistiller().Distill(ctx, full, nil)
	if err != nil {
		t.Fatal(err)
	}
	digest.SnapshotID = id
	digest = domain.MergeDigests(domain.MemoryDigest{}, digest)
	memoryHash, err := st.PutMemory(ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	snap := domain.Snapshot{ID: id, DocHash: id, MemoryHash: memoryHash}
	if err := st.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	omitted := full
	omitted.Events = append([]domain.Event(nil), full.Events[:2]...)
	seed := domain.CIRDocument{Events: append([]domain.Event(nil), full.Events[2:]...)}
	svc := NewLoadSessionService(st, nil, nil, nil, memory.NewRuleDistiller(), nil)

	out, err := svc.prependTrimDigest(ctx, omitted, seed, snap, domain.ProviderClaude, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var prompt strings.Builder
	for _, event := range out.Events {
		for _, block := range event.Blocks {
			prompt.WriteString(block.Text)
		}
	}
	for _, want := range []string{"LOAD STORED BASELINE", "LOAD OLD OMITTED DECISION", "LOAD CURRENT RAW DECISION ONCE"} {
		if !strings.Contains(prompt.String(), want) {
			t.Fatalf("load prompt lost %q:\n%s", want, prompt.String())
		}
	}
	if count := strings.Count(prompt.String(), "LOAD CURRENT RAW DECISION ONCE"); count != 1 {
		t.Fatalf("current raw conversation appears %d times, want one:\n%s", count, prompt.String())
	}
	if len(out.Events) < 2 || strings.Contains(out.Events[0].Blocks[0].Text, "LOAD CURRENT RAW DECISION ONCE") {
		t.Fatalf("stored current delta leaked into summary layer: %+v", out.Events)
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

	out, err := svc.prependTrimDigest(ctx, omitted, seed, snap, domain.ProviderCodex, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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

func TestInsertTrimDigestFailsClosedWhenReplacementFails(t *testing.T) {
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

	out, err := svc.insertTrimDigestAfterPrefix(ctx, omitted, seed, 2, domain.Snapshot{}, domain.ProviderCodex, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "distillation unavailable") {
		t.Fatalf("insert error=%v, want distillation failure", err)
	}
	if len(out.Events) != len(seed.Events) || out.Events[0].Blocks[0].Text != oldSeed {
		t.Fatalf("failed projection mutated the existing seed: %+v", out.Events)
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

	out, err := svc.insertTrimDigestAfterPrefix(ctx, omitted, seed, 2, domain.Snapshot{ID: snapshotID}, domain.ProviderCodex, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
	} {
		if !strings.Contains(seedText, want) {
			t.Fatalf("memoryless projection lost %q:\n%s", want, seedText)
		}
	}
	if strings.Contains(seedText, "Verify the memoryless replay path.") {
		t.Fatalf("memoryless recovery re-attested an unmarked legacy task:\n%s", seedText)
	}
	if out.Events[0].Locked == nil || out.Events[0].Locked.Blob != "PINNED" {
		t.Fatalf("memoryless projection changed provider state: %+v", out.Events)
	}
}

func TestSyntheticSeedTaskAuthorityRequiresVersionedHeading(t *testing.T) {
	legacy := "project memory\n\n" + seedLegacyTasksHeading + "\n- Stale legacy task."
	_, legacyTasks, legacyAuthority := syntheticSeedStructuredProjection(legacy)
	if legacyAuthority || len(legacyTasks) != 1 {
		t.Fatalf("legacy parse authority=%v tasks=%v", legacyAuthority, legacyTasks)
	}

	digest := domain.MemoryDigest{
		Summary:            "provider memory",
		OpenTasks:          []string{"Current attested task."},
		TasksAuthoritative: true,
	}
	rendered := renderSeedDigest(digest, 1, 8<<10)
	if !strings.Contains(rendered, seedAuthoritativeTasksHeading) {
		t.Fatalf("rendered authoritative seed lacks versioned heading:\n%s", rendered)
	}
	_, tasks, authority := syntheticSeedStructuredProjection(rendered)
	if !authority || len(tasks) != 1 || tasks[0] != "Current attested task." {
		t.Fatalf("rendered authority round trip=%v tasks=%v", authority, tasks)
	}

	empty := digest
	empty.OpenTasks = nil
	rendered = renderSeedDigest(empty, 1, 8<<10)
	_, tasks, authority = syntheticSeedStructuredProjection(rendered)
	if !authority || len(tasks) != 0 {
		t.Fatalf("empty authoritative tombstone round trip=%v tasks=%v\n%s", authority, tasks, rendered)
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

	out, err := svc.insertTrimDigestAfterPrefix(ctx, omitted, seed, 2, snap, domain.ProviderCodex, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
