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
	if err := selected.ValidateSource(); err != nil {
		return err
	}
	if selected.SourcePolicy != domain.AgentSourceLatestMain || s.history == nil || s.memory == nil {
		return domain.ErrAgentContextUnavailable
	}
	query := inbound.HistoryQueryInput{Cwd: cwd, Server: true, ServerTip: true, Branch: "main"}
	check := func() error {
		v, err := s.history.QueryHistory(ctx, query)
		if err != nil {
			return err
		}
		if err := validAgentHistory(v, inbound.PrepareAgentContextInput{RepoID: selected.RepositoryID, Branch: "main"}); err != nil {
			return err
		}
		if v.Position != selected.SnapshotID || v.Selection.CodeCommit != selected.CodeCommit || v.StateHash != selected.ContextStateHash || v.Revision.Graph != selected.GraphRevision || v.Revision.Evidence != selected.EvidenceRevision {
			return fmt.Errorf("%w: server main changed after input preparation; prepare again", domain.ErrSelectionChanged)
		}
		return nil
	}
	if err := check(); err != nil {
		return err
	}
	req := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: selected.SnapshotID, Branch: "main", CodeCommit: selected.CodeCommit}, Content: "prompt", Limit: 50}
	memory, err := s.memory.QueryEffectiveMemory(ctx, selected.RepositoryID, req)
	if err != nil {
		return err
	}
	if !validEffectivePromptPage(memory, req) || memory.StateHash != selected.MemoryStateHash || memory.Revision.Graph != selected.GraphRevision || memory.Revision.Evidence != selected.EvidenceRevision {
		return fmt.Errorf("%w: server main memory changed after input preparation; prepare again", domain.ErrSelectionChanged)
	}
	return check()
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
