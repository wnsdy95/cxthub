package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func budgetCapability(window int) AgentHostCapability {
	return AgentHostCapability{Provider: ProviderCodex, Model: "synthetic", HostVersion: "fixture", Tokenizer: "synthetic-counter",
		Verified: true, HostInputKnown: true, AutoCompactKnown: true, Evidence: "synthetic test; no runtime capacity claim", ContextWindow: window}
}

func TestAgentBudgetInitialPromptReservation(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		window, request, host       int
		framing, prompt, reserve    int
		compact, wantLimit, wantCap int
	}{
		{"800k includes prompt", 1000000, 800000, 0, 0, 20000, 0, 0, 800000, 780000},
		{"80 percent with overhead", 200000, 800000, 10000, 123, 2000, 0, 0, 160000, 147877},
		{"requested below capacity", 200000, 100000, 10000, 123, 2000, 0, 0, 160000, 100000},
		{"output reservation", 1000000, 800000, 10000, 100, 20000, 300000, 0, 700000, 669900},
		{"compaction threshold", 1000000, 800000, 10000, 100, 20000, 0, 700000, 699999, 669899},
		{"one token left", 1000000, 800000, 10000, 100, 789899, 0, 0, 800000, 1},
		{"larger window", 2000000, 800000, 0, 0, 20000, 0, 0, 1600000, 800000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := budgetCapability(tc.window)
			c.HostInputTokens, c.FramingTokens, c.InitialPromptTokens = tc.host, tc.framing, tc.prompt
			c.ReservedTokens, c.AutoCompactTokens = tc.reserve, tc.compact
			u := AgentTokenUsage{Exact: true, Tokenizer: c.Tokenizer}
			b, err := c.ResolveBudget(c.Provider, c.Model, tc.request, u)
			if err != nil {
				t.Fatal(err)
			}
			if b.InitialInputLimit != tc.wantLimit || b.EffectiveTokens != tc.wantCap || b.InitialPromptTokens != tc.prompt || b.HostInputTokens != tc.host || b.FramingTokens != tc.framing {
				t.Fatalf("unexpected reservation accounting: %+v", b)
			}
			u.Tokens = tc.wantCap
			if err := b.Validate(c.Provider, c.Model, tc.request, u); err != nil {
				t.Fatal(err)
			}
			u.Tokens++
			if err := b.Validate(c.Provider, c.Model, tc.request, u); !errors.Is(err, ErrContextBudgetExceeded) {
				t.Fatalf("accepted package above remaining prompt capacity: %v", err)
			}
		})
	}
}

func TestAgentBudgetInitialPromptArithmeticExtremes(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	minInt := -maxInt - 1
	// This is floor(80% of MaxInt), using the complementary fraction as an
	// independent check of ResolveBudget's multiplication and rounding.
	maxLimit := maxInt - maxInt/5
	if maxInt%5 != 0 {
		maxLimit--
	}
	for _, tc := range []struct {
		name                          string
		window, host, framing, prompt int
		wantCapacity                  int
	}{
		{"max window small prompt", maxInt, 0, 0, 1, MaxAgentContextTokens},
		{"max window one token left", maxInt, maxLimit - 4, 1, 2, 1},
		{"max window prompt exhausts capacity", maxInt, maxLimit - 4, 1, 3, 0},
		{"max window oversized prompt", maxInt, 1, 1, maxInt, 0},
		{"max host", maxInt, maxInt, 0, maxInt, 0},
		{"max framing", maxInt, 1, maxInt, maxInt, 0},
		{"max prompt", 1000000, 0, 0, maxInt, 0},
		{"negative prompt", 1000000, 0, 0, -1, 0},
		{"min prompt", 1000000, 0, 0, minInt, 0},
		{"exact 80 percent", 1000000, 0, 0, 800000, 0},
		{"over 80 percent", 1000000, 0, 0, 800001, 0},
		{"over after host and framing", 1000000, 10000, 100, 789901, 0},
		{"single token window", 1, 0, 0, 0, 0},
		{"two token window reserved", 2, 0, 0, 1, 0},
		{"two token window unreserved", 2, 0, 0, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := budgetCapability(tc.window)
			c.HostInputTokens, c.FramingTokens, c.InitialPromptTokens = tc.host, tc.framing, tc.prompt
			u := AgentTokenUsage{Exact: true, Tokenizer: c.Tokenizer}
			b, err := c.ResolveBudget(c.Provider, c.Model, MaxAgentContextTokens, u)
			if tc.wantCapacity == 0 {
				if !errors.Is(err, ErrContextBudgetExceeded) {
					t.Fatalf("got %+v %v; want budget exceeded", b, err)
				}
				return
			}
			if err != nil || b.EffectiveTokens != tc.wantCapacity {
				t.Fatalf("got %+v %v; want capacity %d", b, err, tc.wantCapacity)
			}
			if tc.window == maxInt && b.InitialInputLimit != maxLimit {
				t.Fatalf("overflow or bad rounding: limit %d; want %d", b.InitialInputLimit, maxLimit)
			}
			u.Tokens = b.EffectiveTokens
			if err := b.Validate(c.Provider, c.Model, MaxAgentContextTokens, u); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAgentBudgetInitialPromptValidationAndLegacyWire(t *testing.T) {
	c := budgetCapability(1000000)
	u := AgentTokenUsage{Exact: true, Tokens: 100, Tokenizer: c.Tokenizer}
	b, err := c.ResolveBudget(c.Provider, c.Model, MaxAgentContextTokens, u)
	if err != nil {
		t.Fatal(err)
	}
	const legacyBudgetJSON = `{"provider":"codex","model":"synthetic","host_version":"fixture","tokenizer":"synthetic-counter","requested_tokens":800000,"effective_tokens":800000,"context_window":1000000,"initial_input_limit":800000,"host_input_tokens":0,"framing_tokens":0,"reserved_tokens":0,"auto_compact_tokens":0}`
	raw, err := json.Marshal(b)
	if err != nil || string(raw) != legacyBudgetJSON {
		t.Fatalf("zero prompt changed legacy budget JSON: %s %v", raw, err)
	}
	p := initialPromptPackage(t, AgentInitialPrompt{}, 0)
	p.ID = ""
	p.Usage = u
	p.Budget = nil
	withoutBudget, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	// The old optional Budget field was the last serialized package field.
	legacyReceipt := strings.TrimSuffix(string(withoutBudget), "}") + `,"budget":` + legacyBudgetJSON + `}`
	p.Budget = &b
	raw, err = json.Marshal(p)
	if err != nil || string(raw) != legacyReceipt {
		t.Fatalf("zero prompt changed legacy receipt: %s %v", raw, err)
	}
	digest, err := p.Digest()
	if err != nil || digest != HashContent([]byte(legacyReceipt)) {
		t.Fatalf("zero prompt changed legacy digest: %v", err)
	}

	b.InitialPromptTokens = 1
	if err := b.Validate(c.Provider, c.Model, MaxAgentContextTokens, u); !errors.Is(err, ErrContextBudgetExceeded) {
		t.Fatalf("receipt failed to reconstruct prompt subtraction: %v", err)
	}
	b.EffectiveTokens--
	b.AdjustmentReason = "model_window_80_percent"
	if err := b.Validate(c.Provider, c.Model, MaxAgentContextTokens, u); err != nil {
		t.Fatal(err)
	}
	b.InitialPromptTokens = -1
	if err := b.Validate(c.Provider, c.Model, MaxAgentContextTokens, u); !errors.Is(err, ErrContextBudgetExceeded) {
		t.Fatalf("negative receipt prompt count accepted: %v", err)
	}
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
