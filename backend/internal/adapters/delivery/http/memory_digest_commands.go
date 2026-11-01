package http

import (
	"fmt"
	"reflect"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (server *Server) SetMemoryDigestCommands(commands app.MemoryDigestWriter) error {
	if commands == nil {
		return fmt.Errorf("%w: memory digest commands are required", domain.ErrValidation)
	}
	switch value := reflect.ValueOf(commands); value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return fmt.Errorf("%w: memory digest commands are required", domain.ErrValidation)
		}
	}
	server.memoryDigests = commands
	return nil
}
