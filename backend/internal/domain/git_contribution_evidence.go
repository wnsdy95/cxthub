package domain

import (
	"context"
	"slices"
)

type GitPublicationEvidence struct {
	Index    *GitPublicationIndex
	Bindings ContextBranchProjection
	History  map[string]HistoryEvent
}

func NewGitPublicationEvidence(ctx context.Context, repo ContentHash, metadata Repo, history []HistoryEvent) (GitPublicationEvidence, error) {
	if metadata.ID != repo {
		return GitPublicationEvidence{}, ErrIntegrity
	}
	index, err := NewGitPublicationIndex(ctx, repo, history)
	if err != nil {
		return GitPublicationEvidence{}, err
	}
	bindings, err := ProjectContextBranches(history)
	if err != nil {
		return GitPublicationEvidence{}, err
	}
	evidence := GitPublicationEvidence{Index: index, Bindings: bindings, History: make(map[string]HistoryEvent, len(history))}
	for _, event := range history {
		evidence.History[event.ID] = event
	}
	return evidence, nil
}

func GitAdmittedPublication(metadata Repo, index *GitPublicationIndex, history map[string]HistoryEvent, admission GitContributionAdmission, head GitContextHead) (HistoryEvent, error) {
	if admission.Validate() != nil || head.Validate() != nil || admission.Destination.RepoID != metadata.ID || head.Destination.RepoID != metadata.ID || admission.Destination.GitOrigin != metadata.GitRemoteURL || head.Destination.GitOrigin != metadata.GitRemoteURL || index == nil {
		return HistoryEvent{}, ErrIntegrity
	}
	publication, err := index.Publication(admission.PublicationID)
	if err != nil {
		return HistoryEvent{}, err
	}
	name := publication.LocalBranch
	if name == "" {
		name = publication.Branch
	}
	if publication.RepoID != string(metadata.ID) || publication.BranchID != admission.SourceBranchID || publication.Target != admission.Capture || publication.GitAfter != admission.OriginalHead || name != admission.SourceBranch {
		return HistoryEvent{}, ErrIntegrity
	}
	ordinary := index.ObservationIDs(publication.ID)
	carry, err := index.CarryObservationIDs(publication.ID, admission.SourceHead)
	if err != nil {
		return HistoryEvent{}, err
	}
	for _, pair := range []struct{ frozen, current []string }{{admission.OrdinaryObservationIDs, ordinary}, {admission.CarryObservationIDs, carry}} {
		for _, observationID := range pair.frozen {
			if !slices.Contains(pair.current, observationID) {
				return HistoryEvent{}, ErrConflict
			}
		}
	}
	for _, anchor := range []struct{ id, branch, name string }{{admission.SourceAnchorID, admission.SourceBranchID, admission.SourceBranch}, {admission.DestinationAnchorID, admission.Destination.DestinationBranchID, admission.Destination.DestinationBranch}, {head.AnchorID, head.Destination.DestinationBranchID, head.Destination.DestinationBranch}} {
		if anchor.id == "" {
			if anchor.branch != LegacyContextBranchID(string(metadata.ID), anchor.name) {
				return HistoryEvent{}, ErrConflict
			}
			continue
		}
		event, exists := history[anchor.id]
		if !exists || event.RepoID != string(metadata.ID) || !IsBranchBindingEvent(event) || event.Kind == "archive" || event.BranchID != anchor.branch || event.Branch != anchor.name {
			return HistoryEvent{}, ErrConflict
		}
	}
	if head.Destination.DestinationBranchID != admission.Destination.DestinationBranchID {
		return HistoryEvent{}, ErrConflict
	}
	seen := map[string]bool{}
	for anchor := head.AnchorID; anchor != admission.DestinationAnchorID; {
		if anchor == "" || seen[anchor] {
			return HistoryEvent{}, ErrConflict
		}
		seen[anchor] = true
		event, exists := history[anchor]
		if !exists || event.RepoID != string(metadata.ID) || !IsBranchBindingEvent(event) || event.Kind == "archive" || event.BranchID != admission.Destination.DestinationBranchID {
			return HistoryEvent{}, ErrConflict
		}
		anchor = event.BindingParent
	}
	return publication, nil
}

func GitSupplementPublication(index *GitPublicationIndex, record GitContribution, id string) (HistoryEvent, error) {
	if record.Validate() != nil || index == nil {
		return HistoryEvent{}, ErrIntegrity
	}
	publication, err := index.Publication(id)
	if err != nil {
		return HistoryEvent{}, err
	}
	if publication.RepoID != string(record.RepoID) || publication.BranchID != record.SourceBranchID || publication.Target != record.Capture || publication.Source != record.Capture || publication.GitAfter != record.OriginalHead || !publication.MemoryPinned {
		return HistoryEvent{}, ErrConflict
	}
	return publication, nil
}
