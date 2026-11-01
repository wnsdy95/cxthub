package memoryquery

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

var _ inbound.PreparedMemoryQuery = (*PreparedQueries)(nil)
var _ inbound.PreparedMemoryReader = (*PreparedQueries)(nil)

type PreparedCursor struct {
	Version      int                          `json:"version"`
	Requirement  domain.MemoryReadRequirement `json:"requirement"`
	Generation   domain.ContentHash           `json:"generation"`
	Continuation string                       `json:"continuation"`
}

func EncodePreparedCursor(cursor PreparedCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	if len(encoded) > domain.MaxPreparedMemoryCursorBytes {
		return "", domain.ErrMemoryProjectionLimit
	}
	return encoded, nil
}

func DecodePreparedCursor(encoded string, target domain.MemoryReadTarget) (PreparedCursor, error) {
	var cursor PreparedCursor
	if len(encoded) > domain.MaxPreparedMemoryCursorBytes {
		return cursor, domain.ErrValidation
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Version != 1 ||
		len(cursor.Continuation) > 1024 || domain.ValidateOptionalContentHash(cursor.Generation) != nil ||
		(cursor.Generation == "" && cursor.Continuation != "") {
		return PreparedCursor{}, domain.ErrValidation
	}
	canonical, err := json.Marshal(cursor)
	if err != nil || !bytes.Equal(canonical, raw) {
		return PreparedCursor{}, domain.ErrValidation
	}
	if err := cursor.Requirement.Validate(target); err != nil {
		return PreparedCursor{}, err
	}
	return cursor, nil
}

func (queries *PreparedQueries) QueryPreparedMemory(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
	encoded := request.Cursor
	request.Cursor = ""
	target, err := ReadTarget(repo, request)
	if err != nil || len(encoded) > domain.MaxPreparedMemoryCursorBytes {
		return domain.MemoryReadPage{}, domain.ErrValidation
	}
	if queries == nil || !queries.configured[repo] || !queries.minimum.Available() {
		return domain.MemoryReadPage{}, domain.ErrCatalogUnsupported
	}
	var result domain.MemoryReadPage
	err = queries.WithMemoryReadResources(ctx, repo, func(bound context.Context) error {
		cursor := PreparedCursor{Version: 1}
		var err error
		if encoded == "" {
			cursor.Requirement, err = queries.minimum.ObserveMemoryReadRequirement(bound, repo, request)
		} else {
			cursor, err = DecodePreparedCursor(encoded, target)
		}
		if err != nil {
			return err
		}
		request.Cursor = cursor.Continuation
		result, err = queries.minimum.QueryMinimumMemory(bound, repo, request, cursor.Requirement, cursor.Generation)
		if domain.IsOnlyMemoryReadPending(err) {
			if cursor.Generation != "" {
				return domain.ErrEffectiveMemoryCursorStale
			}
			if queries.demands[repo] {
				if err := queries.requester.RequestPrepared(bound, target, cursor.Requirement); err != nil {
					return err
				}
			}
			retry, encodeErr := EncodePreparedCursor(cursor)
			if encodeErr != nil {
				return encodeErr
			}
			return &domain.MemoryReadPending{Cursor: retry}
		}
		if err != nil {
			return err
		}
		cursor.Generation = result.Status.Generation
		result.Cursor, err = EncodePreparedCursor(cursor)
		if err != nil {
			return err
		}
		if result.Page.NextCursor != "" {
			cursor.Continuation = result.Page.NextCursor
			result.Page.NextCursor, err = EncodePreparedCursor(cursor)
		}
		return err
	})
	if err != nil {
		return domain.MemoryReadPage{}, err
	}
	return result, nil
}
