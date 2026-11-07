package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGraphProjectionCostDuplicateMapKeys(test *testing.T) {
	valid := []byte(`{"value":{"left":{"same":["first"]},"right":{"same":["second"],"empty":[],"nil":null}}}`)
	want := map[string]map[string][]string{
		"left":  {"same": {"first"}},
		"right": {"same": {"second"}, "empty": {}, "nil": nil},
	}
	for name, raw := range map[string]string{
		"outer":         `{"value":{"left":{},"left":{}}}`,
		"nested":        `{"value":{"left":{"same":[],"same":null}}}`,
		"escaped outer": `{"value":{"left":{},"le\u0066t":{}}}`,
		"escaped inner": `{"value":{"left":{"same":[],"\u0073ame":[]}}}`,
		"unicode key":   `{"value":{"left":{"` + "\ud55c" + `":[],"\ud55c":[]}}}`,
	} {
		test.Run(name, func(test *testing.T) {
			var got map[string]map[string][]string
			field := graphProjectionTypedField("maps", &got)
			budget := newGraphProjectionBudget(context.Background(), GraphViewProjectionRules)
			if err := field.decode(budget.ctx, valid, &budget); err != nil || !reflect.DeepEqual(got, want) {
				test.Fatal("sibling keys or nil/empty values changed", got, err)
			}
			budget = newGraphProjectionBudget(context.Background(), GraphViewProjectionRules)
			if err := field.decode(budget.ctx, []byte(raw), &budget); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(got, want) {
				test.Fatal("duplicate map key accepted or partially assigned", got, err)
			}
		})
	}
}

type graphProjectionCostRecursiveRow struct {
	Next *graphProjectionCostRecursiveRow `json:"next,omitempty"`
}

func TestGraphProjectionCostDepthBoundary(test *testing.T) {
	for _, depth := range []int{64, 65} {
		test.Run(fmt.Sprintf("depth_%d", depth), func(test *testing.T) {
			raw := []byte(`{"value":` + strings.Repeat(`{"next":`, depth-1) + `{}` + strings.Repeat(`}`, depth-1) + `}`)
			var got *graphProjectionCostRecursiveRow
			budget := newGraphProjectionBudget(context.Background(), GraphViewProjectionRules)
			err := graphProjectionTypedField("recursive", &got).decode(budget.ctx, raw, &budget)
			var legacy graphProjectionValue[*graphProjectionCostRecursiveRow]
			legacyErr := DecodeRepositorySourceJSON(context.Background(), raw, &legacy)
			if depth == 65 {
				if !errors.Is(err, ErrIntegrity) || !errors.Is(legacyErr, ErrIntegrity) || got != nil {
					test.Fatal("depth limit changed or returned partial data", err, legacyErr)
				}
				return
			}
			if err != nil || legacyErr != nil || !reflect.DeepEqual(got, legacy.Value) {
				test.Fatal("supported recursive schema failed at exact depth", err, legacyErr)
			}
			count := 0
			for node := got; node != nil; node = node.Next {
				count++
			}
			if count != depth {
				test.Fatal("recursive fixture did not reach the depth boundary", count)
			}
		})
	}
}

type graphProjectionCostTypedRow struct {
	Quoted  uint64    `json:"quoted,string"`
	Integer int64     `json:"integer"`
	Stamp   time.Time `json:"stamp"`
	Label   string    `json:"label"`
}

func TestGraphProjectionCostTypedValidation(test *testing.T) {
	valid := []byte(`{"value":{"quoted":"42","integer":7,"stamp":"2026-11-07T12:34:56.123456789Z","label":"kept"}}`)
	for _, scenario := range []struct {
		name string
		from string
		to   string
	}{
		{"unquoted integer", `"quoted":"42"`, `"quoted":42`},
		{"invalid quoted integer", `"quoted":"42"`, `"quoted":"not-a-number"`},
		{"quoted overflow", `"quoted":"42"`, `"quoted":"18446744073709551616"`},
		{"negative unsigned", `"quoted":"42"`, `"quoted":"-1"`},
		{"null quoted integer", `"quoted":"42"`, `"quoted":null`},
		{"integer overflow", `"integer":7`, `"integer":9223372036854775808`},
		{"integer underflow", `"integer":7`, `"integer":-9223372036854775809`},
		{"fractional integer", `"integer":7`, `"integer":1.5`},
		{"invalid time", `"stamp":"2026-11-07T12:34:56.123456789Z"`, `"stamp":"not-a-time"`},
		{"invalid UTF8", `"label":"kept"`, "\"label\":\"\xff\""},
		{"trailing value", `"label":"kept"}}`, `"label":"kept"}} {}`},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			got := graphProjectionCostTypedRow{Quoted: 91, Integer: -9, Stamp: time.Unix(1, 2).UTC(), Label: "unchanged"}
			before := got
			budget := newGraphProjectionBudget(context.Background(), GraphViewProjectionRules)
			raw := bytes.Replace(valid, []byte(scenario.from), []byte(scenario.to), 1)
			if bytes.Equal(raw, valid) {
				test.Fatal("invalid fixture did not change the valid input")
			}
			if err := graphProjectionTypedField("typed", &got).decode(budget.ctx, raw, &budget); !errors.Is(err, ErrIntegrity) || got != before {
				test.Fatal("invalid typed value accepted or partially assigned", got, err)
			}
		})
	}
	var got graphProjectionCostTypedRow
	var legacy graphProjectionValue[graphProjectionCostTypedRow]
	budget := newGraphProjectionBudget(context.Background(), GraphViewProjectionRules)
	if err := graphProjectionTypedField("typed", &got).decode(budget.ctx, valid, &budget); err != nil {
		test.Fatal(err)
	}
	if err := DecodeRepositorySourceJSON(context.Background(), valid, &legacy); err != nil || got != legacy.Value {
		test.Fatal("typed decode changed valid values", got, legacy.Value, err)
	}
}

func TestGraphProjectionCostStringMeasurement(test *testing.T) {
	ascii := make([]byte, 128)
	for index := range ascii {
		ascii[index] = byte(index)
	}
	cases := map[string]string{
		"empty":       "",
		"ASCII":       strings.Repeat("plain-ascii/09AZaz", 256),
		"all ASCII":   string(ascii),
		"escapes":     "\"\\\n\r\t\b\f\x00\x1f<>&",
		"UTF8":        "\ud55c\uae00é🙂�\u2028\u2029",
		"many HTML":   strings.Repeat("<>&", 700),
		"many UTF8":   strings.Repeat("\ud55cé🙂\u2028", 350),
		"exact chunk": strings.Repeat("x", 1024),
	}
	for _, prefix := range []int{1021, 1022, 1023, 1024, 1025} {
		for _, suffix := range []string{"é", "\ud55c", "🙂", "\u2028", "\u2029", "<", "\n", "\\"} {
			cases[fmt.Sprintf("boundary_%d_%x", prefix, []byte(suffix))] = strings.Repeat("x", prefix) + suffix + strings.Repeat("y", 1025)
		}
	}
	for name, value := range cases {
		test.Run(name, func(test *testing.T) {
			raw, err := json.Marshal(value)
			if err != nil {
				test.Fatal(err)
			}
			const priorBytes = 17
			budget := newGraphProjectionBudget(context.Background(), GraphViewProjectionRules)
			budget.size, budget.maxBytes = priorBytes, priorBytes+len(raw)
			if err := budget.measureString(value); err != nil || budget.size != priorBytes+len(raw) {
				test.Fatal("measurement differs from JSON encoding", budget.size, len(raw), err)
			}
			budget.size, budget.maxBytes = priorBytes, priorBytes+len(raw)-1
			if err := budget.measureString(value); !errors.Is(err, ErrRepositoryViewSourceLimit) {
				test.Fatal("escaped byte budget was not enforced", err)
			}
		})
	}
	for _, value := range []string{"\x80", "\xc0\xaf", "\xe2\x82", "\xed\xa0\x80", "\xf4\x90\x80\x80", strings.Repeat("x", 1023) + "\xe2\x82", strings.Repeat("x", 1024) + "\xff"} {
		budget := newGraphProjectionBudget(context.Background(), GraphViewProjectionRules)
		if err := budget.measureString(value); !errors.Is(err, ErrIntegrity) {
			test.Fatalf("malformed UTF8 accepted: %x: %v", value, err)
		}
	}
}

func TestGraphProjectionCostCancellation(test *testing.T) {
	test.Run("field assignment", func(test *testing.T) {
		raw := []byte(`{"value":{"left":["first","second"],"right":["third"]}}`)
		counter := &graphProjectionTestContext{Context: context.Background()}
		budget := newGraphProjectionBudget(counter, GraphViewProjectionRules)
		if err := graphProjectionJSONSchema(counter, raw, reflect.TypeFor[graphProjectionValue[map[string][]string]](), &budget); err != nil {
			test.Fatal(err)
		}
		for _, limit := range []int{1, counter.checks / 2, counter.checks + 1} {
			ctx := &graphProjectionTestContext{Context: context.Background(), cancelAfter: limit}
			got := map[string][]string{"sentinel": {"unchanged"}}
			budget := newGraphProjectionBudget(ctx, GraphViewProjectionRules)
			if err := graphProjectionTypedField("maps", &got).decode(ctx, raw, &budget); !errors.Is(err, context.Canceled) ||
				!reflect.DeepEqual(got, map[string][]string{"sentinel": {"unchanged"}}) {
				test.Fatal("canceled field returned partially assigned values", limit, got, err)
			}
		}
	})
	test.Run("string chunks", func(test *testing.T) {
		for _, value := range []string{strings.Repeat("x", 4096), strings.Repeat("<", 4096), strings.Repeat("\ud55c", 1400)} {
			ctx := &graphProjectionTestContext{Context: context.Background(), cancelAfter: 3}
			budget := newGraphProjectionBudget(ctx, GraphViewProjectionRules)
			if err := budget.measureString(value); !errors.Is(err, context.Canceled) {
				test.Fatal("chunked string measurement ignored cancellation", err)
			}
		}
	})
	repo, view := graphProjectionCostView(24, 4)
	parts, err := EncodeGraphViewProjection(context.Background(), repo, view)
	if err != nil {
		test.Fatal(err)
	}
	for _, operation := range []string{"encode", "decode"} {
		test.Run(operation, func(test *testing.T) {
			run := func(ctx context.Context) error {
				if operation == "encode" {
					got, err := EncodeGraphViewProjection(ctx, repo, view)
					if err != nil && got != nil {
						test.Fatal("canceled encoder returned partial parts")
					}
					return err
				}
				got, err := DecodeGraphViewProjection(ctx, repo, parts)
				if err != nil && !reflect.DeepEqual(got, RepositoryView{}) {
					test.Fatal("canceled decoder returned a partial view")
				}
				return err
			}
			counter := &graphProjectionTestContext{Context: context.Background()}
			if err := run(counter); err != nil {
				test.Fatal(err)
			}
			for _, limit := range []int{1, counter.checks / 2, counter.checks - 1, counter.checks} {
				ctx := &graphProjectionTestContext{Context: context.Background(), cancelAfter: limit}
				if err := run(ctx); !errors.Is(err, context.Canceled) {
					test.Fatal("cancellation was lost", limit, counter.checks, err)
				}
			}
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer cancel()
			if err := run(ctx); !errors.Is(err, context.DeadlineExceeded) {
				test.Fatal("expired deadline was lost", err)
			}
		})
	}
}

func graphProjectionCostView(snapshotCount, branchCount int) (ContentHash, RepositoryView) {
	repo := HashContent([]byte("synthetic-graph-codec-cost"))
	stamp := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	identifiers := make([]ContentHash, snapshotCount)
	view := RepositoryView{DefaultBranch: "main", Snapshots: make([]Snapshot, snapshotCount)}
	for index := range identifiers {
		identifiers[index] = HashContent([]byte(fmt.Sprintf("synthetic-snapshot-%d", index)))
		view.Snapshots[index] = Snapshot{RepoID: repo, ID: identifiers[index], CreatedAt: stamp.Add(time.Duration(index) * time.Second),
			Branch: "main", Message: strings.Repeat("synthetic <change> \ud55c\uae00 ", 4)}
		if index > 0 {
			view.Snapshots[index].Parents = []ContentHash{identifiers[index-1]}
		}
	}
	view.Graph = &GraphState{Version: 1, PrimaryBranch: "main", SnapshotIDs: identifiers, GraphIDs: identifiers,
		BranchSnapshots: make(map[string][]ContentHash, branchCount), BranchContexts: make(map[string]BranchContext, branchCount)}
	for index := range branchCount {
		branch := fmt.Sprintf("branch-%d", index)
		view.Graph.BranchSnapshots[branch] = identifiers
		view.Graph.BranchContexts[branch] = BranchContext{BranchID: branch, SnapshotID: identifiers[snapshotCount-1],
			CodeCommit: strings.Repeat("a", 40), Reason: "synthetic", Roots: []ContentHash{identifiers[0]}, SnapshotIDs: identifiers}
	}
	return repo, view
}

func BenchmarkGraphProjectionCodecCost(benchmark *testing.B) {
	repo, view := graphProjectionCostView(384, 8)
	parts, err := EncodeGraphViewProjection(context.Background(), repo, view)
	if err != nil {
		benchmark.Fatal(err)
	}
	decoded, err := DecodeGraphViewProjection(context.Background(), repo, parts)
	if err != nil || !reflect.DeepEqual(decoded, view) {
		benchmark.Fatal("synthetic fixture did not round trip", err)
	}
	var encodedBytes int64
	for _, part := range parts {
		encodedBytes += int64(len(part.Kind) + len(part.Key) + len(part.Value) + 128)
	}
	benchmark.Run("encode", func(benchmark *testing.B) {
		benchmark.ReportAllocs()
		benchmark.SetBytes(encodedBytes)
		for benchmark.Loop() {
			ctx, cancel := context.WithTimeout(benchmark.Context(), 30*time.Second)
			encoded, err := EncodeGraphViewProjection(ctx, repo, view)
			cancel()
			if err != nil || len(encoded) != len(parts) {
				benchmark.Fatal("encode failed", err)
			}
		}
	})
	benchmark.Run("decode", func(benchmark *testing.B) {
		benchmark.ReportAllocs()
		benchmark.SetBytes(encodedBytes)
		for benchmark.Loop() {
			ctx, cancel := context.WithTimeout(benchmark.Context(), 30*time.Second)
			decoded, err := DecodeGraphViewProjection(ctx, repo, parts)
			cancel()
			if err != nil || len(decoded.Snapshots) != len(view.Snapshots) {
				benchmark.Fatal("decode failed", err)
			}
		}
	})
}
