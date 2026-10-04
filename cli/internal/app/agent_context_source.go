package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

// ValidateLatestMain reauthorizes a prepared source at the delivery boundary.
// This is an observation, not a lock spanning server, Git and provider processes.
func (s *AgentContextService) ValidateLatestMain(ctx context.Context, cwd string, selected domain.AgentContextSelection) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := selected.ValidateSource(); err != nil {
		return err
	}
	if selected.SourcePolicy != domain.AgentSourceLatestMain || s == nil || s.history == nil || s.memory == nil {
		return domain.ErrAgentContextUnavailable
	}
	semantic := selected.ContextDeliveryHash != "" // ValidateSource requires both.
	query := inbound.HistoryQueryInput{Cwd: cwd, Server: true, ServerTip: true, Branch: "main"}
	check := func() (domain.RepositoryRevision, error) {
		v, err := s.history.QueryHistory(ctx, query)
		if err != nil {
			return domain.RepositoryRevision{}, err
		}
		if err := validAgentHistory(v, inbound.PrepareAgentContextInput{RepoID: selected.RepositoryID, Branch: "main"}); err != nil {
			return domain.RepositoryRevision{}, err
		}
		changed := v.StateHash != selected.ContextStateHash || v.Revision.Graph != selected.GraphRevision || v.Revision.Evidence != selected.EvidenceRevision
		if semantic {
			// Missing or changed proofs reject; never downgrade this package to
			// legacy comparisons even if its old state/revisions still match.
			changed = v.DeliveryStateHash != selected.ContextDeliveryHash
		}
		if v.Position != selected.SnapshotID || v.Selection.CodeCommit != selected.CodeCommit || changed {
			return domain.RepositoryRevision{}, fmt.Errorf("%w: server main changed after input preparation; prepare again (semantic=%t, position_changed=%t, code_changed=%t, state_changed=%t, delivery_changed=%t, graph_revision=%d->%d, evidence_revision=%d->%d)", domain.ErrSelectionChanged, semantic, v.Position != selected.SnapshotID, v.Selection.CodeCommit != selected.CodeCommit, v.StateHash != selected.ContextStateHash, v.DeliveryStateHash != selected.ContextDeliveryHash, selected.GraphRevision, v.Revision.Graph, selected.EvidenceRevision, v.Revision.Evidence)
		}
		return *v.Revision, nil
	}
	first, err := check()
	if err != nil {
		return err
	}
	req := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: selected.SnapshotID, Branch: "main", CodeCommit: selected.CodeCommit}, Content: "prompt", Limit: 50}
	memory, err := s.memory.QueryEffectiveMemory(ctx, selected.RepositoryID, req)
	if err != nil {
		return err
	}
	changed := memory.StateHash != selected.MemoryStateHash || memory.Revision.Graph != selected.GraphRevision || memory.Revision.Evidence != selected.EvidenceRevision
	if semantic {
		changed = memory.DeliveryStateHash != selected.MemoryDeliveryHash
	}
	valid := validEffectivePromptPage(memory, req)
	if !valid || changed {
		return fmt.Errorf("%w: server main memory changed after input preparation; prepare again (semantic=%t, valid=%t, state_changed=%t, delivery_changed=%t, graph_revision=%d->%d, evidence_revision=%d->%d)", domain.ErrSelectionChanged, semantic, valid, memory.StateHash != selected.MemoryStateHash, memory.DeliveryStateHash != selected.MemoryDeliveryHash, selected.GraphRevision, memory.Revision.Graph, selected.EvidenceRevision, memory.Revision.Evidence)
	}
	if err := sameAgentReadRevision("history/memory", first, domain.RepositoryRevision{Graph: memory.Revision.Graph, Evidence: memory.Revision.Evidence}); err != nil {
		return err
	}
	last, err := check()
	if err != nil {
		return err
	}
	if err := sameAgentReadRevision("history/memory/history", first, last); err != nil {
		return err
	}
	return ctx.Err()
}

func sameAgentReadRevision(stage string, before, after domain.RepositoryRevision) error {
	if before.Graph != after.Graph || before.Evidence != after.Evidence {
		return fmt.Errorf("%w: current source reads changed during validation (%s, graph_revision=%d->%d, evidence_revision=%d->%d)", domain.ErrSelectionChanged, stage, before.Graph, after.Graph, before.Evidence, after.Evidence)
	}
	return nil
}

// A retry can cross repository generations only while the selected semantic
// source is unchanged. Proof appearance/disappearance is never an equivalence.
func sameAgentContextState(state, delivery, nextState, nextDelivery domain.ContentHash) bool {
	if delivery != "" || nextDelivery != "" {
		return delivery != "" && delivery == nextDelivery
	}
	return state == nextState
}

func validateInjectedPackage(ctx context.Context, preparer inbound.PrepareAgentContext, cwd string, p domain.AgentContextPackage) error {
	if err := p.ValidateIdentity(); err != nil {
		return err
	}
	if p.Content.Selection.SourcePolicy != domain.AgentSourceLatestMain {
		return fmt.Errorf("%w: injection requires latest server main", domain.ErrAgentContextUnavailable)
	}
	validator, ok := preparer.(inbound.AgentContextDeliveryValidator)
	if !ok {
		return fmt.Errorf("%w: injection requires source revalidation", domain.ErrAgentContextUnavailable)
	}
	return validator.ValidateAgentContextDelivery(ctx, cwd, p.Content.Selection)
}
