package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

type legacyNativeSourceW88Fixture struct {
	parent   *legacyNativeObjectCheckpoint
	native   *legacyNativeObject
	scanner  *legacyJSONValueScanner
	source   LegacyByteSource
	request  legacyNativeAnyW78Request
	storage  *legacyNativeGoGraphSetW85Store
	before   legacyNormalizationCaptureRQ87State
	roots    []any
	snapshot []legacyNativeGoGraphW85SourceRecord
	rawReads int
}

func legacyNativeSourceW88Initial(mode string) *wire {
	shared := map[string]any{"kept": math.Copysign(0, -1), "nested": []any{true, nil, "original"}}
	if mode == "separate-equal" {
		return &wire{Input: shared, Output: map[string]any{"kept": math.Copysign(0, -1), "nested": []any{true, nil, "original"}}}
	}
	if mode == "same-root" {
		return &wire{Input: shared, Output: shared}
	}
	large := legacyNativeGoGraphW86Fixtures()[2].roots[0].([]any)
	backing := []any{shared, shared, math.Copysign(0, -1), large[:cap(large)][1]}
	return &wire{Input: map[string]any{"array": backing[:1:4], "shared": shared}, Output: backing[:1:4]}
}

func legacyNativeSourceW88Prepare(test *testing.T, mode string) *legacyNativeSourceW88Fixture {
	test.Helper()
	raw := `{}`
	var stop func(*legacyNativeObject) bool
	if mode == "active-scalar" {
		raw = `{"id":"` + strings.Repeat("s", 2*LegacyNormalizationPartBytes) + `","input":{"added":true},"seq":5}`
		stop = func(native *legacyNativeObject) bool {
			return native.scalar != nil && native.scalar.position >= LegacyNormalizationPartBytes
		}
	}
	request, native, scanner, source := legacyNativeEventW82FencePrepare(test, raw, legacyNativeSourceW88Initial(mode), stop)
	fixture := &legacyNativeSourceW88Fixture{native: native, scanner: scanner, source: source, request: request, storage: legacyNativeGoGraphSetW85Storage(test)}
	fixture.before = legacyNormalizationCaptureRQ87Observe(test, native, scanner, source.Bytes)
	read := source.Read
	fixture.source.Read = func(current context.Context, offset, length int) ([]byte, error) {
		fixture.rawReads++
		return read(current, offset, length)
	}
	inputs := make(map[ContentHash][]byte)
	for offset := 0; offset < request.SourceBytes; offset += request.PartBytes {
		part := request.Parts[offset/request.PartBytes]
		body := bytes.Clone(request.Suffix[offset : offset+int(part.Bytes)])
		if HashContent(body) != part.Hash {
			test.Fatal("W88 source manifest does not describe the actual original bytes")
		}
		inputs[part.Hash], fixture.storage.objects[part.Hash] = body, bytes.Clone(body)
	}
	put := fixture.storage.io.Put
	fixture.storage.io.Put = func(current context.Context, hash ContentHash, body []byte) error {
		if _, original := inputs[hash]; original {
			test.Fatal("owned graph capture rewrote original source CAS")
		}
		return put(current, hash, body)
	}
	constructor, cancel := context.WithCancel(context.Background())
	fixture.storage.current = constructor
	parent, err := newLegacyNativeObjectCheckpoint(constructor, native, scanner, request.Binding, fixture.source, fixture.storage.io, legacyNativeEventW82FenceBudget)
	cancel()
	fixture.storage.current = nil
	if err != nil || parent == nil || fixture.storage.reads != 0 || fixture.storage.writes != 0 {
		test.Fatal("actual parent construction failed or performed IO", err)
	}
	fixture.parent = parent
	test.Cleanup(parent.close)
	test.Cleanup(func() {
		for hash, body := range inputs {
			if !bytes.Equal(body, fixture.storage.objects[hash]) {
				test.Fatal("owned capture changed immutable original source bytes")
			}
		}
	})
	for attempt := 0; attempt < legacyNativeEventW82FenceSteps; attempt++ {
		if parent.phase == 6 {
			if parent.initialTask != nil || parent.state.InitialGraphCount != 2 || !parent.capture.valid(native.mutation) {
				test.Fatal("fixture did not reach actual initial-graph dispatch with a live parent grant")
			}
			fixture.roots = append([]any(nil), parent.initialValues...)
			for _, root := range fixture.roots {
				fixture.snapshot = append(fixture.snapshot, legacyNativeGoGraphW85SourceSnapshot(root)...)
			}
			return fixture
		}
		if ready, err := legacyNativeGoGraphSetW85Step[legacyNativeCheckpointRef](test, parent, fixture.storage); err != nil || ready {
			test.Fatal("actual parent failed before phase-six dispatch", ready, err)
		}
	}
	test.Fatal("actual parent exceeded the existing finite dispatch schedule")
	return fixture
}

func (fixture *legacyNativeSourceW88Fixture) unchanged(test *testing.T) {
	test.Helper()
	if fixture.rawReads != 0 || !reflect.DeepEqual(fixture.before, legacyNormalizationCaptureRQ87Observe(test, fixture.native, fixture.scanner, fixture.source.Bytes)) {
		test.Fatal("owned graph capture read RAW or changed the actual source parser", fixture.rawReads)
	}
	legacyNativeGoGraphW85SourceUnchanged(test, fixture.snapshot)
}

func legacyNativeSourceW88Accounting(test *testing.T, parent *legacyNativeObjectCheckpoint, child *legacyNativeCheckpointGoGraphSet) int64 {
	test.Helper()
	metadata, err := parent.retainedBytes()
	if err != nil {
		test.Fatal("actual parent metadata accounting failed", err)
	}
	source, err := parent.native.retained()
	if err != nil || source <= 0 || metadata <= 0 || source > parent.budget || metadata > parent.budget-source {
		test.Fatal("actual parent source plus metadata escaped the original aggregate budget", source, metadata, parent.budget, err)
	}
	want := parent.budget - source - metadata
	remaining, err := parent.remaining()
	if err != nil || remaining != want {
		test.Fatal("parent did not reserve its actual native source exactly once", remaining, want, err)
	}
	if child != nil {
		remaining, err = child.remaining()
		if err != nil || remaining != want {
			test.Fatal("owned graph set kept a stale or source-free remaining budget", remaining, want, err)
		}
	}
	return want
}

func (fixture *legacyNativeSourceW88Fixture) bind(test *testing.T, dispatch bool) *legacyNativeCheckpointGoGraphSet {
	test.Helper()
	parent := fixture.parent
	before, err := parent.retainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	reads, writes := fixture.storage.reads, fixture.storage.writes
	var child *legacyNativeCheckpointGoGraphSet
	if dispatch {
		if ready, err := legacyNativeGoGraphSetW85Step[legacyNativeCheckpointRef](test, parent, fixture.storage); err != nil || ready {
			test.Fatal("actual phase six could not construct its owned graph set", ready, err)
		}
		child, _ = parent.initialTask.(*legacyNativeCheckpointGoGraphSet)
	} else {
		constructor, cancel := context.WithCancel(context.Background())
		fixture.storage.current = constructor
		child, err = newLegacyNativeCheckpointGoGraphSetForObject(constructor, parent)
		cancel()
		fixture.storage.current = nil
		if err != nil {
			test.Fatal("owner-bound graph set construction failed", err)
		}
	}
	if child == nil || parent.initialTask != child || child.owner != parent || child.registry == nil || child.registry.source != child {
		test.Fatal("graph set did not bind the actual parent slot and importer registry")
	}
	if fixture.storage.reads != reads || fixture.storage.writes != writes || child.state.Binding != parent.state.Binding || child.checkOwner() != nil {
		test.Fatal("owned construction performed IO or substituted a source binding")
	}
	if len(child.borrowed) != len(parent.initialValues) || len(child.values) != len(parent.initialValues) || unsafe.SliceData(child.borrowed) != unsafe.SliceData(parent.initialValues) {
		test.Fatal("owned graph set did not borrow the exact parent root vector")
	}
	for index, field := range parent.native.metadata.fields {
		slot := parent.initialSlots[index]
		if slot == 0 {
			continue
		}
		actual := parent.native.destination.Elem().Field(field.index).Interface()
		if !legacyNativeCheckpointGoGraphSetSame(actual, parent.initialValues[slot-1]) || !legacyNativeCheckpointGoGraphSetSame(actual, child.values[slot-1]) {
			test.Fatal("reservation covered a different source with equal content", field.name)
		}
	}
	addition, err := child.retainedBytes()
	after, parentErr := parent.retainedBytes()
	if err != nil || parentErr != nil || addition <= 0 || after-before != addition || child.sourceHeap.err != nil || child.sourceHeap.bytes != 0 {
		test.Fatal("owned child double-counted the borrowed source or recursed through its parent", before, after, addition, err, parentErr)
	}
	legacyNativeSourceW88Accounting(test, parent, child)
	return child
}

func legacyNativeSourceW88ParentReady(test *testing.T, fixture *legacyNativeSourceW88Fixture) {
	test.Helper()
	for attempt := 0; attempt < legacyNativeEventW82FenceSteps; attempt++ {
		ready, err := legacyNativeGoGraphSetW85Step[legacyNativeCheckpointRef](test, fixture.parent, fixture.storage)
		if err != nil {
			test.Fatal("owned parent failed before final readiness", fixture.parent.phase, err)
		}
		child, _ := fixture.parent.initialTask.(*legacyNativeCheckpointGoGraphSet)
		legacyNativeSourceW88Accounting(test, fixture.parent, child)
		if ready {
			return
		}
	}
	test.Fatal("owned parent exceeded the existing finite checkpoint schedule")
}

func TestLegacyNativeSourceW88ReservationAndCanonicalRestore(test *testing.T) {
	for _, mode := range []string{"same-root", "separate-equal", "full-backing", "active-scalar"} {
		test.Run(mode, func(test *testing.T) {
			fixture := legacyNativeSourceW88Prepare(test, mode)
			parent := fixture.parent
			metadata, err := parent.retainedBytes()
			if err != nil {
				test.Fatal(err)
			}
			source, err := parent.native.retained()
			if err != nil {
				test.Fatal(err)
			}
			if mode == "full-backing" || mode == "active-scalar" {
				backing := fixture.native.destination.Interface().(*wire).Output.([]any)
				if source < int64(len(backing[:cap(backing)][3].(string))) {
					test.Fatal("measured parent source omitted the actual large hidden backing")
				}
			}
			budget := parent.budget
			parent.budget = metadata + source
			reads, writes := fixture.storage.reads, fixture.storage.writes
			rejected, err := newLegacyNativeCheckpointGoGraphSetForObject(context.Background(), parent)
			if rejected != nil || !errors.Is(err, ErrDocumentResourceLimit) || parent.initialTask != nil || fixture.storage.reads != reads || fixture.storage.writes != writes || !parent.capture.valid(fixture.native.mutation) {
				test.Fatal("source reservation admitted extra ownership or performed low-budget IO", err)
			}
			parent.budget = budget
			child := fixture.bind(test, mode == "same-root" || mode == "active-scalar")
			legacyNativeSourceW88ParentReady(test, fixture)
			binding := parent.state.Binding
			reads, writes = fixture.storage.reads, fixture.storage.writes
			root, err := parent.take(context.Background())
			if err != nil || !root.valid() || fixture.storage.reads != reads || fixture.storage.writes != writes {
				test.Fatal("actual parent could not hand off its owned canonical root without IO", err)
			}
			legacyNativeGoGraphSetW85Released[legacyNativeCheckpointRef](test, child)
			legacyNativeGoGraphSetW85Released[legacyNativeCheckpointRef](test, parent)
			legacyNormalizationCaptureRQ87Idle(test, fixture.native.mutation)
			fixture.unchanged(test)
			var state legacyNativeObjectCheckpointControl
			body := fixture.storage.objects[root.Hash]
			if len(body) != root.Bytes || HashContent(body) != root.Hash || json.Unmarshal(body, &state) != nil || state.validate(legacyNativeCheckpointEventModel, binding, fixture.source.Bytes, true) != nil || state.InitialGraphs == nil || state.InitialGraphCount != 2 {
				test.Fatal("actual parent did not store a valid canonical initial-graph checkpoint")
			}
			canonical, err := json.Marshal(state)
			if err != nil || !bytes.Equal(canonical, body) {
				test.Fatal("actual parent CAS body is not the canonical control encoding", err)
			}
			setState := legacyNativeGoGraphW86State(test, fixture.storage, *state.InitialGraphs)
			canonical, err = json.Marshal(setState)
			if err != nil || setState.Binding != binding || !bytes.Equal(canonical, fixture.storage.objects[state.InitialGraphs.Hash]) {
				test.Fatal("owned graph set did not preserve canonical bound CAS metadata", err)
			}
			values := legacyNativeGoGraphSetW85Restore(test, *state.InitialGraphs, binding, fixture.storage, 0)
			legacyNativeGoGraphW86Values(test, fixture.roots, values)
			currentSource := legacyNativeAnyW78Source(test, fixture.request, &legacyNativeAnyW78Reads{})
			constructor, cancel := context.WithCancel(context.Background())
			restore, err := newLegacyNativeObjectCheckpointRestore(constructor, root, legacyNativeCheckpointEventModel, fixture.storage.io, currentSource, binding, legacyNativeEventW82FenceBudget)
			cancel()
			if err != nil {
				test.Fatal("actual parent restore constructor failed", err)
			}
			defer restore.close()
			legacyNativeGoGraphSetW85Ready[legacyNativeObjectRestored](test, restore, fixture.storage, legacyNativeEventW82FenceBudget)
			restored, err := restore.take(context.Background())
			if err != nil || restored.Object == nil {
				test.Fatal("actual parent did not restore", err)
			}
			legacyNativeGoGraphSetW85Released[legacyNativeObjectRestored](test, restore)
			value := restored.Object.destination.Interface().(*wire)
			canonical, err = json.Marshal(value)
			if err != nil || !bytes.Equal(canonical, fixture.before.value) {
				test.Fatal("actual parent initial Input/Output changed after canonical restore", err)
			}
			if mode == "same-root" || mode == "separate-equal" {
				alias := reflect.ValueOf(value.Input).Pointer() == reflect.ValueOf(value.Output).Pointer()
				if alias != (mode == "same-root") {
					test.Fatal("actual parent split a shared source or merged distinct equal sources")
				}
			} else {
				output := value.Output.([]any)
				input := value.Input["array"].([]any)
				original := fixture.native.destination.Interface().(*wire).Output.([]any)
				if len(output) != 1 || cap(output) != 4 || reflect.ValueOf(output).Pointer() != reflect.ValueOf(input).Pointer() || reflect.ValueOf(output).Pointer() == reflect.ValueOf(original).Pointer() {
					test.Fatal("actual parent lost independent restored cross-field backing")
				}
				backing := output[:cap(output)]
				if reflect.ValueOf(backing[0]).Pointer() != reflect.ValueOf(backing[1]).Pointer() || reflect.ValueOf(backing[1]).Pointer() != reflect.ValueOf(value.Input["shared"]).Pointer() || math.Float64bits(backing[2].(float64)) != math.Float64bits(math.Copysign(0, -1)) || backing[3] != original[:cap(original)][3] {
					test.Fatal("actual parent changed hidden payload, negative zero, or source aliases")
				}
			}
			if _, err := legacyNativeEventW82Continue(test, &restored); err != nil {
				test.Fatal("restored actual parent could not continue", err)
			}
			if mode == "active-scalar" && (value.Input["added"] != true || value.Seq != 5) {
				test.Fatal("restored actual parent lost its live scalar/map continuation")
			}
			fixture.unchanged(test)
		})
	}
	test.Run("raw-low-level-control-only", func(test *testing.T) {
		roots := []any{map[string]any{"kept": true}}
		storage := legacyNativeGoGraphSetW85Storage(test)
		binding := HashContent([]byte("W88 unchanged raw low-level control"))
		work, err := newLegacyNativeCheckpointGoGraphSet(context.Background(), roots, binding, storage.io, legacyNativeGoGraphSetW85Budget)
		if err != nil || work == nil || work.owner != nil || work.registry.source != nil {
			test.Fatal("raw low-level graph-set control acquired a fabricated parent", err)
		}
		defer work.close()
		legacyNativeGoGraphSetW85Ready[legacyNativeCheckpointRef](test, work, storage, legacyNativeGoGraphSetW85Budget)
		root, err := work.take(context.Background())
		if err != nil {
			test.Fatal(err)
		}
		legacyNativeGoGraphW86Values(test, roots, legacyNativeGoGraphSetW85Restore(test, root, binding, storage, 0))
		test.Log("W88 covers owner-bound reservation only; raw constructors' whole-source shallow accounting remains outside this card")
	})
}

func TestLegacyNativeSourceW88LiveAggregateAdmission(test *testing.T) {
	for _, change := range []string{"parent-budget-decrease", "actual-parent-field-task-growth"} {
		test.Run(change, func(test *testing.T) {
			fixture := legacyNativeSourceW88Prepare(test, "full-backing")
			child := fixture.bind(test, false)
			if ready, err := legacyNativeGoGraphSetW85Step[legacyNativeCheckpointRef](test, child, fixture.storage); err != nil || ready || child.prepare == nil {
				test.Fatal("owned graph set did not create its real low-level importer", ready, err)
			}
			importer := child.prepare
			current, err := importer.retainedBytes()
			if err != nil {
				test.Fatal(err)
			}
			shared, err := importer.registry.retainedBytes()
			if err != nil {
				test.Fatal(err)
			}
			for _, amounts := range [][2]int64{{-1, 0}, {0, -1}, {0, current + shared + 1}} {
				if err := importer.admit(amounts[0], amounts[1]); !errors.Is(err, ErrIntegrity) {
					test.Fatal("owned importer lost its original additional/released integrity fence", amounts, err)
				}
			}
			fixedRemaining := importer.budget - current - shared
			if fixedRemaining < 0 || !errors.Is(importer.admit(fixedRemaining+1, 0), ErrDocumentResourceLimit) {
				test.Fatal("owned importer lost its original fixed budget")
			}
			parent := fixture.parent
			metadata, err := parent.retainedBytes()
			if err != nil {
				test.Fatal(err)
			}
			source, err := parent.native.retained()
			if err != nil {
				test.Fatal(err)
			}
			if change == "parent-budget-decrease" {
				parent.budget = metadata + source + 1
			} else {
				sibling, err := newLegacyNativeObjectCheckpoint(context.Background(), fixture.native, fixture.scanner, fixture.request.Binding, fixture.source, fixture.storage.io, legacyNativeEventW82FenceBudget)
				if err != nil {
					test.Fatal("could not construct a real retained parent field task", err)
				}
				defer sibling.close()
				growth, err := sibling.retainedBytes()
				if err != nil || growth <= 1 || parent.child != nil {
					test.Fatal("field task lacks actual retained metadata or occupied an active slot", growth, err)
				}
				parent.budget = metadata + source + growth + 1
				legacyNativeSourceW88Accounting(test, parent, child)
				if err := importer.admit(2, 0); err != nil {
					test.Fatal("additional allocation was not admitted before actual sibling retention", err)
				}
				parent.child = sibling
				after, err := parent.retainedBytes()
				if err != nil || after-metadata != growth {
					test.Fatal("real field task did not increase the actual retained parent metadata", after, metadata, growth, err)
				}
			}
			if parent.checkSource() != nil || child.checkOwner() != nil || !parent.capture.valid(fixture.native.mutation) {
				test.Fatal("resource-management counterexample accidentally invalidated the raw source")
			}
			if remaining := legacyNativeSourceW88Accounting(test, parent, child); remaining != 1 {
				test.Fatal("live aggregate did not observe the actual resource-management change", remaining)
			}
			reads, writes := fixture.storage.reads, fixture.storage.writes
			if !errors.Is(child.admit(2), ErrDocumentResourceLimit) || !errors.Is(importer.admit(2, 0), ErrDocumentResourceLimit) {
				test.Fatal("source or importer admitted an allocation against stale aggregate capacity")
			}
			if err := importer.admit(2, 1); err != nil {
				test.Fatal("live source admission ignored a valid actual retirement", err)
			}
			if !errors.Is(importer.admit(0, current+shared+1), ErrIntegrity) {
				test.Fatal("live source admission bypassed the original released-byte validation")
			}
			if ready, err := legacyNativeGoGraphSetW85Step[legacyNativeCheckpointRef](test, child, fixture.storage); ready || !errors.Is(err, ErrDocumentResourceLimit) {
				test.Fatal("owned graph set did not reject its next actual allocation", ready, err)
			}
			legacyNativeGoGraphSetW85Failed[legacyNativeCheckpointRef](test, child, fixture.storage, ErrDocumentResourceLimit)
			if fixture.storage.reads != reads || fixture.storage.writes != writes || !parent.capture.valid(fixture.native.mutation) {
				test.Fatal("resource rejection touched CAS or released the parent's live capture")
			}
			fixture.unchanged(test)
			parent.close()
			legacyNormalizationCaptureRQ87Idle(test, fixture.native.mutation)
		})
	}
}

func TestLegacyNativeSourceW88OwnerAndConstructionFences(test *testing.T) {
	for _, mode := range []string{"nil-parent", "wrong-phase", "no-initial-graphs", "ready-parent", "closed-parent", "stopped-scope", "occupied-slot", "binding-mismatch", "root-mismatch", "canceled-construction"} {
		test.Run("constructor/"+mode, func(test *testing.T) {
			fixture := legacyNativeSourceW88Prepare(test, "same-root")
			parent := fixture.parent
			argument := parent
			current, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "nil-parent":
				argument = nil
			case "wrong-phase":
				parent.phase = 5
			case "no-initial-graphs":
				parent.state.InitialGraphCount = 0
			case "ready-parent":
				parent.ready = true
			case "closed-parent":
				parent.close()
			case "stopped-scope":
				fixture.native.mutation.stop()
			case "occupied-slot":
				fixture.bind(test, false)
			case "binding-mismatch":
				parent.state.Binding = HashContent([]byte("W88 different actual parent binding"))
			case "root-mismatch":
				parent.initialValues[0] = map[string]any{"kept": true}
			case "canceled-construction":
				cancel()
			}
			slot := parent.initialTask
			reads, writes := fixture.storage.reads, fixture.storage.writes
			rejected, err := newLegacyNativeCheckpointGoGraphSetForObject(current, argument)
			if rejected != nil || err == nil || parent.initialTask != slot || fixture.storage.reads != reads || fixture.storage.writes != writes {
				test.Fatal("invalid owner construction returned work, replaced a slot, or performed IO", mode, err)
			}
			if mode == "canceled-construction" {
				if !errors.Is(err, context.Canceled) {
					test.Fatal("constructor lost current cancellation", err)
				}
				fixture.bind(test, false)
			} else if !errors.Is(err, ErrIntegrity) && !errors.Is(err, ErrConflict) {
				test.Fatal("invalid parent did not produce an owner/shape rejection", err)
			}
			parent.close()
			if mode == "stopped-scope" {
				legacyNormalizationCaptureRQ87Stopped(test, fixture.native.mutation.enter())
			} else {
				legacyNormalizationCaptureRQ87Idle(test, fixture.native.mutation)
			}
		})
	}
	for _, boundary := range []string{"step", "ready-take"} {
		for _, mode := range []string{"detached-slot", "replaced-slot", "different-native-source", "different-scanner", "changed-parent-root", "changed-destination-root", "parent-binding", "set-binding", "phase", "ready", "scope-stop"} {
			test.Run(boundary+"/"+mode, func(test *testing.T) {
				fixture := legacyNativeSourceW88Prepare(test, "same-root")
				child := fixture.bind(test, false)
				if boundary == "ready-take" {
					legacyNativeGoGraphSetW85Ready[legacyNativeCheckpointRef](test, child, fixture.storage, legacyNativeEventW82FenceBudget)
				}
				parent := fixture.parent
				switch mode {
				case "detached-slot":
					parent.initialTask = nil
				case "replaced-slot":
					other := legacyNativeSourceW88Prepare(test, "same-root")
					replacement := other.bind(test, false)
					parent.initialTask = replacement
					defer func() { parent.initialTask = child }()
				case "different-native-source":
					other := legacyNativeSourceW88Prepare(test, "same-root")
					parent.native = other.native
				case "different-scanner":
					changed := *parent.scanner
					changed.complete = !changed.complete
					parent.scanner = &changed
				case "changed-parent-root":
					parent.initialValues[0] = map[string]any{"kept": true}
				case "changed-destination-root":
					value := fixture.native.destination.Interface().(*wire)
					value.Input = map[string]any{"kept": math.Copysign(0, -1), "nested": []any{true, nil, "original"}}
				case "parent-binding":
					parent.state.Binding = HashContent([]byte("W88 changed actual parent binding"))
				case "set-binding":
					child.state.Binding = HashContent([]byte("W88 substituted root-set binding"))
				case "phase":
					parent.phase = 7
				case "ready":
					parent.ready = true
				case "scope-stop":
					fixture.native.mutation.stop()
				}
				if err := child.checkOwner(); !errors.Is(err, ErrConflict) {
					test.Fatal("owned source did not observe the actual parent/slot/root fence", mode, err)
				}
				reads, writes := fixture.storage.reads, fixture.storage.writes
				if boundary == "step" {
					if ready, err := legacyNativeGoGraphSetW85Step[legacyNativeCheckpointRef](test, child, fixture.storage); ready || !errors.Is(err, ErrConflict) {
						test.Fatal("owner-invalid set advanced instead of failing closed", ready, err)
					}
				} else if root, err := child.take(context.Background()); root != (legacyNativeCheckpointRef{}) || !errors.Is(err, ErrConflict) {
					test.Fatal("owner-invalid ready set transferred a root", root, err)
				}
				legacyNativeGoGraphSetW85Failed[legacyNativeCheckpointRef](test, child, fixture.storage, ErrConflict)
				if fixture.storage.reads != reads || fixture.storage.writes != writes {
					test.Fatal("owner rejection performed dependent CAS IO")
				}
			})
		}
	}
}

func TestLegacyNativeSourceW88ContextScopeAndFinalTake(test *testing.T) {
	for _, ending := range []string{"child-close-parent-take", "parent-close", "parent-failure", "cancel-child-step", "cancel-child-take", "cas-failure", "cas-cancellation", "parent-take-budget"} {
		test.Run(ending, func(test *testing.T) {
			fixture := legacyNativeSourceW88Prepare(test, "full-backing")
			parent := fixture.parent
			child := fixture.bind(test, false)
			grant := parent.capture
			references := grant.references.Load()
			expected := errors.New("W88 current owned CAS failure")
			switch ending {
			case "child-close-parent-take":
				child.close()
				child.close()
				legacyNativeGoGraphSetW85Released[legacyNativeCheckpointRef](test, child)
				if !grant.valid(fixture.native.mutation) || grant.references.Load() != references || parent.closed {
					test.Fatal("closing only the child released or stopped the real parent's capture")
				}
				legacyNormalizationCaptureRQ87Busy(test, fixture.native.mutation.enter())
				parent.initialTask = nil
				fixture.bind(test, true)
				legacyNativeSourceW88ParentReady(test, fixture)
				if root, err := parent.take(context.Background()); err != nil || !root.valid() {
					test.Fatal("real parent could not finish after child-only cleanup", err)
				}
			case "parent-close", "parent-failure":
				if _, err := legacyNativeGoGraphSetW85Step[legacyNativeCheckpointRef](test, child, fixture.storage); err != nil || child.prepare == nil {
					test.Fatal("owned importer did not start before parent cleanup", err)
				}
				importer, registry := child.prepare, child.registry
				if ending == "parent-failure" {
					parent.fail(expected)
					legacyNativeGoGraphSetW85Failed[legacyNativeCheckpointRef](test, parent, fixture.storage, expected)
				} else {
					parent.close()
					parent.close()
				}
				if !child.closed || !importer.closed || !registry.closed {
					test.Fatal("parent termination retained its owned child or importer registry")
				}
				legacyNativeGoGraphSetW85Released[legacyNativeCheckpointRef](test, child)
				if root, err := child.take(context.Background()); err == nil || root != (legacyNativeCheckpointRef{}) {
					test.Fatal("parent-terminated child transferred a partial graph set", err)
				}
			case "cancel-child-step", "cancel-child-take":
				if ending == "cancel-child-take" {
					legacyNativeGoGraphSetW85Ready[legacyNativeCheckpointRef](test, child, fixture.storage, legacyNativeEventW82FenceBudget)
				}
				current, cancel := context.WithCancel(context.Background())
				cancel()
				reads, writes := fixture.storage.reads, fixture.storage.writes
				if ending == "cancel-child-step" {
					if ready, err := child.step(current); ready || !errors.Is(err, context.Canceled) {
						test.Fatal("owned step ignored its current cancellation", ready, err)
					}
				} else if root, err := child.take(current); root != (legacyNativeCheckpointRef{}) || !errors.Is(err, context.Canceled) {
					test.Fatal("owned take ignored its current cancellation", err)
				}
				legacyNativeGoGraphSetW85Failed[legacyNativeCheckpointRef](test, child, fixture.storage, context.Canceled)
				if fixture.storage.reads != reads || fixture.storage.writes != writes || !grant.valid(fixture.native.mutation) || grant.references.Load() != references {
					test.Fatal("canceled child touched CAS or consumed the parent's scope grant")
				}
				legacyNormalizationCaptureRQ87Busy(test, fixture.native.mutation.enter())
				parent.close()
			case "cas-failure", "cas-cancellation":
				if ending == "cas-failure" {
					fixture.storage.failure = expected
				} else {
					expected = context.Canceled
				}
				writes := fixture.storage.writes
				var failure error
				for attempt := 0; attempt < legacyNativeEventW82FenceSteps; attempt++ {
					readsBefore, writesBefore, transferred := fixture.storage.reads, fixture.storage.writes, fixture.storage.transferred
					current, cancel := context.WithCancel(context.Background())
					fixture.storage.current = current
					if ending == "cas-cancellation" {
						fixture.storage.cancelIO = cancel
					}
					ready, err := child.step(current)
					cancel()
					fixture.storage.current, fixture.storage.cancelIO = nil, nil
					if fixture.storage.reads-readsBefore+fixture.storage.writes-writesBefore > 32 || fixture.storage.transferred-transferred > LegacyCanonicalEncodingBytes {
						test.Fatal("owned failure path escaped the original IO quantum")
					}
					failure = err
					if err != nil || ready {
						break
					}
				}
				if fixture.storage.writes == writes || !errors.Is(failure, expected) {
					test.Fatal("owned child never observed the current actual CAS failure", failure)
				}
				fixture.storage.failure = nil
				legacyNativeGoGraphSetW85Failed[legacyNativeCheckpointRef](test, child, fixture.storage, expected)
				if !grant.valid(fixture.native.mutation) || grant.references.Load() != references {
					test.Fatal("child CAS failure released the parent's capture prematurely")
				}
				if ready, err := legacyNativeGoGraphSetW85Step[legacyNativeCheckpointRef](test, parent, fixture.storage); ready || !errors.Is(err, expected) {
					test.Fatal("actual phase six failed to propagate its child's sticky CAS error", ready, err)
				}
				legacyNativeGoGraphSetW85Failed[legacyNativeCheckpointRef](test, parent, fixture.storage, expected)
			case "parent-take-budget":
				legacyNativeSourceW88ParentReady(test, fixture)
				metadata, err := parent.retainedBytes()
				if err != nil {
					test.Fatal(err)
				}
				source, err := parent.native.retained()
				if err != nil {
					test.Fatal(err)
				}
				parent.budget = metadata + source - 1
				if parent.checkSource() != nil || !grant.valid(fixture.native.mutation) {
					test.Fatal("final-take resource counterexample invalidated the source")
				}
				reads, writes := fixture.storage.reads, fixture.storage.writes
				if root, err := parent.take(context.Background()); root != (legacyNativeCheckpointRef{}) || !errors.Is(err, ErrDocumentResourceLimit) {
					test.Fatal("final parent take ignored its current aggregate source budget", root, err)
				}
				legacyNativeGoGraphSetW85Failed[legacyNativeCheckpointRef](test, parent, fixture.storage, ErrDocumentResourceLimit)
				if fixture.storage.reads != reads || fixture.storage.writes != writes {
					test.Fatal("final parent take used IO after live-budget rejection")
				}
			}
			fixture.unchanged(test)
			legacyNativeGoGraphSetW85Released[legacyNativeCheckpointRef](test, parent)
			legacyNormalizationCaptureRQ87Idle(test, fixture.native.mutation)
		})
	}
}
