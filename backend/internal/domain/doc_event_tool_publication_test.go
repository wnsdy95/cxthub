package domain

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func completeStructuredToolResult(test *testing.T, value string) (DocEventDisplayPreparation, *docToolValueFixture) {
	test.Helper()
	fixture := newDocToolValueFixture(test, value)
	raw := fixture.order.keys.tree.raw
	state, err := NewDocEventDisplayPreparation(DocRangeEntry{DocEventIndex: DocEventIndex{Hash: HashContent(raw), Length: len(raw)}, Kind: EventToolResult})
	if err != nil {
		test.Fatal(err)
	}
	tools := fixture.order.tools()
	tools.OrderPlan = fixture.loadOrder
	for count := 0; state.Phase != "completed"; count++ {
		if count > len(raw)*4+100 {
			test.Fatal("structured result failed to converge", state.Phase)
		}
		before, _ := state.Encode()
		step, err := StepDocEventDisplay(context.Background(), state, func(_ context.Context, from, length int) ([]byte, error) {
			if state.Phase == "tool_values_done" || state.Phase == "tool_assembly" {
				test.Fatal("assembly fetched original bytes")
			}
			return bytes.Clone(raw[from : from+length]), nil
		}, fixture.order.keys.tree.load, tools)
		if err != nil {
			test.Fatal("structured result failed", count, state.Phase, err)
		}
		after, _ := state.Encode()
		if !bytes.Equal(before, after) || step.Prior() != HashContent(before) {
			test.Fatal("step mutated its predecessor")
		}
		for _, object := range step.Objects() {
			fixture.order.keys.tree.objects[object.Hash] = bytes.Clone(object.Bytes)
		}
		encoded, err := step.Preparation().Encode()
		if err != nil {
			test.Fatal(err)
		}
		state, err = DecodeDocEventDisplayPreparation(HashContent(encoded), encoded)
		if err != nil {
			test.Fatal(err)
		}
	}
	return state, fixture
}

func TestDocEventStructuredToolResultPublicationParity(test *testing.T) {
	for _, value := range []string{`{}`, `[]`, `true`, `false`, `0`, `1e400`, `-0`, `1.2345`, "{\"3\":\"first\",\"0\":\"zero\",\"dup\":1,\"dup\":2,\"nested\":[true,null,\"\ud55c\uae00\\ud800🙂\"]}"} {
		test.Run(value, func(test *testing.T) {
			state, fixture := completeStructuredToolResult(test, value)
			load := fixture.order.keys.tree.load
			generation, err := ValidateDocEventDisplayPublication(context.Background(), state, load)
			if err != nil || generation.Source.Plan != state.Plan || state.Plan != state.ToolPlan {
				test.Fatal("publication changed source identity", err)
			}
			baseline, err := BuildDocToolDisplay(context.Background(), fixture.order.keys.tree.raw)
			if err != nil {
				test.Fatal(err)
			}
			var actual strings.Builder
			for index := 0; index < generation.Pages.Count; index++ {
				page, err := ReadDocDisplayProjectionPage(context.Background(), generation, index, load)
				if err != nil || page.From != actual.Len() {
					test.Fatal("projection pages lost continuity", err)
				}
				for offset, character := range page.Text {
					width := utf8.RuneLen(character)
					got, err := page.SourceRange(offset, width)
					want, baselineError := baseline.SourceRange(page.From+offset, width)
					if err != nil || baselineError != nil || got != want {
						test.Fatal("projection changed original-byte mapping", got, want, err, baselineError)
					}
				}
				actual.WriteString(page.Text)
			}
			if actual.String() != baseline.Text() {
				test.Fatal("projection changed tool result", actual.String(), baseline.Text())
			}
		})
	}
}

func TestDocEventStructuredToolPublicationFences(test *testing.T) {
	state, fixture := completeStructuredToolResult(test, `{"value":"safe"}`)
	for name, change := range map[string]func(*DocEventDisplayPreparation){
		"call":       func(state *DocEventDisplayPreparation) { state.Source.Kind = EventToolCall },
		"source":     func(state *DocEventDisplayPreparation) { state.ToolValue = HashContent([]byte("missing")) },
		"plan":       func(state *DocEventDisplayPreparation) { state.Plan = HashContent([]byte("missing")) },
		"evidence":   func(state *DocEventDisplayPreparation) { state.ToolFields = HashContent([]byte("missing")) },
		"generation": func(state *DocEventDisplayPreparation) { state.Generation = "" },
		"active":     func(state *DocEventDisplayPreparation) { state.Active = HashContent([]byte("resumed")) },
	} {
		test.Run(name, func(test *testing.T) {
			encoded, _ := state.Encode()
			changed, _ := DecodeDocEventDisplayPreparation(HashContent(encoded), encoded)
			change(&changed)
			got, err := ValidateDocEventDisplayPublication(context.Background(), changed, fixture.order.keys.tree.load)
			if err == nil || !reflect.DeepEqual(got, DocDisplayProjectionGeneration{}) {
				test.Fatal("invalid completed tool state was published", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := ValidateDocEventDisplayPublication(ctx, state, fixture.order.keys.tree.load); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, DocDisplayProjectionGeneration{}) {
		test.Fatal("cancelled publication leaked a result", err)
	}
}

func TestDocEventStructuredToolPublicationRejectsCoherentForeignPlans(test *testing.T) {
	state, fixture := completeStructuredToolResult(test, `{"value":"left"}`)
	foreign, other := completeStructuredToolResult(test, `{"value":"else"}`)
	for hash, raw := range other.order.keys.tree.objects {
		fixture.order.keys.tree.objects[hash] = bytes.Clone(raw)
	}
	foreignGeneration, err := ValidateDocEventDisplayPublication(context.Background(), foreign, fixture.order.keys.tree.load)
	if err != nil {
		test.Fatal(err)
	}
	foreignGeneration.Source.EventHash = state.Source.EventHash
	foreignGeneration.Source.EventBytes = state.Source.EventBytes
	raw, err := foreignGeneration.Encode()
	if err != nil {
		test.Fatal(err)
	}
	forgedGeneration := HashContent(raw)
	fixture.order.keys.tree.objects[forgedGeneration] = raw
	for name, change := range map[string]func(*DocEventDisplayPreparation){
		"source": func(state *DocEventDisplayPreparation) { state.ToolValue = foreign.ToolValue },
		"plan-and-generation": func(state *DocEventDisplayPreparation) {
			state.Plan, state.ToolPlan = foreign.ToolPlan, foreign.ToolPlan
			state.Generation = forgedGeneration
		},
		"all-value-objects": func(state *DocEventDisplayPreparation) {
			state.ToolValue, state.Plan, state.ToolPlan = foreign.ToolValue, foreign.ToolPlan, foreign.ToolPlan
			state.Generation = forgedGeneration
		},
	} {
		test.Run(name, func(test *testing.T) {
			encoded, _ := state.Encode()
			changed, _ := DecodeDocEventDisplayPreparation(HashContent(encoded), encoded)
			change(&changed)
			if changed.Validate() != nil {
				test.Fatal("fixture was not a shape-valid transplant")
			}
			if got, err := ValidateDocEventDisplayPublication(context.Background(), changed, fixture.order.keys.tree.load); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(got, DocDisplayProjectionGeneration{}) {
				test.Fatal("coherent foreign value objects were published", err)
			}
		})
	}
}

func TestDocEventToolResultStringNullMissingPreserveLiteralAndOmission(test *testing.T) {
	for _, value := range []string{`"literal"`, `"   \t\n"`, `null`} {
		test.Run(value, func(test *testing.T) {
			fixture := newDocToolValueFixture(test, value)
			raw := fixture.order.keys.tree.raw
			state, err := NewDocEventDisplayPreparation(DocRangeEntry{DocEventIndex: DocEventIndex{Hash: HashContent(raw), Length: len(raw)}, Kind: EventToolResult})
			if err != nil {
				test.Fatal(err)
			}
			tools := fixture.order.tools()
			tools.OrderPlan = fixture.loadOrder
			for count := 0; state.Phase != "completed"; count++ {
				if count > 100 {
					test.Fatal("scalar policy did not finish")
				}
				step, err := StepDocEventDisplay(context.Background(), state, func(_ context.Context, from, length int) ([]byte, error) {
					return bytes.Clone(raw[from : from+length]), nil
				}, fixture.order.keys.tree.load, tools)
				if err != nil {
					test.Fatal(err)
				}
				for _, object := range step.Objects() {
					fixture.order.keys.tree.objects[object.Hash] = bytes.Clone(object.Bytes)
				}
				state = step.Preparation()
			}
			step, err := StepDocEventDisplay(context.Background(), state, nil, fixture.order.keys.tree.load, tools)
			if !errors.Is(err, ErrIntegrity) || len(step.Objects()) != 0 {
				test.Fatal("invalid raw capability was accepted", err)
			}
			step, err = StepDocEventDisplay(context.Background(), state, func(context.Context, int, int) ([]byte, error) {
				test.Fatal("unsupported decision read original bytes")
				return nil, ErrIntegrity
			}, fixture.order.keys.tree.load, tools)
			if !errors.Is(err, ErrIntegrity) || len(step.Objects()) != 0 || step.Preparation().Generation != "" {
				test.Fatal("completed result was advanced again", err)
			}
			generation, err := ValidateDocEventDisplayPublication(context.Background(), state, fixture.order.keys.tree.load)
			wantBytes := 0
			if value == `"literal"` {
				wantBytes = len("literal")
			}
			if err != nil || generation.Source.TextBytes != wantBytes {
				test.Fatal("literal or omission acquired JSON quotes", generation.Source.TextBytes, err)
			}
		})
	}
	missing := newToolEventDisplayFixture(test, EventToolResult, []byte(`{"kind":"tool_result"}`))
	advanceToolEventDisplayFixture(test, missing)
	if missing.state.Phase != "tool_empty" {
		test.Fatal("missing output acquired a value plan")
	}
	if step, err := StepDocEventDisplay(context.Background(), missing.state, missing.loadRaw, missing.loadObject); err != nil || step.Preparation().Phase != "tool_policy" || step.Preparation().Generation != "" {
		test.Fatal("missing output bypassed policy preparation", err)
	}
	if generation := newDocToolPolicyFixture(test, `{"kind":"tool_result"}`).finish(test); generation.Source.TextBytes != 0 || generation.Pages.Count != 0 {
		test.Fatal("missing output exposed content")
	}
}
