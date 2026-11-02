package http

import (
	"fmt"
	"reflect"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (server *Server) SetContextBranchOperations(operations inbound.ContextBranchOperations) error {
	if operations == nil {
		return fmt.Errorf("%w: context branch operations are required", domain.ErrValidation)
	}
	switch value := reflect.ValueOf(operations); value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return fmt.Errorf("%w: context branch operations are required", domain.ErrValidation)
		}
	}
	server.contextBranches = operations
	return nil
}
