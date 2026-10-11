package app

import (
	"context"
	"errors"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (g *GitScans) freezeGitHeadContext(ctx context.Context, claim domain.GitHeadScan, identity *domain.GitProviderObservation, batch *frozenGitAdmissionBatch) (*gitAdmissionSnapshot, error) {
	_, supported := g.core.meta.(outbound.GitContextHeadStore)
	_, atomic := g.core.meta.(outbound.RepositoryTransactions)
	if !supported || !atomic || identity == nil {
		return nil, nil
	}
	if identity.Validate() != nil {
		return nil, domain.ErrIntegrity
	}
	if batch != nil && batch.snapshot != nil {
		return batch.snapshot, nil
	}
	return repositoryReadForRepo(ctx, g.core, claim.RepoID, func(read context.Context) (*gitAdmissionSnapshot, error) {
		return g.core.readGitAdmissionEvidence(read, claim.RepoID, nil)
	})
}

func prepareGitContextHeads(snapshot *gitAdmissionSnapshot, receipt *domain.GitHeadReceipt) ([]domain.GitContextHead, error) {
	if snapshot == nil || receipt == nil {
		return nil, nil
	}
	if receipt.Validate() != nil || snapshot.repo.ID != receipt.RepoID || snapshot.repo.GitRemoteURL != receipt.GitOrigin {
		return nil, domain.ErrIntegrity
	}
	heads := []domain.GitContextHead{}
	for _, observed := range receipt.Heads {
		name := strings.TrimPrefix(observed.Ref, "refs/heads/")
		identity := snapshot.bindings.Identity(string(receipt.RepoID), name)
		ref, err := snapshot.destination(name, identity)
		if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrConflict) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if ref.Name != name {
			continue
		}
		candidate := (domain.GitDestinationCandidate{RepoID: receipt.RepoID, GitOrigin: receipt.GitOrigin, InstallationID: receipt.Provider.InstallationID, ProviderRepositoryID: receipt.Provider.RepositoryID,
			ObservationID: observed.ObservationID, ReadReceiptID: receipt.ID, ReadVersion: receipt.Version, DestinationBranchID: identity, DestinationBranch: name, DestinationHead: observed.Commit}).WithID()
		head := (domain.GitContextHead{Destination: candidate, AnchorID: snapshot.bindings.ByID[identity].EventID, ContextTarget: ref.Target, GraphRevision: snapshot.revision.Graph, EvidenceRevision: snapshot.revision.Evidence}).WithID()
		if head.Validate() != nil || !candidate.MatchesReceipt(*receipt) {
			return nil, domain.ErrIntegrity
		}
		heads = append(heads, head)
	}
	return heads, nil
}

func (g *GitScans) persistGitContextHeads(ctx context.Context, receipt *domain.GitHeadReceipt, heads []domain.GitContextHead, current *gitAdmissionSnapshot) error {
	if len(heads) == 0 {
		return nil
	}
	if current == nil || receipt == nil {
		return domain.ErrIntegrity
	}
	accepted := []domain.GitContextHead{}
	for _, head := range heads {
		if current.revision.Graph != head.GraphRevision || current.revision.Evidence != head.EvidenceRevision {
			continue
		}
		ref, err := current.destination(head.Destination.DestinationBranch, head.Destination.DestinationBranchID)
		if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrConflict) {
			continue
		}
		if err != nil {
			return err
		}
		if ref.Name != head.Destination.DestinationBranch || ref.Target != head.ContextTarget || current.bindings.ByID[ref.BranchID].EventID != head.AnchorID {
			continue
		}
		accepted = append(accepted, head)
	}
	if len(accepted) == 0 {
		return nil
	}
	return g.core.meta.(outbound.GitContextHeadStore).PutGitContextHeads(ctx, *receipt, accepted)
}
