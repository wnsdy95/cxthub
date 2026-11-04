package http

import (
	"context"
	"net/http"
	"reflect"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func validateMemoryReader(reader inbound.MemoryReadScope) error {
	if reader == nil {
		return domain.ErrValidation
	}
	switch value := reflect.ValueOf(reader); value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return domain.ErrValidation
		}
	}
	return nil
}

func (server *Server) memoryReadWithScope(writer http.ResponseWriter, request *http.Request, scope inbound.MemoryReadScope, read func(context.Context) (any, error)) {
	request, cancel, ok := server.resourceRequestDeadline(writer, request)
	if !ok {
		return
	}
	defer cancel()
	repo := server.repoID(request)
	ctx := inbound.WithDocumentReadAuthorizer(request.Context(), server.memoryReadAuthorizer(request, repo))
	err := scope.WithMemoryReadResources(ctx, repo, func(bound context.Context) error {
		value, err := read(bound)
		server.respond(writer, value, err)
		return nil
	})
	if err != nil {
		server.respond(writer, nil, err)
	}
}
