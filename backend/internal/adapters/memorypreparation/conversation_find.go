package memorypreparation

import (
	"context"
	"net/http"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (client *Client) RequestConversationFindPreparation(ctx context.Context, bearer string, input domain.ConversationFindPreparationRequest) error {
	return client.requestPreparation(ctx, bearer, input, preparationEndpoint{
		url: client.conversationEndpoint, maxBytes: domain.MaxConversationFindPreparationBytes,
		unavailable: domain.ErrConversationFindPreparationUnavailable, rejection: conversationFindPreparationRejection,
	})
}

func conversationFindPreparationRejection(status int, code string) error {
	for _, mapping := range []struct {
		status int
		code   string
		cause  error
	}{
		{http.StatusUnauthorized, "unauthenticated", domain.ErrUnauthorized},
		{http.StatusForbidden, "forbidden", domain.ErrForbidden},
		{http.StatusNotFound, "not_found", domain.ErrNotFound},
		{http.StatusUnprocessableEntity, "validation", domain.ErrValidation},
		{http.StatusConflict, "conflict", domain.ErrConflict},
		{http.StatusConflict, "catalog_reset_required", domain.ErrCatalogResetRequired},
		{http.StatusConflict, "unsupported_document_identity", domain.ErrUnsupportedDocumentIdentity},
		{http.StatusConflict, "document_identity_upgrade_required", domain.ErrDocumentIdentityUpgradeRequired},
		{http.StatusConflict, "doc_visible_generation_stale", domain.ErrDocVisibleGenerationStale},
		{http.StatusServiceUnavailable, "integrity_violation", domain.ErrIntegrity},
		{http.StatusServiceUnavailable, "audit_unavailable", domain.ErrBreakGlassAuditUnavailable},
		{http.StatusServiceUnavailable, "prepared_memory_unavailable", domain.ErrCatalogUnsupported},
		{http.StatusServiceUnavailable, "doc_find_fields_unavailable", domain.ErrDocFindFieldsUnavailable},
		{http.StatusServiceUnavailable, "doc_find_display_unavailable", domain.ErrDocFindDisplayUnavailable},
		{http.StatusServiceUnavailable, "doc_range_index_unavailable", domain.ErrDocRangeIndexUnavailable},
		{http.StatusServiceUnavailable, "doc_range_index_pending", domain.ErrDocRangeIndexPending},
		{http.StatusServiceUnavailable, "doc_find_fields_pending", domain.ErrDocFindFieldsPending},
		{http.StatusTooManyRequests, "resource_overloaded", domain.ErrResourceOverloaded},
		{http.StatusServiceUnavailable, "deadline_exceeded", context.DeadlineExceeded},
		{http.StatusServiceUnavailable, "canceled", context.Canceled},
	} {
		if status == mapping.status && code == mapping.code {
			return mapping.cause
		}
	}
	return domain.ErrConversationFindPreparationUnavailable
}
