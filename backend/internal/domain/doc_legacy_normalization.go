package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"unsafe"
)

const LegacyNormalizationStepEvents = 64

var legacyEnvelopeNativeFields = newLegacyNativeObjectFields(reflect.TypeOf(CIREnvelope{}))

const (
	legacyNormalizationStart uint8 = iota
	legacyNormalizationRootFirst
	legacyNormalizationRootNext
	legacyNormalizationKey
	legacyNormalizationColon
	legacyNormalizationValue
	legacyNormalizationRootNull
	legacyNormalizationEnvelope
	legacyNormalizationIgnored
	legacyNormalizationEventsNull
	legacyNormalizationEventsFirst
	legacyNormalizationEventsNext
	legacyNormalizationEventRequired
	legacyNormalizationEvent
	legacyNormalizationTrailing
	legacyNormalizationReject
	legacyNormalizationFinalizing
	legacyNormalizationComplete
)

const (
	legacyNormalizationUnknown uint8 = iota
	legacyNormalizationEnvelopeField
	legacyNormalizationEventsField
)

type LegacyNormalizedEvent struct {
	Sequence int
	Encoding *LegacyCanonicalEncoding
}

type LegacyNormalizationStep struct {
	Reset    bool
	Events   []LegacyNormalizedEvent
	Complete bool
	Envelope json.RawMessage
	Count    int
}

type LegacyNormalizationDecoder struct {
	mutation       *legacyNormalizationMutation
	source         LegacyByteSource
	sourceHeap     docRangeHeaderHeapEstimate
	position       int
	phase          uint8
	frame          legacyJSONValueScanner
	valueStart     int
	field          uint8
	key            [64]byte
	keyBytes       int
	envelope       CIREnvelope
	header         []byte
	count          int
	requiresV2     bool
	invalid        bool
	rejectPart     int
	terminal       error
	native         *legacyNormalizationNativeEvent
	nativeCache    *legacyNormalizationNativeEvent
	envelopeNative *legacyNativeObject
	envelopeActive bool
	finalizing     *legacyNormalizationHeader
}

func NewLegacyNormalizationDecoder(ctx context.Context, source LegacyByteSource) (*LegacyNormalizationDecoder, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	partBytes := source.partBytes()
	if partBytes == 0 || source.Bytes < 0 || source.Bytes > MaxFinalizedDocBytes || source.Read == nil ||
		len(source.Parts) != (source.Bytes+partBytes-1)/partBytes {
		return nil, ErrIntegrity
	}
	source.Parts = append([]ConversationManifestChunk(nil), source.Parts...)
	var sourceHeap docRangeHeaderHeapEstimate
	for ordinal := range source.Parts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		part := &source.Parts[ordinal]
		if ValidateContentHash(part.Hash) != nil || part.Bytes != int64(min(partBytes, source.Bytes-ordinal*partBytes)) {
			return nil, ErrIntegrity
		}
		part.Hash = ContentHash(strings.Clone(string(part.Hash)))
		sourceHeap.allocation(int64(len(part.Hash)), 1)
	}
	if sourceHeap.err != nil {
		return nil, sourceHeap.err
	}
	authority, err := newLegacyNormalizationSourceAuthority()
	if err != nil {
		return nil, err
	}
	source.authority = authority
	return &LegacyNormalizationDecoder{mutation: &legacyNormalizationMutation{}, source: source, sourceHeap: sourceHeap}, nil
}

func legacyNormalizationSpace(current byte) bool {
	return current == ' ' || current == '\t' || current == '\r' || current == '\n'
}

func (normalizer *LegacyNormalizationDecoder) reject() {
	normalizer.phase = legacyNormalizationReject
	normalizer.envelope, normalizer.header = CIREnvelope{}, nil
	normalizer.envelopeNative, normalizer.envelopeActive = nil, false
	normalizer.finalizing = nil
	normalizer.native = nil
	if normalizer.nativeCache != nil {
		normalizer.nativeCache.releaseValues()
	}
}

func (normalizer *LegacyNormalizationDecoder) beginValue(parents int, phase uint8) error {
	if err := normalizer.frame.reset(parents); err != nil {
		return err
	}
	normalizer.valueStart, normalizer.phase = normalizer.position, phase
	return nil
}

func (normalizer *LegacyNormalizationDecoder) endValue(ctx context.Context, input *legacyNormalizationInput, result *LegacyNormalizationStep) error {
	switch normalizer.phase {
	case legacyNormalizationKey:
		normalizer.field = legacyNormalizationUnknown
		if normalizer.keyBytes <= len(normalizer.key) {
			var name string
			if err := json.Unmarshal(normalizer.key[:normalizer.keyBytes], &name); err != nil {
				return err
			}
			if strings.EqualFold(name, "envelope") {
				normalizer.field = legacyNormalizationEnvelopeField
			} else if strings.EqualFold(name, "events") {
				normalizer.field = legacyNormalizationEventsField
			}
		}
		normalizer.phase = legacyNormalizationColon
	case legacyNormalizationRootNull:
		normalizer.phase = legacyNormalizationTrailing
	case legacyNormalizationIgnored:
		normalizer.phase = legacyNormalizationRootNext
	case legacyNormalizationEnvelope:
		if normalizer.envelopeNative == nil {
			decoder, err := newLegacyNativeObject(reflect.ValueOf(&normalizer.envelope), &legacyEnvelopeNativeFields, 2, false)
			if err != nil {
				return err
			}
			normalizer.envelopeNative = decoder
			normalizer.envelopeNative.mutation = normalizer.mutation
		}
		if err := normalizer.envelopeNative.start(normalizer.valueStart, normalizer.position); err != nil {
			return err
		}
		normalizer.envelopeActive = true
		normalizer.phase = legacyNormalizationRootNext
	case legacyNormalizationEventsNull:
		normalizer.count, normalizer.requiresV2, normalizer.invalid = 0, false, false
		normalizer.phase, result.Reset = legacyNormalizationRootNext, true
	case legacyNormalizationEvent:
		if err := normalizer.beginNativeEvent(normalizer.valueStart, normalizer.position, false); err != nil {
			return err
		}
	default:
		return ErrIntegrity
	}
	return ctx.Err()
}

func (normalizer *LegacyNormalizationDecoder) beginNativeEvent(from, to int, prefix bool) error {
	if normalizer.nativeCache == nil {
		cache, err := newLegacyNormalizationNativeEvent()
		if err != nil {
			return err
		}
		normalizer.nativeCache = cache
		normalizer.nativeCache.object.mutation = normalizer.mutation
	}
	if err := normalizer.nativeCache.reset(from, to); err != nil {
		return err
	}
	normalizer.nativeCache.object.pagedEvents = true
	normalizer.nativeCache.object.prefix = prefix
	normalizer.native, normalizer.phase = normalizer.nativeCache, legacyNormalizationEvent
	return nil
}

func (normalizer *LegacyNormalizationDecoder) finishEvent(ctx context.Context, event CIREvent, result *LegacyNormalizationStep) error {
	normalizer.requiresV2 = normalizer.requiresV2 || eventsRequireCIRV2([]CIREvent{event})
	if normalizer.nativeCache != nil && normalizer.nativeCache.canonicalReady {
		owner, err := normalizer.nativeCache.takeCanonicalOwner(ctx)
		if err != nil {
			return err
		}
		encoding, err := newLegacyCanonicalEncodingNative(ctx, owner)
		if err != nil {
			owner.release()
			normalizer.invalid = true
		} else {
			result.Events = append(result.Events, LegacyNormalizedEvent{Sequence: event.Seq, Encoding: encoding})
		}
		normalizer.count++
		normalizer.phase = legacyNormalizationEventsNext
		return ctx.Err()
	}
	if event.Replacement != nil {
		event.Replacement = canonicalEvents(event.Replacement)
	}
	value, err := legacyCanonicalEventJSONObject(event)
	if err != nil {
		normalizer.invalid = true
	} else {
		encoding, err := newLegacyCanonicalEncodingPending(ctx, value)
		if err != nil {
			normalizer.invalid = true
		} else {
			if normalizer.nativeCache != nil {
				excess, err := normalizer.nativeCache.object.canonicalBackingExcessBytes(value)
				if err != nil {
					return err
				}
				encoding.rootHeap.include(docRangeHeaderHeapEstimate{bytes: excess})
				if encoding.rootHeap.err != nil {
					return encoding.rootHeap.err
				}
			}
			result.Events = append(result.Events, LegacyNormalizedEvent{Sequence: event.Seq, Encoding: encoding})
		}
	}
	normalizer.count++
	normalizer.phase = legacyNormalizationEventsNext
	return ctx.Err()
}

func (normalizer *LegacyNormalizationDecoder) scan(ctx context.Context, input *legacyNormalizationInput) (LegacyNormalizationStep, error) {
	var result LegacyNormalizationStep
	start, events := normalizer.position, 0
	for input.workBytes < ConversationManifestChunkBytes && normalizer.position-start < ConversationManifestChunkBytes {
		if normalizer.envelopeActive {
			complete, err := normalizer.envelopeNative.step(ctx, input, &normalizer.frame)
			if err != nil {
				return LegacyNormalizationStep{}, err
			}
			if !complete {
				return result, nil
			}
			normalizer.envelopeActive = false
			continue
		}
		if normalizer.native != nil {
			event, complete, err := normalizer.native.step(ctx, input, &normalizer.frame)
			if err != nil {
				return LegacyNormalizationStep{}, err
			}
			normalizer.position = max(normalizer.position, normalizer.native.object.progressPosition())
			if !complete {
				return result, nil
			}
			normalizer.native, normalizer.nativeCache = nil, normalizer.native
			err = normalizer.finishEvent(ctx, event, &result)
			normalizer.nativeCache.releaseValues()
			if err != nil {
				return LegacyNormalizationStep{}, err
			}
			events++
			if events == LegacyNormalizationStepEvents || input.semanticBytes >= ConversationManifestChunkBytes {
				return result, nil
			}
			continue
		}
		if normalizer.position == normalizer.source.Bytes {
			break
		}
		if (normalizer.position-start)%canonicalCheckBytes == 0 {
			if err := ctx.Err(); err != nil {
				return LegacyNormalizationStep{}, err
			}
		}
		current, err := input.current(normalizer.position)
		if errors.Is(err, errLegacyNormalizationYield) {
			return result, nil
		}
		if err != nil {
			return LegacyNormalizationStep{}, err
		}
		switch normalizer.phase {
		case legacyNormalizationStart:
			if legacyNormalizationSpace(current) {
				normalizer.position++
			} else if current == '{' {
				normalizer.position++
				normalizer.phase = legacyNormalizationRootFirst
				return result, nil
			} else if current == 'n' {
				if err := normalizer.beginValue(0, legacyNormalizationRootNull); err != nil {
					return LegacyNormalizationStep{}, err
				}
			} else {
				return LegacyNormalizationStep{}, ErrIntegrity
			}
		case legacyNormalizationRootFirst, legacyNormalizationRootNext:
			if legacyNormalizationSpace(current) {
				normalizer.position++
			} else if current == '}' {
				normalizer.position++
				normalizer.phase = legacyNormalizationTrailing
				return result, nil
			} else if normalizer.phase == legacyNormalizationRootNext {
				if current != ',' {
					return LegacyNormalizationStep{}, ErrIntegrity
				}
				normalizer.position++
				normalizer.keyBytes = 0
				if err := normalizer.beginValue(1, legacyNormalizationKey); err != nil {
					return LegacyNormalizationStep{}, err
				}
			} else {
				normalizer.keyBytes = 0
				if err := normalizer.beginValue(1, legacyNormalizationKey); err != nil {
					return LegacyNormalizationStep{}, err
				}
			}
		case legacyNormalizationColon:
			if legacyNormalizationSpace(current) {
				normalizer.position++
			} else if current == ':' {
				normalizer.position++
				normalizer.phase = legacyNormalizationValue
			} else {
				return LegacyNormalizationStep{}, ErrIntegrity
			}
		case legacyNormalizationValue:
			if legacyNormalizationSpace(current) {
				normalizer.position++
				continue
			}
			phase := uint8(legacyNormalizationIgnored)
			if normalizer.field == legacyNormalizationEnvelopeField {
				phase = legacyNormalizationEnvelope
			} else if normalizer.field == legacyNormalizationEventsField {
				if current == '[' {
					normalizer.position++
					normalizer.count, normalizer.requiresV2, normalizer.invalid = 0, false, false
					normalizer.phase, result.Reset = legacyNormalizationEventsFirst, true
					return result, nil
				}
				if current != 'n' {
					return LegacyNormalizationStep{}, ErrIntegrity
				}
				phase = legacyNormalizationEventsNull
			}
			if err := normalizer.beginValue(1, phase); err != nil {
				return LegacyNormalizationStep{}, err
			}
		case legacyNormalizationEventsFirst, legacyNormalizationEventsNext, legacyNormalizationEventRequired:
			if legacyNormalizationSpace(current) {
				normalizer.position++
			} else if current == ']' && normalizer.phase != legacyNormalizationEventRequired {
				normalizer.position++
				normalizer.phase = legacyNormalizationRootNext
				return result, nil
			} else if normalizer.phase == legacyNormalizationEventsNext {
				if current != ',' {
					return LegacyNormalizationStep{}, ErrIntegrity
				}
				normalizer.position++
				normalizer.phase = legacyNormalizationEventRequired
			} else if current == '{' {
				if err := normalizer.beginNativeEvent(normalizer.position, normalizer.source.Bytes, true); err != nil {
					return LegacyNormalizationStep{}, err
				}
			} else if err := normalizer.beginValue(2, legacyNormalizationEvent); err != nil {
				return LegacyNormalizationStep{}, err
			}
		case legacyNormalizationKey, legacyNormalizationRootNull, legacyNormalizationIgnored,
			legacyNormalizationEnvelope, legacyNormalizationEventsNull, legacyNormalizationEvent:
			if normalizer.phase == legacyNormalizationKey && normalizer.keyBytes == 0 {
				if legacyNormalizationSpace(current) {
					normalizer.position++
					continue
				}
				if current != '"' {
					return LegacyNormalizationStep{}, ErrIntegrity
				}
			}
			consumed, complete, err := normalizer.frame.add(current)
			if err != nil {
				return LegacyNormalizationStep{}, err
			}
			if consumed {
				if normalizer.phase == legacyNormalizationKey && normalizer.keyBytes <= len(normalizer.key) {
					if normalizer.keyBytes < len(normalizer.key) {
						normalizer.key[normalizer.keyBytes] = current
					}
					normalizer.keyBytes++
				}
				normalizer.position++
			}
			if complete {
				phase := normalizer.phase
				if err := normalizer.endValue(ctx, input, &result); err != nil {
					return LegacyNormalizationStep{}, err
				}
				if phase == legacyNormalizationRootNull {
					return result, nil
				}
			} else if !consumed {
				return LegacyNormalizationStep{}, ErrIntegrity
			}
		case legacyNormalizationTrailing:
			if !legacyNormalizationSpace(current) {
				return LegacyNormalizationStep{}, ErrIntegrity
			}
			normalizer.position++
		default:
			return LegacyNormalizationStep{}, ErrIntegrity
		}
	}
	if normalizer.native != nil || normalizer.envelopeActive {
		return result, nil
	}
	if normalizer.position == normalizer.source.Bytes {
		if normalizer.phase != legacyNormalizationTrailing {
			return LegacyNormalizationStep{}, ErrIntegrity
		}
		return normalizer.finish(ctx)
	}
	return result, nil
}

func (normalizer *LegacyNormalizationDecoder) finish(ctx context.Context) (LegacyNormalizationStep, error) {
	if err := legacySourceEOF(ctx, normalizer.source.Read, normalizer.source.Bytes); err != nil {
		return LegacyNormalizationStep{}, errors.Join(ErrPreparedDocRangeObject, err)
	}
	if normalizer.invalid || ValidateCIRVersion(CIRDocument{Envelope: normalizer.envelope}) != nil ||
		normalizer.requiresV2 && normalizer.envelope.CIRVersion != CIRVersionV2 {
		return LegacyNormalizationStep{}, ErrIntegrity
	}
	var heap docRangeHeaderHeapEstimate
	if normalizer.envelopeNative != nil {
		for _, field := range normalizer.envelopeNative.heaps {
			heap.include(field)
		}
	}
	encoding, err := newLegacyEnvelopeEncoding(ctx, normalizer.envelope, heap)
	if err != nil {
		return LegacyNormalizationStep{}, err
	}
	native := normalizer.envelopeNative
	if native != nil {
		native.destination = reflect.ValueOf(&encoding.envelope)
		native.lists, native.raw = nil, nil
	}
	normalizer.finalizing = &legacyNormalizationHeader{encoding: encoding, native: native}
	normalizer.phase = legacyNormalizationFinalizing
	normalizer.envelope = CIREnvelope{}
	normalizer.native, normalizer.nativeCache = nil, nil
	normalizer.envelopeNative, normalizer.envelopeActive = nil, false
	return LegacyNormalizationStep{}, nil
}

func (normalizer *LegacyNormalizationDecoder) finishHeader(ctx context.Context) (LegacyNormalizationStep, error) {
	output, complete, err := normalizer.finalizing.step(ctx)
	if err != nil || !complete {
		return LegacyNormalizationStep{}, err
	}
	if err := legacySourceEOF(ctx, normalizer.source.Read, normalizer.source.Bytes); err != nil {
		return LegacyNormalizationStep{}, errors.Join(ErrPreparedDocRangeObject, err)
	}
	normalizer.header, normalizer.phase = normalizer.finalizing.header, legacyNormalizationComplete
	normalizer.finalizing, normalizer.source = nil, LegacyByteSource{}
	normalizer.sourceHeap = docRangeHeaderHeapEstimate{}
	return LegacyNormalizationStep{Complete: true, Envelope: output, Count: normalizer.count}, nil
}

func (normalizer *LegacyNormalizationDecoder) Step(ctx context.Context) (result LegacyNormalizationStep, failure error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if normalizer == nil {
		return result, ErrIntegrity
	}
	if err := normalizer.mutation.enter(); err != nil {
		return result, err
	}
	defer normalizer.mutation.leave()
	if normalizer.terminal != nil {
		return result, normalizer.terminal
	}
	wasComplete := normalizer.phase == legacyNormalizationComplete
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result = LegacyNormalizationStep{}
			if !wasComplete {
				normalizer.terminal = failure
				normalizer.envelope, normalizer.header = CIREnvelope{}, nil
				normalizer.envelopeNative, normalizer.envelopeActive = nil, false
				normalizer.finalizing = nil
				normalizer.native = nil
				if normalizer.nativeCache != nil {
					normalizer.nativeCache.releaseValues()
				}
			}
		}
	}()
	if normalizer.phase == legacyNormalizationComplete {
		return LegacyNormalizationStep{Complete: true, Envelope: bytes.Clone(normalizer.header), Count: normalizer.count}, nil
	}
	if normalizer.phase == legacyNormalizationFinalizing {
		return normalizer.finishHeader(ctx)
	}
	if normalizer.phase == legacyNormalizationReject {
		if normalizer.rejectPart < len(normalizer.source.Parts) {
			_, err := readLegacyByteSourcePart(ctx, normalizer.source, normalizer.rejectPart)
			if err != nil {
				return result, errors.Join(ErrPreparedDocRangeObject, err)
			}
			normalizer.rejectPart++
			return result, nil
		}
		if err := legacySourceEOF(ctx, normalizer.source.Read, normalizer.source.Bytes); err != nil {
			return result, errors.Join(ErrPreparedDocRangeObject, err)
		}
		return result, ErrIntegrity
	}
	input := &legacyNormalizationInput{ctx: ctx, source: normalizer.source, numberDiagnostics: legacyNativeNumberDiagnosticsRejectOnly}
	result, err := normalizer.scan(ctx, input)
	if input.failure != nil {
		return LegacyNormalizationStep{}, input.failure
	}
	if err != nil {
		if errors.Is(err, ErrPreparedDocRangeObject) {
			return LegacyNormalizationStep{}, err
		}
		normalizer.reject()
		return LegacyNormalizationStep{}, nil
	}
	return result, nil
}

func (normalizer *LegacyNormalizationDecoder) RetainedBytes() (int64, error) {
	if normalizer == nil {
		return 0, ErrIntegrity
	}
	estimate := docRangeHeaderHeapEstimate{}
	if normalizer.finalizing != nil {
		retained, err := normalizer.finalizing.retained()
		if err != nil {
			return 0, err
		}
		estimate.include(docRangeHeaderHeapEstimate{bytes: retained})
	}
	if normalizer.envelopeNative != nil {
		retained, err := normalizer.envelopeNative.retained()
		if err != nil {
			return 0, err
		}
		estimate.include(docRangeHeaderHeapEstimate{bytes: retained})
	}
	if normalizer.nativeCache != nil {
		retained, err := normalizer.nativeCache.retained()
		if err != nil {
			return 0, err
		}
		estimate.include(docRangeHeaderHeapEstimate{bytes: retained})
	}
	estimate.allocation(1, int64(unsafe.Sizeof(*normalizer)))
	if normalizer.mutation != nil {
		estimate.allocation(1, int64(unsafe.Sizeof(*normalizer.mutation)))
	}
	estimate.allocation(int64(cap(normalizer.header)), 1)
	estimate.allocation(int64(cap(normalizer.source.Parts)), int64(unsafe.Sizeof(ConversationManifestChunk{})))
	if normalizer.sourceHeap.err != nil {
		return 0, normalizer.sourceHeap.err
	}
	estimate.include(normalizer.sourceHeap)
	return estimate.bytes, estimate.err
}
