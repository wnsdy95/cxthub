package domain

import "reflect"

type legacyNativeCheckpointNestedValue struct {
	Version       uint8                         `json:"v"`
	Model         uint8                         `json:"model"`
	Schema        ContentHash                   `json:"schema"`
	NumberPresent bool                          `json:"number_present"`
	Values        legacyNativeCheckpointStrings `json:"values"`
}

type legacyNativeObjectCheckpointNestedField struct {
	Model  uint8                              `json:"model"`
	Value  *legacyNativeCheckpointNestedValue `json:"value,omitempty"`
	Parser legacyNativeCheckpointRef          `json:"parser"`
}

type legacyNativeObjectNestedRestored struct {
	Value   reflect.Value
	Parser  *legacyNativeObject
	Scanner legacyJSONValueScanner
	Heap    docRangeHeaderHeapEstimate
	Excess  int64
	Source  LegacyByteSource
}
