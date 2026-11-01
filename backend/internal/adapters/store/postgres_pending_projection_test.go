//go:build postgres

package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func pendingProjectionPG(t *testing.T) (*PostgresStore, context.Context, domain.ContentHash, domain.Pending) {
	t.Helper()
	storage, ctx, repo := catalogPG(t)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := storage.pool.Exec(cleanup, `DELETE FROM pending_projection_work WHERE repo_id=$1`, string(repo)); err != nil {
			t.Error(err)
		}
	})
	snapshot := initializationPGSnapshot(t, storage, repo, "pending projection capture")
	stamp := time.Now().UTC().Truncate(time.Microsecond)
	pending := domain.Pending{RepoID: repo, SessionID: "projection-session", Branch: "main", Provider: domain.ProviderClaude,
		Target: snapshot.ID, Author: domain.TeamIdentity{Name: "Capture", Email: "capture@example.test"}, UpdatedAt: stamp, ActivityAt: &stamp}
	if err := storage.PutPending(ctx, repo, pending); err != nil {
		t.Fatal(err)
	}
	if err := storage.ConfigurePendingProjection(ctx, repo, true); err != nil {
		t.Fatal(err)
	}
	return storage, ctx, repo, pending
}

func pendingProjectionClaimPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) domain.PendingProjectionLease {
	t.Helper()
	job, err := storage.ClaimPendingProjection(ctx, time.Minute)
	if err != nil || job.RepoID != repo || job.Dataset == "" || job.Epoch == "" || job.Fence <= 0 || job.LeaseUntil.IsZero() {
		t.Fatalf("claim: job=%+v err=%v", job, err)
	}
	return job
}

func pendingProjectionCapturePG(t *testing.T, ctx context.Context, storage *PostgresStore, job domain.PendingProjectionLease, limit int) domain.PendingProjectionLease {
	t.Helper()
	for pages := 0; pages < 32; pages++ {
		if job.Phase == "project" {
			return job
		}
		if job.Phase != "capture" {
			t.Fatalf("unexpected capture phase: %+v", job)
		}
		request := job.SourceRequest()
		request.Limit = limit
		page := repositoryViewSourcePagePG(t, ctx, storage, job.RepoID, request)
		if err := storage.SavePendingProjectionPage(ctx, job, page); err != nil {
			t.Fatal(err)
		}
		next := pendingProjectionClaimPG(t, ctx, storage, job.RepoID)
		if next.Dataset != job.Dataset || next.Epoch != job.Epoch || next.Fence <= job.Fence || next.Cursor != page.NextCursor {
			t.Fatalf("capture progress was not durable: before=%+v after=%+v page=%+v", job, next, page)
		}
		job = next
	}
	t.Fatal("capture exceeded fixture page bound")
	return domain.PendingProjectionLease{}
}

func pendingProjectionPartsPG(t *testing.T, ctx context.Context, storage *PostgresStore, job domain.PendingProjectionLease) (domain.PendingStateView, []domain.PendingProjectionPart) {
	t.Helper()
	inputs, err := storage.LoadPendingProjectionInputs(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	view, err := domain.ProjectPendingSource(ctx, job.RepoID, inputs)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := domain.EncodePendingProjection(ctx, job.RepoID, view)
	if err != nil {
		t.Fatal(err)
	}
	return view, parts
}

func pendingProjectionReadPG(t *testing.T, ctx context.Context, storage *PostgresStore, job domain.PendingProjectionLease, want domain.PendingStateView) domain.PendingProjectionRead {
	t.Helper()
	read, err := storage.ReadPendingProjection(ctx, job.RepoID)
	if err != nil || !read.Enabled || !read.Ready || read.Dataset != job.Dataset || read.Source != read.Applied ||
		read.Applied.RepoID != job.RepoID || read.Applied.Epoch != job.Epoch || read.Applied.Sequence != job.InputSequence || read.Applied.Validate() != nil {
		t.Fatalf("ready checkpoint: read=%+v job=%+v err=%v", read, job, err)
	}
	got, err := domain.DecodePendingProjection(ctx, job.RepoID, read.Parts)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded projection differs: got=%+v want=%+v err=%v", got, want, err)
	}
	return read
}

func pendingProjectionCountsPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) [3]int64 {
	t.Helper()
	var counts [3]int64
	if err := storage.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pending_projection_inputs WHERE repo_id=$1),
		(SELECT count(*) FROM pending_projection_parts WHERE repo_id=$1),
		(SELECT count(*) FROM pending_projection_ready WHERE repo_id=$1)`, string(repo)).Scan(&counts[0], &counts[1], &counts[2]); err != nil {
		t.Fatal(err)
	}
	return counts
}

func TestPGPendingProjectionCaptureProjectReady(t *testing.T) {
	storage, ctx, repo, pending := pendingProjectionPG(t)
	read, err := storage.ReadPendingProjection(ctx, repo)
	if err != nil || !read.Enabled || read.Ready || len(read.Parts) != 0 {
		t.Fatalf("uncaptured projection is readable: %+v err=%v", read, err)
	}
	job := pendingProjectionClaimPG(t, ctx, storage, repo)
	if job.Phase != "capture" || job.AppliedSequence != -1 || job.InputSequence != -1 || job.Cursor != "" || job.SourceRequest().After != nil {
		t.Fatalf("initial claim is not a baseline: %+v", job)
	}
	page := repositoryViewSourcePagePG(t, ctx, storage, repo, job.SourceRequest())
	if page.Checkpoint == nil || page.NextCursor != "" || *page.Checkpoint != read.Source {
		t.Fatalf("fixture should fit one fixed baseline: %+v", page)
	}
	want, err := domain.ProjectPendingSource(ctx, repo, page.Entries)
	if err != nil || len(want.Pending) != 1 || want.Pending[0].SessionID != pending.SessionID || len(want.Snapshots) != 1 ||
		want.Snapshots[0].ID != pending.Target || !reflect.DeepEqual(want.OrphanSessions, []string{pending.SessionID}) || want.HoldCounts["main"] != 1 {
		t.Fatalf("fixture projection: %+v err=%v", want, err)
	}
	if err := storage.SavePendingProjectionPage(ctx, job, page); err != nil {
		t.Fatal(err)
	}
	if read, err := storage.ReadPendingProjection(ctx, repo); err != nil || read.Ready || len(read.Parts) != 0 {
		t.Fatalf("captured inputs became ready before publication: %+v err=%v", read, err)
	}
	job = pendingProjectionClaimPG(t, ctx, storage, repo)
	if job.Phase != "project" || job.InputSequence != page.Through || job.AppliedSequence != -1 || job.Cursor != "" {
		t.Fatalf("final capture did not advance to project: %+v", job)
	}
	view, parts := pendingProjectionPartsPG(t, ctx, storage, job)
	if !reflect.DeepEqual(view, want) {
		t.Fatalf("persisted inputs changed source projection: got=%+v want=%+v", view, want)
	}
	if err := storage.PublishPendingProjection(ctx, job, parts); err != nil {
		t.Fatal(err)
	}
	pendingProjectionReadPG(t, ctx, storage, job, want)
	if counts := pendingProjectionCountsPG(t, ctx, storage, repo); counts != [3]int64{int64(len(page.Entries)), int64(len(parts)), 1} {
		t.Fatalf("baseline row versions: %v", counts)
	}
	var phase, cursor string
	var applied, input int64
	if err := storage.pool.QueryRow(ctx, `SELECT phase,cursor,applied_seq,input_seq FROM pending_projection_work WHERE repo_id=$1`, string(repo)).Scan(&phase, &cursor, &applied, &input); err != nil {
		t.Fatal(err)
	}
	if phase != "idle" || cursor != "" || applied != page.Through || input != applied {
		t.Fatalf("publication did not commit progress: phase=%s cursor=%q applied=%d input=%d", phase, cursor, applied, input)
	}
	if _, err := storage.ClaimPendingProjection(ctx, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unchanged ready projection was claimed: %v", err)
	}
}

func TestPGPendingProjectionRestartResumesPartialCursor(t *testing.T) {
	storage, ctx, repo, _ := pendingProjectionPG(t)
	job := pendingProjectionClaimPG(t, ctx, storage, repo)
	request := job.SourceRequest()
	request.Limit = 1
	first := repositoryViewSourcePagePG(t, ctx, storage, repo, request)
	if first.NextCursor == "" || first.Checkpoint != nil || len(first.Entries) != 1 {
		t.Fatalf("fixture did not produce partial progress: %+v", first)
	}
	entries, checkpoint := repositoryViewSourceDrainPG(t, ctx, storage, repo, first, 1)
	want, err := domain.ProjectPendingSource(ctx, repo, entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.SavePendingProjectionPage(ctx, job, first); err != nil {
		t.Fatal(err)
	}
	peer, err := NewPostgresStore(ctx, storage.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(peer.Close)
	resumed := pendingProjectionClaimPG(t, ctx, peer, repo)
	if resumed.Phase != "capture" || resumed.Cursor != first.NextCursor || resumed.Dataset != job.Dataset || resumed.Epoch != job.Epoch ||
		resumed.AppliedSequence != -1 || resumed.InputSequence != -1 || resumed.Fence <= job.Fence {
		t.Fatalf("independent worker lost partial progress: before=%+v resumed=%+v", job, resumed)
	}
	if counts := pendingProjectionCountsPG(t, ctx, peer, repo); counts != [3]int64{1, 0, 0} {
		t.Fatalf("partial input rows were not durable: %v", counts)
	}
	resumed = pendingProjectionCapturePG(t, ctx, peer, resumed, 1)
	view, parts := pendingProjectionPartsPG(t, ctx, peer, resumed)
	if resumed.InputSequence != checkpoint.Sequence || !reflect.DeepEqual(view, want) {
		t.Fatalf("resumed capture changed fixed source: job=%+v got=%+v want=%+v", resumed, view, want)
	}
	if err := peer.PublishPendingProjection(ctx, resumed, parts); err != nil {
		t.Fatal(err)
	}
	pendingProjectionReadPG(t, ctx, storage, resumed, want)
	if counts := pendingProjectionCountsPG(t, ctx, peer, repo); counts[0] != int64(len(entries)) {
		t.Fatalf("resume replayed or lost input rows: counts=%v entries=%d", counts, len(entries))
	}
}

func TestPGPendingProjectionOnlyChangedInputAndOutputVersions(t *testing.T) {
	storage, ctx, repo, pending := pendingProjectionPG(t)
	job := pendingProjectionCapturePG(t, ctx, storage, pendingProjectionClaimPG(t, ctx, storage, repo), 0)
	view, parts := pendingProjectionPartsPG(t, ctx, storage, job)
	if err := storage.PublishPendingProjection(ctx, job, parts); err != nil {
		t.Fatal(err)
	}
	pendingProjectionReadPG(t, ctx, storage, job, view)
	for _, mutation := range []struct {
		name, sql, kind, key string
	}{
		{"private", `UPDATE pending_contexts SET data=jsonb_set(data,'{author,name}','"Changed capture"') WHERE repo_id=$1`, "pending", pending.SessionID},
		{"public", `UPDATE snapshots SET message='Changed public snapshot' WHERE repo_id=$1`, "snapshot", string(pending.Target)},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			before := pendingProjectionCountsPG(t, ctx, storage, repo)
			prior := job
			catalogExecPG(t, ctx, storage.pool, mutation.sql, string(repo))
			read, err := storage.ReadPendingProjection(ctx, repo)
			if err != nil || read.Ready || read.Source.Sequence <= prior.InputSequence || read.Applied.Sequence != prior.InputSequence {
				t.Fatalf("mutation did not invalidate old ready head: %+v err=%v", read, err)
			}
			job = pendingProjectionClaimPG(t, ctx, storage, repo)
			if job.Dataset != prior.Dataset || job.AppliedSequence != prior.InputSequence || job.SourceRequest().After == nil {
				t.Fatalf("incremental update bootstrapped a new dataset: before=%+v after=%+v", prior, job)
			}
			page := repositoryViewSourcePagePG(t, ctx, storage, repo, job.SourceRequest())
			if page.Mode != "delta" || page.Checkpoint == nil || len(page.Entries) != 1 || page.Entries[0].Kind != mutation.kind || page.Entries[0].Key != mutation.key {
				t.Fatalf("single-entity SQL mutation was not incremental: %+v", page)
			}
			if err := storage.SavePendingProjectionPage(ctx, job, page); err != nil {
				t.Fatal(err)
			}
			job = pendingProjectionClaimPG(t, ctx, storage, repo)
			view, parts = pendingProjectionPartsPG(t, ctx, storage, job)
			if mutation.kind == "pending" && view.Pending[0].Author.Name != "Changed capture" || mutation.kind == "snapshot" && view.Snapshots[0].Message != "Changed public snapshot" {
				t.Fatalf("latest input version was not selected: %+v", view)
			}
			if err := storage.PublishPendingProjection(ctx, job, parts); err != nil {
				t.Fatal(err)
			}
			pendingProjectionReadPG(t, ctx, storage, job, view)
			if after := pendingProjectionCountsPG(t, ctx, storage, repo); after != before {
				t.Fatalf("replacement accumulated obsolete derived rows: before=%v after=%v", before, after)
			}
			for _, table := range []string{"pending_projection_inputs", "pending_projection_parts"} {
				var matching int
				if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE repo_id=$1 AND dataset=$2 AND source_seq=$3`, string(repo), job.Dataset, job.InputSequence).Scan(&matching); err != nil || matching != 1 {
					t.Fatalf("%s copied unchanged entities: changed=%d err=%v", table, matching, err)
				}
				if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE repo_id=$1 AND dataset=$2 AND source_seq=$3 AND kind=$4 AND entity_key=$5 AND NOT deleted`,
					string(repo), job.Dataset, job.InputSequence, mutation.kind, mutation.key).Scan(&matching); err != nil || matching != 1 {
					t.Fatalf("%s changed version missing: count=%d err=%v", table, matching, err)
				}
			}
		})
	}
}

func TestPGPendingProjectionSourceCommitAndRollback(t *testing.T) {
	storage, ctx, repo, _ := pendingProjectionPG(t)
	job := pendingProjectionCapturePG(t, ctx, storage, pendingProjectionClaimPG(t, ctx, storage, repo), 0)
	view, parts := pendingProjectionPartsPG(t, ctx, storage, job)
	if err := storage.PublishPendingProjection(ctx, job, parts); err != nil {
		t.Fatal(err)
	}
	before := pendingProjectionReadPG(t, ctx, storage, job, view)
	counts := pendingProjectionCountsPG(t, ctx, storage, repo)
	for _, commit := range []bool{false, true} {
		transaction := catalogTxPG(t, ctx, storage)
		catalogExecPG(t, ctx, transaction, `UPDATE pending_contexts SET data=jsonb_set(data,'{dismissed}','true') WHERE repo_id=$1`, string(repo))
		catalogExecPG(t, ctx, transaction, `UPDATE snapshots SET message='committed projection input' WHERE repo_id=$1`, string(repo))
		pendingProjectionReadPG(t, ctx, storage, job, view)
		if _, err := storage.ClaimPendingProjection(ctx, time.Minute); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("uncommitted source was claimable: %v", err)
		}
		if !commit {
			if err := transaction.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if got := pendingProjectionReadPG(t, ctx, storage, job, view); got.Source != before.Source {
				t.Fatalf("rolled-back source advanced checkpoint: before=%+v after=%+v", before.Source, got.Source)
			}
			if got := pendingProjectionCountsPG(t, ctx, storage, repo); got != counts {
				t.Fatalf("rollback changed projection versions: before=%v after=%v", counts, got)
			}
			continue
		}
		if err := transaction.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	read, err := storage.ReadPendingProjection(ctx, repo)
	if err != nil || read.Ready || read.Applied != before.Applied || read.Source.Sequence <= before.Source.Sequence {
		t.Fatalf("commit did not invalidate readiness: %+v err=%v", read, err)
	}
	job = pendingProjectionClaimPG(t, ctx, storage, repo)
	page := repositoryViewSourcePagePG(t, ctx, storage, repo, job.SourceRequest())
	if len(page.Entries) != 2 || page.Checkpoint == nil {
		t.Fatalf("rollback leaked inputs or commit lost inputs: %+v", page)
	}
	for _, entry := range page.Entries {
		if entry.Sequence != page.Through {
			t.Fatalf("committed transaction split across generations: %+v", page)
		}
	}
	if err := storage.SavePendingProjectionPage(ctx, job, page); err != nil {
		t.Fatal(err)
	}
	job = pendingProjectionClaimPG(t, ctx, storage, repo)
	view, parts = pendingProjectionPartsPG(t, ctx, storage, job)
	if len(view.Pending) != 1 || !view.Pending[0].Dismissed || len(view.OrphanSessions) != 0 || len(view.HoldCounts) != 0 || view.Snapshots[0].Message != "committed projection input" {
		t.Fatalf("committed private/public image differs: %+v", view)
	}
	if err := storage.PublishPendingProjection(ctx, job, parts); err != nil {
		t.Fatal(err)
	}
	pendingProjectionReadPG(t, ctx, storage, job, view)
}

func TestPGPendingProjectionExpiredAndStaleFences(t *testing.T) {
	for _, phase := range []string{"capture", "project"} {
		t.Run(phase, func(t *testing.T) {
			storage, ctx, repo, _ := pendingProjectionPG(t)
			job := pendingProjectionClaimPG(t, ctx, storage, repo)
			page := repositoryViewSourcePagePG(t, ctx, storage, repo, job.SourceRequest())
			var parts []domain.PendingProjectionPart
			if phase == "project" {
				job = pendingProjectionCapturePG(t, ctx, storage, job, 0)
				_, parts = pendingProjectionPartsPG(t, ctx, storage, job)
			}
			write := func(lease domain.PendingProjectionLease) error {
				if phase == "capture" {
					return storage.SavePendingProjectionPage(ctx, lease, page)
				}
				return storage.PublishPendingProjection(ctx, lease, parts)
			}
			before := pendingProjectionCountsPG(t, ctx, storage, repo)
			catalogExecPG(t, ctx, storage.pool, `UPDATE pending_projection_work SET lease_until=clock_timestamp()-interval '1 second' WHERE repo_id=$1`, string(repo))
			assertRejected := func() {
				t.Helper()
				if err := write(job); !errors.Is(err, domain.ErrRefConflict) {
					t.Fatalf("invalid %s fence wrote: %v", phase, err)
				}
				if err := storage.RenewPendingProjection(ctx, job, time.Minute); !errors.Is(err, domain.ErrRefConflict) {
					t.Fatalf("invalid %s fence renewed: %v", phase, err)
				}
				if got := pendingProjectionCountsPG(t, ctx, storage, repo); got != before {
					t.Fatalf("rejected fence changed versions: before=%v after=%v", before, got)
				}
			}
			assertRejected()
			replacement := pendingProjectionClaimPG(t, ctx, storage, repo)
			if replacement.Fence <= job.Fence || replacement.Dataset != job.Dataset || replacement.Phase != phase {
				t.Fatalf("expired lease was not fenced: old=%+v new=%+v", job, replacement)
			}
			assertRejected()
			if err := storage.RenewPendingProjection(ctx, replacement, time.Minute); err != nil {
				t.Fatal(err)
			}
			if err := write(replacement); err != nil {
				t.Fatalf("stale worker damaged replacement lease: %v", err)
			}
		})
	}
}

func TestPGPendingProjectionLeaseRenewal(t *testing.T) {
	storage, ctx, repo, _ := pendingProjectionPG(t)
	job := pendingProjectionClaimPG(t, ctx, storage, repo)
	for _, duration := range []time.Duration{0, -time.Second, 6 * time.Minute} {
		if err := storage.RenewPendingProjection(ctx, job, duration); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("invalid renewal duration %v: %v", duration, err)
		}
	}
	var oldLease, oldRetention time.Time
	var pin int64
	if err := storage.pool.QueryRow(ctx, `UPDATE pending_projection_work SET lease_until=clock_timestamp()+interval '10 seconds',
		retention_until=clock_timestamp()+interval '1 minute' WHERE repo_id=$1 RETURNING lease_until,retention_until,pin_seq`, string(repo)).Scan(&oldLease, &oldRetention, &pin); err != nil {
		t.Fatal(err)
	}
	if err := storage.RenewPendingProjection(ctx, job, time.Minute); err != nil {
		t.Fatal(err)
	}
	var renewedLease, renewedRetention time.Time
	var fence, renewedPin int64
	if err := storage.pool.QueryRow(ctx, `SELECT lease_until,retention_until,fence,pin_seq FROM pending_projection_work WHERE repo_id=$1`, string(repo)).Scan(&renewedLease, &renewedRetention, &fence, &renewedPin); err != nil {
		t.Fatal(err)
	}
	if !renewedLease.After(oldLease) || !renewedRetention.After(oldRetention) || fence != job.Fence || renewedPin != pin {
		t.Fatalf("renewal changed ownership or failed to extend protection: lease=%v retention=%v fence=%d pin=%d", renewedLease, renewedRetention, fence, renewedPin)
	}
	if _, err := storage.ClaimPendingProjection(ctx, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("renewed lease was claimable: %v", err)
	}
	page := repositoryViewSourcePagePG(t, ctx, storage, repo, job.SourceRequest())
	if err := storage.SavePendingProjectionPage(ctx, job, page); err != nil {
		t.Fatalf("renewal invalidated the owner's token: %v", err)
	}
}

func TestPGPendingProjectionEpochResetRebootstraps(t *testing.T) {
	storage, ctx, repo, _ := pendingProjectionPG(t)
	job := pendingProjectionCapturePG(t, ctx, storage, pendingProjectionClaimPG(t, ctx, storage, repo), 0)
	view, parts := pendingProjectionPartsPG(t, ctx, storage, job)
	if err := storage.PublishPendingProjection(ctx, job, parts); err != nil {
		t.Fatal(err)
	}
	before := pendingProjectionReadPG(t, ctx, storage, job, view)
	catalogExecPG(t, ctx, storage.pool, `UPDATE snapshots SET message='before epoch reset' WHERE repo_id=$1`, string(repo))
	job = pendingProjectionCapturePG(t, ctx, storage, pendingProjectionClaimPG(t, ctx, storage, repo), 0)
	_, parts = pendingProjectionPartsPG(t, ctx, storage, job)
	catalogExecPG(t, ctx, storage.pool, `SELECT cxt_reset_catalog($1)`, string(repo))
	current := catalogReadStatePG(t, ctx, storage, repo)
	read, err := storage.ReadPendingProjection(ctx, repo)
	if err != nil || !read.Enabled || read.Ready || len(read.Parts) != 0 || read.Source.Epoch != current.epoch || read.Source.Epoch == before.Source.Epoch {
		t.Fatalf("old ready publication survived source epoch reset: %+v err=%v", read, err)
	}
	if err := storage.PublishPendingProjection(ctx, job, parts); !errors.Is(err, domain.ErrCatalogResetRequired) {
		t.Fatalf("old-epoch live lease published: %v", err)
	}
	if err := storage.RenewPendingProjection(ctx, job, time.Minute); !errors.Is(err, domain.ErrCatalogResetRequired) {
		t.Fatalf("old-epoch live lease renewed: %v", err)
	}
	catalogExecPG(t, ctx, storage.pool, `UPDATE pending_projection_work SET lease_until=clock_timestamp()-interval '1 second' WHERE repo_id=$1`, string(repo))
	fresh := pendingProjectionClaimPG(t, ctx, storage, repo)
	if fresh.Dataset == job.Dataset || fresh.Epoch != current.epoch || fresh.AppliedSequence != -1 || fresh.InputSequence != -1 || fresh.Cursor != "" || fresh.Phase != "capture" {
		t.Fatalf("epoch reset reused old progress: old=%+v fresh=%+v", job, fresh)
	}
	fresh = pendingProjectionCapturePG(t, ctx, storage, fresh, 1)
	view, parts = pendingProjectionPartsPG(t, ctx, storage, fresh)
	if view.Snapshots[0].Message != "before epoch reset" {
		t.Fatalf("rebootstrap lost current source image: %+v", view)
	}
	if err := storage.PublishPendingProjection(ctx, fresh, parts); err != nil {
		t.Fatal(err)
	}
	pendingProjectionReadPG(t, ctx, storage, fresh, view)
}

func TestPGPendingProjectionRetentionPinAndExpiredRebootstrap(t *testing.T) {
	storage, ctx, repo, _ := pendingProjectionPG(t)
	job := pendingProjectionClaimPG(t, ctx, storage, repo)
	request := job.SourceRequest()
	request.Limit = 1
	page := repositoryViewSourcePagePG(t, ctx, storage, repo, request)
	if page.NextCursor == "" {
		t.Fatal("fixture must retain a capture cursor")
	}
	if err := storage.SavePendingProjectionPage(ctx, job, page); err != nil {
		t.Fatal(err)
	}
	catalogExecPG(t, ctx, storage.pool, `UPDATE snapshots SET message='after expired capture' WHERE repo_id=$1`, string(repo))
	state := catalogReadStatePG(t, ctx, storage, repo)
	var pin int64
	var retained bool
	if err := storage.pool.QueryRow(ctx, `SELECT pin_seq,retention_until>clock_timestamp() FROM pending_projection_work WHERE repo_id=$1`, string(repo)).Scan(&pin, &retained); err != nil || !retained || pin >= state.head {
		t.Fatalf("capture did not retain history: pin=%d retained=%v state=%+v err=%v", pin, retained, state, err)
	}
	_, err := storage.pool.Exec(ctx, `SELECT cxt_prune_catalog($1,$2)`, string(repo), state.head)
	var constraint *pgconn.PgError
	if !errors.As(err, &constraint) || constraint.Code != "23514" {
		t.Fatalf("active capture pin allowed pruning: %v", err)
	}
	if got := catalogReadStatePG(t, ctx, storage, repo); got != state {
		t.Fatalf("blocked prune changed source: before=%+v after=%+v", state, got)
	}
	catalogExecPG(t, ctx, storage.pool, `UPDATE pending_projection_work SET retention_until=clock_timestamp()-interval '1 second' WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, storage.pool, `SELECT cxt_prune_catalog($1,$2)`, string(repo), state.head)
	if got := catalogReadStatePG(t, ctx, storage, repo); got.floor != state.head {
		t.Fatalf("expired pin still blocked pruning: %+v", got)
	}
	repositoryViewSourceErrorPG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: domain.RepositoryViewSourceVersion, Cursor: page.NextCursor}, domain.ErrCatalogResetRequired)
	fresh := pendingProjectionClaimPG(t, ctx, storage, repo)
	if fresh.Dataset == job.Dataset || fresh.Epoch != job.Epoch || fresh.AppliedSequence != -1 || fresh.InputSequence != -1 || fresh.Cursor != "" || fresh.Phase != "capture" {
		t.Fatalf("pruned capture did not explicitly replace dataset: old=%+v fresh=%+v", job, fresh)
	}
	fresh = pendingProjectionCapturePG(t, ctx, storage, fresh, 1)
	view, parts := pendingProjectionPartsPG(t, ctx, storage, fresh)
	if view.Snapshots[0].Message != "after expired capture" {
		t.Fatalf("old partial dataset leaked into new inputs: %+v", view)
	}
	if err := storage.PublishPendingProjection(ctx, fresh, parts); err != nil {
		t.Fatal(err)
	}
	pendingProjectionReadPG(t, ctx, storage, fresh, view)
}

func TestPGPendingProjectionDisableInvalidatesClaim(t *testing.T) {
	for _, phase := range []string{"capture", "project"} {
		t.Run(phase, func(t *testing.T) {
			storage, ctx, repo, _ := pendingProjectionPG(t)
			job := pendingProjectionClaimPG(t, ctx, storage, repo)
			page := repositoryViewSourcePagePG(t, ctx, storage, repo, job.SourceRequest())
			var parts []domain.PendingProjectionPart
			if phase == "project" {
				job = pendingProjectionCapturePG(t, ctx, storage, job, 0)
				_, parts = pendingProjectionPartsPG(t, ctx, storage, job)
			}
			before := pendingProjectionCountsPG(t, ctx, storage, repo)
			if err := storage.ConfigurePendingProjection(ctx, repo, false); err != nil {
				t.Fatal(err)
			}
			read, err := storage.ReadPendingProjection(ctx, repo)
			if err != nil || read.Enabled || read.Ready || len(read.Parts) != 0 {
				t.Fatalf("disabled projection remained readable: %+v err=%v", read, err)
			}
			if _, err := storage.ClaimPendingProjection(ctx, time.Minute); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("disabled projection remained claimable: %v", err)
			}
			if err := storage.RenewPendingProjection(ctx, job, time.Minute); !errors.Is(err, domain.ErrRefConflict) {
				t.Fatalf("disabled lease renewed: %v", err)
			}
			if phase == "capture" {
				err = storage.SavePendingProjectionPage(ctx, job, page)
			} else {
				err = storage.PublishPendingProjection(ctx, job, parts)
			}
			if !errors.Is(err, domain.ErrRefConflict) {
				t.Fatalf("disabled %s claim wrote: %v", phase, err)
			}
			if got := pendingProjectionCountsPG(t, ctx, storage, repo); got != before {
				t.Fatalf("disabled claim changed projection versions: before=%v after=%v", before, got)
			}
		})
	}
}
