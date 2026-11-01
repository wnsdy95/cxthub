//go:build postgres

package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func graphProjectionPG(t *testing.T) (*PostgresStore, context.Context, domain.ContentHash) {
	t.Helper()
	storage, ctx, repo := catalogPG(t)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, table := range []string{"graph_projection_work", "pending_projection_work"} {
			if _, err := storage.pool.Exec(cleanup, `DELETE FROM `+table+` WHERE repo_id=$1`, string(repo)); err != nil {
				t.Error(err)
			}
		}
	})
	snapshot := initializationPGSnapshot(t, storage, repo, "graph projection capture")
	stamp := time.Now().UTC().Truncate(time.Microsecond)
	pending := domain.Pending{RepoID: repo, SessionID: "graph-session", Branch: "main", Provider: domain.ProviderClaude,
		Target: snapshot.ID, Author: domain.TeamIdentity{Name: "Graph", Email: "graph@example.test"}, UpdatedAt: stamp, ActivityAt: &stamp}
	if err := storage.PutPending(ctx, repo, pending); err != nil {
		t.Fatal(err)
	}
	if err := storage.ConfigureGraphProjection(ctx, repo, true); err != nil {
		t.Fatal(err)
	}
	return storage, ctx, repo
}

func graphProjectionClaimPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) domain.GraphProjectionLease {
	t.Helper()
	job, err := storage.ClaimGraphProjection(ctx, time.Minute)
	if err != nil || job.RepoID != repo || job.Dataset == "" || job.Epoch == "" || job.Fence <= 0 || job.LeaseUntil.IsZero() {
		t.Fatalf("graph claim: job=%+v err=%v", job, err)
	}
	return job
}

func graphProjectionPagePG(t *testing.T, ctx context.Context, storage *PostgresStore, job domain.GraphProjectionLease, limit int) domain.RepositoryViewSourcePage {
	t.Helper()
	request := job.SourceRequest()
	request.Limit = limit
	if request.After != nil && request.After.Scope != domain.RepositoryGraphSourceScope {
		t.Fatalf("graph lease requested a foreign source: %+v", request)
	}
	page, err := storage.RepositoryGraphSourceChanges(ctx, job.RepoID, request)
	if err != nil || page.Scope != domain.RepositoryGraphSourceScope || page.RepoID != job.RepoID {
		t.Fatalf("graph source page: %+v err=%v", page, err)
	}
	return page
}

func graphProjectionCapturePG(t *testing.T, ctx context.Context, storage *PostgresStore, job domain.GraphProjectionLease, limit int) domain.GraphProjectionLease {
	t.Helper()
	for pages := 0; pages < 128; pages++ {
		if job.Phase == "project" {
			return job
		}
		if job.Phase != "capture" {
			t.Fatalf("unexpected graph capture phase: %+v", job)
		}
		page := graphProjectionPagePG(t, ctx, storage, job, limit)
		if err := storage.SaveGraphProjectionPage(ctx, job, page); err != nil {
			t.Fatal(err)
		}
		next := graphProjectionClaimPG(t, ctx, storage, job.RepoID)
		if next.Dataset != job.Dataset || next.Epoch != job.Epoch || next.Fence <= job.Fence || next.Cursor != page.NextCursor {
			t.Fatalf("graph capture progress was not durable: before=%+v after=%+v", job, next)
		}
		job = next
	}
	t.Fatal("graph capture exceeded fixture page bound")
	return domain.GraphProjectionLease{}
}

func graphProjectionPartsPG(t *testing.T, ctx context.Context, storage *PostgresStore, job domain.GraphProjectionLease) (domain.GraphState, []domain.GraphProjectionPart) {
	t.Helper()
	if job.Phase != "project" || job.Cursor != "" || job.InputSequence < 0 {
		t.Fatalf("refusing to project partial graph inputs: %+v", job)
	}
	inputs, err := storage.LoadGraphProjectionInputs(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	var metadataEntries, evidenceEntries []domain.RepositoryViewSourceEntry
	for _, entry := range inputs {
		if domain.RepositoryViewSourceKindValid(entry.Kind) {
			metadataEntries = append(metadataEntries, entry)
		} else {
			evidenceEntries = append(evidenceEntries, entry)
		}
	}
	metadata, view, err := domain.DecodeRepositoryViewSource(ctx, job.RepoID, metadataEntries)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := domain.RepositoryViewSourceCheckpoint{Version: domain.RepositoryViewSourceVersion,
		Scope: domain.RepositoryGraphSourceScope, RepoID: job.RepoID, Epoch: job.Epoch, Sequence: job.InputSequence}
	evidence, err := NewGraphSourceReader(ctx, checkpoint, metadata, evidenceEntries)
	if err != nil {
		t.Fatal(err)
	}
	view, err = app.ProjectCapturedRepositoryView(ctx, checkpoint, view, evidence)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := domain.EncodeGraphViewProjection(ctx, job.RepoID, view)
	if err != nil {
		t.Fatal(err)
	}
	return *view.Graph, parts
}

func graphProjectionReadPG(t *testing.T, ctx context.Context, storage *PostgresStore, job domain.GraphProjectionLease, want domain.GraphState) domain.GraphProjectionRead {
	t.Helper()
	read, err := storage.ReadGraphProjection(ctx, job.RepoID)
	if err != nil || !read.Enabled || !read.Ready || !read.Complete || read.Rules != domain.GraphViewProjectionRules || read.Dataset != job.Dataset || read.Source != read.Applied ||
		read.Source.Scope != domain.RepositoryGraphSourceScope || read.Applied.RepoID != job.RepoID || read.Applied.Epoch != job.Epoch ||
		read.Applied.Sequence != job.InputSequence || read.Applied.Validate() != nil {
		t.Fatalf("graph ready checkpoint: read=%+v job=%+v err=%v", read, job, err)
	}
	got, err := domain.DecodeGraphViewProjection(ctx, job.RepoID, read.Parts)
	if err != nil || !reflect.DeepEqual(got.Graph, &want) {
		t.Fatalf("decoded graph differs: got=%+v want=%+v err=%v", got, want, err)
	}
	status, err := storage.ReadGraphProjectionStatus(ctx, job.RepoID)
	metadata := read
	metadata.Parts = nil
	if err != nil || !reflect.DeepEqual(status, metadata) {
		t.Fatalf("graph status disagrees with full read: got=%+v want=%+v err=%v", status, metadata, err)
	}
	return read
}

func graphProjectionPriorReadPG(t *testing.T, ctx context.Context, storage *PostgresStore, job domain.GraphProjectionLease, want []domain.GraphProjectionPart) domain.GraphProjectionRead {
	t.Helper()
	read, err := storage.ReadGraphProjection(ctx, job.RepoID)
	if err != nil || !read.Enabled || read.NeedsAttention || read.Ready || !read.Complete || read.Rules != domain.GraphViewProjectionRules ||
		read.Dataset != job.Dataset || read.Applied.Epoch != job.Epoch || read.Applied.Sequence != job.InputSequence ||
		read.Source.Epoch != read.Applied.Epoch || read.Source.Sequence <= read.Applied.Sequence || read.Applied.Validate() != nil {
		t.Fatalf("prior bundle checkpoint: read=%+v job=%+v err=%v", read, job, err)
	}
	expected, err := domain.DecodeGraphViewProjection(ctx, job.RepoID, want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := domain.DecodeGraphViewProjection(ctx, job.RepoID, read.Parts)
	if err != nil || !reflect.DeepEqual(got, expected) {
		t.Fatalf("prior bundle mixed graph and metadata: got=%+v want=%+v err=%v", got, expected, err)
	}
	status, err := storage.ReadGraphProjectionStatus(ctx, job.RepoID)
	metadata := read
	metadata.Parts = nil
	if err != nil || !reflect.DeepEqual(status, metadata) {
		t.Fatalf("prior status disagrees with full read: got=%+v want=%+v err=%v", status, metadata, err)
	}
	return read
}

type graphProjectionSnapshot struct {
	work, ready, inputs, parts []byte
}

func graphProjectionSnapshotPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) graphProjectionSnapshot {
	t.Helper()
	var snapshot graphProjectionSnapshot
	if err := storage.pool.QueryRow(ctx, `SELECT
		(SELECT to_jsonb(work) FROM graph_projection_work work WHERE repo_id=$1),
		(SELECT to_jsonb(ready) FROM graph_projection_ready ready WHERE repo_id=$1),
		(SELECT COALESCE(jsonb_agg(to_jsonb(input) || jsonb_build_object('xmin',input.xmin::text)
			ORDER BY input.dataset,input.source_seq,input.kind,input.entity_key),'[]'::jsonb)
			FROM graph_projection_inputs input WHERE repo_id=$1),
		(SELECT COALESCE(jsonb_agg(to_jsonb(part) || jsonb_build_object('xmin',part.xmin::text)
			ORDER BY part.dataset,part.source_seq,part.kind,part.entity_key),'[]'::jsonb)
			FROM graph_projection_parts part WHERE repo_id=$1)`, string(repo)).Scan(&snapshot.work, &snapshot.ready, &snapshot.inputs, &snapshot.parts); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func graphProjectionPublishPG(t *testing.T, ctx context.Context, storage *PostgresStore, job domain.GraphProjectionLease) domain.GraphState {
	t.Helper()
	graph, parts := graphProjectionPartsPG(t, ctx, storage, job)
	if err := storage.PublishGraphProjection(ctx, job, parts); err != nil {
		t.Fatal(err)
	}
	graphProjectionReadPG(t, ctx, storage, job, graph)
	return graph
}

func TestPGGraphProjectionCaptureProjectReady(t *testing.T) {
	storage, ctx, repo := graphProjectionPG(t)
	read, err := storage.ReadGraphProjection(ctx, repo)
	if err != nil || !read.Enabled || read.Ready || read.Complete || read.Rules != 0 || len(read.Parts) != 0 || read.Source.Scope != domain.RepositoryGraphSourceScope {
		t.Fatalf("uncaptured graph became readable: %+v err=%v", read, err)
	}
	if status, err := read.Status(); err != nil || status.State != "catching_up" || read.Applied != (domain.RepositoryViewSourceCheckpoint{}) {
		t.Fatalf("uncaptured graph exposed an invalid applied checkpoint: %+v err=%v", read, err)
	}
	job := graphProjectionClaimPG(t, ctx, storage, repo)
	if job.Phase != "capture" || job.AppliedSequence != -1 || job.InputSequence != -1 || job.Cursor != "" || job.SourceRequest().After != nil {
		t.Fatalf("initial graph claim is not a baseline: %+v", job)
	}
	page := graphProjectionPagePG(t, ctx, storage, job, 0)
	if page.Checkpoint == nil || page.NextCursor != "" || *page.Checkpoint != read.Source {
		t.Fatalf("fixture must fit one complete graph baseline: %+v", page)
	}
	if _, err := storage.LoadGraphProjectionInputs(ctx, job); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("capture lease exposed incomplete inputs: %v", err)
	}
	if err := storage.SaveGraphProjectionPage(ctx, job, page); err != nil {
		t.Fatal(err)
	}
	if read, err := storage.ReadGraphProjection(ctx, repo); err != nil || read.Ready || read.Complete || len(read.Parts) != 0 {
		t.Fatalf("captured inputs became ready before projection: %+v err=%v", read, err)
	}
	job = graphProjectionClaimPG(t, ctx, storage, repo)
	if job.Phase != "project" || job.InputSequence != page.Through || job.AppliedSequence != -1 || job.Cursor != "" {
		t.Fatalf("final page did not schedule graph projection: %+v", job)
	}
	inputs, err := storage.LoadGraphProjectionInputs(ctx, job)
	if err != nil || !reflect.DeepEqual(inputs, page.Entries) {
		t.Fatalf("durable baseline differs from complete source: got=%+v want=%+v err=%v", inputs, page.Entries, err)
	}
	graph := graphProjectionPublishPG(t, ctx, storage, job)
	if !reflect.DeepEqual(graph.OrphanSessions, []string{"graph-session"}) || graph.HoldCounts["main"] != 1 {
		t.Fatalf("graph fixture lost captured pending state: %+v", graph)
	}
	before := graphProjectionSnapshotPG(t, ctx, storage, repo)
	if _, err := storage.ClaimGraphProjection(ctx, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unchanged graph was claimable: %v", err)
	}
	if err := storage.ConfigureGraphProjection(ctx, repo, true); err != nil {
		t.Fatal(err)
	}
	if after := graphProjectionSnapshotPG(t, ctx, storage, repo); !reflect.DeepEqual(after, before) {
		t.Fatal("unchanged enable/claim rewrote durable graph state")
	}
}

func TestPGGraphProjectionRestartResumesPartialCursor(t *testing.T) {
	storage, ctx, repo := graphProjectionPG(t)
	job := graphProjectionClaimPG(t, ctx, storage, repo)
	first := graphProjectionPagePG(t, ctx, storage, job, 1)
	if first.NextCursor == "" || first.Checkpoint != nil || len(first.Entries) != 1 {
		t.Fatalf("fixture did not produce partial graph progress: %+v", first)
	}
	rest, checkpoint := graphSourceDrainPG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Cursor: first.NextCursor, Limit: 1})
	want := append(first.Entries[:len(first.Entries):len(first.Entries)], rest...)
	if err := storage.SaveGraphProjectionPage(ctx, job, first); err != nil {
		t.Fatal(err)
	}
	peer, err := NewPostgresStore(ctx, storage.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(peer.Close)
	resumed := graphProjectionClaimPG(t, ctx, peer, repo)
	if resumed.Phase != "capture" || resumed.Cursor != first.NextCursor || resumed.Dataset != job.Dataset || resumed.Epoch != job.Epoch ||
		resumed.AppliedSequence != -1 || resumed.InputSequence != -1 || resumed.Fence <= job.Fence {
		t.Fatalf("new store lost graph cursor: before=%+v resumed=%+v", job, resumed)
	}
	catalogExecPG(t, ctx, storage.pool, `UPDATE pending_contexts SET data=jsonb_set(data,'{dismissed}','true') WHERE repo_id=$1`, string(repo))
	resumed = graphProjectionCapturePG(t, ctx, peer, resumed, 1)
	inputs, err := peer.LoadGraphProjectionInputs(ctx, resumed)
	if err != nil || resumed.InputSequence != checkpoint.Sequence || !reflect.DeepEqual(inputs, want) {
		t.Fatalf("resumed graph borrowed newer mutable inputs: sequence=%d want=%d err=%v", resumed.InputSequence, checkpoint.Sequence, err)
	}
	graph, parts := graphProjectionPartsPG(t, ctx, peer, resumed)
	if graph.HoldCounts["main"] != 1 {
		t.Fatal("fixed graph capture borrowed the newer dismissal")
	}
	if err := peer.PublishGraphProjection(ctx, resumed, parts); err != nil {
		t.Fatal(err)
	}
	graphProjectionPriorReadPG(t, ctx, storage, resumed, parts)
	current := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 1)
	graph = graphProjectionPublishPG(t, ctx, storage, current)
	if len(graph.OrphanSessions) != 0 || len(graph.HoldCounts) != 0 {
		t.Fatal("follow-up delta failed to apply dismissal")
	}
}

func TestPGGraphProjectionTwoWorkersExpiredAndStaleFences(t *testing.T) {
	for _, phase := range []string{"capture", "project"} {
		t.Run(phase, func(t *testing.T) {
			storage, ctx, repo := graphProjectionPG(t)
			job := graphProjectionClaimPG(t, ctx, storage, repo)
			page := graphProjectionPagePG(t, ctx, storage, job, 0)
			var parts []domain.GraphProjectionPart
			if phase == "project" {
				job = graphProjectionCapturePG(t, ctx, storage, job, 0)
				_, parts = graphProjectionPartsPG(t, ctx, storage, job)
			}
			peer, err := NewPostgresStore(ctx, storage.pool.Config().ConnString())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(peer.Close)
			if _, err := peer.ClaimGraphProjection(ctx, time.Minute); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("second worker stole a live graph lease: %v", err)
			}
			write := func(owner *PostgresStore, lease domain.GraphProjectionLease) error {
				if phase == "capture" {
					return owner.SaveGraphProjectionPage(ctx, lease, page)
				}
				return owner.PublishGraphProjection(ctx, lease, parts)
			}
			catalogExecPG(t, ctx, storage.pool, `UPDATE graph_projection_work SET lease_until=clock_timestamp()-interval '1 second' WHERE repo_id=$1`, string(repo))
			assertRejected := func() {
				t.Helper()
				before := graphProjectionSnapshotPG(t, ctx, storage, repo)
				for _, err := range []error{write(storage, job), storage.RenewGraphProjection(ctx, job, time.Minute), storage.FailGraphProjection(ctx, job, "temporary_failure", true)} {
					if !errors.Is(err, domain.ErrRefConflict) {
						t.Fatalf("stale %s graph worker changed work: %v", phase, err)
					}
				}
				if after := graphProjectionSnapshotPG(t, ctx, storage, repo); !reflect.DeepEqual(after, before) {
					t.Fatal("rejected graph worker changed durable state")
				}
			}
			assertRejected()
			replacement := graphProjectionClaimPG(t, ctx, peer, repo)
			if replacement.Fence <= job.Fence || replacement.Dataset != job.Dataset || replacement.Phase != phase {
				t.Fatalf("expired graph lease was not fenced: old=%+v new=%+v", job, replacement)
			}
			assertRejected()
			if err := peer.RenewGraphProjection(ctx, replacement, 2*time.Minute); err != nil {
				t.Fatal(err)
			}
			if err := write(peer, replacement); err != nil {
				t.Fatalf("stale graph worker damaged replacement: %v", err)
			}
		})
	}
}
