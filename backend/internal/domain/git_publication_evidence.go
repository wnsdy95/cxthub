package domain

import (
	"context"
	"sort"
)

type GitPublicationIndex struct {
	publications         map[string]HistoryEvent
	ordinaryObservations map[publicationObservationKey][]string
}

func NewGitPublicationIndex(ctx context.Context, repo ContentHash, history []HistoryEvent) (*GitPublicationIndex, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ValidateContentHash(repo) != nil {
		return nil, ErrValidation
	}
	seen := map[string]bool{}
	proofs := map[publicationObservationKey][]string{}
	for _, event := range history {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if event.RepoID != string(repo) || ValidateHistoryEvent(event) != nil || seen[event.ID] {
			return nil, ErrIntegrity
		}
		seen[event.ID] = true
		switch event.Kind {
		case "birth", "attach", "position", "advance":
			if event.Target != "" && ValidateGitOID(event.GitAfter) == nil {
				key := publicationKey(event)
				proofs[key] = append(proofs[key], event.ID)
			}
		}
	}
	for _, ids := range proofs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sort.Strings(ids)
	}
	index := &GitPublicationIndex{publications: map[string]HistoryEvent{}, ordinaryObservations: proofs}
	for _, event := range history {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if event.Kind == "publish" && len(proofs[publicationKey(event)]) > 0 {
			index.publications[event.ID] = event
		}
	}
	return index, nil
}

func (index *GitPublicationIndex) ObservationIDs(id string) []string {
	if index == nil {
		return nil
	}
	publication, exists := index.publications[id]
	if !exists {
		return []string{}
	}
	return append([]string{}, index.ordinaryObservations[publicationKey(publication)]...)
}

func (index *GitPublicationIndex) CarryObservationIDs(publicationID, sourceHead string) ([]string, error) {
	if err := ValidateGitOID(sourceHead); err != nil {
		return nil, err
	}
	publication, err := index.Publication(publicationID)
	if err != nil {
		return nil, err
	}
	key := publicationKey(publication)
	key.git = sourceHead
	ids := index.ordinaryObservations[key]
	if len(ids) == 0 {
		return nil, ErrNotFound
	}
	return append([]string{}, ids...), nil
}

func (index *GitPublicationIndex) Publication(id string) (HistoryEvent, error) {
	if index == nil {
		return HistoryEvent{}, ErrNotFound
	}
	publication, exists := index.publications[id]
	if !exists {
		return HistoryEvent{}, ErrNotFound
	}
	return publication, nil
}
