package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

type docToolValueFixture struct {
	order  *docToolOrdersFixture
	source DocToolValueSource
	plans  map[int]DocToolOrderPlan
	states []DocToolValueState
	steps  []DocToolValueStep
}

func newDocToolValueFixture(test testing.TB, value string) *docToolValueFixture {
	return newDocToolValueFixtureFromOrder(test, newDocToolOrdersFixture(test, value))
}

func newDocToolValueFixtureFromOrder(test testing.TB, order *docToolOrdersFixture) *docToolValueFixture {
	test.Helper()
	tree, keys := order.keys.tree.source, order.keys.source
	source, err := NewDocToolOrdersSource(tree, keys)
	if err != nil {
		test.Fatal(err)
	}
	state, _ := NewDocToolOrdersState(tree, keys, source)
	fixture := &docToolValueFixture{order: order, plans: make(map[int]DocToolOrderPlan)}
	for !state.Done(source) {
		step, err := StepDocToolOrders(context.Background(), tree, keys, source, state, order.tools(), order.keys.tree.load)
		if err != nil {
			test.Fatal(err)
		}
		for _, object := range step.Objects() {
			order.keys.tree.objects[object.Hash] = bytes.Clone(object.Bytes)
		}
		if plan, found := step.Plan(); found {
			fixture.plans[plan.Source.Parent.Value.From] = plan
		}
		state = step.State()
	}
	fixture.source, err = NewDocToolValueSource(tree, keys, source, order.keys.tree.nodes[0])
	if err != nil {
		test.Fatal(err)
	}
	return fixture
}

func (fixture *docToolValueFixture) loadOrder(ctx context.Context, parent int) (DocToolOrderPlan, error) {
	if err := ctx.Err(); err != nil {
		return DocToolOrderPlan{}, err
	}
	plan, found := fixture.plans[parent]
	if !found {
		return DocToolOrderPlan{}, ErrNotFound
	}
	return plan, nil
}

func (fixture *docToolValueFixture) compile(test testing.TB) DocToolValuePlan {
	test.Helper()
	state, err := NewDocToolValueState(fixture.source)
	if err != nil {
		test.Fatal(err)
	}
	for attempts := 0; !state.Done(); attempts++ {
		if attempts > len(fixture.order.keys.tree.raw)+100 {
			test.Fatal("value compilation did not progress")
		}
		before, err := state.Encode(fixture.source)
		if err != nil {
			test.Fatal(err)
		}
		fixture.states = append(fixture.states, cloneDocToolValueState(state))
		read := 0
		step, err := StepDocToolValue(context.Background(), fixture.source, state, func(ctx context.Context, from, length int) ([]byte, error) {
			read += length
			return fixture.order.keys.tree.read(ctx, from, length)
		}, fixture.order.keys.tree.load, fixture.loadOrder)
		if err != nil {
			test.Fatalf("value step %d phase=%s depth=%v: %v", attempts, state.Phase, state.Frame, err)
		}
		after, _ := state.Encode(fixture.source)
		if !bytes.Equal(before, after) || step.Prior() != HashContent(before) || step.ReadBytes() != read || read > DocToolValueStepSourceBytes || step.Actions() > DocToolValueStepActions || step.FrameOperations() > DocToolTreeStepFrames || len(step.Objects()) > MaxDocEventDisplayStepObjects || step.State().TextBytes-state.TextBytes > DocToolValueStepOutputBytes {
			test.Fatal("step exceeded a budget or mutated its predecessor", read, step.Actions(), len(step.Objects()))
		}
		for _, object := range step.Objects() {
			if len(object.Bytes) > MaxDocDisplayPageBytes || HashContent(object.Bytes) != object.Hash {
				test.Fatal("invalid staged object")
			}
			fixture.order.keys.tree.objects[object.Hash] = bytes.Clone(object.Bytes)
		}
		state = step.State()
		encoded, err := state.Encode(fixture.source)
		if err != nil || len(encoded) > MaxDocDisplayStateBytes {
			test.Fatal("invalid restart checkpoint", err)
		}
		state, err = DecodeDocToolValueState(fixture.source, HashContent(encoded), encoded)
		if err != nil {
			test.Fatal(err)
		}
		fixture.steps = append(fixture.steps, step)
	}
	fixture.states = append(fixture.states, cloneDocToolValueState(state))
	plan, objects, err := FinishDocToolValue(context.Background(), fixture.source, state, fixture.order.keys.tree.load)
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range objects {
		fixture.order.keys.tree.objects[object.Hash] = bytes.Clone(object.Bytes)
	}
	return plan
}

func readDocToolValueTestPages(test testing.TB, fixture *docToolValueFixture, plan DocToolValuePlan) (string, []DocToolValuePage) {
	test.Helper()
	var text strings.Builder
	var pages []DocToolValuePage
	for index := 0; index < plan.Pages.Count; index++ {
		page, err := ReadDocToolValuePage(context.Background(), plan, index, fixture.order.keys.tree.load)
		if err != nil || page.Display.From != text.Len() {
			test.Fatal("mapped value pages are not contiguous", err)
		}
		text.WriteString(page.Display.Text)
		pages = append(pages, page)
	}
	return text.String(), pages
}

func TestDocToolValueNestedParityAndRestart(test *testing.T) {
	for _, value := range []string{`{}`, `[]`, `null`, `true`, `false`, `9007199254740993`, `"quoted\n\ud800🙂"`, `{"2":"first","b":{"old":1},"\u0032":[true,{},[]],"b":{"new":2},"1":3}`, "{\"z\" \n : [1,2] , \"a\": {\"x\":-0.001e3}}"} {
		test.Run(fmt.Sprint(len(value), ":", value[:min(12, len(value))]), func(test *testing.T) {
			fixture := newDocToolValueFixture(test, value)
			plan := fixture.compile(test)
			got, pages := readDocToolValueTestPages(test, fixture, plan)
			parser, err := newDocFindSourceSyntaxParser(fixture.order.keys.tree.raw, fixture.source.Tree.Root, false)
			if err != nil {
				test.Fatal(err)
			}
			legacy := docToolDisplayBuilder{ctx: context.Background(), parser: parser, display: docToolDisplay{sourceBytes: len(fixture.order.keys.tree.raw)}, metadata: docToolDisplayBaseBytes, budget: MaxDocToolDisplayBytes}
			if err := legacy.pretty(fixture.source.Tree.Root, 0); err != nil {
				test.Fatal(err)
			}
			legacy.display.text = legacy.text.String()
			if got != legacy.display.Text() {
				test.Fatalf("pretty value changed\ngot %q\nwant %q", got, legacy.display.Text())
			}
			for from := range got {
				if !utf8.RuneStart(got[from]) {
					continue
				}
				_, width := utf8.DecodeRuneInString(got[from:])
				want, err := legacy.display.SourceRange(from, width)
				if err != nil {
					test.Fatal(err)
				}
				for _, page := range pages {
					if from >= page.Display.From && from+width <= page.Display.From+len(page.Display.Text) {
						span, err := page.SourceRange(from-page.Display.From, width)
						if err != nil || span != want {
							test.Fatal("original-byte source range changed", from, span, want, err)
						}
					}
				}
			}
		})
	}
}

func TestDocToolValueLongScalarsDeepFramesAndWhitespace(test *testing.T) {
	for name, value := range map[string]string{
		"string": `{"long":"` + strings.Repeat(`\n\ud800abc`, 20000) + `"}`,
		"number": `{"number":1.` + strings.Repeat("0", 150000) + `1e0}`,
		"colon":  `{"key"` + strings.Repeat(" ", 150000) + `:true}`,
		"deep":   strings.Repeat("[", 480) + `0` + strings.Repeat("]", 480),
	} {
		test.Run(name, func(test *testing.T) {
			fixture := newDocToolValueFixture(test, value)
			plan := fixture.compile(test)
			got, _ := readDocToolValueTestPages(test, fixture, plan)
			if len(fixture.steps) < 2 || len(got) != plan.TextBytes {
				test.Fatal("long work was not resumable")
			}
			for _, state := range fixture.states {
				encoded, _ := state.Encode(fixture.source)
				if len(encoded) > 6000 || bytes.Contains(encoded, []byte("abcabc")) {
					test.Fatal("checkpoint retained a body or full frame stack", len(encoded))
				}
			}
		})
	}
}

func TestDocToolValueFailureAndAccessorIsolation(test *testing.T) {
	fixture := newDocToolValueFixture(test, `{"nested":[1,2,{"a":"b"}]}`)
	fixture.compile(test)
	state := fixture.states[0]
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if step, err := StepDocToolValue(ctx, fixture.source, state, fixture.order.keys.tree.read, fixture.order.keys.tree.load, fixture.loadOrder); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(step, DocToolValueStep{}) {
		test.Fatal("cancellation returned partial state", step, err)
	}
	for _, step := range fixture.steps {
		before := step.State()
		changed := step.State()
		if changed.Frame != nil {
			changed.Frame.Node.Value.From++
		}
		if changed.Value != nil {
			changed.Value.Value.From++
		}
		if !reflect.DeepEqual(before, step.State()) {
			test.Fatal("state accessor aliases the sealed step")
		}
	}
	for _, prior := range fixture.states[:len(fixture.states)-1] {
		step, err := StepDocToolValue(context.Background(), fixture.source, prior, fixture.order.keys.tree.read, func(context.Context, ContentHash) ([]byte, error) { return nil, ErrNotFound }, fixture.loadOrder)
		if err == nil || !reflect.DeepEqual(step, DocToolValueStep{}) {
			test.Fatal("missing dependency returned partial step", prior.Phase, err)
		}
	}
}

func TestDocToolValueRejectsParentAndPhaseTransplantsBeforeRawRead(test *testing.T) {
	for _, scenario := range []struct {
		name, value string
	}{
		{"parent", `{"outer":{"long":"` + strings.Repeat(`\nabc`, 40000) + `"}}`},
		{"phase", `{"` + strings.Repeat("longkey", 30000) + `":0}`},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newDocToolValueFixture(test, scenario.value)
			fixture.compile(test)
			found := false
			for _, saved := range fixture.states {
				state := cloneDocToolValueState(saved)
				if scenario.name == "parent" {
					if state.Frame == nil || state.Frame.Parent == "" || state.Phase != "string" {
						continue
					}
					state.Frame.Parent = HashContent([]byte("missing parent"))
				} else {
					if state.Phase != "key" || state.Active == "" {
						continue
					}
					state.Phase, state.Active, state.Start, state.Offset = "colon", "", 0, state.Frame.Child.Key.To
				}
				read := 0
				step, err := StepDocToolValue(context.Background(), fixture.source, state, func(ctx context.Context, from, length int) ([]byte, error) {
					read += length
					return fixture.order.keys.tree.read(ctx, from, length)
				}, fixture.order.keys.tree.load, fixture.loadOrder)
				if err == nil || read != 0 || !reflect.DeepEqual(step, DocToolValueStep{}) {
					test.Fatal("invalid parent/phase read raw data or returned partial output", err, read)
				}
				found = true
				break
			}
			if !found {
				test.Fatal("fixture never reached the intended suspended boundary")
			}
		})
	}
}
