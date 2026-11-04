package domain

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGraphViewProjectionEncodeBudgets(t *testing.T) {
	repo := HashContent([]byte(t.Name()))
	for _, test := range []struct {
		name   string
		change func(*RepositoryView)
	}{
		{"aggregate rows", func(view *RepositoryView) {
			view.Graph.SnapshotIDs = make([]ContentHash, MaxPendingProjectionOutputRows/2)
			view.Snapshots = []Snapshot{{RepoID: repo, Branches: make([]string, MaxPendingProjectionOutputRows/2)}}
		}},
		{"nested command rows", func(view *RepositoryView) {
			view.History = []HistoryEvent{{RepoID: string(repo), Creation: &GitCreation{Command: make([]string, MaxPendingProjectionOutputRows)}}}
		}},
		{"nested archive rows", func(view *RepositoryView) {
			view.ArchivedSessions = []SessionArchiveView{{SessionArchive: SessionArchive{RepoID: repo}, SnapshotIDs: make([]ContentHash, MaxPendingProjectionOutputRows)}}
		}},
		{"aggregate bytes", func(view *RepositoryView) {
			view.Graph.PrimaryBranch = strings.Repeat("x", MaxPendingProjectionOutputBytes/2)
			view.DefaultBranch = strings.Repeat("y", MaxPendingProjectionOutputBytes/2)
		}},
		{"escaped metadata", func(view *RepositoryView) {
			view.Snapshots = []Snapshot{{RepoID: repo, Message: strings.Repeat("<", MaxPendingProjectionOutputBytes/6)}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			view := RepositoryView{Graph: &GraphState{Version: 1}}
			test.change(&view)
			if parts, err := EncodeGraphViewProjection(context.Background(), repo, view); !errors.Is(err, ErrRepositoryViewSourceLimit) || parts != nil {
				t.Fatal("oversized bundle encoded", err)
			}
		})
	}
	view := RepositoryView{
		Graph:     &GraphState{Version: 1, SnapshotIDs: make([]ContentHash, MaxPendingProjectionOutputRows/2-1)},
		Snapshots: []Snapshot{{RepoID: repo, Branches: make([]string, MaxPendingProjectionOutputRows/2)}},
	}
	parts, err := EncodeGraphViewProjection(context.Background(), repo, view)
	if err != nil {
		t.Fatal("exact whole-bundle row cap rejected", err)
	}
	if got, err := DecodeGraphViewProjection(context.Background(), repo, parts); err != nil || !reflect.DeepEqual(got, view) {
		t.Fatal("exact row cap lost bundle data", err)
	}
	view = RepositoryView{Graph: &GraphState{Version: 1}, DefaultBranch: strings.Repeat("x", MaxPendingProjectionOutputBytes-32<<10)}
	parts, err = EncodeGraphViewProjection(context.Background(), repo, view)
	if err != nil {
		t.Fatal("near byte cap rejected", err)
	}
	if got, err := DecodeGraphViewProjection(context.Background(), repo, parts); err != nil || !reflect.DeepEqual(got, view) {
		t.Fatal("near byte cap lost bundle data", err)
	}
}

func TestGraphViewProjectionDecodeBudgetsBeforeParse(t *testing.T) {
	repo := HashContent([]byte(t.Name()))
	original, err := EncodeGraphViewProjection(context.Background(), repo, RepositoryView{Graph: &GraphState{Version: 1}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func([]GraphProjectionPart) []GraphProjectionPart
	}{
		{"part count", func([]GraphProjectionPart) []GraphProjectionPart {
			return make([]GraphProjectionPart, MaxPendingProjectionOutputRows+1)
		}},
		{"body bytes", func(parts []GraphProjectionPart) []GraphProjectionPart {
			parts[0].Value = []byte(strings.Repeat("!", MaxPendingProjectionOutputBytes))
			return parts
		}},
		{"aggregate before identity and parse", func(parts []GraphProjectionPart) []GraphProjectionPart {
			parts[0] = GraphProjectionPart{Kind: "invalid", Key: "", Value: []byte("not-json")}
			parts[1].Value = []byte(strings.Repeat("!", MaxPendingProjectionOutputBytes/2))
			parts[2].Value = []byte(strings.Repeat("!", MaxPendingProjectionOutputBytes/2))
			return parts
		}},
		{"aggregate nested rows", func(parts []GraphProjectionPart) []GraphProjectionPart {
			graphProjectionTestRebind(t, parts, "graph.snapshot_ids", graphProjectionTestJSON(t, graphProjectionValue[[]ContentHash]{Value: make([]ContentHash, MaxPendingProjectionOutputRows/2)}))
			graphProjectionTestRebind(t, parts, "snapshots", graphProjectionTestJSON(t, graphProjectionValue[[]graphViewProjectionSnapshot]{Value: []graphViewProjectionSnapshot{{graphViewProjectionSnapshotValue: graphViewProjectionSnapshotValue{RepoID: repo}, Branches: make([]string, MaxPendingProjectionOutputRows/2)}}}))
			return parts
		}},
		{"nested command rows", func(parts []GraphProjectionPart) []GraphProjectionPart {
			graphProjectionTestRebind(t, parts, "history", graphProjectionTestJSON(t, graphProjectionValue[[]graphViewProjectionHistory]{Value: []graphViewProjectionHistory{{HistoryEvent: HistoryEvent{RepoID: string(repo)}, Creation: &graphViewProjectionCreation{Command: make([]string, MaxPendingProjectionOutputRows)}}}}))
			return parts
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			parts := test.change(append([]GraphProjectionPart(nil), original...))
			if got, err := DecodeGraphViewProjection(context.Background(), repo, parts); !errors.Is(err, ErrRepositoryViewSourceLimit) || !reflect.DeepEqual(got, RepositoryView{}) {
				t.Fatal("oversized bundle was parsed or partially decoded", err)
			}
		})
	}
}

func TestGraphViewProjectionHeaderDiscovery(t *testing.T) {
	repo := HashContent([]byte(t.Name()))
	parts, err := EncodeGraphViewProjection(context.Background(), repo, RepositoryView{Graph: &GraphState{Version: 1}})
	if err != nil {
		t.Fatal(err)
	}
	header := parts[len(parts)-1]
	for _, test := range []struct{ from, to string }{
		{`"version":1`, `"version":2`},
		{`"rules":2`, `"rules":1`},
		{`"projection":"graph-view-v2"`, `"projection":"graph-v1"`},
		{string(repo), "invalid-repo"},
		{`"rules":2`, `"rules":2,"scope":"repository-view-v1"`},
		{`"rules":2`, `"rules":2,"rules":2`},
		{`"rules":2`, `"Rules":2`},
		{`"rules":2`, `"rules":null`},
		{`"kind":"field"`, `"kind":"snapshot"`},
		{`"key":"graph.integrations"`, `"key":"read_generation"`},
		{`"key":"graph.integrations"`, `"key":"graph.read_generation"`},
		{`"key":"graph.integrations"`, `"key":"graph.branch_contexts"`},
		{`"digest":"sha256:`, `"digest":"sha512:`},
	} {
		t.Run(test.to, func(t *testing.T) {
			changed := header
			changed.Value = bytes.Replace(header.Value, []byte(test.from), []byte(test.to), 1)
			if kinds, keys, err := GraphViewProjectionPartKeys(changed); !errors.Is(err, ErrIntegrity) || kinds != nil || keys != nil {
				t.Fatal("invalid manifest exposed storage keys", err)
			}
		})
	}
	oversized := header
	oversized.Value = []byte(strings.Repeat("!", MaxPendingProjectionOutputBytes))
	if kinds, keys, err := GraphViewProjectionPartKeys(oversized); !errors.Is(err, ErrRepositoryViewSourceLimit) || kinds != nil || keys != nil {
		t.Fatal("oversized header parsed before cap", err)
	}
	index := graphProjectionHeader{Version: 1, Rules: GraphViewProjectionRules, Projection: "graph-view-v2", RepoID: repo, Parts: make([]graphProjectionPartIdentity, MaxPendingProjectionOutputRows+1)}
	for position := range index.Parts {
		index.Parts[position] = graphProjectionPartIdentity{Kind: "field", Key: "revision", Digest: HashContent(nil)}
	}
	oversized.Value = graphProjectionTestJSON(t, index)
	if kinds, keys, err := GraphViewProjectionPartKeys(oversized); !errors.Is(err, ErrRepositoryViewSourceLimit) || kinds != nil || keys != nil {
		t.Fatal("unbounded descriptor discovery", err)
	}
	index.Parts = index.Parts[:1]
	oversized.Value = graphProjectionTestJSON(t, index)
	if _, _, err := GraphViewProjectionPartKeys(oversized); !errors.Is(err, ErrIntegrity) {
		t.Fatal("partial descriptor discovery", err)
	}
}

func TestGraphViewProjectionCancellationAndLossyValues(t *testing.T) {
	repo := HashContent([]byte(t.Name()))
	view := graphViewProjectionTestView(repo)
	parts, err := EncodeGraphViewProjection(context.Background(), repo, view)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"encode", "decode"} {
		t.Run(operation, func(t *testing.T) {
			run := func(ctx context.Context) error {
				if operation == "encode" {
					got, err := EncodeGraphViewProjection(ctx, repo, view)
					if err != nil && got != nil {
						t.Fatal("canceled encoder returned parts")
					}
					return err
				}
				got, err := DecodeGraphViewProjection(ctx, repo, parts)
				if err != nil && !reflect.DeepEqual(got, RepositoryView{}) {
					t.Fatal("canceled decoder returned view")
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
					t.Fatal("in-work cancellation lost", limit, err)
				}
			}
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer cancel()
			if err := run(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("deadline lost", err)
			}
		})
	}
	for _, change := range []func(*RepositoryView){
		func(view *RepositoryView) { view.Snapshots[0].Message = string([]byte{0xff}) },
		func(view *RepositoryView) { view.History[0].Creation.Command[0] = string([]byte{0xff}) },
		func(view *RepositoryView) { view.Pending[0].UpdatedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
	} {
		invalid := graphViewProjectionTestView(repo)
		change(&invalid)
		if parts, err := EncodeGraphViewProjection(context.Background(), repo, invalid); !errors.Is(err, ErrIntegrity) || parts != nil {
			t.Fatal("lossy bundle value persisted", err)
		}
	}
}
