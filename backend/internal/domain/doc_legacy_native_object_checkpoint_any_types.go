package domain

type legacyNativeObjectCheckpointAnyField struct {
	Destination *legacyNativeCheckpointValue `json:"destination,omitempty"`
	Binding     uint8                        `json:"binding"`
	Active      bool                         `json:"active,omitempty"`
	Parser      legacyNativeCheckpointRef    `json:"parser"`
}

type legacyNativeObjectAnyRestored struct {
	Value      any
	Parser     *legacyNativeAny
	Scanner    legacyJSONValueScanner
	Heap       docRangeHeaderHeapEstimate
	Excess     int64
	Source     LegacyByteSource
	GraphBytes int64
}

type legacyNativeObjectFloatRestored struct {
	Number *legacyNativeNumberValue
	Source LegacyByteSource
}
