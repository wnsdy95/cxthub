package app

import (
	"context"
	"errors"
	"reflect"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type gitAdmissionLookup struct {
	publicationID string
	sourceHead    string
	ordinaryIDs   []string
	carryIDs      []string
}

func historicalGitAdmissionLookup(admission domain.GitContributionAdmission) gitAdmissionLookup {
	return gitAdmissionLookup{publicationID: admission.PublicationID, sourceHead: admission.SourceHead, ordinaryIDs: admission.OrdinaryObservationIDs, carryIDs: admission.CarryObservationIDs}
}

func (s *Service) readGitAdmissionEvidence(ctx context.Context, repo domain.ContentHash, lookups []gitAdmissionLookup) (*gitAdmissionSnapshot, error) {
	return repositoryReadForRepo(ctx, s, repo, func(read context.Context) (*gitAdmissionSnapshot, error) {
		storage, supported := s.meta.(outbound.GitPublicationLookupStore)
		_, generations := s.meta.(outbound.RepositoryMetadataGenerationStore)
		transactions, known := s.meta.(outbound.RepositoryTransactionState)
		if !supported || !generations || !known || !transactions.InReadOnlyTransaction(read) {
			return s.readGitAdmissionSnapshotInRead(read, repo)
		}
		if len(lookups) > gitAdmissionBatchLimit {
			return nil, domain.ErrValidation
		}
		bindings, err := storage.ListGitBranchBindings(read, repo)
		if err != nil {
			return nil, err
		}
		events := map[string]domain.HistoryEvent{}
		for _, event := range bindings {
			if !domain.IsBranchBindingEvent(event) {
				return nil, domain.ErrIntegrity
			}
			if err := addGitAdmissionEvidence(events, repo, event); err != nil {
				return nil, err
			}
		}
		for _, lookup := range lookups {
			publication, err := storage.GetGitHistoryEvent(read, repo, lookup.publicationID)
			if err != nil {
				return nil, err
			}
			if publication.ID != lookup.publicationID || publication.Kind != "publish" {
				return nil, domain.ErrIntegrity
			}
			if err := addGitAdmissionEvidence(events, repo, publication); err != nil {
				return nil, err
			}
			if err := loadGitAdmissionWitnesses(read, storage, repo, events, domain.GitPublicationWitnessQuery{Publication: publication, GitHead: publication.GitAfter, IDs: lookup.ordinaryIDs}); err != nil {
				return nil, err
			}
			if lookup.sourceHead != "" {
				if err := loadGitAdmissionCarry(read, storage, repo, events, publication, lookup.sourceHead, lookup.carryIDs); err != nil {
					return nil, err
				}
			}
		}
		snapshot, err := s.readGitAdmissionSnapshotWithHistory(read, repo, gitAdmissionEvidenceEvents(events))
		if err != nil {
			return nil, err
		}
		if err := s.captureGitAdmissionGeneration(read, repo, snapshot); err != nil {
			return nil, err
		}
		snapshot.targeted = true
		return snapshot, nil
	})
}

func addGitAdmissionEvidence(events map[string]domain.HistoryEvent, repo domain.ContentHash, event domain.HistoryEvent) error {
	if event.RepoID != string(repo) || domain.ValidateHistoryEvent(event) != nil {
		return domain.ErrIntegrity
	}
	if previous, found := events[event.ID]; found && !reflect.DeepEqual(previous, event) {
		return domain.ErrIntegrity
	}
	events[event.ID] = event
	return nil
}

func gitAdmissionEvidenceEvents(events map[string]domain.HistoryEvent) []domain.HistoryEvent {
	result := make([]domain.HistoryEvent, 0, len(events))
	for _, event := range events {
		result = append(result, event)
	}
	return result
}

func loadGitAdmissionWitnesses(ctx context.Context, storage outbound.GitPublicationLookupStore, repo domain.ContentHash, events map[string]domain.HistoryEvent, query domain.GitPublicationWitnessQuery) error {
	if query.Validate() != nil || query.Publication.RepoID != string(repo) {
		return domain.ErrIntegrity
	}
	witnesses, err := storage.ListGitPublicationWitnesses(ctx, repo, query)
	if err != nil {
		return err
	}
	if len(witnesses) > domain.MaxGitAdmissionWitnesses || (len(query.IDs) > 0 && len(query.IDs) != len(witnesses)) {
		return domain.ErrIntegrity
	}
	for index, event := range witnesses {
		if !query.Matches(event) || (index > 0 && witnesses[index-1].ID >= event.ID) || (len(query.IDs) > 0 && query.IDs[index] != event.ID) {
			return domain.ErrIntegrity
		}
		if err := addGitAdmissionEvidence(events, repo, event); err != nil {
			return err
		}
	}
	return nil
}

func loadGitAdmissionCarry(ctx context.Context, storage outbound.GitPublicationLookupStore, repo domain.ContentHash, events map[string]domain.HistoryEvent, publication domain.HistoryEvent, head string, ids []string) error {
	if err := loadGitAdmissionWitnesses(ctx, storage, repo, events, domain.GitPublicationWitnessQuery{Publication: publication, GitHead: head, IDs: ids}); err != nil {
		return err
	}
	owner, err := storage.FindGitForeignHeadOwner(ctx, repo, head, publication.BranchID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if owner.BranchID == publication.BranchID || owner.GitAfter != head || owner.Target == "" {
		return domain.ErrIntegrity
	}
	switch owner.Kind {
	case "birth", "attach", "position", "advance":
		return addGitAdmissionEvidence(events, repo, owner)
	default:
		return domain.ErrIntegrity
	}
}

func (s *Service) hydrateGitAdmissionEvidence(ctx context.Context, frozen *gitAdmissionSnapshot, publications []domain.HistoryEvent, receipt domain.GitHeadReceipt) (*gitAdmissionSnapshot, error) {
	if frozen == nil || !frozen.targeted {
		return frozen, nil
	}
	if _, err := s.reuseGitAdmissionSnapshot(ctx, receipt.RepoID, frozen); err != nil {
		return nil, err
	}
	storage, supported := s.meta.(outbound.GitPublicationLookupStore)
	if !supported || frozen.generation == nil || len(publications) > gitAdmissionBatchLimit {
		return nil, domain.ErrIntegrity
	}
	events := make(map[string]domain.HistoryEvent, len(frozen.history))
	for id, event := range frozen.history {
		events[id] = event
	}
	for _, publication := range publications {
		if original, found := frozen.history[publication.ID]; !found || !reflect.DeepEqual(original, publication) {
			return nil, domain.ErrIntegrity
		}
		name := publication.LocalBranch
		if name == "" {
			name = publication.Branch
		}
		if source, found := receipt.SourceHead("refs/heads/" + name); found {
			if err := loadGitAdmissionCarry(ctx, storage, receipt.RepoID, events, publication, source.Commit, nil); err != nil {
				return nil, err
			}
		}
	}
	index, err := domain.NewGitPublicationIndex(ctx, receipt.RepoID, gitAdmissionEvidenceEvents(events))
	if err != nil {
		return nil, err
	}
	result := *frozen
	result.history, result.index = events, index
	result.owners = gitAdmissionOwners(events)
	return &result, nil
}
