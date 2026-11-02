//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGEffectiveMemoryEvidenceOnlyCommitInvalidatesProofAndCursor(test *testing.T) {
	service, storage, repo := collaborationPG(test)
	ctx, cancel := context.WithTimeout(systemTestContext(), 30*time.Second)
	defer cancel()
	metadata, err := storage.GetRepo(ctx, repo)
	if err != nil {
		test.Fatal(err)
	}
	snapshot := collaborationSnapshot(test, storage, repo, "memory-source")
	code := effectiveOID(1)
	digest := domain.MemoryDigest{SnapshotID: snapshot, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{{SourceSnapshot: snapshot, Claims: []domain.MemoryClaim{
		{Kind: "rationale", Text: "Unchanged first page"},
		{Kind: "code", Text: "Later code claim", Code: &domain.MemoryCodeScope{Commit: code, Paths: []string{"file"}}},
	}}}}
	memory, err := service.PutMemoryDigestCAS(ctx, repo, digest)
	if err != nil {
		test.Fatal(err)
	}
	event := domain.HistoryEvent{ID: strings.Repeat("d", 32), RepoID: string(repo), Branch: "main", BranchID: "main-id", Kind: "position", Target: snapshot, GitAfter: code, CreatedAt: time.Now().UTC()}
	if err := storage.ApplyHistoryEvent(ctx, event); err != nil {
		test.Fatal(err)
	}
	if err := storage.ApplyHistoryEvent(ctx, prPublication(event)); err != nil {
		test.Fatal(err)
	}
	job := domain.NewGitScan(repo, metadata.GitRemoteURL, code, time.Now().UTC())
	if err := storage.EnqueueGitScan(ctx, job); err != nil {
		test.Fatal(err)
	}
	job, err = storage.ClaimGitScan(ctx, repo, time.Now().UTC(), time.Minute)
	if err != nil {
		test.Fatal(err)
	}
	node, err := domain.NewGitTreeNode(map[string]domain.GitEntry{}, 40)
	if err != nil {
		test.Fatal(err)
	}
	finish := domain.GitScanFinish{Job: job, Tree: &domain.GitTreeEvidence{Commit: domain.GitCommitTree{Commit: code, Tree: node.OID, Parents: []string{}}, Nodes: []domain.GitTreeNode{node}}}
	finish.Job.State, finish.Job.TreeIndexed = "waiting", true
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: snapshot, MemoryHash: memory, CodeCommit: code}, Limit: 1}
	var before domain.EffectiveMemoryPage
	err = storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		var err error
		before, err = service.QueryEffectiveMemory(bound, repo, request)
		if err != nil {
			return err
		}
		if before.NextCursor == "" || before.DeliveryStateHash == "" {
			test.Fatal("fixture needs a complete proof and another page")
		}
		if err := storage.FinishGitScan(ctx, finish); err != nil {
			return err
		}
		held, err := service.QueryEffectiveMemory(bound, repo, request)
		if err == nil && !reflect.DeepEqual(held, before) {
			test.Fatal("read snapshot mixed committed evidence generations")
		}
		return err
	})
	if err != nil {
		test.Fatal(err)
	}
	after, err := service.QueryEffectiveMemory(ctx, repo, request)
	if err != nil {
		test.Fatal(err)
	}
	cold := NewService(storage, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
	expected, err := cold.QueryEffectiveMemory(ctx, repo, request)
	if err != nil {
		test.Fatal(err)
	}
	if before.Revision != after.Revision || !reflect.DeepEqual(before.Items, after.Items) || before.LineageHash != after.LineageHash {
		test.Fatal("fixture changed more than off-page code evidence")
	}
	if expected.DeliveryStateHash == before.DeliveryStateHash {
		test.Fatal("fixture did not change the complete assessment")
	}
	if after.DeliveryStateHash != expected.DeliveryStateHash {
		test.Fatal("warm cache retained an obsolete assessment after evidence-only commit")
	}
	if after.StateHash == before.StateHash {
		test.Fatal("evidence-only commit did not invalidate the continuation state")
	}
	request.Cursor = before.NextCursor
	if _, err := service.QueryEffectiveMemory(ctx, repo, request); !errors.Is(err, domain.ErrEffectiveMemoryCursorStale) {
		test.Fatal("old cursor crossed committed evidence generations", err)
	}
	request.Cursor = ""
	if err := storage.PutPending(ctx, repo, domain.Pending{RepoID: repo, SessionID: "unrelated-live-session", Target: snapshot, UpdatedAt: time.Now().UTC()}); err != nil {
		test.Fatal(err)
	}
	unrelated, err := service.QueryEffectiveMemory(ctx, repo, request)
	if err != nil || unrelated.StateHash == after.StateHash || unrelated.DeliveryStateHash != after.DeliveryStateHash {
		test.Fatal("unrelated source activity must fence continuation without changing delivered knowledge", err)
	}
}

func TestPGEffectiveMemoryWarmProofChecksOwnershipAndBytes(test *testing.T) {
	for _, representation := range []string{"inline", "chunked"} {
		test.Run(representation, func(test *testing.T) {
			testPGEffectiveMemoryWarmProofChecksOwnershipAndBytes(test, representation == "chunked")
		})
	}
}

func testPGEffectiveMemoryWarmProofChecksOwnershipAndBytes(test *testing.T, chunked bool) {
	test.Helper()
	service, storage, repo := collaborationPG(test)
	ctx, cancel := context.WithTimeout(systemTestContext(), 30*time.Second)
	defer cancel()
	connection, err := pgxpool.New(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	defer connection.Close()
	snapshot := collaborationSnapshot(test, storage, repo, "memory-access")
	digest := domain.MemoryDigest{SnapshotID: snapshot, Summary: "Validated current memory", KeyFacts: []string{"A second page"}}
	if chunked {
		digest.Summary = strings.Repeat("Validated current memory", 4000)
	}
	memory, err := service.PutMemoryDigestCAS(ctx, repo, digest)
	if err != nil {
		test.Fatal(err)
	}
	object, kind, missing := memory, "memory", domain.ErrNotFound
	if chunked {
		plan, isChunked, err := domain.PlanMemoryChunks(digest)
		if err != nil || !isChunked || len(plan.Order) == 0 {
			test.Fatal("fixture did not produce chunked memory", err)
		}
		object, kind, missing = plan.Order[0], "memory_chunk", domain.ErrIntegrity
	}
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: snapshot, MemoryHash: memory, CodeCommit: effectiveOID(1)}, Limit: 1}
	before, err := service.QueryEffectiveMemory(ctx, repo, request)
	if err != nil {
		test.Fatal(err)
	}
	if _, exists := service.deliveryCache.Lookup(before.StateHash); !exists {
		test.Fatal("fixture did not warm proof cache")
	}
	if before.NextCursor == "" {
		test.Fatal("fixture needs another page")
	}
	memorySource, err := storage.ReadRepositoryMemorySourceGeneration(ctx, repo)
	if err != nil {
		test.Fatal(err)
	}
	source, err := storage.ReadRepositoryGraphSourceGeneration(ctx, repo)
	if err != nil {
		test.Fatal(err)
	}
	assertUnchangedSource := func() {
		test.Helper()
		current, err := storage.ReadRepositoryGraphSourceGeneration(ctx, repo)
		if err != nil || current != source {
			test.Fatal("fixture unexpectedly changed graph generation", err)
		}
		memoryCurrent, err := storage.ReadRepositoryMemorySourceGeneration(ctx, repo)
		if err != nil || memoryCurrent.Graph != source || memoryCurrent.Epoch != memorySource.Epoch || memoryCurrent.Sequence <= memorySource.Sequence {
			test.Fatal("memory ownership/bytes change did not advance its independent generation", err)
		}
	}
	if _, err := connection.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2 AND kind=$3`, string(repo), string(object), kind); err != nil {
		test.Fatal(err)
	}
	assertUnchangedSource()
	if _, err := service.QueryEffectiveMemory(ctx, repo, request); !errors.Is(err, missing) {
		test.Fatal("warm proof bypassed current memory ownership", err)
	}
	if _, err := connection.Exec(ctx, `INSERT INTO repo_blobs(repo_id,hash,kind) VALUES($1,$2,$3)`, string(repo), string(object), kind); err != nil {
		test.Fatal(err)
	}
	intactMetadata, err := storage.ReadRepositoryMemorySourceGeneration(ctx, repo)
	if err != nil {
		test.Fatal(err)
	}
	var original []byte
	if err := connection.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, string(object)).Scan(&original); err != nil {
		test.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, string(object), []byte(`{"snapshot_id":"`+string(snapshot)+`","summary":"changed under same key"}`)); err != nil {
		test.Fatal(err)
	}
	assertUnchangedSource()
	if changed, err := storage.ReadRepositoryMemorySourceGeneration(ctx, repo); err != nil || changed != intactMetadata {
		test.Fatal("byte-only change unexpectedly changed memory metadata generation", err)
	}
	if _, err := service.QueryEffectiveMemory(ctx, repo, request); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("warm proof bypassed current memory hash check", err)
	}
	if _, err := connection.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, string(object), original); err != nil {
		test.Fatal(err)
	}
	after, err := service.QueryEffectiveMemory(ctx, repo, request)
	if err != nil || before.StateHash == after.StateHash {
		test.Fatal("restored bytes reused an earlier dependency generation", err)
	}
	comparable := after
	comparable.StateHash, comparable.NextCursor = before.StateHash, before.NextCursor
	if !reflect.DeepEqual(before, comparable) {
		test.Fatal("dependency changes altered restored semantic memory")
	}
	request.Cursor = before.NextCursor
	if _, err := service.QueryEffectiveMemory(ctx, repo, request); !errors.Is(err, domain.ErrEffectiveMemoryCursorStale) {
		test.Fatal("continuation accepted a restored but different dependency generation", err)
	}
}
