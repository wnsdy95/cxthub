package domain

type legacyCanonicalReplacementState struct {
	Kind    string      `json:"kind"`
	Version uint8       `json:"version"`
	Binding ContentHash `json:"binding"`
}

type legacyCanonicalReplacementCheckpoint struct {
	source     *legacyCanonicalReplacement
	state      legacyCanonicalReplacementState
	budget     int64
	frameIndex int
	position   int
	phase      uint8
	ready      bool
	closed     bool
	failure    error
}
