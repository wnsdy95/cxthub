package mcp

import (
	"fmt"
	"reflect"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (server *Server) SetDocumentEventQueries(queries inbound.DocumentEventQueries, profile inbound.DocumentQueryProfile) error {
	if profile != inbound.DocumentQueryCompatibility && profile != inbound.DocumentQueryVerifiedRoots {
		return fmt.Errorf("%w: explicit document query profile is required", domain.ErrValidation)
	}
	if queries == nil {
		return fmt.Errorf("%w: document event queries are required", domain.ErrValidation)
	}
	reflected := reflect.ValueOf(queries)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if reflected.IsNil() {
			return fmt.Errorf("%w: typed-nil document event queries", domain.ErrValidation)
		}
	}
	server.documentEvents = queries
	server.documentQueryProfile = profile
	return nil
}
