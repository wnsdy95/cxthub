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

type legacyNativeGoGraphW86Fixture struct {
	name  string
	roots []any
}

func legacyNativeGoGraphW86Fixtures() []legacyNativeGoGraphW86Fixture {
	sharedMap := map[string]any{"text": "shared", "zero": math.Copysign(0, -1)}
	separateMap := map[string]any{"text": "shared", "zero": math.Copysign(0, -1)}
	sharedBacking := []any{"visible", "hidden", sharedMap}
	sharedArray := sharedBacking[:1:3]
	separateBacking := []any{"visible", "hidden", sharedMap}
	separateArray := separateBacking[:1:3]
	nestedBacking := []any{true, "nested hidden", sharedMap}
	backing := []any{
		sharedMap, "\x00\"\\\n한글🙂", math.Copysign(0, -1), nil,
		map[string]any(nil), []any(nil), map[string]any{}, make([]any, 0),
		map[string]any{"array": nestedBacking[:1:3], "empty": map[string]any{}},
		sharedArray, sharedMap, separateMap, sharedArray, separateArray,
	}
	emptyBacking := []any{"entirely hidden", math.Copysign(0, -1), sharedMap, sharedArray}
	largeBacking := []any{true, strings.Repeat("한🙂\x00\\\"", LegacyCanonicalEncodingBytes/3+19)}
	wideBacking := make([]any, 97)
	for position := range wideBacking {
		wideBacking[position] = float64(position)
	}
	wideBacking[32], wideBacking[96] = sharedMap, sharedMap
	return []legacyNativeGoGraphW86Fixture{
		{name: "mixed-aliases-and-separate-equal-values", roots: []any{backing[:1:14], backing[:1:14], sharedMap, sharedArray, separateMap, separateArray}},
		{name: "zero-visible-positive-capacity", roots: []any{emptyBacking[:0:4], emptyBacking[:0:4], sharedMap}},
		{name: "large-hidden-scalar", roots: []any{largeBacking[:1:2]}},
		{name: "hidden-members-cross-metadata-pages", roots: []any{wideBacking[:0:97]}},
	}
}

func legacyNativeGoGraphW86Values(test *testing.T, expected []any, actual []legacyNativeAnyValue) {
	test.Helper()
	if len(expected) != len(actual) {
		test.Fatal("full backing inspection lost roots", len(expected), len(actual))
	}
	inspector := &legacyNativeGoGraphW85Inspector{}
	sources := make(map[legacyNativeGoGraphW85Identity]any)
	for position, source := range expected {
		inspector.check(test, source, actual[position])
		for _, record := range legacyNativeGoGraphW85SourceSnapshot(source) {
			sources[legacyNativeGoGraphW85IdentityOf(record.source)] = record.source
		}
	}
	checked := make(map[legacyNativeGoGraphW85Identity]bool)
	for round := 0; round <= len(sources); round++ {
		progress, pending := false, false
		for identity, source := range sources {
			decoded, array := source.([]any)
			if !array || checked[identity] {
				continue
			}
			owner, present := inspector.owners[identity]
			if !present {
				pending = true
				continue
			}
			if owner.array == nil || owner.array.validateTail(true) != nil {
				test.Fatal("full backing lacks complete actual native tail metadata")
			}
			owned := owner.array.values()
			if len(owned) != len(decoded) || cap(owned) != cap(decoded) {
				test.Fatal("full backing changed physical visible length or capacity")
			}
			payload := legacyNativeAnyArrayBacking(cap(decoded))
			payload.allocation(1, int64(unsafe.Sizeof(decoded)))
			var minimumMetadata docRangeHeaderHeapEstimate
			minimumMetadata.allocation(1, int64(unsafe.Sizeof(*owner.array)))
			minimumMetadata.allocation(int64(cap(decoded)), int64(unsafe.Sizeof(legacyNativeAnyArrayChild{})))
			var minimumExcess int64
			for position, sourceChild := range decoded[:cap(decoded)] {
				var child legacyNativeAnyValue
				var failure error
				if position < len(decoded) {
					child, failure = owner.arrayValue(context.Background(), position)
				} else {
					child, failure = owner.array.tailValue(context.Background(), position)
				}
				if failure != nil || !legacyNativeGoGraphW85SameSource(child.value, owned[:cap(owned)][position]) {
					test.Fatal("physical backing slot disagrees with actual child metadata", position, failure)
				}
				inspector.check(test, sourceChild, child)
				payload.include(child.heap)
				minimumMetadata.include(docRangeHeaderHeapEstimate{bytes: child.owner.bytes})
				minimumExcess += child.excess
			}
			var retained docRangeHeaderHeapEstimate
			owner.array.retained(&retained)
			if payload.err != nil || minimumMetadata.err != nil || retained.err != nil ||
				owner.array.heap().bytes < payload.bytes || retained.bytes < minimumMetadata.bytes ||
				owner.array.backingExcess() < minimumExcess {
				test.Fatal("native array omitted full backing payload, excess, or child owner metadata", payload, minimumMetadata, retained)
			}
			checked[identity], progress = true, true
		}
		if !pending {
			break
		}
		if !progress || round == len(sources) {
			test.Fatal("hidden descendant native owners were not reachable within the finite source graph")
		}
	}
	for identity := range sources {
		if _, present := inspector.owners[identity]; !present {
			test.Fatal("full backing traversal omitted a nonnil source container")
		}
	}
}

func legacyNativeGoGraphW86State(test *testing.T, storage *legacyNativeGoGraphSetW85Store, saved legacyNativeCheckpointRef) legacyNativeCheckpointGoGraphSetState {
	test.Helper()
	var state legacyNativeCheckpointGoGraphSetState
	body, present := storage.objects[saved.Hash]
	if !present || len(body) != saved.Bytes || HashContent(body) != saved.Hash {
		test.Fatal("tail fixture did not locate actual root set CAS bytes")
	}
	if failure := json.Unmarshal(body, &state); failure != nil || state.validate(state.Binding) != nil {
		test.Fatal("tail fixture did not decode an actual root set", failure)
	}
	return state
}

func legacyNativeGoGraphW86Owner(test *testing.T, storage *legacyNativeGoGraphSetW85Store, descriptor legacyNativeCheckpointValue) legacyNativeCheckpointOwner {
	test.Helper()
	var owner legacyNativeCheckpointOwner
	body, present := storage.objects[descriptor.Body.Hash]
	if !present || !descriptor.Body.valid() || len(body) != descriptor.Body.Bytes || HashContent(body) != descriptor.Body.Hash {
		test.Fatal("tail fixture did not locate an actual owner CAS record")
	}
	if failure := json.Unmarshal(body, &owner); failure != nil {
		test.Fatal("tail fixture could not decode the existing owner schema", failure)
	}
	return owner
}

func legacyNativeGoGraphW86ChangedOwner(test *testing.T, storage *legacyNativeGoGraphSetW85Store, saved legacyNativeCheckpointRef, body []byte) legacyNativeCheckpointRef {
	test.Helper()
	state := legacyNativeGoGraphW86State(test, storage, saved)
	original := state.Roots[0].Body
	changed := legacyNativeCheckpointRef{Hash: HashContent(body), Bytes: len(body)}
	storage.objects[changed.Hash] = bytes.Clone(body)
	for position := range state.Roots {
		if state.Roots[position].Body == original {
			state.Roots[position].Body = changed
		}
	}
	rootBody, failure := json.Marshal(state)
	if failure != nil {
		test.Fatal("could not rebind actual root descriptors to the malformed owner", failure)
	}
	result := legacyNativeCheckpointRef{Hash: HashContent(rootBody), Bytes: len(rootBody)}
	storage.objects[result.Hash] = rootBody
	return result
}

func legacyNativeGoGraphW86ImportReject(test *testing.T, source any, budget int64, expected error) {
	test.Helper()
	registry := newLegacyNativeCheckpointGoGraphRegistry()
	defer registry.close()
	work, failure := newLegacyNativeCheckpointGoGraph(context.Background(), source, registry, budget)
	if work != nil {
		defer work.close()
	}
	if failure == nil {
		if work == nil {
			test.Fatal("tail importer returned an empty constructor success")
		}
		for attempt := 0; attempt < legacyNativeGoGraphW85StepLimit; attempt++ {
			current, cancel := context.WithCancel(context.Background())
			ready, stepFailure := work.step(current)
			cancel()
			if ready {
				test.Fatal("invalid or unadmitted hidden backing became successful", stepFailure)
			}
			if stepFailure != nil {
				failure = stepFailure
				break
			}
			legacyNativeGoGraphW85BudgetCheck(test, work, registry, budget)
		}
	}
	if !errors.Is(failure, expected) {
		test.Fatal("hidden backing did not receive its explicit original rejection", failure, expected)
	}
	if work != nil {
		legacyNativeGoGraphW85Failure(test, work, registry, failure)
	} else if retained, retainedFailure := registry.retainedBytes(); retainedFailure != nil || retained != 0 {
		test.Fatal("rejected tail constructor retained an incomplete registry", retained, retainedFailure)
	}
}

func TestLegacyNativeGoGraphW86FullBacking(test *testing.T) {
	for _, fixture := range legacyNativeGoGraphW86Fixtures() {
		test.Run(fixture.name, func(test *testing.T) {
			snapshot := legacyNativeGoGraphW85SourceSnapshot(fixture.roots)
			defer legacyNativeGoGraphW85SourceUnchanged(test, snapshot)
			registry := newLegacyNativeCheckpointGoGraphRegistry()
			defer registry.close()
			imported := make([]legacyNativeAnyValue, len(fixture.roots))
			for position, source := range fixture.roots {
				imported[position], _ = legacyNativeGoGraphW85Import(test, source, registry)
			}
			legacyNativeGoGraphW86Values(test, fixture.roots, imported)
			legacyNativeGoGraphW85RegistryReleased(test, registry)
			legacyNativeGoGraphW86Values(test, fixture.roots, imported)
			binding := HashContent([]byte("W86 full backing " + fixture.name))
			storage := legacyNativeGoGraphSetW85Storage(test)
			saved := legacyNativeGoGraphSetW85Capture(test, fixture.roots, binding, storage)
			state := legacyNativeGoGraphW86State(test, storage, saved)
			for position, source := range fixture.roots {
				decoded, array := source.([]any)
				if !array || cap(decoded) <= len(decoded) {
					continue
				}
				owner := legacyNativeGoGraphW86Owner(test, storage, state.Roots[position])
				bounds := legacyNativeValueCheckpointBounds{PayloadBytes: legacyNativeValueCheckpointPayloadLimit(), Nodes: legacyNativeCheckpointNodes}
				wire, failure := decodeLegacyNativeValueCheckpoint(context.Background(), state.Roots[position].Wire, bounds)
				if failure != nil || wire.Kind != legacyNativeCheckpointArray || wire.Length != uint64(len(decoded)) || wire.Capacity != uint64(cap(decoded)) ||
					!owner.FullBacking || owner.Visible != uint64(len(decoded)) || owner.Kind != legacyNativeCheckpointArray ||
					owner.Length != uint64(cap(decoded)) || owner.Capacity != owner.Length || owner.Members.Count != owner.Length {
					test.Fatal("actual owner schema did not separate visible wire length from complete backing members", owner, wire, failure)
				}
			}
			actual := legacyNativeGoGraphSetW85Restore(test, saved, binding, storage, 0)
			legacyNativeGoGraphW86Values(test, fixture.roots, actual)
			independent := legacyNativeGoGraphSetW85Restore(test, saved, binding, storage, 0)
			legacyNativeGoGraphW86Values(test, fixture.roots, independent)
			if actual[0].owner.array == independent[0].owner.array {
				test.Fatal("independent restore reused a released full backing owner registry")
			}
		})
	}
}

func TestLegacyNativeGoGraphW86OwnerSchema(test *testing.T) {
	binding := HashContent([]byte("W86 full backing owner schema"))
	storage := legacyNativeGoGraphSetW85Storage(test)
	backing := []any{true, "hidden", math.Copysign(0, -1)}
	roots := []any{backing[:1:3], backing[:1:3]}
	snapshot := legacyNativeGoGraphW85SourceSnapshot(roots)
	defer legacyNativeGoGraphW85SourceUnchanged(test, snapshot)
	saved := legacyNativeGoGraphSetW85Capture(test, roots, binding, storage)
	state := legacyNativeGoGraphW86State(test, storage, saved)
	owner := legacyNativeGoGraphW86Owner(test, storage, state.Roots[0])
	if !owner.FullBacking || owner.Visible != 1 || owner.Length != 3 || owner.Capacity != 3 || owner.Members.Count != 3 {
		test.Fatal("schema fences require a real complete nonnil hidden tail", owner)
	}
	for _, fixture := range []struct {
		name   string
		change func(*legacyNativeCheckpointOwner)
	}{
		{name: "disabled-with-visible", change: func(record *legacyNativeCheckpointOwner) { record.FullBacking = false }},
		{name: "disabled-full-length", change: func(record *legacyNativeCheckpointOwner) { record.FullBacking, record.Visible = false, 0 }},
		{name: "visible-equals-capacity", change: func(record *legacyNativeCheckpointOwner) { record.Visible = record.Capacity }},
		{name: "visible-over-capacity", change: func(record *legacyNativeCheckpointOwner) { record.Visible = record.Capacity + 1 }},
		{name: "visible-disagrees-with-wire", change: func(record *legacyNativeCheckpointOwner) { record.Visible = 0 }},
		{name: "length-is-visible", change: func(record *legacyNativeCheckpointOwner) { record.Length = record.Visible }},
		{name: "capacity-is-visible", change: func(record *legacyNativeCheckpointOwner) { record.Capacity = record.Visible }},
		{name: "member-count-is-visible", change: func(record *legacyNativeCheckpointOwner) { record.Members.Count = record.Visible }},
		{name: "missing-complete-members", change: func(record *legacyNativeCheckpointOwner) { record.Members = legacyNativeCheckpointTree{} }},
		{name: "maximum-below-capacity", change: func(record *legacyNativeCheckpointOwner) { record.Maximum = record.Capacity - 1 }},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			changed := owner
			fixture.change(&changed)
			body, failure := json.Marshal(changed)
			if failure != nil {
				test.Fatal(failure)
			}
			ref := legacyNativeGoGraphW86ChangedOwner(test, storage, saved, body)
			work, failure := newLegacyNativeCheckpointGoGraphSetRestore(context.Background(), ref, binding, storage.io, legacyNativeGoGraphSetW85Budget)
			legacyNativeGoGraphSetW85Reject[[]legacyNativeAnyValue](test, work, failure, storage, ErrIntegrity, legacyNativeGoGraphSetW85Budget)
		})
	}
	body := storage.objects[state.Roots[0].Body.Hash]
	for _, fixture := range []struct {
		name string
		body []byte
	}{
		{name: "full-backing-string", body: legacyNativeGoGraphSetW85Field(test, body, "f", []byte("\"true\""))},
		{name: "full-backing-null", body: legacyNativeGoGraphSetW85Field(test, body, "f", []byte("null"))},
		{name: "visible-negative", body: legacyNativeGoGraphSetW85Field(test, body, "l", []byte("-1"))},
		{name: "visible-string", body: legacyNativeGoGraphSetW85Field(test, body, "l", []byte("\"1\""))},
		{name: "visible-overflow", body: legacyNativeGoGraphSetW85Field(test, body, "l", []byte("18446744073709551616"))},
		{name: "duplicate-full-backing", body: append(bytes.Clone(body[:len(body)-1]), []byte(",\"f\":true}")...)},
		{name: "duplicate-visible", body: append(bytes.Clone(body[:len(body)-1]), []byte(",\"l\":1}")...)},
		{name: "unknown-tail-field", body: append(bytes.Clone(body[:len(body)-1]), []byte(",\"tail_w86\":true}")...)},
		{name: "trailing-owner-data", body: append(bytes.Clone(body), []byte("{}")...)},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			ref := legacyNativeGoGraphW86ChangedOwner(test, storage, saved, fixture.body)
			work, failure := newLegacyNativeCheckpointGoGraphSetRestore(context.Background(), ref, binding, storage.io, legacyNativeGoGraphSetW85Budget)
			legacyNativeGoGraphSetW85Reject[[]legacyNativeAnyValue](test, work, failure, storage, ErrIntegrity, legacyNativeGoGraphSetW85Budget)
		})
	}
	for _, fixture := range []struct {
		name    string
		full    bool
		visible uint64
	}{
		{name: "map-full-backing", full: true},
		{name: "map-visible-without-full", visible: 1},
		{name: "map-full-and-visible", full: true, visible: 1},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			object := map[string]any{"value": "map"}
			mapSaved := legacyNativeGoGraphSetW85Capture(test, []any{object, object}, binding, storage)
			mapState := legacyNativeGoGraphW86State(test, storage, mapSaved)
			record := legacyNativeGoGraphW86Owner(test, storage, mapState.Roots[0])
			record.FullBacking, record.Visible = fixture.full, fixture.visible
			changed, failure := json.Marshal(record)
			if failure != nil {
				test.Fatal(failure)
			}
			ref := legacyNativeGoGraphW86ChangedOwner(test, storage, mapSaved, changed)
			work, failure := newLegacyNativeCheckpointGoGraphSetRestore(context.Background(), ref, binding, storage.io, legacyNativeGoGraphSetW85Budget)
			legacyNativeGoGraphSetW85Reject[[]legacyNativeAnyValue](test, work, failure, storage, ErrIntegrity, legacyNativeGoGraphSetW85Budget)
		})
	}
	test.Run("nil-tail-keeps-existing-owner-format", func(test *testing.T) {
		list, failure := newLegacyNativeAnyArray(3)
		if failure != nil {
			test.Fatal(failure)
		}
		list.decoded = make([]any, 0, 3)
		list.estimate.include(legacyNativeAnyArrayBacking(3))
		if appended, failure := list.appendValueGuarded(context.Background(), &legacyNormalizationInput{}, legacyNativeAnyValue{}, nil); failure != nil || !appended {
			test.Fatal("could not construct existing tracked nil-tail owner", appended, failure)
		}
		value, failure := legacyNativeAnyCheckpointContainer(nil, list)
		if failure != nil {
			test.Fatal(failure)
		}
		identity := newLegacyNativeCheckpointGraphIdentity()
		defer identity.close()
		writer, failure := newLegacyNativeCheckpointGraphWriter(context.Background(), value, storage.io, identity, legacyNativeGoGraphSetW85Budget)
		if failure != nil {
			test.Fatal(failure)
		}
		defer writer.close()
		legacyNativeGoGraphSetW85Ready[legacyNativeCheckpointValue](test, writer, storage, legacyNativeGoGraphSetW85Budget)
		descriptor, failure := writer.take(context.Background())
		if failure != nil {
			test.Fatal(failure)
		}
		record := legacyNativeGoGraphW86Owner(test, storage, descriptor)
		var fields map[string]json.RawMessage
		if failure := json.Unmarshal(storage.objects[descriptor.Body.Hash], &fields); failure != nil {
			test.Fatal(failure)
		}
		if record.FullBacking || record.Visible != 0 || record.Length != 1 || record.Capacity != 3 || record.Members.Count != 1 || fields["f"] != nil || fields["l"] != nil {
			test.Fatal("existing nil-tail owner was silently converted to full backing format", record)
		}
	})
}

func TestLegacyNativeGoGraphW86TailFences(test *testing.T) {
	binding := HashContent([]byte("W86 tail resource context and integrity fences"))
	test.Run("hidden-payload-budget", func(test *testing.T) {
		backing := []any{strings.Repeat("x", 4<<20)}
		source := backing[:0:1]
		roots := []any{source}
		snapshot := legacyNativeGoGraphW85SourceSnapshot(roots)
		defer legacyNativeGoGraphW85SourceUnchanged(test, snapshot)
		budget := int64(len(backing[0].(string)) - 1)
		legacyNativeGoGraphW86ImportReject(test, source, budget, ErrDocumentResourceLimit)
		storage := legacyNativeGoGraphSetW85Storage(test)
		writer, failure := newLegacyNativeCheckpointGoGraphSet(context.Background(), roots, binding, storage.io, budget)
		legacyNativeGoGraphSetW85Reject[legacyNativeCheckpointRef](test, writer, failure, storage, ErrDocumentResourceLimit, budget)
		saved := legacyNativeGoGraphSetW85Capture(test, roots, binding, storage)
		restore, failure := newLegacyNativeCheckpointGoGraphSetRestore(context.Background(), saved, binding, storage.io, budget)
		legacyNativeGoGraphSetW85Reject[[]legacyNativeAnyValue](test, restore, failure, storage, ErrDocumentResourceLimit, budget)
	})
	cyclicBacking := make([]any, 1)
	cyclic := cyclicBacking[:0:1]
	cyclicBacking[0] = cyclic
	for _, fixture := range []struct {
		name   string
		hidden any
	}{
		{name: "hidden-integer", hidden: int(1)},
		{name: "hidden-named-string", hidden: legacyNativeGoGraphW85NamedString("tail")},
		{name: "hidden-nonfinite", hidden: math.NaN()},
		{name: "hidden-nested-invalid", hidden: map[string]any{"bad": []any{int64(1)}}},
		{name: "hidden-cycle", hidden: cyclic},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			backing := []any{fixture.hidden}
			source := backing[:0:1]
			roots := []any{source}
			snapshot := legacyNativeGoGraphW85SourceSnapshot(roots)
			defer legacyNativeGoGraphW85SourceUnchanged(test, snapshot)
			legacyNativeGoGraphW86ImportReject(test, source, legacyNativeGoGraphW85Budget, ErrIntegrity)
			storage := legacyNativeGoGraphSetW85Storage(test)
			work, failure := newLegacyNativeCheckpointGoGraphSet(context.Background(), roots, binding, storage.io, legacyNativeGoGraphSetW85Budget)
			legacyNativeGoGraphSetW85Reject[legacyNativeCheckpointRef](test, work, failure, storage, ErrIntegrity, legacyNativeGoGraphSetW85Budget)
		})
	}
	for _, scenario := range []string{"step-current-context", "take-current-context", "close-partial"} {
		test.Run("importer-"+scenario, func(test *testing.T) {
			backing := make([]any, 8193)
			for position := range backing {
				backing[position] = float64(position)
			}
			source := backing[:0:8193]
			snapshot := legacyNativeGoGraphW85SourceSnapshot(source)
			defer legacyNativeGoGraphW85SourceUnchanged(test, snapshot)
			registry := newLegacyNativeCheckpointGoGraphRegistry()
			defer registry.close()
			constructor, cancel := context.WithCancel(context.Background())
			work, failure := newLegacyNativeCheckpointGoGraph(constructor, source, registry, legacyNativeGoGraphW85Budget)
			cancel()
			if failure != nil || work == nil {
				test.Fatal("tail lifecycle constructor failed", failure)
			}
			defer work.close()
			switch scenario {
			case "step-current-context":
				base, stop := context.WithCancel(context.Background())
				defer stop()
				current := &legacyNativeGoGraphW85CutContext{Context: base, cancel: stop, limit: 8}
				for attempt := 0; attempt < legacyNativeGoGraphW85StepLimit; attempt++ {
					ready, stepFailure := work.step(current)
					if ready {
						test.Fatal("hidden-only traversal ignored current cancellation", stepFailure)
					}
					if stepFailure != nil {
						failure = stepFailure
						break
					}
					legacyNativeGoGraphW85BudgetCheck(test, work, registry, legacyNativeGoGraphW85Budget)
				}
				if !errors.Is(failure, context.Canceled) || current.calls < current.limit {
					test.Fatal("hidden-only importer did not observe its current context", current.calls, failure)
				}
			case "take-current-context":
				legacyNativeGoGraphW85Drain(test, work, registry, legacyNativeGoGraphW85Budget)
				current, stop := context.WithCancel(context.Background())
				stop()
				value, takeFailure := work.take(current)
				if !errors.Is(takeFailure, context.Canceled) || !reflect.DeepEqual(value, legacyNativeAnyValue{}) {
					test.Fatal("full backing handoff ignored current cancellation", takeFailure)
				}
				failure = takeFailure
			case "close-partial":
				ready, stepFailure := work.step(context.Background())
				if ready || stepFailure != nil {
					test.Fatal("large hidden-only importer did not yield before close", ready, stepFailure)
				}
				legacyNativeGoGraphW85WorkReleased(test, work)
				if resumed, stepFailure := work.step(context.Background()); resumed || !errors.Is(stepFailure, ErrIntegrity) {
					test.Fatal("closed hidden-only importer resumed", resumed, stepFailure)
				}
				value, takeFailure := work.take(context.Background())
				if !errors.Is(takeFailure, ErrIntegrity) || !reflect.DeepEqual(value, legacyNativeAnyValue{}) {
					test.Fatal("closed hidden-only importer transferred partial ownership", takeFailure)
				}
				legacyNativeGoGraphW85RegistryReleased(test, registry)
				return
			}
			legacyNativeGoGraphW85Failure(test, work, registry, failure)
		})
	}
	test.Run("tail-access-and-immutable-append", func(test *testing.T) {
		backing := []any{true, "tail", map[string]any{"child": "owned"}}
		source := backing[:1:3]
		snapshot := legacyNativeGoGraphW85SourceSnapshot(source)
		defer legacyNativeGoGraphW85SourceUnchanged(test, snapshot)
		registry := newLegacyNativeCheckpointGoGraphRegistry()
		defer registry.close()
		value, _ := legacyNativeGoGraphW85Import(test, source, registry)
		list := value.owner.array
		if list == nil || list.validateTail(true) != nil {
			test.Fatal("tail access fixture lacks complete native ownership")
		}
		for _, position := range []int{-1, 0, cap(source)} {
			child, failure := list.tailValue(context.Background(), position)
			if !errors.Is(failure, ErrIntegrity) || !reflect.DeepEqual(child, legacyNativeAnyValue{}) {
				test.Fatal("tail access accepted a non-tail absolute physical index", position, failure)
			}
		}
		current, cancel := context.WithCancel(context.Background())
		cancel()
		if child, failure := list.tailValue(current, len(source)); !errors.Is(failure, context.Canceled) || !reflect.DeepEqual(child, legacyNativeAnyValue{}) {
			test.Fatal("tail access ignored the current canceled context", failure)
		}
		if appended, failure := list.appendValueGuarded(context.Background(), &legacyNormalizationInput{}, legacyNativeAnyValue{}, nil); failure == nil || appended {
			test.Fatal("immutable captured tail accepted an unconnected normal append", appended, failure)
		}
		if failure := value.validateOwnership(); failure != nil {
			test.Fatal("denied append or tail access changed existing ownership", failure)
		}
		legacyNativeGoGraphW86Values(test, []any{source}, []legacyNativeAnyValue{value})
	})
	fixtures := legacyNativeGoGraphW86Fixtures()
	roots := fixtures[0].roots
	snapshot := legacyNativeGoGraphW85SourceSnapshot(roots)
	defer legacyNativeGoGraphW85SourceUnchanged(test, snapshot)
	legacyNativeGoGraphSetW85Lifecycle[legacyNativeCheckpointRef](test, "full-backing-capture", func(storage *legacyNativeGoGraphSetW85Store) func(context.Context) (legacyNativeGoGraphSetW85Work[legacyNativeCheckpointRef], error) {
		return func(current context.Context) (legacyNativeGoGraphSetW85Work[legacyNativeCheckpointRef], error) {
			return newLegacyNativeCheckpointGoGraphSet(current, roots, binding, storage.io, legacyNativeGoGraphSetW85Budget)
		}
	})
	legacyNativeGoGraphSetW85Lifecycle[[]legacyNativeAnyValue](test, "full-backing-restore", func(storage *legacyNativeGoGraphSetW85Store) func(context.Context) (legacyNativeGoGraphSetW85Work[[]legacyNativeAnyValue], error) {
		saved := legacyNativeGoGraphSetW85Capture(test, roots, binding, storage)
		return func(current context.Context) (legacyNativeGoGraphSetW85Work[[]legacyNativeAnyValue], error) {
			return newLegacyNativeCheckpointGoGraphSetRestore(current, saved, binding, storage.io, legacyNativeGoGraphSetW85Budget)
		}
	})
	test.Run("actual-hidden-dependent-corruption", func(test *testing.T) {
		storage := legacyNativeGoGraphSetW85Storage(test)
		backing := []any{map[string]any{"hidden": true}, strings.Repeat("한🙂", LegacyCanonicalEncodingBytes/3+7)}
		roots := []any{backing[:0:2]}
		snapshot := legacyNativeGoGraphW85SourceSnapshot(roots)
		defer legacyNativeGoGraphW85SourceUnchanged(test, snapshot)
		saved := legacyNativeGoGraphSetW85Capture(test, roots, binding, storage)
		state := legacyNativeGoGraphW86State(test, storage, saved)
		owner := legacyNativeGoGraphW86Owner(test, storage, state.Roots[0])
		if !owner.FullBacking || owner.Visible != 0 || owner.Members.Count != 2 || owner.Members.Height != 0 {
			test.Fatal("hidden dependency fixture lacks its actual two-member tail page", owner)
		}
		var page legacyNativeCheckpointListPage[legacyNativeCheckpointMember]
		if failure := json.Unmarshal(storage.objects[owner.Members.Ref.Hash], &page); failure != nil || len(page.Values) != 2 || !page.Values[1].Value.Text.Tree.Ref.valid() {
			test.Fatal("hidden scalar did not produce its actual dependent text tree", failure)
		}
		for _, target := range []struct {
			name string
			ref  legacyNativeCheckpointRef
		}{
			{name: "owner", ref: state.Roots[0].Body},
			{name: "tail-member-page", ref: owner.Members.Ref},
			{name: "hidden-scalar-tree", ref: page.Values[1].Value.Text.Tree.Ref},
		} {
			for _, damage := range []string{"missing", "hash", "length"} {
				test.Run(target.name+"/"+damage, func(test *testing.T) {
					original, present := storage.objects[target.ref.Hash]
					if !present || len(original) < 2 {
						test.Fatal("damage did not target an actual hidden dependent CAS object")
					}
					defer func() { storage.objects[target.ref.Hash] = original }()
					switch damage {
					case "missing":
						delete(storage.objects, target.ref.Hash)
					case "hash":
						storage.objects[target.ref.Hash] = bytes.Repeat([]byte{'x'}, len(original))
					case "length":
						storage.objects[target.ref.Hash] = bytes.Clone(original[:len(original)-1])
					}
					work, failure := newLegacyNativeCheckpointGoGraphSetRestore(context.Background(), saved, binding, storage.io, legacyNativeGoGraphSetW85Budget)
					legacyNativeGoGraphSetW85Reject[[]legacyNativeAnyValue](test, work, failure, storage, ErrPreparedDocRangeObject, legacyNativeGoGraphSetW85Budget)
				})
			}
		}
	})
}
