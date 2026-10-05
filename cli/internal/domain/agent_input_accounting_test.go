package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func estimateCapability() AgentHostCapability {
	return AgentHostCapability{Provider: ProviderClaude, Model: "native-model", HostVersion: "2.1.287", Verified: true,
		Evidence: "owned synthetic summary", ContextWindow: 200000, Tokenizer: UTF8ByteBoundCounter,
		InputAccountingPolicy: NativeEstimateReserveV1, RuntimeScope: HashContent([]byte("owned fresh session")),
		BaselineInputEstimateTokens: 5000, BaselineInputMeasurement: NativeLocalEstimate, FramingAllowanceTokens: 48,
		InitialPromptTokens: 2000}
}

func estimateUsage(n int) AgentTokenUsage {
	return AgentTokenUsage{Tokens: n, Tokenizer: UTF8ByteBoundCounter, Scope: "text", Reason: "model_tokenizer_unavailable"}
}

func TestNativeEstimateBudgetAndFinalAdmission(t *testing.T) {
	for _, tc := range []struct {
		name                                                               string
		window, reserve, threshold, request, wantLimit, wantBudget, margin int
	}{
		{"baseline", 200000, 0, 0, 800000, 160000, 136952, 16000},
		{"five percent reserve", 1000000, 0, 0, 800000, 800000, 742952, 50000},
		{"known threshold", 200000, 0, 150000, 800000, 149999, 126951, 16000},
		{"output reserve", 200000, 60000, 0, 800000, 140000, 116952, 16000},
		{"requested smaller", 200000, 0, 0, 100000, 160000, 100000, 16000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := estimateCapability()
			c.ContextWindow, c.ReservedTokens = tc.window, tc.reserve
			c.AutoCompactKnown, c.AutoCompactTokens = tc.threshold > 0, tc.threshold
			b, err := c.ResolveBudget(c.Provider, c.Model, tc.request, estimateUsage(2000))
			if err != nil || b.InitialInputLimit != tc.wantLimit || b.EffectiveTokens != tc.wantBudget || b.OverheadAllowanceTokens != tc.margin || !b.HostInputUnverified || b.AutoCompactUnverified != !c.AutoCompactKnown || b.HostInputTokens != 0 || b.FramingTokens != 0 {
				t.Fatalf("bad allowance budget: %+v %v", b, err)
			}
			u := estimateUsage(b.EffectiveTokens)
			if err := b.Validate(c.Provider, c.Model, tc.request, u); err != nil {
				t.Fatal(err)
			}
			e := AgentNativeInputEstimate{Provider: c.Provider, Model: c.Model, HostVersion: c.HostVersion, RuntimeScope: c.RuntimeScope,
				ContextWindow: c.ContextWindow, AutoCompactKnown: c.AutoCompactKnown, AutoCompactTokens: c.AutoCompactTokens,
				Measurement: NativeLocalEstimate, TotalTokens: tc.wantLimit - c.InitialPromptTokens - tc.margin}
			if err := b.ValidateNativeInputEstimate(u, e); err != nil {
				t.Fatal("reference/baseline charged twice", err)
			}
			e.TotalTokens++
			if err := b.ValidateNativeInputEstimate(u, e); !errors.Is(err, ErrContextBudgetExceeded) {
				t.Fatal("final estimate exceeds reserved limit", err)
			}
			u.Tokens++
			if err := b.Validate(c.Provider, c.Model, tc.request, u); !errors.Is(err, ErrContextBudgetExceeded) {
				t.Fatal("package allowance exceeds budget", err)
			}
		})
	}
}

func TestNativeEstimateRejectsCapabilityAndCounterDrift(t *testing.T) {
	for name, mutate := range map[string]func(*AgentHostCapability, *AgentTokenUsage){
		"provider":                     func(c *AgentHostCapability, _ *AgentTokenUsage) { c.Provider = ProviderCodex },
		"missing version":              func(c *AgentHostCapability, _ *AgentTokenUsage) { c.HostVersion = "" },
		"scope":                        func(c *AgentHostCapability, _ *AgentTokenUsage) { c.RuntimeScope = "" },
		"no measurement":               func(c *AgentHostCapability, _ *AgentTokenUsage) { c.BaselineInputMeasurement = "" },
		"exact measurement":            func(c *AgentHostCapability, _ *AgentTokenUsage) { c.BaselineInputMeasurement = "exact" },
		"negative baseline":            func(c *AgentHostCapability, _ *AgentTokenUsage) { c.BaselineInputEstimateTokens = -1 },
		"negative framing":             func(c *AgentHostCapability, _ *AgentTokenUsage) { c.FramingAllowanceTokens = -1 },
		"known host":                   func(c *AgentHostCapability, _ *AgentTokenUsage) { c.HostInputKnown = true },
		"exact host count":             func(c *AgentHostCapability, _ *AgentTokenUsage) { c.HostInputTokens = 5000 },
		"exact framing count":          func(c *AgentHostCapability, _ *AgentTokenUsage) { c.FramingTokens = 48 },
		"unknown threshold with value": func(c *AgentHostCapability, _ *AgentTokenUsage) { c.AutoCompactTokens = 160000 },
		"calibration":                  func(c *AgentHostCapability, _ *AgentTokenUsage) { c.Calibration.Scope = c.RuntimeScope },
		"fake exact":                   func(_ *AgentHostCapability, u *AgentTokenUsage) { u.Exact = true },
		"counter mismatch":             func(_ *AgentHostCapability, u *AgentTokenUsage) { u.Tokenizer = "other" },
		"request scope":                func(_ *AgentHostCapability, u *AgentTokenUsage) { u.Scope = "request" },
		"missing reason":               func(_ *AgentHostCapability, u *AgentTokenUsage) { u.Reason = "" },
		"work fallback":                func(_ *AgentHostCapability, u *AgentTokenUsage) { u.Reason = "tokenizer_work_limit" },
		"unicode fallback":             func(_ *AgentHostCapability, u *AgentTokenUsage) { u.Reason = "unicode_classification_unavailable" },
	} {
		t.Run(name, func(t *testing.T) {
			c, u := estimateCapability(), estimateUsage(100)
			mutate(&c, &u)
			if _, err := c.ResolveBudget(c.Provider, c.Model, 800000, u); !errors.Is(err, ErrProviderCapabilityUnknown) {
				t.Fatal("invalid native accounting admitted", err)
			}
		})
	}
	c := estimateCapability()
	c.HostVersion = "adapter-owned-supported-version"
	c.BaselineInputEstimateTokens = 0
	c.AutoCompactKnown = true // native known-disabled, distinct from unknown
	if b, err := c.ResolveBudget(c.Provider, c.Model, 800000, estimateUsage(1)); err != nil || b.AutoCompactUnverified || b.BaselineInputMeasurement != NativeLocalEstimate {
		t.Fatal("observed zero/disabled rejected", err)
	}
}

func TestNativeEstimateArithmeticOverflowAndMeasurementBinding(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, field := range []string{"baseline", "framing", "question", "reserve"} {
		c := estimateCapability()
		switch field {
		case "baseline":
			c.BaselineInputEstimateTokens = maxInt
		case "framing":
			c.FramingAllowanceTokens = maxInt
		case "question":
			c.InitialPromptTokens = maxInt
		case "reserve":
			c.ReservedTokens = maxInt
		}
		if _, err := c.ResolveBudget(c.Provider, c.Model, 800000, estimateUsage(1)); !errors.Is(err, ErrContextBudgetExceeded) {
			t.Fatal("overflow accepted", field, err)
		}
	}
	c := estimateCapability()
	c.ContextWindow = maxInt
	b, err := c.ResolveBudget(c.Provider, c.Model, 800000, estimateUsage(1))
	if err != nil || b.EffectiveTokens != 800000 || b.InitialInputLimit != maxInt/5*4+maxInt%5*4/5 {
		t.Fatal("large valid window overflow", err)
	}
	c = estimateCapability()
	b, err = c.ResolveBudget(c.Provider, c.Model, 800000, estimateUsage(1))
	if err != nil {
		t.Fatal(err)
	}
	e := AgentNativeInputEstimate{Provider: c.Provider, Model: c.Model, HostVersion: c.HostVersion, RuntimeScope: c.RuntimeScope, ContextWindow: c.ContextWindow, Measurement: NativeLocalEstimate, TotalTokens: 100000}
	for name, mutate := range map[string]func(*AgentNativeInputEstimate){
		"provider":         func(e *AgentNativeInputEstimate) { e.Provider = ProviderCodex },
		"model":            func(e *AgentNativeInputEstimate) { e.Model = "other" },
		"host":             func(e *AgentNativeInputEstimate) { e.HostVersion = "other" },
		"scope":            func(e *AgentNativeInputEstimate) { e.RuntimeScope = HashContent([]byte("another session")) },
		"window":           func(e *AgentNativeInputEstimate) { e.ContextWindow++ },
		"threshold status": func(e *AgentNativeInputEstimate) { e.AutoCompactKnown = true },
		"threshold":        func(e *AgentNativeInputEstimate) { e.AutoCompactTokens = 150000 },
		"measurement":      func(e *AgentNativeInputEstimate) { e.Measurement = "exact" },
		"negative":         func(e *AgentNativeInputEstimate) { e.TotalTokens = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			changed := e
			mutate(&changed)
			if err := b.ValidateNativeInputEstimate(estimateUsage(100), changed); !errors.Is(err, ErrProviderCapabilityUnknown) {
				t.Fatal("foreign estimate accepted", err)
			}
		})
	}
	b.ObservedOverheadTokens = 1
	if err := b.ValidateNativeInputEstimate(estimateUsage(100), e); err == nil {
		t.Fatal("forged measured feedback accepted")
	}
}

func TestNativeEstimateQuestionOwnershipAndBytes(t *testing.T) {
	c := estimateCapability()
	prompt := NewAgentInitialPrompt("\uc548\ub155 👋")
	u := estimateUsage(len(prompt.Text()))
	r, err := NewAgentPromptReservationForCapability(prompt, c, u)
	if err != nil {
		t.Fatal(err)
	}
	c.InitialPromptTokens = u.Tokens
	b, err := c.ResolveBudget(c.Provider, c.Model, 800000, u)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ValidateForBudget(prompt, b); err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(prompt, c.Provider, c.Model, c.Tokenizer, u.Tokens); !errors.Is(err, ErrProviderCapabilityUnknown) {
		t.Fatal("legacy API accepted allowance authority", err)
	}
	if _, err := NewAgentPromptReservation(prompt, c.Provider, c.Model, u); !errors.Is(err, ErrProviderCapabilityUnknown) {
		t.Fatal("legacy constructor accepted allowance", err)
	}
	wrong := u
	wrong.Tokens = len([]rune(prompt.Text()))
	if _, err := NewAgentPromptReservationForCapability(prompt, c, wrong); !errors.Is(err, ErrContextBudgetExceeded) {
		t.Fatal("runes mistaken for bytes", err)
	}
	if err := r.ValidateForBudget(NewAgentInitialPrompt(prompt.Text()+"!"), b); !errors.Is(err, ErrContextBudgetExceeded) {
		t.Fatal("changed question accepted", err)
	}
	for _, field := range []string{"policy", "scope", "model", "tokenizer", "count"} {
		changed := b
		switch field {
		case "policy":
			changed.InputAccountingPolicy = MeasuredInputReserveV1
		case "scope":
			changed.RuntimeScope = HashContent([]byte("other"))
		case "model":
			changed.Model = "other"
		case "tokenizer":
			changed.Tokenizer = "other"
		case "count":
			changed.InitialPromptTokens++
		}
		if err := r.ValidateForBudget(prompt, changed); err == nil {
			t.Fatal("reservation drift", field)
		}
	}
	copy := r
	if err := json.Unmarshal([]byte(`{}`), &r); err != nil {
		t.Fatal(err)
	}
	if err := r.ValidateForBudget(prompt, b); err == nil {
		t.Fatal("decoded authority revived")
	}
	if err := copy.ValidateForBudget(prompt, b); err != nil {
		t.Fatal("copy lost immutable binding", err)
	}
	for _, empty := range []AgentInitialPrompt{{}, NewAgentInitialPrompt("")} {
		reservation, err := NewAgentPromptReservationForCapability(empty, c, estimateUsage(0))
		if err != nil {
			t.Fatal(err)
		}
		zero := b
		zero.InitialPromptTokens = 0
		if err := reservation.ValidateForBudget(empty, zero); err != nil {
			t.Fatal(err)
		}
		other := AgentInitialPrompt{}
		if !empty.Present() {
			other = NewAgentInitialPrompt("")
		}
		if err := reservation.ValidateForBudget(other, zero); err == nil {
			t.Fatal("presence erased")
		}
	}
}

func TestNativeEstimateCannotBecomeMeasuredCalibration(t *testing.T) {
	c := estimateCapability()
	if _, err := c.CalibrationScope(); !errors.Is(err, ErrProviderCapabilityUnknown) {
		t.Fatal(err)
	}
	b, err := c.ResolveBudget(c.Provider, c.Model, 800000, estimateUsage(1))
	if err != nil {
		t.Fatal(err)
	}
	_, retry, err := b.ObserveInput(HashContent([]byte("package")), estimateUsage(1), AgentInputObservation{InitialRequest: true, UsageKnown: true, SubmittedTextExact: true, SubmittedTextTokens: 2001, TotalInputTokens: 10000})
	if !errors.Is(err, ErrProviderCapabilityUnknown) || retry {
		t.Fatal("estimated usage entered exact calibration", err)
	}
	for _, policy := range []string{"", MeasuredInputReserveV1} {
		if kind, err := ValidateAgentTokenAccounting(policy, ProviderClaude, UTF8ByteBoundCounter, estimateUsage(100)); kind != AgentTextExact || !errors.Is(err, ErrProviderCapabilityUnknown) {
			t.Fatal("old policy now accepts allowance", policy, err)
		}
		old := budgetCapability(200000)
		old.InputAccountingPolicy = policy
		if policy != "" {
			old.RuntimeScope = HashContent([]byte("measured"))
			old.Calibration.Scope, _ = old.CalibrationScope()
		}
		for _, field := range []string{"baseline", "measurement", "framing"} {
			changed := old
			switch field {
			case "baseline":
				changed.BaselineInputEstimateTokens = 1
			case "measurement":
				changed.BaselineInputMeasurement = NativeLocalEstimate
			case "framing":
				changed.FramingAllowanceTokens = 1
			}
			if _, err := changed.ResolveBudget(changed.Provider, changed.Model, 800000, AgentTokenUsage{Exact: true, Tokenizer: changed.Tokenizer}); !errors.Is(err, ErrProviderCapabilityUnknown) {
				t.Fatal("old policy ignored estimate fields", field, err)
			}
		}
		raw, err := json.Marshal(old)
		if err != nil || strings.Contains(string(raw), "baseline_input") || strings.Contains(string(raw), "framing_allowance") {
			t.Fatal("zero estimate fields changed old JSON", err)
		}
	}
}
