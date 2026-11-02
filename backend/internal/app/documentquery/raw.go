package documentquery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type RawConfig struct {
	Profile   Profile
	Snapshots SnapshotReader
	Legacy    LegacyReader
	Index     outbound.DocReadStore
	Verified  outbound.VerifiedDocReader
}

type RawReader struct {
	profile Profile
	source  Queries
}

type SearchIndex struct {
	index domain.DocReadIndex
}

func NewRawReader(config RawConfig) (*RawReader, error) {
	if config.Profile != LegacyCompatibility && config.Profile != VerifiedRoots {
		return nil, fmt.Errorf("%w: raw document query profile is required", domain.ErrValidation)
	}
	for _, required := range []any{config.Snapshots, config.Legacy} {
		if absent(required) {
			return nil, fmt.Errorf("%w: missing raw document query dependency", domain.ErrValidation)
		}
	}
	for _, optional := range []any{config.Index, config.Verified} {
		if optional != nil && absent(optional) {
			return nil, fmt.Errorf("%w: typed-nil raw document query dependency", domain.ErrValidation)
		}
	}
	if config.Profile == VerifiedRoots && (config.Index == nil || config.Verified == nil) {
		return nil, domain.ErrUnsupportedDocumentIdentity
	}
	return &RawReader{profile: config.Profile, source: Queries{
		metadata: config.Snapshots, legacy: config.Legacy, index: config.Index, verified: config.Verified,
	}}, nil
}

func (reader *RawReader) Profile() Profile {
	return reader.profile
}

// materializedDocumentRead is for whole-document consumers only. The stored
// snapshot selects the reference; a root is materialized from its current owned
// proof without relabeling its canonical CIR byte hash. No read index is needed.
func (reader *RawReader) MaterializedDocumentRead(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	if err := ctx.Err(); err != nil {
		return domain.SessionDoc{}, err
	}
	snapshot, err := reader.source.metadata.GetSnapshot(ctx, repo, hash)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return domain.SessionDoc{}, err
	}
	if err == nil {
		if err := matchReadSnapshot(domain.DocumentRef{Hash: hash, Identity: snapshot.DocIdentity}, snapshot); err != nil {
			return domain.SessionDoc{}, err
		}
		if snapshot.DocIdentity == domain.DocumentIdentityRootV1 {
			proof, err := reader.source.verifiedRootDoc(ctx, repo, snapshot)
			if err != nil {
				return domain.SessionDoc{}, err
			}
			document := domain.SessionDoc{Hash: hash, Identity: proof.DocumentRef().Identity}
			if err := json.Unmarshal(proof.Bytes(), &document.CIR); err != nil {
				return domain.SessionDoc{}, domain.ErrIntegrity
			}
			if err := ctx.Err(); err != nil {
				return domain.SessionDoc{}, err
			}
			return document, nil
		}
	}
	document, err := reader.source.legacy.GetDoc(ctx, repo, hash)
	if err != nil {
		return domain.SessionDoc{}, err
	}
	if document.DocumentRef() != (domain.DocumentRef{Hash: hash}) {
		return domain.SessionDoc{}, domain.ErrIntegrity
	}
	return document, ctx.Err()
}

func (reader *RawReader) RootSearchIndex(ctx context.Context, repo, hash domain.ContentHash) (*SearchIndex, error) {
	snapshot, err := reader.source.metadata.GetSnapshot(ctx, repo, hash)
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
	source, err := reader.source.verifiedRootReadSource(ctx, repo, snapshot, docReadSearch)
	if err != nil {
		return nil, err
	}
	return &SearchIndex{index: source.index}, nil
}

func (index *SearchIndex) Search(ctx context.Context, query string, after, limit int) ([]domain.DocEventIndex, error) {
	return searchReadIndex(ctx, index.index, query, after, limit)
}

func (reader *RawReader) SearchDocEvents(ctx context.Context, repo, hash domain.ContentHash, query string, after, limit int) ([]domain.DocEventIndex, error) {
	query = strings.ToLower(strings.TrimSpace(query))
	if len([]rune(query)) < 2 || len([]rune(query)) > 256 || after < -1 || limit < 1 || limit > 201 {
		return nil, fmt.Errorf("%w: invalid event search", domain.ErrValidation)
	}
	root, err := reader.RootSearchIndex(ctx, repo, hash)
	if err != nil {
		return nil, err
	}
	if root != nil {
		return root.Search(ctx, query, after, limit)
	}
	if indexed := reader.source.index; indexed != nil {
		return indexed.SearchDocEvents(ctx, repo, hash, query, after, limit)
	}
	index, err := reader.source.legacyDocReadIndex(ctx, repo, hash)
	if err != nil {
		return nil, err
	}
	return searchReadIndex(ctx, index, query, after, limit)
}
