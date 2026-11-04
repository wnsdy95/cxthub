package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

type docToolOrdersFixture struct {
	keys    *docToolKeyMapFixture
	entries map[int][]DocToolOrderEntry
}

func newDocToolOrdersFixture(test testing.TB, value string) *docToolOrdersFixture {
	return newDocToolOrdersFixtureFromKeys(test, newDocToolKeyMapFixture(test, value))
}

func newDocToolOrdersFixtureFromKeys(test testing.TB, keys *docToolKeyMapFixture) *docToolOrdersFixture {
	test.Helper()
	keys.run(test)
	fixture := &docToolOrdersFixture{keys: keys, entries: make(map[int][]DocToolOrderEntry)}
	groups := make(map[string]DocToolKeyMember)
	for _, item := range keys.items {
		group := fmt.Sprintf("%d:%s", item.Node.Parent, item.Digest)
		var prior *DocToolKeyMember
		if member, exists := groups[group]; exists {
			prior = &member
		}
		member, err := MergeDocToolKeyMember(keys.tree.source, prior, item)
		if err != nil {
			test.Fatal(err)
		}
		groups[group] = member
	}
	for _, member := range groups {
		fixture.entries[member.Last.Node.Parent] = append(fixture.entries[member.Last.Node.Parent], NewDocToolOrderMember(member))
	}
	for _, node := range keys.tree.nodes {
		if node.Depth > 0 && node.Key == nil {
			fixture.entries[node.Parent] = append(fixture.entries[node.Parent], DocToolOrderEntry{Node: cloneDocToolTreeNode(node), First: node.Value.From})
		}
	}
	for _, node := range keys.tree.nodes {
		if node.Type != DocToolValueArray && node.Type != DocToolValueObject {
			continue
		}
		source, err := NewDocToolOrderSource(keys.tree.source, keys.source, node, len(fixture.entries[node.Value.From]))
		if err != nil {
			test.Fatal(err)
		}
		slices.SortFunc(fixture.entries[node.Value.From], func(left, right DocToolOrderEntry) int {
			return CompareDocToolOrderEntries(left, right, source)
		})
	}
	return fixture
}

func (fixture *docToolOrdersFixture) source(ctx context.Context, parent int) (DocToolOrderSource, error) {
	if err := ctx.Err(); err != nil {
		return DocToolOrderSource{}, err
	}
	for _, node := range fixture.keys.tree.nodes {
		if node.Value.From == parent {
			return NewDocToolOrderSource(fixture.keys.tree.source, fixture.keys.source, node, len(fixture.entries[parent]))
		}
	}
	return DocToolOrderSource{}, ErrIntegrity
}

func (fixture *docToolOrdersFixture) read(ctx context.Context, source DocToolOrderSource, after *DocToolOrderEntry, limit int) ([]DocToolOrderEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > DocToolOrderPageEntries {
		return nil, ErrIntegrity
	}
	result := make([]DocToolOrderEntry, 0, limit)
	for _, entry := range fixture.entries[source.Parent.Value.From] {
		if after == nil || CompareDocToolOrderEntries(*after, entry, source) < 0 {
			result = append(result, cloneDocToolOrderEntry(entry))
			if len(result) == limit {
				break
			}
		}
	}
	return result, nil
}

func (fixture *docToolOrdersFixture) tools() DocToolPreparationReader {
	return DocToolPreparationReader{Nodes: fixture.keys.loadNodes, OrderSource: fixture.source, OrderEntries: fixture.read}
}

func TestDocToolOrderPagesRestartAndSelectedReads(test *testing.T) {
	for _, count := range []int{0, 1, 63, 64, 65, 127, 128, 129, 8193} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			fixture := newDocToolOrdersFixture(test, `[`+strings.TrimSuffix(strings.Repeat(`0,`, count), ",")+`]`)
			tree, keys := fixture.keys.tree.source, fixture.keys.source
			source, err := fixture.source(context.Background(), tree.Root.From)
			if err != nil {
				test.Fatal(err)
			}
			state, err := NewDocToolOrderState(tree, keys, source)
			if err != nil {
				test.Fatal(err)
			}
			for !state.Done(source) {
				before, _ := state.Encode(tree, keys, source)
				step, err := StepDocToolOrder(context.Background(), tree, keys, source, state,
					func(ctx context.Context, after *DocToolOrderEntry, limit int) ([]DocToolOrderEntry, error) {
						return fixture.read(ctx, source, after, limit)
					}, fixture.keys.tree.load)
				if err != nil || len(step.Entries()) > 64 || len(step.Objects()) > 4 {
					test.Fatal("bounded order step failed", err)
				}
				after, _ := state.Encode(tree, keys, source)
				if !bytes.Equal(before, after) || step.Prior() != HashContent(before) {
					test.Fatal("order step changed its predecessor")
				}
				for _, object := range step.Objects() {
					fixture.keys.tree.objects[object.Hash] = bytes.Clone(object.Bytes)
				}
				state = step.State()
				encoded, err := state.Encode(tree, keys, source)
				if err != nil || len(encoded) > 64<<10 {
					test.Fatal("unbounded order state", err)
				}
				state, err = DecodeDocToolOrderState(tree, keys, source, HashContent(encoded), encoded)
				if err != nil {
					test.Fatal(err)
				}
			}
			plan, objects, err := FinishDocToolOrder(context.Background(), tree, keys, source, state, fixture.keys.tree.load)
			if err != nil || plan.Pages.Count != (count+63)/64 {
				test.Fatal("order plan did not seal", err)
			}
			for _, object := range objects {
				fixture.keys.tree.objects[object.Hash] = bytes.Clone(object.Bytes)
			}
			encoded, _ := plan.Encode(tree, keys)
			plan, err = DecodeDocToolOrderPlan(tree, keys, HashContent(encoded), encoded)
			if err != nil {
				test.Fatal(err)
			}
			if count > 0 {
				reads := 0
				page, err := ReadDocToolOrderPage(context.Background(), tree, keys, plan, plan.Pages.Count-1, func(ctx context.Context, hash ContentHash) ([]byte, error) {
					reads++
					return fixture.keys.tree.load(ctx, hash)
				})
				if err != nil || page.Entries[len(page.Entries)-1].Node.Ordinal != count-1 || reads > 4 {
					test.Fatal("late page read replayed earlier child lists", count, reads, err)
				}
			}
		})
	}
}

func TestDocToolOrdersAllContainersAndSealedGetters(test *testing.T) {
	fixture := newDocToolOrdersFixture(test, `{"b":{"old":1},"2":false,"\u0032":[true,{},[]],"b":{"new":2},"empty":{}}`)
	tree, keys := fixture.keys.tree.source, fixture.keys.source
	source, _ := NewDocToolOrdersSource(tree, keys)
	state, _ := NewDocToolOrdersState(tree, keys, source)
	plans := make(map[int]DocToolOrderPlan)
	for !state.Done(source) {
		before, _ := state.Encode(tree, keys, source)
		step, err := StepDocToolOrders(context.Background(), tree, keys, source, state, fixture.tools(), fixture.keys.tree.load)
		if err != nil || len(step.Inputs()) > 16 || len(step.Objects()) > 16 {
			test.Fatal("whole-source order preparation failed", err)
		}
		after, _ := state.Encode(tree, keys, source)
		if !bytes.Equal(before, after) || step.Prior() != HashContent(before) {
			test.Fatal("whole-source prior changed")
		}
		for _, object := range step.Objects() {
			fixture.keys.tree.objects[object.Hash] = bytes.Clone(object.Bytes)
		}
		if plan, found := step.Plan(); found {
			if _, duplicate := plans[plan.Source.Parent.Value.From]; duplicate {
				test.Fatal("container plan emitted twice")
			}
			plans[plan.Source.Parent.Value.From] = plan
			plan.Source.Parent.Value.From++
			original, _ := step.Plan()
			if original.Source.Parent.Value.From == plan.Source.Parent.Value.From {
				test.Fatal("plan accessor aliases sealed source")
			}
		}
		state = step.State()
		encoded, err := state.Encode(tree, keys, source)
		if err != nil {
			test.Fatal(err)
		}
		state, err = DecodeDocToolOrdersState(tree, keys, source, HashContent(encoded), encoded)
		if err != nil {
			test.Fatal(err)
		}
	}
	containers := 0
	for _, node := range fixture.keys.tree.nodes {
		if node.Type == DocToolValueArray || node.Type == DocToolValueObject {
			containers++
			if _, found := plans[node.Value.From]; !found {
				test.Fatal("source container has no final order plan")
			}
		}
	}
	if state.Seen != keys.Nodes || state.Containers != containers || len(plans) != containers {
		test.Fatal("source order completion lost containers", state, containers)
	}
	root := plans[tree.Root.From]
	page, err := ReadDocToolOrderPage(context.Background(), tree, keys, root, 0, fixture.keys.tree.load)
	if err != nil || len(page.Entries) != 3 || page.Entries[0].ArrayIndex != 2 {
		test.Fatal("root key order/dedup did not survive final pages", err)
	}
	lastValue := page.Entries[1].Node.Value
	if string(fixture.keys.tree.raw[lastValue.From:lastValue.To]) != `{"new":2}` {
		test.Fatal("final ordered page merged duplicate values instead of selecting the whole last member")
	}
}

func TestDocToolOrderFailureHasNoPartialEffects(test *testing.T) {
	fixture := newDocToolOrdersFixture(test, `[0,1,2]`)
	tree, keys := fixture.keys.tree.source, fixture.keys.source
	source, _ := fixture.source(context.Background(), tree.Root.From)
	state, _ := NewDocToolOrderState(tree, keys, source)
	injected := errors.New("injected order input failure")
	for _, expected := range []error{injected, context.Canceled, ErrIntegrity} {
		ctx := context.Background()
		loader := func(context.Context, *DocToolOrderEntry, int) ([]DocToolOrderEntry, error) { return nil, injected }
		if expected == context.Canceled {
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			ctx = cancelled
		} else if expected == ErrIntegrity {
			loader = func(context.Context, *DocToolOrderEntry, int) ([]DocToolOrderEntry, error) {
				return fixture.entries[tree.Root.From][:2], nil
			}
		}
		step, err := StepDocToolOrder(ctx, tree, keys, source, state, loader, fixture.keys.tree.load)
		if !errors.Is(err, expected) || !reflect.DeepEqual(step, DocToolOrderStep{}) {
			test.Fatal("failed order step leaked partial state", err, expected)
		}
	}
}

func TestDocToolOrderSealedInputAndGetterAliases(test *testing.T) {
	fixture := newDocToolOrdersFixture(test, `{"nested":{"a":1,"b":2}}`)
	tree, keys := fixture.keys.tree.source, fixture.keys.source
	var parent int
	for _, node := range fixture.keys.tree.nodes {
		if node.Depth == 1 && node.Type == DocToolValueObject {
			parent = node.Value.From
		}
	}
	source, _ := fixture.source(context.Background(), parent)
	state, _ := NewDocToolOrderState(tree, keys, source)
	step, err := StepDocToolOrder(context.Background(), tree, keys, source, state,
		func(ctx context.Context, after *DocToolOrderEntry, limit int) ([]DocToolOrderEntry, error) {
			return fixture.read(ctx, source, after, limit)
		}, fixture.keys.tree.load)
	if err != nil {
		test.Fatal(err)
	}
	before, _ := step.Source().Encode(tree, keys)
	source.Parent.Key.From++
	current := step.Source()
	current.Parent.Key.To--
	entries := step.Entries()
	entries[0].Node.Key.From++
	objects := step.Objects()
	objects[0].Bytes[0] ^= 1
	if after, err := step.Source().Encode(tree, keys); err != nil || !bytes.Equal(before, after) || reflect.DeepEqual(entries, step.Entries()) || bytes.Equal(objects[0].Bytes, step.Objects()[0].Bytes) {
		test.Fatal("sealed order retained an input or getter alias", err)
	}
}

func TestDocToolOrderRejectsImpossibleCursorAndChildOrdinal(test *testing.T) {
	fixture := newDocToolOrdersFixture(test, `[0]`)
	tree, keys := fixture.keys.tree.source, fixture.keys.source
	source, _ := NewDocToolOrdersSource(tree, keys)
	state, _ := NewDocToolOrdersState(tree, keys, source)
	state.Seen, state.After, state.Containers = 2, tree.Root.From, 1
	if state.Validate(tree, keys, source) == nil {
		test.Fatal("root-only cursor claimed multiple visited nodes")
	}
	state.Seen, state.Containers = 1, 0
	if state.Validate(tree, keys, source) == nil {
		test.Fatal("visited container root had no completed container")
	}
	objects := newDocToolOrdersFixture(test, `{"a":0,"b":1}`)
	tree, keys = objects.keys.tree.source, objects.keys.source
	container, _ := objects.source(context.Background(), tree.Root.From)
	entry := cloneDocToolOrderEntry(objects.entries[tree.Root.From][1])
	entry.Node.Ordinal = container.Parent.Children
	if entry.Validate(tree, keys, container) == nil {
		test.Fatal("object child ordinal exceeded the original parent cardinality")
	}
}
