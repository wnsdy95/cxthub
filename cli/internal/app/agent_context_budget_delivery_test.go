package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func TestAgentContextHistoryBudgetCheckedBeforeMaterialization(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*domain.AgentContextPackage, *inbound.PrepareAgentContextInput)
		want   error
	}{
		{"verified synthetic preparation", func(*domain.AgentContextPackage, *inbound.PrepareAgentContextInput) {}, nil},
		{"legacy missing budget", func(p *domain.AgentContextPackage, _ *inbound.PrepareAgentContextInput) { p.Budget = nil }, domain.ErrProviderCapabilityUnknown},
		{"request changed", func(_ *domain.AgentContextPackage, in *inbound.PrepareAgentContextInput) {
			in.Policy.BudgetTokens = 100000
		}, domain.ErrProviderCapabilityUnknown},
		{"model changed", func(_ *domain.AgentContextPackage, in *inbound.PrepareAgentContextInput) { in.Model = "other" }, domain.ErrProviderCapabilityUnknown},
		{"provider changed", func(_ *domain.AgentContextPackage, in *inbound.PrepareAgentContextInput) {
			in.Provider = domain.ProviderClaude
		}, domain.ErrProviderCapabilityUnknown},
		{"effective overflow", func(p *domain.AgentContextPackage, _ *inbound.PrepareAgentContextInput) {
			p.Usage.Tokens = p.Budget.EffectiveTokens + 1
		}, domain.ErrContextBudgetExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := inbound.PrepareAgentContextInput{Provider: domain.ProviderCodex, Model: "fixture", Policy: domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: 800000, Source: "explicit"}}
			c, err := (agentCapabilityFixture{window: 200000}).AgentCapability(context.Background(), in.Provider, in.Model)
			if err != nil {
				t.Fatal(err)
			}
			u := domain.AgentTokenUsage{Tokens: 100, Exact: true, Tokenizer: c.Tokenizer}
			b, err := c.ResolveBudget(in.Provider, in.Model, in.Policy.BudgetTokens, u)
			if err != nil {
				t.Fatal(err)
			}
			p := domain.AgentContextPackage{Version: 1, Policy: in.Policy, Delivery: "prepared", Capability: "verified_for_preparation", Budget: &b, Usage: u, Content: domain.AgentContextContent{Notice: "synthetic preparation; not provider acceptance", Selection: domain.AgentContextSelection{CodeCommit: strings.Repeat("a", 40)}}}
			tc.mutate(&p, &in)
			p.ID, _ = p.Digest()
			mat := &agentMaterializerFixture{}
			s := NewLoadSessionService(nil, map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderCodex: codec.NewCodexCodec()}, map[domain.ProviderKind]outbound.SessionMaterializer{domain.ProviderCodex: mat}, nil, nil, nil).WithAgentCodePosition(agentCodeFixture{})
			_, err = s.materializeAgentPackage(context.Background(), in, p)
			if tc.want == nil {
				if err != nil || mat.calls != 1 {
					t.Fatalf("valid preparation: %v calls=%d", err, mat.calls)
				}
			} else if !errors.Is(err, tc.want) || mat.calls != 0 {
				t.Fatalf("rejected preparation: %v want=%v calls=%d", err, tc.want, mat.calls)
			}
		})
	}
}
