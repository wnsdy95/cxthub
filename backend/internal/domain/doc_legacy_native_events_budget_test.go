package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func legacyNativeEventsL02Step(test *testing.T, fixture *legacyNativeCanonicalOwnerFixture, ctx context.Context, remaining, fields, values int) (CIREvent, bool, *legacyNormalizationInput, error) {
	test.Helper()
	if ConversationManifestChunkBytes != 512<<10 || remaining < 0 || remaining > ConversationManifestChunkBytes {
		test.Fatal("native events changed the original source quantum", remaining, ConversationManifestChunkBytes)
	}
	readSource := fixture.source.Read
	fixture.source.Read = func(current context.Context, offset, length int) ([]byte, error) {
		if current != ctx || current.Err() != nil {
			test.Fatal("native events read with an earlier or canceled call context", offset, length, current.Err())
		}
		return readSource(current, offset, length)
	}
	defer func() { fixture.source.Read = readSource }()
	initialWork := ConversationManifestChunkBytes - remaining
	input := &legacyNormalizationInput{ctx: ctx, source: fixture.source, workBytes: initialWork, fields: fields, values: values}
	before, reads := fixture.native.object.progressPosition(), len(fixture.buffer.reads)
	event, complete, err := fixture.native.step(ctx, input, &fixture.frame)
	if input.workBytes < initialWork || input.workBytes > ConversationManifestChunkBytes || input.loads < 0 || input.loads > 2 || input.unbounded ||
		input.fields < fields || input.fields > fixture.native.object.fieldActionLimit() || input.values < values || input.values > legacyNativeAnyValueActions {
		test.Fatal("native event driver reset or exceeded the shared byte/load/field/value budget", initialWork, input.workBytes, fields, input.fields, values, input.values, input.loads)
	}
	if input.eventsRunning != nil || input.eventsPending != nil {
		test.Fatal("native event driver retained a call-local pending/running signal")
	}
	if input.failure != nil && err == nil {
		test.Fatal("native event driver hid an authenticated source failure", input.failure)
	}
	attempts := len(fixture.buffer.reads) - reads
	if attempts > 2 || input.failure == nil && attempts != input.loads || input.failure != nil && attempts != input.loads+1 {
		test.Fatal("native events bypassed the shared input's two source loads", attempts, input.loads, input.failure)
	}
	for _, read := range fixture.buffer.reads[reads:] {
		if read[0] < 0 || read[0] >= len(fixture.buffer.body) || read[0]%ConversationManifestChunkBytes != 0 ||
			read[1] != min(ConversationManifestChunkBytes, len(fixture.buffer.body)-read[0]) {
			test.Fatal("native events bypassed authenticated full source parts", read)
		}
	}
	progress := fixture.native.object.progressPosition()
	if progress < before || progress > fixture.source.Bytes || progress-before > input.workBytes-initialWork {
		test.Fatal("native events lost monotonic, byte-accounted raw progress", before, progress, input.workBytes-initialWork)
	}
	if complete != fixture.native.canonicalReady || !complete && !reflect.DeepEqual(event, CIREvent{}) {
		test.Fatal("partial native source decoding exposed a ready event", complete, fixture.native.canonicalReady)
	}
	for _, page := range input.pages {
		clear(page)
	}
	return event, complete, input, err
}

func legacyNativeEventsL02Cursor(test *testing.T, fixture *legacyNativeCanonicalOwnerFixture) (*legacyNativeEventList, *legacyNativeEventList) {
	test.Helper()
	object := fixture.native.object
	if !object.eventActive {
		return nil, nil
	}
	if object.field < 0 || object.field >= len(object.events) || object.events[object.field] == nil {
		test.Fatal("active replacement has no real native event list")
	}
	root := object.events[object.field]
	cursor := root.cursor
	if cursor == nil {
		cursor = root
	}
	for current, depth := cursor, 0; ; current, depth = current.parent, depth+1 {
		if current == nil || depth >= 10000 || current.frame != &fixture.frame || current.parentDepth < 1 || current.parentDepth > 10000 {
			test.Fatal("partial replacement lost its bounded real driver parent/cursor chain", depth)
		}
		if current == root {
			if current.parent != nil {
				test.Fatal("outer replacement acquired a foreign parent")
			}
			break
		}
		if current.parent == nil || current.parentDepth != current.parent.parentDepth+2 {
			test.Fatal("nested replacement lost its original object/array depth")
		}
	}
	return root, cursor
}

func legacyNativeEventsL02Nested(leaf string, layers int) string {
	return strings.Repeat(`{"kind":"compaction","replacement":[`, layers) + leaf + strings.Repeat(`]}`, layers)
}

func legacyNativeEventsL02Drain(test *testing.T, fixture *legacyNativeCanonicalOwnerFixture, raw string, requireDeep bool) CIREvent {
	test.Helper()
	stationary, deep := 0, false
	for quantum := 0; quantum < 1024; quantum++ {
		before := fixture.native.object.progressPosition()
		ctx, cancel := context.WithCancel(context.Background())
		event, complete, input, err := legacyNativeEventsL02Step(test, fixture, ctx, ConversationManifestChunkBytes, 0, 0)
		cancel()
		if err != nil || !bytes.Equal(fixture.buffer.body, []byte(raw)) {
			test.Fatal("native source quantum failed or mutated its original bytes", quantum, err)
		}
		if complete {
			if fixture.native.object.progressPosition() != len(raw) || requireDeep && !deep {
				test.Fatal("native source did not finish at its real end or expose a partial deep cursor", requireDeep, deep)
			}
			var physical int64
			for _, read := range fixture.buffer.reads {
				physical += int64(read[1])
			}
			test.Logf("source bytes=%d remaining decode quanta=%d total authenticated reads=%d total read bytes=%d amplification=%.2fx stationary raw-position quanta=%d; bounded calls are not a speed or durable-proof result",
				len(raw), quantum+1, len(fixture.buffer.reads), physical, float64(physical)/float64(len(raw)), stationary)
			return event
		}
		root, cursor := legacyNativeEventsL02Cursor(test, fixture)
		deep = deep || root != nil && cursor != root
		if input.workBytes == 0 && input.fields == 0 && input.values == 0 {
			test.Fatal("fresh native source quantum made no raw or accounted semantic progress", quantum, before, fixture.native.object.progressPosition(), len(fixture.buffer.reads))
		}
		if fixture.native.object.progressPosition() == before {
			stationary++
		}
	}
	test.Fatal("native source exhausted its finite 1024-call allowance without completing", fixture.native.object.progressPosition(), len(fixture.buffer.reads))
	return CIREvent{}
}

func TestLegacyNativeEventsL02BudgetLargeSources(test *testing.T) {
	child := `{"kind":"message","role":"assistant","blocks":[{"type":"text","text":"` + strings.Repeat("x", 257) + `"}]}`
	for _, sample := range []struct {
		name string
		raw  string
		deep bool
	}{
		{name: "2053-wide-children", raw: `{"kind":"compaction","replacement":[` + strings.Repeat(child+",", 2052) + child + `],"replacement_complete":true}`},
		{name: "single-child-over-two-parts", raw: `{"kind":"compaction","replacement":[{"kind":"message","role":"user","blocks":[{"type":"text","text":"` + strings.Repeat("y", 2*ConversationManifestChunkBytes+73) + `\uD83D\uDE42"}]}]}`},
		{name: "deep-64-large-child", raw: legacyNativeEventsL02Nested(`{"kind":"tool_result","output":{"text":"`+strings.Repeat("z", ConversationManifestChunkBytes+173)+`","values":[null,true,false,1]}}`, 64), deep: true},
	} {
		test.Run(sample.name, func(test *testing.T) {
			if len(sample.raw) <= ConversationManifestChunkBytes {
				test.Fatal("native event fixture does not exceed the unchanged source quantum", len(sample.raw))
			}
			var expected CIREvent
			if err := json.Unmarshal([]byte(sample.raw), &expected); err != nil {
				test.Fatal("original Go event oracle rejected the source fixture", err)
			}
			fixture := legacyNativeCanonicalOwnerStart(test, sample.raw)
			fixture.source = legacyNormalizationPreparationInput(test, fixture.buffer, true)
			if len(fixture.buffer.reads) != 0 {
				test.Fatal("native event constructor read the source")
			}
			actual := legacyNativeEventsL02Drain(test, fixture, sample.raw, sample.deep)
			if !reflect.DeepEqual(actual, expected) {
				test.Fatal("actual native event driver changed typed values, presence, or replacement order")
			}
		})
	}
}

func TestLegacyNativeEventsL02BudgetSharedFieldsAndOneByte(test *testing.T) {
	raw := `{"replacement":[{"role":"user","id":"` + strings.Repeat("i", ConversationManifestChunkBytes+73) + `","kind":"turn"}],"kind":"compaction"}`
	fixture := legacyNativeCanonicalOwnerStart(test, raw)
	fixture.source = legacyNormalizationPreparationInput(test, fixture.buffer, true)
	limit := fixture.native.object.fieldActionLimit()
	_, complete, input, err := legacyNativeEventsL02Step(test, fixture, context.Background(), ConversationManifestChunkBytes, limit-1, 0)
	if err != nil || complete || input.fields != limit {
		test.Fatal("real replacement child did not consume precisely the parent's last field action", complete, input.fields, limit, err)
	}
	root, cursor := legacyNativeEventsL02Cursor(test, fixture)
	if root == nil || cursor != root || cursor.object == nil || cursor.child.Role != RoleUser || cursor.child.ID != "" ||
		cursor.object.phase != legacyNativeObjectAfter || fixture.native.decoded.Replacement != nil {
		test.Fatal("field-limited replacement published a child or skipped its partial wire state")
	}
	position, objectPosition, frame := fixture.native.object.progressPosition(), fixture.native.object.position, fixture.frame
	for _, exhausted := range []struct {
		remaining int
		fields    int
	}{
		{fields: 0},
		{remaining: ConversationManifestChunkBytes, fields: limit},
	} {
		reads := len(fixture.buffer.reads)
		_, complete, input, err = legacyNativeEventsL02Step(test, fixture, context.Background(), exhausted.remaining, exhausted.fields, 0)
		actualRoot, actualCursor := legacyNativeEventsL02Cursor(test, fixture)
		if err != nil || complete || input.workBytes != ConversationManifestChunkBytes-exhausted.remaining || input.fields != exhausted.fields || input.values != 0 ||
			len(fixture.buffer.reads) != reads || actualRoot != root || actualCursor != cursor || fixture.frame != frame || fixture.native.object.progressPosition() != position {
			test.Fatal("exhausted shared byte/field quantum changed cursor, scanner, source reads, or values", complete, err)
		}
	}
	_, complete, input, err = legacyNativeEventsL02Step(test, fixture, context.Background(), 1, 0, 0)
	actualRoot, actualCursor := legacyNativeEventsL02Cursor(test, fixture)
	if err != nil || complete || input.workBytes != ConversationManifestChunkBytes || input.fields != 0 || input.values != 0 || actualRoot != root || actualCursor != cursor ||
		fixture.native.object.progressPosition() != position+1 || fixture.native.object.position != objectPosition || cursor.object.phase != legacyNativeObjectKey {
		test.Fatal("one remaining byte did not resume the actual child comma without replaying its parent", complete, err)
	}
	var expected CIREvent
	if err := json.Unmarshal([]byte(raw), &expected); err != nil {
		test.Fatal(err)
	}
	if actual := legacyNativeEventsL02Drain(test, fixture, raw, false); !reflect.DeepEqual(actual, expected) {
		test.Fatal("field- and byte-limited resumptions changed the original Go event")
	}
}

func TestLegacyNativeEventsL02BudgetDeepCursorValuesAndCurrentCancellation(test *testing.T) {
	leaf := `{"kind":"tool_result","output":[` + strings.Repeat("null,", 4*legacyNativeAnyValueActions) + `null]}`
	raw := legacyNativeEventsL02Nested(leaf, 8)
	if len(raw) <= ConversationManifestChunkBytes {
		test.Fatal("deep value-action source no longer spans multiple parts", len(raw))
	}
	fixture := legacyNativeCanonicalOwnerStart(test, raw)
	fixture.source = legacyNormalizationPreparationInput(test, fixture.buffer, true)
	ctx, cancel := context.WithCancel(context.Background())
	_, complete, _, err := legacyNativeEventsL02Step(test, fixture, ctx, LegacyCanonicalEncodingBytes, 0, 0)
	cancel()
	root, cursor := legacyNativeEventsL02Cursor(test, fixture)
	if err != nil || complete || root == nil || cursor == root || cursor.object == nil || !cursor.object.containerActive {
		test.Fatal("deep replacement did not yield inside its actual generic child", complete, err)
	}
	child := cursor.object
	parser := child.containers[child.field]
	if parser == nil || parser.finished || child.metadata.fields[child.field].name != "output" {
		test.Fatal("deep cursor is not the unfinished real child output parser")
	}
	position, parserPosition, frame := fixture.native.object.progressPosition(), parser.position, fixture.frame
	for _, exhausted := range []struct {
		remaining int
		fields    int
		values    int
	}{
		{fields: 0},
		{remaining: ConversationManifestChunkBytes, fields: fixture.native.object.fieldActionLimit()},
		{remaining: ConversationManifestChunkBytes, values: legacyNativeAnyValueActions},
	} {
		reads := len(fixture.buffer.reads)
		_, complete, input, stepErr := legacyNativeEventsL02Step(test, fixture, context.Background(), exhausted.remaining, exhausted.fields, exhausted.values)
		actualRoot, actualCursor := legacyNativeEventsL02Cursor(test, fixture)
		if stepErr != nil || complete || input.workBytes != ConversationManifestChunkBytes-exhausted.remaining || input.fields != exhausted.fields || input.values != exhausted.values ||
			len(fixture.buffer.reads) != reads || actualRoot != root || actualCursor != cursor || parser.position != parserPosition || fixture.frame != frame || fixture.native.object.progressPosition() != position {
			test.Fatal("deep cursor restarted its parent or advanced an exhausted shared budget", complete, stepErr)
		}
	}
	_, complete, input, err := legacyNativeEventsL02Step(test, fixture, context.Background(), ConversationManifestChunkBytes, 0, legacyNativeAnyValueActions-1)
	actualRoot, actualCursor := legacyNativeEventsL02Cursor(test, fixture)
	if err != nil || complete || input.values != legacyNativeAnyValueActions || input.fields != 0 || actualRoot != root || actualCursor != cursor ||
		child.containers[child.field] != parser || parser.finished || fixture.native.decoded.Replacement != nil {
		test.Fatal("one remaining generic value action escaped its deep child or published the replacement", complete, input.values, input.fields, err)
	}
	position, parserPosition, frame = fixture.native.object.progressPosition(), parser.position, fixture.frame
	reads := len(fixture.buffer.reads)
	canceled, stop := context.WithCancel(context.Background())
	stop()
	_, complete, input, err = legacyNativeEventsL02Step(test, fixture, canceled, ConversationManifestChunkBytes, 0, 0)
	actualRoot, actualCursor = legacyNativeEventsL02Cursor(test, fixture)
	if !errors.Is(err, context.Canceled) || complete || input.workBytes != 0 || input.fields != 0 || input.values != 0 || len(fixture.buffer.reads) != reads ||
		actualRoot != root || actualCursor != cursor || parser.position != parserPosition || fixture.frame != frame || fixture.native.object.progressPosition() != position {
		test.Fatal("current pre-canceled call changed the partial driver or reread its source", complete, err)
	}
	readSource := fixture.source.Read
	during, cancelDuring := context.WithCancel(context.Background())
	defer cancelDuring()
	triggered := false
	fixture.source.Read = func(current context.Context, offset, length int) ([]byte, error) {
		body, readErr := readSource(current, offset, length)
		triggered = true
		cancelDuring()
		return body, readErr
	}
	_, complete, _, err = legacyNativeEventsL02Step(test, fixture, during, ConversationManifestChunkBytes, 0, 0)
	fixture.source.Read = readSource
	if !triggered || !errors.Is(err, context.Canceled) || complete || len(fixture.buffer.reads) != reads+1 || !bytes.Equal(fixture.buffer.body, []byte(raw)) {
		test.Fatal("current in-read cancellation was ignored, continued reading, or changed the original source", triggered, complete, err)
	}
	var expected CIREvent
	if err := json.Unmarshal([]byte(raw), &expected); err != nil {
		test.Fatal("original Go event oracle rejected the deep value fixture", err)
	}
	if actual := legacyNativeEventsL02Drain(test, fixture, raw, true); !reflect.DeepEqual(actual, expected) {
		test.Fatal("fresh current contexts did not resume the actual deep driver with unchanged values")
	}
}

func TestLegacyNativeEventsL02BudgetOriginalDocumentDepthOracle(test *testing.T) {
	const layers = 32
	for _, totalDepth := range []int{9999, 10000, 10001} {
		test.Run(fmt.Sprintf("document-depth-%d", totalDepth), func(test *testing.T) {
			arrays := totalDepth - (3 + 2*layers)
			leaf := `{"kind":"turn","unknown":` + strings.Repeat("[", arrays) + `0` + strings.Repeat("]", arrays) + `}`
			raw := legacyNativeEventsL02Nested(leaf, layers)
			var original CIRDocument
			oracleErr := json.Unmarshal(legacyNativeEventDocument("2", raw), &original)
			if (oracleErr == nil) != (totalDepth <= 10000) {
				test.Fatal("original Go document oracle disagrees with the unchanged depth fixture", totalDepth, oracleErr)
			}
			fixture := legacyNativeCanonicalOwnerStart(test, raw)
			fixture.source = legacyNormalizationPreparationInput(test, fixture.buffer, true)
			for quantum := 0; quantum < 1024; quantum++ {
				before := fixture.native.object.progressPosition()
				event, complete, input, err := legacyNativeEventsL02Step(test, fixture, context.Background(), ConversationManifestChunkBytes, 0, 0)
				if !bytes.Equal(fixture.buffer.body, []byte(raw)) {
					test.Fatal("depth-limited native decoding mutated its original source")
				}
				if err != nil {
					if oracleErr == nil || !errors.Is(err, ErrIntegrity) || complete {
						test.Fatal("native replacement depth rejection differs from the original Go document oracle", totalDepth, err, oracleErr)
					}
					return
				}
				if complete {
					if oracleErr != nil || len(original.Events) != 1 || !reflect.DeepEqual(event, original.Events[0]) {
						test.Fatal("native replacement depth acceptance differs from the original Go document oracle", totalDepth, oracleErr)
					}
					return
				}
				if input.workBytes == 0 && input.fields == 0 && input.values == 0 {
					test.Fatal("depth cursor made no bounded progress", totalDepth, quantum, before, fixture.native.object.progressPosition())
				}
			}
			test.Fatal("depth cursor exhausted its finite allowance", totalDepth)
		})
	}
}
