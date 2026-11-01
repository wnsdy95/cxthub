//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGGraphProjectionRejectsWrongScopeAndInvalidParts(t *testing.T) {
	storage, ctx, repo := graphProjectionPG(t)
	job := graphProjectionClaimPG(t, ctx, storage, repo)
	page := repositoryViewSourcePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1})
	before := graphProjectionSnapshotPG(t, ctx, storage, repo)
	if err := storage.SaveGraphProjectionPage(ctx, job, page); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("graph accepted a complete pending-scope page: %v", err)
	}
	if after := graphProjectionSnapshotPG(t, ctx, storage, repo); !reflect.DeepEqual(after, before) {
		t.Fatal("wrong source scope changed graph progress")
	}
	job = graphProjectionCapturePG(t, ctx, storage, job, 0)
	graph, valid := graphProjectionPartsPG(t, ctx, storage, job)
	legacy, err := domain.EncodeGraphProjection(ctx, repo, graph)
	if err != nil {
		t.Fatal(err)
	}
	foreign := append([]domain.GraphProjectionPart{}, valid...)
	for index, part := range foreign {
		if part.Kind == "header" {
			var header map[string]json.RawMessage
			if err := json.Unmarshal(part.Value, &header); err != nil {
				t.Fatal(err)
			}
			header["repo_id"], err = json.Marshal(domain.HashContent([]byte("foreign graph projection")))
			if err != nil {
				t.Fatal(err)
			}
			foreign[index].Value, err = json.Marshal(header)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, invalid := range []struct {
		name  string
		parts func() []domain.GraphProjectionPart
	}{
		{"missing_all", func() []domain.GraphProjectionPart { return nil }},
		{"foreign_repo", func() []domain.GraphProjectionPart { return foreign }},
		{"graph_only", func() []domain.GraphProjectionPart { return legacy }},
		{"missing_field", func() []domain.GraphProjectionPart { return valid[1:] }},
		{"duplicate", func() []domain.GraphProjectionPart {
			return append(append([]domain.GraphProjectionPart{}, valid...), valid[0])
		}},
		{"extra", func() []domain.GraphProjectionPart {
			return append(append([]domain.GraphProjectionPart{}, valid...), domain.GraphProjectionPart{Kind: "field", Key: "unlisted", Value: json.RawMessage(`{"value":[]}`)})
		}},
		{"corrupt_value", func() []domain.GraphProjectionPart {
			parts := append([]domain.GraphProjectionPart{}, valid...)
			parts[0].Value = json.RawMessage(`{"value":17}`)
			return parts
		}},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			before := graphProjectionSnapshotPG(t, ctx, storage, repo)
			if err := storage.PublishGraphProjection(ctx, job, invalid.parts()); err == nil {
				t.Fatal("invalid graph publication accepted")
			}
			if after := graphProjectionSnapshotPG(t, ctx, storage, repo); !reflect.DeepEqual(after, before) {
				t.Fatal("invalid publication changed graph rows or consumed lease")
			}
		})
	}
	if err := storage.PublishGraphProjection(ctx, job, valid); err != nil {
		t.Fatalf("rejected graph parts consumed project lease: %v", err)
	}
	graphProjectionReadPG(t, ctx, storage, job, graph)
}

func TestPGGraphProjectionRuleMismatchRebuildsUnchangedSource(t *testing.T) {
	storage, ctx, repo := graphProjectionPG(t)
	job := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
	graph := graphProjectionPublishPG(t, ctx, storage, job)
	source := catalogReadStatePG(t, ctx, storage, repo)
	catalogExecPG(t, ctx, storage.pool, `UPDATE graph_projection_ready SET rules=$2 WHERE repo_id=$1`, string(repo), domain.GraphProjectionRules)
	before := graphProjectionSnapshotPG(t, ctx, storage, repo)
	for _, readMethod := range []func(context.Context, domain.ContentHash) (domain.GraphProjectionRead, error){storage.ReadGraphProjection, storage.ReadGraphProjectionStatus} {
		read, err := readMethod(ctx, repo)
		if err != nil || !read.Enabled || read.Ready || read.Complete || read.Rules != domain.GraphProjectionRules || len(read.Parts) != 0 || read.Source.Sequence != job.InputSequence {
			t.Fatalf("wrong graph rules became current: %+v err=%v", read, err)
		}
	}
	fresh := graphProjectionClaimPG(t, ctx, storage, repo)
	if fresh.Dataset == job.Dataset || fresh.Epoch != job.Epoch || fresh.AppliedSequence != -1 || fresh.InputSequence != -1 || fresh.Phase != "capture" || fresh.Cursor != "" || fresh.SourceRequest().After != nil {
		t.Fatalf("unchanged source did not rebuild mismatched graph rules: before=%+v after=%+v", job, fresh)
	}
	retained := graphProjectionSnapshotPG(t, ctx, storage, repo)
	if !reflect.DeepEqual(retained.ready, before.ready) || !reflect.DeepEqual(retained.parts, before.parts) {
		t.Fatal("rules reset destroyed previous ready graph before replacement")
	}
	if read, err := storage.ReadGraphProjection(ctx, repo); err != nil || read.Ready || read.Complete || len(read.Parts) != 0 {
		t.Fatalf("rule-mismatched graph leaked during recapture: %+v err=%v", read, err)
	}
	fresh = graphProjectionCapturePG(t, ctx, storage, fresh, 1)
	if got := graphProjectionPublishPG(t, ctx, storage, fresh); !reflect.DeepEqual(got, graph) {
		t.Fatal("rules-only recapture changed unchanged graph inputs")
	}
	var rules int
	if err := storage.pool.QueryRow(ctx, `SELECT rules FROM graph_projection_ready WHERE repo_id=$1`, string(repo)).Scan(&rules); err != nil || rules != domain.GraphViewProjectionRules {
		t.Fatalf("republished graph retained mismatched rules: rules=%d err=%v", rules, err)
	}
	if after := catalogReadStatePG(t, ctx, storage, repo); after != source {
		t.Fatal("graph rules rebuild mutated source generation")
	}
}

func TestPGGraphProjectionDisableReenableFencesBothPhases(t *testing.T) {
	for _, phase := range []string{"capture", "project"} {
		t.Run(phase, func(t *testing.T) {
			storage, ctx, repo := graphProjectionPG(t)
			job := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
			graphProjectionPublishPG(t, ctx, storage, job)
			if err := storage.ConfigurePendingProjection(ctx, repo, true); err != nil {
				t.Fatal(err)
			}
			pendingBefore := pendingProjectionEmptyDeltaSnapshotPG(t, ctx, storage, repo)
			catalogExecPG(t, ctx, storage.pool, `UPDATE pending_contexts SET data=jsonb_set(data,'{dismissed}','true') WHERE repo_id=$1`, string(repo))
			job = graphProjectionClaimPG(t, ctx, storage, repo)
			page := graphProjectionPagePG(t, ctx, storage, job, 0)
			var parts []domain.GraphProjectionPart
			if phase == "project" {
				job = graphProjectionCapturePG(t, ctx, storage, job, 0)
				_, parts = graphProjectionPartsPG(t, ctx, storage, job)
			}
			if err := storage.ConfigureGraphProjection(ctx, repo, false); err != nil {
				t.Fatal(err)
			}
			before := graphProjectionSnapshotPG(t, ctx, storage, repo)
			read, err := storage.ReadGraphProjection(ctx, repo)
			if err != nil || read.Enabled || read.Ready || read.Complete || len(read.Parts) != 0 {
				t.Fatalf("disabled graph remained readable: %+v err=%v", read, err)
			}
			if _, err := storage.ClaimGraphProjection(ctx, time.Minute); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("disabled graph remained claimable: %v", err)
			}
			if err := storage.RenewGraphProjection(ctx, job, time.Minute); !errors.Is(err, domain.ErrRefConflict) {
				t.Fatalf("disabled graph lease renewed: %v", err)
			}
			if phase == "capture" {
				err = storage.SaveGraphProjectionPage(ctx, job, page)
			} else {
				err = storage.PublishGraphProjection(ctx, job, parts)
			}
			if !errors.Is(err, domain.ErrRefConflict) {
				t.Fatalf("disabled graph %s worker wrote: %v", phase, err)
			}
			if after := graphProjectionSnapshotPG(t, ctx, storage, repo); !reflect.DeepEqual(after, before) {
				t.Fatal("disabled graph worker changed durable state")
			}
			if err := storage.ConfigureGraphProjection(ctx, repo, true); err != nil {
				t.Fatal(err)
			}
			fresh := graphProjectionClaimPG(t, ctx, storage, repo)
			if fresh.Dataset == job.Dataset || fresh.AppliedSequence != -1 || fresh.InputSequence != -1 || fresh.Phase != "capture" || fresh.Fence <= job.Fence {
				t.Fatalf("re-enable did not fence and restart graph: before=%+v after=%+v", job, fresh)
			}
			retained := graphProjectionSnapshotPG(t, ctx, storage, repo)
			if !reflect.DeepEqual(retained.ready, before.ready) || !reflect.DeepEqual(retained.parts, before.parts) {
				t.Fatal("re-enable destroyed last graph publication")
			}
			fresh = graphProjectionCapturePG(t, ctx, storage, fresh, 1)
			graphProjectionPublishPG(t, ctx, storage, fresh)
			if after := pendingProjectionEmptyDeltaSnapshotPG(t, ctx, storage, repo); !reflect.DeepEqual(after, pendingBefore) {
				t.Fatal("graph disable/re-enable modified pending queue")
			}
		})
	}
}

func TestPGGraphProjectionNeedsAttentionRequiresExplicitReenable(t *testing.T) {
	storage, ctx, repo := graphProjectionPG(t)
	job := graphProjectionClaimPG(t, ctx, storage, repo)
	if err := storage.FailGraphProjection(ctx, job, "invalid_source", false); err != nil {
		t.Fatal(err)
	}
	catalogExecPG(t, ctx, storage.pool, `UPDATE graph_projection_work SET retention_until=clock_timestamp()-interval '1 second',next_attempt=clock_timestamp() WHERE repo_id=$1`, string(repo))
	status, err := storage.ReadGraphProjectionStatus(ctx, repo)
	if err != nil || !status.Enabled || !status.NeedsAttention || status.Ready || status.Complete || len(status.Parts) != 0 {
		t.Fatalf("failed graph did not report durable attention: %+v err=%v", status, err)
	}
	if _, err := storage.ClaimGraphProjection(ctx, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired retention silently retried terminal graph failure: %v", err)
	}
	if err := storage.ConfigureGraphProjection(ctx, repo, true); err != nil {
		t.Fatal(err)
	}
	fresh := graphProjectionClaimPG(t, ctx, storage, repo)
	if fresh.Dataset == job.Dataset || fresh.AppliedSequence != -1 || fresh.Phase != "capture" {
		t.Fatalf("explicit recovery reused failed graph capture: %+v", fresh)
	}
	fresh = graphProjectionCapturePG(t, ctx, storage, fresh, 1)
	graphProjectionPublishPG(t, ctx, storage, fresh)
}
