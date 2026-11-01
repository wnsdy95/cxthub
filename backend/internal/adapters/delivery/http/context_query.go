package http

import (
	"fmt"
	"reflect"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (server *Server) SetContextQuery(query inbound.ContextQuery, profile inbound.ContextQueryProfile) error {
	if profile != inbound.ContextQueryCompatibility && profile != inbound.ContextQueryPinnedSnapshots {
		return fmt.Errorf("%w: explicit context query profile is required", domain.ErrValidation)
	}
	if query == nil {
		return fmt.Errorf("%w: context query is required", domain.ErrValidation)
	}
	switch value := reflect.ValueOf(query); value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return fmt.Errorf("%w: context query is required", domain.ErrValidation)
		}
	}
	server.contextQueries = query
	server.contextQueryProfile = profile
	return nil
}
