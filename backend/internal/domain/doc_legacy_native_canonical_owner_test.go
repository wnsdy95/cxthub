package domain

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

type legacyNativeCanonicalOwnerFixture struct {
	native *legacyNormalizationNativeEvent
	buffer *legacySourceProofBuffer
	source LegacyByteSource
	frame  legacyJSONValueScanner
}

func legacyNativeCanonicalOwnerStart(test *testing.T, raw string) *legacyNativeCanonicalOwnerFixture {
	test.Helper()
	native, err := newLegacyNormalizationNativeEvent()
	if err != nil {
		test.Fatal(err)
	}
	native.reset(0, len(raw))
	native.object.prefix = true
	buffer := &legacySourceProofBuffer{body: []byte(raw)}
	return &legacyNativeCanonicalOwnerFixture{native: native, buffer: buffer, source: buffer.input()}
}

func (fixture *legacyNativeCanonicalOwnerFixture) step(test *testing.T, ctx context.Context, fields int) (CIREvent, bool, error) {
	test.Helper()
	input := &legacyNormalizationInput{ctx: ctx, source: fixture.source, fields: fields}
	event, complete, err := fixture.native.step(ctx, input, &fixture.frame)
	if input.failure != nil {
		test.Fatal("native owner fixture lost authenticated source access", input.failure)
	}
	if input.workBytes < 0 || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.unbounded ||
		input.fields > fixture.native.object.fieldActionLimit() || input.values > legacyNativeAnyValueActions {
		test.Fatal("native owner fixture exceeded the existing decode quantum", input.workBytes, input.loads, input.fields, input.values)
	}
	for _, page := range input.pages {
		clear(page)
	}
	return event, complete, err
}

func (fixture *legacyNativeCanonicalOwnerFixture) drain(test *testing.T) CIREvent {
	test.Helper()
	for range 256 {
		event, complete, err := fixture.step(test, context.Background(), 0)
		if err != nil {
			test.Fatal("native owner fixture rejected its event", err)
		}
		if complete {
			if !fixture.native.canonicalReady {
				test.Fatal("successful native event did not become canonical ready")
			}
			return event
		}
		if fixture.native.canonicalReady {
			test.Fatal("incomplete native event became canonical ready")
		}
	}
	test.Fatal("native owner fixture exhausted its finite decode allowance")
	return CIREvent{}
}

func legacyNativeCanonicalOwnerReject(test *testing.T, native *legacyNormalizationNativeEvent, expected error) {
	test.Helper()
	owner, err := native.takeCanonicalOwner(context.Background())
	if owner != nil || !errors.Is(err, expected) {
		test.Fatal("invalid native event produced a canonical owner", owner != nil, err, expected)
	}
}

func legacyNativeCanonicalOwnerClaimReject(test *testing.T, owner *legacyNativeCanonicalOwner) {
	test.Helper()
	event, heap, err := owner.claim(context.Background())
	if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(event, CIREvent{}) || heap.bytes != 0 || heap.err != nil {
		test.Fatal("invalid canonical owner exposed an event or heap", err, heap)
	}
}

func TestLegacyNativeCanonicalOwnerReadyLifecycle(test *testing.T) {
	fixture := legacyNativeCanonicalOwnerStart(test, `{"kind":"turn","seq":1}`)
	if fixture.native.canonicalReady {
		test.Fatal("native constructor started canonical ready")
	}
	legacyNativeCanonicalOwnerReject(test, fixture.native, ErrIntegrity)
	ctx := context.Background()
	input := &legacyNormalizationInput{ctx: ctx, source: fixture.source}
	complete, err := fixture.native.object.step(ctx, input, &fixture.frame)
	if err != nil || !complete || fixture.native.canonicalReady {
		test.Fatal("object completion bypassed the native event readiness gate", complete, err)
	}
	legacyNativeCanonicalOwnerReject(test, fixture.native, ErrIntegrity)
	fixture.drain(test)
	_, complete, err = fixture.step(test, ctx, fixture.native.object.fieldActionLimit())
	if err != nil || complete || fixture.native.canonicalReady {
		test.Fatal("yielding step retained stale native readiness", complete, err)
	}
	legacyNativeCanonicalOwnerReject(test, fixture.native, ErrIntegrity)
	fixture.drain(test)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, complete, err = fixture.step(test, canceled, 0)
	if !errors.Is(err, context.Canceled) || complete || fixture.native.canonicalReady {
		test.Fatal("failed step retained stale native readiness", complete, err)
	}
	legacyNativeCanonicalOwnerReject(test, fixture.native, ErrIntegrity)
	fixture.drain(test)
	fixture.native.reset(0, fixture.source.Bytes)
	if fixture.native.canonicalReady {
		test.Fatal("reset retained native readiness")
	}
	legacyNativeCanonicalOwnerReject(test, fixture.native, ErrIntegrity)
	fixture.drain(test)
	fixture.native.releaseValues()
	if fixture.native.canonicalReady {
		test.Fatal("release retained native readiness")
	}
	legacyNativeCanonicalOwnerReject(test, fixture.native, ErrIntegrity)
}

func TestLegacyNativeCanonicalOwnerFactoryStateGuards(test *testing.T) {
	legacyNativeCanonicalOwnerReject(test, nil, ErrIntegrity)
	legacyNativeCanonicalOwnerReject(test, &legacyNormalizationNativeEvent{}, ErrIntegrity)
	for _, fixture := range []struct {
		name   string
		mutate func(*legacyNormalizationNativeEvent)
	}{
		{name: "object-missing", mutate: func(native *legacyNormalizationNativeEvent) { native.object = nil }},
		{name: "metadata-missing", mutate: func(native *legacyNormalizationNativeEvent) { native.object.metadata = nil }},
		{name: "metadata-copy", mutate: func(native *legacyNormalizationNativeEvent) {
			metadata := legacyEventNativeFields
			native.object.metadata = &metadata
		}},
		{name: "destination-missing", mutate: func(native *legacyNormalizationNativeEvent) { native.object.destination = reflect.Value{} }},
		{name: "destination-mismatch", mutate: func(native *legacyNormalizationNativeEvent) { native.object.destination = reflect.ValueOf(new(wire)) }},
		{name: "not-done", mutate: func(native *legacyNormalizationNativeEvent) { native.object.phase = legacyNativeObjectAfter }},
		{name: "past-end", mutate: func(native *legacyNormalizationNativeEvent) { native.object.position = native.object.end + 1 }},
		{name: "negative-position", mutate: func(native *legacyNormalizationNativeEvent) { native.object.position = -1 }},
		{name: "unfinished-nonprefix", mutate: func(native *legacyNormalizationNativeEvent) {
			native.object.prefix = false
			native.object.end++
		}},
		{name: "no-presence", mutate: func(native *legacyNormalizationNativeEvent) { native.object.trackPresence = false }},
		{name: "wrong-depth", mutate: func(native *legacyNormalizationNativeEvent) { native.object.parentDepth = 1 }},
		{name: "type-error", mutate: func(native *legacyNormalizationNativeEvent) { native.object.typeError = &json.UnmarshalTypeError{} }},
		{name: "list-active", mutate: func(native *legacyNormalizationNativeEvent) { native.object.listActive = true }},
		{name: "blocks-active", mutate: func(native *legacyNormalizationNativeEvent) { native.object.blockActive = true }},
		{name: "nested-active", mutate: func(native *legacyNormalizationNativeEvent) { native.object.nestedActive = true }},
		{name: "container-active", mutate: func(native *legacyNormalizationNativeEvent) { native.object.containerActive = true }},
		{name: "scalar-active", mutate: func(native *legacyNormalizationNativeEvent) { native.object.scalar = &legacyNativeScalar{} }},
		{name: "number-active", mutate: func(native *legacyNormalizationNativeEvent) { native.object.number = &legacyNativeNumberValue{} }},
		{name: "typed-number-active", mutate: func(native *legacyNormalizationNativeEvent) { native.object.typedNumber = &legacyNativeTypedNumber{} }},
		{name: "heap-count", mutate: func(native *legacyNormalizationNativeEvent) { native.object.heaps = nil }},
		{name: "excess-count", mutate: func(native *legacyNormalizationNativeEvent) { native.object.backingExcess = nil }},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			decoded := legacyNativeCanonicalOwnerStart(test, `{"kind":"turn","seq":1}`)
			decoded.drain(test)
			fixture.mutate(decoded.native)
			legacyNativeCanonicalOwnerReject(test, decoded.native, ErrIntegrity)
		})
	}
	fixture := legacyNativeCanonicalOwnerStart(test, `{"kind":"turn","seq":1}`)
	fixture.drain(test)
	copied := *fixture.native
	legacyNativeCanonicalOwnerReject(test, &copied, ErrIntegrity)
	owner, err := fixture.native.takeCanonicalOwner(context.Background())
	if err != nil || owner == nil {
		test.Fatal("copied native rejection consumed the original", err)
	}
	owner.release()
}

func TestLegacyNativeCanonicalOwnerHeapGuards(test *testing.T) {
	heapFailure := errors.New("native field heap failure")
	for _, fixture := range []struct {
		name     string
		heap     docRangeHeaderHeapEstimate
		expected error
	}{
		{name: "negative", heap: docRangeHeaderHeapEstimate{bytes: -1}, expected: ErrIntegrity},
		{name: "missing", expected: ErrIntegrity},
		{name: "below-storage", heap: docRangeHeaderHeapEstimate{bytes: 1}, expected: ErrIntegrity},
		{name: "overflow", heap: docRangeHeaderHeapEstimate{bytes: math.MaxInt64}, expected: ErrIntegrity},
		{name: "error", heap: docRangeHeaderHeapEstimate{err: heapFailure}, expected: heapFailure},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			decoded := legacyNativeCanonicalOwnerStart(test, `{"kind":"turn","seq":1}`)
			decoded.drain(test)
			decoded.native.object.heaps[legacyEventNativeFields.match("kind")] = fixture.heap
			legacyNativeCanonicalOwnerReject(test, decoded.native, fixture.expected)
			if !decoded.native.canonicalReady || decoded.native.decoded.Kind != EventTurn {
				test.Fatal("failed heap check consumed native state")
			}
		})
	}
}

type legacyNativeCanonicalOwnerContext struct {
	context.Context
	calls int
	limit int
}

func (ctx *legacyNativeCanonicalOwnerContext) Err() error {
	ctx.calls++
	if ctx.calls > ctx.limit {
		return context.Canceled
	}
	return ctx.Context.Err()
}

func TestLegacyNativeCanonicalOwnerCancellation(test *testing.T) {
	fixture := legacyNativeCanonicalOwnerStart(test, `{"kind":"turn","seq":1}`)
	fixture.drain(test)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	owner, err := fixture.native.takeCanonicalOwner(canceled)
	if owner != nil || !errors.Is(err, context.Canceled) || !fixture.native.canonicalReady {
		test.Fatal("canceled transfer consumed native ownership", owner != nil, err)
	}
	during := &legacyNativeCanonicalOwnerContext{Context: context.Background(), limit: 1}
	owner, err = fixture.native.takeCanonicalOwner(during)
	if owner != nil || !errors.Is(err, context.Canceled) || !fixture.native.canonicalReady {
		test.Fatal("transfer ignored cancellation before capture", owner != nil, err, during.calls)
	}
	owner, err = fixture.native.takeCanonicalOwner(context.Background())
	if err != nil || owner == nil {
		test.Fatal("canceled transfer could not be retried", err)
	}
	defer owner.release()
	event, heap, err := owner.claim(canceled)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(event, CIREvent{}) || heap.bytes != 0 || heap.err != nil || owner.seal != owner {
		test.Fatal("canceled claim consumed or exposed the owner", err, heap)
	}
	if _, _, err := owner.claim(context.Background()); err != nil {
		test.Fatal("canceled claim could not be retried", err)
	}
	legacyNativeCanonicalOwnerClaimReject(test, owner)
}

func TestLegacyNativeCanonicalOwnerSingleUseAndCopies(test *testing.T) {
	legacyNativeCanonicalOwnerClaimReject(test, nil)
	legacyNativeCanonicalOwnerClaimReject(test, &legacyNativeCanonicalOwner{})
	fixture := legacyNativeCanonicalOwnerStart(test, `{"kind":"tool_result","output":["payload"]}`)
	before := fixture.drain(test)
	owner, err := fixture.native.takeCanonicalOwner(context.Background())
	if err != nil || owner == nil || owner.seal != owner {
		test.Fatal("factory did not seal its owner", err)
	}
	defer owner.release()
	legacyNativeCanonicalOwnerReject(test, fixture.native, ErrIntegrity)
	copied := *owner
	legacyNativeCanonicalOwnerClaimReject(test, &copied)
	copied.release()
	event, heap, err := owner.claim(context.Background())
	if err != nil || !reflect.DeepEqual(event, before) || heap.bytes != owner.heap.bytes || heap.err != nil {
		test.Fatal("original owner lost its single claim", err, heap)
	}
	if &event.Output.([]any)[0] != &before.Output.([]any)[0] || &event.Output.([]any)[0] != &owner.event.Output.([]any)[0] {
		test.Fatal("claim copied the native array backing")
	}
	legacyNativeCanonicalOwnerClaimReject(test, owner)
	consumed := *owner
	legacyNativeCanonicalOwnerClaimReject(test, &consumed)
	owner.release()
	owner.release()
	if owner.seal != nil || !reflect.DeepEqual(owner.event, CIREvent{}) || owner.heap.bytes != 0 || owner.heap.err != nil {
		test.Fatal("release retained canonical payload or accounting")
	}
	legacyNativeCanonicalOwnerClaimReject(test, owner)
	if event.Output.([]any)[0] != "payload" {
		test.Fatal("release mutated transferred payload")
	}
	fixture = legacyNativeCanonicalOwnerStart(test, `{"kind":"turn"}`)
	fixture.drain(test)
	owner, err = fixture.native.takeCanonicalOwner(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	owner.release()
	legacyNativeCanonicalOwnerClaimReject(test, owner)
	var absent *legacyNativeCanonicalOwner
	absent.release()
}

func TestLegacyNativeCanonicalOwnerWholeHeapAndCacheRelease(test *testing.T) {
	hidden := strings.Repeat("hidden", LegacyCanonicalEncodingBytes/3+19)
	omitted := strings.Repeat("omitted", LegacyCanonicalEncodingBytes/3+23)
	raw := `{"kind":"message","role":"user","blocks":[{"type":"text","text":"first"},{"type":"text","text":"` + hidden +
		`"}],"blocks":[{"text":"changed"}],"input":{"retained":"` + omitted +
		`"},"output":{"retained":"` + omitted + `"},"provider_metadata":{"turn_id":"turn","create_time":123.450e-8},"agent_message":false}`
	fixture := legacyNativeCanonicalOwnerStart(test, raw)
	before := fixture.drain(test)
	object := fixture.native.object
	if len(before.Blocks) != 1 || cap(before.Blocks) < 2 || before.Blocks[:cap(before.Blocks)][1].Text != hidden ||
		before.Input["retained"] != omitted || before.Output.(map[string]any)["retained"] != omitted ||
		before.fieldPresence&eventFieldAgentMessage == 0 || object.blocks == nil || object.nested == nil || object.containers == nil {
		test.Fatal("whole-owner fixture lost hidden, omitted, presence, or helper state")
	}
	var expected docRangeHeaderHeapEstimate
	for index, field := range legacyEventNativeFields.fields {
		expected.include(object.heaps[index])
		if object.heaps[index].bytes == 0 {
			continue
		}
		minimum, err := legacyNativeValueHeap(context.Background(), object.destination.Elem().Field(field.index))
		if err != nil || object.heaps[index].bytes < minimum.bytes {
			test.Fatal("native field estimate omitted actual backing", field.name, object.heaps[index], minimum, err)
		}
	}
	retainedBefore, err := fixture.native.retained()
	if err != nil || expected.err != nil {
		test.Fatal("native heap fixture lost accounting", err, expected.err)
	}
	reads := len(fixture.buffer.reads)
	owner, err := fixture.native.takeCanonicalOwner(context.Background())
	if err != nil || owner == nil {
		test.Fatal("whole native event could not transfer", err)
	}
	defer owner.release()
	if len(fixture.buffer.reads) != reads || owner.heap.err != nil || owner.heap.bytes != expected.bytes {
		test.Fatal("capture reread source or did not charge exactly the field heap aggregate", owner.heap, expected)
	}
	if fixture.native.canonicalReady || !reflect.DeepEqual(fixture.native.decoded, wire{}) || object.presence != 0 ||
		object.lists != nil || object.blocks != nil || object.nested != nil || object.containers != nil ||
		object.scalar != nil || object.number != nil || object.typedNumber != nil {
		test.Fatal("native cache retained transferred payload or helper references")
	}
	for index, heap := range object.heaps {
		if heap.bytes != 0 || heap.err != nil || object.backingExcess[index] != 0 {
			test.Fatal("native cache retained transferred heap accounting", index, heap)
		}
	}
	retainedAfter, err := fixture.native.retained()
	if err != nil || retainedAfter >= retainedBefore {
		test.Fatal("native cache did not release owner-backed retention", retainedBefore, retainedAfter, err)
	}
	event, heap, err := owner.claim(context.Background())
	if err != nil || !reflect.DeepEqual(event, before) || heap.bytes != expected.bytes || heap.err != nil ||
		&event.Blocks[0] != &before.Blocks[0] || event.ProviderMetadata != before.ProviderMetadata {
		test.Fatal("owner changed native payload, presence, or backing identity", err, heap)
	}
	reused := `{"kind":"turn","seq":91}`
	fixture.buffer = &legacySourceProofBuffer{body: []byte(reused)}
	fixture.source = fixture.buffer.input()
	fixture.native.reset(0, len(reused))
	fixture.drain(test)
	fixture.native.releaseValues()
	if !reflect.DeepEqual(owner.event, before) || owner.heap.bytes != expected.bytes ||
		owner.event.Blocks[:cap(owner.event.Blocks)][1].Text != hidden || owner.event.Input["retained"] != omitted {
		test.Fatal("cache reuse or claim dropped omitted or hidden owner payload")
	}
	event.Input["retained"] = "same-map"
	if owner.event.Input["retained"] != "same-map" || before.Input["retained"] != "same-map" {
		test.Fatal("owner copied the native map")
	}
}

func TestLegacyNativeCanonicalOwnerNoPayloadWalk(test *testing.T) {
	raw := `{"kind":"tool_result","output":[` + strings.Repeat(`[1,2,3],`, 2048) + `true]}`
	fixture := legacyNativeCanonicalOwnerStart(test, raw+`,{"kind":"turn"}`)
	fixture.drain(test)
	if fixture.native.object.position != len(raw) || fixture.native.object.end <= len(raw) {
		test.Fatal("prefix fixture did not complete before the next event")
	}
	ctx := &legacyNativeCanonicalOwnerContext{Context: context.Background(), limit: len(legacyEventNativeFields.fields) + 3}
	reads := len(fixture.buffer.reads)
	owner, err := fixture.native.takeCanonicalOwner(ctx)
	if err != nil || owner == nil {
		test.Fatal("factory walked payload instead of its fixed field heap table", err, ctx.calls)
	}
	defer owner.release()
	if _, _, err := owner.claim(ctx); err != nil || len(fixture.buffer.reads) != reads {
		test.Fatal("claim walked payload or reread source", err, ctx.calls)
	}
}

func TestLegacyNativeCanonicalOwnerDefersUnionValidation(test *testing.T) {
	for _, raw := range []string{
		`null`,
		`{}`,
		`{"kind":"unknown"}`,
		`{"kind":"turn","provider_metadata":null}`,
		`{"kind":"turn","agent_message":false}`,
	} {
		fixture := legacyNativeCanonicalOwnerStart(test, raw)
		before := fixture.drain(test)
		owner, err := fixture.native.takeCanonicalOwner(context.Background())
		if err != nil || owner == nil {
			test.Fatal("factory introduced a second union validation policy", raw, err)
		}
		event, _, err := owner.claim(context.Background())
		owner.release()
		if err != nil || !reflect.DeepEqual(event, before) {
			test.Fatal("owner changed native zero values or exact presence", raw, err)
		}
	}
}

func TestLegacyNativeCanonicalOwnerReplacementTransfer(test *testing.T) {
	for _, fixture := range []struct {
		name        string
		members     string
		replacement bool
		presence    eventFieldPresence
	}{
		{name: "absent"},
		{name: "empty", members: `,"replacement":[]`, replacement: true, presence: eventFieldReplacement},
		{name: "child", members: `,"replacement":[{"kind":"turn"}]`, replacement: true, presence: eventFieldReplacement},
		{name: "null-child", members: `,"replacement":[null]`, replacement: true, presence: eventFieldReplacement},
		{name: "folded-empty", members: `,"REPLACEMENT":[]`, replacement: true},
		{name: "null", members: `,"replacement":null`, presence: eventFieldReplacement},
		{name: "folded-null", members: `,"REPLACEMENT":null`},
		{name: "duplicate-null", members: `,"replacement":[{"kind":"turn"}],"replacement":null`, presence: eventFieldReplacement},
		{name: "false-complete", members: `,"replacement_complete":false`, presence: eventFieldReplacementComplete},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			decoded := legacyNativeCanonicalOwnerStart(test, `{"kind":"compaction"`+fixture.members+`}`)
			before := decoded.drain(test)
			if (before.Replacement != nil) != fixture.replacement || before.fieldPresence != fixture.presence {
				test.Fatal("replacement fixture changed nil/empty distinction or exact presence", before.fieldPresence, fixture.presence)
			}
			owner, err := decoded.native.takeCanonicalOwner(context.Background())
			if err != nil || owner == nil {
				test.Fatal("replacement event could not transfer", err)
			}
			defer owner.release()
			if decoded.native.canonicalReady || !reflect.DeepEqual(decoded.native.decoded, wire{}) {
				test.Fatal("replacement transfer retained the native payload or readiness")
			}
			legacyNativeCanonicalOwnerReject(test, decoded.native, ErrIntegrity)
			event, _, err := owner.claim(context.Background())
			if err != nil || (event.Replacement != nil) != fixture.replacement || event.fieldPresence != fixture.presence || !reflect.DeepEqual(event, before) {
				test.Fatal("replacement transfer changed nil/empty distinction, exact presence, or native semantics", err, event.fieldPresence, fixture.presence)
			}
			if cap(event.Replacement) != cap(before.Replacement) || len(event.Replacement) != 0 && &event.Replacement[0] != &before.Replacement[0] {
				test.Fatal("replacement transfer copied the native array backing")
			}
			legacyNativeCanonicalOwnerClaimReject(test, owner)
		})
	}
}
