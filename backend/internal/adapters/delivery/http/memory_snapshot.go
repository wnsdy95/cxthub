package http

import (
	"reflect"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (server *Server) SetMemorySnapshotQueries(reader inbound.MemorySnapshotQueries) error {
	if reader == nil {
		return domain.ErrValidation
	}
	switch value := reflect.ValueOf(reader); value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return domain.ErrValidation
		}
	}
	server.memorySnapshots = reader
	return nil
}
