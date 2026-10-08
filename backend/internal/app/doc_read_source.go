package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// A root read keeps its index and ranges attached to the same owned, verified
// bytes. Existing legacy projections keep their indexed, lazy range reads.
type docReadSource struct {
	index domain.DocReadIndex
	read  func(int, int) ([]byte, error)
}

func (s *Service) rootReadSource(ctx context.Context, repo, hash domain.ContentHash) (*docReadSource, error) {
	snapshot, err := s.meta.GetSnapshot(ctx, repo, hash)
	if errors.Is(err, domain.ErrNotFound) {
		// Prepared legacy documents can be read before snapshot publication.
		// Root preparation remains disabled in this reader-only stage.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if snapshot.DocIdentity == domain.DocumentIdentityLegacy {
		return nil, nil
	}
	if err := snapshot.DocumentRef().Validate(); err != nil {
		return nil, err
	}
	reader, ok := s.blobs.(outbound.VerifiedDocReader)
	if !ok {
		return nil, domain.ErrUnsupportedDocumentIdentity
	}
	return repositoryRead(ctx, s, func(bound context.Context) (*docReadSource, error) {
		current, err := s.meta.GetSnapshot(bound, repo, hash)
		if err != nil {
			return nil, err
		}
		if current.DocumentRef() != snapshot.DocumentRef() {
			return nil, domain.ErrIntegrity
		}
		doc, err := reader.ReadVerifiedDoc(bound, repo, hash)
		if err != nil {
			return nil, err
		}
		if !doc.Reference().Matches(current) {
			return nil, domain.ErrIntegrity
		}
		plan, err := doc.PlanReadIndexContext(bound)
		if err != nil {
			return nil, err
		}
		index, err := plan.Build(nil)
		if err != nil {
			return nil, err
		}
		if err := bound.Err(); err != nil {
			return nil, err
		}
		return &docReadSource{index: index, read: func(start, length int) ([]byte, error) {
			return doc.EventStreamRange(ctx, start, length)
		}}, nil
	})
}

func (s *Service) documentReadSource(ctx context.Context, repo, hash domain.ContentHash) (docReadSource, error) {
	root, err := s.rootReadSource(ctx, repo, hash)
	if err != nil {
		return docReadSource{}, err
	}
	if root != nil {
		return *root, nil
	}
	index, err := s.legacyDocReadIndex(ctx, repo, hash)
	if err != nil {
		return docReadSource{}, err
	}
	var read func(int, int) ([]byte, error)
	return docReadSource{index: index, read: func(start, length int) ([]byte, error) {
		if read == nil {
			var err error
			read, err = s.eventRangeReader(ctx, repo, hash)
			if err != nil {
				return nil, err
			}
		}
		return read(start, length)
	}}, nil
}
