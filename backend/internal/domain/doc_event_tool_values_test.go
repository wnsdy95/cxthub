package domain

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestDocEventToolValuesActualOrchestration(test *testing.T) {
	fixture := newDocToolValueFixture(test, `{"nested":[1,{"huge":"`+strings.Repeat(`\nabc`, 20000)+`"}],"number":1.`+strings.Repeat("0", 90000)+`1}`)
	raw := fixture.order.keys.tree.raw
	state, err := NewDocEventDisplayPreparation(DocRangeEntry{DocEventIndex: DocEventIndex{Hash: HashContent(raw), Length: len(raw)}, Kind: EventToolResult})
	if err != nil {
		test.Fatal(err)
	}
	tools := fixture.order.tools()
	tools.OrderPlan = fixture.loadOrder
	loadRaw := func(_ context.Context, from, length int) ([]byte, error) {
		if from < 0 || length <= 0 || from > len(raw)-length {
			return nil, ErrIntegrity
		}
		return bytes.Clone(raw[from : from+length]), nil
	}
	for count := 0; state.Phase != "tool_values_done"; count++ {
		if count > 3000 {
			test.Fatal("outer worker did not reach value completion")
		}
		before, _ := state.Encode()
		step, err := StepDocEventDisplay(context.Background(), state, loadRaw, fixture.order.keys.tree.load, tools)
		if err != nil {
			test.Fatal("actual orchestration", count, state.Phase, err)
		}
		if step.Prior() != HashContent(before) || len(step.Objects()) > MaxDocEventDisplayStepObjects {
			test.Fatal("outer step lost its fence or staging budget")
		}
		for _, object := range step.Objects() {
			fixture.order.keys.tree.objects[object.Hash] = bytes.Clone(object.Bytes)
		}
		encoded, err := step.Preparation().Encode()
		if err != nil || len(encoded) > MaxDocEventDisplayPreparationBytes {
			test.Fatal("outer preparation exceeded its unchanged limit", len(encoded), err)
		}
		state, err = DecodeDocEventDisplayPreparation(HashContent(encoded), encoded)
		if err != nil {
			test.Fatal(err)
		}
	}
	planRaw, err := fixture.order.keys.tree.load(context.Background(), state.ToolPlan)
	if err != nil {
		test.Fatal(err)
	}
	plan, err := DecodeDocToolValuePlan(state.ToolPlan, planRaw)
	if err != nil || plan.Source.Tree != *state.ToolTree || plan.Source.Orders != *state.ToolOrders || plan.TextBytes <= 80000 || plan.Pages.Count < 2 || state.Generation != "" {
		test.Fatal("private value plan was missing or prematurely published", err)
	}
	for count := 0; state.Phase != "completed"; count++ {
		if count > plan.Pages.Count*4+20 {
			test.Fatal("structured result assembly did not converge")
		}
		step, err := StepDocEventDisplay(context.Background(), state, func(context.Context, int, int) ([]byte, error) {
			test.Fatal("assembly reread original event bytes")
			return nil, ErrIntegrity
		}, fixture.order.keys.tree.load, tools)
		if err != nil {
			test.Fatal("structured result assembly", state.Phase, err)
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
	generation, err := ValidateDocEventDisplayPublication(context.Background(), state, fixture.order.keys.tree.load)
	if err != nil || generation.Source.TextBytes != plan.TextBytes || generation.Source.Plan != state.ToolPlan {
		test.Fatal("structured result publication lost its value plan", err)
	}
}
