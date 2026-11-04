package domain

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGraphProjectionEncodeBudgets(t *testing.T) {
	repo := HashContent([]byte(t.Name()))
	cases := []struct {
		name  string
		graph GraphState
	}{
		{"top-level rows", GraphState{Version: 1, SnapshotIDs: make([]ContentHash, MaxPendingProjectionOutputRows+1)}},
		{"nested rows", GraphState{Version: 1, BranchSnapshots: map[string][]ContentHash{"main": make([]ContentHash, MaxPendingProjectionOutputRows)}}},
		{"operation rows", GraphState{Version: 1, Operations: GraphOperations{Births: []GraphBirth{{Children: make([]ContentHash, MaxPendingProjectionOutputRows)}}}}},
		{"aggregate rows", GraphState{Version: 1, SnapshotIDs: make([]ContentHash, 10001), GraphIDs: make([]ContentHash, 10000)}},
		{"huge string", GraphState{Version: 1, PrimaryBranch: strings.Repeat("x", MaxPendingProjectionOutputBytes)}},
		{"escaped string", GraphState{Version: 1, PrimaryBranch: strings.Repeat("<", MaxPendingProjectionOutputBytes/6+1)}},
		{"aggregate bytes", GraphState{Version: 1, PrimaryBranch: strings.Repeat("x", MaxPendingProjectionOutputBytes/2), OrphanSessions: []string{strings.Repeat("y", MaxPendingProjectionOutputBytes/2)}}},
		{"private identity", GraphState{Version: 1, Operations: GraphOperations{Merges: []GraphMerge{{identity: strings.Repeat("x", MaxPendingProjectionOutputBytes)}}}}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			parts, err := EncodeGraphProjection(context.Background(), repo, test.graph)
			if !errors.Is(err, ErrRepositoryViewSourceLimit) || parts != nil {
				t.Fatal("oversized graph encoded", err)
			}
		})
	}
	graph := GraphState{Version: 1, SnapshotIDs: make([]ContentHash, MaxPendingProjectionOutputRows)}
	parts, err := EncodeGraphProjection(context.Background(), repo, graph)
	if err != nil {
		t.Fatal("exact row budget rejected", err)
	}
	got, err := DecodeGraphProjection(context.Background(), repo, parts)
	if err != nil || !reflect.DeepEqual(got, graph) {
		t.Fatal("exact row budget failed round trip", err)
	}
	graph = GraphState{Version: 1, PrimaryBranch: strings.Repeat("x", MaxPendingProjectionOutputBytes-16<<10)}
	parts, err = EncodeGraphProjection(context.Background(), repo, graph)
	if err != nil {
		t.Fatal("near byte budget rejected", err)
	}
	size := 0
	for _, part := range parts {
		size += len(part.Kind) + len(part.Key) + len(part.Value) + 128
	}
	if size > MaxPendingProjectionOutputBytes {
		t.Fatal("encoded output exceeded budget")
	}
	if got, err := DecodeGraphProjection(context.Background(), repo, parts); err != nil || !reflect.DeepEqual(got, graph) {
		t.Fatal("near byte budget failed round trip", err)
	}
}

func TestGraphProjectionDecodeBudgets(t *testing.T) {
	repo, original := graphProjectionTestParts(t, GraphState{Version: 1})
	cases := []struct {
		name   string
		change func([]GraphProjectionPart) []GraphProjectionPart
	}{
		{"part count", func([]GraphProjectionPart) []GraphProjectionPart {
			return make([]GraphProjectionPart, MaxPendingProjectionOutputRows+1)
		}},
		{"part bytes", func(parts []GraphProjectionPart) []GraphProjectionPart {
			parts[0].Value = []byte(strings.Repeat(" ", MaxPendingProjectionOutputBytes))
			return parts
		}},
		{"aggregate bytes", func(parts []GraphProjectionPart) []GraphProjectionPart {
			parts[0].Value = []byte(strings.Repeat(" ", MaxPendingProjectionOutputBytes/2))
			parts[1].Value = []byte(strings.Repeat(" ", MaxPendingProjectionOutputBytes/2))
			return parts
		}},
		{"top-level rows", func(parts []GraphProjectionPart) []GraphProjectionPart {
			graphProjectionTestRebind(t, parts, "snapshot_ids", graphProjectionTestJSON(t, graphProjectionValue[[]ContentHash]{Value: make([]ContentHash, MaxPendingProjectionOutputRows+1)}))
			return parts
		}},
		{"nested rows", func(parts []GraphProjectionPart) []GraphProjectionPart {
			graphProjectionTestRebind(t, parts, "branch_snapshots", graphProjectionTestJSON(t, graphProjectionValue[map[string][]ContentHash]{Value: map[string][]ContentHash{"main": make([]ContentHash, MaxPendingProjectionOutputRows)}}))
			return parts
		}},
		{"aggregate rows", func(parts []GraphProjectionPart) []GraphProjectionPart {
			for _, key := range []string{"snapshot_ids", "graph_ids"} {
				graphProjectionTestRebind(t, parts, key, graphProjectionTestJSON(t, graphProjectionValue[[]ContentHash]{Value: make([]ContentHash, MaxPendingProjectionOutputRows/2+1)}))
			}
			return parts
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			parts := test.change(append([]GraphProjectionPart(nil), original...))
			got, err := DecodeGraphProjection(context.Background(), repo, parts)
			if !errors.Is(err, ErrRepositoryViewSourceLimit) || !reflect.DeepEqual(got, GraphState{}) {
				t.Fatal("oversized persisted graph decoded", err)
			}
		})
	}
}

func TestGraphProjectionRepeatedMembershipRowBudget(t *testing.T) {
	repo := HashContent([]byte(t.Name()))
	makeGraph := func(nodes int) GraphState {
		identifiers := make([]ContentHash, nodes)
		for index := range identifiers {
			identifiers[index] = HashContent([]byte(fmt.Sprintf("snapshot-%d", index)))
		}
		return GraphState{
			Version: 1, PrimaryBranch: "main",
			SnapshotIDs: identifiers, GraphIDs: identifiers, CommittedIDs: identifiers,
			HistoricalIDs: identifiers, SharedIDs: identifiers, PushedIDs: identifiers,
			BranchSnapshots: map[string][]ContentHash{"main": identifiers},
			BranchContexts:  map[string]BranchContext{"main": {BranchID: "main-id", SnapshotID: identifiers[0], SnapshotIDs: identifiers, Roots: []ContentHash{identifiers[0]}}},
			BranchHeads:     map[string]GraphBranchHead{"main-id": {Branch: "main"}},
			RefScopes:       map[string]string{"main": "main-id"},
			ScopeLabels:     map[string]string{"main-id": "main"},
		}
	}
	const membershipsPerNode, fixedRows = 8, 6
	maxNodes := (MaxPendingProjectionOutputRows - fixedRows) / membershipsPerNode
	want := makeGraph(maxNodes)
	budget := graphProjectionBudget{ctx: context.Background()}
	if err := budget.measure(reflect.ValueOf(want)); err != nil || budget.rows != membershipsPerNode*maxNodes+fixedRows {
		t.Fatal("unexpected repeated membership accounting", budget.rows, err)
	}
	parts, err := EncodeGraphProjection(context.Background(), repo, want)
	if err != nil {
		t.Fatal("measured maximum rejected", err)
	}
	if got, err := DecodeGraphProjection(context.Background(), repo, parts); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("measured maximum did not round trip", err)
	}
	if _, err := EncodeGraphProjection(context.Background(), repo, makeGraph(maxNodes+1)); !errors.Is(err, ErrRepositoryViewSourceLimit) {
		t.Fatal("graph exceeding repeated-membership budget accepted", err)
	}
	t.Logf("eight memberships per node plus six map/root rows: max %d nodes (%d rows), %d nodes exceed %d rows", maxNodes, budget.rows, maxNodes+1, MaxPendingProjectionOutputRows)
}

func TestGraphProjectionHeaderDiscoveryBounds(t *testing.T) {
	repo, parts := graphProjectionTestParts(t, GraphState{Version: 1})
	header := parts[graphProjectionTestIndex(t, parts, "view")]
	oversized := header
	oversized.Value = []byte(strings.Repeat(" ", MaxPendingProjectionOutputBytes))
	if kinds, keys, err := GraphProjectionPartKeys(oversized); !errors.Is(err, ErrRepositoryViewSourceLimit) || kinds != nil || keys != nil {
		t.Fatal("oversized discovery header accepted", err)
	}
	index := graphProjectionHeader{Version: 1, Rules: GraphProjectionRules, Projection: "graph-v1", RepoID: repo, Parts: make([]graphProjectionPartIdentity, MaxPendingProjectionOutputRows+1)}
	for position := range index.Parts {
		index.Parts[position] = graphProjectionPartIdentity{Kind: "field", Key: "version", Digest: HashContent([]byte("value"))}
	}
	header.Value = graphProjectionTestJSON(t, index)
	if kinds, keys, err := GraphProjectionPartKeys(header); !errors.Is(err, ErrRepositoryViewSourceLimit) || kinds != nil || keys != nil {
		t.Fatal("oversized discovery identities accepted", err)
	}
	index.Parts = index.Parts[:1]
	header.Value = graphProjectionTestJSON(t, index)
	if _, _, err := GraphProjectionPartKeys(header); !errors.Is(err, ErrIntegrity) {
		t.Fatal("incomplete field manifest accepted", err)
	}
	index.Parts = nil
	header.Value = graphProjectionTestJSON(t, index)
	if _, _, err := GraphProjectionPartKeys(header); !errors.Is(err, ErrIntegrity) {
		t.Fatal("null field manifest accepted", err)
	}
}

func TestGraphProjectionRejectsLossyValues(t *testing.T) {
	repo := HashContent([]byte(t.Name()))
	for _, graph := range []GraphState{
		{Version: 1, PrimaryBranch: string([]byte{0xff})},
		{Version: 1, ScopeLabels: map[string]string{string([]byte{0xff}): "bad key"}},
		{Version: 1, Positions: []GraphPosition{{CreatedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}}},
	} {
		if parts, err := EncodeGraphProjection(context.Background(), repo, graph); !errors.Is(err, ErrIntegrity) || parts != nil {
			t.Fatal("lossy or invalid JSON value encoded", err)
		}
	}
}

type graphProjectionTestContext struct {
	context.Context
	checks      int
	cancelAfter int
}

func (ctx *graphProjectionTestContext) Err() error {
	ctx.checks++
	if ctx.cancelAfter != 0 && ctx.checks >= ctx.cancelAfter {
		return context.Canceled
	}
	return nil
}

func TestGraphProjectionCancellationDuringWork(t *testing.T) {
	repo := HashContent([]byte("graph projection repository"))
	graph := graphProjectionTestState(repo)
	_, parts := graphProjectionTestParts(t, graph)
	for _, operation := range []string{"encode", "decode"} {
		t.Run(operation, func(t *testing.T) {
			run := func(ctx context.Context) error {
				if operation == "encode" {
					got, err := EncodeGraphProjection(ctx, repo, graph)
					if err != nil && got != nil {
						t.Fatal("canceled encoder returned partial data")
					}
					return err
				}
				got, err := DecodeGraphProjection(ctx, repo, parts)
				if err != nil && !reflect.DeepEqual(got, GraphState{}) {
					t.Fatal("canceled decoder returned partial data")
				}
				return err
			}
			counter := &graphProjectionTestContext{Context: context.Background()}
			if err := run(counter); err != nil {
				t.Fatal(err)
			}
			for _, limit := range []int{1, 50, counter.checks / 2, counter.checks - 1, counter.checks} {
				ctx := &graphProjectionTestContext{Context: context.Background(), cancelAfter: limit}
				if err := run(ctx); !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation at check %d/%d lost: %v", limit, counter.checks, err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := run(ctx); !errors.Is(err, context.Canceled) {
				t.Fatal("pre-cancellation lost", err)
			}
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer cancel()
			if err := run(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("deadline lost", err)
			}
		})
	}
}

func TestGraphProjectionConcurrentReaders(t *testing.T) {
	repo := HashContent([]byte("graph projection repository"))
	graph := graphProjectionTestState(repo)
	_, parts := graphProjectionTestParts(t, graph)
	header := parts[graphProjectionTestIndex(t, parts, "view")]
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Go(func() {
			for iteration := 0; iteration < 3; iteration++ {
				got, err := DecodeGraphProjection(context.Background(), repo, parts)
				if err != nil || !reflect.DeepEqual(got, graph) {
					t.Error("concurrent decode", err)
					return
				}
				next, err := EncodeGraphProjection(context.Background(), repo, graph)
				if err != nil || !reflect.DeepEqual(next, parts) {
					t.Error("concurrent encode", err)
					return
				}
				if _, _, err := GraphProjectionPartKeys(header); err != nil {
					t.Error(fmt.Errorf("concurrent discovery: %w", err))
					return
				}
			}
		})
	}
	workers.Wait()
}
