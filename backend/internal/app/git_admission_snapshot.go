package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type gitAdmissionSnapshot struct {
	targeted   bool
	generation *domain.RepositoryMetadataGeneration
	repo       domain.Repo
	index      *domain.GitPublicationIndex
	bindings   domain.ContextBranchProjection
	revision   domain.RepositoryRevision
	refs       map[string]domain.Ref
	owners     map[string]map[string]bool
	history    map[string]domain.HistoryEvent
}

func (s *Service) readGitAdmissionSnapshot(ctx context.Context, repo domain.ContentHash) (*gitAdmissionSnapshot, error) {
	return repositoryReadForRepo(ctx, s, repo, func(read context.Context) (*gitAdmissionSnapshot, error) {
		return s.readGitAdmissionSnapshotInRead(read, repo)
	})
}

func (s *Service) readGitAdmissionSnapshotInRead(ctx context.Context, repo domain.ContentHash) (*gitAdmissionSnapshot, error) {
	history, err := s.ListHistory(ctx, repo)
	if err != nil {
		return nil, err
	}
	snapshot, err := s.readGitAdmissionSnapshotWithHistory(ctx, repo, history)
	if err != nil {
		return nil, err
	}
	if err := s.captureGitAdmissionGeneration(ctx, repo, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *Service) captureGitAdmissionGeneration(ctx context.Context, repo domain.ContentHash, snapshot *gitAdmissionSnapshot) error {
	if generations, supported := s.meta.(outbound.RepositoryMetadataGenerationStore); supported {
		if transactions, known := s.meta.(outbound.RepositoryTransactionState); known && transactions.InReadOnlyTransaction(ctx) {
			generation, err := generations.ReadRepositoryMetadataGeneration(ctx, repo)
			if err != nil {
				return err
			}
			if generation.Validate() != nil || generation.RepoID != repo {
				return domain.ErrIntegrity
			}
			snapshot.generation = &generation
		}
	}
	return nil
}

func (s *Service) readGitAdmissionHistory(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent) (*gitAdmissionSnapshot, error) {
	metadata, err := s.meta.GetRepo(ctx, repo)
	if err != nil {
		return nil, err
	}
	index, err := domain.NewGitPublicationIndex(ctx, repo, history)
	if err != nil {
		return nil, err
	}
	bindings, err := domain.ProjectContextBranches(history)
	if err != nil {
		return nil, err
	}
	snapshot := &gitAdmissionSnapshot{repo: metadata, index: index, bindings: bindings, history: make(map[string]domain.HistoryEvent, len(history))}
	for _, event := range history {
		snapshot.history[event.ID] = event
	}
	return snapshot, nil
}

func (s *Service) readGitAdmissionSnapshotWithHistory(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent) (*gitAdmissionSnapshot, error) {
	snapshot, err := s.readGitAdmissionHistory(ctx, repo, history)
	if err != nil {
		return nil, err
	}
	revision, err := s.RepositoryRevision(ctx, repo)
	if err != nil {
		return nil, err
	}
	var refs []domain.Ref
	if lookup, supported := s.meta.(outbound.GitPublicationLookupStore); supported {
		refs, err = lookup.ListGitAdmissionRefs(ctx, repo)
	} else {
		refs, err = s.meta.ListRefs(ctx, repo)
	}
	if err != nil {
		return nil, err
	}
	snapshot.revision, snapshot.refs = revision, map[string]domain.Ref{}
	for _, ref := range refs {
		if ref.RepoID != repo {
			return nil, domain.ErrIntegrity
		}
		if ref.Kind == domain.RefBranch {
			if _, duplicate := snapshot.refs[ref.Name]; duplicate {
				return nil, domain.ErrIntegrity
			}
			snapshot.refs[ref.Name] = ref
		}
	}
	snapshot.owners = gitAdmissionOwners(snapshot.history)
	return snapshot, nil
}

func gitAdmissionOwners(history map[string]domain.HistoryEvent) map[string]map[string]bool {
	owners := map[string]map[string]bool{}
	for _, event := range history {
		switch event.Kind {
		case "birth", "attach", "position", "advance":
			if event.Target != "" && event.GitAfter != "" {
				if owners[event.GitAfter] == nil {
					owners[event.GitAfter] = map[string]bool{}
				}
				owners[event.GitAfter][event.BranchID] = true
			}
		}
	}
	return owners
}

func (s *Service) reuseGitAdmissionSnapshot(ctx context.Context, repo domain.ContentHash, frozen *gitAdmissionSnapshot) (*gitAdmissionSnapshot, error) {
	if frozen == nil || frozen.generation == nil {
		return s.readGitAdmissionSnapshot(ctx, repo)
	}
	if frozen.repo.ID != repo || frozen.generation.RepoID != repo || frozen.generation.Validate() != nil {
		return nil, domain.ErrIntegrity
	}
	storage, supported := s.meta.(outbound.RepositoryMetadataGenerationStore)
	if !supported {
		return nil, domain.ErrConflict
	}
	generation, err := storage.ReadRepositoryMetadataGeneration(ctx, repo)
	if err != nil {
		return nil, err
	}
	if generation.Validate() != nil || generation.RepoID != repo {
		return nil, domain.ErrIntegrity
	}
	metadata, err := s.meta.GetRepo(ctx, repo)
	if err != nil {
		return nil, err
	}
	revision, err := s.RepositoryRevision(ctx, repo)
	if err != nil {
		return nil, err
	}
	if generation != *frozen.generation || metadata.GitRemoteURL != frozen.repo.GitRemoteURL || revision.Graph != frozen.revision.Graph || revision.Evidence != frozen.revision.Evidence {
		return nil, domain.ErrConflict
	}
	return frozen, nil
}

func (snapshot *gitAdmissionSnapshot) destination(name, identity string) (domain.Ref, error) {
	resolved, err := contributionBranchName(snapshot.repo.ID, name, identity, snapshot.bindings)
	if err != nil {
		return domain.Ref{}, err
	}
	ref, exists := snapshot.refs[resolved]
	if !exists {
		return domain.Ref{}, domain.ErrNotFound
	}
	return bindContributionRef(ref, identity)
}

func (snapshot *gitAdmissionSnapshot) freeze(publicationID, sourceHead, destinationName, destinationID string) (frozenGitAdmission, error) {
	var frozen frozenGitAdmission
	publication, err := snapshot.index.Publication(publicationID)
	if err != nil {
		return frozen, err
	}
	carry, err := snapshot.index.CarryObservationIDs(publicationID, sourceHead)
	if err != nil {
		return frozen, err
	}
	for owner := range snapshot.owners[sourceHead] {
		if owner != publication.BranchID {
			return frozen, domain.ErrConflict
		}
	}
	sourceName := publication.LocalBranch
	if sourceName == "" {
		sourceName = publication.Branch
	}
	source, err := snapshot.destination(sourceName, publication.BranchID)
	if err != nil {
		return frozen, err
	}
	if source.Name != sourceName || source.Target != publication.Target || source.BranchID == destinationID {
		return frozen, domain.ErrConflict
	}
	destination, err := snapshot.destination(destinationName, destinationID)
	if err != nil {
		return frozen, err
	}
	if destination.Name != destinationName {
		return frozen, domain.ErrConflict
	}
	frozen.source, frozen.destination = source, destination
	frozen.snapshot = snapshot
	ordinary := snapshot.index.ObservationIDs(publication.ID)
	frozen.input = domain.GitContributionAdmission{
		Destination:    domain.GitDestinationCandidate{RepoID: snapshot.repo.ID, GitOrigin: snapshot.repo.GitRemoteURL, DestinationBranchID: destination.BranchID, DestinationBranch: destination.Name},
		SourceBranchID: source.BranchID, SourceBranch: source.Name, SourceAnchorID: snapshot.bindings.ByID[source.BranchID].EventID,
		DestinationAnchorID: snapshot.bindings.ByID[destination.BranchID].EventID, PublicationID: publication.ID,
		Capture: publication.Target, OriginalHead: publication.GitAfter, SourceHead: sourceHead,
		OrdinaryObservationIDs: ordinary[:min(len(ordinary), domain.MaxGitAdmissionWitnesses)], CarryObservationIDs: carry[:min(len(carry), domain.MaxGitAdmissionWitnesses)],
		GraphRevision: snapshot.revision.Graph, EvidenceRevision: snapshot.revision.Evidence,
	}
	return frozen, nil
}
