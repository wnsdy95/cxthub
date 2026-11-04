package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

type legacySourceHeaderOwnershipFixture struct {
	header *legacySourceEnvelope
	input  *legacySourceProofBuffer
	source LegacyByteSource
}

func legacySourceHeaderOwnershipStart(test *testing.T, raw []byte) *legacySourceHeaderOwnershipFixture {
	test.Helper()
	input := &legacySourceProofBuffer{body: AssembleCanonicalDoc(raw, nil)}
	header, err := newLegacySourceEnvelope(len(canonicalDocPrefix), len(canonicalDocPrefix)+len(raw), HashContent(raw))
	if err != nil || header == nil || len(input.reads) != 0 {
		test.Fatal("source envelope constructor failed or read payloads", err, len(input.reads))
	}
	return &legacySourceHeaderOwnershipFixture{header: header, input: input, source: input.input()}
}

func legacySourceHeaderOwnershipEnvelope() CIREnvelope {
	models := make([]string, 257)
	for index := range models {
		models[index] = fmt.Sprintf("model-%03d", index)
	}
	models[1] = strings.Repeat("hidden-model", 2*LegacyCanonicalEncodingBytes/len("hidden-model")+1)
	return CIREnvelope{
		CIRVersion: "2", SourceProvider: ProviderCodex, Fidelity: FidelityFull,
		SourceModel: strings.Repeat("representative", ConversationManifestChunkBytes/len("representative")+7),
		Cwd:         strings.Repeat("directory", LegacyCanonicalEncodingBytes/len("directory")+11),
		CapturedAt:  "2026-10-26T00:00:00Z", GitBranch: "main", SessionOriginID: "ownership",
		ContextTokens: 128000, OutputTokens: 1024, SourceModels: models, CompactionCount: 1,
	}
}

func legacySourceHeaderOwnershipRaw(test *testing.T, envelope CIREnvelope) []byte {
	test.Helper()
	raw, err := canonicalJSON(envelope)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func legacySourceHeaderOwnershipNativeHeap(test *testing.T, object *legacyNativeObject, capacities *l04LegacyCanonicalCapacityObservation) docRangeHeaderHeapEstimate {
	test.Helper()
	if object == nil || object.metadata != &legacyEnvelopeNativeFields || object.parentDepth != 1 || object.trackPresence || object.prefix ||
		object.phase != legacyNativeObjectDone || object.position != object.end || object.scalar != nil ||
		object.listActive || object.typeError != nil || object.number != nil || object.typedNumber != nil ||
		object.blocks != nil || object.events != nil || object.nested != nil || object.containers != nil ||
		len(object.heaps) != len(legacyEnvelopeNativeFields.fields) {
		test.Fatal("source envelope did not finish through the existing typed native decoder")
	}
	var expected docRangeHeaderHeapEstimate
	for index, field := range object.metadata.fields {
		member := object.destination.Elem().Field(field.index)
		actual, err := legacyNativeValueHeap(context.Background(), member)
		if err != nil || actual.err != nil {
			test.Fatal("actual native field heap observation failed", field.name, actual, err)
		}
		actual.include(docRangeHeaderHeapEstimate{bytes: capacities.excess(test, member, 0)})
		if object.heaps[index] != actual {
			test.Fatal("native field ledger differs from actual values and measured builder capacities", field.name, object.heaps[index], actual)
		}
		if member.Kind() == reflect.String && object.backingExcess[index] != capacities.textExcess(test, member.String()) {
			test.Fatal("native scalar excess differs from its observed backing", field.name)
		}
		if member.Kind() == reflect.Slice {
			list := object.lists[index]
			values := member.Interface().([]string)
			if list == nil || list.phase != legacyNativeStringsDone || list.previous != nil || list.scalar != nil ||
				len(list.decoded) != len(values) || cap(list.decoded) != cap(values) ||
				cap(values) != 0 && unsafe.SliceData(list.decoded) != unsafe.SliceData(values) {
				test.Fatal("native model list lost its actual typed backing before handoff")
			}
		}
		expected.include(actual)
	}
	if expected.err != nil {
		test.Fatal(expected.err)
	}
	return expected
}

func legacySourceHeaderOwnershipNativeRetained(test *testing.T, object *legacyNativeObject, values docRangeHeaderHeapEstimate) int64 {
	test.Helper()
	var expected docRangeHeaderHeapEstimate
	expected.allocation(1, int64(unsafe.Sizeof(*object)))
	if object.mutation != nil {
		expected.allocation(1, int64(unsafe.Sizeof(*object.mutation)))
	}
	expected.include(values)
	expected.allocation(int64(cap(object.key)+cap(object.raw)), 1)
	expected.allocation(int64(cap(object.heaps)), int64(unsafe.Sizeof(docRangeHeaderHeapEstimate{})))
	expected.allocation(int64(cap(object.backingExcess)), int64(unsafe.Sizeof(int64(0))))
	expected.allocation(int64(cap(object.lists)), int64(unsafe.Sizeof((*legacyNativeStringList)(nil))))
	for _, list := range object.lists {
		if list == nil {
			continue
		}
		if list.owned || list.previousOwned || list.previous != nil || list.scalar != nil || list.added != (docRangeHeaderHeapEstimate{}) {
			test.Fatal("completed model list retained a second owner or unfinished scalar")
		}
		expected.allocation(1, int64(unsafe.Sizeof(*list)))
		expected.allocation(int64(cap(list.capacities)), int64(unsafe.Sizeof(int(0))))
		expected.allocation(int64(cap(list.previousCaps)), int64(unsafe.Sizeof(int(0))))
	}
	retained, err := object.retained()
	if err != nil || expected.err != nil || retained != expected.bytes {
		test.Fatal("native retained bytes lost or duplicated actual values and decoder metadata", retained, expected, err)
	}
	return retained
}

func legacySourceHeaderOwnershipEncodingHeap(test *testing.T, encoding *LegacyEnvelopeEncoding, valueHeap docRangeHeaderHeapEstimate) int64 {
	test.Helper()
	if encoding == nil || !encoding.initialized || encoding.envelopeHeap != valueHeap {
		test.Fatal("encoder did not receive the independently measured typed value heap", valueHeap)
	}
	var expected docRangeHeaderHeapEstimate
	expected.allocation(1, int64(unsafe.Sizeof(*encoding)))
	expected.include(valueHeap)
	expected.allocation(int64(cap(encoding.fields)), int64(unsafe.Sizeof(legacyEnvelopeEncodingField{})))
	value := reflect.ValueOf(encoding.envelope)
	for _, field := range encoding.fields {
		definition := value.Type().Field(field.index)
		name, _, _ := strings.Cut(definition.Tag.Get("json"), ",")
		key, err := json.Marshal(name)
		if err != nil || field.name != name || !bytes.Equal(field.key, key) {
			test.Fatal("encoder retained metadata does not describe its actual typed field", field.name, err)
		}
		expected.allocation(int64(len(field.name)), 1)
		expected.allocation(int64(cap(field.key)), 1)
	}
	expected.allocation(int64(cap(encoding.pending)), 1)
	if encoding.scalar != nil {
		var borrowed docRangeHeaderHeapEstimate
		if text, ok := encoding.scalar.root.(string); ok {
			borrowed.allocation(int64(len(text)), 1)
		}
		if encoding.scalarBorrowed != borrowed {
			test.Fatal("scalar borrowed accounting differs from the actual source value", encoding.scalarBorrowed, borrowed)
		}
		retained, err := encoding.scalar.RetainedBytes()
		if err != nil || retained < borrowed.bytes {
			test.Fatal("actual canonical scalar retained accounting failed", retained, borrowed, err)
		}
		expected.include(docRangeHeaderHeapEstimate{bytes: retained - borrowed.bytes})
	} else if encoding.scalarBorrowed != (docRangeHeaderHeapEstimate{}) {
		test.Fatal("encoder retained a borrow after releasing its scalar")
	}
	retained, err := encoding.RetainedBytes()
	if err != nil || expected.err != nil || retained != expected.bytes {
		test.Fatal("encoder lost or duplicated actual values, metadata, pending output, or scalar ownership", retained, expected, err)
	}
	return retained
}

func legacySourceHeaderOwnershipRetained(test *testing.T, header *legacySourceEnvelope, nativeBytes, encodingBytes int64) {
	test.Helper()
	var expected docRangeHeaderHeapEstimate
	expected.allocation(1, int64(unsafe.Sizeof(*header)))
	expected.allocation(int64(len(header.expected)), 1)
	expected.allocation(int64(len(header.version)), 1)
	expected.allocation(int64(cap(header.hashState)), 1)
	if header.frame != nil {
		expected.allocation(1, int64(unsafe.Sizeof(*header.frame)))
	}
	expected.include(docRangeHeaderHeapEstimate{bytes: nativeBytes})
	expected.include(docRangeHeaderHeapEstimate{bytes: encodingBytes})
	retained, err := header.retained()
	if err != nil || expected.err != nil || retained != expected.bytes || retained > 16<<20 {
		test.Fatal("source helper lost or duplicated actual native, encoder, frame, or hash-state ownership", retained, expected, err)
	}
}

func legacySourceHeaderOwnershipSameBacking(test *testing.T, before, after CIREnvelope) {
	test.Helper()
	if !reflect.DeepEqual(before, after) || len(before.SourceModels) != len(after.SourceModels) ||
		cap(before.SourceModels) != cap(after.SourceModels) || unsafe.SliceData(before.SourceModels) != unsafe.SliceData(after.SourceModels) {
		test.Fatal("encoder cloned or changed the native envelope or model backing")
	}
	original, transferred := reflect.ValueOf(before), reflect.ValueOf(after)
	for index := 0; index < original.NumField(); index++ {
		if original.Field(index).Kind() == reflect.String &&
			unsafe.StringData(original.Field(index).String()) != unsafe.StringData(transferred.Field(index).String()) {
			test.Fatal("encoder replaced an actual native string backing", original.Type().Field(index).Name)
		}
	}
}

type legacySourceHeaderOwnershipObservation struct {
	context.Context
	test        *testing.T
	header      *legacySourceEnvelope
	capacities  l04LegacyCanonicalCapacityObservation
	values      docRangeHeaderHeapEstimate
	pendingSeen bool
}

func (observation *legacySourceHeaderOwnershipObservation) Err() error {
	observation.capacities.observe(observation.header.native)
	if encoding := observation.header.encoding; encoding != nil && len(encoding.pending) >= 4096 && !observation.pendingSeen {
		observation.pendingSeen = true
		retained := legacySourceHeaderOwnershipEncodingHeap(observation.test, encoding, observation.values)
		legacySourceHeaderOwnershipRetained(observation.test, observation.header, 0, retained)
	}
	return observation.Context.Err()
}

func (fixture *legacySourceHeaderOwnershipFixture) step(test *testing.T, ctx context.Context) (bool, error) {
	test.Helper()
	reads := len(fixture.input.reads)
	complete, err := fixture.header.step(ctx, fixture.source)
	if len(fixture.input.reads)-reads > 2 {
		test.Fatal("ownership fixture crossed the original two-load source quantum")
	}
	return complete, err
}

func legacySourceHeaderOwnershipReleased(test *testing.T, header *legacySourceEnvelope, version string) {
	test.Helper()
	if !reflect.DeepEqual(header.decoded, CIREnvelope{}) || header.native != nil || header.frame != nil ||
		header.encoding != nil || header.hashState != nil || header.expected != "" || header.version != version || header.pending != nil {
		test.Fatal("source envelope retains decoded values, work metadata, digest state, or pending work after release")
	}
	legacySourceHeaderOwnershipRetained(test, header, 0, 0)
}

func legacySourceHeaderOwnershipEncoderReleased(test *testing.T, encoding *LegacyEnvelopeEncoding) {
	test.Helper()
	if encoding != nil && (!reflect.DeepEqual(encoding.envelope, CIREnvelope{}) || encoding.fields != nil ||
		encoding.scalar != nil || encoding.pending != nil || encoding.envelopeHeap != (docRangeHeaderHeapEstimate{})) {
		test.Fatal("source release abandoned a live existing encoder owner")
	}
}

func TestLegacySourceHeaderOwnershipActualNativeEncoderHandoff(test *testing.T) {
	envelope := legacySourceHeaderOwnershipEnvelope()
	base := legacySourceHeaderOwnershipRaw(test, envelope)
	for _, fixtureCase := range []struct {
		name   string
		suffix string
		hidden bool
	}{
		{name: "canonical"},
		{name: "shortened-models-retain-hidden-capacity", suffix: `,"source_models":[null]`, hidden: true},
		{name: "null-models-release-hidden-capacity", suffix: `,"source_models":[null],"source_models":null`},
	} {
		test.Run(fixtureCase.name, func(test *testing.T) {
			raw := append(bytes.Clone(base[:len(base)-1]), []byte(fixtureCase.suffix+"}")...)
			fixture := legacySourceHeaderOwnershipStart(test, raw)
			_, strictErr := decodeCanonicalCIREnvelope(raw)
			if (strictErr == nil) != (fixtureCase.suffix == "") {
				test.Fatal("ownership fixture lost the original exact-byte strict oracle", strictErr)
			}
			var original CIREnvelope
			if err := json.Unmarshal(raw, &original); err != nil {
				test.Fatal(err)
			}
			canonical := legacySourceHeaderOwnershipRaw(test, original)
			borrowed := make([]byte, ConversationManifestChunkBytes)
			fixture.source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
				body, err := fixture.input.Read(ctx, offset, length)
				if err != nil {
					return nil, err
				}
				copy(borrowed, body)
				return borrowed[:len(body)], nil
			}
			observation := &legacySourceHeaderOwnershipObservation{test: test, header: fixture.header}
			var nativeValues CIREnvelope
			var encoderOwner *LegacyEnvelopeEncoding
			nativeSeen, encodingSeen, digestSeen := false, false, false
			parts := len(fixture.source.Parts)
			for stepIndex := 0; stepIndex < 64+32*parts; stepIndex++ {
				ctx, cancel := context.WithCancel(context.Background())
				observation.Context = ctx
				complete, err := fixture.step(test, observation)
				cancel()
				clear(borrowed)
				if object := fixture.header.native; object != nil && object.phase == legacyNativeObjectDone {
					if nativeSeen || fixture.header.encoding != nil || object.destination != reflect.ValueOf(&fixture.header.decoded) {
						test.Fatal("native completion was not an independent typed ownership boundary")
					}
					nativeSeen, nativeValues = true, fixture.header.decoded
					if !reflect.DeepEqual(nativeValues, original) {
						test.Fatal("actual source native values differ from the existing typed decoder")
					}
					if fixtureCase.hidden && (len(nativeValues.SourceModels) != 1 || cap(nativeValues.SourceModels) < len(envelope.SourceModels) ||
						nativeValues.SourceModels[:cap(nativeValues.SourceModels)][1] != envelope.SourceModels[1]) {
						test.Fatal("shortened actual native model list lost its giant hidden backing")
					}
					observation.values = legacySourceHeaderOwnershipNativeHeap(test, object, &observation.capacities)
					nativeBytes := legacySourceHeaderOwnershipNativeRetained(test, object, observation.values)
					legacySourceHeaderOwnershipRetained(test, fixture.header, nativeBytes, 0)
				}
				if encoding := fixture.header.encoding; encoding != nil {
					if !nativeSeen || fixture.header.native != nil || fixture.header.frame != nil || !reflect.DeepEqual(fixture.header.decoded, CIREnvelope{}) {
						test.Fatal("encoder handoff retained the native decoder, frame, or a second typed owner")
					}
					encodingSeen = true
					if encoderOwner != nil && encoderOwner != encoding {
						test.Fatal("source helper replaced its existing encoder during resumable ownership")
					}
					encoderOwner = encoding
					legacySourceHeaderOwnershipSameBacking(test, nativeValues, encoding.envelope)
					encodingBytes := legacySourceHeaderOwnershipEncodingHeap(test, encoding, observation.values)
					legacySourceHeaderOwnershipRetained(test, fixture.header, 0, encodingBytes)
				}
				if fixture.header.phase == legacySourceEnvelopeEOF && fixture.header.pending == nil {
					digestSeen = true
					digest, digestErr := legacySourceHashSum(context.Background(), fixture.header.hashState)
					if digestErr != nil || digest != HashContent(canonical) || len(fixture.header.hashState) == 0 {
						test.Fatal("actual existing encoder output did not reach the incremental canonical digest", digest, digestErr)
					}
					legacySourceHeaderOwnershipRetained(test, fixture.header, 0, 0)
				}
				if err != nil || complete {
					if !nativeSeen || !encodingSeen || !digestSeen || !observation.pendingSeen || observation.capacities.err != nil ||
						!errors.Is(err, strictErr) || complete != (strictErr == nil) {
						test.Fatal("source ownership path skipped actual native, encoding, pending, digest, or exact-byte validation", nativeSeen, encodingSeen, digestSeen, observation.pendingSeen, err, strictErr)
					}
					version := ""
					if complete {
						version = nativeValues.CIRVersion
					}
					legacySourceHeaderOwnershipReleased(test, fixture.header, version)
					legacySourceHeaderOwnershipEncoderReleased(test, encoderOwner)
					return
				}
			}
			test.Fatal("ownership fixture exhausted its finite per-source-part allowance")
		})
	}
}

func legacySourceHeaderOwnershipPark(test *testing.T, fixture *legacySourceHeaderOwnershipFixture, stage string) {
	test.Helper()
	for stepIndex := 0; stepIndex < 64+32*len(fixture.source.Parts); stepIndex++ {
		if stage == "native" && fixture.header.native != nil && fixture.header.native.scalar != nil &&
			fixture.header.native.scalar.text != nil && fixture.header.native.scalar.text.Len() != 0 ||
			stage == "encoder" && fixture.header.encoding != nil {
			retained, err := fixture.header.retained()
			if err != nil || retained <= int64(LegacyCanonicalEncodingBytes) {
				test.Fatal("release fixture did not retain actual large native or encoder values", stage, retained, err)
			}
			return
		}
		if complete, err := fixture.step(test, context.Background()); err != nil || complete {
			test.Fatal("release fixture terminated before its actual ownership checkpoint", stage, err)
		}
	}
	test.Fatal("release fixture exhausted its finite checkpoint allowance", stage)
}

func TestLegacySourceHeaderOwnershipCancellationAndFailureRelease(test *testing.T) {
	base := legacySourceHeaderOwnershipRaw(test, legacySourceHeaderOwnershipEnvelope())
	for _, fixtureCase := range []struct {
		name    string
		stage   string
		failure error
	}{
		{name: "native-cancellation", stage: "native", failure: context.Canceled},
		{name: "active-encoder-cancellation", stage: "encoder", failure: context.Canceled},
		{name: "pending-encoder-cancellation", stage: "encoder", failure: context.Canceled},
		{name: "native-private-io", stage: "native", failure: io.ErrClosedPipe},
		{name: "encoded-eof-private-io", stage: "encoder", failure: io.ErrClosedPipe},
		{name: "native-semantic-rejection", stage: "native", failure: ErrIntegrity},
		{name: "canonical-digest-rejection", stage: "encoder", failure: ErrIntegrity},
	} {
		test.Run(fixtureCase.name, func(test *testing.T) {
			raw := base
			if fixtureCase.name == "native-semantic-rejection" {
				raw = append(bytes.Clone(base[:len(base)-1]), []byte(`,"context_tokens":"wrong"}`)...)
			}
			fixture := legacySourceHeaderOwnershipStart(test, raw)
			if fixtureCase.name == "canonical-digest-rejection" {
				fixture.header.release()
				var err error
				fixture.header, err = newLegacySourceEnvelope(len(canonicalDocPrefix), len(canonicalDocPrefix)+len(raw), HashContent([]byte("different-header")))
				if err != nil {
					test.Fatal(err)
				}
			}
			legacySourceHeaderOwnershipPark(test, fixture, fixtureCase.stage)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			selected := context.Context(ctx)
			cancellationObserved := fixtureCase.name == "native-cancellation"
			if cancellationObserved {
				cancel()
			} else if errors.Is(fixtureCase.failure, context.Canceled) {
				selected = canonicalAdmissionContext{Context: ctx, check: func() {
					encoding := fixture.header.encoding
					if encoding != nil && (fixtureCase.name == "active-encoder-cancellation" && encoding.scalar != nil ||
						fixtureCase.name == "pending-encoder-cancellation" && len(encoding.pending) != 0) {
						cancellationObserved = true
						cancel()
					}
				}}
			}
			if errors.Is(fixtureCase.failure, io.ErrClosedPipe) {
				fixture.source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
					if fixtureCase.stage == "native" || offset == fixture.source.Bytes {
						return nil, io.ErrClosedPipe
					}
					return fixture.input.Read(ctx, offset, length)
				}
			}
			pendingRejection := false
			encoderOwner := fixture.header.encoding
			for stepIndex := 0; stepIndex < 64+32*len(fixture.source.Parts); stepIndex++ {
				if fixture.header.encoding != nil {
					encoderOwner = fixture.header.encoding
				}
				complete, err := fixture.step(test, selected)
				if fixture.header.pending != nil {
					pendingRejection = true
					if !reflect.DeepEqual(fixture.header.decoded, CIREnvelope{}) || fixture.header.native != nil || fixture.header.frame != nil ||
						fixture.header.encoding != nil || fixture.header.hashState != nil {
						test.Fatal("deferred semantic rejection retains actual native or encoder owners")
					}
					legacySourceHeaderOwnershipRetained(test, fixture.header, 0, 0)
				}
				if complete {
					test.Fatal("failed ownership work became complete", fixtureCase.name)
				}
				if err == nil {
					continue
				}
				if !errors.Is(err, fixtureCase.failure) || errors.Is(fixtureCase.failure, context.Canceled) && !cancellationObserved ||
					fixtureCase.name == "native-semantic-rejection" && !pendingRejection {
					test.Fatal("failure did not cross its intended actual ownership checkpoint", err, fixtureCase.failure, cancellationObserved, pendingRejection)
				}
				legacySourceHeaderOwnershipReleased(test, fixture.header, "")
				legacySourceHeaderOwnershipEncoderReleased(test, encoderOwner)
				reads := len(fixture.input.reads)
				complete, retryErr := fixture.step(test, context.Background())
				if complete || !errors.Is(retryErr, fixtureCase.failure) || len(fixture.input.reads) != reads {
					test.Fatal("failed ownership work resumed or reread source", complete, retryErr)
				}
				fixture.header.release()
				fixture.header.release()
				legacySourceHeaderOwnershipReleased(test, fixture.header, "")
				return
			}
			test.Fatal("ownership failure exhausted its finite per-source-part allowance")
		})
	}
}

func TestLegacySourceHeaderOwnershipExplicitReleaseDropsBothOwners(test *testing.T) {
	raw := legacySourceHeaderOwnershipRaw(test, legacySourceHeaderOwnershipEnvelope())
	for _, stage := range []string{"native", "encoder"} {
		test.Run(stage, func(test *testing.T) {
			fixture := legacySourceHeaderOwnershipStart(test, raw)
			legacySourceHeaderOwnershipPark(test, fixture, stage)
			encoding := fixture.header.encoding
			fixture.header.release()
			fixture.header.release()
			legacySourceHeaderOwnershipReleased(test, fixture.header, "")
			legacySourceHeaderOwnershipEncoderReleased(test, encoding)
		})
	}
}
