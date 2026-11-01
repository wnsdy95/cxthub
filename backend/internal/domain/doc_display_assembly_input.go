package domain

import (
	"context"
	"encoding/json"
)

type DocDisplayAssemblyInput struct {
	source  DocDisplayProjectionSource
	initial json.RawMessage
	cursor  func(json.RawMessage) (int, error)
	read    func(context.Context, json.RawMessage, int, int, func(context.Context, ContentHash) ([]byte, error)) (DocDisplayPage, json.RawMessage, error)
}

func (input DocDisplayAssemblyInput) Source() DocDisplayProjectionSource { return input.source }

func (input DocDisplayAssemblyInput) validate() error {
	if input.source.Validate() != nil || len(input.initial) == 0 || len(input.initial) > MaxDocDisplaySourceBytes || input.cursor == nil || input.read == nil {
		return ErrIntegrity
	}
	position, err := input.cursor(input.initial)
	if err != nil || position != 0 {
		return ErrIntegrity
	}
	return nil
}
