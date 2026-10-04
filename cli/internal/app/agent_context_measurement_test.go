package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type agentMeasurementCounter func(string) (domain.AgentTokenUsage, error)

func (f agentMeasurementCounter) CountAgentTokens(ctx context.Context, _ domain.ProviderKind, _ string, text string) (domain.AgentTokenUsage, error) {
	if err := ctx.Err(); err != nil {
		return domain.AgentTokenUsage{}, err
	}
	return f(text)
}

func agentMeasurementUsage(text, reason string) domain.AgentTokenUsage {
	u := domain.AgentTokenUsage{Tokens: len(text), Exact: true, Tokenizer: "fixture-byte-counter"}
	if reason != "" {
		u.Exact, u.Tokenizer, u.Reason = false, domain.UTF8ByteBoundCounter, reason
	}
	return u
}

func agentMeasurementHistory(t *testing.T, paged bool, texts ...string) (*AgentContextService, inbound.PrepareAgentContextInput, domain.SessionDoc) {
	t.Helper()
	s, in, h, memory, docs := agentServiceFixture(t)
	var events []domain.Event
	for _, text := range texts {
		events = append(events, agentMessage("user", text, len(events)), agentMessage("assistant", "complete", len(events)+1))
	}
	doc := agentDocument(t, "measurement-history", events...)
	h.view.Snapshots = []domain.Snapshot{{ID: doc.Hash, DocHash: doc.Hash, RepoID: in.RepoID}}
	h.view.Position, in.SnapshotID = doc.Hash, doc.Hash
	memory.items = nil
	docs.docs = map[domain.ContentHash]domain.SessionDoc{doc.Hash: doc}
	if paged {
		s.documents = &agentPageFixture{docs: docs.docs}
	}
	in.ArtifactOnly = true
	in.Policy = domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: 8000, Source: "explicit"}
	s.capabilities = agentCapabilityFixture{window: 100000}
	s.tokens = agentTokenFixture{}
	return s, in, doc
}

func TestAgentContextMeasurementNewestTurnDiagnostics(t *testing.T) {
	for _, paged := range []bool{false, true} {
		for _, reason := range []string{"", "tokenizer_work_limit", "unicode_classification_unavailable", "model_tokenizer_unavailable"} {
			t.Run(fmt.Sprintf("paged=%t/reason=%s", paged, reason), func(t *testing.T) {
				private := "private-newest-condition " + strings.Repeat("x", 10000)
				s, in, _ := agentMeasurementHistory(t, paged, private)
				required := 0
				s.tokens = agentMeasurementCounter(func(text string) (domain.AgentTokenUsage, error) {
					if strings.Contains(text, "private-newest-condition") {
						required = len(text)
						return agentMeasurementUsage(text, reason), nil
					}
					return agentMeasurementUsage(text, ""), nil
				})
				p, err := s.PrepareAgentContext(context.Background(), in)
				if err == nil || len(p.Content.History) != 0 {
					t.Fatal("selected a rejected or truncated newest turn")
				}
				var measurement *AgentTokenMeasurementError
				if reason == "" {
					counts := fmt.Sprintf("required package %d, effective budget %d (requested %d)", required, in.Policy.BudgetTokens, in.Policy.BudgetTokens)
					if required <= len(private) || !errors.Is(err, domain.ErrContextBudgetExceeded) || errors.As(err, &measurement) || !strings.Contains(err.Error(), counts) {
						t.Fatalf("lost exact overflow: %v", err)
					}
				} else {
					if errors.Is(err, domain.ErrContextBudgetExceeded) || !errors.As(err, &measurement) || measurement.Reason != reason || measurement.Allowance <= measurement.Budget {
						t.Fatalf("fallback reported as measured overflow or lost its reason: %v", err)
					}
					for _, misleading := range []string{"exceeds remaining", "larger budget", "required package"} {
						if strings.Contains(err.Error(), misleading) {
							t.Fatalf("misleading fallback advice: %v", err)
						}
					}
				}
				if strings.Contains(err.Error(), "private-newest-condition") {
					t.Fatal("diagnostic exposed historical text")
				}
			})
		}
	}
}

func TestAgentContextMeasurementExactNewestRetainsEffectiveBudget(t *testing.T) {
	for _, paged := range []bool{false, true} {
		t.Run(fmt.Sprintf("paged=%t", paged), func(t *testing.T) {
			s, in, _ := agentMeasurementHistory(t, paged, "exact-newest "+strings.Repeat("x", 100000))
			in.ArtifactOnly = false
			in.Policy.BudgetTokens = 300000
			// This fixture's verified 100k window allows 80k initial input,
			// less its ten-token framing reservation. The request is larger.
			const effective = 79990
			required := 0
			s.tokens = agentMeasurementCounter(func(text string) (domain.AgentTokenUsage, error) {
				if strings.Contains(text, "exact-newest") {
					required = len(text)
				}
				return agentMeasurementUsage(text, ""), nil
			})
			_, err := s.PrepareAgentContext(context.Background(), in)
			counts := fmt.Sprintf("required package %d, effective budget %d (requested %d)", required, effective, in.Policy.BudgetTokens)
			var measurement *AgentTokenMeasurementError
			if required <= effective || required >= in.Policy.BudgetTokens || !errors.Is(err, domain.ErrContextBudgetExceeded) || errors.As(err, &measurement) || !strings.Contains(err.Error(), counts) || !strings.Contains(err.Error(), "newest complete turn") {
				t.Fatalf("lost exact rendered count or substituted requested budget: %v", err)
			}
		})
	}
}

func TestAgentContextMeasurementPreservesSmallestRejectedReason(t *testing.T) {
	for _, paged := range []bool{false, true} {
		for _, smallestReason := range []string{"", "unicode_classification_unavailable"} {
			t.Run(fmt.Sprintf("paged=%t/smallest=%s", paged, smallestReason), func(t *testing.T) {
				s, in, _ := agentMeasurementHistory(t, paged, "oldest", "middle-probe", "newest-probe "+strings.Repeat("x", 10000))
				var measured []string
				s.tokens = agentMeasurementCounter(func(text string) (domain.AgentTokenUsage, error) {
					reason := ""
					if strings.Contains(text, "middle-probe") {
						reason = "tokenizer_work_limit"
					} else if strings.Contains(text, "newest-probe") {
						reason = smallestReason
					}
					if strings.Contains(text, "newest-probe") {
						measured = append(measured, reason)
					}
					return agentMeasurementUsage(text, reason), nil
				})
				_, err := s.PrepareAgentContext(context.Background(), in)
				if !reflect.DeepEqual(measured, []string{"tokenizer_work_limit", smallestReason}) {
					t.Fatalf("did not shrink complete candidates: %v", measured)
				}
				var measurement *AgentTokenMeasurementError
				if smallestReason == "" {
					if !errors.Is(err, domain.ErrContextBudgetExceeded) || errors.As(err, &measurement) {
						t.Fatalf("kept earlier fallback instead of exact smallest rejection: %v", err)
					}
				} else if !errors.As(err, &measurement) || measurement.Reason != smallestReason || errors.Is(err, domain.ErrContextBudgetExceeded) {
					t.Fatalf("lost smallest candidate's reason: %v", err)
				}
			})
		}
	}
}

func TestAgentContextMeasurementOlderGapKeepsCompleteNewestTurn(t *testing.T) {
	for _, paged := range []bool{false, true} {
		for _, artifact := range []bool{false, true} {
			for _, separate := range []bool{false, true} {
				t.Run(fmt.Sprintf("paged=%t/artifact=%t/separate=%t", paged, artifact, separate), func(t *testing.T) {
					older := "older-work-limited " + strings.Repeat("x", 10000)
					s, in, doc := agentMeasurementHistory(t, paged, older, "latest exact condition")
					in.ArtifactOnly = artifact
					want := doc.CIR.Events[2:]
					if separate {
						latest := agentDocument(t, "latest-session", want...)
						old := agentDocument(t, "older-session", doc.CIR.Events[:2]...)
						h := s.history.(*agentHistoryFixture)
						h.view.Snapshots = []domain.Snapshot{{ID: latest.Hash, DocHash: latest.Hash, RepoID: in.RepoID}, {ID: old.Hash, DocHash: old.Hash, RepoID: in.RepoID}}
						h.view.Position, in.SnapshotID = latest.Hash, latest.Hash
						docs := map[domain.ContentHash]domain.SessionDoc{latest.Hash: latest, old.Hash: old}
						if paged {
							s.documents = &agentPageFixture{docs: docs}
						} else {
							s.documents = &agentDocFixture{docs: docs}
						}
					}
					s.tokens = agentMeasurementCounter(func(text string) (domain.AgentTokenUsage, error) {
						reason := ""
						if strings.Contains(text, "older-work-limited") {
							reason = "tokenizer_work_limit"
						}
						return agentMeasurementUsage(text, reason), nil
					})
					p, err := s.PrepareAgentContext(context.Background(), in)
					if err != nil {
						t.Fatal(err)
					}
					if len(p.Content.History) != 1 || !reflect.DeepEqual(p.Content.History[0].Events, want) || !p.Usage.Exact {
						t.Fatal("lost newest complete turn or admitted inexact native history")
					}
					prompt, _ := p.Prompt()
					if !strings.Contains(prompt, `"reason":"history_tokenizer_work_limit"`) || strings.Contains(prompt, "older-work-limited") || p.Usage.Tokens != len(prompt) || p.Usage.Tokens > p.EffectiveBudget() {
						t.Fatal("missing truthful measured coverage gap")
					}
					if err := p.ValidateIdentity(); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}

func TestAgentContextMeasurementFittingFallbackIsArtifactOnly(t *testing.T) {
	for _, paged := range []bool{false, true} {
		for _, artifact := range []bool{false, true} {
			t.Run(fmt.Sprintf("paged=%t/artifact=%t", paged, artifact), func(t *testing.T) {
				s, in, doc := agentMeasurementHistory(t, paged, "newest-inexact")
				in.ArtifactOnly = artifact
				s.tokens = agentMeasurementCounter(func(text string) (domain.AgentTokenUsage, error) {
					reason := ""
					if strings.Contains(text, "newest-inexact") {
						reason = "tokenizer_work_limit"
					}
					return agentMeasurementUsage(text, reason), nil
				})
				p, err := s.PrepareAgentContext(context.Background(), in)
				if artifact {
					if err != nil {
						t.Fatal(err)
					}
					if p.Usage.Exact || p.Usage.Tokens > p.EffectiveBudget() || len(p.Content.History) != 1 || !reflect.DeepEqual(p.Content.History[0].Events, doc.CIR.Events) {
						t.Fatal("fitting conservative artifact lost its accounting or complete turn")
					}
				} else {
					var measurement *AgentTokenMeasurementError
					if !errors.As(err, &measurement) || !measurement.ExactRequired || measurement.Allowance > measurement.Budget || !errors.Is(err, domain.ErrProviderCapabilityUnknown) || errors.Is(err, domain.ErrContextBudgetExceeded) {
						t.Fatalf("native route accepted inexact usage or misdiagnosed it: %v", err)
					}
				}
			})
		}
	}
}

func TestAgentContextMeasurementCancellationDoesNotReselect(t *testing.T) {
	for _, paged := range []bool{false, true} {
		t.Run(fmt.Sprintf("paged=%t", paged), func(t *testing.T) {
			s, in, _ := agentMeasurementHistory(t, paged, "older", "cancel-counter", "latest")
			calls := 0
			s.tokens = agentMeasurementCounter(func(text string) (domain.AgentTokenUsage, error) {
				if strings.Contains(text, "cancel-counter") {
					calls++
					return domain.AgentTokenUsage{}, context.Canceled
				}
				return agentMeasurementUsage(text, ""), nil
			})
			if _, err := s.PrepareAgentContext(context.Background(), in); !errors.Is(err, context.Canceled) || calls != 1 {
				t.Fatalf("cancelled measurement became selection pressure: %v calls=%d", err, calls)
			}
		})
	}
}

func TestAgentContextMeasurementFinalDiagnosticsParticipateInSelection(t *testing.T) {
	for _, paged := range []bool{false, true} {
		for _, artifact := range []bool{false, true} {
			for _, scenario := range []struct {
				name             string
				memory, separate bool
			}{
				{name: "memory and history diagnostics", memory: true},
				{name: "reselect previously accepted document", memory: true, separate: true},
				{name: "shorter replacement costs more tokens"},
			} {
				t.Run(fmt.Sprintf("paged=%t/artifact=%t/%s", paged, artifact, scenario.name), func(t *testing.T) {
					s, in, doc := agentMeasurementHistory(t, paged,
						"unmeasurable-history "+strings.Repeat("x", 10000),
						"middle complete condition "+strings.Repeat("m", 400),
						"newest complete condition "+strings.Repeat("n", 400))
					in.ArtifactOnly = artifact
					if scenario.separate {
						latest := agentDocument(t, "latest-session", doc.CIR.Events[2:]...)
						old := agentDocument(t, "older-session", doc.CIR.Events[:2]...)
						h := s.history.(*agentHistoryFixture)
						h.view.Snapshots = []domain.Snapshot{{ID: latest.Hash, DocHash: latest.Hash, RepoID: in.RepoID}, {ID: old.Hash, DocHash: old.Hash, RepoID: in.RepoID}}
						h.view.Position, in.SnapshotID = latest.Hash, latest.Hash
						docs := map[domain.ContentHash]domain.SessionDoc{latest.Hash: latest, old.Hash: old}
						if paged {
							s.documents = &agentPageFixture{docs: docs}
						} else {
							s.documents = &agentDocFixture{docs: docs}
						}
					}
					if scenario.memory {
						memory := s.memory.(*agentMemoryFixture)
						memory.items = []domain.EffectiveMemoryItem{{ID: agentHash("limited-memory"), SourceSnapshot: in.SnapshotID, Kind: "decision", State: "retained", Reason: "project_decision", Text: "unmeasurable-memory " + strings.Repeat("x", 8000)}}
					}
					counter := agentMeasurementCounter(func(text string) (domain.AgentTokenUsage, error) {
						if strings.Contains(text, "unmeasurable-memory") {
							return agentMeasurementUsage(text, "unicode_classification_unavailable"), nil
						}
						if strings.Contains(text, "unmeasurable-history") {
							return agentMeasurementUsage(text, "tokenizer_work_limit"), nil
						}
						u := agentMeasurementUsage(text, "")
						if strings.Contains(text, `"reason":"history_tokenizer_work_limit"`) {
							// Synthetic encoding cost: a shorter reason is not proof
							// of fewer BPE tokens. Selection must ask the counter.
							u.Tokens += 128
						}
						return u, nil
					})
					s.tokens = counter
					baseline, err := s.PrepareAgentContext(context.Background(), in)
					if err != nil || len(baseline.Content.History) != 2 {
						t.Fatalf("cannot establish complete-turn boundary: %v", err)
					}
					memoryGap, historyGap := false, false
					for _, gap := range baseline.Content.Gaps {
						memoryGap = memoryGap || gap.Reason == "memory_unicode_classification_unavailable"
						historyGap = historyGap || gap.Reason == "history_tokenizer_work_limit"
					}
					if memoryGap != scenario.memory || !historyGap {
						t.Fatal("fixture did not preserve each scoped measurement diagnostic")
					}
					beforeDiagnostic := baseline
					beforeDiagnostic.Content.Gaps = nil
					for _, gap := range baseline.Content.Gaps {
						if gap.Reason == "history_tokenizer_work_limit" {
							if scenario.memory {
								continue
							}
							gap.Reason = agentBoundedProjectionGap
						}
						beforeDiagnostic.Content.Gaps = append(beforeDiagnostic.Content.Gaps, gap)
					}
					text, _ := beforeDiagnostic.Prompt()
					boundary, _ := counter(text)
					in.Policy.BudgetTokens = boundary.Tokens
					if baseline.Usage.Tokens <= boundary.Tokens {
						t.Fatal("diagnostic must move the prior selection over budget")
					}
					p, err := s.PrepareAgentContext(context.Background(), in)
					if err != nil {
						t.Fatalf("diagnostic prevented a fitting complete newest turn: %v", err)
					}
					if len(p.Content.History) != 1 || !reflect.DeepEqual(p.Content.History[0], baseline.Content.History[1]) || !reflect.DeepEqual(p.Content.Gaps, baseline.Content.Gaps) || len(p.Content.ProjectMemory) != 0 {
						t.Fatal("lost complete newest turn, diagnostic, or omission boundary")
					}
					text, _ = p.Prompt()
					measured, _ := counter(text)
					if p.Usage != measured || !p.Usage.Exact || p.Usage.Tokens > p.EffectiveBudget() {
						t.Fatal("final diagnostics were not included in exact accounting")
					}
					if err := p.ValidateIdentity(); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}
