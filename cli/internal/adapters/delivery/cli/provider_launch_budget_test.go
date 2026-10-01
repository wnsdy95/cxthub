package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestProviderLaunchBudgetAccountsRenderedFixtureAndPreservesReceipts(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, tc := range []struct {
			name, reason            string
			reserve, trigger, limit int
		}{
			{"window", "model_window_80_percent", 20000, 120000, 102400},
			{"output reserve", "required_output_reserve", 40000, 120000, 88000},
			{"compaction", "automatic_compaction_threshold", 20000, 100000, 99999},
		} {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				root, _ := providerLaunchFixture(t, provider, "")
				intent := LaunchIntent{Provider: provider, Pull: true, ContextBudget: 800000, ProviderArgs: []string{"--model", "fixture-model", "user task"}}
				request := ProviderLaunchRequest{Cwd: root, Intent: intent}
				host := launchHostFixture(request)
				host.ContextWindow, host.HostInputTokens, host.FramingTokens = 128000, 3000, 400
				host.ReservedTokens, host.AutoCompactTokens = tc.reserve, tc.trigger
				fixture := strictLaunchContextFixture(request, host)
				prompt, err := fixture.Prompt()
				if err != nil {
					t.Fatal(err)
				}
				if fixture.Usage.Tokens != len([]rune(prompt)) || fixture.Usage.Tokens == 123 || !fixture.Usage.Exact || fixture.Content.PersonalWork == nil || len(fixture.Content.History) == 0 {
					t.Fatalf("fixture does not account for complete rendered input: %+v", fixture)
				}
				budget := fixture.Budget
				if budget.RequestedTokens != 800000 || budget.InitialInputLimit != tc.limit || budget.EffectiveTokens != tc.limit-3400 || budget.AdjustmentReason != tc.reason {
					t.Fatalf("incorrect adjusted accounting: %+v", budget)
				}
				total := fixture.Usage.Tokens + budget.HostInputTokens + budget.FramingTokens
				if total > budget.ContextWindow*4/5 || total+budget.ReservedTokens > budget.ContextWindow || total >= budget.AutoCompactTokens {
					t.Fatalf("initial input violates runtime limits: total=%d budget=%+v", total, budget)
				}
				var receipts []ProviderLaunchReceipt
				hooks := ProviderLaunchHooks{Prepare: func(_ context.Context, req ProviderLaunchRequest) (PreparedProviderLaunch, error) {
					p := preparedLaunch(req)
					p.PackageHash, p.SelectedTokens, p.Budget = fixture.ID, fixture.Usage.Tokens, fixture.Budget
					return p, nil
				}, Record: func(_ context.Context, r ProviderLaunchReceipt) error { receipts = append(receipts, r); return nil }}
				var summary bytes.Buffer
				runtime := launchTestRuntime()
				runtime.stderr = &summary
				if err := runProviderLaunch(context.Background(), root, intent, hooks, runtime); err != nil {
					t.Fatal(err)
				}
				if len(receipts) != 2 || receipts[0].State != "prepared" || receipts[1].State != "launched" {
					t.Fatalf("incorrect receipt lifecycle: %+v", receipts)
				}
				for _, r := range receipts {
					if !reflect.DeepEqual(r.Budget, budget) || r.PackageHash != fixture.ID || r.RequestedBudget != 800000 || r.SelectedTokens != fixture.Usage.Tokens || r.Mode != "history" || r.Acceptance != "unknown" {
						t.Fatalf("accounting changed in receipt: %+v", r)
					}
					raw, err := json.Marshal(r)
					if err != nil {
						t.Fatal(err)
					}
					var decoded ProviderLaunchReceipt
					if err := json.Unmarshal(raw, &decoded); err != nil || !reflect.DeepEqual(decoded.Budget, budget) {
						t.Fatalf("budget lost in receipt JSON: %s / %v", raw, err)
					}
				}
				for _, text := range []string{"budget=800000", `model="fixture-model"`, fmt.Sprintf("effective_budget=%d", budget.EffectiveTokens), fmt.Sprintf("initial_input_limit=%d", tc.limit), "host_input_tokens=3000", "framing_tokens=400", fmt.Sprintf("reserved_tokens=%d", tc.reserve), "context_window=128000", "adjustment=" + tc.reason, "provider acceptance unknown"} {
					if !strings.Contains(summary.String(), text) {
						t.Fatalf("summary omits %q: %s", text, summary.String())
					}
				}
			})
		}
	}
}

func TestProviderLaunchBudgetRejectsInvalidAccountingBeforeChildStarts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*PreparedProviderLaunch)
		want   error
	}{
		{"missing budget", func(p *PreparedProviderLaunch) { p.Budget = nil }, domain.ErrProviderCapabilityUnknown},
		{"zero selected tokens", func(p *PreparedProviderLaunch) { p.SelectedTokens = 0 }, domain.ErrContextBudgetExceeded},
		{"provider mismatch", func(p *PreparedProviderLaunch) { p.Budget.Provider = domain.ProviderCodex }, domain.ErrProviderCapabilityUnknown},
		{"explicit model mismatch", func(p *PreparedProviderLaunch) { p.Budget.Model = "different" }, domain.ErrProviderCapabilityUnknown},
		{"original request mismatch", func(p *PreparedProviderLaunch) { p.Budget.RequestedTokens = 200000 }, domain.ErrContextBudgetExceeded},
		{"forged effective limit", func(p *PreparedProviderLaunch) { p.Budget.EffectiveTokens++ }, domain.ErrContextBudgetExceeded},
		{"forged initial input limit", func(p *PreparedProviderLaunch) { p.Budget.InitialInputLimit++ }, domain.ErrContextBudgetExceeded},
		{"forged adjustment reason", func(p *PreparedProviderLaunch) { p.Budget.AdjustmentReason = "unverified_override" }, domain.ErrContextBudgetExceeded},
		{"missing resolved model", func(p *PreparedProviderLaunch) { p.Budget.Model = "" }, domain.ErrProviderCapabilityUnknown},
		{"missing host version", func(p *PreparedProviderLaunch) { p.Budget.HostVersion = "" }, domain.ErrProviderCapabilityUnknown},
		{"missing tokenizer", func(p *PreparedProviderLaunch) { p.Budget.Tokenizer = "" }, domain.ErrProviderCapabilityUnknown},
		{"negative host input", func(p *PreparedProviderLaunch) { p.Budget.HostInputTokens = -1 }, domain.ErrProviderCapabilityUnknown},
		{"inexact measurement", func(p *PreparedProviderLaunch) { p.TokenMeasurement = "conservative_bound" }, domain.ErrProviderCapabilityUnknown},
		{"effective overflow below original request", func(p *PreparedProviderLaunch) { p.SelectedTokens = p.Budget.EffectiveTokens + 1 }, domain.ErrContextBudgetExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := providerLaunchFixture(t, "claude", "")
			intent := LaunchIntent{Provider: "claude", Pull: true, ContextBudget: 800000, ProviderArgs: []string{"--model", "fixture-model", "user task"}}
			var states []string
			cleaned := false
			hooks := ProviderLaunchHooks{Prepare: func(_ context.Context, req ProviderLaunchRequest) (PreparedProviderLaunch, error) {
				p := preparedLaunch(req)
				tc.mutate(&p)
				p.Cleanup = func() error { cleaned = true; return nil }
				return p, nil
			}, Record: func(_ context.Context, r ProviderLaunchReceipt) error {
				states = append(states, r.State)
				if r.Acceptance != "unknown" {
					t.Fatalf("invalid receipt claims acceptance: %+v", r)
				}
				return nil
			}}
			err := runProviderLaunch(context.Background(), root, intent, hooks, launchTestRuntime())
			if !errors.Is(err, tc.want) || !cleaned || !reflect.DeepEqual(states, []string{"failed"}) {
				t.Fatalf("error=%v cleaned=%v states=%v", err, cleaned, states)
			}
			if _, err := os.Stat(filepath.Join(root, "launch.log")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("provider started with invalid accounting: %v", err)
			}
		})
	}
}

func TestProviderLaunchBudgetUsesResolvedModelWhenUnspecified(t *testing.T) {
	for _, tc := range []struct {
		provider string
		args     []string
	}{
		{"claude", nil},
		{"claude", []string{"--settings", `{"model":"fixture-default"}`}},
		{"codex", []string{"--profile", "fixture-profile", "-c", `model="fixture-default"`}},
		{"codex", []string{"--", "--model=literal user prompt"}},
		{"codex", []string{"--", "--model="}},
		{"codex", []string{"-mfixture-model"}},
		{"claude", []string{"--model=fixture-model"}},
	} {
		t.Run(tc.provider+" "+strings.Join(tc.args, " "), func(t *testing.T) {
			request := ProviderLaunchRequest{Intent: LaunchIntent{Provider: tc.provider, ProviderArgs: tc.args, Pull: true, ContextBudget: 800000}}
			p := preparedLaunch(request)
			if err := validatePreparedProviderLaunch(request, p); err != nil {
				t.Fatalf("resolved/explicit model rejected: %+v / %v", p.Budget, err)
			}
			p.SelectedTokens = p.Budget.EffectiveTokens
			if err := validatePreparedProviderLaunch(request, p); err != nil {
				t.Fatalf("exact effective limit rejected: %v", err)
			}
			p.Budget.Model = ""
			if err := validatePreparedProviderLaunch(request, p); !errors.Is(err, domain.ErrProviderCapabilityUnknown) {
				t.Fatalf("missing resolved model accepted: %v", err)
			}
		})
	}
}

func TestProviderLaunchBudgetRejectsEmptyExplicitModelBeforeChildStarts(t *testing.T) {
	for _, tc := range []struct {
		provider string
		args     []string
	}{
		{"claude", []string{"--model="}},
		{"claude", []string{"--model", ""}},
		{"codex", []string{"-m="}},
		{"codex", []string{"--model=fixture-model", "--model="}},
	} {
		t.Run(tc.provider+" "+strings.Join(tc.args, " "), func(t *testing.T) {
			root, _ := providerLaunchFixture(t, tc.provider, "")
			intent := LaunchIntent{Provider: tc.provider, ProviderArgs: tc.args, Pull: true, ContextBudget: 800000}
			var states []string
			hooks := ProviderLaunchHooks{Prepare: func(_ context.Context, req ProviderLaunchRequest) (PreparedProviderLaunch, error) {
				return preparedLaunch(req), nil
			}, Record: func(_ context.Context, r ProviderLaunchReceipt) error { states = append(states, r.State); return nil }}
			if err := runProviderLaunch(context.Background(), root, intent, hooks, launchTestRuntime()); !errors.Is(err, domain.ErrProviderCapabilityUnknown) || !reflect.DeepEqual(states, []string{"failed"}) {
				t.Fatalf("empty model launched: error=%v states=%v", err, states)
			}
			if _, err := os.Stat(filepath.Join(root, "launch.log")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("provider started with an empty model: %v", err)
			}
		})
	}
}

func TestProviderLaunchBudgetCopiesSurviveCallerAndRecordMutation(t *testing.T) {
	root, _ := providerLaunchFixture(t, "claude", "exit 7")
	intent := LaunchIntent{Provider: "claude", Pull: true, ContextBudget: 800000}
	var original *domain.AgentContextBudget
	var expected domain.AgentContextBudget
	var states []string
	var delivered []*domain.AgentContextBudget
	hooks := ProviderLaunchHooks{Prepare: func(_ context.Context, req ProviderLaunchRequest) (PreparedProviderLaunch, error) {
		p := preparedLaunch(req)
		original, expected = p.Budget, *p.Budget
		p.Validate = func(context.Context) error { original.Model = "caller-mutated-during-validation"; return nil }
		return p, nil
	}, Record: func(_ context.Context, r ProviderLaunchReceipt) error {
		states = append(states, r.State)
		if r.Budget == nil || *r.Budget != expected || r.Budget == original || r.Acceptance != "unknown" {
			t.Fatalf("receipt aliases caller or lost accounting: %+v", r)
		}
		for _, prior := range delivered {
			if r.Budget == prior {
				t.Fatal("receipt states share mutable budget metadata")
			}
		}
		delivered = append(delivered, r.Budget)
		original.EffectiveTokens = 0
		r.Budget.RequestedTokens, r.Budget.EffectiveTokens = 1, 0
		return nil
	}}
	if err := runProviderLaunch(context.Background(), root, intent, hooks, launchTestRuntime()); err == nil || !reflect.DeepEqual(states, []string{"prepared", "launched", "failed"}) {
		t.Fatalf("error=%v states=%v", err, states)
	}
}

func TestProviderLaunchBudgetReturnedPreparationAndReceiptAreIndependent(t *testing.T) {
	request := ProviderLaunchRequest{Intent: LaunchIntent{Provider: "claude", Pull: true, ContextBudget: 800000}}
	original := preparedLaunch(request)
	expected := *original.Budget
	p, r, err := prepareProviderLaunch(context.Background(), request, ProviderLaunchHooks{Prepare: func(context.Context, ProviderLaunchRequest) (PreparedProviderLaunch, error) {
		return original, nil
	}, Record: func(context.Context, ProviderLaunchReceipt) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if p.Budget == original.Budget || r.Budget == p.Budget || r.Budget == original.Budget {
		t.Fatal("preparation, caller and receipt share budget metadata")
	}
	original.Budget.Model = "changed"
	r.Budget.EffectiveTokens = 0
	if *p.Budget != expected || validatePreparedProviderLaunch(request, p) != nil {
		t.Fatalf("caller or receipt mutation changed preparation: %+v", p.Budget)
	}
}

func TestProviderLaunchBudgetOmittedForDefaultMemoryAndBootstrap(t *testing.T) {
	for _, mode := range []string{"memory", "empty_bootstrap"} {
		t.Run(mode, func(t *testing.T) {
			root, _ := providerLaunchFixture(t, "claude", "")
			var states []string
			hooks := ProviderLaunchHooks{Prepare: func(_ context.Context, req ProviderLaunchRequest) (PreparedProviderLaunch, error) {
				if mode == "empty_bootstrap" {
					return preparedBootstrap(req), nil
				}
				return preparedLaunch(req), nil
			}, Record: func(_ context.Context, r ProviderLaunchReceipt) error {
				states = append(states, r.State)
				raw, err := json.Marshal(r)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				if _, present := fields["budget"]; present || r.Budget != nil || r.Mode != mode || r.Acceptance != "unknown" {
					t.Fatalf("default receipt gained history accounting: %s", raw)
				}
				return nil
			}}
			if err := runProviderLaunch(context.Background(), root, LaunchIntent{Provider: "claude"}, hooks, launchTestRuntime()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(states, []string{"prepared", "launched"}) {
				t.Fatalf("incorrect default lifecycle: %v", states)
			}
		})
	}
}
