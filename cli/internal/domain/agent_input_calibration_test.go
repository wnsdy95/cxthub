package domain

import (
	"encoding/json"
	"errors"
	"testing"
)

func measuredCapability(t *testing.T, window int) AgentHostCapability {
	t.Helper()
	c := budgetCapability(window)
	c.HostInputKnown = false
	c.AutoCompactKnown = false
	c.InputAccountingPolicy = MeasuredInputReserveV1
	c.RuntimeScope = HashContent([]byte("synthetic route/account/config/instruction/tool scope"))
	scope, err := c.CalibrationScope()
	if err != nil {
		t.Fatal(err)
	}
	c.Calibration = AgentInputCalibration{Scope: scope}
	return c
}

func TestMeasuredReserveWindowPromptAndFeedback(t *testing.T) {
	for _, tc := range []struct {
		name                                                              string
		window, host, framing, prompt, overhead, ceiling, want, allowance int
	}{
		{"1m initial", 1000000, 0, 0, 2000, 0, 0, 748000, 50000},
		{"200k initial", 200000, 0, 0, 2000, 0, 0, 142000, 16000},
		{"known plus observed", 1000000, 10000, 1000, 2000, 30000, 0, 718000, 69000},
		{"observed below known", 1000000, 10000, 1000, 2000, 1000, 0, 737000, 50000},
		{"observed threshold", 1000000, 0, 0, 2000, 30000, 600000, 518000, 80000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := measuredCapability(t, tc.window)
			c.HostInputKnown = tc.host > 0
			c.HostInputTokens = tc.host
			c.FramingTokens = tc.framing
			c.InitialPromptTokens = tc.prompt
			c.Calibration.OverheadTokens = tc.overhead
			c.Calibration.InputCeilingTokens = tc.ceiling
			u := AgentTokenUsage{Exact: true, Tokenizer: c.Tokenizer, Tokens: tc.want}
			b, err := c.ResolveBudget(c.Provider, c.Model, 800000, u)
			if err != nil {
				t.Fatal(err)
			}
			if b.EffectiveTokens != tc.want || b.OverheadAllowanceTokens != tc.allowance || !b.AutoCompactUnverified {
				t.Fatalf("%+v", b)
			}
			if err = b.Validate(c.Provider, c.Model, 800000, u); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(b)
			var decoded AgentContextBudget
			if err = json.Unmarshal(raw, &decoded); err != nil || decoded != b {
				t.Fatal("wire accounting changed", err)
			}
			if err = decoded.Validate(c.Provider, c.Model, 800000, u); err != nil {
				t.Fatal(err)
			}
			decoded.OverheadAllowanceTokens--
			if !errors.Is(decoded.Validate(c.Provider, c.Model, 800000, u), ErrContextBudgetExceeded) {
				t.Fatal("forged allowance passed")
			}
		})
	}
}

func TestMeasuredReserveStillRequiresRuntimeEvidence(t *testing.T) {
	for name, mutate := range map[string]func(*AgentHostCapability){
		"no window":             func(c *AgentHostCapability) { c.ContextWindow = 0 },
		"unverified":            func(c *AgentHostCapability) { c.Verified = false },
		"no evidence":           func(c *AgentHostCapability) { c.Evidence = "" },
		"no runtime scope":      func(c *AgentHostCapability) { c.RuntimeScope = "" },
		"changed route":         func(c *AgentHostCapability) { c.RuntimeScope = HashContent([]byte("other route")) },
		"changed model":         func(c *AgentHostCapability) { c.Model = "other" },
		"changed host":          func(c *AgentHostCapability) { c.HostVersion = "other" },
		"changed window":        func(c *AgentHostCapability) { c.ContextWindow = 400000 },
		"changed tokenizer":     func(c *AgentHostCapability) { c.Tokenizer = "other" },
		"unknown host count":    func(c *AgentHostCapability) { c.HostInputTokens = 1 },
		"unknown compact count": func(c *AgentHostCapability) { c.AutoCompactTokens = 800000 },
		"negative feedback":     func(c *AgentHostCapability) { c.Calibration.OverheadTokens = -1 },
		"negative ceiling":      func(c *AgentHostCapability) { c.Calibration.InputCeilingTokens = -1 },
		"max overhead":          func(c *AgentHostCapability) { c.Calibration.OverheadTokens = int(^uint(0) >> 1) },
	} {
		t.Run(name, func(t *testing.T) {
			c := measuredCapability(t, 1000000)
			mutate(&c)
			if _, err := c.ResolveBudget(c.Provider, c.Model, 800000, AgentTokenUsage{Exact: true, Tokenizer: c.Tokenizer}); err == nil {
				t.Fatal("accepted invalid evidence")
			}
		})
	}
	c := measuredCapability(t, int(^uint(0)>>1))
	if _, err := c.ResolveBudget(c.Provider, c.Model, 800000, AgentTokenUsage{Exact: true, Tokenizer: c.Tokenizer}); err != nil {
		t.Fatal("overflow", err)
	}
	c = measuredCapability(t, 1000)
	if _, err := c.ResolveBudget(c.Provider, c.Model, 800000, AgentTokenUsage{Exact: true, Tokenizer: c.Tokenizer}); !errors.Is(err, ErrContextBudgetExceeded) {
		t.Fatal("small window must reject margin exhaustion", err)
	}
	c = measuredCapability(t, 1000000)
	c.AutoCompactKnown = true
	c.AutoCompactTokens = 100000
	b, err := c.ResolveBudget(c.Provider, c.Model, 800000, AgentTokenUsage{Exact: true, Tokenizer: c.Tokenizer})
	if err != nil || b.EffectiveTokens != 49999 || b.AutoCompactUnverified {
		t.Fatalf("ignored known trigger: %+v %v", b, err)
	}
}

func TestAgentInitialObservationCalibrationAndRetry(t *testing.T) {
	c := measuredCapability(t, 1000000)
	c.InitialPromptTokens = 2000
	selected := AgentTokenUsage{Exact: true, Tokenizer: c.Tokenizer, Tokens: 100000}
	b, err := c.ResolveBudget(c.Provider, c.Model, 800000, selected)
	if err != nil {
		t.Fatal(err)
	}
	id := HashContent([]byte("prepared package"))
	observation := AgentInputObservation{Scope: c.Calibration.Scope, PackageID: id, InitialRequest: true, Outcome: "completed", ExecutionKnown: true, ExecutionStarted: true, UsageKnown: true, UsageBeforeCompaction: true, SubmittedTextExact: true, SubmittedTextTokens: 102000, TotalInputTokens: 132000}
	next, retry, err := b.ObserveInput(id, selected, observation)
	if err != nil || retry || next.OverheadTokens != 30000 || next.InputCeilingTokens != 0 {
		t.Fatalf("%+v %t %v", next, retry, err)
	}
	c.Calibration = next
	corrected, err := c.ResolveBudget(c.Provider, c.Model, 800000, selected)
	if err != nil || corrected.EffectiveTokens != 718000 {
		t.Fatalf("next budget %+v %v", corrected, err)
	}
	for _, outcome := range []string{"input_rejected", "initial_compaction", "unknown"} {
		o := AgentInputObservation{Scope: c.Calibration.Scope, PackageID: id, InitialRequest: true, Outcome: outcome, ExecutionKnown: true}
		got, retry, err := b.ObserveInput(id, selected, o)
		if err != nil || retry != (outcome == "input_rejected") {
			t.Fatal(outcome, got, retry, err)
		}
		if outcome != "unknown" && got.InputCeilingTokens != 102000 {
			t.Fatalf("failed to reduce from submitted size: %+v", got)
		}
		o.RetryAttempt = 1
		if _, again, err := b.ObserveInput(id, selected, o); err != nil || again {
			t.Fatal("repeated retry permitted", err)
		}
	}
	for name, mutate := range map[string]func(*AgentInputObservation){
		"different package":         func(o *AgentInputObservation) { o.PackageID = HashContent([]byte("other")) },
		"different scope":           func(o *AgentInputObservation) { o.Scope = HashContent([]byte("other")) },
		"later request":             func(o *AgentInputObservation) { o.InitialRequest = false },
		"missing count":             func(o *AgentInputObservation) { o.UsageKnown = false },
		"post compact usage":        func(o *AgentInputObservation) { o.UsageBeforeCompaction = false },
		"cache only total":          func(o *AgentInputObservation) { o.TotalInputTokens = 1 },
		"wrong submitted":           func(o *AgentInputObservation) { o.SubmittedTextTokens++ },
		"inexact submitted":         func(o *AgentInputObservation) { o.SubmittedTextExact = false },
		"rejection after execution": func(o *AgentInputObservation) { o.Outcome = "input_rejected" },
		"negative attempts":         func(o *AgentInputObservation) { o.RetryAttempt = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			o := observation
			mutate(&o)
			if _, retry, err := b.ObserveInput(id, selected, o); err == nil || retry {
				t.Fatal("invalid observation accepted")
			}
		})
	}
}

func TestAgentCalibrationMergeMonotonicIdempotent(t *testing.T) {
	scope := HashContent([]byte("scope"))
	a := AgentInputCalibration{Scope: scope, OverheadTokens: 30000, InputCeilingTokens: 300000}
	b := AgentInputCalibration{Scope: scope, OverheadTokens: 20000, InputCeilingTokens: 200000}
	ab, err := a.Merge(b)
	if err != nil {
		t.Fatal(err)
	}
	ba, err := b.Merge(a)
	if err != nil || ab != ba || ab.OverheadTokens != 30000 || ab.InputCeilingTokens != 200000 {
		t.Fatal("merge lost stricter feedback")
	}
	dup, err := ab.Merge(a)
	if err != nil || dup != ab {
		t.Fatal("duplicate weakened calibration")
	}
	b.Scope = HashContent([]byte("other"))
	if _, err = a.Merge(b); err == nil {
		t.Fatal("merged different runtime")
	}
}

func TestAgentInputObservationUnknownExecutionAndNoUsageTextEvidence(t *testing.T) {
	c := measuredCapability(t, 1000000)
	u := AgentTokenUsage{Exact: true, Tokenizer: c.Tokenizer, Tokens: 100000}
	b, err := c.ResolveBudget(c.Provider, c.Model, 800000, u)
	if err != nil {
		t.Fatal(err)
	}
	id := HashContent([]byte("package"))
	o := AgentInputObservation{Scope: c.Calibration.Scope, PackageID: id, InitialRequest: true, Outcome: "input_rejected"}
	if _, retry, err := b.ObserveInput(id, u, o); err != nil || retry {
		t.Fatal("omitted execution state permitted retry", err)
	}
	o.ExecutionKnown = true
	if _, retry, err := b.ObserveInput(id, u, o); err != nil || !retry {
		t.Fatal("confirmed pre-execution rejection not eligible", err)
	}
	o.SubmittedTextExact = true
	o.SubmittedTextTokens = u.Tokens + 1
	if _, retry, err := b.ObserveInput(id, u, o); err == nil || retry {
		t.Fatal("contradictory local count accepted without native usage")
	}
	o.SubmittedTextTokens = u.Tokens
	if _, retry, err := b.ObserveInput(id, u, o); err != nil || !retry {
		t.Fatal("valid local count requires native usage", err)
	}
	o.SubmittedTextExact = false
	if _, retry, err := b.ObserveInput(id, u, o); err == nil || retry {
		t.Fatal("unverified local count accepted")
	}
}
