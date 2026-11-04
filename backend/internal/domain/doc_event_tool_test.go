package domain

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func newToolEventDisplayFixture(test *testing.T, kind EventKind, raw []byte) *eventDisplayTestFixture {
	test.Helper()
	fixture := newEventDisplayTestFixture(test, raw, false)
	state, err := NewDocEventDisplayPreparation(DocRangeEntry{
		DocEventIndex: DocEventIndex{Hash: HashContent(raw), Length: len(raw)},
		Kind:          kind, EmptyReasoning: true, CompactSummary: true, SeedSummary: true,
	})
	if err != nil || state.Phase != "tool_discovery" || state.Source.EmptyReasoning || state.Source.Summary {
		test.Fatal("tool source inherited irrelevant message flags", state, err)
	}
	fixture.state = state
	return fixture
}

func advanceToolEventDisplayFixture(test *testing.T, fixture *eventDisplayTestFixture) {
	test.Helper()
	for index := 0; fixture.state.Phase == "tool_discovery" || fixture.state.Phase == "tool_tree"; index++ {
		if index > len(fixture.raw)+1 {
			test.Fatal("tool preparation did not make progress")
		}
		prior, err := fixture.state.Encode()
		if err != nil {
			test.Fatal(err)
		}
		beforeReads := fixture.rawBytes
		step, err := StepDocEventDisplay(context.Background(), fixture.state, fixture.loadRaw, fixture.loadObject)
		if err != nil {
			test.Fatal("outer tool step failed", fixture.state.Phase, err)
		}
		after, err := fixture.state.Encode()
		if err != nil || !bytes.Equal(prior, after) || step.Prior() != HashContent(prior) || fixture.rawBytes-beforeReads > DocFieldDiscoveryStepBytes {
			test.Fatal("outer tool step mutated its predecessor or exceeded its read budget", err)
		}
		for _, object := range step.Objects() {
			if HashContent(object.Bytes) != object.Hash {
				test.Fatal("outer step emitted an invalid object hash")
			}
			eventDisplayTestNoBodies(test, object.Bytes)
			fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
		}
		next := step.Preparation()
		encoded, err := next.Encode()
		if err != nil || len(encoded) > MaxDocEventDisplayPreparationBytes || bytes.Equal(encoded, prior) {
			test.Fatal("outer tool preparation has invalid progress", err)
		}
		eventDisplayTestNoBodies(test, encoded)
		fixture.state, err = DecodeDocEventDisplayPreparation(HashContent(encoded), bytes.Clone(encoded))
		if err != nil || !reflect.DeepEqual(fixture.state, next) {
			test.Fatal("outer tool preparation did not resume", err)
		}
		if next.ToolTree != nil {
			next.ToolTree.Root.From++
			if reflect.DeepEqual(next, step.Preparation()) {
				test.Fatal("preparation accessor retained a mutable tree pointer")
			}
		}
		objects := step.Objects()
		objects[0].Bytes[0] ^= 1
		if bytes.Equal(objects[0].Bytes, step.Objects()[0].Bytes) {
			test.Fatal("sealed outer objects could be mutated through an accessor")
		}
	}
}

func TestDocEventToolPreparationUsesActualSourceAndNeverPublishesTree(test *testing.T) {
	for _, fixtureCase := range []struct {
		name string
		kind EventKind
		raw  string
	}{
		{"call", EventToolCall, `{"kind":"tool_call","tool_name":"run","input":{"items":[true,null,{"text":"PUBLIC_BODY_MARKER"}],"same":1,"same":2}}`},
		{"result_object", EventToolResult, `{"kind":"tool_result","output":{"items":[true,null,{"text":"PUBLIC_BODY_MARKER"}]}}`},
		{"result_string", EventToolResult, `{"kind":"tool_result","output":"PUBLIC_BODY_MARKER"}`},
		{"result_number", EventToolResult, `{"kind":"tool_result","output":0}`},
		{"result_null", EventToolResult, `{"kind":"tool_result","output":null}`},
		{"missing", EventToolResult, `{"kind":"tool_result","opaque":{"output":{"not":"public"}}}`},
	} {
		test.Run(fixtureCase.name, func(test *testing.T) {
			fixture := newToolEventDisplayFixture(test, fixtureCase.kind, []byte(fixtureCase.raw))
			advanceToolEventDisplayFixture(test, fixture)
			if fixture.state.Phase != "tool_tree_done" && fixture.state.Phase != "tool_empty" {
				test.Fatal("tool did not reach its private terminal stage", fixture.state.Phase)
			}
			if fixture.state.Generation != "" || fixture.state.Plan != "" {
				test.Fatal("unformatted tool acquired a public generation")
			}
			step, err := StepDocEventDisplay(context.Background(), fixture.state, fixture.loadRaw, fixture.loadObject)
			if fixture.state.Phase == "tool_empty" {
				if err != nil || step.Preparation().Phase != "tool_policy" || step.Preparation().Generation != "" {
					test.Fatal("empty source bypassed policy preparation", err)
				}
			} else if !errors.Is(err, ErrDocFindDisplayUnavailable) || !reflect.DeepEqual(step, DocEventDisplayStep{}) {
				test.Fatal("unfinished formatter returned success or partial progress", err)
			}
			if generation, err := ValidateDocEventDisplayPublication(context.Background(), fixture.state, fixture.loadObject); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(generation, DocDisplayProjectionGeneration{}) {
				test.Fatal("private tree was accepted as a display generation", generation, err)
			}
		})
	}
}

func TestDocEventToolDiscoveryRejectsLateHashFailureWithoutSelectedState(test *testing.T) {
	raw := []byte(`{"kind":"tool_result","output":{"text":"PUBLIC_BODY_MARKER"},"opaque":"` + strings.Repeat("x", 2*DocFieldDiscoveryStepBytes) + `"}`)
	fixture := newToolEventDisplayFixture(test, EventToolResult, raw)
	fixture.raw[len(raw)-3] = 'y'
	steps := 0
	for {
		prior, _ := fixture.state.Encode()
		step, err := StepDocEventDisplay(context.Background(), fixture.state, fixture.loadRaw, fixture.loadObject)
		if err != nil {
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(step, DocEventDisplayStep{}) || steps < 2 {
				test.Fatal("late source mismatch was not rejected without partial output", steps, err)
			}
			after, _ := fixture.state.Encode()
			if !bytes.Equal(prior, after) {
				test.Fatal("failed discovery changed the saved predecessor")
			}
			return
		}
		for _, object := range step.Objects() {
			fixture.objects[object.Hash] = object.Bytes
		}
		fixture.state = step.Preparation()
		if fixture.state.Phase != "tool_discovery" || fixture.state.ToolFields != "" || fixture.state.ToolTree != nil {
			test.Fatal("selected tool fields escaped before complete source verification")
		}
		steps++
	}
}

func TestDocEventToolPreparationRejectsCrossKindAndEvidence(test *testing.T) {
	fixture := newToolEventDisplayFixture(test, EventToolResult, []byte(`{"kind":"tool_result","output":{"items":[true,null]}}`))
	advanceToolEventDisplayFixture(test, fixture)
	for _, mutate := range []func(*DocEventDisplayPreparation){
		func(state *DocEventDisplayPreparation) { state.Source.Kind = EventMessage },
		func(state *DocEventDisplayPreparation) { state.Source.Summary = true },
		func(state *DocEventDisplayPreparation) { state.Source.EmptyReasoning = true },
		func(state *DocEventDisplayPreparation) { state.ToolFields = HashContent([]byte("foreign")) },
		func(state *DocEventDisplayPreparation) { state.ToolTree.EventHash = HashContent([]byte("foreign")) },
		func(state *DocEventDisplayPreparation) { state.Phase = "completed" },
		func(state *DocEventDisplayPreparation) { state.Generation = HashContent([]byte("pretend generation")) },
	} {
		raw, _ := fixture.state.Encode()
		state, err := DecodeDocEventDisplayPreparation(HashContent(raw), raw)
		if err != nil {
			test.Fatal(err)
		}
		mutate(&state)
		if state.Validate() == nil {
			test.Fatal("outer tool preparation accepted incompatible phase/evidence", state)
		}
		step, err := StepDocEventDisplay(context.Background(), state, fixture.loadRaw, fixture.loadObject)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(step, DocEventDisplayStep{}) {
			test.Fatal("invalid tool preparation returned partial output", err)
		}
	}
}
