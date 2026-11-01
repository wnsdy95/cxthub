package domain

type legacyNativeCheckpointStrings struct {
	Nil      bool                       `json:"nil"`
	Length   int                        `json:"length"`
	Capacity int                        `json:"capacity"`
	Items    legacyNativeCheckpointTree `json:"items"`
}

func (value legacyNativeCheckpointStrings) validate(items bool) error {
	if value.Length < 0 || value.Capacity < value.Length || value.Capacity > MaxFinalizedDocBytes || value.Nil && (value.Length != 0 || value.Capacity != 0) {
		return ErrIntegrity
	}
	if items && (!value.Items.valid() || value.Items.Count != uint64(value.Capacity) || (value.Capacity == 0) != (value.Items == (legacyNativeCheckpointTree{}))) {
		return ErrIntegrity
	}
	if !items && value.Items != (legacyNativeCheckpointTree{}) {
		return ErrIntegrity
	}
	return nil
}

type legacyNativeCheckpointStringValues struct {
	Values     []string
	Capacities []int
	Payload    docRangeHeaderHeapEstimate
}

type legacyNativeCheckpointStringListState struct {
	Version     uint8                         `json:"v"`
	SourceBytes int                           `json:"n"`
	Position    int                           `json:"position"`
	End         int                           `json:"end"`
	Maximum     int                           `json:"maximum"`
	Count       int                           `json:"count"`
	Copied      int                           `json:"copied"`
	StringStart int                           `json:"string_start"`
	Literal     int                           `json:"literal"`
	Seeded      int                           `json:"seeded"`
	Phase       uint8                         `json:"phase"`
	Escaped     bool                          `json:"escaped"`
	NullList    bool                          `json:"null"`
	Initialized bool                          `json:"initialized"`
	Prefix      bool                          `json:"prefix"`
	Decoded     legacyNativeCheckpointStrings `json:"decoded"`
	Previous    legacyNativeCheckpointStrings `json:"previous"`
	HasScalar   bool                          `json:"has_scalar"`
	Scalar      legacyNativeCheckpointScalar  `json:"scalar"`
}
