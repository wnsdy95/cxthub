package app

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const gitAdmissionBatchLimit = 8

type frozenGitAdmissionBatch struct {
	claim         domain.GitHeadScan
	authorization gitAdmissionAuthorization
	snapshot      *gitAdmissionSnapshot
	previous      domain.GitAdmissionMatch
	next          domain.GitAdmissionMatch
	publications  []domain.HistoryEvent
	more          bool
}

type preparedGitAdmission struct {
	frozen    frozenGitAdmission
	admission domain.GitContributionAdmission
}

type preparedGitAdmissionBatch struct {
	frozen     *frozenGitAdmissionBatch
	receipt    domain.GitHeadReceipt
	next       domain.GitAdmissionMatch
	admissions []preparedGitAdmission
}

func (g *GitScans) freezeGitAdmissionBatch(ctx context.Context, claim domain.GitHeadScan, identity *domain.GitProviderObservation, fence GitReadFence) (*frozenGitAdmissionBatch, error) {
	matcher, supported := g.core.meta.(outbound.GitAdmissionMatchStore)
	queue, queued := g.core.meta.(outbound.GitAssociationDiscoveryStore)
	_, admissions := g.core.meta.(outbound.GitContributionAdmissionStore)
	_, atomic := g.core.meta.(outbound.RepositoryTransactions)
	if !supported || !queued || !admissions || !atomic || identity == nil {
		return nil, nil
	}
	if identity.Validate() != nil || fence == nil || claim.Validate() != nil || claim.State != "running" {
		return nil, domain.ErrIntegrity
	}
	return repositoryReadForRepo(ctx, g.core, claim.RepoID, func(read context.Context) (*frozenGitAdmissionBatch, error) {
		previous, err := matcher.GetGitAdmissionMatch(read, claim.RepoID, claim.GitOrigin, *identity, claim.Page)
		if errors.Is(err, domain.ErrNotFound) {
			previous = domain.GitAdmissionMatch{RepoID: claim.RepoID, GitOrigin: claim.GitOrigin, Provider: *identity, Page: claim.Page}
		} else if err != nil {
			return nil, err
		}
		if previous.Validate() != nil || previous.Version == math.MaxInt64 {
			return nil, domain.ErrIntegrity
		}
		next := previous
		next.Version++
		if next.ThroughPublicationID == "" {
			next.ThroughPublicationID, err = matcher.GitAssociationPublicationUpperID(read, claim.RepoID, claim.GitOrigin)
			if err != nil {
				return nil, err
			}
			if next.ThroughPublicationID == "" {
				return nil, nil
			}
		}
		if next.Validate() != nil {
			return nil, domain.ErrIntegrity
		}
		publications, err := queue.ListGitAssociationPublications(read, claim.RepoID, claim.GitOrigin, next.AfterPublicationID, gitAdmissionBatchLimit+1)
		if err != nil {
			return nil, err
		}
		bounded := []domain.HistoryEvent{}
		for _, publication := range publications {
			if publication.ID > next.ThroughPublicationID {
				break
			}
			bounded = append(bounded, publication)
		}
		batch := &frozenGitAdmissionBatch{claim: claim, previous: previous, next: next, publications: bounded,
			authorization: gitAdmissionAuthorization{repo: claim.RepoID, provider: *identity, fence: fence}}
		if len(bounded) > gitAdmissionBatchLimit {
			batch.more = true
			batch.publications = bounded[:gitAdmissionBatchLimit]
		}
		if len(batch.publications) > 0 {
			lookups := make([]gitAdmissionLookup, 0, len(batch.publications))
			for _, publication := range batch.publications {
				lookups = append(lookups, gitAdmissionLookup{publicationID: publication.ID})
			}
			batch.snapshot, err = g.core.readGitAdmissionEvidence(read, claim.RepoID, lookups)
			if err != nil {
				return nil, err
			}
			if batch.snapshot.repo.GitRemoteURL != claim.GitOrigin {
				return nil, domain.ErrConflict
			}
		}
		return batch, nil
	})
}

func gitAdmissionDeferred(err error) bool {
	return errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrConflict) || errors.Is(err, errGitContributionEvidencePending) || errors.Is(err, errGitContributionAlreadyPresent)
}

func (g *GitScans) prepareGitAdmissionBatch(ctx context.Context, frozen *frozenGitAdmissionBatch, receipt *domain.GitHeadReceipt) (*preparedGitAdmissionBatch, error) {
	if frozen == nil || receipt == nil {
		return nil, nil
	}
	if receipt.Validate() != nil || receipt.RepoID != frozen.claim.RepoID || receipt.GitOrigin != frozen.claim.GitOrigin || receipt.Version != frozen.claim.Version || receipt.Page != frozen.claim.Page || receipt.Provider != frozen.authorization.provider {
		return nil, domain.ErrIntegrity
	}
	return repositoryReadForRepo(ctx, g.core, receipt.RepoID, func(read context.Context) (*preparedGitAdmissionBatch, error) {
		snapshot, err := g.core.hydrateGitAdmissionEvidence(read, frozen.snapshot, frozen.publications, *receipt)
		stale := errors.Is(err, domain.ErrConflict)
		if err != nil && !stale {
			return nil, err
		}
		preparedFrozen := *frozen
		if !stale {
			preparedFrozen.snapshot = snapshot
		}
		evidence, err := g.core.newCodeEvidence(read, receipt.RepoID)
		if err != nil {
			return nil, err
		}
		prepared, err := planGitAdmissionBatch(read, &preparedFrozen, *receipt, evidence)
		if stale && prepared != nil {
			prepared.admissions = nil
		}
		return prepared, err
	})
}

func planGitAdmissionBatch(ctx context.Context, frozen *frozenGitAdmissionBatch, receipt domain.GitHeadReceipt, evidence *codeEvidence) (*preparedGitAdmissionBatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	batch := &preparedGitAdmissionBatch{frozen: frozen, receipt: receipt, next: frozen.next}
	heads := map[string]string{}
	for _, head := range receipt.PriorSourceHeads {
		heads[strings.TrimPrefix(head.Ref, "refs/heads/")] = head.Commit
	}
	for _, head := range receipt.Heads {
		heads[strings.TrimPrefix(head.Ref, "refs/heads/")] = head.Commit
	}
	visited := 0
	for _, publication := range frozen.publications {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sourceName := publication.LocalBranch
		if sourceName == "" {
			sourceName = publication.Branch
		}
		sourceHead := heads[sourceName]
		for _, head := range receipt.Heads {
			name := strings.TrimPrefix(head.Ref, "refs/heads/")
			if sourceHead == "" || name == sourceName || name <= batch.next.AfterDestination {
				continue
			}
			batch.next.AfterDestination = name
			visited++
			identity := frozen.snapshot.bindings.Identity(string(receipt.RepoID), name)
			input, err := frozen.snapshot.freeze(publication.ID, sourceHead, name, identity)
			if err == nil {
				original, originalErr := frozen.snapshot.index.Publication(publication.ID)
				if originalErr != nil || !reflect.DeepEqual(original, publication) {
					return nil, domain.ErrIntegrity
				}
				var prepared domain.GitContributionAdmission
				prepared, err = prepareGitAdmission(ctx, input, receipt, evidence)
				if err == nil {
					batch.admissions = append(batch.admissions, preparedGitAdmission{input, prepared})
				}
			}
			if err != nil && !gitAdmissionDeferred(err) {
				return nil, err
			}
			if visited == gitAdmissionBatchLimit || errors.Is(err, errGitContributionEvidenceLimit) {
				return batch, nil
			}
		}
		batch.next.AfterPublicationID, batch.next.AfterDestination = publication.ID, ""
	}
	if !frozen.more {
		batch.next.ThroughPublicationID, batch.next.AfterPublicationID, batch.next.AfterDestination = "", "", ""
	}
	return batch, nil
}

func (g *GitScans) persistGitAdmissionBatch(ctx context.Context, batch *preparedGitAdmissionBatch, current *gitAdmissionSnapshot) error {
	if batch == nil {
		return nil
	}
	if err := batch.frozen.authorization.fence(ctx); err != nil {
		return err
	}
	if len(batch.admissions) > 0 {
		if current == nil {
			return domain.ErrIntegrity
		}
		for _, item := range batch.admissions {
			if _, err := g.core.persistGitAdmissionInWrite(ctx, item.frozen, item.admission, batch.frozen.authorization, current); err != nil && !gitAdmissionDeferred(err) {
				return err
			}
		}
	}
	return g.core.meta.(outbound.GitAdmissionMatchStore).SaveGitAdmissionMatch(ctx, batch.next, batch.frozen.previous.Version, batch.receipt)
}
