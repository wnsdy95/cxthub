package http

import (
	"fmt"
	"reflect"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (server *Server) SetRepositoryRefCommands(commands inbound.RepositoryRefCommands) error {
	if commands == nil {
		return fmt.Errorf("%w: repository ref commands are required", domain.ErrValidation)
	}
	switch value := reflect.ValueOf(commands); value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return fmt.Errorf("%w: repository ref commands are required", domain.ErrValidation)
		}
	}
	server.repositoryRefs = commands
	return nil
}
