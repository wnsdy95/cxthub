package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (g *GitScans) discoverGitAssociations(ctx context.Context, repo domain.Repo) (bool, error) {
	store, supported := g.core.meta.(outbound.GitAssociationDiscoveryStore)
	catalog, catalogSupported := g.core.meta.(outbound.CatalogStore)
	_, atomic := g.core.meta.(outbound.RepositoryTransactions)
	if !supported || !catalogSupported || !atomic {
		return false, nil
	}
	work, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	currentRepo, err := g.core.workerRepository(work, repo.ID)
	if err != nil {
		return false, err
	}
	if currentRepo.GitRemoteURL != repo.GitRemoteURL {
		return false, domain.ErrConflict
	}
	authorization, err := authorizeGitAdmission(work, g.sourceAccess, repo.ID)
	if err != nil {
		return false, err
	}
	previous, err := store.GetGitAssociationDiscovery(work, repo.ID, repo.GitRemoteURL)
	if errors.Is(err, domain.ErrNotFound) {
		previous = domain.GitAssociationDiscovery{RepoID: repo.ID, GitOrigin: repo.GitRemoteURL}
	} else if err != nil {
		return false, err
	}
	page, readErr := catalog.CatalogChanges(work, repo.ID, previous.Request())
	var next domain.GitAssociationDiscovery
	var publications []domain.HistoryEvent
	if errors.Is(readErr, domain.ErrCatalogResetRequired) {
		if previous.Cursor == "" && previous.Checkpoint == nil {
			return false, readErr
		}
		next, err = previous.Reset()
	} else if readErr != nil {
		return false, readErr
	} else {
		next, err = previous.Advance(page)
		if err == nil {
			publications, err = gitAssociationPublications(page)
		}
	}
	if err != nil {
		return false, err
	}
	unchanged := next
	unchanged.Version = previous.Version
	if reflect.DeepEqual(previous, unchanged) {
		return false, nil
	}
	err = repositoryWriteError(context.WithValue(work, revisionScopeKey{}, "none"), g.core, repo.ID, func(write context.Context) error {
		current, err := g.core.meta.GetRepo(write, repo.ID)
		if err != nil {
			return err
		}
		if current.GitRemoteURL != repo.GitRemoteURL {
			return domain.ErrConflict
		}
		if err := authorization.fence(write); err != nil {
			return err
		}
		return store.SaveGitAssociationDiscovery(write, next, previous.Version, publications)
	})
	return err == nil, err
}

func gitAssociationPublications(page domain.CatalogPage) ([]domain.HistoryEvent, error) {
	publications := []domain.HistoryEvent{}
	seen := map[string]bool{}
	for _, entry := range page.Entries {
		if entry.Kind != "history" {
			continue
		}
		var event domain.HistoryEvent
		if entry.Deleted || json.Unmarshal(entry.Value, &event) != nil || domain.ValidateHistoryEvent(event) != nil || event.ID != entry.Key || event.RepoID != string(page.RepoID) || seen[event.ID] {
			return nil, domain.ErrIntegrity
		}
		seen[event.ID] = true
		if event.Kind == "publish" {
			publications = append(publications, event)
		}
	}
	return publications, nil
}
