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

const maxConversationFindTransportCursorBytes = 8 << 10

func (server *Server) SetConversationFind(query inbound.ConversationFindQuery, requester outbound.ConversationFindPreparationRequester) {
	if nilConversationFindDependency(query) {
		query = nil
	}
	if nilConversationFindDependency(requester) {
		requester = nil
	}
	server.conversationFind, server.conversationFindPreparation = query, requester
}

func nilConversationFindDependency(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func encodeConversationFindCursor(cursor pageCursor) (string, error) {
	cursor.Version = 6
	encoded := encodeCursor(cursor)
	if len(encoded) > maxConversationFindTransportCursorBytes {
		return "", domain.ErrDocumentResourceLimit
	}
	return encoded, nil
}

func (server *Server) conversationFindPage(ctx context.Context, repo domain.Repo, args toolArgs) (string, error) {
	result, err := server.readConversationFindPage(ctx, repo, args)
	if err != nil {
		return "", conversationFindToolFailure(err)
	}
	return result, nil
}

func (server *Server) readConversationFindPage(ctx context.Context, repo domain.Repo, args toolArgs) (string, error) {
	if server.conversationFind == nil {
		return "", domain.ErrDocFindDisplayUnavailable
	}
	if domain.ValidateDocFindQuery(args.Query) != nil {
		return "", domain.ErrValidation
	}
	cursor, err := cursorFor(string(repo.ID), "context_find", args)
	if err != nil {
		return "", domain.ErrValidation
	}
	if args.Cursor != "" && (cursor.Version != 6 || cursor.Snapshot == "" || cursor.Index != 0 || cursor.Memory != "" || cursor.Generation != "" || cursor.Projection != "" || cursor.ContextBranch != "" || cursor.EffectiveCursor != "" || cursor.FragmentFormat != "" || cursor.PreparedPending || cursor.After != "" || cursor.Top != "" || cursor.Offset > 0 && (cursor.DocumentCursor == "" || cursor.Scan == "")) {
		return "", domain.ErrValidation
	}
	if cursor.Snapshot == "" {
		snapshot, err := server.resolveRefWithShortHash(ctx, repo, args.Ref, false)
		if err != nil {
			return "", err
		}
		cursor.Snapshot = snapshot.ID
	}
	find := domain.ConversationFindRequest{Query: args.Query, Cursor: cursor.DocumentCursor}
	if find.Validate() != nil {
		return "", domain.ErrValidation
	}
	page, err := server.conversationFind.QueryConversationFind(outbound.WithDocReadOnly(ctx), repo.ID, cursor.Snapshot, find)
	if err != nil {
		var pending *domain.ConversationFindNotReady
		if !errors.As(err, &pending) || !domain.IsOnlyConversationFindPending(err, true) {
			return "", err
		}
		if cursor.Offset != 0 || cursor.Scan != "" {
			return "", domain.ErrDocVisibleGenerationStale
		}
		find.Cursor = pending.Cursor
		if find.Validate() != nil {
			return "", domain.ErrIntegrity
		}
		cursor.DocumentCursor = find.Cursor
		retry, err := encodeConversationFindCursor(cursor)
		if err != nil {
			return "", err
		}
		failure := conversationFindToolFailure(pending).(*conversationFindToolError)
		failure.RetryCursor = retry
		failure.preparation = &domain.ConversationFindPreparationRequest{Version: 1, RepoID: repo.ID, Hash: cursor.Snapshot, Find: find, DocumentIdentities: inbound.DocumentIdentities(ctx)}
		return "", failure
	}
	if page.Page.Hash != cursor.Snapshot || page.Page.Projection != domain.DocFindPreparedProjection && page.Page.Projection != domain.DocLegacyFindPreparedProjection || page.Page.Prepared == nil || page.ReplayCursor == "" || (domain.ConversationFindRequest{Query: args.Query, Cursor: page.ReplayCursor}).Validate() != nil {
		return "", domain.ErrIntegrity
	}
	if cursor.DocumentCursor != "" && cursor.DocumentCursor != page.ReplayCursor {
		return "", domain.ErrIntegrity
	}
	if page.Page.NextCursor != nil && (*page.Page.NextCursor == "" || (domain.ConversationFindRequest{Query: args.Query, Cursor: *page.Page.NextCursor}).Validate() != nil) {
		return "", domain.ErrIntegrity
	}
	raw, err := json.Marshal(page.Page)
	if err != nil {
		return "", err
	}
	hash := domain.HashContent(raw)
	if cursor.Scan != "" && cursor.Scan != hash {
		return "", domain.ErrDocVisibleGenerationStale
	}
	if cursor.Offset >= len(raw) || cursor.Offset > 0 && !utf8.RuneStart(raw[cursor.Offset]) {
		return "", domain.ErrValidation
	}
	end := fragmentEnd(raw, cursor.Offset, pageBytes/3)
	for end > cursor.Offset {
		next := cursor
		next.DocumentCursor, next.Offset, next.Scan = page.ReplayCursor, end, hash
		continuation := ""
		if end < len(raw) {
			continuation, err = encodeConversationFindCursor(next)
		} else if page.Page.NextCursor != nil {
			next.DocumentCursor, next.Offset, next.Scan = *page.Page.NextCursor, 0, ""
			continuation, err = encodeConversationFindCursor(next)
		}
		if err != nil {
			return "", err
		}
		encoded, err := pageJSON(map[string]any{"notice": archiveNotice, "snapshot_id": cursor.Snapshot, "projection": page.Page.Projection, "byte_offset": cursor.Offset, "page_hash": hash, "json_fragment": string(raw[cursor.Offset:end]), "page_complete": end == len(raw), "next_cursor": continuation})
		if err != nil {
			return "", err
		}
		if len(encoded) <= pageBytes {
			return encoded, nil
		}
		end = fragmentEnd(raw, cursor.Offset, (end-cursor.Offset)/2)
	}
	return "", domain.ErrDocumentResourceLimit
}

type conversationFindToolError struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Retryable   bool   `json:"retryable"`
	RetryCursor string `json:"retry_cursor,omitempty"`
	cause       error
	preparation *domain.ConversationFindPreparationRequest
}

func (failure *conversationFindToolError) Error() string { return failure.Message }
func (failure *conversationFindToolError) Unwrap() error { return failure.cause }

func conversationFindToolFailure(err error) error {
	if existing, ok := err.(*conversationFindToolError); ok {
		return existing
	}
	failure := &conversationFindToolError{Code: "unavailable", Message: "Conversation search unavailable", cause: err}
	var pending *domain.ConversationFindNotReady
	if errors.As(err, &pending) && domain.IsOnlyConversationFindPending(err, true) {
		failure.Code, failure.Message, failure.Retryable = "conversation_find_not_ready", "Conversation search requires preparation; retry with retry_cursor", true
		return failure
	}
	for _, mapping := range []struct {
		cause         error
		code, message string
		retryable     bool
	}{
		{domain.ErrUnauthorized, "unauthenticated", "Authentication required", false},
		{domain.ErrForbidden, "forbidden", "Repository access denied", false},
		{domain.ErrBreakGlassAuditUnavailable, "audit_unavailable", "Access audit unavailable", false},
		{domain.ErrIntegrity, "integrity_violation", "Conversation integrity check failed", false},
		{domain.ErrValidation, "validation", "Invalid conversation search arguments or cursor", false},
		{domain.ErrDocVisibleGenerationStale, "doc_visible_generation_stale", "Conversation generation changed; restart without cursor", false},
		{domain.ErrDocumentIdentityUpgradeRequired, "document_identity_upgrade_required", "Document identity upgrade required", false},
		{domain.ErrDocFindDisplayUnavailable, "doc_find_display_unavailable", "Selected display formatter is unavailable", false},
		{domain.ErrDocVisibleProjectionUnsupported, "doc_visible_projection_unsupported", "Conversation projection is unsupported", false},
		{domain.ErrNotFound, "not_found", "Conversation not found or not authorized", false},
		{domain.ErrConflict, "conflict", "Conversation selection changed", false},
		{domain.ErrDocumentResourceLimit, "document_resource_limit", "Conversation response exceeds transport bounds", false},
		{domain.ErrConversationFindPreparationUnavailable, "conversation_find_preparation_unavailable", "Conversation preparation service unavailable", true},
		{domain.ErrResourceOverloaded, "resource_overloaded", "Resource capacity unavailable; retry later", true},
		{context.DeadlineExceeded, "deadline_exceeded", "Conversation search deadline exceeded", true},
		{context.Canceled, "canceled", "Conversation search canceled", false},
	} {
		if errors.Is(err, mapping.cause) {
			failure.Code, failure.Message, failure.Retryable = mapping.code, mapping.message, mapping.retryable
			break
		}
	}
	return failure
}

func (server *Server) forwardConversationFindPreparation(ctx context.Context, token string, failure *conversationFindToolError) *conversationFindToolError {
	if failure.preparation == nil || failure.Code != "conversation_find_not_ready" || !domain.IsOnlyConversationFindPending(failure, true) {
		return failure
	}
	if failure.preparation.Validate() != nil {
		return conversationFindToolFailure(domain.ErrIntegrity).(*conversationFindToolError)
	}
	err := error(domain.ErrConversationFindPreparationUnavailable)
	if server.conversationFindPreparation != nil {
		err = server.conversationFindPreparation.RequestConversationFindPreparation(ctx, token, *failure.preparation)
	}
	if err == nil {
		return failure
	}
	forwarded := conversationFindToolFailure(err).(*conversationFindToolError)
	if forwarded.Retryable {
		forwarded.RetryCursor = failure.RetryCursor
	}
	return forwarded
}
