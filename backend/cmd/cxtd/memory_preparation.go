package main

import (
	"net/http"
	"strings"

	delivery "github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/preparation"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"github.com/wnsdy95/cxthub/backend/internal/serverruntime"
)

func withMemoryPreparation(runtime *serverruntime.Runtime, api http.Handler, resourceOrigin string, repos []domain.ContentHash) (http.Handler, error) {
	if len(repos) == 0 || strings.TrimSpace(resourceOrigin) == "" {
		return api, nil
	}
	storage, ok := runtime.Store.(outbound.RuntimeStore)
	if !ok {
		return nil, domain.ErrCatalogUnsupported
	}
	handler, err := delivery.NewMemoryPreparationHandler(runtime.Context, runtime.Identity, runtime.Context, storage, runtime.PublicURL, resourceOrigin)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle(domain.MemoryReadPreparationPath, handler)
	mux.Handle("/", api)
	return mux, nil
}
