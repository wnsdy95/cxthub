package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestAgentContextHistoryBudgets(t *testing.T) {
	for _, test := range []struct {
		input string
		want  int
	}{{"", 800000}, {"full", 800000}, {"800k", 800000}, {"200k", 200000}, {"1", 1}, {"800000", 800000}} {
		got, err := ParseHistoryBudget(test.input)
		if err != nil || got != test.want {
			t.Fatalf("%q: %d %v", test.input, got, err)
		}
	}
	for _, input := range []string{"0", "-1", "1.5k", "1K", "2m", "800001", "801k", " full", "200k ", strings.Repeat("9", 100)} {
		if _, err := ParseHistoryBudget(input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
}

func TestAgentContextCapabilityDoesNotFabricateHostAcceptance(t *testing.T) {
	usage := AgentTokenUsage{Tokens: 100, Exact: true, Tokenizer: "fixture"}
	capability := AgentHostCapability{Provider: ProviderCodex, Model: "fixture", HostVersion: "fixture-v1", Evidence: "synthetic test only", Verified: true, AutoCompactKnown: true, ContextWindow: 1000, ReservedTokens: 100, FramingTokens: 10, AutoCompactTokens: 800, Tokenizer: "fixture"}
	if err := capability.Check(ProviderCodex, "fixture", usage); err != nil {
		t.Fatal(err)
	}
	usage.Tokens = 691
	if err := capability.Check(ProviderCodex, "fixture", usage); !errors.Is(err, ErrContextBudgetExceeded) {
		t.Fatal(err)
	}
	usage.Tokens = 100
	usage.Exact = false
	if err := capability.Check(ProviderCodex, "fixture", usage); !errors.Is(err, ErrProviderCapabilityUnknown) {
		t.Fatal(err)
	}
	usage.Exact = true
	capability.Verified = false
	if err := capability.Check(ProviderCodex, "fixture", usage); !errors.Is(err, ErrProviderCapabilityUnknown) {
		t.Fatal(err)
	}
}

func TestAgentContextIdentityCoversSuppliedContent(t *testing.T) {
	p := AgentContextPackage{Version: AgentContextVersion, Delivery: "prepared", Policy: MemoryInputPolicy(), Content: AgentContextContent{Notice: "fixture"}}
	p.ID, _ = p.Digest()
	if err := p.ValidateIdentity(); err != nil {
		t.Fatal(err)
	}
	p.Content.Notice = "changed"
	if err := p.ValidateIdentity(); !errors.Is(err, ErrHashMismatch) {
		t.Fatal(err)
	}
}
