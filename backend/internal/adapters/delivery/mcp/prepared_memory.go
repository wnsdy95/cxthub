package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const maxPreparedMemoryTransportCursorBytes = 8 << 10

func (server *Server) SetPreparedMemory(reader inbound.PreparedMemoryReader) error {
	if reader == nil {
		return domain.ErrValidation
	}
	switch value := reflect.ValueOf(reader); value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return domain.ErrValidation
		}
	}
	server.preparedMemory = reader
	return nil
}

func (server *Server) SetMemoryPreparationRequester(requester outbound.MemoryPreparationRequester) {
	server.memoryPreparation = requester
}

func encodePreparedMemoryCursor(cursor pageCursor) (string, error) {
	if cursor.EffectiveCursor == "" || len(cursor.EffectiveCursor) > domain.MaxPreparedMemoryCursorBytes {
		return "", domain.ErrIntegrity
	}
	encoded := encodeCursor(cursor)
	if len(encoded) > maxPreparedMemoryTransportCursorBytes {
		return "", domain.ErrMemoryProjectionLimit
	}
	return encoded, nil
}

func (server *Server) preparedMemoryPage(ctx context.Context, repo domain.Repo, args toolArgs) (string, error) {
	if domain.ValidateGitOID(args.CodeCommit) != nil || domain.ValidateOptionalContentHash(domain.ContentHash(args.MemoryHash)) != nil {
		return "", domain.ErrValidation
	}
	cursor, err := cursorFor(string(repo.ID), "memory_load", args)
	if err != nil {
		return "", domain.ErrValidation
	}
	if args.Cursor != "" {
		if cursor.Version != 5 || cursor.FragmentFormat != "memory-prepared-v1" || cursor.Snapshot == "" ||
			cursor.EffectiveCursor == "" || len(cursor.EffectiveCursor) > domain.MaxPreparedMemoryCursorBytes || cursor.Index != 0 ||
			(cursor.PreparedPending && (cursor.Offset != 0 || cursor.Projection != "" || cursor.Generation != "")) ||
			(!cursor.PreparedPending && (cursor.Projection == "" || cursor.Generation == "")) {
			return "", domain.ErrValidation
		}
	}
	if server.preparedMemory == nil {
		return "", domain.ErrCatalogUnsupported
	}
	if args.Cursor == "" {
		snapshot, err := server.resolveRef(ctx, repo, args.Ref)
		if err != nil {
			return "", err
		}
		cursor.Snapshot = snapshot.ID
		if args.MemoryHash == "" {
			cursor.ContextBranch, err = server.memoryBranch(ctx, repo, args.Ref)
			if err != nil {
				return "", err
			}
		}
	}
	selection := domain.EffectiveMemorySelection{Branch: cursor.ContextBranch, SnapshotID: cursor.Snapshot, CodeCommit: args.CodeCommit, MemoryHash: domain.ContentHash(args.MemoryHash)}
	if selection.Validate() != nil || !utf8.ValidString(selection.Branch) {
		return "", domain.ErrValidation
	}
	cursor.Version = 5
	cursor.FragmentFormat = "memory-prepared-v1"
	out, err := server.preparedMemory.QueryPreparedMemory(ctx, repo.ID, domain.EffectiveMemoryRequest{Selection: selection, Limit: 1, Cursor: cursor.EffectiveCursor})
	if err != nil {
		failure := preparedMemoryFailure(err).(*preparedMemoryToolError)
		var pending *domain.MemoryReadPending
		if failure.Code == "memory_read_not_ready" && domain.IsOnlyMemoryReadPending(err) && errors.As(err, &pending) && pending != nil {
			cursor.EffectiveCursor = pending.Cursor
			cursor.PreparedPending = cursor.Projection == ""
			retry, encodeErr := encodePreparedMemoryCursor(cursor)
			if encodeErr != nil {
				return "", encodeErr
			}
			failure.RetryCursor = retry
			failure.preparation = &domain.MemoryReadPreparationRequest{Version: 1, RepoID: repo.ID,
				Selection: selection, Cursor: pending.Cursor, DocumentIdentities: inbound.DocumentIdentities(ctx)}
		}
		return "", failure
	}
	page := out.Page
	if out.Status.Validate() != nil || out.Status.Applied.Graph.RepoID != repo.ID || page.Selection != selection ||
		domain.ValidateContentHash(page.StateHash) != nil || domain.ValidateContentHash(page.LineageHash) != nil ||
		len(page.Items) > 1 || page.Total < len(page.Items) || len(out.Cursor) > domain.MaxPreparedMemoryCursorBytes || len(page.NextCursor) > domain.MaxPreparedMemoryCursorBytes {
		return "", domain.ErrIntegrity
	}
	if cursor.Projection != "" && (cursor.Projection != page.StateHash || cursor.Generation != out.Status.Generation) {
		return "", domain.ErrEffectiveMemoryCursorStale
	}
	cursor.PreparedPending = false
	cursor.Projection = page.StateHash
	cursor.Generation = out.Status.Generation
	result := map[string]any{"notice": archiveNotice, "mode": "effective", "read_version": 1, "fragment_format": cursor.FragmentFormat,
		"selection": page.Selection, "status": out.Status, "state_hash": page.StateHash, "lineage_hash": page.LineageHash, "total": page.Total, "next_cursor": ""}
	if page.Inclusion != nil {
		result["inclusion"] = memoryInclusionSummary(page.Inclusion)
	}
	if len(page.Items) == 0 {
		if cursor.Offset != 0 || page.NextCursor != "" {
			return "", domain.ErrIntegrity
		}
		result["item"] = nil
		encoded, err := pageJSON(result)
		if len(encoded) > pageBytes {
			return "", domain.ErrMemoryProjectionLimit
		}
		return encoded, err
	}
	if out.Cursor == "" {
		return "", domain.ErrIntegrity
	}
	raw, err := json.Marshal(page.Items[0])
	if err != nil {
		return "", err
	}
	start := cursor.Offset
	if start >= len(raw) || (start > 0 && !utf8.RuneStart(raw[start])) {
		return "", domain.ErrValidation
	}
	for budget := pageBytes / 2; budget >= 4; budget /= 2 {
		end := fragmentEnd(raw, start, budget)
		next := cursor
		next.Offset = end
		next.EffectiveCursor = out.Cursor
		if end == len(raw) {
			next.Offset = 0
			next.EffectiveCursor = page.NextCursor
		}
		continuation := ""
		if end < len(raw) || page.NextCursor != "" {
			continuation, err = encodePreparedMemoryCursor(next)
			if err != nil {
				return "", err
			}
		}
		result["item_id"] = page.Items[0].ID
		result["byte_offset"] = start
		result["json_fragment"] = string(raw[start:end])
		result["item_complete"] = end == len(raw)
		result["next_cursor"] = continuation
		encoded, err := pageJSON(result)
		if err != nil {
			return "", err
		}
		if len(encoded) <= pageBytes {
			return encoded, nil
		}
	}
	return "", domain.ErrMemoryProjectionLimit
}

type preparedMemoryToolError struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Retryable   bool   `json:"retryable"`
	RetryCursor string `json:"retry_cursor,omitempty"`
	cause       error
	preparation *domain.MemoryReadPreparationRequest
}

func (failure *preparedMemoryToolError) Error() string { return failure.Message }

func (failure *preparedMemoryToolError) Unwrap() error { return failure.cause }

func preparedMemoryFailure(err error) error {
	if err == nil {
		return nil
	}
	var existing *preparedMemoryToolError
	errors.As(err, &existing)
	failure := &preparedMemoryToolError{Code: "unavailable", Message: "Prepared memory read unavailable", cause: err}
	for _, mapping := range []struct {
		cause     error
		code      string
		message   string
		retryable bool
	}{
		{domain.ErrUnauthorized, "unauthenticated", "Authentication required", false},
		{domain.ErrForbidden, "forbidden", "Repository access denied", false},
		{domain.ErrBreakGlassAuditUnavailable, "audit_unavailable", "Break-glass audit unavailable", false},
		{domain.ErrIntegrity, "integrity_violation", "Prepared memory integrity check failed", false},
		{domain.ErrMemoryReadNeedsAttention, "memory_read_needs_attention", "Memory read preparation requires attention", false},
		{domain.ErrEffectiveMemoryCursorStale, "memory_cursor_stale", "Memory cursor is stale; restart without cursor", false},
		{domain.ErrMemoryCaptureExpired, "memory_capture_expired", "Prepared memory capture expired; restart without cursor", false},
		{domain.ErrCatalogResetRequired, "catalog_reset_required", "Memory source reset; restart without cursor", false},
		{domain.ErrCatalogUnsupported, "prepared_memory_unavailable", "Prepared memory reads are not enabled or supported for this repository", false},
		{domain.ErrDocumentIdentityUpgradeRequired, "document_identity_upgrade_required", "Document identity upgrade required", false},
		{domain.ErrMemoryPreparationUnavailable, "memory_preparation_unavailable", "Memory preparation service unavailable; retry later", true},
		{domain.ErrConflict, "conflict", "Prepared memory read conflict", false},
		{context.Canceled, "canceled", "Memory read canceled", false},
		{domain.ErrValidation, "validation", "Invalid prepared memory arguments or cursor; read_version=1 requires memory_load mode=effective and a full code_commit", false},
		{domain.ErrNotFound, "not_found", "Memory selection not found or not authorized", false},
		{domain.ErrMemoryProjectionLimit, "memory_projection_limit", "Prepared memory exceeds the transport bound", false},
		{domain.ErrResourceOverloaded, "resource_overloaded", "Resource capacity unavailable; retry later", true},
		{context.DeadlineExceeded, "deadline_exceeded", "Memory read deadline exceeded", true},
		{domain.ErrMemoryReadNotReady, "memory_read_not_ready", "Memory read model is being prepared; retry with retry_cursor when present", true},
	} {
		if errors.Is(err, mapping.cause) {
			failure.Code, failure.Message, failure.Retryable = mapping.code, mapping.message, mapping.retryable
			break
		}
	}
	if failure.Code == "memory_read_not_ready" && !domain.IsOnlyMemoryReadPending(err) {
		return &preparedMemoryToolError{Code: "unavailable", Message: "Prepared memory read unavailable", cause: err}
	}
	if failure.Code == "memory_read_not_ready" && existing != nil && existing.Code == failure.Code {
		failure.RetryCursor = existing.RetryCursor
		failure.preparation = existing.preparation
	}
	return failure
}

func (server *Server) forwardMemoryPreparation(ctx context.Context, token string, failure *preparedMemoryToolError) *preparedMemoryToolError {
	if server.memoryPreparation == nil || failure.preparation == nil || failure.Code != "memory_read_not_ready" || !domain.IsOnlyMemoryReadPending(failure) {
		return failure
	}
	if err := failure.preparation.Validate(); err != nil {
		return preparedMemoryFailure(err).(*preparedMemoryToolError)
	}
	if err := server.memoryPreparation.RequestMemoryPreparation(ctx, token, *failure.preparation); err != nil {
		forwarded := preparedMemoryFailure(err).(*preparedMemoryToolError)
		if forwarded.Retryable {
			forwarded.RetryCursor = failure.RetryCursor
		}
		return forwarded
	}
	return failure
}
