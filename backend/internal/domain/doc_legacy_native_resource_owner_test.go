package domain

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

func legacyNativeResourceW90BigFixture(test *testing.T) *legacyNativeSourceW88Fixture {
	test.Helper()
	backing := []any{strings.Repeat("retained outside narrow view", 32768), true, nil}
	array := backing[:1:3]
	request, native, scanner, source := legacyNativeEventW82FencePrepare(test, `{}`, &wire{Input: map[string]any{"array": array}, Output: array}, nil)
	fixture := &legacyNativeSourceW88Fixture{native: native, scanner: scanner, source: source, request: request, storage: legacyNativeGoGraphSetW85Storage(test)}
	fixture.before = legacyNormalizationCaptureRQ87Observe(test, native, scanner, source.Bytes)
	parent, err := newLegacyNativeObjectCheckpoint(context.Background(), native, scanner, request.Binding, source, fixture.storage.io, legacyNativeEventW82FenceBudget)
	if err != nil {
		test.Fatal(err)
	}
	fixture.parent = parent
	test.Cleanup(parent.close)
	for attempt := 0; attempt < legacyNativeEventW82FenceSteps; attempt++ {
		if parent.phase == 6 {
			return fixture
		}
		if ready, err := legacyNativeGoGraphSetW85Step[legacyNativeCheckpointRef](test, parent, fixture.storage); err != nil || ready {
			test.Fatal("did not reach the real source owner", ready, err)
		}
	}
	test.Fatal("resource fixture exceeded the original phase dispatch bound")
	return nil
}

func legacyNativeResourceW90Backing(test *testing.T, fixture *legacyNativeSourceW88Fixture, capacity int) (*legacyNativeCheckpointGoGraphSet, *legacyNativeCheckpointGoGraphEntry) {
	test.Helper()
	child := fixture.bind(test, false)
	legacyNativeGoGraphSetW85Ready[legacyNativeCheckpointRef](test, child, fixture.storage, fixture.parent.budget)
	for entry := child.registry.first; entry != nil; entry = entry.next {
		if entry.array != nil && entry.key.capacity == capacity && entry.complete {
			return child, entry
		}
	}
	test.Fatal("real imported backing is absent")
	return nil, nil
}

func legacyNativeResourceW90Prepare(test *testing.T, fixture *legacyNativeSourceW88Fixture, child *legacyNativeCheckpointGoGraphSet, entry *legacyNativeCheckpointGoGraphEntry) *legacyNativeResourcePreparation {
	test.Helper()
	work, err := newLegacyNativeResourcePreparation(context.Background(), fixture.parent, child.registry, entry.key)
	if err != nil || work == nil || fixture.parent.resourceTask != work {
		test.Fatal("actual resource preparation did not register in its owner", err)
	}
	return work
}

func legacyNativeResourceW90Ready(test *testing.T, work *legacyNativeResourcePreparation, fixture *legacyNativeSourceW88Fixture) {
	test.Helper()
	reads, writes := fixture.storage.reads, fixture.storage.writes
	for attempt := 0; attempt < legacyNativeGoGraphSetW85Steps; attempt++ {
		ready, err := work.step(context.Background())
		if err != nil {
			test.Fatal("resource preparation failed", err)
		}
		if fixture.storage.reads != reads || fixture.storage.writes != writes {
			test.Fatal("resource retention performed CAS IO")
		}
		legacyNativeSourceW88Accounting(test, fixture.parent, nil)
		if ready {
			return
		}
	}
	test.Fatal("resource retention exceeded the original finite schedule")
}

func TestLegacyNativeResourceW90WholeBackingTransfer(test *testing.T) {
	fixture := legacyNativeResourceW90BigFixture(test)
	child, entry := legacyNativeResourceW90Backing(test, fixture, 3)
	work := legacyNativeResourceW90Prepare(test, fixture, child, entry)
	legacyNativeResourceW90Ready(test, work, fixture)
	owner, err := work.take(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	defer owner.close()
	if owner.count != 1 {
		test.Fatal("one backing invented unrelated retained objects", owner.count)
	}
	narrow, err := owner.view(context.Background(), 1, 1, 1)
	if err != nil {
		test.Fatal(err)
	}
	wide, err := owner.view(context.Background(), 0, 1, 3)
	if err != nil {
		test.Fatal(err)
	}
	child.close()
	fixture.parent.initialTask = nil
	fixture.parent.close()
	values, err := narrow.values(context.Background())
	if err != nil || len(values) != 1 || cap(values) != 1 || values[0] != true {
		test.Fatal("narrow view did not survive registry and parent closure", values, err)
	}
	backing, err := wide.values(context.Background())
	if err != nil || len(backing) != 1 || cap(backing) != 3 || &backing[:3][1] != &values[0] {
		test.Fatal("views lost actual shared physical slots", err)
	}
	if retained, err := narrow.retainedBytes(); err != nil || retained < int64(len(backing[0].(string))) {
		test.Fatal("narrow owner undercounts the real retained backing payload", retained, len(backing[0].(string)), err)
	}
	legacyNormalizationCaptureRQ87Busy(test, fixture.native.mutation.enter())
	owner.close()
	owner.close()
	if _, err := narrow.values(context.Background()); !errors.Is(err, ErrConflict) {
		test.Fatal("released view retained valid ownership", err)
	}
	if retained, err := owner.retainedBytes(); err != nil || retained != 0 {
		test.Fatal("released owner kept resource charges", retained, err)
	}
	if err := fixture.native.mutation.enter(); err != nil {
		test.Fatal("resource release did not resume normal mutations", err)
	}
	fixture.native.mutation.leave()
}

func TestLegacyNativeResourceW90SharedClosureAndMutation(test *testing.T) {
	fixture := legacyNativeSourceW88Prepare(test, "full-backing")
	child, entry := legacyNativeResourceW90Backing(test, fixture, 4)
	work := legacyNativeResourceW90Prepare(test, fixture, child, entry)
	legacyNativeResourceW90Ready(test, work, fixture)
	owner, err := work.take(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	defer owner.close()
	if owner.count != 3 {
		test.Fatal("shared map and nested array were not deduplicated", owner.count)
	}
	shared, err := entry.value.owner.array.tailValue(context.Background(), 1)
	if err != nil || shared.owner.object == nil {
		test.Fatal("actual backing omitted shared child metadata", err)
	}
	rootTotal := entry.value.heap.bytes + entry.value.owner.bytes
	sharedTotal := shared.heap.bytes + shared.owner.bytes
	if owner.bytes != rootTotal-sharedTotal {
		test.Fatal("union does not equal the independently specified three-object closure", owner.bytes, rootTotal, sharedTotal)
	}
	before := shared.owner.object.heap()
	if _, err := shared.owner.object.putValue("blocked", 7, legacyNativeAnyValue{}); !errors.Is(err, ErrLegacyNormalizationBusy) || shared.owner.object.heap() != before {
		test.Fatal("actual captured map mutator changed a retained resource", err)
	}
	if _, err := entry.array.appendValue(context.Background(), &legacyNormalizationInput{}, legacyNativeAnyValue{}); !errors.Is(err, ErrLegacyNormalizationBusy) {
		test.Fatal("actual array append ignored its resource capture", err)
	}
	if _, err := entry.array.appendTailValueGuarded(context.Background(), &legacyNormalizationInput{}, legacyNativeAnyValue{}, nil); !errors.Is(err, ErrLegacyNormalizationBusy) {
		test.Fatal("actual hidden-tail append ignored its resource capture", err)
	}
	view, err := owner.view(context.Background(), 1, 1, 1)
	if err != nil {
		test.Fatal(err)
	}
	values, err := view.values(context.Background())
	if err != nil || reflect.ValueOf(values[0]).Pointer() != reflect.ValueOf(shared.value).Pointer() {
		test.Fatal("view copied or lost its shared map slot", err)
	}
	fixture.unchanged(test)
}

func TestLegacyNativeResourceW91AllocationMetadata(test *testing.T) {
	fixture := legacyNativeSourceW88Prepare(test, "full-backing")
	child, entry := legacyNativeResourceW90Backing(test, fixture, 4)
	work := legacyNativeResourceW90Prepare(test, fixture, child, entry)
	legacyNativeResourceW90Ready(test, work, fixture)
	var expected docRangeHeaderHeapEstimate
	expected.allocation(1, int64(unsafe.Sizeof(*work)))
	expected.include(work.root.heap)
	expected.include(docRangeHeaderHeapEstimate{bytes: work.root.owner.bytes})
	expected.allocation(1, int64(unsafe.Sizeof(legacyNormalizationCapture{})))
	for index := int64(0); index < work.indices; index++ {
		expected.allocation(1, int64(unsafe.Sizeof(legacyNativeResourceLookup{})))
	}
	for node := work.first; node != nil; node = node.next {
		expected.allocation(1, int64(unsafe.Sizeof(*node)))
	}
	actual, err := work.retainedBytes()
	if err != nil || expected.err != nil || actual != expected.bytes {
		test.Fatal("resource preparation omitted actual per-allocation metadata", actual, expected.bytes, err, expected.err)
	}
	owner, err := work.take(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	defer owner.close()
	expected = docRangeHeaderHeapEstimate{bytes: owner.bytes}
	expected.allocation(1, int64(unsafe.Sizeof(*owner)))
	for node := owner.nodes; node != nil; node = node.next {
		expected.allocation(1, int64(unsafe.Sizeof(*node)))
	}
	expected.allocation(1, int64(unsafe.Sizeof(legacyNormalizationCapture{})))
	expected.allocation(1, int64(unsafe.Sizeof(legacyNormalizationMutation{})))
	actual, err = owner.retainedBytes()
	if err != nil || expected.err != nil || actual != expected.bytes {
		test.Fatal("transferred resource omitted actual per-allocation metadata", actual, expected.bytes, err, expected.err)
	}
}

func TestLegacyNativeResourceW90AdmissionAndLifetime(test *testing.T) {
	for _, ending := range []string{"constructor-budget", "step-cancel", "take-cancel", "take-budget", "scope-stop", "parent-close"} {
		test.Run(ending, func(test *testing.T) {
			fixture := legacyNativeSourceW88Prepare(test, "full-backing")
			child, entry := legacyNativeResourceW90Backing(test, fixture, 4)
			grant := fixture.parent.capture
			before := grant.references.Load()
			if ending == "constructor-budget" {
				metadata, err := fixture.parent.retainedBytes()
				if err != nil {
					test.Fatal(err)
				}
				source, err := fixture.parent.native.retained()
				if err != nil {
					test.Fatal(err)
				}
				fixture.parent.budget = metadata + source + 1
				work, err := newLegacyNativeResourcePreparation(context.Background(), fixture.parent, child.registry, entry.key)
				if work != nil || !errors.Is(err, ErrDocumentResourceLimit) || fixture.parent.resourceTask != nil || grant.references.Load() != before {
					test.Fatal("constructor retained unadmitted resources or capture", err)
				}
				return
			}
			work := legacyNativeResourceW90Prepare(test, fixture, child, entry)
			if ending != "step-cancel" && ending != "parent-close" {
				legacyNativeResourceW90Ready(test, work, fixture)
			}
			switch ending {
			case "step-cancel", "take-cancel":
				current, cancel := context.WithCancel(context.Background())
				cancel()
				if ending == "step-cancel" {
					_, err := work.step(current)
					if !errors.Is(err, context.Canceled) {
						test.Fatal(err)
					}
				} else if owner, err := work.take(current); owner != nil || !errors.Is(err, context.Canceled) {
					test.Fatal("canceled take returned resources", err)
				}
			case "take-budget":
				metadata, _ := fixture.parent.retainedBytes()
				source, _ := fixture.parent.native.retained()
				fixture.parent.budget = metadata + source + 1
				if owner, err := work.take(context.Background()); owner != nil || !errors.Is(err, ErrDocumentResourceLimit) {
					test.Fatal("resource take ignored current aggregate budget", err)
				}
			case "scope-stop":
				fixture.native.mutation.stop()
				if owner, err := work.take(context.Background()); owner != nil || !errors.Is(err, ErrConflict) {
					test.Fatal("stopped source produced an owned result", err)
				}
			case "parent-close":
				fixture.parent.close()
				if !work.closed {
					test.Fatal("parent retained its resource preparation")
				}
			}
			if retained, err := work.retainedBytes(); err != nil || retained != 0 {
				test.Fatal("failed preparation retained a resource graph", retained, err)
			}
			if ending != "parent-close" && grant.references.Load() != before {
				test.Fatal("failed preparation leaked the extra capture reference", grant.references.Load(), before)
			}
		})
	}
}

func TestLegacyNativeResourceW90MembershipAndViewFences(test *testing.T) {
	for _, invalid := range []string{"missing-parent", "raw-registry", "wrong-parent", "wrong-array-key", "constructor-cancel"} {
		test.Run(invalid, func(test *testing.T) {
			fixture := legacyNativeSourceW88Prepare(test, "full-backing")
			child, entry := legacyNativeResourceW90Backing(test, fixture, 4)
			parent, registry, key := fixture.parent, child.registry, entry.key
			expected := ErrConflict
			current := context.Background()
			before := parent.capture.references.Load()
			switch invalid {
			case "missing-parent":
				parent = nil
			case "raw-registry":
				registry = newLegacyNativeCheckpointGoGraphRegistry()
			case "wrong-parent":
				foreign := legacyNativeSourceW88Prepare(test, "full-backing")
				parent = foreign.parent
			case "wrong-array-key":
				key.capacity++
				expected = ErrIntegrity
			case "constructor-cancel":
				canceled, cancel := context.WithCancel(context.Background())
				cancel()
				current, expected = canceled, context.Canceled
			}
			work, err := newLegacyNativeResourcePreparation(current, parent, registry, key)
			if work != nil || !errors.Is(err, expected) || fixture.parent.resourceTask != nil || fixture.parent.capture.references.Load() != before {
				test.Fatal("invalid membership acquired ownership or capture", err)
			}
		})
	}
	test.Run("views-and-current-scope", func(test *testing.T) {
		fixture := legacyNativeSourceW88Prepare(test, "full-backing")
		child, entry := legacyNativeResourceW90Backing(test, fixture, 4)
		work := legacyNativeResourceW90Prepare(test, fixture, child, entry)
		legacyNativeResourceW90Ready(test, work, fixture)
		owner, err := work.take(context.Background())
		if err != nil {
			test.Fatal(err)
		}
		defer owner.close()
		for _, bounds := range [][3]int{{-1, 0, 0}, {0, -1, 0}, {0, 2, 1}, {5, 0, 0}, {4, 0, 1}, {0, 0, int(^uint(0) >> 1)}} {
			view, err := owner.view(context.Background(), bounds[0], bounds[1], bounds[2])
			if view != nil || !errors.Is(err, ErrIntegrity) {
				test.Fatal("invalid view bounds acquired backing", bounds, err)
			}
		}
		empty, err := owner.view(context.Background(), 4, 0, 0)
		if err != nil {
			test.Fatal(err)
		}
		if values, err := empty.values(context.Background()); err != nil || values == nil || len(values) != 0 || cap(values) != 0 {
			test.Fatal("valid zero-capacity view changed nilness", values, err)
		}
		fixture.native.mutation.stop()
		if _, err := empty.values(context.Background()); !errors.Is(err, ErrConflict) {
			test.Fatal("stopped source left a valid backing view", err)
		}
		if _, err := owner.retainedBytes(); !errors.Is(err, ErrConflict) {
			test.Fatal("stopped source left valid resource accounting", err)
		}
	})
}
