//go:build postgres

package store

import (
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGGraphInputProfileResetsLegacyCursor(t *testing.T) {
	storage, ctx, repo := graphProjectionPG(t)
	job := graphProjectionClaimPG(t, ctx, storage, repo)
	page := graphProjectionPagePG(t, ctx, storage, job, 1)
	if page.NextCursor == "" {
		t.Fatal("fixture did not produce a continuation")
	}
	if err := storage.SaveGraphProjectionPage(ctx, job, page); err != nil {
		t.Fatal(err)
	}
	if err := storage.ConfigureGraphProjection(ctx, repo, true); err != nil {
		t.Fatal(err)
	}
	next := graphProjectionClaimPG(t, ctx, storage, repo)
	if next.Dataset == job.Dataset || next.Cursor != "" || next.AppliedSequence != -1 || next.Fence <= job.Fence {
		t.Fatalf("legacy continuation was resumed: %+v", next)
	}
}

func TestPGGraphInputProfilePreservesFilteredContinuation(t *testing.T) {
	storage, ctx, repo := graphProjectionPG(t)
	job := graphProjectionClaimPG(t, ctx, storage, repo)
	request := job.SourceRequest()
	request.Profile, request.Limit = domain.RepositoryGraphProjectionSourceProfile, 1
	page, err := storage.RepositoryGraphSourceChanges(ctx, repo, request)
	if err != nil || page.NextCursor == "" {
		t.Fatalf("filtered continuation: %+v %v", page, err)
	}
	if err := storage.SaveGraphProjectionPage(ctx, job, page); err != nil {
		t.Fatal(err)
	}
	if err := storage.ConfigureGraphProjection(ctx, repo, true); err != nil {
		t.Fatal(err)
	}
	next := graphProjectionClaimPG(t, ctx, storage, repo)
	if next.Dataset != job.Dataset || next.Cursor != page.NextCursor {
		t.Fatalf("filtered progress was discarded: %+v", next)
	}
}

func TestPGGraphInputProfileRebuildRetainsPriorOutput(t *testing.T) {
	storage, ctx, repo := graphProjectionPG(t)
	graphProjectionTreePG(t, ctx, storage, repo)
	job := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
	graph := graphProjectionPublishPG(t, ctx, storage, job)
	prior := graphProjectionReadPG(t, ctx, storage, job, graph)
	if err := storage.ConfigureGraphProjection(ctx, repo, true); err != nil {
		t.Fatal(err)
	}
	next := graphProjectionClaimPG(t, ctx, storage, repo)
	if next.Dataset == job.Dataset || next.AppliedSequence != -1 || next.Phase != "capture" {
		t.Fatalf("obsolete proof inputs were retained: %+v", next)
	}
	retained, err := storage.ReadGraphProjection(ctx, repo)
	if err != nil || !retained.Complete || retained.Dataset != prior.Dataset || retained.Applied != prior.Applied || !reflect.DeepEqual(retained.Parts, prior.Parts) {
		t.Fatalf("input reset lost complete output: %v", err)
	}
	if err := storage.ConfigureGraphProjection(ctx, repo, true); err != nil {
		t.Fatal(err)
	}
	if err := storage.RenewGraphProjection(ctx, next, time.Minute); err != nil {
		t.Fatalf("repeat configuration fenced fresh work: %v", err)
	}
}

func TestPGGraphQualificationExcludesFilePayloadsOnly(t *testing.T) {
	storage, ctx, key := graphQualificationPG(t)
	before, err := graphQualificationInputCheckPG(t, ctx, storage, key.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	graphQualificationUnjournaledInputPG(t, ctx, storage, `INSERT INTO git_tree_nodes(repo_id,origin,oid,payload)
		SELECT $1,'unused',ordinal::text,jsonb_build_object('padding',repeat('x',524288)) FROM generate_series(1,65) AS ordinal`, string(key.RepoID))
	after, err := graphQualificationInputCheckPG(t, ctx, storage, key.RepoID)
	if err != nil || before.rows != after.rows {
		t.Fatalf("unread file payloads blocked graph qualification: before=%+v after=%+v err=%v", before, after, err)
	}
}
