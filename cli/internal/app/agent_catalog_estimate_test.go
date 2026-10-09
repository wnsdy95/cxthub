package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/agenttokens"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func catalogEstimateAppCapability(exact bool) domain.AgentHostCapability {
	c := domain.AgentHostCapability{Provider: domain.ProviderCodex, Model: "synthetic-stock-model", HostVersion: "stock-host",
		Evidence: "synthetic native model cache", ContextWindow: 200000, Tokenizer: domain.UTF8ByteBoundCounter,
		InputAccountingPolicy: domain.CatalogEstimateReserveV1, RuntimeScope: agentHash("stock route/settings"),
		WindowEstimateSource: "native_model_cache", WindowEstimateObservedAt: "2026-10-09T12:34:56Z", WindowEstimateHash: agentHash("model cache"), WindowEstimateClientVersion: "0.157.1"}
	if exact {
		c.Tokenizer = "fixture-byte-counter"
	}
	return c
}

func TestCatalogEstimateAppPreparationAndDelivery(t *testing.T) {
	for _, exact := range []bool{false, true} {
		for _, paged := range []bool{false, true} {
			t.Run(map[bool]string{false: "byte/", true: "exact/"}[exact]+map[bool]string{false: "full", true: "paged"}[paged], func(t *testing.T) {
				s, in, h, docs, original := adaptiveHistoryFixture(t)
				c := catalogEstimateAppCapability(exact)
				in.Provider, in.Model, in.LatestMain = c.Provider, "", true
				in.InitialPrompt = domain.NewAgentInitialPrompt("\uacc4\uc18d\ud574 👋\n")
				in.WorkingPosition = &domain.AgentWorkingPosition{Branch: "feature", CodeCommit: strings.Repeat("a", 40)}
				in.WorktreeStateHash = agentHash("worktree")
				store := &calibrationFixture{fail: errors.New("must not access calibration")}
				s.capabilities = MeasuredAgentCapabilities{Runtime: measuredRuntimeFixture{c}, Observations: store}
				if !exact {
					s.tokens = agenttokens.New()
				}
				if paged {
					s.documents = &agentPageFixture{docs: docs.docs}
				}
				p, err := s.PrepareAgentContext(context.Background(), in)
				if err != nil {
					t.Fatal(err)
				}
				if p.Capability != "estimated_for_preparation" || p.Delivery != "prepared" || p.Usage.Exact != exact || p.Policy != in.Policy {
					t.Fatal("preparation promoted or changed estimate", p.Capability, p.Usage)
				}
				b := p.Budget
				if b.WindowEstimateSource != c.WindowEstimateSource || b.WindowEstimateObservedAt != c.WindowEstimateObservedAt || b.WindowEstimateHash != c.WindowEstimateHash || b.WindowEstimateClientVersion != c.WindowEstimateClientVersion || b.InputAccountingPolicy != c.InputAccountingPolicy || b.Model != c.Model {
					t.Fatal("lost model-cache provenance", b)
				}
				if !b.HostInputUnverified || !b.AutoCompactUnverified || b.HostInputTokens != 0 || b.FramingTokens != 0 || b.BaselineInputEstimateTokens != 0 || b.BaselineInputMeasurement != "" || b.FramingAllowanceTokens != 0 || b.ObservedOverheadTokens != 0 || b.ObservedInputCeilingTokens != 0 || b.InitialPromptTokens != len(in.InitialPrompt.Text()) || b.EffectiveTokens != 144000-len(in.InitialPrompt.Text()) {
					t.Fatal("invented baseline or lost question/reserve", b)
				}
				if len(p.Content.History) != 1 || !reflect.DeepEqual(p.Content.History[0].Events, original.CIR.Events[4:]) || !reflect.DeepEqual(docs.docs[original.Hash], original) || len(p.Content.ProjectMemory) != 1 {
					t.Fatal("lost recent complete turn or memory; changed archive")
				}
				if p.Content.Selection.SnapshotID != h.view.Position || p.Content.Selection.SourcePolicy != domain.AgentSourceLatestMain {
					t.Fatal("source selection changed")
				}
				if err := p.ValidateIdentity(); err != nil {
					t.Fatal(err)
				}
				if err := p.ValidateInitialPrompt(in.InitialPrompt); err != nil {
					t.Fatal(err)
				}
				if _, retry, err := RecordAgentInputObservation(context.Background(), store, p, domain.AgentInputObservation{InitialRequest: true, UsageKnown: true, SubmittedTextExact: true}); !errors.Is(err, domain.ErrProviderCapabilityUnknown) || retry {
					t.Fatal("catalog entered calibration", err)
				}
				if store.calls != 0 || store.writes != 0 {
					t.Fatal("accessed calibration store")
				}
				mat := &agentMaterializerFixture{}
				load := NewLoadSessionService(nil, map[domain.ProviderKind]outbound.ProviderCodec{c.Provider: codec.NewCodexCodec()}, map[domain.ProviderKind]outbound.SessionMaterializer{c.Provider: mat}, nil, nil, nil).WithAgentContext(&agentPackageFixture{}).WithAgentCodePosition(agentCodeFixture{})
				if _, err := load.materializeAgentPackage(context.Background(), in, p); err != nil || mat.calls != 1 {
					t.Fatal("estimate delivery rejected", err, mat.calls)
				}
				for _, mutation := range []string{"status", "question", "policy", "decoded", "provenance", "client version"} {
					changed, request, budget := p, in, *p.Budget
					changed.Budget = &budget
					switch mutation {
					case "status":
						changed.Capability = "verified_for_preparation"
					case "question":
						request.InitialPrompt = domain.NewAgentInitialPrompt("\ub2e4\ub978\ub9d0 👋\n")
					case "policy":
						changed.Budget.InputAccountingPolicy = domain.MeasuredInputReserveV1
					case "decoded":
						raw, err := p.Artifact()
						if err != nil {
							t.Fatal(err)
						}
						if err := json.Unmarshal(raw, &changed); err != nil {
							t.Fatal(err)
						}
					case "provenance":
						changed.Budget.WindowEstimateHash = agentHash("other valid cache")
					case "client version":
						changed.Budget.WindowEstimateClientVersion = "0.162.0"
					}
					changed.ID, _ = changed.Digest()
					if _, err := load.materializeAgentPackage(context.Background(), request, changed); err == nil || mat.calls != 1 {
						t.Fatal("invalid delivery reached materializer", mutation, err, mat.calls)
					}
				}
			})
		}
	}
}

func TestCatalogEstimateAppUsesRealExactTextCounter(t *testing.T) {
	s, in, _, _, _ := agentServiceFixture(t)
	c := catalogEstimateAppCapability(true)
	c.Model = "gpt-5.4"
	in.Provider, in.Model = c.Provider, c.Model
	in.Policy = domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: 800000, Source: "explicit"}
	in.InitialPrompt = domain.NewAgentInitialPrompt("Explain this code.\n")
	s.tokens = agenttokens.New()
	probe := agentTokenizerCount(t, s.tokens, in, in.InitialPrompt.Text())
	if !probe.Exact {
		t.Fatal("fixture requires a locally supported exact counter", probe)
	}
	c.Tokenizer = probe.Tokenizer
	s.capabilities = MeasuredAgentCapabilities{Runtime: measuredRuntimeFixture{c}}
	p, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	actual := agentTokenizerCount(t, s.tokens, in, agentTokenizerPrompt(t, p))
	if !p.Usage.Exact || p.Usage != actual || p.Budget.InitialPromptTokens != probe.Tokens || p.Capability != "estimated_for_preparation" {
		t.Fatal("exact text promoted window authority", p.Usage, p.Capability)
	}
}

func TestCatalogEstimateAppRejectsUnknownAndUnverifiedLegacyBeforeReads(t *testing.T) {
	for name, mutate := range map[string]func(*domain.AgentHostCapability){
		"unknown": func(c *domain.AgentHostCapability) { c.InputAccountingPolicy = "future" },
		"legacy": func(c *domain.AgentHostCapability) {
			c.InputAccountingPolicy = ""
			c.WindowEstimateSource, c.WindowEstimateObservedAt, c.WindowEstimateHash = "", "", ""
			c.WindowEstimateClientVersion = ""
		},
		"measured": func(c *domain.AgentHostCapability) {
			c.InputAccountingPolicy = domain.MeasuredInputReserveV1
			c.WindowEstimateSource, c.WindowEstimateObservedAt, c.WindowEstimateHash = "", "", ""
			c.WindowEstimateClientVersion = ""
		},
		"verified estimate":      func(c *domain.AgentHostCapability) { c.Verified = true },
		"missing source":         func(c *domain.AgentHostCapability) { c.WindowEstimateSource = "" },
		"unknown source":         func(c *domain.AgentHostCapability) { c.WindowEstimateSource = "config" },
		"bad time":               func(c *domain.AgentHostCapability) { c.WindowEstimateObservedAt = "yesterday" },
		"bad hash":               func(c *domain.AgentHostCapability) { c.WindowEstimateHash = "bad" },
		"missing client version": func(c *domain.AgentHostCapability) { c.WindowEstimateClientVersion = "" },
		"native baseline":        func(c *domain.AgentHostCapability) { c.BaselineInputMeasurement = domain.NativeLocalEstimate },
		"calibration":            func(c *domain.AgentHostCapability) { c.Calibration.Scope = c.RuntimeScope },
	} {
		t.Run(name, func(t *testing.T) {
			s, in, history, docs, _ := adaptiveHistoryFixture(t)
			c := catalogEstimateAppCapability(true)
			mutate(&c)
			in.Provider, in.Model = c.Provider, c.Model
			store := &calibrationFixture{fail: errors.New("unexpected calibration")}
			s.capabilities = MeasuredAgentCapabilities{Runtime: measuredRuntimeFixture{c}, Observations: store}
			if _, err := s.PrepareAgentContext(context.Background(), in); !errors.Is(err, domain.ErrProviderCapabilityUnknown) || history.calls != 0 || docs.calls != 0 || store.calls != 0 || store.writes != 0 {
				t.Fatal("bad capability read sources/calibration", err, history.calls, docs.calls, store.calls)
			}
		})
	}
}

type catalogEstimateRuntimeFunc func() domain.AgentHostCapability

func (f catalogEstimateRuntimeFunc) AgentCapability(context.Context, domain.ProviderKind, string) (domain.AgentHostCapability, error) {
	return f(), nil
}

func TestCatalogEstimateAppRevalidatesWindowAndProvenance(t *testing.T) {
	for _, field := range []string{"window", "hash", "observed at", "client version", "verified"} {
		t.Run(field, func(t *testing.T) {
			s, in, _, _, _ := adaptiveHistoryFixture(t)
			c := catalogEstimateAppCapability(false)
			c.ContextWindow = 400000
			in.Provider, in.Model = c.Provider, c.Model
			s.tokens = agenttokens.New()
			calls := 0
			store := &calibrationFixture{fail: errors.New("unexpected calibration")}
			s.capabilities = MeasuredAgentCapabilities{Observations: store, Runtime: catalogEstimateRuntimeFunc(func() domain.AgentHostCapability {
				calls++
				if calls == 2 {
					switch field {
					case "window":
						c.ContextWindow = 200000
					case "hash":
						c.WindowEstimateHash = agentHash("new cache")
					case "observed at":
						c.WindowEstimateObservedAt = "2026-10-09T12:35:56Z"
					case "client version":
						c.WindowEstimateClientVersion = "0.162.0"
					case "verified":
						c.Verified = true
					}
				}
				return c
			})}
			p, err := s.PrepareAgentContext(context.Background(), in)
			if field == "verified" {
				if !errors.Is(err, domain.ErrProviderCapabilityUnknown) || calls != 2 {
					t.Fatal("revalidation promoted catalog authority", err, calls)
				}
				return
			}
			if err != nil || calls != 4 || p.Budget.ContextWindow != c.ContextWindow || p.Budget.WindowEstimateHash != c.WindowEstimateHash || p.Budget.WindowEstimateObservedAt != c.WindowEstimateObservedAt || p.Budget.WindowEstimateClientVersion != c.WindowEstimateClientVersion || p.Capability != "estimated_for_preparation" {
				t.Fatal("stale catalog receipt", err, calls)
			}
			if field == "window" && len(p.Content.History) != 1 {
				t.Fatal("stale large-window selection")
			}
			if err := p.ValidateInitialPrompt(in.InitialPrompt); err != nil {
				t.Fatal("stale question binding", err)
			}
			if store.calls != 0 || store.writes != 0 {
				t.Fatal("revalidation accessed calibration")
			}
		})
	}
}

func TestCatalogEstimateAppRejectsBadCounterWithoutReselection(t *testing.T) {
	for _, phase := range []string{"question", "candidate", "question recheck"} {
		for _, mutation := range []string{"bytes", "reason", "scope", "exact"} {
			t.Run(phase+"/"+mutation, func(t *testing.T) {
				s, in, _, _, _ := adaptiveHistoryFixture(t)
				c := catalogEstimateAppCapability(false)
				in.Provider, in.Model = c.Provider, c.Model
				in.InitialPrompt = domain.NewAgentInitialPrompt("\uacc4\uc18d\ud574 👋")
				s.capabilities = MeasuredAgentCapabilities{Runtime: measuredRuntimeFixture{c}}
				questions, candidates := 0, 0
				s.tokens = agentMeasurementCounter(func(text string) (domain.AgentTokenUsage, error) {
					u := nativeEstimateAppUsage(text)
					question := text == in.InitialPrompt.Text()
					if question {
						questions++
					} else {
						candidates++
					}
					if (phase == "question" && question) || (phase == "candidate" && !question) || (phase == "question recheck" && question && questions > 1) {
						switch mutation {
						case "bytes":
							u.Tokens--
						case "reason":
							u.Reason = "tokenizer_work_limit"
						case "scope":
							u.Scope = "request"
						case "exact":
							u.Exact = true
						}
					}
					return u, nil
				})
				_, err := s.PrepareAgentContext(context.Background(), in)
				if err == nil {
					t.Fatal("bad counter accepted")
				}
				if mutation != "bytes" && (!errors.Is(err, domain.ErrProviderCapabilityUnknown) || agentCandidateLimit(err)) {
					t.Fatal("provenance drift became a fit problem", err)
				}
				if phase == "candidate" && candidates != 1 {
					t.Fatal("reselected to hide bad counter", candidates)
				}
			})
		}
	}
}

func TestCatalogEstimateWrapperPreservesPolicyWithoutCalibration(t *testing.T) {
	c := catalogEstimateAppCapability(true)
	store := &calibrationFixture{fail: errors.New("must not read calibration")}
	reader := MeasuredAgentCapabilities{Runtime: measuredRuntimeFixture{c}, Observations: store}
	got, err := reader.AgentCapability(context.Background(), c.Provider, c.Model)
	if err != nil || got != c || got.Verified || store.calls != 0 || store.writes != 0 {
		t.Fatal("wrapper promoted/overwrote estimate", err)
	}
	c.Calibration.Scope = c.RuntimeScope
	reader.Runtime = measuredRuntimeFixture{c}
	if _, err := reader.AgentCapability(context.Background(), c.Provider, c.Model); !errors.Is(err, domain.ErrProviderCapabilityUnknown) || store.calls != 0 || store.writes != 0 {
		t.Fatal("silently discarded calibration", err)
	}
}
