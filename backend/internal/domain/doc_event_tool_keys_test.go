package domain

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func eventToolKeyNodes(test *testing.T, raw []byte) DocToolTreeNodeLoader {
	test.Helper()
	tree := newDocToolTreeFixture(test, string(raw))
	if err := tree.scan(test); err != nil {
		test.Fatal(err)
	}
	nodes := slices.Clone(tree.nodes)
	slices.SortFunc(nodes, func(left, right DocToolTreeNode) int { return left.Value.From - right.Value.From })
	return func(ctx context.Context, after, limit int) ([]DocToolTreeNode, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if limit < 1 || limit > DocToolKeyMapStepNodes {
			test.Fatal("unbounded node request", limit)
		}
		result := make([]DocToolTreeNode, 0, limit)
		for _, node := range nodes {
			if node.Value.From > after {
				result = append(result, cloneDocToolTreeNode(node))
				if len(result) == limit {
					break
				}
			}
		}
		return result, nil
	}
}

func TestDocEventToolKeyMapActualOrchestrationAndUnfinishedBoundary(test *testing.T) {
	for _, long := range []bool{false, true} {
		test.Run(map[bool]string{false: "short", true: "long"}[long], func(test *testing.T) {
			key := "a"
			if long {
				key = strings.Repeat("KEY_MARKER_", 14000)
			}
			raw := []byte(`{"kind":"tool_result","output":{"2":0,"\u0032":[1],"` + key + `":{"old":1},"` + key + `":{"new":2}}}`)
			fixture := newToolEventDisplayFixture(test, EventToolResult, raw)
			advanceToolEventDisplayFixture(test, fixture)
			nodes := eventToolKeyNodes(test, raw)
			originalReads := fixture.rawBytes
			keys, steps := 0, 0
			phases := make(map[string]bool)
			for fixture.state.Phase != "tool_keys_done" {
				if steps > 2000 {
					test.Fatal("outer key-map preparation made no progress")
				}
				before, err := fixture.state.Encode()
				if err != nil {
					test.Fatal(err)
				}
				step, err := StepDocEventDisplay(context.Background(), fixture.state, fixture.loadRaw, fixture.loadObject, DocToolPreparationReader{Nodes: nodes})
				if err != nil {
					test.Fatal("actual generic key step failed", fixture.state.Phase, err)
				}
				after, err := fixture.state.Encode()
				if err != nil || !bytes.Equal(before, after) {
					test.Fatal("generic key step changed the prior preparation")
				}
				for _, object := range step.Objects() {
					fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
				}
				fixture.state = step.Preparation()
				encoded, err := fixture.state.Encode()
				if err != nil || len(encoded) > MaxDocEventDisplayPreparationBytes || bytes.Contains(encoded, []byte("KEY_MARKER_")) {
					test.Fatal("outer state is not bounded and body-free", err)
				}
				fixture.state, err = DecodeDocEventDisplayPreparation(HashContent(encoded), encoded)
				if err != nil {
					test.Fatal(err)
				}
				if source, mapped, found := step.ToolKeyMap(); found {
					if source != *fixture.state.ToolKeys || mapped.ReadBytes() > DocFieldDiscoveryStepBytes {
						test.Fatal("sealed key map lost source or read bounds")
					}
					state := mapped.State()
					phases[state.Phase] = true
					keys += len(mapped.Items())
					if !long && len(mapped.Objects()) != 0 {
						test.Fatal("short keys retained disposable quoted-string plans")
					}
				}
				steps++
			}
			if keys != 6 || long && (!phases["string"] || !phases["digest"]) {
				test.Fatal("key-map stages skipped original members or long-key preparation", keys, phases)
			}
			step, err := StepDocEventDisplay(context.Background(), fixture.state, fixture.loadRaw, fixture.loadObject, DocToolPreparationReader{Nodes: nodes})
			if !errors.Is(err, ErrDocFindDisplayUnavailable) || !reflect.DeepEqual(step, DocEventDisplayStep{}) || fixture.state.Generation != "" {
				test.Fatal("private key completion was mistaken for a formatter", err)
			}
			if generation, err := ValidateDocEventDisplayPublication(context.Background(), fixture.state, fixture.loadObject); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(generation, DocDisplayProjectionGeneration{}) {
				test.Fatal("private key completion was published", generation, err)
			}
			test.Logf("long=%t original_key_occurrences=%d generic_steps=%d requested_key_raw_bytes=%d", long, keys, steps, fixture.rawBytes-originalReads)
		})
	}
}
