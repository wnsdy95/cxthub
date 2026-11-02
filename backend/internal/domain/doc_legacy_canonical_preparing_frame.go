package domain

type legacyCanonicalPreparingFrameState struct {
	Value        legacyNativeCheckpointRef `json:"value"`
	Continuation legacyNativeCheckpointRef `json:"continuation"`
	Heap         int64                     `json:"heap"`
	Depth        int                       `json:"depth"`
	Position     int                       `json:"position"`
	Length       int                       `json:"length"`
	Count        int                       `json:"count"`
	MapPosition  int                       `json:"map_position"`
	KeyBytes     int                       `json:"key_bytes"`
	Kind         byte                      `json:"kind"`
	Phase        uint8                     `json:"phase"`
	Prepare      bool                      `json:"prepare"`
}

func (state legacyCanonicalPreparingFrameState) validate() error {
	if !state.Value.valid() || state.Heap < 0 || state.Depth < 0 || state.Depth > 10000 ||
		state.Position < 0 || state.Length < 0 || state.Count < 0 || state.Position > state.Count ||
		state.MapPosition < 0 || state.KeyBytes < 0 || state.KeyBytes > 1025 {
		return ErrIntegrity
	}
	if state.Kind == 'o' && state.Phase == 0 {
		if !state.Continuation.valid() {
			return ErrIntegrity
		}
	} else if state.Continuation != (legacyNativeCheckpointRef{}) {
		return ErrIntegrity
	}
	switch state.Kind {
	case 0:
		if state.Heap != 0 || state.Phase != 0 || state.Position != 0 || state.Length != 0 || state.Count != 0 ||
			state.MapPosition != 0 || state.KeyBytes != 0 {
			return ErrIntegrity
		}
	case 'o':
		if state.Heap == 0 || state.Depth >= 10000 || state.Phase > 2 || state.Length != 0 ||
			!state.Prepare && (state.Phase != 0 || state.MapPosition != 0 || state.KeyBytes != 0) ||
			state.Phase == 1 && state.Position != 0 || state.Count == 0 && state.KeyBytes != 0 ||
			state.Phase == 0 && state.Position == 0 && state.KeyBytes != 0 {
			return ErrIntegrity
		}
	case 'a', 'm', 'c', 'd':
		if state.Heap == 0 || state.Phase != 0 || state.Length > state.Count || state.MapPosition != 0 || state.KeyBytes != 0 {
			return ErrIntegrity
		}
	case 'r':
		if state.Heap == 0 || state.Depth >= 10000 || !state.Prepare || state.Phase > 2 || state.Length != 0 ||
			state.KeyBytes != 0 || state.Phase == 1 && state.Position != 0 {
			return ErrIntegrity
		}
	default:
		return ErrIntegrity
	}
	return nil
}

func (state legacyCanonicalPreparingFrameState) validateValue(value any) error {
	switch state.Kind {
	case 0:
		return nil
	case 'o':
		object, valid := value.(map[string]any)
		if !valid || object == nil || len(object) != state.Count {
			return ErrIntegrity
		}
	case 'a':
		values, valid := value.([]any)
		if !valid || len(values) != state.Length || cap(values) != state.Count || values != nil && state.Depth >= 10000 {
			return ErrIntegrity
		}
	case 'm':
		values, valid := value.([]map[string]any)
		if !valid || len(values) != state.Length || cap(values) != state.Count || values != nil && state.Depth >= 10000 {
			return ErrIntegrity
		}
	case 'c':
		values, valid := value.([]ContentBlock)
		if !valid || len(values) != state.Length || cap(values) != state.Count || values != nil && state.Depth >= 10000 {
			return ErrIntegrity
		}
	case 'd':
		values, valid := value.(*legacyNativeBlockValues)
		if !valid || values == nil || state.Depth >= 10000 || !values.records.valid() ||
			values.count != state.Length || values.records.length() != state.Count {
			return ErrIntegrity
		}
	case 'r':
		replacement, valid := value.(*legacyCanonicalReplacement)
		if !valid || replacement.length() < 0 || replacement.length() != state.Count {
			return ErrIntegrity
		}
		if replacement.native != nil && (!replacement.native.records.valid() || replacement.native.count > replacement.native.records.length()) {
			return ErrIntegrity
		}
	default:
		return ErrIntegrity
	}
	return nil
}

func (state legacyCanonicalPreparingFrameState) validateContinuation(continuation *legacyCanonicalMapContinuation) error {
	if state.Kind != 'o' || state.Phase != 0 {
		if continuation != nil {
			return ErrIntegrity
		}
		return nil
	}
	if continuation == nil || !continuation.fields.valid() || continuation.position < 0 ||
		continuation.position > continuation.fields.length() ||
		continuation.fields.length()-continuation.position != state.Count-state.Position {
		return ErrIntegrity
	}
	return nil
}

func captureLegacyCanonicalPreparingFrame(frame *legacyCanonicalPreparationFrame, value, continuation legacyNativeCheckpointRef) (legacyCanonicalPreparingFrameState, error) {
	if frame == nil {
		return legacyCanonicalPreparingFrameState{}, ErrIntegrity
	}
	if frame.heap.err != nil {
		return legacyCanonicalPreparingFrameState{}, frame.heap.err
	}
	state := legacyCanonicalPreparingFrameState{Value: value, Continuation: continuation, Heap: frame.heap.bytes,
		Depth: frame.depth, Position: frame.position, Length: frame.length, Count: frame.count,
		MapPosition: frame.mapPosition, KeyBytes: frame.keyBytes, Kind: frame.kind, Phase: frame.phase, Prepare: frame.prepare}
	if err := state.validate(); err != nil {
		return legacyCanonicalPreparingFrameState{}, err
	}
	if err := state.validateValue(frame.value); err != nil {
		return legacyCanonicalPreparingFrameState{}, err
	}
	if frame.kind == 'o' && frame.phase == 0 {
		if (frame.iterator == nil) == (frame.mapResume == nil) {
			return legacyCanonicalPreparingFrameState{}, ErrIntegrity
		}
	} else if frame.iterator != nil || frame.mapResume != nil {
		return legacyCanonicalPreparingFrameState{}, ErrIntegrity
	}
	if frame.mapResume != nil {
		if err := state.validateContinuation(frame.mapResume); err != nil {
			return legacyCanonicalPreparingFrameState{}, err
		}
	}
	return state, nil
}

func restoreLegacyCanonicalPreparingFrame(state legacyCanonicalPreparingFrameState, value any, continuation *legacyCanonicalMapContinuation) (legacyCanonicalPreparationFrame, error) {
	if err := state.validate(); err != nil {
		return legacyCanonicalPreparationFrame{}, err
	}
	if err := state.validateValue(value); err != nil {
		return legacyCanonicalPreparationFrame{}, err
	}
	if err := state.validateContinuation(continuation); err != nil {
		return legacyCanonicalPreparationFrame{}, err
	}
	return legacyCanonicalPreparationFrame{value: value, heap: docRangeHeaderHeapEstimate{bytes: state.Heap}, mapResume: continuation,
		depth: state.Depth, position: state.Position, length: state.Length, count: state.Count,
		mapPosition: state.MapPosition, keyBytes: state.KeyBytes, kind: state.Kind, phase: state.Phase, prepare: state.Prepare}, nil
}
