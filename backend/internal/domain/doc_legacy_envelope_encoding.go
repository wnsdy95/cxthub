package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"unsafe"
)

type LegacyEnvelopeEncoding struct {
	envelope       CIREnvelope
	envelopeHeap   docRangeHeaderHeapEstimate
	fields         []legacyEnvelopeEncodingField
	field          int
	model          int
	phase          uint8
	scalar         *LegacyCanonicalEncoding
	scalarBorrowed docRangeHeaderHeapEstimate
	pending        []byte
	initialized    bool
	complete       bool
	err            error
}

type legacyEnvelopeEncodingField struct {
	name  string
	key   []byte
	index int
}

const (
	legacyEnvelopeEncodingStart uint8 = iota
	legacyEnvelopeEncodingFieldNext
	legacyEnvelopeEncodingValue
	legacyEnvelopeEncodingModelNext
	legacyEnvelopeEncodingModelValue
)

func newLegacyEnvelopeEncoding(ctx context.Context, envelope CIREnvelope, envelopeHeap docRangeHeaderHeapEstimate) (*LegacyEnvelopeEncoding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if envelopeHeap.err != nil {
		return nil, envelopeHeap.err
	}
	if envelopeHeap.bytes < 0 {
		return nil, ErrIntegrity
	}
	encoding := &LegacyEnvelopeEncoding{envelope: envelope, envelopeHeap: envelopeHeap, initialized: true}
	value := reflect.ValueOf(&encoding.envelope).Elem()
	valueType := value.Type()
	encoding.fields = make([]legacyEnvelopeEncodingField, 0, value.NumField())
	var minimumHeap docRangeHeaderHeapEstimate
	for index := 0; index < value.NumField(); index++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		field := valueType.Field(index)
		if field.PkgPath != "" || field.Tag.Get("json") == "-" {
			continue
		}
		name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" {
			name = field.Name
		}
		if field.Anonymous || options != "" && options != "omitempty" {
			return nil, fmt.Errorf("%w: unsupported legacy envelope JSON field %s", ErrValidation, field.Name)
		}
		member := value.Field(index)
		switch member.Kind() {
		case reflect.String, reflect.Int:
			if member.Kind() == reflect.String {
				minimumHeap.allocation(int64(member.Len()), 1)
			}
			if options == "omitempty" && member.IsZero() {
				continue
			}
		case reflect.Slice:
			if member.Type().Elem().Kind() != reflect.String {
				return nil, fmt.Errorf("%w: unsupported legacy envelope slice %s", ErrValidation, field.Name)
			}
			minimumHeap.allocation(int64(member.Cap()), int64(member.Type().Elem().Size()))
			if options == "omitempty" && member.Len() == 0 {
				continue
			}
		default:
			return nil, fmt.Errorf("%w: unsupported legacy envelope type %s", ErrValidation, field.Name)
		}
		key, err := json.Marshal(name)
		if err != nil {
			return nil, err
		}
		encoding.fields = append(encoding.fields, legacyEnvelopeEncodingField{name: name, key: key, index: index})
	}
	if minimumHeap.err != nil {
		return nil, minimumHeap.err
	}
	if envelopeHeap.bytes < minimumHeap.bytes {
		return nil, ErrIntegrity
	}
	sort.Slice(encoding.fields, func(first, second int) bool {
		return encoding.fields[first].name < encoding.fields[second].name
	})
	if _, err := encoding.RetainedBytes(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return encoding, nil
}

func (encoding *LegacyEnvelopeEncoding) stepWork() (int, int) {
	if encoding.pending != nil {
		return 2 * len(encoding.pending), len(encoding.pending)
	}
	if encoding.scalar != nil {
		source := 32
		if text, ok := encoding.scalar.root.(string); ok {
			source = min(LegacyCanonicalEncodingBytes, len(text)+2)
		}
		encoded := min(LegacyCanonicalEncodingBytes, 6*source+2)
		return source + 5*encoded, 0
	}
	return 128, 128
}

func (encoding *LegacyEnvelopeEncoding) Step(ctx context.Context) (output []byte, complete bool, failure error) {
	if err := ctx.Err(); err != nil {
		if encoding != nil && encoding.initialized {
			encoding.release()
			encoding.err = err
		}
		return nil, false, err
	}
	if encoding == nil || !encoding.initialized {
		return nil, false, ErrValidation
	}
	if encoding.err != nil {
		return nil, false, encoding.err
	}
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			encoding.release()
			encoding.err = failure
			output, complete = nil, false
		}
	}()
	if encoding.complete {
		return nil, true, nil
	}
	if encoding.pending != nil {
		output, failure = legacyEnvelopeCanonicalStringChunk(ctx, encoding.pending)
		encoding.pending = nil
		return output, false, failure
	}
	if encoding.scalar != nil {
		chunk, done, err := encoding.scalar.Next(ctx)
		if err != nil {
			return nil, false, err
		}
		encoding.pending = chunk
		if done {
			encoding.scalar = nil
			encoding.scalarBorrowed = docRangeHeaderHeapEstimate{}
		}
		return nil, false, nil
	}
	value := reflect.ValueOf(&encoding.envelope).Elem()
	switch encoding.phase {
	case legacyEnvelopeEncodingStart:
		encoding.phase = legacyEnvelopeEncodingFieldNext
		return []byte{'{'}, false, nil
	case legacyEnvelopeEncodingFieldNext:
		if encoding.field == len(encoding.fields) {
			encoding.release()
			encoding.complete = true
			return []byte{'}'}, true, nil
		}
		field := encoding.fields[encoding.field]
		output = make([]byte, 0, len(field.key)+2)
		if encoding.field != 0 {
			output = append(output, ',')
		}
		output = append(output, field.key...)
		output = append(output, ':')
		encoding.phase = legacyEnvelopeEncodingValue
		return output, false, nil
	case legacyEnvelopeEncodingValue:
		member := value.Field(encoding.fields[encoding.field].index)
		switch member.Kind() {
		case reflect.String:
			failure = encoding.beginScalar(ctx, member.String())
		case reflect.Int:
			failure = encoding.beginScalar(ctx, int(member.Int()))
		case reflect.Slice:
			if !member.IsNil() {
				encoding.model, encoding.phase = 0, legacyEnvelopeEncodingModelNext
				return []byte{'['}, false, nil
			}
			failure = encoding.beginScalar(ctx, nil)
		default:
			return nil, false, ErrIntegrity
		}
		encoding.field++
		encoding.phase = legacyEnvelopeEncodingFieldNext
		return nil, false, failure
	case legacyEnvelopeEncodingModelNext:
		member := value.Field(encoding.fields[encoding.field].index)
		if encoding.model == member.Len() {
			encoding.field++
			encoding.phase = legacyEnvelopeEncodingFieldNext
			return []byte{']'}, false, nil
		}
		encoding.phase = legacyEnvelopeEncodingModelValue
		if encoding.model != 0 {
			return []byte{','}, false, nil
		}
		return nil, false, nil
	case legacyEnvelopeEncodingModelValue:
		member := value.Field(encoding.fields[encoding.field].index)
		failure = encoding.beginScalar(ctx, member.Index(encoding.model).String())
		encoding.model++
		encoding.phase = legacyEnvelopeEncodingModelNext
		return nil, false, failure
	default:
		return nil, false, ErrIntegrity
	}
}

func (encoding *LegacyEnvelopeEncoding) beginScalar(ctx context.Context, value any) error {
	scalar, err := newLegacyCanonicalEncodingPending(ctx, value)
	if err != nil {
		return err
	}
	encoding.scalar = scalar
	encoding.scalarBorrowed = docRangeHeaderHeapEstimate{}
	if text, ok := value.(string); ok {
		encoding.scalarBorrowed.allocation(int64(len(text)), 1)
	}
	return nil
}

func legacyEnvelopeCanonicalStringChunk(ctx context.Context, chunk []byte) ([]byte, error) {
	if len(chunk) > LegacyCanonicalEncodingBytes || cap(chunk) > LegacyCanonicalEncodingBytes {
		return nil, ErrIntegrity
	}
	written := 0
	for position := 0; position < len(chunk); {
		if position == 0 || position%4096 < 6 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		width := 1
		if chunk[position] == '\\' {
			if position+1 >= len(chunk) {
				return nil, ErrIntegrity
			}
			width = 2
			if chunk[position+1] == 'u' {
				width = 6
				if len(chunk)-position < width {
					return nil, ErrIntegrity
				}
				if string(chunk[position:position+width]) == `\ufffd` {
					chunk[written], chunk[written+1], chunk[written+2] = 0xef, 0xbf, 0xbd
					written += 3
					position += width
					continue
				}
			}
		}
		copy(chunk[written:written+width], chunk[position:position+width])
		written += width
		position += width
	}
	return chunk[:written], ctx.Err()
}

func (encoding *LegacyEnvelopeEncoding) RetainedBytes() (int64, error) {
	if encoding == nil || !encoding.initialized {
		return 0, ErrValidation
	}
	if encoding.err != nil {
		return 0, encoding.err
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*encoding)))
	heap.include(encoding.envelopeHeap)
	heap.allocation(int64(cap(encoding.fields)), int64(unsafe.Sizeof(legacyEnvelopeEncodingField{})))
	for _, field := range encoding.fields {
		heap.allocation(int64(len(field.name)), 1)
		heap.allocation(int64(cap(field.key)), 1)
	}
	heap.allocation(int64(cap(encoding.pending)), 1)
	if encoding.scalar != nil {
		retained, err := encoding.scalar.RetainedBytes()
		if err != nil {
			return 0, err
		}
		if retained < encoding.scalarBorrowed.bytes {
			return 0, ErrIntegrity
		}
		heap.include(docRangeHeaderHeapEstimate{bytes: retained - encoding.scalarBorrowed.bytes})
	}
	return heap.bytes, heap.err
}

func (encoding *LegacyEnvelopeEncoding) release() {
	encoding.envelope = CIREnvelope{}
	encoding.envelopeHeap = docRangeHeaderHeapEstimate{}
	encoding.fields = nil
	encoding.scalar = nil
	encoding.scalarBorrowed = docRangeHeaderHeapEstimate{}
	encoding.pending = nil
}
