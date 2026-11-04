//go:build postgres

package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type graphServingSizePauseKey struct{}

type graphServingSizePause struct {
	prefix  string
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func (pause *graphServingSizePause) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, pause.prefix) {
		return context.WithValue(ctx, graphServingSizePauseKey{}, true)
	}
	return ctx
}

func (pause *graphServingSizePause) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(graphServingSizePauseKey{}) != true {
		return
	}
	pause.once.Do(func() {
		close(pause.reached)
		select {
		case <-pause.release:
		case <-ctx.Done():
		}
	})
}

func graphServingPausedPeerPG(t *testing.T, ctx context.Context, storage *PostgresStore, pause *graphServingSizePause) *PostgresStore {
	t.Helper()
	peer, err := NewPostgresStore(ctx, storage.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(peer.Close)
	config := peer.pool.Config()
	config.ConnConfig.Tracer = pause
	traced, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	peer.pool.Close()
	peer.pool = traced
	return peer
}

func TestPGGraphServingPromotionPinsSizeAndPayloadAgainstConcurrentGrowth(t *testing.T) {
	storage, ctx, key := graphQualificationPG(t)
	lease := graphQualificationClaimPG(t, ctx, storage, key)
	result := graphQualificationResultPG(t, ctx, storage, key, true)
	candidate, err := storage.ReadGraphProjection(ctx, key.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	pause := &graphServingSizePause{prefix: "SELECT source_seq,deleted,COALESCE(octet_length(image::text),0)",
		reached: make(chan struct{}), release: make(chan struct{})}
	peer := graphServingPausedPeerPG(t, ctx, storage, pause)
	var release sync.Once
	unblock := func() { release.Do(func() { close(pause.release) }) }
	defer unblock()
	done := make(chan error, 1)
	go func() { done <- peer.FinishGraphQualification(ctx, lease, result) }()
	select {
	case <-pause.reached:
	case err := <-done:
		t.Fatal("promotion skipped size gate", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	catalogExecPG(t, ctx, storage.pool, `UPDATE graph_projection_parts SET image=jsonb_build_object('large',repeat('x',$2))
		WHERE repo_id=$1 AND kind='header'`, string(key.RepoID), domain.MaxPendingProjectionOutputBytes+1)
	unblock()
	if err := <-done; err != nil {
		t.Fatal("size check and payload read used different snapshots", err)
	}
	serving := graphServingReadPG(t, ctx, storage, key.RepoID)
	if !reflect.DeepEqual(serving.Parts, candidate.Parts) || serving.Qualification.Actual != result.Actual {
		t.Fatal("concurrent growth reached retained copy")
	}
	if _, err := storage.ReadGraphProjection(ctx, key.RepoID); !errors.Is(err, domain.ErrRepositoryViewSourceLimit) {
		t.Fatal("fixture did not grow candidate beyond bound", err)
	}
}

func TestPGGraphServingPromotionRejectsQuarantineCommittedAfterSnapshot(t *testing.T) {
	storage, ctx, initial := graphServingPG(t)
	repo := initial.Source.RepoID
	for range 3 {
		failure := graphServingCorruptPG(t, ctx, storage, repo)
		if err := storage.RequestGraphServingRepair(ctx, failure); err != nil {
			t.Fatal(err)
		}
		initial = graphServingPromotePG(t, ctx, storage, initial.QualificationKey())
	}
	failure := graphServingCorruptPG(t, ctx, storage, repo)
	catalogExecPG(t, ctx, storage.pool, `DELETE FROM graph_serving_parts WHERE repo_id=$1`, string(repo))
	mismatched := graphQualificationAdvancePG(t, ctx, storage, repo)
	graphQualificationDuePG(t, ctx, storage, repo)
	lease := graphQualificationClaimPG(t, ctx, storage, mismatched)
	if err := storage.FinishGraphQualification(ctx, lease, graphQualificationResultPG(t, ctx, storage, mismatched, false)); err != nil {
		t.Fatal(err)
	}
	latest := graphQualificationAdvancePG(t, ctx, storage, repo)
	graphQualificationDuePG(t, ctx, storage, repo)
	lease = graphQualificationClaimPG(t, ctx, storage, latest)
	result := graphQualificationResultPG(t, ctx, storage, latest, true)
	pause := &graphServingSizePause{prefix: "SELECT required_doc_identity FROM repos WHERE id=$1 FOR SHARE",
		reached: make(chan struct{}), release: make(chan struct{})}
	peer := graphServingPausedPeerPG(t, ctx, storage, pause)
	var release sync.Once
	unblock := func() { release.Do(func() { close(pause.release) }) }
	defer unblock()
	done := make(chan error, 1)
	go func() { done <- peer.FinishGraphQualification(ctx, lease, result) }()
	select {
	case <-pause.reached:
	case err := <-done:
		t.Fatal("promotion skipped snapshot barrier", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := storage.RequestGraphServingRepair(ctx, failure); err != nil {
		t.Fatal(err)
	}
	before := graphServingSnapshotPG(t, ctx, storage, repo)
	receipt := graphQualificationSnapshotPG(t, ctx, storage, repo)
	unblock()
	if err := <-done; !errors.Is(err, domain.ErrRefConflict) {
		t.Fatal("snapshot preceding quarantine promoted behind it", err)
	}
	if !reflect.DeepEqual(before, graphServingSnapshotPG(t, ctx, storage, repo)) || !reflect.DeepEqual(receipt, graphQualificationSnapshotPG(t, ctx, storage, repo)) {
		t.Fatal("quarantined completion changed retained state or receipt")
	}
	status, err := storage.ReadQualifiedGraphProjectionStatus(ctx, repo)
	if err != nil || !status.NeedsAttention || status.Complete {
		t.Fatal("quarantine no longer authoritative", status, err)
	}
}
