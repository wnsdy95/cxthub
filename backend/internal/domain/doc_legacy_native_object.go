package domain

import (
	"context"
	"encoding"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
	"unsafe"
)

type legacyNativeObjectField struct {
	name            string
	prefix          string
	index           int
	scalarString    bool
	interfaceString bool
	stringList      bool
	blockList       bool
	eventList       bool
	fixedScalar     bool
	nestedObject    bool
	genericMap      bool
	typedNumber     bool
}

type legacyNativeObjectFields struct {
	model    reflect.Type
	fields   []legacyNativeObjectField
	keyBytes int
	rawBytes int
	err      error
}

func newLegacyNativeObjectFields(model reflect.Type) legacyNativeObjectFields {
	metadata := legacyNativeObjectFields{model: model}
	if model == nil || model.Kind() != reflect.Struct {
		metadata.err = ErrIntegrity
		return metadata
	}
	jsonUnmarshaler := reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
	textUnmarshaler := reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
	for index := 0; index < model.NumField(); index++ {
		field := model.Field(index)
		if field.PkgPath != "" {
			continue
		}
		if field.Anonymous {
			metadata.err = ErrIntegrity
			return metadata
		}
		tag := strings.Split(field.Tag.Get("json"), ",")
		name := tag[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		raw, err := json.Marshal(name)
		if err != nil {
			metadata.err = err
			return metadata
		}
		prefix := "{" + string(raw) + ":"
		pointer := reflect.PointerTo(field.Type)
		quoted := false
		for _, option := range tag[1:] {
			quoted = quoted || option == "string"
		}
		scalarString := field.Type.Kind() == reflect.String &&
			!quoted && !pointer.Implements(jsonUnmarshaler) && !pointer.Implements(textUnmarshaler)
		interfaceString := field.Type == reflect.TypeFor[any]() &&
			!quoted && !pointer.Implements(jsonUnmarshaler) && !pointer.Implements(textUnmarshaler)
		stringList := field.Type == reflect.TypeOf([]string(nil)) &&
			!quoted && !pointer.Implements(jsonUnmarshaler) && !pointer.Implements(textUnmarshaler)
		blockList := field.Type == reflect.TypeFor[[]ContentBlock]() &&
			!quoted && !pointer.Implements(jsonUnmarshaler) && !pointer.Implements(textUnmarshaler)
		eventList := model == reflect.TypeFor[wire]() && field.Type == reflect.TypeFor[[]CIREvent]() && !quoted
		nestedObject := (field.Type == reflect.TypeFor[*LockedBlob]() || field.Type == reflect.TypeFor[*ProviderMetadata]()) &&
			!quoted && !pointer.Implements(jsonUnmarshaler) && !pointer.Implements(textUnmarshaler)
		genericMap := field.Type == reflect.TypeFor[map[string]any]() &&
			!quoted && !pointer.Implements(jsonUnmarshaler) && !pointer.Implements(textUnmarshaler)
		typedNumber := field.Type == reflect.TypeFor[*json.Number]() &&
			!quoted && !pointer.Implements(jsonUnmarshaler) && !pointer.Implements(textUnmarshaler)
		kind := field.Type.Kind()
		fixedScalar := (kind == reflect.Bool || kind >= reflect.Int && kind <= reflect.Uintptr) &&
			!quoted && !pointer.Implements(jsonUnmarshaler) && !pointer.Implements(textUnmarshaler)
		metadata.fields = append(metadata.fields, legacyNativeObjectField{name: name, prefix: prefix, index: index, scalarString: scalarString, interfaceString: interfaceString, stringList: stringList, blockList: blockList, eventList: eventList, fixedScalar: fixedScalar, nestedObject: nestedObject, genericMap: genericMap, typedNumber: typedNumber})
		metadata.keyBytes = max(metadata.keyBytes, 12*utf8.RuneCountInString(name)+2)
		metadata.rawBytes = max(metadata.rawBytes, len(prefix)+LegacyCanonicalEncodingBytes+1)
	}
	return metadata
}

func (metadata legacyNativeObjectFields) match(name string) int {
	for index, field := range metadata.fields {
		if name == field.name {
			return index
		}
	}
	for index, field := range metadata.fields {
		if strings.EqualFold(name, field.name) {
			return index
		}
	}
	return -1
}

func legacyNativeFieldTypeError(current byte, model reflect.Type, position int) error {
	var kind string
	switch {
	case current == 't' || current == 'f':
		kind = "bool"
	case current == '"':
		kind = "string"
	case current == '[':
		kind = "array"
	case current == '{':
		kind = "object"
	case current == '-' || current >= '0' && current <= '9':
		kind = "number"
	default:
		return ErrIntegrity
	}
	return &json.UnmarshalTypeError{Value: kind, Type: model, Offset: int64(position)}
}

const (
	legacyNativeObjectStart uint8 = iota
	legacyNativeObjectFirst
	legacyNativeObjectKey
	legacyNativeObjectColon
	legacyNativeObjectValue
	legacyNativeObjectApply
	legacyNativeObjectAfter
	legacyNativeObjectDone
)

type legacyNativeObject struct {
	mutation          *legacyNormalizationMutation
	destination       reflect.Value
	checkpointScanner *legacyJSONValueScanner
	checkpointCursor  *legacyNativeEventList
	checkpointBinding bool
	metadata          *legacyNativeObjectFields
	parentDepth       int
	trackPresence     bool
	heaps             []docRangeHeaderHeapEstimate
	backingExcess     []int64
	key               []byte
	raw               []byte
	scalar            *legacyNativeScalar
	number            *legacyNativeNumberValue
	typedNumber       *legacyNativeTypedNumber
	lists             []*legacyNativeStringList
	blocks            []*legacyNativeBlockList
	events            []*legacyNativeEventList
	pagedEvents       bool
	nested            []*legacyNativeObject
	containers        []*legacyNativeAny
	initialAlias      legacyNativeInitialAlias
	listActive        bool
	blockActive       bool
	eventActive       bool
	nestedActive      bool
	containerActive   bool
	deferTypeErrors   bool
	typeError         *json.UnmarshalTypeError
	skipValue         bool
	fieldLimit        int
	prefix            bool
	position          int
	end               int
	valueStart        int
	valueEnd          int
	field             int
	presence          eventFieldPresence
	phase             uint8
	valueFirst        byte
	keyStarted        bool
	keyOverflow       bool
	valueStarted      bool
}

func newLegacyNativeObject(destination reflect.Value, metadata *legacyNativeObjectFields, parentDepth int, trackPresence bool) (*legacyNativeObject, error) {
	if metadata == nil || metadata.err != nil {
		if metadata != nil && metadata.err != nil {
			return nil, metadata.err
		}
		return nil, ErrIntegrity
	}
	if destination.Kind() != reflect.Pointer || destination.IsNil() || destination.Elem().Type() != metadata.model || parentDepth < 1 {
		return nil, ErrIntegrity
	}
	return &legacyNativeObject{
		mutation:    &legacyNormalizationMutation{},
		destination: destination, metadata: metadata, parentDepth: parentDepth, trackPresence: trackPresence,
		heaps:         make([]docRangeHeaderHeapEstimate, len(metadata.fields)),
		backingExcess: make([]int64, len(metadata.fields)),
		key:           make([]byte, 0, metadata.keyBytes),
	}, nil
}

func (native *legacyNativeObject) start(from, to int) error {
	if err := native.mutation.enter(); err != nil {
		return err
	}
	defer native.mutation.leave()
	if native.checkpointScanner != nil {
		for _, list := range native.blocks {
			if list != nil && list.frame == native.checkpointScanner {
				list.frame = nil
				list.ownedFrame = false
			}
		}
		native.checkpointScanner = nil
	}
	native.checkpointCursor, native.checkpointBinding = nil, false
	native.position, native.end, native.phase, native.field = from, to, legacyNativeObjectStart, -1
	native.presence = 0
	native.key, native.raw = native.key[:0], native.raw[:0]
	native.scalar = nil
	native.number = nil
	native.typedNumber = nil
	native.listActive = false
	native.blockActive = false
	native.eventActive = false
	native.nestedActive = false
	native.containerActive = false
	native.typeError, native.skipValue = nil, false
	native.prefix = false
	native.valueFirst = 0
	native.keyStarted, native.keyOverflow, native.valueStarted = false, false, false
	return nil
}

func (native *legacyNativeObject) releaseValues() error {
	if err := native.mutation.enter(); err != nil {
		return err
	}
	defer native.mutation.leave()
	clear(native.heaps)
	clear(native.backingExcess)
	native.lists = nil
	native.blocks = nil
	native.events = nil
	native.nested = nil
	native.containers = nil
	native.initialAlias = legacyNativeInitialAlias{}
	return native.start(0, 0)
}

func (native *legacyNativeObject) decodeStringList(ctx context.Context, input *legacyNormalizationInput) (bool, error) {
	if native.lists == nil {
		native.lists = make([]*legacyNativeStringList, len(native.metadata.fields))
	}
	field := native.metadata.fields[native.field]
	value := native.destination.Elem().Field(field.index)
	list := native.lists[native.field]
	if !native.listActive {
		if list == nil {
			var err error
			list, err = newLegacyNativeStringList(value.Interface().([]string), native.heaps[native.field], native.valueStart, native.valueEnd)
			if err != nil {
				return false, err
			}
			native.lists[native.field] = list
		} else if err := list.start(native.valueStart, native.valueEnd); err != nil {
			return false, err
		}
		native.listActive = true
		list.prefix = true
	}
	complete, err := list.step(ctx, input)
	if err != nil || !complete {
		return false, err
	}
	value.Set(reflect.ValueOf(list.values()))
	native.position, native.valueEnd = list.position, list.position
	native.heaps[native.field], native.listActive = list.heap(), false
	return true, nil
}

func (native *legacyNativeObject) decodeField(ctx context.Context, input *legacyNormalizationInput) error {
	field := native.metadata.fields[native.field]
	length := native.valueEnd - native.valueStart
	input.semanticBytes += length
	if length <= LegacyCanonicalEncodingBytes {
		needed := len(field.prefix) + length + 1
		if cap(native.raw) < needed {
			capacity := min(native.metadata.rawBytes, max(needed, 2*cap(native.raw), 128))
			native.raw = make([]byte, needed, capacity)
		} else {
			native.raw = native.raw[:needed]
		}
		copy(native.raw, field.prefix)
		reader := &legacyNormalizationValueReader{input: input, position: native.valueStart, end: native.valueEnd}
		if _, err := io.ReadFull(reader, native.raw[len(field.prefix):needed-1]); err != nil {
			return err
		}
		native.raw[needed-1] = '}'
		if err := json.Unmarshal(native.raw, native.destination.Interface()); err != nil {
			return err
		}
	} else {
		previous := input.unbounded
		input.unbounded = true
		defer func() { input.unbounded = previous }()
		reader := &legacyNormalizationValueReader{input: input, position: native.valueStart, end: native.valueEnd}
		decoder := json.NewDecoder(io.MultiReader(strings.NewReader(field.prefix), reader, strings.NewReader("}")))
		if err := decoder.Decode(native.destination.Interface()); err != nil {
			return err
		}
		if decoder.InputOffset() != int64(len(field.prefix)+length+1) {
			return ErrIntegrity
		}
	}
	native.clearPublishedInitialAlias()
	return native.refreshDecodedFieldHeap(ctx)
}

func (native *legacyNativeObject) refreshDecodedFieldHeap(ctx context.Context) error {
	field := native.metadata.fields[native.field]
	if field.scalarString && native.valueFirst == 'n' {
		return ctx.Err()
	}
	if preserved, err := native.preserveInitialAliasHeap(ctx); err != nil || preserved {
		if err != nil {
			return err
		}
		return ctx.Err()
	}
	value := native.destination.Elem().Field(field.index)
	estimate, err := legacyNativeValueHeap(ctx, value)
	if err != nil {
		return err
	}
	if err := native.refreshInitialAlias(ctx, estimate); err != nil {
		return err
	}
	native.heaps[native.field] = estimate
	native.backingExcess[native.field] = 0
	if field.nestedObject && native.nested != nil {
		native.nested[native.field] = nil
	}
	if native.containers != nil {
		native.containers[native.field] = nil
	}
	return ctx.Err()
}

func (native *legacyNativeObject) rememberTypeError(err error) bool {
	var failure *json.UnmarshalTypeError
	if !native.deferTypeErrors || !errors.As(err, &failure) {
		return false
	}
	if native.typeError == nil {
		native.typeError = failure
	}
	return true
}

func (native *legacyNativeObject) fieldActionLimit() int {
	limit := LegacyNormalizationStepEvents * len(native.metadata.fields)
	if native.fieldLimit > 0 {
		limit = min(limit, native.fieldLimit)
	}
	return limit
}

func (native *legacyNativeObject) blockListField(field legacyNativeObjectField) bool {
	return field.blockList && (native.blocks != nil && native.blocks[native.field] != nil ||
		native.heaps[native.field].bytes != 0 || native.destination.Elem().Field(field.index).Cap() == 0)
}

func (native *legacyNativeObject) decodeBlockList(ctx context.Context, input *legacyNormalizationInput, frame *legacyJSONValueScanner) (bool, error) {
	if native.blocks == nil {
		native.blocks = make([]*legacyNativeBlockList, len(native.metadata.fields))
	}
	field := native.metadata.fields[native.field]
	value := native.destination.Elem().Field(field.index)
	list := native.blocks[native.field]
	if !native.blockActive {
		if list == nil {
			var err error
			list, err = newLegacyNativeBlockList(value.Interface().([]ContentBlock), native.heaps[native.field], native.valueStart, native.valueEnd)
			if err != nil {
				return false, err
			}
			list.mutation = native.mutation
			list.paged = native.pagedEvents
			native.blocks[native.field] = list
		} else if err := list.start(native.valueStart, native.valueEnd); err != nil {
			return false, err
		}
		list.parentDepth = native.parentDepth + 1
		native.blockActive, list.prefix = true, true
	}
	if list.frame == nil {
		list.frame = frame
	}
	if list.frame != frame || list.ownedFrame {
		return false, ErrIntegrity
	}
	list.borrowed = native.heaps[native.field]
	complete, err := list.step(ctx, input)
	if err != nil || !complete {
		return false, err
	}
	excess, err := list.backingExcess()
	if err != nil {
		return false, err
	}
	if !list.paged {
		value.Set(reflect.ValueOf(list.values()))
	}
	native.position, native.valueEnd = list.position, list.position
	native.heaps[native.field], native.backingExcess[native.field] = list.heap(), excess
	list.borrowed = native.heaps[native.field]
	native.blockActive = false
	return true, nil
}

func (native *legacyNativeObject) backingExcessBytes(fields map[string]any) (int64, error) {
	if native == nil || native.metadata == nil || len(native.backingExcess) != len(native.metadata.fields) {
		return 0, ErrIntegrity
	}
	var estimate docRangeHeaderHeapEstimate
	for index, field := range native.metadata.fields {
		if native.backingExcess[index] == 0 {
			continue
		}
		if fields != nil {
			if _, emitted := fields[field.name]; !emitted {
				continue
			}
		}
		estimate.include(docRangeHeaderHeapEstimate{bytes: native.backingExcess[index]})
	}
	return estimate.bytes, estimate.err
}

func (native *legacyNativeObject) canonicalBackingExcessBytes(fields map[string]any) (int64, error) {
	visible, err := native.backingExcessBytes(fields)
	if err != nil {
		return 0, err
	}
	if len(native.blocks) != 0 && len(native.blocks) != len(native.metadata.fields) {
		return 0, ErrIntegrity
	}
	estimate := docRangeHeaderHeapEstimate{bytes: visible}
	for index, field := range native.metadata.fields {
		values, typed := fields[field.name].([]ContentBlock)
		if !field.blockList || !typed || len(native.blocks) == 0 || native.blocks[index] == nil {
			continue
		}
		list := native.blocks[index]
		if list.phase != legacyNativeBlocksDone || len(values) != len(list.decoded) || cap(values) != cap(list.decoded) ||
			cap(values) != 0 && unsafe.SliceData(values) != unsafe.SliceData(list.decoded) || list.visibleExcess != native.backingExcess[index] ||
			list.totalExcess < list.visibleExcess {
			return 0, ErrIntegrity
		}
		estimate.include(docRangeHeaderHeapEstimate{bytes: list.totalExcess - list.visibleExcess})
	}
	return estimate.bytes, estimate.err
}

func (native *legacyNativeObject) scalarStringField(field legacyNativeObjectField) bool {
	if field.scalarString {
		return true
	}
	if !field.interfaceString {
		return false
	}
	value := native.destination.Elem().Field(field.index)
	return value.IsNil() || value.Elem().Kind() != reflect.Pointer || value.Elem().IsNil()
}

func (native *legacyNativeObject) progressPosition() int {
	position := native.position
	if native.eventActive && native.events[native.field] != nil {
		position = max(position, native.events[native.field].progressPosition())
	}
	if native.listActive && native.lists[native.field] != nil {
		position = max(position, native.lists[native.field].position)
	}
	if native.blockActive && native.blocks[native.field] != nil {
		list := native.blocks[native.field]
		position = max(position, list.position)
		if list.object != nil {
			position = max(position, list.object.progressPosition())
		}
	}
	if native.nestedActive && native.nested[native.field] != nil {
		position = max(position, native.nested[native.field].progressPosition())
	}
	if native.containerActive && native.containers[native.field] != nil {
		position = max(position, native.containers[native.field].position)
	}
	if native.number != nil {
		position = max(position, native.number.parser.position)
	}
	return position
}

func (native *legacyNativeObject) step(ctx context.Context, input *legacyNormalizationInput, frame *legacyJSONValueScanner) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := native.mutation.enter(); err != nil {
		return false, err
	}
	defer native.mutation.leave()
	if native.checkpointScanner != nil {
		ready, err := native.bindCheckpointScanner(ctx, input, frame)
		if err != nil || !ready {
			return false, err
		}
	}
	checked := input.workBytes
	fieldLimit := input.fieldActionLimit(native.fieldActionLimit())
	if input.eventsRunning != nil {
		fieldLimit = min(fieldLimit, input.fields+native.fieldActionLimit())
		previousLimit := input.eventsFieldLimit
		input.eventsFieldLimit = fieldLimit
		defer func() { input.eventsFieldLimit = previousLimit }()
	}
	for input.workBytes < ConversationManifestChunkBytes && input.fields < fieldLimit {
		if input.workBytes-checked >= canonicalCheckBytes {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			checked = input.workBytes
		}
		if native.phase == legacyNativeObjectDone {
			if native.position != native.end && !native.prefix {
				return false, ErrIntegrity
			}
			if native.typeError != nil {
				return false, native.typeError
			}
			return true, nil
		}
		if native.phase == legacyNativeObjectApply {
			if native.field >= 0 && !native.skipValue {
				field := native.metadata.fields[native.field]
				if native.eventListField(field) && (native.valueFirst == '[' || native.valueFirst == 'n') {
					complete, err := native.decodeEventList(ctx, input, frame)
					if err != nil || !complete {
						return false, err
					}
				} else if native.genericContainerField(field, native.valueFirst) {
					complete, err := native.decodeAnyContainer(ctx, input, frame)
					if err != nil && (input.failure != nil || ctx.Err() != nil || !native.rememberTypeError(err)) {
						return false, err
					}
					if !complete {
						return false, nil
					}
				} else if native.numberField(field) {
					complete, err := native.decodeNumber(ctx, input)
					if err != nil && (input.failure != nil || ctx.Err() != nil || !native.rememberTypeError(err)) {
						return false, err
					}
					if !complete {
						return false, nil
					}
				} else if native.typedNumberField(field) {
					complete, err := native.decodeTypedNumber(ctx, input)
					if err != nil && (input.failure != nil || ctx.Err() != nil || !native.rememberTypeError(err)) {
						return false, err
					}
					if !complete {
						return false, nil
					}
				} else if native.wrongNumberField(field) {
					complete, err := native.decodeWrongNumber(ctx, input)
					if err != nil && (input.failure != nil || ctx.Err() != nil || !native.rememberTypeError(err)) {
						return false, err
					}
					if !complete {
						return false, nil
					}
				} else if field.nestedObject && native.valueFirst == '{' {
					complete, err := native.decodeNestedObject(ctx, input, frame)
					if err != nil || !complete {
						return false, err
					}
				} else if field.stringList && (native.valueFirst == '[' || native.valueFirst == 'n') {
					complete, err := native.decodeStringList(ctx, input)
					if err != nil || !complete {
						return false, err
					}
				} else if native.blockListField(field) && (native.valueFirst == '[' || native.valueFirst == 'n') {
					complete, err := native.decodeBlockList(ctx, input, frame)
					if err != nil || !complete {
						return false, err
					}
				} else if native.scalarStringField(field) && native.valueFirst == '"' && native.valueEnd-native.valueStart > LegacyCanonicalEncodingBytes {
					if native.scalar == nil {
						native.scalar = &legacyNativeScalar{
							position: native.valueStart + 1,
							end:      native.valueEnd - 1,
							maximum:  3 * (native.valueEnd - native.valueStart - 2),
						}
					}
					complete, err := native.scalar.step(ctx, input)
					if err != nil {
						return false, err
					}
					if !complete {
						return false, nil
					}
					value := native.destination.Elem().Field(field.index)
					var estimate docRangeHeaderHeapEstimate
					estimate.allocation(1, int64(value.Type().Size()))
					if field.interfaceString {
						value.Set(reflect.ValueOf(native.scalar.text.String()))
						native.clearPublishedInitialAlias()
						estimate.allocation(1, int64(unsafe.Sizeof(string(""))))
					} else {
						value.SetString(native.scalar.text.String())
					}
					estimate.allocation(int64(native.scalar.text.Cap()), 1)
					if estimate.err != nil {
						return false, estimate.err
					}
					native.backingExcess[native.field] = 2 * int64(native.scalar.text.Cap()-native.scalar.text.Len())
					native.heaps[native.field], native.scalar = estimate, nil
					if native.containers != nil {
						native.containers[native.field] = nil
					}
				} else {
					if input.numberDiagnostics == legacyNativeNumberDiagnosticsRejectOnly && native.valueEnd-native.valueStart > LegacyCanonicalEncodingBytes &&
						(field.fixedScalar || field.scalarString && native.valueFirst != '"') {
						return false, ErrIntegrity
					}
					if !input.canDecode(native.valueStart, native.valueEnd) {
						return false, nil
					}
					if err := native.decodeField(ctx, input); err != nil {
						if input.failure != nil || ctx.Err() != nil || !native.rememberTypeError(err) {
							return false, err
						}
						if err := native.refreshDecodedFieldHeap(ctx); err != nil {
							return false, err
						}
					}
				}
			}
			input.fields++
			native.phase = legacyNativeObjectAfter
			continue
		}
		if native.position >= native.end {
			return false, ErrIntegrity
		}
		current, err := input.current(native.position)
		if errors.Is(err, errLegacyNormalizationYield) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		switch native.phase {
		case legacyNativeObjectStart:
			if current == '{' {
				native.position++
				native.phase = legacyNativeObjectFirst
			} else if current == 'n' {
				native.position, native.phase = native.end, legacyNativeObjectDone
			} else {
				return false, ErrIntegrity
			}
		case legacyNativeObjectFirst, legacyNativeObjectAfter:
			if legacyNormalizationSpace(current) {
				native.position++
			} else if current == '}' {
				native.position++
				native.phase = legacyNativeObjectDone
			} else {
				if native.phase == legacyNativeObjectAfter {
					if current != ',' {
						return false, ErrIntegrity
					}
					native.position++
				}
				native.key = native.key[:0]
				native.keyStarted, native.keyOverflow = false, false
				if err := frame.reset(native.parentDepth); err != nil {
					return false, err
				}
				native.phase = legacyNativeObjectKey
			}
		case legacyNativeObjectKey:
			if !native.keyStarted {
				if legacyNormalizationSpace(current) {
					native.position++
					continue
				}
				if current != '"' {
					return false, ErrIntegrity
				}
				native.keyStarted = true
			}
			consumed, complete, err := frame.add(current)
			if err != nil || !consumed {
				return false, ErrIntegrity
			}
			if len(native.key) < cap(native.key) {
				native.key = append(native.key, current)
			} else {
				native.keyOverflow = true
			}
			native.position++
			if complete {
				native.field = -1
				if !native.keyOverflow {
					var name string
					if err := json.Unmarshal(native.key, &name); err != nil {
						return false, err
					}
					if native.trackPresence {
						native.presence |= eventPresenceName(name)
					}
					native.field = native.metadata.match(name)
				}
				native.phase = legacyNativeObjectColon
			}
		case legacyNativeObjectColon:
			if legacyNormalizationSpace(current) {
				native.position++
			} else if current == ':' {
				native.position++
				native.valueStarted = false
				if err := frame.reset(native.parentDepth); err != nil {
					return false, err
				}
				native.phase = legacyNativeObjectValue
			} else {
				return false, ErrIntegrity
			}
		case legacyNativeObjectValue:
			if !native.valueStarted {
				if legacyNormalizationSpace(current) {
					native.position++
					continue
				}
				native.valueStart, native.valueStarted = native.position, true
				native.valueFirst = current
				native.skipValue = false
				if native.field >= 0 && native.eventListField(native.metadata.fields[native.field]) && (current == '[' || current == 'n') {
					native.valueEnd, native.phase = native.end, legacyNativeObjectApply
					continue
				}
				if native.field >= 0 && native.genericContainerField(native.metadata.fields[native.field], current) {
					native.valueEnd, native.phase = native.end, legacyNativeObjectApply
					continue
				}
				if native.prefix && native.field >= 0 && native.metadata.fields[native.field].scalarString && current != '"' && current != 'n' {
					field := native.metadata.fields[native.field]
					failure := legacyNativeFieldTypeError(current, native.metadata.model.Field(field.index).Type, native.position)
					if !native.rememberTypeError(failure) {
						return false, failure
					}
					native.skipValue = true
				}
				if native.field >= 0 && native.metadata.fields[native.field].nestedObject && current == '{' {
					native.valueEnd, native.phase = native.end, legacyNativeObjectApply
					continue
				}
				if native.field >= 0 && native.metadata.fields[native.field].stringList && (current == '[' || current == 'n') {
					native.valueEnd, native.phase = native.end, legacyNativeObjectApply
					continue
				}
				if native.field >= 0 && native.blockListField(native.metadata.fields[native.field]) && (current == '[' || current == 'n') {
					native.valueEnd, native.phase = native.end, legacyNativeObjectApply
					continue
				}
			}
			consumed, complete, err := frame.add(current)
			if err != nil || !consumed && !complete {
				return false, ErrIntegrity
			}
			if consumed {
				native.position++
			}
			if complete {
				native.valueEnd, native.phase = native.position, legacyNativeObjectApply
			}
		default:
			return false, ErrIntegrity
		}
	}
	return false, nil
}

func (native *legacyNativeObject) retained() (int64, error) {
	bytes, err := native.retainedBase()
	heap := docRangeHeaderHeapEstimate{bytes: bytes, err: err}
	for _, list := range native.events {
		if list != nil {
			list.retained(&heap)
		}
	}
	return heap.bytes, heap.err
}

func (native *legacyNativeObject) retainedBase() (int64, error) {
	var estimate docRangeHeaderHeapEstimate
	estimate.allocation(1, int64(unsafe.Sizeof(*native)))
	if native.mutation != nil {
		estimate.allocation(1, int64(unsafe.Sizeof(*native.mutation)))
	}
	if native.checkpointScanner != nil {
		estimate.allocation(1, int64(unsafe.Sizeof(*native.checkpointScanner)))
	}
	estimate.allocation(int64(cap(native.key)+cap(native.raw)), 1)
	estimate.allocation(int64(cap(native.heaps)), int64(unsafe.Sizeof(docRangeHeaderHeapEstimate{})))
	estimate.allocation(int64(cap(native.backingExcess)), int64(unsafe.Sizeof(int64(0))))
	estimate.allocation(int64(cap(native.lists)), int64(unsafe.Sizeof((*legacyNativeStringList)(nil))))
	estimate.allocation(int64(cap(native.blocks)), int64(unsafe.Sizeof((*legacyNativeBlockList)(nil))))
	estimate.allocation(int64(cap(native.events)), int64(unsafe.Sizeof((*legacyNativeEventList)(nil))))
	estimate.allocation(int64(cap(native.nested)), int64(unsafe.Sizeof((*legacyNativeObject)(nil))))
	estimate.allocation(int64(cap(native.containers)), int64(unsafe.Sizeof((*legacyNativeAny)(nil))))
	if native.typeError != nil {
		estimate.allocation(1, int64(unsafe.Sizeof(*native.typeError)))
		estimate.allocation(int64(len(native.typeError.Value)), 1)
		estimate.allocation(int64(len(native.typeError.Struct)), 1)
		estimate.allocation(int64(len(native.typeError.Field)), 1)
	}
	for _, list := range native.lists {
		list.retained(&estimate)
	}
	for _, list := range native.blocks {
		list.retained(&estimate)
	}
	if native.scalar != nil {
		native.scalar.retained(&estimate)
	}
	native.number.retained(&estimate)
	native.typedNumber.retained(&estimate)
	for index, field := range native.heaps {
		if index < len(native.containers) && native.containers[index] != nil {
			parser := native.containers[index]
			parser.retained(&estimate)
			var storage docRangeHeaderHeapEstimate
			storage.allocation(1, int64(native.metadata.model.Field(native.metadata.fields[index].index).Type.Size()))
			if !native.metadata.fields[index].genericMap && native.containerActive && native.field == index {
				estimate.include(field)
				if field.bytes < storage.bytes {
					estimate.include(storage)
				}
			} else {
				estimate.include(storage)
			}
		} else if index < len(native.nested) && native.nested[index] != nil {
			child := native.nested[index]
			bytes, err := child.retained()
			estimate.include(docRangeHeaderHeapEstimate{bytes: bytes, err: err})
			estimate.allocation(1, int64(child.destination.Type().Size()))
			estimate.allocation(1, int64(child.metadata.model.Size()))
		} else {
			estimate.include(field)
		}
	}
	return estimate.bytes, estimate.err
}
