package main

import (
	"net/http"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/preparation"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"github.com/wnsdy95/cxthub/backend/internal/serverruntime"
)

func withConversationFindPreparation(runtime *serverruntime.Runtime, api http.Handler, resourceOrigin string) (http.Handler, error) {
	if strings.TrimSpace(resourceOrigin) == "" {
		return api, nil
	}
	if runtime == nil {
		return nil, domain.ErrCatalogUnsupported
	}
	storage, ok := runtime.Store.(outbound.RuntimeStore)
	if !ok {
		return nil, domain.ErrCatalogUnsupported
	}
	handler, err := preparation.NewConversationFindPreparationHandler(runtime.Context, runtime.Identity, runtime.Context, storage, runtime.PublicURL, resourceOrigin)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle(domain.ConversationFindPreparationPath, handler)
	mux.Handle("/", api)
	return mux, nil
}
