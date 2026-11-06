package domain

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGraphViewProjectionLargeRoundTrip(t *testing.T) {
	ctx := context.Background()
	repo := HashContent([]byte(t.Name()))
	const count = 4096
	identifiers := make([]ContentHash, count)
	snapshots := make([]Snapshot, count)
	for index := range snapshots {
		identifier := HashContent([]byte(fmt.Sprintf("snapshot-%d", index)))
		identifiers[index] = identifier
		snapshots[index] = Snapshot{
			ID: identifier, RepoID: repo, DocHash: identifier, Branch: "main", Branches: []string{"main", "topic"},
			Message: strings.Repeat("large graph metadata ", 32), CreatedAt: time.Unix(int64(index), 0).UTC(),
			Models: []string{"model"},
		}
		if index > 0 {
			snapshots[index].Parents = []ContentHash{identifiers[index-1]}
		}
	}
	graph := GraphState{
		Version: 1, PrimaryBranch: "main", Revision: RepositoryRevision{Graph: 17, Pending: 3, Evidence: 5},
		SnapshotIDs: identifiers, GraphIDs: identifiers, CommittedIDs: identifiers,
		HistoricalIDs: identifiers, SharedIDs: identifiers, PushedIDs: identifiers,
		BranchSnapshots: map[string][]ContentHash{"main": identifiers},
		BranchContexts: map[string]BranchContext{"main": {
			BranchID: "main-id", SnapshotID: identifiers[count-1], SnapshotIDs: identifiers, Roots: []ContentHash{identifiers[0]},
		}},
	}
	view := RepositoryView{Graph: &graph, Revision: graph.Revision, DefaultBranch: "main", Snapshots: snapshots}
	budget := newGraphProjectionBudget(ctx, GraphViewProjectionRules)
	if err := budget.measure(reflect.ValueOf(view)); err != nil || budget.rows <= MaxPendingProjectionOutputRows {
		t.Fatal("large fixture must exceed legacy structural budget", budget.rows, err)
	}
	parts, err := EncodeGraphViewProjection(ctx, repo, view)
	if err != nil {
		t.Fatal(err)
	}
	size := 0
	for _, part := range parts {
		size += len(part.Kind) + len(part.Key) + len(part.Value) + 128
	}
	if size <= MaxPendingProjectionOutputBytes || size > MaxGraphViewProjectionOutputBytes {
		t.Fatal("large fixture must exceed only the legacy byte budget", size)
	}
	decoded, err := DecodeGraphViewProjection(ctx, repo, parts)
	if err != nil || !reflect.DeepEqual(decoded, view) {
		t.Fatal("large graph view did not round trip", err)
	}
	digest, err := GraphViewDigest(ctx, repo, decoded)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := GraphViewProjectionManifestDigest(ctx, repo, parts)
	if err != nil || manifest != digest {
		t.Fatal("large view and manifest digests disagree", err)
	}
	projection := graphReadQualificationTestProjection(t)
	projection.Parts = parts
	projection.Qualification.Expected, projection.Qualification.Actual = digest, digest
	if err := ValidateGraphReadQualification(ctx, projection); err != nil {
		t.Fatal("large qualified bundle rejected", err)
	}
	if parts, err := EncodeGraphProjection(ctx, repo, graph); !errors.Is(err, ErrRepositoryViewSourceLimit) || parts != nil {
		t.Fatal("graph-v1 structural budget changed", err)
	}
	for index, part := range parts {
		if part.Key == "snapshots" {
			parts[index].Value = []byte(strings.Replace(string(part.Value), "large graph metadata", "wrong graph metadata", 1))
			break
		}
	}
	if _, err := DecodeGraphViewProjection(ctx, repo, parts); !errors.Is(err, ErrIntegrity) {
		t.Fatal("large graph bypassed field digest validation", err)
	}
	t.Logf("large bundle: %d snapshots, %d structural entries, %d encoded bytes", count, budget.rows, size)
}

func TestGraphViewProjectionBudgetsLeavePendingAndLegacyUnchanged(t *testing.T) {
	ctx := context.Background()
	repo, _, view := pendingProjectionFixture(t, 0)
	if MaxPendingProjectionOutputBytes != 4<<20 || MaxPendingProjectionOutputRows != 20000 {
		t.Fatal("pending output contract changed")
	}
	legacy := newGraphProjectionBudget(ctx, GraphProjectionRules)
	current := newGraphProjectionBudget(ctx, GraphViewProjectionRules)
	if legacy.maxBytes != 4<<20 || legacy.maxRows != 20000 || legacy.maxParts != 20000 ||
		current.maxBytes != 32<<20 || current.maxRows != 1000000 || current.maxParts != 20000 {
		t.Fatal("graph version budget contract changed", legacy, current)
	}
	if parts, err := EncodePendingProjection(ctx, repo, view); err != nil || len(parts) == 0 {
		t.Fatal("pending size-test baseline must encode before enlargement", err)
	}
	view.Snapshots[0].Message = strings.Repeat("x", MaxPendingProjectionOutputBytes)
	if err := pendingProjectionView(ctx, view); err != nil {
		t.Fatal("enlarging metadata changed pending view semantics", err)
	}
	if _, err := pendingProjectionRowKey(repo, view.Snapshots[0]); err != nil {
		t.Fatal("enlarged snapshot must retain a valid pending row identity", err)
	}
	if parts, err := EncodePendingProjection(ctx, repo, view); !errors.Is(err, ErrRepositoryViewSourceLimit) || parts != nil {
		t.Fatalf("pending size guard returned %v and %d parts; want resource limit and no parts", err, len(parts))
	}
	view = PendingStateView{Projection: "on-hold-v1", Refs: make([]Ref, MaxPendingProjectionOutputRows)}
	if parts, err := EncodePendingProjection(ctx, repo, view); !errors.Is(err, ErrRepositoryViewSourceLimit) || parts != nil {
		t.Fatal("pending encoder accepted excess rows", err)
	}
	for _, parts := range [][]PendingProjectionPart{
		{{Kind: "header", Key: "view", Value: []byte(strings.Repeat("!", MaxPendingProjectionOutputBytes))}},
		make([]PendingProjectionPart, MaxPendingProjectionOutputRows+1),
	} {
		if _, err := DecodePendingProjection(ctx, repo, parts); !errors.Is(err, ErrRepositoryViewSourceLimit) {
			t.Fatal("pending decoder budget changed", err)
		}
	}
}
