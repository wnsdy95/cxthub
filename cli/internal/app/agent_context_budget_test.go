package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type adaptiveCapabilityFixture struct {
	windows         []int
	models          []string
	requestedModels []string
	calls           int
	unknownHost     bool
}

func (f *adaptiveCapabilityFixture) AgentCapability(ctx context.Context, provider domain.ProviderKind, model string) (domain.AgentHostCapability, error) {
	window := f.windows[min(f.calls, len(f.windows)-1)]
	f.requestedModels = append(f.requestedModels, model)
	if len(f.models) > 0 {
		model = f.models[min(f.calls, len(f.models)-1)]
	} else if model == "" {
		model = "resolved-fixture"
	}
	f.calls++
	c, err := (agentCapabilityFixture{window: window}).AgentCapability(ctx, provider, model)
	c.HostInputTokens = 10000
	if f.unknownHost {
		c.HostInputKnown = false
	}
	return c, err
}

func adaptiveHistoryFixture(t *testing.T) (*AgentContextService, inbound.PrepareAgentContextInput, *agentHistoryFixture, *agentDocFixture, domain.SessionDoc) {
	t.Helper()
	s, in, h, _, docs := agentServiceFixture(t)
	doc := agentDocument(t, "shared-session", agentMessage("user", "old "+strings.Repeat("o", 75000), 0), agentMessage("assistant", "old answer", 1),
		agentMessage("user", "middle "+strings.Repeat("m", 75000), 2), agentMessage("assistant", "middle answer", 3),
		agentMessage("user", "newest "+strings.Repeat("n", 75000), 4), agentMessage("assistant", "newest answer", 5))
	h.view.Snapshots[0].ID, h.view.Snapshots[0].DocHash = doc.Hash, doc.Hash
	h.view.Position, in.SnapshotID = doc.Hash, doc.Hash
	docs.docs = map[domain.ContentHash]domain.SessionDoc{doc.Hash: doc}
	in.Policy = domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: 800000, Source: "explicit"}
	s.tokens = agentTokenFixture{}
	return s, in, h, docs, doc
}

func TestAgentContextAdaptiveBudgetSelectsRecentCompleteTurns(t *testing.T) {
	for _, provider := range []domain.ProviderKind{domain.ProviderCodex, domain.ProviderClaude} {
		for _, paged := range []bool{false, true} {
			t.Run(string(provider)+"/paged="+map[bool]string{false: "false", true: "true"}[paged], func(t *testing.T) {
				s, in, _, docs, doc := adaptiveHistoryFixture(t)
				in.Provider = provider
				cap := &adaptiveCapabilityFixture{windows: []int{200000}}
				s.capabilities = cap
				if paged {
					s.documents = &agentPageFixture{docs: docs.docs}
				}
				p, err := s.PrepareAgentContext(context.Background(), in)
				if err != nil {
					t.Fatal(err)
				}
				if p.Policy != in.Policy || p.Budget == nil || p.Budget.RequestedTokens != 800000 || p.Budget.EffectiveTokens != 149990 || p.Budget.InitialInputLimit != 160000 || p.Budget.AdjustmentReason != "model_window_80_percent" {
					t.Fatalf("request/accounting changed: policy=%+v budget=%+v", p.Policy, p.Budget)
				}
				// With 10k of host input, two 75k turns cannot fit. Preserve the
				// newest complete user+answer pair, not fragments of the prior turn.
				if len(p.Content.History) != 1 || len(p.Content.History[0].Events) != 2 || p.Content.History[0].Source.StartEvent != 4 || p.Content.History[0].Source.EndEvent != 6 || !reflect.DeepEqual(p.Content.History[0].Events, doc.CIR.Events[4:]) {
					t.Fatal("did not select exactly the newest complete turn")
				}
				if p.Usage.Tokens > p.Budget.EffectiveTokens || len(p.Content.ProjectMemory) != 1 || cap.calls != 2 {
					t.Fatal("overflow, lost memory or missing runtime recheck")
				}
				if err = p.ValidateIdentity(); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(docs.docs[doc.Hash], doc) {
					t.Fatal("changed archived source")
				}
				if p.Capability != "verified_for_preparation" || p.Delivery != "prepared" {
					t.Fatal("claimed model acceptance")
				}
			})
		}
	}
}

func TestAgentContextAdaptiveBudgetDoesNotTruncateRequiredConditions(t *testing.T) {
	s, in, _, docs, _ := adaptiveHistoryFixture(t)
	s.capabilities = &adaptiveCapabilityFixture{windows: []int{200000}}
	in.PersonalScope = domain.PersonalWorkScope{ActorID: "alice", SessionID: "s", WorktreeID: "w"}
	condition := "mandatory " + strings.Repeat("c", 170000)
	state := domain.PersonalWorkState{Scope: in.PersonalScope, Sources: []domain.AgentSourcePointer{{SnapshotID: in.SnapshotID}}, Constraints: []domain.ExactUserConstraint{{Text: condition, Source: domain.AgentSourcePointer{SnapshotID: in.SnapshotID}}}}
	s.work = agentWorkFixture{state: state}
	if _, err := s.PrepareAgentContext(context.Background(), in); !errors.Is(err, domain.ErrContextBudgetExceeded) {
		t.Fatal(err)
	}
	if docs.calls != 0 || state.Constraints[0].Text != condition {
		t.Fatal("read history or truncated required condition")
	}
}

func TestAgentContextAdaptiveBudgetReselectsWhenVerifiedWindowChanges(t *testing.T) {
	s, in, _, _, _ := adaptiveHistoryFixture(t)
	cap := &adaptiveCapabilityFixture{windows: []int{1000000, 200000}}
	s.capabilities = cap
	p, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if cap.calls != 4 || p.Budget.ContextWindow != 200000 || len(p.Content.History) != 1 || p.Policy.BudgetTokens != 800000 {
		t.Fatal("did not discard the first large selection")
	}
	// Churning limits must not publish a stale package or loop unboundedly.
	s, in, _, _, _ = adaptiveHistoryFixture(t)
	cap = &adaptiveCapabilityFixture{windows: []int{1000000, 200000, 1000000, 200000, 1000000, 200000}}
	s.capabilities = cap
	if _, err = s.PrepareAgentContext(context.Background(), in); !errors.Is(err, domain.ErrProviderCapabilityUnknown) || cap.calls != 6 {
		t.Fatalf("churn: calls=%d err=%v", cap.calls, err)
	}
}

func TestAgentContextArtifactsDoNotInventRuntimeWindow(t *testing.T) {
	s, in, _, _, _ := adaptiveHistoryFixture(t)
	in.ArtifactOnly = true
	p, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if p.Budget != nil || p.Policy.BudgetTokens != 800000 || p.Capability != "unverified_artifact_only" {
		t.Fatal("artifact invented runtime accounting")
	}
}

func TestAgentContextUnknownHostInputStopsBeforeCloudRead(t *testing.T) {
	s, in, h, docs, _ := adaptiveHistoryFixture(t)
	s.capabilities = &adaptiveCapabilityFixture{windows: []int{1000000}, unknownHost: true}
	if _, err := s.PrepareAgentContext(context.Background(), in); !errors.Is(err, domain.ErrProviderCapabilityUnknown) {
		t.Fatal(err)
	}
	if h.calls != 0 || docs.calls != 0 {
		t.Fatal("read server bodies before verified host input")
	}
}

type changingAgentTokenizer struct{ calls int }

func (f *changingAgentTokenizer) CountAgentTokens(ctx context.Context, p domain.ProviderKind, model, prompt string) (domain.AgentTokenUsage, error) {
	f.calls++
	u, err := (agentTokenFixture{}).CountAgentTokens(ctx, p, model, prompt)
	if f.calls == 2 {
		u.Tokenizer = "different-counter"
	}
	return u, err
}

func TestAgentContextCounterChangeStopsSelection(t *testing.T) {
	s, in, _, docs, _ := adaptiveHistoryFixture(t)
	s.capabilities = &adaptiveCapabilityFixture{windows: []int{200000}}
	counter := &changingAgentTokenizer{}
	s.tokens = counter
	if _, err := s.PrepareAgentContext(context.Background(), in); !errors.Is(err, domain.ErrProviderCapabilityUnknown) {
		t.Fatal(err)
	}
	if counter.calls != 2 || docs.calls != 0 {
		t.Fatal("continued reading with a changed tokenizer")
	}
}

type bindingAgentCounter struct{ models []string }

func (f *bindingAgentCounter) CountAgentTokens(ctx context.Context, p domain.ProviderKind, model, prompt string) (domain.AgentTokenUsage, error) {
	f.models = append(f.models, model)
	return (agentTokenFixture{}).CountAgentTokens(ctx, p, model, prompt)
}

func TestAgentContextOmittedModelBindsVerifiedRuntimeDefault(t *testing.T) {
	s, in, _, _, _ := adaptiveHistoryFixture(t)
	in.Model = ""
	cap := &adaptiveCapabilityFixture{windows: []int{200000}}
	s.capabilities = cap
	counter := &bindingAgentCounter{}
	s.tokens = counter
	p, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if in.Model != "" || p.Budget.Model != "resolved-fixture" || !reflect.DeepEqual(cap.requestedModels, []string{"", ""}) {
		t.Fatal("changed caller request or failed default rediscovery")
	}
	for _, model := range counter.models {
		if model != "resolved-fixture" {
			t.Fatalf("counted with unresolved model %q", model)
		}
	}
}

func TestAgentContextDefaultModelChangeReselectsAndExplicitModelStaysPinned(t *testing.T) {
	s, in, _, _, _ := adaptiveHistoryFixture(t)
	in.Model = ""
	cap := &adaptiveCapabilityFixture{windows: []int{200000}, models: []string{"default-a", "default-b"}}
	s.capabilities = cap
	p, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil || cap.calls != 4 || p.Budget.Model != "default-b" {
		t.Fatalf("default change: calls=%d budget=%+v err=%v", cap.calls, p.Budget, err)
	}
	s, in, h, docs, _ := adaptiveHistoryFixture(t)
	cap = &adaptiveCapabilityFixture{windows: []int{200000}, models: []string{"other"}}
	s.capabilities = cap
	if _, err = s.PrepareAgentContext(context.Background(), in); !errors.Is(err, domain.ErrProviderCapabilityUnknown) || h.calls != 0 || docs.calls != 0 {
		t.Fatalf("explicit model silently changed: err=%v", err)
	}
}
