package app

import (
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

func TestAgentInitialPromptReservesCapacityBeforeSelectingRecentTurns(t *testing.T) {
	for _, provider := range []domain.ProviderKind{domain.ProviderCodex, domain.ProviderClaude} {
		for _, paged := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/paged=%t", provider, paged), func(t *testing.T) {
				s, in, _, docs, doc := adaptiveHistoryFixture(t)
				in.Provider = provider
				s.capabilities = &adaptiveCapabilityFixture{windows: []int{400000}}
				if paged {
					s.documents = &agentPageFixture{docs: docs.docs}
				}
				without, err := s.PrepareAgentContext(context.Background(), in)
				if err != nil || len(without.Content.History) != 3 {
					t.Fatalf("fixture must fit all three turns before reserving the task: %v", err)
				}
				for i, segment := range without.Content.History {
					if !reflect.DeepEqual(segment.Events, doc.CIR.Events[i*2:i*2+2]) {
						t.Fatal("fixture did not preserve complete ordered turns")
					}
				}
				const marker = "PRIVATE INITIAL TASK "
				literal := marker + strings.Repeat("t", 100000-len(marker))
				in.InitialPrompt = domain.NewAgentInitialPrompt(literal)
				p, err := s.PrepareAgentContext(context.Background(), in)
				if err != nil {
					t.Fatal(err)
				}
				if p.Budget.InitialPromptTokens != 100000 || p.Budget.EffectiveTokens != 209990 || p.Policy.BudgetTokens != 800000 {
					t.Fatalf("first task not deducted exactly once: %+v", p.Budget)
				}
				if len(p.Content.History) != 2 || !reflect.DeepEqual(p.Content.History[0].Events, doc.CIR.Events[2:4]) || !reflect.DeepEqual(p.Content.History[1].Events, doc.CIR.Events[4:]) || p.Content.History[0].Source.StartEvent != 2 || p.Content.History[1].Source.EndEvent != 6 {
					t.Fatal("reservation did not remove the oldest complete turn")
				}
				prompt, err := p.Prompt()
				if err != nil || p.Usage.Tokens != len(prompt) || p.Usage.Tokens+p.Budget.InitialPromptTokens+p.Budget.HostInputTokens+p.Budget.FramingTokens > p.Budget.InitialInputLimit {
					t.Fatal("package accounting includes the task or exceeds the initial limit")
				}
				if err = p.ValidateInitialPrompt(in.InitialPrompt); err != nil {
					t.Fatal(err)
				}
				for _, value := range []any{in, p, &p, struct{ Package domain.AgentContextPackage }{p}} {
					raw, err := json.Marshal(value)
					if err != nil || strings.Contains(string(raw), marker) || strings.Contains(fmt.Sprintf("%+v", value), marker) || strings.Contains(fmt.Sprintf("%#v", value), marker) {
						t.Fatal("private initial task entered a receipt or routine formatting")
					}
				}
				if in.InitialPrompt.Text() != literal || !reflect.DeepEqual(docs.docs[doc.Hash], doc) {
					t.Fatal("modified user task or archived conversation")
				}
			})
		}
	}
}

type initialPromptCounter struct {
	prompt string
	models []string
	alter  func(string, *domain.AgentTokenUsage)
}

func (f *initialPromptCounter) CountAgentTokens(ctx context.Context, provider domain.ProviderKind, model, text string) (domain.AgentTokenUsage, error) {
	u, err := (agentTokenFixture{}).CountAgentTokens(ctx, provider, model, text)
	if text == f.prompt {
		f.models = append(f.models, model)
		f.alter(model, &u)
	}
	return u, err
}

func TestAgentInitialPromptInvalidCountingStopsBeforeCloudRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(string, *domain.AgentTokenUsage)
		want  error
	}{
		{"fallback", func(_ string, u *domain.AgentTokenUsage) { u.Exact = false }, domain.ErrProviderCapabilityUnknown},
		{"other tokenizer", func(_ string, u *domain.AgentTokenUsage) { u.Tokenizer = "other" }, domain.ErrProviderCapabilityUnknown},
		{"zero nonempty", func(_ string, u *domain.AgentTokenUsage) { u.Tokens = 0 }, domain.ErrContextBudgetExceeded},
		{"negative", func(_ string, u *domain.AgentTokenUsage) { u.Tokens = -1 }, domain.ErrContextBudgetExceeded},
		{"all available space", func(_ string, u *domain.AgentTokenUsage) { u.Tokens = 149990 }, domain.ErrContextBudgetExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, in, h, docs, _ := adaptiveHistoryFixture(t)
			in.InitialPrompt = domain.NewAgentInitialPrompt("private task")
			s.capabilities = &adaptiveCapabilityFixture{windows: []int{200000}}
			s.tokens = &initialPromptCounter{prompt: in.InitialPrompt.Text(), alter: tc.alter}
			_, err := s.PrepareAgentContext(context.Background(), in)
			if !errors.Is(err, tc.want) || h.calls != 0 || docs.calls != 0 {
				t.Fatalf("unsafe count continued: err=%v cloud=%d docs=%d", err, h.calls, docs.calls)
			}
		})
	}
}

func TestAgentInitialPromptRecountedForChangedResolvedModel(t *testing.T) {
	s, in, _, _, _ := adaptiveHistoryFixture(t)
	in.Model = ""
	in.InitialPrompt = domain.NewAgentInitialPrompt("model-dependent private task")
	cap := &adaptiveCapabilityFixture{windows: []int{400000}, models: []string{"default-a", "default-b"}}
	s.capabilities = cap
	counter := &initialPromptCounter{prompt: in.InitialPrompt.Text(), alter: func(model string, u *domain.AgentTokenUsage) {
		u.Tokens = 100
		if model == "default-b" {
			u.Tokens = 200
		}
	}}
	s.tokens = counter
	p, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if cap.calls != 4 || p.Budget.Model != "default-b" || p.Budget.InitialPromptTokens != 200 || p.Budget.EffectiveTokens != 309790 || !reflect.DeepEqual(counter.models, []string{"default-a", "default-b", "default-b", "default-b"}) {
		t.Fatalf("stale prompt reservation: budget=%+v models=%v", p.Budget, counter.models)
	}
	if err = p.ValidateInitialPrompt(in.InitialPrompt); err != nil {
		t.Fatal(err)
	}
}

func TestAgentInitialPromptUsesWholeTextOfflineCounter(t *testing.T) {
	s, in, _, _, _ := agentServiceFixture(t)
	in.Policy = domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: 800000, Source: "explicit"}
	in.Model = "gpt-5.4"
	counter := agenttokens.New()
	s.tokens = counter
	s.capabilities = agentTokenizerHost(t, counter, in, 200000)
	in.InitialPrompt = domain.NewAgentInitialPrompt("  private \"\ud55c\uae00\" task\n<|endoftext|> & <scope> ")
	count := agentTokenizerCount(t, counter, in, in.InitialPrompt.Text())
	p, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !count.Exact || p.Budget.InitialPromptTokens != count.Tokens || p.Budget.EffectiveTokens != 149990-count.Tokens || p.Budget.Tokenizer != count.Tokenizer {
		t.Fatal("literal task did not use the resolved offline encoding")
	}
	if err = p.ValidateInitialPrompt(in.InitialPrompt); err != nil {
		t.Fatal(err)
	}
}

func TestAgentInitialPromptBoundBeforeMaterialization(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*domain.AgentContextPackage, *inbound.PrepareAgentContextInput)
		valid  bool
	}{
		{"original", func(*domain.AgentContextPackage, *inbound.PrepareAgentContextInput) {}, true},
		{"same count substitution", func(_ *domain.AgentContextPackage, in *inbound.PrepareAgentContextInput) {
			in.InitialPrompt = domain.NewAgentInitialPrompt("private task B")
		}, false},
		{"removed task", func(_ *domain.AgentContextPackage, in *inbound.PrepareAgentContextInput) {
			in.InitialPrompt = domain.AgentInitialPrompt{}
		}, false},
		{"serialized receipt", func(p *domain.AgentContextPackage, _ *inbound.PrepareAgentContextInput) {
			raw, _ := json.Marshal(p)
			*p = domain.AgentContextPackage{}
			_ = json.Unmarshal(raw, p)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, in, _, _, _ := adaptiveHistoryFixture(t)
			in.LatestMain = true
			in.WorkingPosition = &domain.AgentWorkingPosition{Branch: "feature", CodeCommit: strings.Repeat("a", 40)}
			in.WorktreeStateHash = agentHash("worktree")
			in.InitialPrompt = domain.NewAgentInitialPrompt("private task A")
			s.capabilities = &adaptiveCapabilityFixture{windows: []int{200000}}
			p, err := s.PrepareAgentContext(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(&p, &in)
			mat := &agentMaterializerFixture{}
			loader := NewLoadSessionService(nil, map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderCodex: codec.NewCodexCodec()}, map[domain.ProviderKind]outbound.SessionMaterializer{domain.ProviderCodex: mat}, nil, nil, nil).WithAgentContext(&agentPackageFixture{}).WithAgentCodePosition(agentCodeFixture{})
			_, err = loader.materializeAgentPackage(context.Background(), in, p)
			if tc.valid {
				if err != nil || mat.calls != 1 {
					t.Fatalf("original rejected: %v / %d", err, mat.calls)
				}
			} else if err == nil || mat.calls != 0 {
				t.Fatalf("unbound question materialized: %v / %d", err, mat.calls)
			}
		})
	}
}
