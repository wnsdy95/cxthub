package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func catalogEstimateCapability(exact bool) (AgentHostCapability, AgentTokenUsage) {
	c := AgentHostCapability{Provider: ProviderCodex, Model: "stock-model", HostVersion: "stock-host",
		Evidence: "synthetic model cache", ContextWindow: 200000, InputAccountingPolicy: CatalogEstimateReserveV1,
		RuntimeScope: HashContent([]byte("stock route/settings")), WindowEstimateSource: "native_model_cache",
		WindowEstimateObservedAt: "2026-10-09T12:34:56Z", WindowEstimateHash: HashContent([]byte("model cache")), WindowEstimateClientVersion: "0.157.1"}
	u := estimateUsage(100)
	if exact {
		u = AgentTokenUsage{Tokens: 100, Exact: true, Tokenizer: "synthetic-exact-text", Scope: "text"}
	}
	c.Tokenizer = u.Tokenizer
	return c, u
}

func TestCatalogEstimateBudgetBoundaries(t *testing.T) {
	for _, exact := range []bool{false, true} {
		for _, tc := range []struct {
			name                                        string
			window, reserve, compact, question, request int
			limit, effective, margin                    int
		}{
			{"80 percent", 200000, 0, 0, 2000, 800000, 160000, 142000, 16000},
			{"floor", 200003, 0, 0, 2000, 800000, 160002, 142002, 16000},
			{"five percent margin", 1000000, 0, 0, 20000, 800000, 800000, 730000, 50000},
			{"request cap", 2000000, 0, 0, 20000, 800000, 1600000, 800000, 100000},
			{"smaller request", 200000, 0, 0, 2000, 100000, 160000, 100000, 16000},
			{"output reserve", 200000, 60000, 0, 2000, 800000, 140000, 122000, 16000},
			{"compaction", 200000, 0, 150000, 2000, 800000, 149999, 131999, 16000},
			{"one token left", 200000, 0, 0, 143999, 800000, 160000, 1, 16000},
		} {
			t.Run(map[bool]string{false: "byte/", true: "exact/"}[exact]+tc.name, func(t *testing.T) {
				c, u := catalogEstimateCapability(exact)
				c.ContextWindow, c.ReservedTokens, c.AutoCompactTokens, c.InitialPromptTokens = tc.window, tc.reserve, tc.compact, tc.question
				c.AutoCompactKnown = tc.compact > 0
				b, err := c.ResolveBudget(c.Provider, c.Model, tc.request, u)
				if err != nil || b.InitialInputLimit != tc.limit || b.EffectiveTokens != tc.effective || b.OverheadAllowanceTokens != tc.margin {
					t.Fatalf("budget: %+v %v", b, err)
				}
				if !b.HostInputUnverified || b.AutoCompactUnverified != !c.AutoCompactKnown || b.HostInputTokens != 0 || b.FramingTokens != 0 || b.ObservedOverheadTokens != 0 || b.ObservedInputCeilingTokens != 0 || b.BaselineInputEstimateTokens != 0 || b.BaselineInputMeasurement != "" || b.FramingAllowanceTokens != 0 {
					t.Fatal("invented input evidence", b)
				}
				if b.WindowEstimateSource != c.WindowEstimateSource || b.WindowEstimateObservedAt != c.WindowEstimateObservedAt || b.WindowEstimateHash != c.WindowEstimateHash || b.WindowEstimateClientVersion != c.WindowEstimateClientVersion || b.ExpectedPreparationCapability() != "estimated_for_preparation" || b.capability().Verified {
					t.Fatal("lost estimate provenance", b)
				}
				u.Tokens = b.EffectiveTokens
				if err := b.Validate(c.Provider, c.Model, tc.request, u); err != nil {
					t.Fatal(err)
				}
				u.Tokens++
				if err := b.Validate(c.Provider, c.Model, tc.request, u); !errors.Is(err, ErrContextBudgetExceeded) {
					t.Fatal("overflow accepted", err)
				}
			})
		}
	}
	for _, mutate := range []func(*AgentHostCapability, *int){
		func(_ *AgentHostCapability, n *int) { *n = 800001 },
		func(_ *AgentHostCapability, n *int) { *n = 0 },
		func(c *AgentHostCapability, _ *int) { c.InitialPromptTokens = 144000 },
		func(c *AgentHostCapability, _ *int) { c.InitialPromptTokens = -1 },
		func(c *AgentHostCapability, _ *int) { c.InitialPromptTokens = int(^uint(0) >> 1) },
		func(c *AgentHostCapability, _ *int) { c.ReservedTokens = c.ContextWindow },
		func(c *AgentHostCapability, _ *int) { c.ContextWindow = 20000 },
	} {
		c, u := catalogEstimateCapability(true)
		n := 800000
		mutate(&c, &n)
		if _, err := c.ResolveBudget(c.Provider, c.Model, n, u); !errors.Is(err, ErrContextBudgetExceeded) {
			t.Fatal("invalid capacity accepted", err)
		}
	}
}

func TestCatalogEstimateRejectsCapabilityMislabelAndBadProvenance(t *testing.T) {
	for name, mutate := range map[string]func(*AgentHostCapability){
		"verified":                 func(c *AgentHostCapability) { c.Verified = true },
		"claude":                   func(c *AgentHostCapability) { c.Provider = ProviderClaude },
		"missing source":           func(c *AgentHostCapability) { c.WindowEstimateSource = "" },
		"config source":            func(c *AgentHostCapability) { c.WindowEstimateSource = "user_config" },
		"missing timestamp":        func(c *AgentHostCapability) { c.WindowEstimateObservedAt = "" },
		"local timestamp":          func(c *AgentHostCapability) { c.WindowEstimateObservedAt = "2026-10-09T12:34:56" },
		"invalid date":             func(c *AgentHostCapability) { c.WindowEstimateObservedAt = "2026-02-30T12:34:56Z" },
		"missing hash":             func(c *AgentHostCapability) { c.WindowEstimateHash = "" },
		"invalid hash":             func(c *AgentHostCapability) { c.WindowEstimateHash = "model-cache" },
		"missing client version":   func(c *AgentHostCapability) { c.WindowEstimateClientVersion = "" },
		"blank client version":     func(c *AgentHostCapability) { c.WindowEstimateClientVersion = " \t" },
		"missing runtime":          func(c *AgentHostCapability) { c.RuntimeScope = "" },
		"missing evidence":         func(c *AgentHostCapability) { c.Evidence = "" },
		"known host":               func(c *AgentHostCapability) { c.HostInputKnown = true },
		"host count":               func(c *AgentHostCapability) { c.HostInputTokens = 10 },
		"framing":                  func(c *AgentHostCapability) { c.FramingTokens = 10 },
		"unknown compaction count": func(c *AgentHostCapability) { c.AutoCompactTokens = 150000 },
		"baseline":                 func(c *AgentHostCapability) { c.BaselineInputEstimateTokens = 10 },
		"baseline tag":             func(c *AgentHostCapability) { c.BaselineInputMeasurement = NativeLocalEstimate },
		"framing allowance":        func(c *AgentHostCapability) { c.FramingAllowanceTokens = 10 },
		"calibration":              func(c *AgentHostCapability) { c.Calibration.Scope = c.RuntimeScope },
		"unknown policy":           func(c *AgentHostCapability) { c.InputAccountingPolicy = "future" },
		"unverified legacy":        func(c *AgentHostCapability) { c.InputAccountingPolicy = "" },
		"unverified measured":      func(c *AgentHostCapability) { c.InputAccountingPolicy = MeasuredInputReserveV1 },
	} {
		t.Run(name, func(t *testing.T) {
			c, u := catalogEstimateCapability(true)
			mutate(&c)
			if _, err := c.ResolveBudget(c.Provider, c.Model, 800000, u); !errors.Is(err, ErrProviderCapabilityUnknown) {
				t.Fatal("invalid capability accepted", err)
			}
		})
	}
	// Catalog provenance cannot be relabeled as verified native/measured evidence.
	for _, policy := range []string{"", MeasuredInputReserveV1, NativeEstimateReserveV1} {
		c, u := catalogEstimateCapability(true)
		c.InputAccountingPolicy, c.Verified = policy, true
		if _, err := c.ResolveBudget(c.Provider, c.Model, 800000, u); !errors.Is(err, ErrProviderCapabilityUnknown) {
			t.Fatal("catalog relabeled as", policy, err)
		}
		if _, err := c.CalibrationScope(); !errors.Is(err, ErrProviderCapabilityUnknown) {
			t.Fatal("catalog provenance entered legacy calibration", policy, err)
		}
	}
}

func TestCatalogEstimateTextAccountingIsExplicit(t *testing.T) {
	for _, exact := range []bool{false, true} {
		c, u := catalogEstimateCapability(exact)
		kind, err := ValidateAgentTokenAccounting(c.InputAccountingPolicy, c.Provider, c.Tokenizer, u)
		if err != nil || (kind == AgentTextExact) != exact {
			t.Fatal(kind, err)
		}
		for name, mutate := range map[string]func(*AgentTokenUsage){
			"wrong counter":        func(u *AgentTokenUsage) { u.Tokenizer = "other" },
			"request scope":        func(u *AgentTokenUsage) { u.Scope = "request" },
			"work limit":           func(u *AgentTokenUsage) { u.Reason = "tokenizer_work_limit" },
			"exactness mislabeled": func(u *AgentTokenUsage) { u.Exact = !u.Exact },
		} {
			changed := u
			mutate(&changed)
			if _, err := ValidateAgentTokenAccounting(c.InputAccountingPolicy, c.Provider, c.Tokenizer, changed); !errors.Is(err, ErrProviderCapabilityUnknown) {
				t.Fatal(name, err)
			}
		}
	}
	u := estimateUsage(100)
	for _, policy := range []string{"", MeasuredInputReserveV1, NativeEstimateReserveV1} {
		if _, err := ValidateAgentTokenAccounting(policy, ProviderCodex, u.Tokenizer, u); !errors.Is(err, ErrProviderCapabilityUnknown) {
			t.Fatal("weakened Codex policy", policy, err)
		}
	}
	u.Scope, u.Reason = "", ""
	if _, err := ValidateAgentTokenAccounting(CatalogEstimateReserveV1, ProviderCodex, u.Tokenizer, u); !errors.Is(err, ErrProviderCapabilityUnknown) {
		t.Fatal("generic fallback accepted", err)
	}
}

func TestCatalogEstimateQuestionReceiptAndCalibration(t *testing.T) {
	for _, exact := range []bool{false, true} {
		c, u := catalogEstimateCapability(exact)
		question := NewAgentInitialPrompt("\uacc4\uc18d\ud574 👋\n")
		u.Tokens = len(question.Text())
		r, err := NewAgentPromptReservationForCapability(question, c, u)
		if err != nil {
			t.Fatal(err)
		}
		c.InitialPromptTokens = u.Tokens
		b, err := c.ResolveBudget(c.Provider, c.Model, 800000, u)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.ValidateForBudget(question, b); err != nil {
			t.Fatal(err)
		}
		if err := r.ValidateForBudget(NewAgentInitialPrompt("\ub2e4\ub978\ub9d0 👋\n"), b); !errors.Is(err, ErrContextBudgetExceeded) {
			t.Fatal(err)
		}
		if err := r.Validate(question, c.Provider, c.Model, c.Tokenizer, u.Tokens); !errors.Is(err, ErrProviderCapabilityUnknown) {
			t.Fatal("legacy authority accepted", err)
		}
		for _, mutate := range []func(*AgentContextBudget){
			func(b *AgentContextBudget) { b.WindowEstimateSource = "other" },
			func(b *AgentContextBudget) { b.WindowEstimateObservedAt = "2026-10-09T12:34:57Z" },
			func(b *AgentContextBudget) { b.WindowEstimateHash = HashContent([]byte("different cache")) },
			func(b *AgentContextBudget) { b.WindowEstimateClientVersion = "0.162.0" },
			func(b *AgentContextBudget) { b.RuntimeScope = HashContent([]byte("different runtime")) },
			func(b *AgentContextBudget) { b.InputAccountingPolicy = MeasuredInputReserveV1 },
		} {
			changed := b
			mutate(&changed)
			if err := r.ValidateForBudget(question, changed); !errors.Is(err, ErrProviderCapabilityUnknown) {
				t.Fatal("unbound provenance", err)
			}
		}
		p := AgentContextPackage{Provider: c.Provider, Version: 1, Policy: InputPolicy{Version: 1, Mode: "history", BudgetTokens: 800000, Source: "explicit"},
			Delivery: "prepared", Capability: b.ExpectedPreparationCapability(), Budget: &b, Content: AgentContextContent{Notice: "synthetic catalog estimate"}}
		p.BindInitialPrompt(r)
		prompt, err := p.Prompt()
		if err != nil {
			t.Fatal(err)
		}
		u.Tokens = len(prompt)
		p.Usage = u
		p.ID, err = p.Digest()
		if err != nil {
			t.Fatal(err)
		}
		if err := p.ValidateIdentity(); err != nil {
			t.Fatal(err)
		}
		raw, err := p.Artifact()
		if err != nil || strings.Contains(string(raw), question.Text()) {
			t.Fatal("receipt error or leaked question", err)
		}
		var decoded AgentContextPackage
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if *decoded.Budget != b || decoded.Capability != p.Capability {
			t.Fatal("lost estimate receipt")
		}
		if err := decoded.ValidateIdentity(); err != nil {
			t.Fatal(err)
		}
		if err := decoded.ValidateInitialPrompt(question); !errors.Is(err, ErrProviderCapabilityUnknown) {
			t.Fatal("decoded authority", err)
		}
		for _, status := range []string{"verified_for_preparation", "unknown"} {
			changed := p
			changed.Capability = status
			changed.ID, _ = changed.Digest()
			if err := changed.ValidateIdentity(); !errors.Is(err, ErrAgentContextUnavailable) {
				t.Fatal("mislabeled receipt", err)
			}
		}
		p.Budget = nil
		p.ID, _ = p.Digest()
		if err := p.ValidateIdentity(); !errors.Is(err, ErrAgentContextUnavailable) {
			t.Fatal("estimate without budget", err)
		}
		if _, err := c.CalibrationScope(); !errors.Is(err, ErrProviderCapabilityUnknown) {
			t.Fatal("catalog calibration", err)
		}
		if _, err := b.capability().CalibrationScope(); !errors.Is(err, ErrProviderCapabilityUnknown) {
			t.Fatal("receipt calibration", err)
		}
		if _, retry, err := b.ObserveInput(p.ID, u, AgentInputObservation{InitialRequest: true, UsageKnown: true, SubmittedTextExact: true}); !errors.Is(err, ErrProviderCapabilityUnknown) || retry {
			t.Fatal("catalog feedback reused", err)
		}
		if err := b.ValidateNativeInputEstimate(u, AgentNativeInputEstimate{}); !errors.Is(err, ErrProviderCapabilityUnknown) {
			t.Fatal("catalog accepted native baseline", err)
		}
	}
}

func TestCatalogEstimateLegacyReservationCannotAuthorizeEvenAbsentQuestion(t *testing.T) {
	c, u := catalogEstimateCapability(true)
	u.Tokens = 0
	b, err := c.ResolveBudget(c.Provider, c.Model, 800000, u)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := NewAgentPromptReservation(AgentInitialPrompt{}, c.Provider, c.Model, u)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []AgentPromptReservation{legacy, {}} {
		if err := r.ValidateForBudget(AgentInitialPrompt{}, b); !errors.Is(err, ErrProviderCapabilityUnknown) {
			t.Fatal("legacy reservation became estimate authority", err)
		}
	}
	for _, prompt := range []AgentInitialPrompt{{}, NewAgentInitialPrompt("")} {
		r, err := NewAgentPromptReservationForCapability(prompt, c, u)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.ValidateForBudget(prompt, b); err != nil {
			t.Fatal(err)
		}
		other := AgentInitialPrompt{}
		if !prompt.Present() {
			other = NewAgentInitialPrompt("")
		}
		if err := r.ValidateForBudget(other, b); !errors.Is(err, ErrContextBudgetExceeded) {
			t.Fatal("question presence lost", err)
		}
	}
}

func TestCatalogEstimateClientVersionProvenance(t *testing.T) {
	// Compatibility belongs to the adapter, including future reviewed writers.
	for _, version := range []string{"0.157.1", "0.162.0", "future-reviewed-writer"} {
		c, u := catalogEstimateCapability(true)
		c.HostVersion, c.WindowEstimateClientVersion = "0.157.1", version
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		var decoded AgentHostCapability
		if err := json.Unmarshal(raw, &decoded); err != nil || decoded != c {
			t.Fatal("capability provenance lost", err)
		}
		b, err := decoded.ResolveBudget(c.Provider, c.Model, 800000, u)
		if err != nil || b.HostVersion != "0.157.1" || b.WindowEstimateClientVersion != version {
			t.Fatal("cache writer conflated with executing host", err)
		}
		if err := b.Validate(c.Provider, c.Model, 800000, u); err != nil {
			t.Fatal("budget reconstruction lost writer", err)
		}
		b.WindowEstimateClientVersion = ""
		if err := b.Validate(c.Provider, c.Model, 800000, u); !errors.Is(err, ErrProviderCapabilityUnknown) {
			t.Fatal("budget accepted missing writer", err)
		}
	}
	for _, policy := range []string{"", MeasuredInputReserveV1, NativeEstimateReserveV1} {
		c := budgetCapability(200000)
		c.InputAccountingPolicy = policy
		u := AgentTokenUsage{Tokens: 100, Exact: true, Tokenizer: c.Tokenizer}
		if policy == MeasuredInputReserveV1 {
			c.RuntimeScope = HashContent([]byte("measured runtime"))
			var err error
			c.Calibration.Scope, err = c.CalibrationScope()
			if err != nil {
				t.Fatal(err)
			}
		} else if policy == NativeEstimateReserveV1 {
			c, u = estimateCapability(), estimateUsage(100)
		}
		b, err := c.ResolveBudget(c.Provider, c.Model, 800000, u)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(b)
		if err != nil || strings.Contains(string(raw), "window_estimate_client_version") {
			t.Fatal("legacy receipt changed", err)
		}
		c.WindowEstimateClientVersion, b.WindowEstimateClientVersion = "0.157.1", "0.157.1"
		if _, err := c.ResolveBudget(c.Provider, c.Model, 800000, u); !errors.Is(err, ErrProviderCapabilityUnknown) {
			t.Fatal("other policy carried catalog writer", policy, err)
		}
		if err := b.Validate(c.Provider, c.Model, 800000, u); !errors.Is(err, ErrProviderCapabilityUnknown) {
			t.Fatal("other budget carried catalog writer", policy, err)
		}
		if _, err := c.CalibrationScope(); !errors.Is(err, ErrProviderCapabilityUnknown) {
			t.Fatal("writer provenance entered calibration", policy, err)
		}
	}
}
