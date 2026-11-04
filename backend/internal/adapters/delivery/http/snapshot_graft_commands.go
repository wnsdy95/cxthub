package http

import (
	"fmt"
	"reflect"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (server *Server) SetSnapshotGraftCommands(commands app.SnapshotGraftWriter) error {
	if commands == nil {
		return fmt.Errorf("%w: snapshot graft commands are required", domain.ErrValidation)
	}
	switch value := reflect.ValueOf(commands); value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return fmt.Errorf("%w: snapshot graft commands are required", domain.ErrValidation)
		}
	}
	server.snapshotGrafts = commands
	return nil
}
