package mcp

import (
	"errors"
	"reflect"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

var errMemorySnapshotsUnavailable = errors.New("memory snapshot queries unavailable")

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

func memorySnapshotOnly(args toolArgs) bool {
	switch args.Mode {
	case "stored":
		return true
	case "", "project":
		return args.Mode == "" && args.MemoryHash != "" || domain.ValidateContentHash(domain.ContentHash(strings.TrimSpace(args.Ref))) == nil
	default:
		return false
	}
}
