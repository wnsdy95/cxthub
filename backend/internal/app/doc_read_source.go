package app

import (
	"context"
	"encoding/json"
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

type docReadProjection uint8

const (
	docReadMetadata docReadProjection = iota
	docReadRanges
	docReadSearch
)

func (s *Service) rootReadSource(ctx context.Context, repo, hash domain.ContentHash, projection docReadProjection) (*docReadSource, error) {
	return repositoryReadForRepo(ctx, s, repo, func(bound context.Context) (*docReadSource, error) {
		snapshot, err := s.meta.GetSnapshot(bound, repo, hash)
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if err := matchReadSnapshot(domain.DocumentRef{Hash: hash, Identity: snapshot.DocIdentity}, snapshot); err != nil {
			return nil, err
		}
		if snapshot.DocIdentity == domain.DocumentIdentityLegacy {
			return nil, nil
		}
		source, err := s.verifiedRootReadSource(bound, repo, snapshot, projection)
		return &source, err
	})
}

func matchReadSnapshot(expected domain.DocumentRef, snapshot domain.Snapshot) error {
	if err := expected.Validate(); err != nil {
		return err
	}
	if err := snapshot.DocumentRef().Validate(); err != nil {
		return err
	}
	// Keep the legacy-only missing-scheme fence explicit, including empty pages.
	if expected.Identity == domain.DocumentIdentityLegacy && snapshot.DocIdentity != domain.DocumentIdentityLegacy {
		return domain.ErrUnsupportedDocumentIdentity
	}
	if !expected.MatchesSnapshot(snapshot) {
		return domain.ErrIntegrity
	}
	return nil
}

// These helpers use the caller's repository read context. Agent pages can join
// requested and coverage sources without opening nested or independent reads.
func (s *Service) verifiedRootDoc(ctx context.Context, repo domain.ContentHash, snapshot domain.Snapshot) (domain.VerifiedSessionDoc, error) {
	if err := ctx.Err(); err != nil {
		return domain.VerifiedSessionDoc{}, err
	}
	if snapshot.DocIdentity != domain.DocumentIdentityRootV1 {
		return domain.VerifiedSessionDoc{}, domain.ErrUnsupportedDocumentIdentity
	}
	reader, ok := s.blobs.(outbound.VerifiedDocReader)
	if !ok {
		return domain.VerifiedSessionDoc{}, domain.ErrUnsupportedDocumentIdentity
	}
	doc, err := reader.ReadVerifiedDoc(ctx, repo, snapshot.DocHash)
	if err != nil {
		return domain.VerifiedSessionDoc{}, err
	}
	if !doc.Reference().Matches(snapshot) {
		return domain.VerifiedSessionDoc{}, domain.ErrIntegrity
	}
	return doc, ctx.Err()
}
func (s *Service) verifiedRootReadSource(ctx context.Context, repo domain.ContentHash, snapshot domain.Snapshot, projection docReadProjection) (docReadSource, error) {
	doc, err := s.verifiedRootDoc(ctx, repo, snapshot)
	if err != nil {
		return docReadSource{}, err
	}
	plan, err := doc.PlanReadIndexContext(ctx)
	if err != nil {
		return docReadSource{}, err
	}
	var index domain.DocReadIndex
	switch projection {
	case docReadRanges:
		index, err = plan.BuildRangesContext(ctx)
	case docReadMetadata:
		index, err = plan.BuildMetadataContext(ctx)
	case docReadSearch:
		index, err = plan.BuildContext(ctx, nil)
	default:
		return docReadSource{}, domain.ErrIntegrity
	}
	if err != nil {
		return docReadSource{}, err
	}
	if err := ctx.Err(); err != nil {
		return docReadSource{}, err
	}
	return docReadSource{index: index, read: func(start, length int) ([]byte, error) { return doc.EventStreamRange(ctx, start, length) }}, nil
}

func (s *Service) agentHistoryReadSource(ctx context.Context, repo domain.ContentHash, ref domain.DocumentRef) (docReadSource, error) {
	if err := ref.Validate(); err != nil {
		return docReadSource{}, err
	}
	snapshot, err := s.meta.GetSnapshot(ctx, repo, ref.Hash)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return docReadSource{}, err
	}
	if err == nil {
		if err := matchReadSnapshot(ref, snapshot); err != nil {
			return docReadSource{}, err
		}
	} else if ref.Identity != domain.DocumentIdentityLegacy {
		return docReadSource{}, err
	}
	if ref.Identity == domain.DocumentIdentityRootV1 {
		return s.verifiedRootReadSource(ctx, repo, snapshot, docReadMetadata)
	}
	// Legacy prepared docs may lack snapshots. The indexed adapter must still
	// assert ownership and reject a root object, even behind a warm projection.
	indexed, ok := s.blobs.(outbound.DocReadStore)
	if !ok {
		return docReadSource{}, domain.ErrAgentHistoryUnavailable
	}
	idx, err := indexed.DocReadIndex(ctx, repo, ref.Hash)
	if err != nil {
		return docReadSource{}, err
	}
	return s.indexedReadSource(ctx, repo, ref.Hash, idx), nil
}

// materializedDocumentRead is for whole-document consumers only. The stored
// snapshot selects the reference; a root is materialized from its current owned
// proof without relabeling its canonical CIR byte hash. No read index is needed.
func (s *Service) materializedDocumentRead(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	if err := ctx.Err(); err != nil {
		return domain.SessionDoc{}, err
	}
	snapshot, err := s.meta.GetSnapshot(ctx, repo, hash)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return domain.SessionDoc{}, err
	}
	if err == nil {
		if err := matchReadSnapshot(domain.DocumentRef{Hash: hash, Identity: snapshot.DocIdentity}, snapshot); err != nil {
			return domain.SessionDoc{}, err
		}
		if snapshot.DocIdentity == domain.DocumentIdentityRootV1 {
			proof, err := s.verifiedRootDoc(ctx, repo, snapshot)
			if err != nil {
				return domain.SessionDoc{}, err
			}
			doc := domain.SessionDoc{Hash: hash, Identity: proof.DocumentRef().Identity}
			if err := json.Unmarshal(proof.Bytes(), &doc.CIR); err != nil {
				return domain.SessionDoc{}, domain.ErrIntegrity
			}
			if err := ctx.Err(); err != nil {
				return domain.SessionDoc{}, err
			}
			return doc, nil
		}
	}
	doc, err := s.blobs.GetDoc(ctx, repo, hash)
	if err != nil {
		return domain.SessionDoc{}, err
	}
	if doc.DocumentRef() != (domain.DocumentRef{Hash: hash}) {
		return domain.SessionDoc{}, domain.ErrIntegrity
	}
	return doc, ctx.Err()
}

func (s *Service) documentReadSource(ctx context.Context, repo, hash domain.ContentHash) (docReadSource, error) {
	root, err := s.rootReadSource(ctx, repo, hash, docReadRanges)
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
	return s.indexedReadSource(ctx, repo, hash, index), nil
}

func (s *Service) indexedReadSource(ctx context.Context, repo, hash domain.ContentHash, index domain.DocReadIndex) docReadSource {
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
	}}
}
