package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/agenttokens"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func nativeEstimateAppCapability() domain.AgentHostCapability {
	return domain.AgentHostCapability{Provider: domain.ProviderClaude, Model: "synthetic-native-model", HostVersion: "2.1.287",
		Verified: true, Evidence: "owned synthetic summary", ContextWindow: 200000, Tokenizer: domain.UTF8ByteBoundCounter,
		InputAccountingPolicy: domain.NativeEstimateReserveV1, RuntimeScope: agentHash("owned ephemeral session"),
		BaselineInputEstimateTokens: 5000, BaselineInputMeasurement: domain.NativeLocalEstimate, FramingAllowanceTokens: 49}
}

func nativeEstimateAppUsage(text string) domain.AgentTokenUsage {
	return domain.AgentTokenUsage{Tokens: len(text), Tokenizer: domain.UTF8ByteBoundCounter, Scope: "text", Reason: "model_tokenizer_unavailable"}
}

func TestNativeEstimatePreparationUsesLatestMainAndRealAllowance(t *testing.T) {
	for _, paged := range []bool{false, true} {
		t.Run(map[bool]string{false: "full", true: "paged"}[paged], func(t *testing.T) {
			s, in, h, docs, original := adaptiveHistoryFixture(t)
			in.Provider, in.Model, in.LatestMain = domain.ProviderClaude, "", true
			in.InitialPrompt = domain.NewAgentInitialPrompt("\uacc4\uc18d\ud574 👋")
			in.WorkingPosition = &domain.AgentWorkingPosition{Branch: "feature", CodeCommit: strings.Repeat("b", 40)}
			in.WorktreeStateHash = agentHash("worktree")
			c := nativeEstimateAppCapability()
			store := &calibrationFixture{fail: errors.New("calibration must not be accessed")}
			s.capabilities = MeasuredAgentCapabilities{Runtime: measuredRuntimeFixture{c}, Observations: store}
			s.tokens = agenttokens.New() // Current real Claude path, no model calls.
			if paged {
				s.documents = &agentPageFixture{docs: docs.docs}
			}
			s.history = retryHistoryFunc(func(ctx context.Context, q inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
				if !q.ServerTip || q.Branch != "main" || q.Position != "" {
					t.Fatal("wrong source query")
				}
				return h.QueryHistory(ctx, q)
			})
			p, err := s.PrepareAgentContext(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			prompt, err := p.Prompt()
			if err != nil || p.Usage != nativeEstimateAppUsage(prompt) || p.Budget.InputAccountingPolicy != domain.NativeEstimateReserveV1 || p.Budget.BaselineInputEstimateTokens != c.BaselineInputEstimateTokens || p.Budget.FramingAllowanceTokens != 49 || p.Budget.InitialPromptTokens != len(in.InitialPrompt.Text()) {
				t.Fatal("lost declared allowance/estimate accounting", err)
			}
			if p.Usage.Tokens > p.EffectiveBudget() || p.Policy.BudgetTokens != 800000 || !p.Budget.HostInputUnverified || p.Budget.HostInputTokens != 0 || p.Budget.FramingTokens != 0 || p.Budget.ObservedOverheadTokens != 0 {
				t.Fatal("false exact accounting or changed requested budget")
			}
			if len(p.Content.ProjectMemory) != 1 || len(p.Content.History) != 1 || !reflect.DeepEqual(p.Content.History[0].Events, original.CIR.Events[4:]) || !reflect.DeepEqual(docs.docs[original.Hash], original) {
				t.Fatal("lost memory, split recent turn, or changed archive")
			}
			if p.Content.Selection.SourcePolicy != domain.AgentSourceLatestMain || p.Content.Selection.DeliveryCodeCommit() != in.WorkingPosition.CodeCommit || p.Content.Selection.SnapshotID != h.view.Position {
				t.Fatal("lost source/working-position identity")
			}
			if err := p.ValidateIdentity(); err != nil {
				t.Fatal(err)
			}
			if err := p.ValidateInitialPrompt(in.InitialPrompt); err != nil {
				t.Fatal(err)
			}
			raw, err := p.Artifact()
			if err != nil {
				t.Fatal(err)
			}
			var decoded domain.AgentContextPackage
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			if err := decoded.ValidateIdentity(); err != nil {
				t.Fatal("receipt integrity lost", err)
			}
			if err := decoded.ValidateInitialPrompt(in.InitialPrompt); err == nil {
				t.Fatal("receipt reconstituted launch authority")
			}
			decoded.Usage.Tokens--
			decoded.ID, err = decoded.Digest()
			if err != nil {
				t.Fatal(err)
			}
			if err := decoded.ValidateIdentity(); !errors.Is(err, domain.ErrHashMismatch) {
				t.Fatal("redigested receipt accepted an underreported allowance", err)
			}
			if _, retry, err := RecordAgentInputObservation(context.Background(), store, p, domain.AgentInputObservation{InitialRequest: true, SubmittedTextExact: true, UsageKnown: true}); !errors.Is(err, domain.ErrProviderCapabilityUnknown) || retry {
				t.Fatal("estimate entered calibration", err)
			}
			if store.calls != 0 || store.writes != 0 {
				t.Fatal("ephemeral policy accessed persistent calibration")
			}
			if err := s.ValidateLatestMain(context.Background(), in.Cwd, p.Content.Selection); err != nil {
				t.Fatal(err)
			}
			h.after = func(v *domain.HistoryQueryResult) { v.Selection.CodeCommit = strings.Repeat("c", 40) }
			if err := s.ValidateLatestMain(context.Background(), in.Cwd, p.Content.Selection); err == nil {
				t.Fatal("main drift bypassed source gate")
			}
		})
	}
}

func TestNativeEstimatePreparationRejectsUnderreportedPackage(t *testing.T) {
	s, in, _, docs, _ := adaptiveHistoryFixture(t)
	c := nativeEstimateAppCapability()
	in.Provider, in.Model = domain.ProviderClaude, c.Model
	in.InitialPrompt = domain.NewAgentInitialPrompt("question")
	s.capabilities = MeasuredAgentCapabilities{Runtime: measuredRuntimeFixture{c}}
	candidateCalls := 0
	s.tokens = agentMeasurementCounter(func(text string) (domain.AgentTokenUsage, error) {
		u := nativeEstimateAppUsage(text)
		if text != in.InitialPrompt.Text() {
			candidateCalls++
			u.Tokens--
		}
		return u, nil
	})
	if _, err := s.PrepareAgentContext(context.Background(), in); !errors.Is(err, domain.ErrHashMismatch) || agentCandidateLimit(err) || candidateCalls != 1 || docs.calls != 0 {
		t.Fatal("underreporting caused candidate reselection or history reads", candidateCalls, docs.calls, err)
	}
}

func TestNativeEstimatePreparationRejectsAccountingDrift(t *testing.T) {
	for _, phase := range []string{"question", "candidate", "question-recheck"} {
		for _, mutation := range []string{"reason", "scope", "exact", "counter"} {
			t.Run(phase+"/"+mutation, func(t *testing.T) {
				s, in, _, _, _ := adaptiveHistoryFixture(t)
				c := nativeEstimateAppCapability()
				in.Provider, in.Model = domain.ProviderClaude, c.Model
				in.InitialPrompt = domain.NewAgentInitialPrompt("unique question")
				s.capabilities = MeasuredAgentCapabilities{Runtime: measuredRuntimeFixture{c}}
				questionCalls := 0
				s.tokens = agentMeasurementCounter(func(text string) (domain.AgentTokenUsage, error) {
					u := nativeEstimateAppUsage(text)
					question := text == in.InitialPrompt.Text()
					if question {
						questionCalls++
					}
					if (phase == "question" && question) || (phase == "candidate" && !question) || (phase == "question-recheck" && question && questionCalls > 1) {
						switch mutation {
						case "reason":
							u.Reason = "tokenizer_work_limit"
						case "scope":
							u.Scope = "request"
						case "exact":
							u.Exact = true
						case "counter":
							u.Tokenizer = "other"
						}
					}
					return u, nil
				})
				if _, err := s.PrepareAgentContext(context.Background(), in); !errors.Is(err, domain.ErrProviderCapabilityUnknown) {
					t.Fatal("counter provenance drift accepted", err)
				}
			})
		}
	}
}

type nativeEstimateChangingRuntime struct {
	c     domain.AgentHostCapability
	calls int
}

func (f *nativeEstimateChangingRuntime) AgentCapability(context.Context, domain.ProviderKind, string) (domain.AgentHostCapability, error) {
	f.calls++
	c := f.c
	if f.calls > 1 {
		c.BaselineInputEstimateTokens = 80000
	}
	return c, nil
}

func TestNativeEstimatePreparationReselectsChangedBaseline(t *testing.T) {
	s, in, _, _, _ := adaptiveHistoryFixture(t)
	c := nativeEstimateAppCapability()
	c.ContextWindow, c.BaselineInputEstimateTokens = 400000, 0
	in.Provider, in.Model = domain.ProviderClaude, c.Model
	runtime := &nativeEstimateChangingRuntime{c: c}
	store := &calibrationFixture{fail: errors.New("unexpected calibration read")}
	s.capabilities = MeasuredAgentCapabilities{Runtime: runtime, Observations: store}
	s.tokens = agenttokens.New()
	p, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil || runtime.calls != 4 || p.Budget.BaselineInputEstimateTokens != 80000 || len(p.Content.History) != 2 || store.calls != 0 {
		t.Fatal("stale baseline or unbounded preparation", runtime.calls, err)
	}
}

func TestNativeEstimateWrapperDoesNotOverwritePolicyOrReadCalibration(t *testing.T) {
	c := nativeEstimateAppCapability()
	store := &calibrationFixture{fail: errors.New("unexpected calibration")}
	reader := MeasuredAgentCapabilities{Runtime: measuredRuntimeFixture{c}, Observations: store}
	got, err := reader.AgentCapability(context.Background(), c.Provider, c.Model)
	if err != nil || got != c || store.calls != 0 {
		t.Fatal("overwritten native policy", err)
	}
	c.Calibration.Scope = c.RuntimeScope
	reader.Runtime = measuredRuntimeFixture{c}
	if _, err := reader.AgentCapability(context.Background(), c.Provider, c.Model); !errors.Is(err, domain.ErrProviderCapabilityUnknown) || store.calls != 0 {
		t.Fatal("silently discarded calibration", err)
	}
	c.Calibration = domain.AgentInputCalibration{}
	c.InputAccountingPolicy = "future-unknown"
	reader.Runtime = measuredRuntimeFixture{c}
	if _, err := reader.AgentCapability(context.Background(), c.Provider, c.Model); !errors.Is(err, domain.ErrProviderCapabilityUnknown) || store.calls != 0 {
		t.Fatal("unknown policy overwritten", err)
	}
}
