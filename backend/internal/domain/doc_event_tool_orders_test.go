package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestDocEventToolOrdersActualOrchestration(test *testing.T) {
	for _, value := range []string{
		`0`, `{}`, `[]`, `{"a":[{},[],true],"b":0,"a":[{"new":2}]}`,
		`[` + strings.TrimSuffix(strings.Repeat(`{"same":[0,1]},`, 70), ",") + `]`,
	} {
		test.Run(fmt.Sprintf("bytes=%d", len(value)), func(test *testing.T) {
			prepared := newDocToolOrdersFixture(test, value)
			fixture := newToolEventDisplayFixture(test, EventToolResult, prepared.keys.tree.raw)
			advanceToolEventDisplayFixture(test, fixture)
			plans, steps := 0, 0
			for fixture.state.Phase != "tool_orders_done" {
				if steps > 1000 {
					test.Fatal("generic orders did not finish")
				}
				before, _ := fixture.state.Encode()
				step, err := StepDocEventDisplay(context.Background(), fixture.state, fixture.loadRaw, fixture.loadObject, prepared.tools())
				if err != nil {
					test.Fatal("generic order step failed", fixture.state.Phase, err)
				}
				after, _ := fixture.state.Encode()
				if !bytes.Equal(before, after) {
					test.Fatal("generic order step changed prior state")
				}
				for _, object := range step.Objects() {
					fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
				}
				fixture.state = step.Preparation()
				encoded, err := fixture.state.Encode()
				if err != nil || len(encoded) > MaxDocEventDisplayPreparationBytes {
					test.Fatal("generic order outer checkpoint exceeded bounds", err)
				}
				fixture.state, err = DecodeDocEventDisplayPreparation(HashContent(encoded), encoded)
				if err != nil {
					test.Fatal(err)
				}
				if source, ordered, found := step.ToolOrders(); found {
					if source != *fixture.state.ToolOrders || len(ordered.Inputs()) > 16 {
						test.Fatal("generic order lost its sealed scope")
					}
					if plan, found := ordered.Plan(); found {
						encoded, err := plan.Encode(*fixture.state.ToolTree, *fixture.state.ToolKeys)
						if err != nil || !bytes.Equal(fixture.objects[HashContent(encoded)], encoded) {
							test.Fatal("generic order did not retain a completed immutable plan", err)
						}
						plans++
					}
				}
				steps++
			}
			containers := 0
			for _, node := range prepared.keys.tree.nodes {
				if node.Type == DocToolValueObject || node.Type == DocToolValueArray {
					containers++
				}
			}
			if plans != containers {
				test.Fatal("generic stage omitted a source container", plans, containers)
			}
			step, err := StepDocEventDisplay(context.Background(), fixture.state, fixture.loadRaw, fixture.loadObject, prepared.tools())
			if !errors.Is(err, ErrDocFindDisplayUnavailable) || !reflect.DeepEqual(step, DocEventDisplayStep{}) {
				test.Fatal("ordered metadata was mistaken for completed formatting", err)
			}
			if generation, err := ValidateDocEventDisplayPublication(context.Background(), fixture.state, fixture.loadObject); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(generation, DocDisplayProjectionGeneration{}) {
				test.Fatal("ordered metadata was published as display content", generation, err)
			}
			test.Logf("containers=%d generic_steps=%d", containers, steps)
		})
	}
}
