package domain

type legacyEnvelopeEncodingCheckpointControl struct {
	Phase      uint8                                  `json:"phase"`
	Field      int                                    `json:"field"`
	Model      int                                    `json:"model"`
	HasNative  bool                                   `json:"has_native"`
	Scalar     *legacyEnvelopeScalarCheckpointControl `json:"scalar,omitempty"`
	HasPending bool                                   `json:"has_pending"`
	Pending    legacyNativeCheckpointRef              `json:"pending"`
}

type legacyNormalizationHeaderCheckpointControl struct {
	Kind        string                                   `json:"kind"`
	Version     uint8                                    `json:"version"`
	Binding     ContentHash                              `json:"binding"`
	Schema      ContentHash                              `json:"schema"`
	SourceBytes int                                      `json:"source_bytes"`
	Length      int                                      `json:"length"`
	Position    int                                      `json:"position"`
	Copying     bool                                     `json:"copying"`
	Encoding    *legacyEnvelopeEncodingCheckpointControl `json:"encoding,omitempty"`
	Envelope    legacyNativeCheckpointRef                `json:"envelope"`
	Chunks      legacyNativeCheckpointTree               `json:"chunks"`
	Header      legacyNativeCheckpointText               `json:"header"`
	Output      legacyNativeCheckpointText               `json:"output"`
}

type legacyNormalizationHeaderCheckpointChunk struct {
	Capacity int                       `json:"capacity"`
	Body     legacyNativeCheckpointRef `json:"body"`
}

func (state legacyNormalizationHeaderCheckpointControl) validate(binding ContentHash, sourceBytes int, ready bool) error {
	schema, err := legacyNativeCheckpointModelSchema(&legacyEnvelopeNativeFields)
	if err != nil || state.Schema != schema {
		return ErrIntegrity
	}
	if state.Kind != "legacy-normalization-header" || state.Version != 1 || state.Binding != binding || ValidateContentHash(binding) != nil ||
		state.SourceBytes != sourceBytes || sourceBytes <= 0 || sourceBytes > MaxFinalizedDocBytes ||
		state.Length < 0 || uint64(state.Length) > legacyNativeValueCheckpointPayloadLimit() || state.Position < 0 || state.Position > state.Length {
		return ErrIntegrity
	}
	if state.Encoding != nil {
		encoding := state.Encoding
		if state.Position != 0 || state.Copying || state.Header != (legacyNativeCheckpointText{}) || state.Output != (legacyNativeCheckpointText{}) ||
			encoding.Phase > legacyEnvelopeEncodingModelValue || encoding.Field < 0 || encoding.Field > len(legacyEnvelopeNativeFields.fields) ||
			encoding.Model < 0 || encoding.Model > sourceBytes || encoding.Scalar != nil && encoding.Phase != legacyEnvelopeEncodingFieldNext && encoding.Phase != legacyEnvelopeEncodingModelNext ||
			!encoding.HasNative && state.Envelope != (legacyNativeCheckpointRef{}) || ready && encoding.HasNative && !state.Envelope.valid() ||
			!encoding.HasPending && encoding.Pending != (legacyNativeCheckpointRef{}) || ready && encoding.HasPending && (!encoding.Pending.valid() || encoding.Pending.Bytes > LegacyCanonicalEncodingBytes) {
			return ErrIntegrity
		}
	} else if state.Length == 0 || state.Header.Bytes != state.Length || state.Envelope != (legacyNativeCheckpointRef{}) ||
		state.Copying && state.Output.Bytes != state.Length || !state.Copying && state.Output != (legacyNativeCheckpointText{}) {
		return ErrIntegrity
	}
	if state.Copying && state.Chunks != (legacyNativeCheckpointTree{}) {
		return ErrIntegrity
	}
	if ready {
		if !state.Chunks.valid() || state.Chunks.Count > uint64(state.Length) {
			return ErrIntegrity
		}
		for _, text := range []legacyNativeCheckpointText{state.Header, state.Output} {
			if text.Bytes < 0 || text.Tree.Count != uint64(text.Bytes) || !text.Tree.valid() {
				return ErrIntegrity
			}
		}
	}
	return nil
}

func (chunk legacyNormalizationHeaderCheckpointChunk) validate() error {
	if chunk.Capacity < 256 || chunk.Capacity > LegacyCanonicalEncodingBytes || chunk.Capacity&(chunk.Capacity-1) != 0 ||
		!chunk.Body.valid() || chunk.Body.Bytes > chunk.Capacity {
		return ErrIntegrity
	}
	return nil
}
