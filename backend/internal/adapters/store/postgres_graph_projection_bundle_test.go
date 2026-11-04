//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestGraphProjectionSpecRequiresCompatibleMetadataBundle(t *testing.T) {
	ctx := context.Background()
	repo := domain.HashContent([]byte(t.Name()))
	graph := domain.GraphState{Version: 1}
	view := domain.RepositoryView{Graph: &graph, DefaultBranch: "main"}
	parts, err := domain.EncodeGraphViewProjection(ctx, repo, view)
	if err != nil {
		t.Fatal(err)
	}
	projection := (*PostgresStore)(nil).graphProjection()
	if projection.spec.rules != domain.GraphViewProjectionRules {
		t.Fatalf("graph store selected incompatible rules: %d", projection.spec.rules)
	}
	if err := projection.validateParts(ctx, repo, parts); err != nil {
		t.Fatalf("graph store rejected a complete metadata bundle: %v", err)
	}
	legacy, err := domain.EncodeGraphProjection(ctx, repo, graph)
	if err != nil {
		t.Fatal(err)
	}
	if err := projection.validateParts(ctx, repo, legacy); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("rules 2 accepted graph-only output: %v", err)
	}
	for _, part := range legacy {
		if part.Kind == "header" {
			if _, _, err := projection.spec.partKeys(part); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatalf("rules 2 accepted a graph-only read index: %v", err)
			}
		}
	}
	view.DefaultBranch = "other"
	changed, err := domain.EncodeGraphViewProjection(ctx, repo, view)
	if err != nil {
		t.Fatal(err)
	}
	mixed := append([]domain.GraphProjectionPart{}, parts...)
	replaced := false
	for index, part := range mixed {
		for _, next := range changed {
			if part.Kind != "header" && next.Kind == part.Kind && next.Key == part.Key && !reflect.DeepEqual(next.Value, part.Value) {
				mixed[index], replaced = next, true
			}
		}
	}
	if !replaced {
		t.Fatal("metadata mutation did not affect a bundle part")
	}
	if err := projection.validateParts(ctx, repo, mixed); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("rules 2 accepted mismatched metadata: %v", err)
	}
	if err := projection.validateParts(ctx, domain.HashContent([]byte("foreign repository")), parts); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("rules 2 accepted a foreign bundle: %v", err)
	}
}

func TestPGGraphProjectionBundleRemainsCoherentAcrossCaptureAndPublication(t *testing.T) {
	storage, ctx, repo := graphProjectionPG(t)
	initial := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
	initialGraph := graphProjectionPublishPG(t, ctx, storage, initial)
	before := graphProjectionReadPG(t, ctx, storage, initial, initialGraph)
	initialView, err := domain.DecodeGraphViewProjection(ctx, repo, before.Parts)
	if err != nil || len(initialView.Snapshots) != 1 || len(initialView.Pending) != 1 || initialView.Pending[0].Dismissed {
		t.Fatalf("bundle fixture lacks initial metadata: %+v err=%v", initialView, err)
	}
	err = storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		pinned := graphProjectionReadPG(t, bound, storage, initial, initialGraph)
		catalogExecPG(t, ctx, storage.pool, `UPDATE snapshots SET message='captured bundle metadata' WHERE repo_id=$1`, string(repo))
		catalogExecPG(t, ctx, storage.pool, `UPDATE pending_contexts SET data=jsonb_set(data,'{dismissed}','true') WHERE repo_id=$1`, string(repo))
		graphProjectionPriorReadPG(t, ctx, storage, initial, before.Parts)
		job := graphProjectionClaimPG(t, ctx, storage, repo)
		partial := false
		for pages := 0; job.Phase == "capture" && pages < 128; pages++ {
			page := graphProjectionPagePG(t, ctx, storage, job, 1)
			partial = partial || page.NextCursor != ""
			if err := storage.SaveGraphProjectionPage(ctx, job, page); err != nil {
				return err
			}
			graphProjectionPriorReadPG(t, ctx, storage, initial, before.Parts)
			job = graphProjectionClaimPG(t, ctx, storage, repo)
		}
		if !partial || job.Phase != "project" || job.Dataset != initial.Dataset {
			t.Fatalf("fixture did not mutate the existing dataset incrementally: %+v partial=%v", job, partial)
		}
		graph, parts := graphProjectionPartsPG(t, ctx, storage, job)
		captured, err := domain.DecodeGraphViewProjection(ctx, repo, parts)
		if err != nil || len(captured.Snapshots) != 1 || captured.Snapshots[0].Message != "captured bundle metadata" ||
			len(captured.Pending) != 1 || !captured.Pending[0].Dismissed || len(graph.HoldCounts) != 0 {
			t.Fatalf("new bundle lost captured metadata or graph: %+v err=%v", captured, err)
		}
		catalogExecPG(t, ctx, storage.pool, `UPDATE snapshots SET message='newer than captured bundle' WHERE repo_id=$1`, string(repo))
		if err := storage.PublishGraphProjection(ctx, job, parts); err != nil {
			return err
		}
		graphProjectionPriorReadPG(t, ctx, storage, job, parts)
		stillPinned := graphProjectionReadPG(t, bound, storage, initial, initialGraph)
		if !reflect.DeepEqual(stillPinned, pinned) || !reflect.DeepEqual(stillPinned, before) {
			t.Fatal("publication replaced metadata or graph inside the pinned ready snapshot")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	current := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 1)
	graph := graphProjectionPublishPG(t, ctx, storage, current)
	read := graphProjectionReadPG(t, ctx, storage, current, graph)
	view, err := domain.DecodeGraphViewProjection(ctx, repo, read.Parts)
	if err != nil || view.Snapshots[0].Message != "newer than captured bundle" || len(view.Graph.HoldCounts) != 0 {
		t.Fatalf("follow-up bundle failed to catch up atomically: %+v err=%v", view, err)
	}
}

func TestPGGraphProjectionRejectsMismatchedBundleMetadata(t *testing.T) {
	storage, ctx, repo := graphProjectionPG(t)
	job := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
	graph, parts := graphProjectionPartsPG(t, ctx, storage, job)
	view, err := domain.DecodeGraphViewProjection(ctx, repo, parts)
	if err != nil || len(view.Snapshots) == 0 {
		t.Fatalf("fixture has no metadata: %+v err=%v", view, err)
	}
	view.Snapshots[0].Message = "mismatched generation metadata"
	changed, err := domain.EncodeGraphViewProjection(ctx, repo, view)
	if err != nil {
		t.Fatal(err)
	}
	mixed := append([]domain.GraphProjectionPart{}, parts...)
	replaced := false
	for index, part := range mixed {
		if part.Kind == "header" {
			continue
		}
		for _, next := range changed {
			if next.Kind == part.Kind && next.Key == part.Key && !reflect.DeepEqual(next.Value, part.Value) {
				mixed[index], replaced = next, true
			}
		}
	}
	if !replaced {
		t.Fatal("metadata mutation did not change a bundle body part")
	}
	before := graphProjectionSnapshotPG(t, ctx, storage, repo)
	if err := storage.PublishGraphProjection(ctx, job, mixed); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("publication accepted mismatched graph/metadata generation: %v", err)
	}
	if after := graphProjectionSnapshotPG(t, ctx, storage, repo); !reflect.DeepEqual(after, before) {
		t.Fatal("mismatched bundle consumed the lease or changed durable state")
	}
	if err := storage.PublishGraphProjection(ctx, job, parts); err != nil {
		t.Fatal(err)
	}
	graphProjectionReadPG(t, ctx, storage, job, graph)
}

func TestPGGraphProjectionBundleCorruptionIsValidatedForCurrentAndPriorReads(t *testing.T) {
	for _, lagged := range []bool{false, true} {
		for _, corruption := range []string{"graph_only", "metadata", "missing", "oversized"} {
			t.Run(corruption+map[bool]string{false: "_current", true: "_prior"}[lagged], func(t *testing.T) {
				storage, ctx, repo := graphProjectionPG(t)
				job := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
				graph, parts := graphProjectionPartsPG(t, ctx, storage, job)
				if err := storage.PublishGraphProjection(ctx, job, parts); err != nil {
					t.Fatal(err)
				}
				if lagged {
					catalogExecPG(t, ctx, storage.pool, `UPDATE snapshots SET message='advance source only' WHERE repo_id=$1`, string(repo))
				}
				before := graphProjectionStatusObservedPG(t, ctx, storage, repo)
				want := domain.ErrIntegrity
				switch corruption {
				case "graph_only":
					legacy, err := domain.EncodeGraphProjection(ctx, repo, graph)
					if err != nil {
						t.Fatal(err)
					}
					raw, err := json.Marshal(legacy)
					if err != nil {
						t.Fatal(err)
					}
					catalogExecPG(t, ctx, storage.pool, `DELETE FROM graph_projection_parts WHERE repo_id=$1`, string(repo))
					catalogExecPG(t, ctx, storage.pool, `INSERT INTO graph_projection_parts(repo_id,dataset,source_seq,kind,entity_key,deleted,image)
						SELECT $1,$2,$3,part.kind,part.key,false,part.value FROM jsonb_to_recordset($4::jsonb) AS part(kind text,key text,value jsonb)`,
						string(repo), job.Dataset, job.InputSequence, raw)
				case "metadata":
					view, err := domain.DecodeGraphViewProjection(ctx, repo, parts)
					if err != nil {
						t.Fatal(err)
					}
					view.Snapshots[0].Message = "tampered metadata"
					changed, err := domain.EncodeGraphViewProjection(ctx, repo, view)
					if err != nil {
						t.Fatal(err)
					}
					replaced := 0
					for _, next := range changed {
						if next.Kind == "header" {
							continue
						}
						for _, part := range parts {
							if next.Kind == part.Kind && next.Key == part.Key && !reflect.DeepEqual(next.Value, part.Value) {
								catalogExecPG(t, ctx, storage.pool, `UPDATE graph_projection_parts SET image=$5 WHERE repo_id=$1 AND dataset=$2 AND kind=$3 AND entity_key=$4`,
									string(repo), job.Dataset, next.Kind, next.Key, next.Value)
								replaced++
							}
						}
					}
					if replaced == 0 {
						t.Fatal("fixture failed to corrupt stored metadata")
					}
				case "missing":
					catalogExecPG(t, ctx, storage.pool, `DELETE FROM graph_projection_parts WHERE repo_id=$1`, string(repo))
				case "oversized":
					catalogExecPG(t, ctx, storage.pool, `UPDATE graph_projection_parts SET image=jsonb_build_object('oversized',repeat('x',$2::int)) WHERE repo_id=$1 AND kind='header'`, string(repo), domain.MaxPendingProjectionOutputBytes)
					want = domain.ErrRepositoryViewSourceLimit
				}
				status := graphProjectionStatusObservedPG(t, ctx, storage, repo)
				if !reflect.DeepEqual(status, before) || !status.Complete || status.Ready == lagged || status.Rules != domain.GraphViewProjectionRules {
					t.Fatalf("status read validated payloads instead of publication metadata: before=%+v after=%+v", before, status)
				}
				read, err := storage.ReadGraphProjection(ctx, repo)
				var failure *domain.GraphProjectionCorruption
				if !errors.Is(err, want) || !errors.As(err, &failure) || failure.Validate() != nil ||
					failure.RepoID != repo || failure.Dataset != before.Dataset || failure.Applied != before.Applied || failure.Rules != before.Rules ||
					!reflect.DeepEqual(read, before) {
					t.Fatalf("full bundle read skipped corruption: read=%+v err=%v want=%v", read, err, want)
				}
			})
		}
	}
}
