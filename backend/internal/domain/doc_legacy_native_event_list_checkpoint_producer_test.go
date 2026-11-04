package domain

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type legacyEventListCheckpointProducerWork interface {
	legacyNativeAnyW78Work
	take(context.Context) (legacyNativeCheckpointRef, error)
}

type legacyEventListCheckpointProducerBounded struct {
	legacyNativeAnyW78Work
	test    *testing.T
	decoder *LegacyNormalizationDecoder
	storage *legacyNativeAnyW78CAS
}

func (bounded *legacyEventListCheckpointProducerBounded) step(ctx context.Context) (bool, error) {
	bounded.test.Helper()
	if _, err := bounded.decoder.Step(context.Background()); !errors.Is(err, ErrLegacyNormalizationBusy) {
		bounded.test.Fatal("producer did not freeze its actual decoder", err)
	}
	bounded.storage.quantum = 0
	ready, err := bounded.legacyNativeAnyW78Work.step(ctx)
	if bounded.storage.quantum > ConversationManifestChunkBytes {
		bounded.test.Fatal("producer exceeded its checkpoint IO quantum", bounded.storage.quantum)
	}
	if err == nil {
		if _, err := bounded.decoder.Step(context.Background()); !errors.Is(err, ErrLegacyNormalizationBusy) {
			bounded.test.Fatal("producer released its actual decoder before take", err)
		}
	}
	return ready, err
}

func legacyEventListCheckpointProducerTake(test *testing.T, decoder *LegacyNormalizationDecoder, storage *legacyNativeAnyW78CAS, work legacyEventListCheckpointProducerWork) legacyNativeCheckpointRef {
	test.Helper()
	capture := decoder.mutation.capture.Load()
	if capture == nil {
		test.Fatal("producer did not acquire its decoder capture")
	}
	bounded := &legacyEventListCheckpointProducerBounded{legacyNativeAnyW78Work: work, test: test, decoder: decoder, storage: storage}
	legacyDecoderCheckpointDrain(test, bounded, legacyDecoderCheckpointBudget)
	root, err := work.take(context.Background())
	if err != nil || !root.valid() {
		test.Fatal("producer did not hand off a valid checkpoint root", err)
	}
	if capture.references.Load() != 0 || decoder.mutation.capture.Load() != nil || decoder.terminal != nil {
		test.Fatal("producer take retained a capture grant or poisoned its decoder")
	}
	legacyNormalizationCaptureRQ87Idle(test, decoder.mutation)
	legacyNativeAnyW78Released(test, work)
	return root
}

func legacyEventListCheckpointProducerObject(test *testing.T, native *legacyNativeObject, root legacyNativeCheckpointRef, binding ContentHash, sourceBytes int, store legacyNativeCheckpointIO) legacyNativeObjectCheckpointControl {
	test.Helper()
	var state legacyNativeObjectCheckpointControl
	if err := legacyNativeCheckpointReadJSON(context.Background(), store, root, &state); err != nil {
		test.Fatal("producer object reference did not resolve", err)
	}
	if err := state.validate(legacyNativeCheckpointEventModel, binding, sourceBytes, true); err != nil {
		test.Fatal("producer emitted an invalid typed Event object", err)
	}
	version := uint8(3)
	if native.events != nil {
		version = 4
	}
	if state.Version != version || state.HasEventCache != (native.events != nil) || state.EventActive != native.eventActive ||
		state.Position != native.position || state.End != native.end || state.Phase != native.phase || state.Field != native.field || state.ParentDepth != native.parentDepth {
		test.Fatal("producer changed the actual Event object control", state)
	}
	return state
}

func legacyEventListCheckpointProducerLists(test *testing.T, decoder *LegacyNormalizationDecoder, root legacyNativeCheckpointRef, binding ContentHash, store legacyNativeCheckpointIO) []legacyNativeEventListCheckpointControl {
	test.Helper()
	object := legacyEventListCheckpointProducerObject(test, decoder.native.object, root, binding, decoder.source.Bytes, store)
	list, field := legacyEventValuesCheckpointList(test, decoder)
	if object.Version != 4 || !object.HasEventCache || object.Fields[field].Event == nil || !object.Fields[field].Event.List.valid() {
		test.Fatal("producer omitted its typed v4 replacement-list root")
	}
	reference := object.Fields[field].Event.List
	seen := make(map[legacyNativeCheckpointRef]bool)
	var controls []legacyNativeEventListCheckpointControl
	var prior *legacyNativeEventList
	for index := 0; list != nil; index++ {
		if index >= 10000 || seen[reference] {
			test.Fatal("producer replacement references did not form a finite child chain")
		}
		seen[reference] = true
		var state legacyNativeEventListCheckpointControl
		if err := legacyNativeCheckpointReadJSON(context.Background(), store, reference, &state); err != nil {
			test.Fatal("producer list reference did not resolve", index, err)
		}
		if err := state.validate(binding, decoder.source.Bytes, true); err != nil {
			test.Fatal("producer list control was not ready", index, err)
		}
		parent, cursor := -1, -1
		if list.parent != nil {
			if prior == nil || list.parent != prior {
				test.Fatal("actual decoder has a foreign list parent")
			}
			parent = index - 1
		}
		if list.cursor != nil {
			cursor = index + legacyNativeEventListCheckpointControlCursor(test, list)
		}
		expected, err := legacyNativeEventListCheckpointControlFor(list, binding, decoder.source.Bytes, index, parent, cursor)
		if err != nil {
			test.Fatal("actual decoder list shape was rejected", index, err)
		}
		expected.Data, expected.Frame, expected.Object = state.Data, state.Frame, state.Object
		if state != expected {
			test.Fatal("producer changed list geometry, parent or cursor", state, expected)
		}
		var data legacyNativeEventValuesCheckpointRoot
		if err := legacyNativeCheckpointReadJSON(context.Background(), store, state.Data, &data); err != nil {
			test.Fatal("producer data reference did not resolve", index, err)
		}
		if err := data.validate(binding, decoder.source.Bytes); err != nil {
			test.Fatal("producer emitted invalid replacement data", index, err)
		}
		rows, err := newLegacyNativeCheckpointListReader[legacyNativeEventValuesCheckpointEntry](data.Entries, store)
		if err != nil {
			test.Fatal(err)
		}
		entry, present, err := rows.next(context.Background())
		rows.close()
		if err != nil || !present || entry.validate(legacyNativeEventValuesEntryList, decoder.source.Bytes) != nil || entry.Nil != (list.values == nil) {
			test.Fatal("producer lost replacement nil/empty data shape", index, entry, err)
		}
		if list.values != nil {
			if entry.Count != list.values.count || entry.Length != list.values.records.length() {
				test.Fatal("producer lost replacement visible/high-water data shape", index, entry)
			}
		} else if data.Entries.Count != 1 {
			test.Fatal("null replacement retained unexpected data records")
		}
		if state.HasFrame {
			frame, err := legacyNativeCheckpointRead(context.Background(), store, state.Frame)
			expectedFrame, frameErr := list.frame.checkpoint(context.Background())
			if err != nil || frameErr != nil || !bytes.Equal(frame, expectedFrame) || state.Frame != object.Scanner {
				test.Fatal("producer changed the shared decoder frame", index, err, frameErr)
			}
		}
		controls = append(controls, state)
		child, err := legacyNativeEventListCheckpointChild(list)
		if err != nil {
			test.Fatal(err)
		}
		if state.Child == 2 {
			object = legacyEventListCheckpointProducerObject(test, list.object, state.Object, binding, decoder.source.Bytes, store)
			if object.Scanner != state.Frame {
				test.Fatal("active child object did not reference its list's shared frame")
			}
		} else if child != nil {
			test.Fatal("inactive child object retained a nested replacement list")
		}
		if child != nil {
			field := list.object.metadata.match("replacement")
			if object.Version != 4 || !object.HasEventCache || field < 0 || object.Fields[field].Event == nil || !object.Fields[field].Event.List.valid() {
				test.Fatal("active child object omitted its actual nested list root")
			}
			reference = object.Fields[field].Event.List
		}
		prior, list = list, child
	}
	return controls
}

func legacyEventListCheckpointProducerContinuation(test *testing.T, fixture legacyDecoderCheckpointFixture, decoder, baseline *LegacyNormalizationDecoder) {
	test.Helper()
	continuation := legacyDecoderCheckpointContinue
	if strings.HasSuffix(fixture.name, "-rejection") {
		continuation = legacyEventValuesCheckpointRejectedContinuation
	}
	if got, want := continuation(test, decoder), continuation(test, baseline); !reflect.DeepEqual(got, want) {
		test.Fatal("checkpoint producer changed its source decoder continuation", got, want)
	}
}

func TestLegacyNativeEventListCheckpointDecoderProducerReplacementCuts(test *testing.T) {
	for _, fixture := range legacyEventValuesCheckpointFixtures() {
		test.Run(fixture.name, func(test *testing.T) {
			decoder, buffer := legacyDecoderCheckpointCut(test, fixture)
			baseline, _ := legacyDecoderCheckpointCut(test, fixture)
			storage := legacyNativeAnyW78Store(test)
			binding, reads := HashContent(buffer.body), len(buffer.reads)
			work, err := newLegacyNormalizationDecoderCheckpoint(context.Background(), decoder, binding, storage.io(), legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal("replacement decoder producer construction failed", err)
			}
			defer work.close()
			root := legacyEventListCheckpointProducerTake(test, decoder, storage, work)
			var control legacyNormalizationDecoderControl
			if err := legacyNativeCheckpointReadJSON(context.Background(), storage.io(), root, &control); err != nil {
				test.Fatal(err)
			}
			if err := control.validate(context.Background(), binding, decoder.source, true); err != nil || !control.HasEvent || !control.Event.valid() || control.Position != decoder.position || control.Phase != decoder.phase {
				test.Fatal("decoder producer omitted its actual replacement-bearing Event", err)
			}
			legacyEventListCheckpointProducerLists(test, decoder, control.Event, binding, storage.io())
			if len(buffer.reads) != reads {
				test.Fatal("decoder producer replayed its source input")
			}
			legacyEventListCheckpointProducerContinuation(test, fixture, decoder, baseline)
		})
	}
}

func TestLegacyNativeEventListCheckpointObjectProducerNestedChildReferences(test *testing.T) {
	fixture := legacyEventValuesCheckpointFixtures()[1]
	fixture.name = "active-nested-replacement-cursor"
	fixture.cut = func(decoder *LegacyNormalizationDecoder) bool {
		if decoder.native != nil && decoder.native.object.eventActive {
			for _, list := range decoder.native.object.events {
				if list != nil && list.cursor != nil && list.cursor != list && list.object != nil && list.object.eventActive {
					return true
				}
			}
		}
		return false
	}
	decoder, buffer := legacyDecoderCheckpointCut(test, fixture)
	baseline, _ := legacyDecoderCheckpointCut(test, fixture)
	storage := legacyNativeAnyW78Store(test)
	binding, reads := HashContent(buffer.body), len(buffer.reads)
	work, err := newLegacyNativeObjectCheckpoint(context.Background(), decoder.native.object, &decoder.frame, binding, decoder.source, storage.io(), legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal("nested replacement object producer construction failed", err)
	}
	defer work.close()
	root := legacyEventListCheckpointProducerTake(test, decoder, storage, work)
	controls := legacyEventListCheckpointProducerLists(test, decoder, root, binding, storage.io())
	if len(controls) < 2 || controls[0].Index != 0 || controls[0].Parent != -1 || controls[0].Cursor <= 0 || controls[0].Cursor >= len(controls) || controls[1].Index != 1 || controls[1].Parent != 0 {
		test.Fatal("nested producer did not preserve the descendant cursor and child-list indices", controls)
	}
	if len(buffer.reads) != reads {
		test.Fatal("nested object producer replayed its source input")
	}
	legacyEventListCheckpointProducerContinuation(test, fixture, decoder, baseline)
}

func TestLegacyNativeEventListCheckpointObjectProducerInactiveAndNullCaches(test *testing.T) {
	fixtures := []legacyDecoderCheckpointFixture{legacyEventValuesCheckpointFixtures()[1], legacyEventListRestoreNullFixture()}
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			decoder, buffer := legacyDecoderCheckpointCut(test, fixture)
			baseline, _ := legacyDecoderCheckpointCut(test, fixture)
			storage := legacyNativeAnyW78Store(test)
			binding, reads := HashContent(buffer.body), len(buffer.reads)
			work, err := newLegacyNativeObjectCheckpoint(context.Background(), decoder.native.object, &decoder.frame, binding, decoder.source, storage.io(), legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal("inactive replacement object producer construction failed", err)
			}
			defer work.close()
			root := legacyEventListCheckpointProducerTake(test, decoder, storage, work)
			controls := legacyEventListCheckpointProducerLists(test, decoder, root, binding, storage.io())
			list, _ := legacyEventValuesCheckpointList(test, decoder)
			if len(controls) != 1 || controls[0].Phase != legacyNativeEventsDone || controls[0].Cursor != -1 || controls[0].HasValues != (list.values != nil) || controls[0].Borrowed != list.estimate.bytes {
				test.Fatal("producer lost its completed inactive replacement cache", controls)
			}
			if list.values == nil && (controls[0].Literal != len("null") || !controls[0].DecodedNil || controls[0].Child != 0 || !controls[0].HasFrame) {
				test.Fatal("producer changed its completed null-cache shape", controls[0])
			}
			if len(buffer.reads) != reads {
				test.Fatal("inactive object producer replayed its source input")
			}
			legacyEventListCheckpointProducerContinuation(test, fixture, decoder, baseline)
		})
	}
}

func TestLegacyNativeEventListCheckpointDecoderProducerCancellationReleasesNestedGrants(test *testing.T) {
	for _, ending := range []string{"cancel-active-data-child", "source-stop"} {
		test.Run(ending, func(test *testing.T) {
			fixture := legacyEventValuesCheckpointFixtures()[0]
			decoder, buffer := legacyDecoderCheckpointCut(test, fixture)
			baseline, _ := legacyDecoderCheckpointCut(test, fixture)
			storage := legacyNativeAnyW78Store(test)
			reads := len(buffer.reads)
			work, err := newLegacyNormalizationDecoderCheckpoint(context.Background(), decoder, HashContent(buffer.body), storage.io(), legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			capture := decoder.mutation.capture.Load()
			bounded := &legacyEventListCheckpointProducerBounded{legacyNativeAnyW78Work: work, test: test, decoder: decoder, storage: storage}
			active := false
			for attempt := 0; attempt < legacyDecoderCheckpointSteps; attempt++ {
				if work.child != nil && work.child.eventTask != nil && work.child.eventTask.values != nil && work.child.eventTask.values.fields != nil {
					active = true
					break
				}
				ready, err := bounded.step(context.Background())
				retained, heapErr := bounded.retainedBytes()
				if err != nil || ready || heapErr != nil || retained < 0 || retained > legacyDecoderCheckpointBudget {
					test.Fatal("producer stopped before its bounded nested data capture", err, ready, retained, heapErr)
				}
			}
			if !active || capture == nil || capture.references.Load() < 4 {
				test.Fatal("producer did not acquire nested decoder/object/list/data grants")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if ending == "source-stop" {
				decoder.mutation.stop()
			} else {
				cancel()
			}
			ready, failure := work.step(ctx)
			if ready || failure == nil || ending == "cancel-active-data-child" && !errors.Is(failure, context.Canceled) {
				test.Fatal("canceled nested producer returned readiness or lost its failure", ready, failure)
			}
			if root, err := work.take(context.Background()); root.valid() || !errors.Is(err, failure) {
				test.Fatal("failed producer handed off a root or changed its failure", root, err)
			}
			if capture.references.Load() != 0 || decoder.mutation.capture.Load() != nil || decoder.mutation.activity.Load() != 0 || decoder.terminal != nil || len(buffer.reads) != reads {
				test.Fatal("failed producer leaked a nested grant, replayed input or poisoned its source")
			}
			legacyNativeAnyW78Released(test, work)
			if ending == "source-stop" {
				_, err := decoder.Step(context.Background())
				legacyNormalizationCaptureRQ87Stopped(test, err)
			} else {
				legacyNormalizationCaptureRQ87Idle(test, decoder.mutation)
				legacyEventListCheckpointProducerContinuation(test, fixture, decoder, baseline)
			}
		})
	}
}
