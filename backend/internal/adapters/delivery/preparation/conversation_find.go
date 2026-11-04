package preparation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func NewConversationFindPreparationHandler(contextBackend ContextBackend, identity IdentityBackend, preparer inbound.ConversationFindDemand, runtime outbound.RuntimeStore, publicURL, resourceOrigin string) (http.Handler, error) {
	if missingBackend(preparer) {
		return nil, fmt.Errorf("conversation find preparation requires a preparation backend")
	}
	endpoint := preparationEndpoint{
		path: domain.ConversationFindPreparationPath, maxBytes: domain.MaxConversationFindPreparationBytes,
		rateKey: "mcp:conversation-find-preparation", unavailable: domain.ErrConversationFindPreparationUnavailable,
		decode: func(decoder *json.Decoder) (preparationRequest, error) {
			var body domain.ConversationFindPreparationRequest
			if err := decoder.Decode(&body); err != nil {
				return preparationRequest{}, err
			}
			if err := body.Validate(); err != nil {
				return preparationRequest{}, err
			}
			return preparationRequest{
				repoID: body.RepoID, documentIdentities: body.DocumentIdentities,
				prepare: func(ctx context.Context) error {
					return preparer.RequestConversationFind(ctx, body.RepoID, body.Hash, body.Find)
				},
			}, nil
		},
	}
	return newPreparationHandler(contextBackend, identity, runtime, publicURL, resourceOrigin, endpoint)
}
