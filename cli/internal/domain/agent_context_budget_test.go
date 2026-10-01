package domain

import (
	"encoding/json"
	"errors"
	"testing"
)

func budgetCapability(window int) AgentHostCapability {
	return AgentHostCapability{Provider: ProviderCodex, Model: "synthetic", HostVersion: "fixture", Tokenizer: "synthetic-counter",
		Verified: true, HostInputKnown: true, AutoCompactKnown: true, Evidence: "synthetic test; no runtime capacity claim", ContextWindow: window}
}

func TestAgentBudgetWindowAndReservations(t *testing.T) {
	for _, tc := range []struct {
		name                                                                     string
		window, request, host, framing, reserve, compact, wantLimit, wantPackage int
		reason                                                                   string
	}{
		{"200k", 200000, 800000, 0, 0, 0, 0, 160000, 160000, "model_window_80_percent"},
		{"400k", 400000, 800000, 0, 0, 0, 0, 320000, 320000, "model_window_80_percent"},
		{"1m", 1000000, 800000, 0, 0, 0, 0, 800000, 800000, ""},
		{"host", 200000, 800000, 10000, 123, 10000, 0, 160000, 149877, "model_window_80_percent"},
		{"requested", 200000, 100000, 10000, 10, 0, 0, 160000, 100000, ""},
		{"no double reserve", 200000, 800000, 0, 0, 40000, 0, 160000, 160000, "model_window_80_percent"},
		{"large reserve", 200000, 800000, 1000, 10, 70000, 0, 130000, 128990, "required_output_reserve"},
		{"early compact", 200000, 800000, 1000, 10, 0, 150000, 149999, 148989, "automatic_compaction_threshold"},
		{"rounding", 200004, 800000, 0, 0, 0, 0, 160003, 160003, "model_window_80_percent"},
		{"above product ceiling", 2000000, 800000, 0, 0, 0, 0, 1600000, 800000, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := budgetCapability(tc.window)
			c.HostInputTokens, c.FramingTokens, c.ReservedTokens, c.AutoCompactTokens = tc.host, tc.framing, tc.reserve, tc.compact
			u := AgentTokenUsage{Exact: true, Tokenizer: c.Tokenizer}
			b, err := c.ResolveBudget(c.Provider, c.Model, tc.request, u)
			if err != nil || b.InitialInputLimit != tc.wantLimit || b.EffectiveTokens != tc.wantPackage || b.RequestedTokens != tc.request || b.AdjustmentReason != tc.reason {
				t.Fatalf("budget=%+v err=%v", b, err)
			}
			u.Tokens = b.EffectiveTokens
			if err = b.Validate(c.Provider, c.Model, tc.request, u); err != nil {
				t.Fatal(err)
			}
			if err = b.Validate(c.Provider, c.Model, tc.request, AgentTokenUsage{Exact: true, Tokenizer: c.Tokenizer}); !errors.Is(err, ErrContextBudgetExceeded) {
				t.Fatalf("accepted zero selected tokens: %v", err)
			}
			u.Tokens++
			if err = b.Validate(c.Provider, c.Model, tc.request, u); !errors.Is(err, ErrContextBudgetExceeded) {
				t.Fatalf("over limit=%v", err)
			}
		})
	}
	// Arithmetic must stay valid even on hostile metadata near MaxInt.
	c := budgetCapability(int(^uint(0) >> 1))
	b, err := c.ResolveBudget(c.Provider, c.Model, MaxAgentContextTokens, AgentTokenUsage{Exact: true, Tokenizer: c.Tokenizer})
	if err != nil || b.InitialInputLimit <= 0 || b.InitialInputLimit >= c.ContextWindow || b.EffectiveTokens != MaxAgentContextTokens {
		t.Fatalf("overflow: %+v %v", b, err)
	}
}

func TestAgentBudgetRejectsUnknownOrImpossibleRuntime(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*AgentHostCapability)
		want   error
	}{
		{"unknown host", func(c *AgentHostCapability) { c.HostInputKnown = false }, ErrProviderCapabilityUnknown},
		{"unknown compact", func(c *AgentHostCapability) { c.AutoCompactKnown = false }, ErrProviderCapabilityUnknown},
		{"unverified", func(c *AgentHostCapability) { c.Verified = false }, ErrProviderCapabilityUnknown},
		{"model changed", func(c *AgentHostCapability) { c.Model = "different" }, ErrProviderCapabilityUnknown},
		{"negative host", func(c *AgentHostCapability) { c.HostInputTokens = -1 }, ErrProviderCapabilityUnknown},
		{"no window", func(c *AgentHostCapability) { c.ContextWindow = 0 }, ErrProviderCapabilityUnknown},
		{"host consumes all", func(c *AgentHostCapability) { c.HostInputTokens = 160000 }, ErrContextBudgetExceeded},
		{"huge framing", func(c *AgentHostCapability) { c.FramingTokens = int(^uint(0) >> 1) }, ErrContextBudgetExceeded},
		{"reserve exceeds window", func(c *AgentHostCapability) { c.ReservedTokens = 200001 }, ErrContextBudgetExceeded},
		{"immediate compact", func(c *AgentHostCapability) { c.AutoCompactTokens = 1 }, ErrContextBudgetExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := budgetCapability(200000)
			tc.mutate(&c)
			_, err := c.ResolveBudget(ProviderCodex, "synthetic", 800000, AgentTokenUsage{Exact: true, Tokenizer: "synthetic-counter"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
	for _, usage := range []AgentTokenUsage{{Exact: false, Tokenizer: "synthetic-counter"}, {Exact: true, Tokenizer: "different"}, {Exact: true, Tokenizer: "synthetic-counter", Tokens: -1}} {
		c := budgetCapability(200000)
		if _, err := c.ResolveBudget(c.Provider, c.Model, 800000, usage); err == nil {
			t.Fatal("accepted invalid counter")
		}
	}
}

func TestAgentBudgetIdentityAndLegacyPackageCompatibility(t *testing.T) {
	p := AgentContextPackage{Version: AgentContextVersion, Delivery: "prepared", Policy: MemoryInputPolicy(), Content: AgentContextContent{Notice: "fixture"}}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var old map[string]json.RawMessage
	if err = json.Unmarshal(raw, &old); err != nil {
		t.Fatal(err)
	}
	if _, present := old["budget"]; present {
		t.Fatal("nil budget changed old wire shape")
	}
	p.ID, _ = p.Digest()
	if err = p.ValidateIdentity(); err != nil {
		t.Fatal(err)
	}
	c := budgetCapability(200000)
	b, err := c.ResolveBudget(c.Provider, c.Model, 800000, AgentTokenUsage{Exact: true, Tokenizer: c.Tokenizer})
	if err != nil {
		t.Fatal(err)
	}
	p.Policy = InputPolicy{Version: 1, Mode: "history", BudgetTokens: 800000, Source: "explicit"}
	p.Capability = "verified_for_preparation"
	p.Budget = &b
	p.Usage = AgentTokenUsage{Exact: true, Tokenizer: c.Tokenizer, Tokens: 100}
	p.ID, _ = p.Digest()
	if err = p.ValidateIdentity(); err != nil {
		t.Fatal(err)
	}
	p.Budget.EffectiveTokens++
	if err = p.ValidateIdentity(); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("budget not hash bound: %v", err)
	}
	p.ID, _ = p.Digest()
	if err = p.ValidateIdentity(); !errors.Is(err, ErrContextBudgetExceeded) {
		t.Fatalf("forged recomputed limits: %v", err)
	}
}
