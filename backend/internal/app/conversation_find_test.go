package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestConversationFindPendingIsOnlyPreparation(test *testing.T) {
	for _, scenario := range []struct {
		failure           error
		readOnly, pending bool
	}{
		{domain.ErrDocDisplayPending, false, true},
		{domain.ErrDocFindFieldsPending, false, true},
		{domain.ErrDocRangeIndexPending, false, true},
		{domain.ErrDocFindDisplayUnavailable, true, false},
		{errors.Join(domain.ErrDocFindDisplayUnavailable), true, false},
		{errors.Join(domain.ErrDocFindDisplayUnavailable, domain.ErrConversationFindPreparationRequired), true, true},
		{errors.Join(domain.ErrDocFindDisplayUnavailable, domain.ErrConversationFindPreparationRequired, domain.ErrIntegrity), true, false},
		{errors.Join(domain.ErrDocDisplayPending, domain.ErrForbidden), true, false},
		{domain.ErrConversationFindPreparationRequired, false, false},
		{domain.ErrDocRangeIndexUnavailable, true, true},
		{domain.ErrDocRangeIndexUnavailable, false, false},
		{context.Canceled, true, false},
		{nil, true, false},
	} {
		if got := domain.IsOnlyConversationFindPending(scenario.failure, scenario.readOnly); got != scenario.pending {
			test.Fatal("non-preparation failure accepted or pending lost", scenario.failure, scenario.readOnly, got)
		}
	}
	service := NewService(nil, nil, nil, nil, nil)
	if err := service.RequestConversationFind(outbound.WithDocReadOnly(context.Background()), "", "", domain.ConversationFindRequest{}); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("read-only demand entered application writes", err)
	}
}
