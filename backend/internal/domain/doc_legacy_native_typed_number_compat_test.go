package domain

import (
	"encoding/json"
)

type legacyNativeTypedNumberCompatState struct {
	provider         bool
	direct           ProviderMetadata
	event            wire
	originalMetadata *ProviderMetadata
	originalTime     *json.Number
}

func (state *legacyNativeTypedNumberCompatState) destination() any {
	if state.provider {
		return &state.event
	}
	return &state.direct
}

func (state *legacyNativeTypedNumberCompatState) metadata() *ProviderMetadata {
	if state.provider {
		return state.event.ProviderMetadata
	}
	return &state.direct
}
