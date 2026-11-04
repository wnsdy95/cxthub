package memoryquery

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (queries *MinimumQueries) ObserveMemoryReadRequirement(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.MemoryReadRequirement, error) {
	if request.Cursor != "" {
		return domain.MemoryReadRequirement{}, domain.ErrValidation
	}
	target, err := ReadTarget(repo, request)
	if err != nil {
		return domain.MemoryReadRequirement{}, err
	}
	if !queries.Available() {
		return domain.MemoryReadRequirement{}, domain.ErrCatalogUnsupported
	}
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), queries.supported)
	var requirement domain.MemoryReadRequirement
	err = queries.scope.WithMemoryReadResources(ctx, repo, func(bound context.Context) error {
		var err error
		requirement, err = queries.reader.ObserveMemoryReadRequirement(bound, target, func(current context.Context) (context.Context, error) {
			return queries.scope.AuthorizeSnapshot(current, repo)
		})
		return err
	})
	if err != nil {
		return domain.MemoryReadRequirement{}, err
	}
	if err := requirement.Validate(target); err != nil {
		return domain.MemoryReadRequirement{}, err
	}
	return requirement, nil
}

type minimumMemoryRangeReader struct {
	scope       Scope
	storage     outbound.MinimumMemoryReadStore
	target      domain.MemoryReadTarget
	requirement domain.MemoryReadRequirement
	generation  domain.ContentHash
	status      domain.MemoryReadStatus
}

func (reader *minimumMemoryRangeReader) WithMemoryReadRange(ctx context.Context, repo, identity domain.ContentHash, consume func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
	if repo != reader.target.RepoID || identity != reader.requirement.TargetID {
		return domain.ErrValidation
	}
	return reader.storage.WithMinimumMemoryReadRange(ctx, reader.target, reader.requirement, reader.generation,
		func(current context.Context) (context.Context, error) {
			return reader.scope.AuthorizeSnapshot(current, repo)
		},
		func(bound context.Context, status domain.MemoryReadStatus, header domain.MemoryReadHeader, chunks outbound.MemoryReadChunkReader) error {
			if status.Validate() != nil || status.Minimum != reader.requirement.Minimum || status.Applied != header.Source ||
				(reader.generation != "" && status.Generation != reader.generation) {
				return domain.ErrIntegrity
			}
			reader.status = status
			return consume(bound, header, chunks)
		})
}

type minimumMemoryCursor struct {
	Version      int                `json:"version"`
	Requirement  domain.ContentHash `json:"requirement"`
	Generation   domain.ContentHash `json:"generation"`
	Continuation string             `json:"continuation"`
}

func minimumMemoryContinuation(encoded string, requirement, generation domain.ContentHash) (string, error) {
	if encoded == "" {
		return "", nil
	}
	if generation == "" || len(encoded) > 1024 {
		return "", domain.ErrValidation
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", domain.ErrValidation
	}
	var cursor minimumMemoryCursor
	if err := json.Unmarshal(raw, &cursor); err != nil || cursor.Version != 1 || cursor.Continuation == "" {
		return "", domain.ErrValidation
	}
	canonical, err := json.Marshal(cursor)
	if err != nil || !bytes.Equal(raw, canonical) {
		return "", domain.ErrValidation
	}
	if cursor.Generation != generation || cursor.Requirement != requirement {
		return "", domain.ErrEffectiveMemoryCursorStale
	}
	return cursor.Continuation, nil
}

func (queries *MinimumQueries) QueryMinimumMemory(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest, requirement domain.MemoryReadRequirement, generation domain.ContentHash) (domain.MemoryReadPage, error) {
	target, err := ReadTarget(repo, request)
	if err != nil {
		return domain.MemoryReadPage{}, err
	}
	if err := requirement.Validate(target); err != nil {
		return domain.MemoryReadPage{}, err
	}
	if err := domain.ValidateOptionalContentHash(generation); err != nil {
		return domain.MemoryReadPage{}, err
	}
	raw, err := json.Marshal(requirement)
	if err != nil {
		return domain.MemoryReadPage{}, err
	}
	requirementID := domain.HashContent(raw)
	request.Cursor, err = minimumMemoryContinuation(request.Cursor, requirementID, generation)
	if err != nil {
		return domain.MemoryReadPage{}, err
	}
	if !queries.Available() {
		return domain.MemoryReadPage{}, domain.ErrCatalogUnsupported
	}
	reader := &minimumMemoryRangeReader{scope: queries.scope, storage: queries.reader, target: target, requirement: requirement, generation: generation}
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), queries.supported)
	var page domain.EffectiveMemoryPage
	err = queries.scope.WithMemoryReadResources(ctx, repo, func(bound context.Context) error {
		var err error
		page, err = ReadPage(bound, reader, repo, requirement.TargetID, request)
		return err
	})
	if err != nil {
		return domain.MemoryReadPage{}, err
	}
	if page.NextCursor != "" {
		raw, err := json.Marshal(minimumMemoryCursor{Version: 1, Requirement: requirementID, Generation: reader.status.Generation, Continuation: page.NextCursor})
		if err != nil {
			return domain.MemoryReadPage{}, err
		}
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
		if len(page.NextCursor) > 1024 {
			return domain.MemoryReadPage{}, domain.ErrMemoryProjectionLimit
		}
	}
	return domain.MemoryReadPage{Status: reader.status, Page: page}, nil
}
