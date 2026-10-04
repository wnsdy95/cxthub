package main

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestNativeWindowPackageCannotUseAnotherRuntimePolicy(t *testing.T) {
	f := newNativeGenerationBridgeFixture(t)
	f.prepared(t, "private initial question")
	if err := checkNativeWindowPackage(f.capability, f.returned); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*domain.AgentContextBudget){
		"packing window":   func(b *domain.AgentContextBudget) { b.ContextWindow++ },
		"scope":            func(b *domain.AgentContextBudget) { b.RuntimeScope = domain.HashContent([]byte("another invocation")) },
		"tokenizer":        func(b *domain.AgentContextBudget) { b.Tokenizer = "other" },
		"model":            func(b *domain.AgentContextBudget) { b.Model = "other" },
		"host":             func(b *domain.AgentContextBudget) { b.HostVersion = "other" },
		"compaction scope": func(b *domain.AgentContextBudget) { b.AutoCompactUnverified = !b.AutoCompactUnverified },
		"compaction limit": func(b *domain.AgentContextBudget) { b.AutoCompactTokens++ },
	} {
		t.Run(name, func(t *testing.T) {
			p := f.returned
			b := *p.Budget
			p.Budget = &b
			mutate(p.Budget)
			if err := checkNativeWindowPackage(f.capability, p); !errors.Is(err, domain.ErrProviderCapabilityUnknown) {
				t.Fatal("mismatched runtime policy accepted", err)
			}
		})
	}
	r := nativeCodexWindowReader{}
	if _, err := r.AgentCapability(context.Background(), domain.ProviderCodex, "fixture"); !errors.Is(err, domain.ErrProviderCapabilityUnknown) {
		t.Fatal("unbound reader accepted", err)
	}
	if callback, err := newNativeCodexWindowGenerationPrepare(f.bridge.bound, nil, r, f.bridge.prepare, f.bridge.validate, f.store); err == nil || callback != nil {
		t.Fatal("unowned runtime accepted")
	}
}
