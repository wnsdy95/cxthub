package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/agenttokens"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type agentTokenizerHostFixture struct {
	capability domain.AgentHostCapability
	calls      int
}

func (f *agentTokenizerHostFixture) AgentCapability(ctx context.Context, _ domain.ProviderKind, _ string) (domain.AgentHostCapability, error) {
	f.calls++
	return f.capability, ctx.Err()
}

func agentTokenizerCount(t testing.TB, counter outbound.AgentTokenCounter, in inbound.PrepareAgentContextInput, text string) domain.AgentTokenUsage {
	t.Helper()
	usage, err := counter.CountAgentTokens(context.Background(), in.Provider, in.Model, text)
	if err != nil {
		t.Fatal(err)
	}
	return usage
}

func agentTokenizerPrompt(t testing.TB, p domain.AgentContextPackage) string {
	t.Helper()
	prompt, err := p.Prompt()
	if err != nil {
		t.Fatal(err)
	}
	return prompt
}

func agentTokenizerHost(t testing.TB, counter outbound.AgentTokenCounter, in inbound.PrepareAgentContextInput, window int) *agentTokenizerHostFixture {
	t.Helper()
	probe := agentTokenizerCount(t, counter, in, "")
	if !probe.Exact || probe.Tokenizer == "" {
		t.Fatalf("known model has no exact tokenizer: %+v", probe)
	}
	return &agentTokenizerHostFixture{capability: domain.AgentHostCapability{
		Provider: in.Provider, Model: in.Model, Tokenizer: probe.Tokenizer,
		HostVersion: "test", Evidence: "synthetic fixture; not native host support",
		Verified: true, HostInputKnown: true, AutoCompactKnown: true,
		ContextWindow: window, HostInputTokens: 10000, FramingTokens: 10, ReservedTokens: 100,
	}}
}

func TestAgentContextTokenizerSelectsByFinalRenderedPrompt(t *testing.T) {
	for _, paged := range []bool{false, true} {
		t.Run(fmt.Sprintf("paged=%t", paged), func(t *testing.T) {
			s, in, h, memory, docs := agentServiceFixture(t)
			in.Model = "gpt-5.4"
			in.ArtifactOnly = true
			in.Policy = domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: 800000, Source: "explicit"}
			counter := agenttokens.New()
			s.tokens = counter
			text := "quoted \"\ud55c\uae00\" <&> \\path\nsecond line\t<|endoftext|>"
			var events []domain.Event
			for i := 0; i < 3; i++ {
				call := fmt.Sprintf("call-%d", i)
				events = append(events,
					agentMessage("user", fmt.Sprintf("turn-%d %s", i, text), 4*i),
					domain.Event{Kind: domain.EventToolCall, CallID: call, ToolName: "fixture", Input: map[string]any{"query": text}, Seq: 4*i + 1},
					domain.Event{Kind: domain.EventToolResult, CallID: call, Output: text, Seq: 4*i + 2},
					agentMessage("assistant", "complete "+text, 4*i+3))
			}
			doc := agentDocument(t, "tokenizer-history", events...)
			h.view.Snapshots[0].ID, h.view.Snapshots[0].DocHash = doc.Hash, doc.Hash
			h.view.Position, in.SnapshotID = doc.Hash, doc.Hash
			memory.items[0].SourceSnapshot = doc.Hash
			docs.docs = map[domain.ContentHash]domain.SessionDoc{doc.Hash: doc}
			original, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			pages := &agentPageFixture{docs: docs.docs}
			if paged {
				s.documents = pages
			}
			all, err := s.PrepareAgentContext(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if len(all.Content.History) != 3 {
				t.Fatalf("fixture selected %d turns; want 3", len(all.Content.History))
			}
			// Derive a boundary from the final JSON, including escaping, notice,
			// memory, source pointers and prompt framing. Raw text alone fits.
			latestTwo := all
			latestTwo.Content.History = all.Content.History[1:]
			boundary := agentTokenizerCount(t, counter, in, agentTokenizerPrompt(t, latestTwo))
			raw := agentTokenizerCount(t, counter, in, strings.Repeat(text, 12))
			if !boundary.Exact || raw.Tokens >= boundary.Tokens || all.Usage.Tokens <= boundary.Tokens {
				t.Fatalf("fixture does not distinguish raw text from final prompt: raw=%+v boundary=%+v all=%+v", raw, boundary, all.Usage)
			}
			in.ArtifactOnly = false
			host := agentTokenizerHost(t, counter, in, 1000000)
			s.capabilities = host
			for _, delta := range []int{0, -1} {
				t.Run(fmt.Sprintf("boundary%+d", delta), func(t *testing.T) {
					request := in
					request.Policy.BudgetTokens = boundary.Tokens + delta
					p, err := s.PrepareAgentContext(context.Background(), request)
					if err != nil {
						t.Fatal(err)
					}
					wantTurns := 2 + delta
					if len(p.Content.History) != wantTurns {
						t.Fatalf("selected %d turns; want %d at %d tokens", len(p.Content.History), wantTurns, request.Policy.BudgetTokens)
					}
					for i, segment := range p.Content.History {
						start := (3 - wantTurns + i) * 4
						if segment.Source.SnapshotID != doc.Hash || segment.Source.DocHash != doc.Hash || segment.Source.StartEvent != start || segment.Source.EndEvent != start+4 || !reflect.DeepEqual(segment.Events, events[start:start+4]) {
							t.Fatalf("changed chronology, complete turn/tool pair, or provenance: %+v", segment.Source)
						}
					}
					prompt := agentTokenizerPrompt(t, p)
					measured := agentTokenizerCount(t, counter, request, prompt)
					if p.Usage != measured || !p.Usage.Exact || p.Usage.Tokens > request.Policy.BudgetTokens || p.Usage.Tokens >= len(prompt) {
						t.Fatalf("incorrect final prompt accounting: receipt=%+v measured=%+v bytes=%d", p.Usage, measured, len(prompt))
					}
					if delta == 0 && (p.Usage != boundary || prompt != agentTokenizerPrompt(t, latestTwo)) {
						t.Fatal("exactly fitting final prompt was not preserved")
					}
					quoted, err := json.Marshal(p.Content.History[0].Events[0].Blocks[0].Text)
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(prompt, string(quoted)) || !strings.Contains(prompt, `\u003c`) || p.Content.Notice != all.Content.Notice || !reflect.DeepEqual(p.Content.Sources, all.Content.Sources) || !reflect.DeepEqual(p.Content.ProjectMemory, memory.items) {
						t.Fatal("lost escaped text, notice, memory or source overhead")
					}
					if p.Policy != request.Policy || p.Budget == nil || p.Budget.Tokenizer != boundary.Tokenizer || p.Capability != "verified_for_preparation" || p.Delivery != "prepared" {
						t.Fatal("wrong preparation receipt or claimed provider acceptance")
					}
					if err := p.ValidateIdentity(); err != nil {
						t.Fatal(err)
					}
				})
			}
			after, err := json.Marshal(docs.docs[doc.Hash])
			if err != nil || !bytes.Equal(original, after) {
				t.Fatal("selection changed the original archived document", err)
			}
			if host.calls != 4 || paged && (pages.calls == 0 || docs.calls != 0) || !paged && docs.calls == 0 {
				t.Fatalf("missing runtime recheck or wrong reader: host=%d pages=%d docs=%d", host.calls, pages.calls, docs.calls)
			}
		})
	}
}

func agentTokenizerLargeFixture(t testing.TB) (*AgentContextService, inbound.PrepareAgentContextInput, *agentDocFixture, domain.SessionDoc) {
	t.Helper()
	s, in, h, memory, docs := agentServiceFixture(t)
	in.Model = "gpt-5.4"
	in.Policy = domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: 800000, Source: "explicit"}
	counter := agenttokens.New()
	s.tokens = counter
	unit := " alpha beta gamma delta"
	unitUsage := agentTokenizerCount(t, counter, in, unit)
	if !unitUsage.Exact || unitUsage.Tokens <= 0 {
		t.Fatalf("cannot size synthetic history with real tokens: %+v", unitUsage)
	}
	payload := strings.Repeat(unit, 80000/unitUsage.Tokens+1)
	var events []domain.Event
	for i := 0; i < 3; i++ {
		events = append(events, agentMessage("user", fmt.Sprintf("turn-%d%s", i, payload), 2*i), agentMessage("assistant", "complete", 2*i+1))
	}
	doc := agentDocument(t, "large-tokenizer-history", events...)
	h.view.Snapshots[0].ID, h.view.Snapshots[0].DocHash = doc.Hash, doc.Hash
	h.view.Position, in.SnapshotID = doc.Hash, doc.Hash
	memory.items[0].SourceSnapshot = doc.Hash
	docs.docs = map[domain.ContentHash]domain.SessionDoc{doc.Hash: doc}
	return s, in, docs, doc
}

func TestAgentContextTokenizerAdaptiveBudget800K(t *testing.T) {
	for _, paged := range []bool{false, true} {
		t.Run(fmt.Sprintf("paged=%t", paged), func(t *testing.T) {
			s, in, docs, doc := agentTokenizerLargeFixture(t)
			if paged {
				s.documents = &agentPageFixture{docs: docs.docs}
			}
			in.ArtifactOnly = true
			all, err := s.PrepareAgentContext(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if len(all.Content.History) != 3 || !all.Usage.Exact || all.Usage.Tokens <= 200000 || all.Usage.Tokens > in.Policy.BudgetTokens {
				t.Fatalf("expected >200k real tokens in an 800k artifact: turns=%d usage=%+v", len(all.Content.History), all.Usage)
			}
			in.ArtifactOnly = false
			host := agentTokenizerHost(t, s.tokens, in, 200000)
			s.capabilities = host
			p, err := s.PrepareAgentContext(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if p.Policy != in.Policy || p.Budget == nil || p.Budget.RequestedTokens != 800000 || p.Budget.InitialInputLimit != 160000 || p.Budget.EffectiveTokens != 149990 || p.Budget.AdjustmentReason != "model_window_80_percent" || p.Budget.Tokenizer != host.capability.Tokenizer {
				t.Fatalf("incorrect adaptive accounting: policy=%+v budget=%+v", p.Policy, p.Budget)
			}
			if len(p.Content.History) != 1 || p.Content.History[0].Source.StartEvent != 4 || p.Content.History[0].Source.EndEvent != 6 || !reflect.DeepEqual(p.Content.History[0].Events, doc.CIR.Events[4:]) {
				t.Fatal("adaptive budget did not retain exactly the newest complete turn")
			}
			if p.Usage != agentTokenizerCount(t, s.tokens, in, agentTokenizerPrompt(t, p)) || p.Usage.Tokens > p.Budget.EffectiveTokens || host.calls != 2 {
				t.Fatal("final prompt overflow, incorrect count or missing runtime recheck")
			}
			if err := p.ValidateIdentity(); err != nil {
				t.Fatal(err)
			}
			if err := domain.ValidateSessionDocHash(docs.docs[doc.Hash]); err != nil {
				t.Fatal("changed archived source", err)
			}
		})
	}
}

func TestAgentContextTokenizerArtifactsWithoutNativeHost(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider domain.ProviderKind
		model    string
		exact    bool
	}{
		{"known Codex", domain.ProviderCodex, "gpt-5.4", true},
		{"unknown Codex", domain.ProviderCodex, "unknown-fixture-model", false},
		{"Claude", domain.ProviderClaude, "gpt-5.4", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, in, _, memory, _ := agentServiceFixture(t)
			in.Provider, in.Model, in.ArtifactOnly = tc.provider, tc.model, true
			in.Policy = domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: 800000, Source: "explicit"}
			memory.items[0].Text = "Keep \"\ud55c\uae00\" <&> and emoji 😀 intact.\nNext line."
			s.tokens = agenttokens.New()
			probe := agentTokenizerCount(t, s.tokens, in, "")
			p, err := s.PrepareAgentContext(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			prompt := agentTokenizerPrompt(t, p)
			if p.Usage != agentTokenizerCount(t, s.tokens, in, prompt) || p.Usage.Exact != tc.exact || p.Usage.Tokenizer != probe.Tokenizer || p.Usage.Tokenizer == "" {
				t.Fatalf("wrong model counter: probe=%+v usage=%+v", probe, p.Usage)
			}
			if !tc.exact && p.Usage.Tokens != len([]byte(prompt)) {
				t.Fatalf("fallback did not count UTF-8 bytes: %+v bytes=%d", p.Usage, len([]byte(prompt)))
			}
			if !p.ArtifactOnly || p.Budget != nil || p.Capability != "unverified_artifact_only" || p.Delivery != "prepared" || len(p.Content.History) != 1 {
				t.Fatal("artifact invented native host accounting or acceptance")
			}
			raw, err := p.Artifact()
			if err != nil {
				t.Fatal(err)
			}
			var restored domain.AgentContextPackage
			if err := json.Unmarshal(raw, &restored); err != nil {
				t.Fatal(err)
			}
			if err := restored.ValidateIdentity(); err != nil {
				t.Fatal(err)
			}
			if restored.Usage != p.Usage || !restored.ArtifactOnly || agentTokenizerPrompt(t, restored) != prompt {
				t.Fatal("artifact round trip changed accounted prompt or delivery restriction")
			}
			load := NewLoadSessionService(nil, nil, nil, nil, nil, nil)
			if _, err := load.materializeAgentPackage(context.Background(), in, restored); !errors.Is(err, domain.ErrProviderCapabilityUnknown) {
				t.Fatalf("artifact became launchable: %v", err)
			}
		})
	}
}

func TestAgentContextTokenizerExactTextDoesNotAuthorizeUnknownHost(t *testing.T) {
	for _, kind := range []string{"no host", "unknown host input", "unverified host"} {
		t.Run(kind, func(t *testing.T) {
			s, in, history, memory, docs := agentServiceFixture(t)
			in.Model = "gpt-5.4"
			in.Policy = domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: 800000, Source: "explicit"}
			s.tokens = agenttokens.New()
			known := agentTokenizerCount(t, s.tokens, in, "Known exact text \ud55c\uae00")
			if !known.Exact || known.Tokens <= 0 {
				t.Fatalf("fixture did not use an exact counter: %+v", known)
			}
			host := agentTokenizerHost(t, s.tokens, in, 1000000)
			switch kind {
			case "unknown host input":
				host.capability.HostInputKnown = false
				s.capabilities = host
			case "unverified host":
				host.capability.Verified = false
				s.capabilities = host
			}
			mat := &agentMaterializerFixture{}
			load := NewLoadSessionService(nil, map[domain.ProviderKind]outbound.ProviderCodec{in.Provider: codec.NewCodexCodec()}, map[domain.ProviderKind]outbound.SessionMaterializer{in.Provider: mat}, nil, nil, nil).WithAgentContext(s).WithAgentCodePosition(agentCodeFixture{})
			in.WorkingPosition = &domain.AgentWorkingPosition{Branch: "feature", CodeCommit: strings.Repeat("a", 40)}
			in.WorktreeStateHash = agentHash("worktree")
			_, out, err := load.PrepareAgentDelivery(context.Background(), in)
			if !errors.Is(err, domain.ErrProviderCapabilityUnknown) {
				t.Fatalf("exact text authorized an unknown host: %v", err)
			}
			if history.calls != 0 || memory.calls != 0 || docs.calls != 0 || mat.calls != 0 || out.WrittenPath != "" || out.ResumeCmd != "" {
				t.Fatal("unknown host read cloud context or materialized a native session")
			}
		})
	}
}

func BenchmarkAgentContextTokenizerArtifact800K(b *testing.B) {
	s, in, docs, _ := agentTokenizerLargeFixture(b)
	in.ArtifactOnly = true
	s.documents = &agentPageFixture{docs: docs.docs}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Include tokenization rather than reusing prior iterations' prompt cache.
		s.tokens = agenttokens.New()
		p, err := s.PrepareAgentContext(context.Background(), in)
		if err != nil {
			b.Fatal(err)
		}
		if !p.Usage.Exact || p.Usage.Tokens <= 200000 || p.Usage.Tokens > in.Policy.BudgetTokens {
			b.Fatalf("incorrect large artifact accounting: %+v", p.Usage)
		}
	}
}
