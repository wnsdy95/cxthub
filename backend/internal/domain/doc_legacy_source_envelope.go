package domain

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"unsafe"
)

const (
	legacySourceEnvelopeNative uint8 = iota
	legacySourceEnvelopeInitialize
	legacySourceEnvelopeEncoding
	legacySourceEnvelopeEOF
	legacySourceEnvelopeComplete
	legacySourceEnvelopeReleased
)

type legacySourceEnvelope struct {
	from      int
	end       int
	expected  ContentHash
	decoded   CIREnvelope
	native    *legacyNativeObject
	frame     *legacyJSONValueScanner
	encoding  *LegacyEnvelopeEncoding
	hashState []byte
	version   string
	phase     uint8
	pending   error
	failure   error
}

func newLegacySourceEnvelope(from, end int, expected ContentHash) (*legacySourceEnvelope, error) {
	if from < 0 || end <= from || end > MaxFinalizedDocBytes || ValidateContentHash(expected) != nil {
		return nil, ErrIntegrity
	}
	envelope := &legacySourceEnvelope{from: from, end: end, expected: ContentHash(strings.Clone(string(expected)))}
	native, err := newLegacyNativeObject(reflect.ValueOf(&envelope.decoded), &legacyEnvelopeNativeFields, 1, false)
	if err != nil {
		return nil, err
	}
	native.start(from, end)
	envelope.native, envelope.frame = native, &legacyJSONValueScanner{}
	return envelope, nil
}

func (envelope *legacySourceEnvelope) step(ctx context.Context, source LegacyByteSource) (complete bool, failure error) {
	if envelope == nil {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		return false, ErrIntegrity
	}
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			envelope.release()
			envelope.failure = failure
			complete = false
		}
	}()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if envelope.failure != nil {
		return false, envelope.failure
	}
	if envelope.phase == legacySourceEnvelopeComplete {
		return true, nil
	}
	if source.Read == nil || source.Bytes < envelope.end || source.Bytes > MaxFinalizedDocBytes ||
		source.PartBytes != 0 && source.PartBytes != ConversationManifestChunkBytes ||
		len(source.Parts) != (source.Bytes+ConversationManifestChunkBytes-1)/ConversationManifestChunkBytes {
		return false, ErrIntegrity
	}
	switch envelope.phase {
	case legacySourceEnvelopeNative:
		input := &legacyNormalizationInput{ctx: ctx, source: source, numberDiagnostics: legacyNativeNumberDiagnosticsRejectOnly}
		done, err := envelope.native.step(ctx, input, envelope.frame)
		if input.failure != nil {
			return false, input.failure
		}
		if errors.Is(err, errLegacyNormalizationYield) {
			return false, nil
		}
		if err != nil {
			if errors.Is(err, ErrPreparedDocRangeObject) {
				return false, err
			}
			envelope.reject(ErrIntegrity)
		} else if done {
			envelope.phase = legacySourceEnvelopeInitialize
		}
		return false, nil
	case legacySourceEnvelopeInitialize:
		if envelope.decoded.CIRVersion != "" && !SupportsCIRVersion(SupportedCIRVersions(), envelope.decoded.CIRVersion) {
			envelope.reject(ErrUnsupportedCIRVersion)
			return false, nil
		}
		var heap docRangeHeaderHeapEstimate
		for _, field := range envelope.native.heaps {
			heap.include(field)
		}
		encoding, err := newLegacyEnvelopeEncoding(ctx, envelope.decoded, heap)
		if err != nil {
			envelope.reject(ErrIntegrity)
			return false, nil
		}
		envelope.version = strings.Clone(envelope.decoded.CIRVersion)
		envelope.encoding = encoding
		envelope.decoded, envelope.native, envelope.frame = CIREnvelope{}, nil, nil
		envelope.phase = legacySourceEnvelopeEncoding
		return false, nil
	case legacySourceEnvelopeEncoding:
		return false, envelope.encode(ctx)
	case legacySourceEnvelopeEOF:
		if err := legacySourceEOF(ctx, source.Read, source.Bytes); err != nil {
			return false, err
		}
		if envelope.pending != nil {
			return false, envelope.pending
		}
		digest, err := legacySourceHashSum(ctx, envelope.hashState)
		if err != nil {
			return false, err
		}
		if digest != envelope.expected {
			return false, ErrIntegrity
		}
		version := envelope.version
		envelope.release()
		envelope.version, envelope.phase = version, legacySourceEnvelopeComplete
		return true, nil
	default:
		return false, ErrIntegrity
	}
}

func (envelope *legacySourceEnvelope) encode(ctx context.Context) error {
	work, hashed := 0, 0
	for actions := 0; actions < legacyCanonicalEncodingActions; actions++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		operation, output := envelope.encoding.stepWork()
		if operation < 0 || output < 0 || output > LegacyCanonicalEncodingBytes || operation > ConversationManifestChunkBytes-output {
			envelope.reject(ErrIntegrity)
			return nil
		}
		if operation+output > ConversationManifestChunkBytes-work || output > LegacyCanonicalEncodingBytes-hashed {
			return nil
		}
		chunk, complete, err := envelope.encoding.Step(ctx)
		if err != nil || len(chunk) > output || cap(chunk) > LegacyCanonicalEncodingBytes {
			envelope.reject(ErrIntegrity)
			return nil
		}
		if len(chunk) != 0 {
			envelope.hashState, err = legacySourceHashAppend(ctx, envelope.hashState, chunk)
			if err != nil {
				envelope.reject(ErrIntegrity)
				return nil
			}
		}
		work += operation + len(chunk)
		hashed += len(chunk)
		if complete {
			envelope.encoding = nil
			envelope.phase = legacySourceEnvelopeEOF
			return nil
		}
	}
	return ctx.Err()
}

func (envelope *legacySourceEnvelope) reject(failure error) {
	envelope.release()
	envelope.pending, envelope.phase = failure, legacySourceEnvelopeEOF
}

func (envelope *legacySourceEnvelope) retained() (int64, error) {
	if envelope == nil {
		return 0, ErrIntegrity
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*envelope)))
	heap.allocation(int64(len(envelope.expected)), 1)
	heap.allocation(int64(len(envelope.version)), 1)
	heap.allocation(int64(cap(envelope.hashState)), 1)
	if envelope.frame != nil {
		heap.allocation(1, int64(unsafe.Sizeof(*envelope.frame)))
	}
	if envelope.native != nil {
		retained, err := envelope.native.retained()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if envelope.encoding != nil {
		retained, err := envelope.encoding.RetainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (envelope *legacySourceEnvelope) release() {
	if envelope == nil {
		return
	}
	if envelope.encoding != nil {
		envelope.encoding.release()
	}
	envelope.decoded, envelope.native, envelope.frame = CIREnvelope{}, nil, nil
	envelope.encoding, envelope.hashState = nil, nil
	envelope.expected, envelope.version, envelope.pending = "", "", nil
	envelope.phase = legacySourceEnvelopeReleased
}
