package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func legacyCanonicalReplacementBudgetDense(count int) string {
	var raw strings.Builder
	raw.WriteByte('[')
	for position := 0; position < count; position++ {
		if position != 0 {
			raw.WriteByte(',')
		}
		sequence := (count-position-1)/3 - count/6
		identifier := fmt.Sprintf("dense-%05d", position)
		switch position % 8 {
		case 0:
			fmt.Fprintf(&raw, `{"kind":"message","id":%q,"seq":%d,"role":"assistant","blocks":[{"type":"text","text":%q},{"type":"text","text":"tail"}]}`,
				identifier, sequence, fmt.Sprintf("replacement-%05d <&> \ud55c\uad6d🙂", position))
		case 1:
			fmt.Fprintf(&raw, `{"kind":"tool_call","id":%q,"seq":%d,"call_id":%q,"tool_name":"tool","input":{"z":%d,"a":{"z":true,"a":[null,"<&>"]}}}`,
				identifier, sequence, identifier, position)
		case 2:
			fmt.Fprintf(&raw, "{\"kind\":\"tool_result\",\"id\":%q,\"seq\":%d,\"call_id\":%q,\"output\":{\"z\":%d,\"a\":[\"\ud55c\uad6d🙂\",false]}}",
				identifier, sequence, identifier, position)
		default:
			fmt.Fprintf(&raw, `{"kind":"turn","id":%q,"seq":%d,"role":"user"}`, identifier, sequence)
		}
	}
	raw.WriteByte(']')
	return raw.String()
}

func legacyCanonicalReplacementBudgetNested(dense string) string {
	var raw strings.Builder
	raw.WriteString(`{"kind":"compaction","id":"outer","seq":701,"replacement_complete":true,"replacement":[`)
	raw.WriteString(`{"kind":"turn","id":"outer-high","seq":9,"role":"assistant"},`)
	raw.WriteString(`{"kind":"compaction","id":"selected-middle","seq":-3,"replacement_complete":true,"replacement":[`)
	for position := 0; position < 65; position++ {
		if position != 0 {
			raw.WriteByte(',')
		}
		if position == 32 {
			fmt.Fprintf(&raw, `{"kind":"compaction","id":"selected-dense","seq":-1,"replacement_complete":true,"replacement":%s}`, dense)
		} else {
			fmt.Fprintf(&raw, `{"kind":"turn","id":"middle-%02d","seq":%d,"role":"user"}`, position, (64-position)/2-16)
		}
	}
	raw.WriteString(`]},{"kind":"turn","id":"outer-low","seq":-10,"role":"user"}]}`)
	return raw.String()
}

func legacyCanonicalReplacementBudgetSelect(test *testing.T, event CIREvent, path []int) []CIREvent {
	test.Helper()
	for _, position := range path {
		if position < 0 || position >= len(event.Replacement) {
			test.Fatal("replacement budget fixture lost its selected nested branch", path, position)
		}
		event = event.Replacement[position]
	}
	return event.Replacement
}

func legacyCanonicalReplacementBudgetOrderUnchanged(actual, original CIREvent) bool {
	if actual.Kind != original.Kind || actual.ID != original.ID || actual.Seq != original.Seq ||
		len(actual.Replacement) != len(original.Replacement) || (actual.Replacement == nil) != (original.Replacement == nil) {
		return false
	}
	for position := range actual.Replacement {
		if !legacyCanonicalReplacementBudgetOrderUnchanged(actual.Replacement[position], original.Replacement[position]) {
			return false
		}
	}
	return true
}

func legacyCanonicalReplacementBudgetFrame(preparation *legacyCanonicalPreparation, source []CIREvent) *legacyCanonicalPreparationFrame {
	for position := 0; position < preparation.frames.length(); position++ {
		frame := preparation.frames.at(position)
		replacement, ok := frame.value.(*legacyCanonicalReplacement)
		if ok && replacement != nil && frame.kind == 'r' && len(replacement.events) == len(source) &&
			len(source) != 0 && &replacement.events[0] == &source[0] {
			return frame
		}
	}
	return nil
}

func TestLegacyCanonicalReplacementBudgetActualNativeBoundedNext(test *testing.T) {
	if LegacyCanonicalEncodingBytes != 64<<10 || legacyCanonicalEncodingActions != 1024 {
		test.Fatal("replacement preparation changed the original byte/action quantum")
	}
	dense := legacyCanonicalReplacementBudgetDense(4097)
	for _, fixture := range []struct {
		name string
		raw  string
		path []int
	}{
		{name: "dense-4097", raw: `{"kind":"compaction","id":"root","seq":701,"replacement_complete":true,"replacement":` + dense + `}`},
		{name: "selected-nested-4097", raw: legacyCanonicalReplacementBudgetNested(dense), path: []int{1, 32}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			if len(fixture.raw) > ConversationManifestChunkBytes {
				test.Fatal("replacement preparation fixture exceeds the original native decode byte quantum", len(fixture.raw))
			}
			var original CIREvent
			if err := json.Unmarshal([]byte(fixture.raw), &original); err != nil {
				test.Fatal("original decoder rejected the replacement budget fixture", err)
			}
			want := legacyCanonicalNativePreparationOracle(test, canonicalEvents([]CIREvent{original})[0])
			if len(want) <= 64<<10 || bytes.Equal(want, legacyCanonicalNativePreparationOracle(test, original)) {
				test.Fatal("replacement budget fixture does not require large, genuinely reordered output")
			}
			decoded := legacyNativeCanonicalOwnerStart(test, fixture.raw+`,{"kind":"turn","id":"unread-suffix","seq":702}`)
			decoded.source = legacyNormalizationPreparationInput(test, decoded.buffer, true)
			sourceBytes := bytes.Clone(decoded.buffer.body)
			event := decoded.drain(test)
			if !reflect.DeepEqual(event, original) || decoded.native.object.position != len(fixture.raw) ||
				!bytes.Equal(decoded.buffer.body, sourceBytes) {
				test.Fatal("actual native prefix decoding changed replacement values, order, or source")
			}
			selected := legacyCanonicalReplacementBudgetSelect(test, event, fixture.path)
			if len(selected) != 4097 {
				test.Fatal("replacement budget fixture lost its dense selected list", len(selected))
			}
			reads := len(decoded.buffer.reads)
			guard := &legacyNativeCanonicalOwnerContext{Context: context.Background(), limit: len(legacyEventNativeFields.fields) + 3}
			owner, err := decoded.native.takeCanonicalOwner(guard)
			if err != nil || owner == nil {
				test.Fatal("actual replacement owner transfer exceeded the original fixed-field guard", err, guard.calls)
			}
			defer owner.release()
			encoding, err := newLegacyCanonicalEncodingNative(guard, owner)
			if err != nil || encoding == nil {
				test.Fatal("actual replacement constructor exceeded the fixed-field guard", err, guard.calls)
			}
			defer encoding.release()
			if encoding.nativeOwner != owner || encoding.preparation == nil || encoding.preparation.complete ||
				encoding.preparation.frames.length() != 1 || encoding.inspectWork != 0 || encoding.keyWork != 0 ||
				encoding.mapKeys.length() != 0 || encoding.frames.length() != 0 || encoding.numbers.pending.length() != 0 {
				test.Fatal("replacement constructor eagerly inspected, ordered, or projected children")
			}
			initial := encoding.preparation.frames.at(0)
			root, ok := encoding.root.(map[string]any)
			if !ok || initial.kind != 0 || initial.phase != 0 || initial.depth != 2 || !initial.prepare {
				test.Fatal("replacement constructor lost its single unvisited preparation root")
			}
			replacement, ok := root["replacement"].(*legacyCanonicalReplacement)
			if !ok || replacement == nil || len(replacement.events) != len(event.Replacement) ||
				&replacement.events[0] != &event.Replacement[0] || replacement.order != (legacyCanonicalKeyOrder{}) {
				test.Fatal("replacement constructor copied, sorted, or recursively projected the original list")
			}
			if guard.calls > guard.limit || len(decoded.buffer.reads) != reads || !reflect.DeepEqual(owner.event, original) ||
				!reflect.DeepEqual(event, original) || !bytes.Equal(decoded.buffer.body, sourceBytes) {
				test.Fatal("owner transfer or constructor walked source or changed replacement order")
			}
			var actual []byte
			preparationSteps, projectionSteps, orderingSteps, projected := 0, 0, 0, 0
			partialProjection, prepared, finished := false, false, false
			for stepIndex := 0; stepIndex < 1<<16; stepIndex++ {
				preparation := encoding.preparation
				beforeInspect, beforeKeys := encoding.inspectWork, encoding.keyWork
				beforeNumber, beforePosition := encoding.numbers.next, encoding.numbers.position
				sorting := false
				if preparation != nil {
					frame := legacyCanonicalReplacementBudgetFrame(preparation, selected)
					sorting = frame != nil && frame.phase == 1
				}
				chunk, complete, err := encoding.Next(context.Background())
				if err != nil || len(chunk) > 64<<10 || cap(chunk) > 64<<10 || encoding.inspectWork < beforeInspect ||
					encoding.keyWork < beforeKeys || encoding.lastStorageBytes < 0 || encoding.lastStorageBytes > 64<<10 {
					test.Fatal("replacement Next failed or exceeded its original output/storage quantum", stepIndex, err, len(chunk), cap(chunk), encoding.lastStorageBytes)
				}
				if preparation != nil {
					preparationSteps++
					if len(chunk) != 0 || cap(chunk) != 0 || complete || preparation.lastActions <= 0 || preparation.lastActions > 1024 ||
						preparation.lastBytes < 0 || preparation.lastBytes > 64<<10 || preparation.lastStorageBytes < 0 ||
						preparation.lastStorageBytes > preparation.lastBytes || encoding.lastStorageBytes != preparation.lastStorageBytes ||
						encoding.inspectWork-beforeInspect != uint64(preparation.lastActions) ||
						(preparation.frames.length() == 0) != preparation.complete {
						test.Fatal("replacement preparation emitted bytes or escaped its accounted Next reservation", stepIndex,
							len(chunk), complete, preparation.lastActions, preparation.lastBytes, preparation.lastStorageBytes, encoding.inspectWork-beforeInspect)
					}
					if frame := legacyCanonicalReplacementBudgetFrame(preparation, selected); frame != nil {
						keys := encoding.mapKeys.at(frame.mapPosition)
						if keys == nil || keys.fields.length() < projected || keys.fields.length() > len(selected) {
							test.Fatal("selected replacement lost incremental projection progress", stepIndex, projected)
						}
						if count := keys.fields.length(); count > projected {
							projectionSteps++
							projected = count
							partialProjection = partialProjection || count < len(selected)
						}
						sorting = sorting || frame.phase == 1
					}
					if sorting && encoding.keyWork > beforeKeys {
						orderingSteps++
					}
					if encoding.preparation == nil {
						prepared = true
						if !preparation.complete || !reflect.DeepEqual(event, original) || !reflect.DeepEqual(owner.event, original) {
							test.Fatal("finished replacement preparation changed original source values")
						}
					}
				} else if !prepared || encoding.inspectWork != beforeInspect {
					test.Fatal("replacement output preceded preparation or restarted inspection", stepIndex)
				}
				if len(decoded.buffer.reads) != reads || !bytes.Equal(decoded.buffer.body, sourceBytes) ||
					!legacyCanonicalReplacementBudgetOrderUnchanged(event, original) ||
					!complete && !legacyCanonicalReplacementBudgetOrderUnchanged(owner.event, original) {
					test.Fatal("replacement Next reread or mutated source bytes or original list order", stepIndex)
				}
				if len(chunk) > len(want)-len(actual) || !bytes.Equal(chunk, want[len(actual):len(actual)+len(chunk)]) {
					test.Fatal("replacement Next changed the original stable oracle bytes", stepIndex, len(actual))
				}
				actual = append(actual, chunk...)
				if complete {
					finished = true
					break
				}
				if encoding.nativeOwner != owner {
					test.Fatal("pending replacement Next lost its actual native owner", stepIndex)
				}
				if len(chunk) == 0 && encoding.inspectWork == beforeInspect && encoding.keyWork == beforeKeys &&
					encoding.numbers.next == beforeNumber && encoding.numbers.position == beforePosition {
					test.Fatal("pending replacement Next made no bounded preparation or streaming progress", stepIndex)
				}
			}
			if !finished || !prepared || preparationSteps < 2 || projectionSteps < 2 || orderingSteps < 2 ||
				!partialProjection || projected != len(selected) || encoding.keyWork == 0 {
				test.Fatal("large selected replacement did not split projection and ordering over finite Next calls",
					finished, prepared, preparationSteps, projectionSteps, orderingSteps, projected, partialProjection)
			}
			if !bytes.Equal(actual, want) || sha256.Sum256(actual) != sha256.Sum256(want) || !reflect.DeepEqual(event, original) ||
				encoding.nativeOwner != nil || encoding.preparation != nil || encoding.root != nil {
				test.Fatal("completed replacement changed final canonical bytes/SHA or retained its live source")
			}
			work, keyWork := encoding.inspectWork, encoding.keyWork
			chunk, complete, err := encoding.Next(context.Background())
			if err != nil || !complete || len(chunk) != 0 || encoding.inspectWork != work || encoding.keyWork != keyWork ||
				len(decoded.buffer.reads) != reads || !bytes.Equal(decoded.buffer.body, sourceBytes) {
				test.Fatal("completed replacement retry changed EOF, source, or accounted work", complete, err)
			}
			test.Logf("preparation Next=%d projection Next=%d ordering Next=%d; accounted reservations, not allocator, wall-clock, or RSS bounds",
				preparationSteps, projectionSteps, orderingSteps)
		})
	}
}
