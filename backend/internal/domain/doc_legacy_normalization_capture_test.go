package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type legacyNormalizationCaptureRQ87State struct {
	control  legacyNativeObjectCheckpointControl
	value    []byte
	scanner  []byte
	raw      []byte
	heaps    []docRangeHeaderHeapEstimate
	excess   []int64
	scalar   []string
	children []legacyNormalizationCaptureRQ87State
}

func legacyNormalizationCaptureRQ87Observe(test *testing.T, native *legacyNativeObject, scanner *legacyJSONValueScanner, sourceBytes int) legacyNormalizationCaptureRQ87State {
	test.Helper()
	model, err := legacyNativeCheckpointModel(native.metadata)
	if err != nil {
		test.Fatal(err)
	}
	value, err := json.Marshal(native.destination.Interface())
	if err != nil {
		test.Fatal(err)
	}
	frame, err := scanner.checkpoint(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	state := legacyNormalizationCaptureRQ87State{
		control: legacyNativeObjectCheckpointControlFor(native, model, HashContent([]byte("RQ87 observation")), sourceBytes),
		value:   value, scanner: frame, raw: bytes.Clone(native.raw),
		heaps:  append([]docRangeHeaderHeapEstimate(nil), native.heaps...),
		excess: append([]int64(nil), native.backingExcess...),
	}
	state.control.Key = bytes.Clone(state.control.Key)
	if native.scalar != nil {
		state.scalar = legacyNativeAnyW78ScalarState(test, native.scalar)
	}
	children := append([]*legacyNativeObject(nil), native.nested...)
	for _, list := range native.blocks {
		if list != nil {
			children = append(children, list.object)
		}
	}
	for _, list := range native.events {
		if list != nil {
			children = append(children, list.object)
		}
	}
	for _, child := range children {
		if child != nil {
			state.children = append(state.children, legacyNormalizationCaptureRQ87Observe(test, child, scanner, sourceBytes))
		}
	}
	return state
}

func legacyNormalizationCaptureRQ87Busy(test *testing.T, err error) {
	test.Helper()
	if !errors.Is(err, ErrLegacyNormalizationBusy) || !errors.Is(err, ErrConflict) {
		test.Fatalf("capture exclusion must match busy and conflict: %v", err)
	}
}

func legacyNormalizationCaptureRQ87Stopped(test *testing.T, err error) {
	test.Helper()
	if !errors.Is(err, ErrConflict) || errors.Is(err, ErrLegacyNormalizationBusy) {
		test.Fatalf("closed owner must return non-busy conflict: %v", err)
	}
}

func legacyNormalizationCaptureRQ87Idle(test *testing.T, scope *legacyNormalizationMutation) {
	test.Helper()
	if scope == nil || scope.closed.Load() || scope.activity.Load() != 0 {
		test.Fatal("live scope did not release its final capture")
	}
	if err := scope.enter(); err != nil {
		test.Fatal("live scope did not resume mutation", err)
	}
	scope.leave()
}

func legacyNormalizationCaptureRQ87Ready(test *testing.T, ctx context.Context, work legacyNativeAnyW78Work) {
	test.Helper()
	for attempt := 0; attempt < legacyNativeNestedW83CheckpointSteps; attempt++ {
		ready, err := work.step(ctx)
		if err != nil {
			test.Fatal("checkpoint did not reach readiness", err)
		}
		if ready {
			return
		}
	}
	test.Fatal("checkpoint exceeded the finite capture schedule")
}

func legacyNormalizationCaptureRQ87Block(test *testing.T) (legacyNativeAnyW78Request, *legacyNativeObject, *legacyJSONValueScanner, LegacyByteSource) {
	test.Helper()
	fixture := legacyNativeObjectW79Fixtures()[1]
	request, source := legacyNativeAnyW78FullSource(test, fixture.raw)
	native, err := newLegacyNativeObject(reflect.ValueOf(&ContentBlock{}), &legacyContentBlockNativeFields, 1, false)
	if err != nil {
		test.Fatal(err)
	}
	if err := native.start(0, source.Bytes); err != nil {
		test.Fatal(err)
	}
	native.fieldLimit = 1
	scanner := &legacyJSONValueScanner{}
	for attempt := 0; attempt < legacyNativeObjectW79Steps; attempt++ {
		if legacyNativeObjectW79Stop(native, fixture) {
			return request, native, scanner, source
		}
		if ready, err := legacyNativeObjectW79Step(test, context.Background(), native, scanner, source, legacyNativeNumberDiagnosticsFull, true); err != nil || ready {
			test.Fatal("block did not reach its actual scalar prefix", ready, err)
		}
	}
	test.Fatal("block prefix exceeded the finite source schedule")
	return request, native, scanner, source
}

func legacyNormalizationCaptureRQ87Blocked(test *testing.T, native *legacyNativeObject, event *legacyNormalizationNativeEvent, scanner *legacyJSONValueScanner, source LegacyByteSource) {
	test.Helper()
	before := legacyNormalizationCaptureRQ87Observe(test, native, scanner, source.Bytes)
	canonicalReady := event != nil && event.canonicalReady
	unchanged := func() {
		if !reflect.DeepEqual(before, legacyNormalizationCaptureRQ87Observe(test, native, scanner, source.Bytes)) || event != nil && event.canonicalReady != canonicalReady {
			test.Fatal("busy mutation changed captured parser, scanner, destination, or wrapper")
		}
	}
	legacyNormalizationCaptureRQ87Busy(test, native.start(source.Bytes/2, source.Bytes))
	unchanged()
	legacyNormalizationCaptureRQ87Busy(test, native.releaseValues())
	unchanged()
	input := &legacyNormalizationInput{ctx: context.Background(), source: source}
	ready, err := native.step(input.ctx, input, scanner)
	legacyNormalizationCaptureRQ87Busy(test, err)
	if ready || input.loads != 0 || input.workBytes != 0 || input.fields != 0 || input.failure != nil {
		test.Fatal("busy native step consumed input or reported progress")
	}
	unchanged()
	if event != nil {
		legacyNormalizationCaptureRQ87Busy(test, event.reset(source.Bytes/2, source.Bytes))
		unchanged()
		legacyNormalizationCaptureRQ87Busy(test, event.releaseValues())
		unchanged()
		input = &legacyNormalizationInput{ctx: context.Background(), source: source}
		value, ready, err := event.step(input.ctx, input, scanner)
		legacyNormalizationCaptureRQ87Busy(test, err)
		if ready || !reflect.DeepEqual(value, CIREvent{}) || input.loads != 0 || input.workBytes != 0 {
			test.Fatal("busy Event step consumed input or exposed a value")
		}
		unchanged()
	}
}

func legacyNormalizationCaptureRQ87Resume(test *testing.T, native *legacyNativeObject, event *legacyNormalizationNativeEvent) legacyJSONValueScanner {
	test.Helper()
	legacyNormalizationCaptureRQ87Idle(test, native.mutation)
	raw := `{"type":"text","text":"fresh input after capture"}`
	if event != nil {
		raw = `{"kind":"message","seq":91,"role":"user","id":"fresh-input"}`
	}
	_, source := legacyNativeAnyW78FullSource(test, raw)
	if event != nil {
		if err := event.reset(0, source.Bytes); err != nil {
			test.Fatal("Event reset did not resume", err)
		}
	} else {
		if err := native.releaseValues(); err != nil {
			test.Fatal("native release did not resume", err)
		}
		if err := native.start(0, source.Bytes); err != nil {
			test.Fatal("native start did not resume", err)
		}
	}
	var scanner legacyJSONValueScanner
	if event == nil {
		if _, err := legacyNativeObjectW79Continue(test, native, &scanner, source, legacyNativeNumberDiagnosticsFull); err != nil {
			test.Fatal("new native input failed after capture", err)
		}
	} else {
		complete := false
		for attempt := 0; attempt < legacyNativeObjectW79Steps; attempt++ {
			input := &legacyNormalizationInput{ctx: context.Background(), source: source}
			_, ready, err := event.step(input.ctx, input, &scanner)
			if err != nil {
				test.Fatal("new Event input failed after capture", err)
			}
			if ready {
				complete = true
				break
			}
		}
		if !complete || !event.canonicalReady {
			test.Fatal("new Event input did not reach canonical readiness")
		}
	}
	want := reflect.New(native.metadata.model).Interface()
	if err := json.Unmarshal([]byte(raw), want); err != nil || !reflect.DeepEqual(native.destination.Interface(), want) {
		test.Fatal("resumed parser did not consume the actual new input", err)
	}
	return scanner
}

func TestLegacyNormalizationCaptureRQ87NativeLifecycle(test *testing.T) {
	for _, target := range []string{"native", "event-wrapper"} {
		for _, ending := range []string{"put-failure", "cancel-put", "cancel-step", "close", "take", "cancel-take", "early-take"} {
			test.Run(target+"/"+ending, func(test *testing.T) {
				request, native, scanner, source := legacyNormalizationCaptureRQ87Block(test)
				var event *legacyNormalizationNativeEvent
				if target == "event-wrapper" {
					var err error
					event, err = newLegacyNormalizationNativeEvent()
					if err != nil {
						test.Fatal(err)
					}
					eventFrame := legacyNormalizationCaptureRQ87Resume(test, event.object, event)
					request, source = legacyNativeAnyW78FullSource(test, `{"kind":"message","seq":91,"role":"user","id":"fresh-input"}`)
					native, scanner = event.object, &eventFrame
				}
				before := legacyNormalizationCaptureRQ87Observe(test, native, scanner, source.Bytes)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				store, _ := legacyNativeCheckpointStoreFixture(test)
				put := store.Put
				writes, sourceReads := 0, 0
				read := source.Read
				source.Read = func(current context.Context, offset, length int) ([]byte, error) {
					sourceReads++
					return read(current, offset, length)
				}
				injected := errors.New("RQ87 checkpoint Put failed")
				store.Put = func(current context.Context, hash ContentHash, body []byte) error {
					writes++
					if current != ctx {
						test.Fatal("checkpoint replaced the current CAS context")
					}
					legacyNormalizationCaptureRQ87Blocked(test, native, event, scanner, source)
					if ending == "put-failure" {
						return injected
					}
					err := put(current, hash, body)
					if ending == "cancel-put" {
						cancel()
					}
					return err
				}
				work, err := newLegacyNativeObjectCheckpoint(ctx, native, scanner, request.Binding, source, store, legacyNativeObjectW79Budget)
				if err != nil {
					test.Fatal(err)
				}
				defer work.close()
				legacyNormalizationCaptureRQ87Blocked(test, native, event, scanner, source)
				var failure error
				for attempt := 0; attempt < legacyNativeObjectW79Steps && writes == 0; attempt++ {
					ready, err := work.step(ctx)
					failure = err
					if err != nil || ready {
						break
					}
				}
				if writes == 0 {
					test.Fatal("capture never reached actual checkpoint storage", failure)
				}
				switch ending {
				case "put-failure":
					if !errors.Is(failure, injected) {
						test.Fatal("lost checkpoint storage failure", failure)
					}
				case "cancel-put":
					if !errors.Is(failure, context.Canceled) {
						test.Fatal("lost cancellation inside actual Put", failure)
					}
				default:
					if failure != nil {
						test.Fatal(failure)
					}
					switch ending {
					case "cancel-step":
						cancel()
						_, failure = work.step(ctx)
					case "close":
						work.close()
					case "early-take":
						_, failure = work.take(ctx)
						if !errors.Is(failure, ErrIntegrity) {
							test.Fatal("premature take did not fail", failure)
						}
					default:
						legacyNormalizationCaptureRQ87Ready(test, ctx, work)
						legacyNormalizationCaptureRQ87Blocked(test, native, event, scanner, source)
						if ending == "cancel-take" {
							cancel()
						}
						root, err := work.take(ctx)
						failure = err
						if ending == "take" && (err != nil || !root.valid()) || ending == "cancel-take" && root != (legacyNativeCheckpointRef{}) {
							test.Fatal("take returned the wrong root", root, err)
						}
					}
				}
				if ending == "cancel-step" || ending == "cancel-take" {
					if !errors.Is(failure, context.Canceled) {
						test.Fatal("lost cancellation", failure)
					}
				}
				if failure != nil {
					legacyNativeObjectW79Failed[legacyNativeCheckpointRef](test, work, failure)
				} else {
					legacyNativeAnyW78Released(test, work)
				}
				if sourceReads != 0 || !reflect.DeepEqual(before, legacyNormalizationCaptureRQ87Observe(test, native, scanner, source.Bytes)) {
					test.Fatal("capture replayed source or changed the borrowed parser", sourceReads)
				}
				legacyNormalizationCaptureRQ87Resume(test, native, event)
			})
		}
	}
}

func TestLegacyNormalizationCaptureRQ87ScopeLifetime(test *testing.T) {
	test.Run("nested-mutation-and-shared-grant", func(test *testing.T) {
		scope := &legacyNormalizationMutation{}
		for depth := 0; depth < 2; depth++ {
			if err := scope.enter(); err != nil {
				test.Fatal("same-parser nested mutation was rejected", err)
			}
		}
		if grant, err := scope.beginCapture(); grant != nil {
			grant.release()
			test.Fatal("capture entered an active mutation")
		} else {
			legacyNormalizationCaptureRQ87Busy(test, err)
		}
		scope.leave()
		if scope.activity.Load() <= 0 {
			test.Fatal("inner leave released the outer mutation")
		}
		scope.leave()
		first, err := scope.beginCapture()
		if err != nil {
			test.Fatal(err)
		}
		second, err := scope.beginCapture()
		if err != nil || first != second || !first.valid(scope) || first.valid(&legacyNormalizationMutation{}) || scope.activity.Load() >= 0 {
			test.Fatal("captures did not share a scope-local grant", err)
		}
		legacyNormalizationCaptureRQ87Busy(test, scope.enter())
		first.release()
		if !second.valid(scope) {
			test.Fatal("first release invalidated another capture reference")
		}
		legacyNormalizationCaptureRQ87Busy(test, scope.enter())
		second.release()
		legacyNormalizationCaptureRQ87Idle(test, scope)
		if first.valid(scope) {
			test.Fatal("retired grant remained valid")
		}
		current, err := scope.beginCapture()
		if err != nil || !current.valid(scope) || first.valid(scope) {
			test.Fatal("new capture revived a retired grant", err)
		}
		scope.stop()
		scope.stop()
		if !scope.closed.Load() || current.valid(scope) {
			test.Fatal("owner stop did not revoke the active grant")
		}
		legacyNormalizationCaptureRQ87Stopped(test, scope.enter())
		if rejected, err := scope.beginCapture(); rejected != nil {
			rejected.release()
			test.Fatal("closed scope admitted another capture")
		} else {
			legacyNormalizationCaptureRQ87Stopped(test, err)
		}
		current.release()
		legacyNormalizationCaptureRQ87Stopped(test, scope.enter())
	})

	test.Run("two-independent-checkpoints", func(test *testing.T) {
		request, native, scanner, source := legacyNormalizationCaptureRQ87Block(test)
		store, _ := legacyNativeCheckpointStoreFixture(test)
		first, err := newLegacyNativeObjectCheckpoint(context.Background(), native, scanner, request.Binding, source, store, legacyNativeObjectW79Budget)
		if err != nil {
			test.Fatal(err)
		}
		defer first.close()
		grant := native.mutation.capture.Load()
		second, err := newLegacyNativeObjectCheckpoint(context.Background(), native, scanner, request.Binding, source, store, legacyNativeObjectW79Budget)
		if err != nil {
			test.Fatal(err)
		}
		defer second.close()
		if grant == nil || grant != native.mutation.capture.Load() || !grant.valid(native.mutation) {
			test.Fatal("independent checkpoint handles did not retain the same grant")
		}
		first.close()
		first.close()
		legacyNormalizationCaptureRQ87Blocked(test, native, nil, scanner, source)
		legacyNormalizationCaptureRQ87Ready(test, context.Background(), second)
		legacyNormalizationCaptureRQ87Blocked(test, native, nil, scanner, source)
		if root, err := second.take(context.Background()); err != nil || !root.valid() {
			test.Fatal("remaining capture could not finish", err)
		}
		legacyNormalizationCaptureRQ87Idle(test, native.mutation)
		third, err := newLegacyNativeObjectCheckpoint(context.Background(), native, scanner, request.Binding, source, store, legacyNativeObjectW79Budget)
		if err != nil {
			test.Fatal(err)
		}
		defer third.close()
		first.close()
		second.close()
		if grant.valid(native.mutation) {
			test.Fatal("new checkpoint revived the retired shared grant")
		}
		legacyNormalizationCaptureRQ87Blocked(test, native, nil, scanner, source)
		third.close()
		third.close()
		legacyNormalizationCaptureRQ87Resume(test, native, nil)
	})

	for _, ending := range []string{"take", "close", "cancel"} {
		test.Run("automatic-nested-child/"+ending, func(test *testing.T) {
			fixture := legacyNativeNestedW83Fixtures()[1]
			request, bundle := legacyNativeNestedW83Prepare(test, fixture)
			reached := false
			for attempt := 0; attempt < legacyNativeNestedW83SourceSteps; attempt++ {
				if legacyNativeNestedW83Stop(bundle, fixture) {
					reached = true
					break
				}
				if ready, err := legacyNativeEventW82Step(test, context.Background(), bundle, true, false); err != nil || ready {
					test.Fatal("nested parser missed its actual shared-key cut", ready, err)
				}
			}
			if !reached {
				test.Fatal("nested parser exceeded its finite source schedule")
			}
			child := legacyNativeNestedW83Child(bundle.Object, fixture.field)
			if child == nil || child.mutation != bundle.Object.mutation {
				test.Fatal("nested parser did not inherit the parent mutation scope")
			}
			store, _ := legacyNativeCheckpointStoreFixture(test)
			work, err := newLegacyNativeObjectCheckpoint(context.Background(), bundle.Object, &bundle.Scanner, request.Binding, bundle.Source, store, legacyNativeNestedW83Budget)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			grant := bundle.Object.mutation.capture.Load()
			var capturedChild *legacyNativeObjectCheckpoint
			for attempt := 0; attempt < legacyNativeNestedW83CheckpointSteps; attempt++ {
				if nested, ok := work.nestedTask.(*legacyNativeObjectNestedCheckpoint); ok && nested.child != nil {
					capturedChild = nested.child
					break
				}
				if ready, err := work.step(context.Background()); err != nil || ready {
					test.Fatal("root capture did not construct its real nested child", ready, err)
				}
			}
			if capturedChild == nil || capturedChild.native != child || grant == nil || grant != child.mutation.capture.Load() || !grant.valid(child.mutation) {
				test.Fatal("automatic child capture did not retain the shared grant")
			}
			legacyNormalizationCaptureRQ87Blocked(test, child, nil, &bundle.Scanner, bundle.Source)
			switch ending {
			case "close":
				work.close()
			case "cancel":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if ready, err := work.step(ctx); ready || !errors.Is(err, context.Canceled) {
					test.Fatal("nested cancellation did not fail its root capture", ready, err)
				}
			default:
				for attempt := 0; attempt < legacyNativeNestedW83CheckpointSteps && !capturedChild.closed; attempt++ {
					if _, err := work.step(context.Background()); err != nil {
						test.Fatal(err)
					}
				}
				if !capturedChild.closed || !grant.valid(bundle.Object.mutation) {
					test.Fatal("completed child released its still-live parent grant")
				}
				legacyNormalizationCaptureRQ87Blocked(test, bundle.Object, nil, &bundle.Scanner, bundle.Source)
				legacyNormalizationCaptureRQ87Ready(test, context.Background(), work)
				if root, err := work.take(context.Background()); err != nil || !root.valid() {
					test.Fatal("nested capture did not finish", err)
				}
			}
			work.close()
			work.close()
			legacyNormalizationCaptureRQ87Idle(test, bundle.Object.mutation)
			if !capturedChild.closed || grant.valid(bundle.Object.mutation) {
				test.Fatal("root cleanup left a child capture reference alive")
			}
			if _, err := legacyNativeNestedW83Continue(test, bundle); err != nil {
				test.Fatal("nested source did not resume after capture cleanup", err)
			}
		})
	}
}

func TestLegacyNormalizationCaptureRQ87ConstructorCleanup(test *testing.T) {
	for _, fault := range []string{"model", "schema", "budget", "source", "canceled"} {
		test.Run(fault, func(test *testing.T) {
			request, native, scanner, source := legacyNormalizationCaptureRQ87Block(test)
			metadata := native.metadata
			current := source
			current.Parts = append([]ConversationManifestChunk(nil), source.Parts...)
			budget := legacyNativeObjectW79Budget
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := ErrIntegrity
			switch fault {
			case "model":
				changed := newLegacyNativeObjectFields(reflect.TypeOf(struct {
					Unregistered string `json:"unregistered"`
				}{}))
				native.metadata = &changed
			case "schema":
				changed := *metadata
				changed.fields = append([]legacyNativeObjectField(nil), metadata.fields...)
				changed.fields[0].prefix += " "
				native.metadata = &changed
			case "budget":
				budget, want = 1, ErrDocumentResourceLimit
			case "source":
				current.Parts[0].Bytes++
			case "canceled":
				cancel()
				want = context.Canceled
			}
			store, _ := legacyNativeCheckpointStoreFixture(test)
			writes, reads := 0, 0
			put, read := store.Put, current.Read
			store.Put = func(current context.Context, hash ContentHash, body []byte) error {
				writes++
				return put(current, hash, body)
			}
			current.Read = func(current context.Context, offset, length int) ([]byte, error) {
				reads++
				return read(current, offset, length)
			}
			if fault != "canceled" {
				if err := native.mutation.enter(); err != nil {
					test.Fatal(err)
				}
				rejected, err := newLegacyNativeObjectCheckpoint(ctx, native, scanner, request.Binding, current, store, budget)
				native.mutation.leave()
				if rejected != nil {
					rejected.close()
					test.Fatal("constructor observed model/source before excluding an active mutation")
				}
				if fault == "budget" {
					if !errors.Is(err, ErrDocumentResourceLimit) {
						test.Fatal("fixed capture allocation was not admitted before acquisition", err)
					}
				} else {
					legacyNormalizationCaptureRQ87Busy(test, err)
				}
			}
			work, err := newLegacyNativeObjectCheckpoint(ctx, native, scanner, request.Binding, current, store, budget)
			if work != nil {
				work.close()
				test.Fatal("invalid capture constructor returned a work handle")
			}
			if !errors.Is(err, want) || errors.Is(err, ErrLegacyNormalizationBusy) || writes != 0 || reads != 0 {
				test.Fatal("constructor lost its error or performed payload IO", err, want, writes, reads)
			}
			native.metadata = metadata
			legacyNormalizationCaptureRQ87Idle(test, native.mutation)
			retry, err := newLegacyNativeObjectCheckpoint(context.Background(), native, scanner, request.Binding, source, store, legacyNativeObjectW79Budget)
			if err != nil {
				test.Fatal("failed constructor stranded its capture grant", err)
			}
			retry.close()
			retry.close()
			legacyNormalizationCaptureRQ87Resume(test, native, nil)
		})
	}
}

func legacyNormalizationCaptureRQ87Shared(test *testing.T, native *legacyNativeObject, scope *legacyNormalizationMutation) {
	test.Helper()
	if native == nil {
		return
	}
	if scope == nil || native.mutation != scope {
		test.Fatal("real parser child did not inherit its owner's scope")
	}
	for _, child := range native.nested {
		legacyNormalizationCaptureRQ87Shared(test, child, scope)
	}
	for _, list := range native.blocks {
		if list != nil {
			legacyNormalizationCaptureRQ87Shared(test, list.object, scope)
		}
	}
	for _, list := range native.events {
		if list != nil {
			legacyNormalizationCaptureRQ87Shared(test, list.object, scope)
		}
	}
}

func TestLegacyNormalizationCaptureRQ87RealPipelines(test *testing.T) {
	for _, pipeline := range []string{"decoder", "preparation"} {
		for _, target := range []string{"event", "envelope", "replacement-child"} {
			endings := []string{"take"}
			if target == "event" {
				endings = []string{"put-failure", "cancel", "close", "take"}
				if pipeline == "preparation" {
					endings = append(endings, "owner-close-step", "owner-close-take")
				}
			}
			for _, ending := range endings {
				test.Run(pipeline+"/"+target+"/"+ending, func(test *testing.T) {
					payload := strings.Repeat("p", ConversationManifestChunkBytes+73)
					event := `{"kind":"message","seq":2,"role":"user","provider_metadata":{"turn_id":"shared-child"},"blocks":[{"type":"text","text":"` + payload + `"}]}`
					raw := []byte(`{"envelope":{"cir_version":"2"},"events":[` + event + `,{"kind":"turn","seq":1,"role":"user"}]}`)
					if target == "envelope" {
						raw = []byte(`{"envelope":{"cir_version":"2","source_model":"` + payload + `"},"events":[{"kind":"turn","seq":1,"role":"user"}]}`)
					}
					if target == "replacement-child" {
						raw = []byte(`{"envelope":{"cir_version":"2"},"events":[{"kind":"compaction","seq":3,"replacement_complete":true,"replacement":[` + event + `]}]}`)
					}
					want, err := legacyNormalizationOracle(raw)
					if err != nil {
						test.Fatal("pipeline fixture failed the independent typed oracle", err)
					}
					buffer := &legacySourceProofBuffer{body: raw}
					source := legacyNormalizationPreparationInput(test, buffer, false)
					storage := &legacyNormalizationPreparationStorage{test: test}
					var preparation *LegacyNormalizationPreparation
					var normalizer *LegacyNormalizationDecoder
					if pipeline == "preparation" {
						preparation, err = NewLegacyNormalizationPreparation(context.Background(), source, storage)
						if err == nil {
							normalizer = preparation.decoder
						}
					} else {
						normalizer, err = NewLegacyNormalizationDecoder(context.Background(), source)
					}
					if err != nil {
						test.Fatal(err)
					}
					var selected, root *legacyNativeObject
					for attempt := 0; attempt < 128; attempt++ {
						if target == "envelope" && normalizer.envelopeActive && normalizer.envelopeNative != nil && normalizer.envelopeNative.scalar != nil {
							selected, root = normalizer.envelopeNative, normalizer.envelopeNative
							break
						}
						if target != "envelope" && normalizer.native != nil {
							candidate := normalizer.nativeCache.object
							if target == "replacement-child" {
								candidate = nil
								for _, list := range normalizer.nativeCache.object.events {
									if list != nil && list.object != nil {
										candidate = list.object
										break
									}
								}
							}
							if candidate != nil {
								list := legacyNativeEventW82List(candidate)
								if list != nil && list.object != nil && list.object.scalar != nil {
									selected, root = candidate, normalizer.nativeCache.object
									break
								}
							}
						}
						if preparation != nil {
							if ready, err := preparation.Step(context.Background()); err != nil || ready || preparation.decoder != normalizer {
								test.Fatal("preparation missed the real active parser cut", ready, err)
							}
						} else {
							step, err := normalizer.Step(context.Background())
							if err != nil || step.Complete || len(step.Events) != 0 {
								test.Fatal("decoder emitted an event before the capture cut", err)
							}
						}
					}
					if selected == nil || root == nil {
						test.Fatal("real parser cut exceeded its finite source schedule")
					}
					scope := normalizer.mutation
					if scope == nil || preparation != nil && preparation.mutation != scope {
						test.Fatal("preparation and decoder do not share the same mutation scope")
					}
					legacyNormalizationCaptureRQ87Shared(test, root, scope)
					legacyNormalizationCaptureRQ87Shared(test, normalizer.envelopeNative, scope)
					beforeNative := legacyNormalizationCaptureRQ87Observe(test, root, &normalizer.frame, normalizer.source.Bytes)
					beforeDecoder := *normalizer
					beforeDecoder.source.Read = nil
					var beforePreparation LegacyNormalizationPreparation
					if preparation != nil {
						beforePreparation = *preparation
					}
					beforeSlots := storage.slots
					for slot := range beforeSlots {
						beforeSlots[slot] = bytes.Clone(beforeSlots[slot])
					}
					resets, storageReads, sourceReads := storage.resets, len(storage.reads), len(buffer.reads)
					unchanged := func() {
						afterDecoder := *normalizer
						afterDecoder.source.Read = nil
						if !reflect.DeepEqual(beforeDecoder, afterDecoder) || !reflect.DeepEqual(beforeNative, legacyNormalizationCaptureRQ87Observe(test, root, &normalizer.frame, normalizer.source.Bytes)) || preparation != nil && !reflect.DeepEqual(beforePreparation, *preparation) {
							test.Fatal("exclusion changed decoder, preparation, scanner, or parser state")
						}
						if !reflect.DeepEqual(beforeSlots, storage.slots) || resets != storage.resets || storageReads != len(storage.reads) || sourceReads != len(buffer.reads) {
							test.Fatal("exclusion performed source or scratch IO")
						}
					}
					blocked := func() {
						step, err := normalizer.Step(context.Background())
						legacyNormalizationCaptureRQ87Busy(test, err)
						legacyNativeEventZero(test, step)
						if preparation != nil {
							ready, err := preparation.Step(context.Background())
							legacyNormalizationCaptureRQ87Busy(test, err)
							if ready {
								test.Fatal("busy preparation reported readiness")
							}
						}
						unchanged()
					}
					if target == "replacement-child" {
						grant, err := selected.mutation.beginCapture()
						if err != nil {
							test.Fatal(err)
						}
						blocked()
						grant.release()
					} else {
						store, _ := legacyNativeCheckpointStoreFixture(test)
						put := store.Put
						writes := 0
						injected := errors.New("RQ87 pipeline checkpoint Put failed")
						ctx, cancel := context.WithCancel(context.Background())
						defer cancel()
						store.Put = func(current context.Context, hash ContentHash, body []byte) error {
							writes++
							if current != ctx {
								test.Fatal("pipeline capture replaced the current CAS context")
							}
							blocked()
							if ending == "put-failure" {
								return injected
							}
							return put(current, hash, body)
						}
						work, err := newLegacyNativeObjectCheckpoint(ctx, selected, &normalizer.frame, HashContent([]byte("RQ87 live pipeline capture")), normalizer.source, store, legacyNativeNestedW83Budget)
						if err != nil {
							test.Fatal(err)
						}
						defer work.close()
						grant := scope.capture.Load()
						blocked()
						var failure error
						for attempt := 0; attempt < legacyNativeObjectW79Steps && writes == 0; attempt++ {
							ready, err := work.step(ctx)
							failure = err
							if err != nil || ready {
								break
							}
						}
						if writes == 0 {
							test.Fatal("pipeline capture never reached actual checkpoint storage", failure)
						}
						switch ending {
						case "put-failure":
							if !errors.Is(failure, injected) {
								test.Fatal(failure)
							}
						case "cancel":
							if failure != nil {
								test.Fatal(failure)
							}
							cancel()
							_, failure = work.step(ctx)
							if !errors.Is(failure, context.Canceled) {
								test.Fatal("pipeline capture lost cancellation", failure)
							}
						case "close":
							if failure != nil {
								test.Fatal(failure)
							}
							work.close()
						default:
							if failure != nil {
								test.Fatal(failure)
							}
							legacyNormalizationCaptureRQ87Ready(test, ctx, work)
							blocked()
							if strings.HasPrefix(ending, "owner-close-") {
								preparation.Close()
								preparation.Close()
								if grant == nil || grant.valid(scope) || !scope.closed.Load() {
									test.Fatal("owner Close did not revoke the Ready capture")
								}
								unchanged()
								if ending == "owner-close-step" {
									ready, err := work.step(ctx)
									failure = err
									if ready {
										test.Fatal("revoked Ready capture still reported readiness")
									}
								} else {
									root, err := work.take(ctx)
									failure = err
									if root != (legacyNativeCheckpointRef{}) {
										test.Fatal("revoked Ready capture published a root")
									}
								}
								legacyNormalizationCaptureRQ87Stopped(test, failure)
								legacyNativeObjectW79Failed[legacyNativeCheckpointRef](test, work, failure)
								if ready, err := preparation.Step(context.Background()); ready {
									test.Fatal("closed preparation resumed")
								} else {
									legacyNormalizationCaptureRQ87Stopped(test, err)
								}
								if rejected, err := scope.beginCapture(); rejected != nil {
									rejected.release()
									test.Fatal("closed preparation admitted a new capture")
								} else {
									legacyNormalizationCaptureRQ87Stopped(test, err)
								}
								legacyNormalizationCaptureRQ87Stopped(test, scope.enter())
								unchanged()
								return
							}
							if root, err := work.take(ctx); err != nil || !root.valid() {
								test.Fatal("pipeline checkpoint did not finish", err)
							}
						}
						if failure != nil {
							legacyNativeObjectW79Failed[legacyNativeCheckpointRef](test, work, failure)
						} else {
							legacyNativeAnyW78Released(test, work)
						}
					}
					unchanged()
					legacyNormalizationCaptureRQ87Idle(test, scope)
					if preparation == nil {
						got, err := legacyNativeScalarReviewDrain(test, normalizer, buffer)
						if err != nil || !bytes.Equal(got, want) {
							test.Fatal("decoder did not resume the same real input to typed parity", err)
						}
					} else {
						complete := false
						for attempt := 0; attempt < 8192; attempt++ {
							ready, err := preparation.Step(context.Background())
							if err != nil {
								test.Fatal("preparation did not resume the same real input", err)
							}
							if ready {
								complete = true
								break
							}
						}
						prepared, err := preparation.Source()
						if !complete || err != nil || prepared.Bytes != len(want) || !bytes.Equal(storage.slots[3], want) || preparation.mutation != scope {
							test.Fatal("preparation lost typed parity or its owner scope after decoder handoff", err)
						}
					}
					legacyNormalizationCaptureRQ87Idle(test, scope)
				})
			}
		}
	}
}
