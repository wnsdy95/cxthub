package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.AnchoredPublicationStore = (*FSStore)(nil)

// The development store keeps a repository's publications in one file.
func (s *FSStore) anchoredPublicationPath(repo domain.ContentHash) string {
	return filepath.Join(s.repoDir(repo), "anchored-publications.json")
}

func sortAnchoredPublications(publications []domain.AnchoredPublication) {
	sort.Slice(publications, func(left, right int) bool {
		if publications[left].Provider != publications[right].Provider {
			return publications[left].Provider < publications[right].Provider
		}
		return publications[left].SessionID < publications[right].SessionID
	})
}

func (s *FSStore) readAnchoredPublications(ctx context.Context, repo domain.ContentHash) ([]domain.AnchoredPublication, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	publications, err := readRepoJSON[domain.AnchoredPublication](ctx, s.anchoredPublicationPath(repo), "publication list")
	if err != nil {
		return nil, err
	}
	for _, publication := range publications {
		if err := publication.Validate(); err != nil || len(publication.Ranges) == 0 {
			return nil, fmt.Errorf("%w: invalid stored publication", domain.ErrIntegrity)
		}
	}
	sortAnchoredPublications(publications)
	return publications, nil
}

func (s *FSStore) GetAnchoredPublication(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) ([]domain.AnchoredPublishedRange, error) {
	lock := s.anchoredSessionLock(repo)
	lock.Lock()
	defer lock.Unlock()
	publications, err := s.readAnchoredPublications(ctx, repo)
	if err != nil {
		return nil, err
	}
	for _, publication := range publications {
		if publication.Provider == provider && publication.SessionID == sessionID {
			return publication.Ranges, nil
		}
	}
	return []domain.AnchoredPublishedRange{}, nil
}

func (s *FSStore) PutAnchoredPublication(ctx context.Context, repo domain.ContentHash, publication domain.AnchoredPublication) error {
	if err := validateHash(repo); err != nil {
		return err
	}
	if err := publication.Validate(); err != nil {
		return err
	}
	if len(publication.Ranges) == 0 {
		return fmt.Errorf("%w: a publication holds at least one range", domain.ErrValidation)
	}
	lock := s.anchoredSessionLock(repo)
	lock.Lock()
	defer lock.Unlock()
	publications, err := s.readAnchoredPublications(ctx, repo)
	if err != nil {
		return err
	}
	next := make([]domain.AnchoredPublication, 0, len(publications)+1)
	for _, existing := range publications {
		if existing.Provider != publication.Provider || existing.SessionID != publication.SessionID {
			next = append(next, existing)
		}
	}
	next = append(next, publication)
	sortAnchoredPublications(next)
	return writeRepoJSON(s.anchoredPublicationPath(repo), next)
}

func (s *FSStore) ClearAnchoredPublication(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) error {
	if err := validateHash(repo); err != nil {
		return err
	}
	lock := s.anchoredSessionLock(repo)
	lock.Lock()
	defer lock.Unlock()
	publications, err := s.readAnchoredPublications(ctx, repo)
	if err != nil {
		return err
	}
	next := make([]domain.AnchoredPublication, 0, len(publications))
	for _, existing := range publications {
		if existing.Provider != provider || existing.SessionID != sessionID {
			next = append(next, existing)
		}
	}
	if len(next) == len(publications) {
		return nil
	}
	return writeRepoJSON(s.anchoredPublicationPath(repo), next)
}

func (s *FSStore) ListAnchoredPublications(ctx context.Context, repo domain.ContentHash) ([]domain.AnchoredPublication, error) {
	lock := s.anchoredSessionLock(repo)
	lock.Lock()
	defer lock.Unlock()
	return s.readAnchoredPublications(ctx, repo)
}
