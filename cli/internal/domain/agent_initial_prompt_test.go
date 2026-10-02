package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestAgentInitialPromptPresenceAndExactBytes(t *testing.T) {
	var absent AgentInitialPrompt
	if absent.Present() || absent.Text() != "" {
		t.Fatal("zero prompt is not absent")
	}
	for _, text := range []string{"", " ", "hello\n", "\ud55c\uae00\x00\xff"} {
		prompt := NewAgentInitialPrompt(text)
		if !prompt.Present() || prompt.Text() != text {
			t.Fatal("constructor changed prompt presence or bytes")
		}
	}
}

func TestAgentPromptReservationBindsExactInputAndAccounting(t *testing.T) {
	prompt := NewAgentInitialPrompt("first prompt\n")
	usage := AgentTokenUsage{Exact: true, Tokens: 3, Tokenizer: "exact-counter"}
	for _, provider := range []ProviderKind{ProviderCodex, ProviderClaude} {
		res, err := NewAgentPromptReservation(prompt, provider, "resolved-model", usage)
		if err != nil {
			t.Fatal(err)
		}
		if err := res.Validate(NewAgentInitialPrompt(prompt.Text()), provider, "resolved-model", usage.Tokenizer, usage.Tokens); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name      string
			prompt    AgentInitialPrompt
			provider  ProviderKind
			model     string
			tokenizer string
			tokens    int
		}{
			{"same count different text", NewAgentInitialPrompt("other prompt\n"), provider, "resolved-model", usage.Tokenizer, 3},
			{"trailing newline removed", NewAgentInitialPrompt("first prompt"), provider, "resolved-model", usage.Tokenizer, 3},
			{"absent", AgentInitialPrompt{}, provider, "resolved-model", usage.Tokenizer, 3},
			{"explicit empty", NewAgentInitialPrompt(""), provider, "resolved-model", usage.Tokenizer, 3},
			{"provider", prompt, ProviderUnknown, "resolved-model", usage.Tokenizer, 3},
			{"model", prompt, provider, "other-model", usage.Tokenizer, 3},
			{"tokenizer", prompt, provider, "resolved-model", "other-counter", 3},
			{"missing tokenizer", prompt, provider, "resolved-model", "", 3},
			{"count", prompt, provider, "resolved-model", usage.Tokenizer, 4},
			{"negative count", prompt, provider, "resolved-model", usage.Tokenizer, -1},
		} {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				if err := res.Validate(tc.prompt, tc.provider, tc.model, tc.tokenizer, tc.tokens); err == nil {
					t.Fatal("accepted mismatched reservation")
				}
			})
		}
	}
}

func TestAgentPromptReservationRequiresExactValidAccounting(t *testing.T) {
	for _, tc := range []struct {
		name     string
		prompt   AgentInitialPrompt
		provider ProviderKind
		model    string
		usage    AgentTokenUsage
		want     error
	}{
		{"unknown provider", NewAgentInitialPrompt("text"), ProviderUnknown, "model", AgentTokenUsage{Exact: true, Tokens: 1, Tokenizer: "counter"}, ErrProviderCapabilityUnknown},
		{"missing provider", NewAgentInitialPrompt("text"), "", "model", AgentTokenUsage{Exact: true, Tokens: 1, Tokenizer: "counter"}, ErrProviderCapabilityUnknown},
		{"missing model", NewAgentInitialPrompt("text"), ProviderCodex, "", AgentTokenUsage{Exact: true, Tokens: 1, Tokenizer: "counter"}, ErrProviderCapabilityUnknown},
		{"blank model", NewAgentInitialPrompt("text"), ProviderCodex, " \n", AgentTokenUsage{Exact: true, Tokens: 1, Tokenizer: "counter"}, ErrProviderCapabilityUnknown},
		{"approximate", NewAgentInitialPrompt("text"), ProviderCodex, "model", AgentTokenUsage{Tokens: 1, Tokenizer: "counter"}, ErrProviderCapabilityUnknown},
		{"missing tokenizer", NewAgentInitialPrompt("text"), ProviderCodex, "model", AgentTokenUsage{Exact: true, Tokens: 1}, ErrProviderCapabilityUnknown},
		{"blank tokenizer", NewAgentInitialPrompt("text"), ProviderCodex, "model", AgentTokenUsage{Exact: true, Tokens: 1, Tokenizer: " "}, ErrProviderCapabilityUnknown},
		{"negative count", NewAgentInitialPrompt("text"), ProviderCodex, "model", AgentTokenUsage{Exact: true, Tokens: -1, Tokenizer: "counter"}, ErrContextBudgetExceeded},
		{"uncounted text", NewAgentInitialPrompt("text"), ProviderCodex, "model", AgentTokenUsage{Exact: true, Tokenizer: "counter"}, ErrContextBudgetExceeded},
		{"uncounted whitespace", NewAgentInitialPrompt(" "), ProviderCodex, "model", AgentTokenUsage{Exact: true, Tokenizer: "counter"}, ErrContextBudgetExceeded},
		{"counted empty", NewAgentInitialPrompt(""), ProviderCodex, "model", AgentTokenUsage{Exact: true, Tokens: 1, Tokenizer: "counter"}, ErrContextBudgetExceeded},
		{"counted absent", AgentInitialPrompt{}, ProviderCodex, "model", AgentTokenUsage{Exact: true, Tokens: 1, Tokenizer: "counter"}, ErrContextBudgetExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := NewAgentPromptReservation(tc.prompt, tc.provider, tc.model, tc.usage)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v; want %v", err, tc.want)
			}
			if err := res.Validate(NewAgentInitialPrompt("text"), ProviderCodex, "model", "counter", 1); err == nil {
				t.Fatal("failed construction returned usable authorization")
			}
		})
	}
}

func TestAgentPromptReservationEmptyAbsentAndLegacy(t *testing.T) {
	usage := AgentTokenUsage{Exact: true, Tokenizer: "counter"}
	absent, empty := AgentInitialPrompt{}, NewAgentInitialPrompt("")
	for _, prompt := range []AgentInitialPrompt{absent, empty} {
		res, err := NewAgentPromptReservation(prompt, ProviderCodex, "model", usage)
		if err != nil {
			t.Fatal(err)
		}
		if err := res.Validate(prompt, ProviderCodex, "model", usage.Tokenizer, 0); err != nil {
			t.Fatal(err)
		}
		other := empty
		if prompt.Present() {
			other = absent
		}
		if err := res.Validate(other, ProviderCodex, "model", usage.Tokenizer, 0); err == nil {
			t.Fatal("reservation did not distinguish absent and explicit empty")
		}
		if err := res.Validate(prompt, ProviderClaude, "model", usage.Tokenizer, 0); err == nil {
			t.Fatal("bound empty/absent reservation ignored metadata")
		}
	}
	var legacy AgentPromptReservation
	if err := legacy.Validate(absent, "", "", "", 0); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		prompt AgentInitialPrompt
		tokens int
	}{{empty, 0}, {NewAgentInitialPrompt("text"), 0}, {NewAgentInitialPrompt("text"), 1}, {absent, 1}, {absent, -1}} {
		if err := legacy.Validate(tc.prompt, ProviderCodex, "model", "counter", tc.tokens); err == nil {
			t.Fatal("legacy zero reservation authorized more than absent/zero")
		}
	}
}

func initialPromptPackage(t *testing.T, prompt AgentInitialPrompt, tokens int) AgentContextPackage {
	t.Helper()
	c := budgetCapability(1000000)
	c.InitialPromptTokens = tokens
	usage := AgentTokenUsage{Exact: true, Tokens: tokens, Tokenizer: c.Tokenizer}
	b, err := c.ResolveBudget(c.Provider, c.Model, MaxAgentContextTokens, usage)
	if err != nil {
		t.Fatal(err)
	}
	res, err := NewAgentPromptReservation(prompt, c.Provider, c.Model, usage)
	if err != nil {
		t.Fatal(err)
	}
	p := AgentContextPackage{Provider: c.Provider, Version: AgentContextVersion, Delivery: "prepared", Capability: "verified_for_preparation",
		Policy:  InputPolicy{Version: AgentContextVersion, Mode: "history", BudgetTokens: MaxAgentContextTokens, Source: "explicit"},
		Content: AgentContextContent{Notice: "fixture"}, Budget: &b, Usage: AgentTokenUsage{Exact: true, Tokens: 100, Tokenizer: c.Tokenizer}}
	p.BindInitialPrompt(res)
	p.ID, err = p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAgentContextPackageValidatesInitialPromptBudgetBinding(t *testing.T) {
	prompt := NewAgentInitialPrompt("first prompt")
	p := initialPromptPackage(t, prompt, 3)
	if err := p.ValidateIdentity(); err != nil {
		t.Fatal(err)
	}
	if err := p.ValidateInitialPrompt(prompt); err != nil {
		t.Fatal(err)
	}
	b := p.Budget
	if err := p.InitialPromptReservation().Validate(prompt, b.Provider, b.Model, b.Tokenizer, b.InitialPromptTokens); err != nil {
		t.Fatal(err)
	}
	if err := p.ValidateInitialPrompt(NewAgentInitialPrompt("other prompt")); err == nil {
		t.Fatal("same-count replacement authorized")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*AgentContextBudget)
	}{
		{"provider", func(b *AgentContextBudget) { b.Provider = ProviderClaude }},
		{"model", func(b *AgentContextBudget) { b.Model = "other-model" }},
		{"tokenizer", func(b *AgentContextBudget) { b.Tokenizer = "other-counter" }},
		{"count", func(b *AgentContextBudget) { b.InitialPromptTokens++ }},
		{"negative count", func(b *AgentContextBudget) { b.InitialPromptTokens = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed, budget := p, *p.Budget
			changed.Budget = &budget
			tc.mutate(changed.Budget)
			if err := changed.ValidateInitialPrompt(prompt); err == nil {
				t.Fatal("accepted mismatched budget metadata")
			}
		})
	}
	p.Budget = nil
	if err := p.ValidateInitialPrompt(prompt); err == nil {
		t.Fatal("binding authorized without budget")
	}
	p.BindInitialPrompt(AgentPromptReservation{})
	if err := p.ValidateInitialPrompt(prompt); err != nil {
		t.Fatalf("legacy unbound package changed: %v", err)
	}
}

func TestAgentInitialPromptAndReservationPrivacy(t *testing.T) {
	secret := "PRIVATE-first-prompt-\ud55c\uae00-3ef817"
	prompt := NewAgentInitialPrompt(secret)
	p := initialPromptPackage(t, prompt, 5)
	res := p.InitialPromptReservation()
	wrapper := struct {
		Prompt      AgentInitialPrompt
		Reservation AgentPromptReservation
		Package     AgentContextPackage
		PackagePtr  *AgentContextPackage
	}{prompt, res, p, &p}
	for _, value := range []any{prompt, &prompt, res, &res, p, &p, wrapper, &wrapper, []AgentContextPackage{p}} {
		for _, format := range []string{"%v", "%+v", "%#v"} {
			assertNoInitialPromptLeak(t, fmt.Sprintf(format, value), secret)
		}
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		assertNoInitialPromptLeak(t, string(raw), secret)
	}
	artifact, err := p.Artifact()
	if err != nil {
		t.Fatal(err)
	}
	assertNoInitialPromptLeak(t, string(artifact), secret)
	if !strings.Contains(string(artifact), `"initial_prompt_tokens": 5`) {
		t.Fatal("receipt omitted public prompt count")
	}
	unbound := p
	unbound.BindInitialPrompt(AgentPromptReservation{})
	digest, err := unbound.Digest()
	if err != nil || digest != p.ID {
		t.Fatalf("private binding changed receipt digest: %v", err)
	}
	// Private validation also must not echo prompt text or a stable prompt hash.
	err = p.ValidateInitialPrompt(NewAgentInitialPrompt("replacement"))
	if err == nil {
		t.Fatal("accepted replacement")
	}
	assertNoInitialPromptLeak(t, err.Error(), secret)
}

func assertNoInitialPromptLeak(t *testing.T, output, secret string) {
	t.Helper()
	hash := string(HashContent([]byte(secret)))
	if strings.Contains(output, secret) || strings.Contains(output, hash) || strings.Contains(output, strings.TrimPrefix(hash, "sha256:")) {
		t.Fatalf("private prompt or its hash leaked: %s", output)
	}
}

func TestAgentContextPackageDecodedReceiptCannotAuthorizePrompt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prompt AgentInitialPrompt
		tokens int
	}{
		{"nonempty", NewAgentInitialPrompt("private initial prompt"), 3},
		{"explicit empty", NewAgentInitialPrompt(""), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := initialPromptPackage(t, tc.prompt, tc.tokens)
			raw, err := p.Artifact()
			if err != nil {
				t.Fatal(err)
			}
			for _, decoded := range []AgentContextPackage{{}, p} {
				if err := json.Unmarshal(raw, &decoded); err != nil {
					t.Fatal(err)
				}
				if err := decoded.ValidateIdentity(); err != nil {
					t.Fatalf("receipt identity changed on decode: %v", err)
				}
				if err := decoded.ValidateInitialPrompt(tc.prompt); err == nil {
					t.Fatal("decoded receipt authorized prompt")
				}
				if tc.tokens > 0 && decoded.ValidateInitialPrompt(AgentInitialPrompt{}) == nil {
					t.Fatal("positive receipt count authorized absent prompt")
				}
				decoded.BindInitialPrompt(p.InitialPromptReservation())
				if err := decoded.ValidateInitialPrompt(tc.prompt); err != nil {
					t.Fatalf("explicit rebind failed: %v", err)
				}
			}
		})
	}
	// Old strict receipts without a prompt reservation still accept absence.
	legacy := initialPromptPackage(t, AgentInitialPrompt{}, 0)
	raw, err := legacy.Artifact()
	if err != nil {
		t.Fatal(err)
	}
	var decoded AgentContextPackage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.ValidateInitialPrompt(AgentInitialPrompt{}); err != nil {
		t.Fatal(err)
	}
	if err := decoded.ValidateInitialPrompt(NewAgentInitialPrompt("")); err == nil {
		t.Fatal("legacy receipt authorized explicit empty prompt")
	}
}

func TestAgentOpaquePromptJSONCannotRestoreAuthorization(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"text":"injected","present":true,"validate":{}}`} {
		prompt := NewAgentInitialPrompt("private")
		res, err := NewAgentPromptReservation(prompt, ProviderCodex, "model", AgentTokenUsage{Exact: true, Tokens: 1, Tokenizer: "counter"})
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(raw), &prompt); err != nil {
			t.Fatal(err)
		}
		if prompt.Present() || prompt.Text() != "" {
			t.Fatal("opaque prompt retained authority after decode")
		}
		if err := json.Unmarshal([]byte(raw), &res); err != nil {
			t.Fatal(err)
		}
		if err := res.Validate(NewAgentInitialPrompt("private"), ProviderCodex, "model", "counter", 1); err == nil {
			t.Fatal("opaque reservation retained authority after decode")
		}
	}
}
