//go:build postgres

package store

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.RepositoryMemorySourceGenerationStore = (*PostgresStore)(nil)

func memorySourceGenerationPG(test *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) domain.RepositoryMemorySourceGeneration {
	test.Helper()
	generation, err := storage.ReadRepositoryMemorySourceGeneration(ctx, repo)
	if err != nil {
		test.Fatal(err)
	}
	if err := generation.Validate(); err != nil || generation.Version != 1 || generation.Graph.RepoID != repo || generation.Graph.Scope != domain.RepositoryGraphSourceScope || generation.Epoch == generation.Graph.Epoch {
		test.Fatalf("invalid or graph-coupled memory generation: %+v err=%v", generation, err)
	}
	graph, err := storage.ReadRepositoryGraphSourceGeneration(ctx, repo)
	if err != nil || generation.Graph != graph {
		test.Fatalf("memory generation disagrees with graph: memory=%+v graph=%+v err=%v", generation, graph, err)
	}
	return generation
}

func memorySourceAdvancePG(test *testing.T, ctx context.Context, storage *PostgresStore, before domain.RepositoryMemorySourceGeneration, delta int64) domain.RepositoryMemorySourceGeneration {
	test.Helper()
	after := memorySourceGenerationPG(test, ctx, storage, before.Graph.RepoID)
	if after.Graph != before.Graph || after.Epoch != before.Epoch || after.Sequence != before.Sequence+delta {
		test.Fatalf("memory-only mutation advanced wrong generation: before=%+v after=%+v want_delta=%d", before, after, delta)
	}
	return after
}

func memorySourceRepoPG(test *testing.T, ctx context.Context, storage *PostgresStore, label string) domain.ContentHash {
	test.Helper()
	repo := domain.HashContent([]byte(test.Name() + label + domain.NewID("memory_source_")))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		test.Fatal(err)
	}
	return repo
}

func memorySourceBlobPG(test *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash, kind, label string) domain.ContentHash {
	test.Helper()
	body := []byte(string(repo) + ":memory-source-private-body:" + label)
	hash := domain.HashContent(body)
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, string(hash), body)
	if kind != "" {
		catalogExecPG(test, ctx, storage.pool, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,$2,$3)`, string(repo), kind, string(hash))
	}
	return hash
}

func TestPGRepositoryMemorySourceGenerationValidation(test *testing.T) {
	valid := domain.RepositoryMemorySourceGeneration{
		Version: 1,
		Graph: domain.RepositoryViewSourceCheckpoint{
			Version: 1, Scope: domain.RepositoryGraphSourceScope, RepoID: domain.HashContent([]byte(test.Name())),
			Epoch: "11111111-1111-4111-8111-111111111111", Sequence: 7,
		},
		Epoch: "22222222-2222-4222-8222-222222222222", Sequence: 0,
	}
	if err := valid.Validate(); err != nil {
		test.Fatal(err)
	}
	for _, testcase := range []struct {
		name   string
		mutate func(*domain.RepositoryMemorySourceGeneration)
	}{
		{"missing_version", func(generation *domain.RepositoryMemorySourceGeneration) { generation.Version = 0 }},
		{"future_version", func(generation *domain.RepositoryMemorySourceGeneration) { generation.Version = 2 }},
		{"missing_epoch", func(generation *domain.RepositoryMemorySourceGeneration) { generation.Epoch = "" }},
		{"malformed_epoch", func(generation *domain.RepositoryMemorySourceGeneration) { generation.Epoch = "not-a-uuid" }},
		{"zero_epoch", func(generation *domain.RepositoryMemorySourceGeneration) {
			generation.Epoch = "00000000-0000-0000-0000-000000000000"
		}},
		{"negative_sequence", func(generation *domain.RepositoryMemorySourceGeneration) { generation.Sequence = -1 }},
		{"wrong_graph_scope", func(generation *domain.RepositoryMemorySourceGeneration) {
			generation.Graph.Scope = domain.RepositoryViewSourceScope
		}},
		{"invalid_graph_version", func(generation *domain.RepositoryMemorySourceGeneration) { generation.Graph.Version = 2 }},
		{"invalid_graph_repo", func(generation *domain.RepositoryMemorySourceGeneration) { generation.Graph.RepoID = "invalid" }},
		{"invalid_graph_epoch", func(generation *domain.RepositoryMemorySourceGeneration) { generation.Graph.Epoch = "invalid" }},
		{"zero_graph_epoch", func(generation *domain.RepositoryMemorySourceGeneration) {
			generation.Graph.Epoch = "00000000-0000-0000-0000-000000000000"
		}},
		{"negative_graph_sequence", func(generation *domain.RepositoryMemorySourceGeneration) { generation.Graph.Sequence = -1 }},
	} {
		test.Run(testcase.name, func(test *testing.T) {
			generation := valid
			testcase.mutate(&generation)
			if err := generation.Validate(); err == nil {
				test.Fatalf("malformed generation accepted: %+v", generation)
			}
		})
	}
}

func TestPGRepositoryMemorySourceSharedOwnershipGrantRevokeAndMove(test *testing.T) {
	for _, kind := range []string{"memory", "memory_chunk"} {
		test.Run(kind, func(test *testing.T) {
			storage, ctx, owner := catalogPG(test)
			peer := memorySourceRepoPG(test, ctx, storage, "peer")
			outsider := memorySourceRepoPG(test, ctx, storage, "outsider")
			hash := memorySourceBlobPG(test, ctx, storage, owner, kind, "shared")
			catalogExecPG(test, ctx, storage.pool, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'chunk',$2)`, string(outsider), string(hash))
			ownerBefore := memorySourceGenerationPG(test, ctx, storage, owner)
			peerBefore := memorySourceGenerationPG(test, ctx, storage, peer)
			outsiderBefore := memorySourceGenerationPG(test, ctx, storage, outsider)
			catalogExecPG(test, ctx, storage.pool, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,$2,$3)`, string(peer), kind, string(hash))
			peerBefore = memorySourceAdvancePG(test, ctx, storage, peerBefore, 1)
			memorySourceAdvancePG(test, ctx, storage, ownerBefore, 0)
			catalogExecPG(test, ctx, storage.pool, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind=$2 AND hash=$3`, string(peer), kind, string(hash))
			peerBefore = memorySourceAdvancePG(test, ctx, storage, peerBefore, 1)
			memorySourceAdvancePG(test, ctx, storage, ownerBefore, 0)
			catalogExecPG(test, ctx, storage.pool, `UPDATE repo_blobs SET repo_id=$2 WHERE repo_id=$1 AND kind=$3 AND hash=$4`, string(owner), string(peer), kind, string(hash))
			ownerBefore = memorySourceAdvancePG(test, ctx, storage, ownerBefore, 1)
			peerBefore = memorySourceAdvancePG(test, ctx, storage, peerBefore, 1)
			catalogExecPG(test, ctx, storage.pool, `UPDATE repo_blobs SET kind='doc' WHERE repo_id=$1 AND kind=$2 AND hash=$3`, string(peer), kind, string(hash))
			peerBefore = memorySourceAdvancePG(test, ctx, storage, peerBefore, 1)
			catalogExecPG(test, ctx, storage.pool, `UPDATE repo_blobs SET kind=$2 WHERE repo_id=$1 AND kind='doc' AND hash=$3`, string(peer), kind, string(hash))
			peerBefore = memorySourceAdvancePG(test, ctx, storage, peerBefore, 1)
			replacement := memorySourceBlobPG(test, ctx, storage, peer, "", "replacement")
			catalogExecPG(test, ctx, storage.pool, `UPDATE repo_blobs SET hash=$3 WHERE repo_id=$1 AND hash=$2 AND kind=$4`, string(peer), string(hash), string(replacement), kind)
			memorySourceAdvancePG(test, ctx, storage, peerBefore, 1)
			memorySourceAdvancePG(test, ctx, storage, ownerBefore, 0)
			memorySourceAdvancePG(test, ctx, storage, outsiderBefore, 0)
		})
	}
}

func TestPGRepositoryMemorySourceSharedRootAndChunkBytes(test *testing.T) {
	for _, kind := range []string{"memory", "memory_chunk"} {
		test.Run(kind, func(test *testing.T) {
			storage, ctx, owner := catalogPG(test)
			peer := memorySourceRepoPG(test, ctx, storage, "peer")
			outsider := memorySourceRepoPG(test, ctx, storage, "doc-only")
			hash := memorySourceBlobPG(test, ctx, storage, owner, kind, "bytes")
			catalogExecPG(test, ctx, storage.pool, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory',$3),($1,'memory_chunk',$3),($2,'doc',$3)`, string(peer), string(outsider), string(hash))
			ownerBefore := memorySourceGenerationPG(test, ctx, storage, owner)
			peerBefore := memorySourceGenerationPG(test, ctx, storage, peer)
			outsiderBefore := memorySourceGenerationPG(test, ctx, storage, outsider)
			catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, string(hash), []byte("memory-source-private-body:changed"))
			memorySourceAdvancePG(test, ctx, storage, ownerBefore, 0)
			memorySourceAdvancePG(test, ctx, storage, peerBefore, 0)
			catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
			memorySourceAdvancePG(test, ctx, storage, ownerBefore, 0)
			memorySourceAdvancePG(test, ctx, storage, peerBefore, 0)
			memorySourceAdvancePG(test, ctx, storage, outsiderBefore, 0)
			for _, repo := range []domain.ContentHash{owner, peer} {
				entries, _ := graphSourceDrainPG(test, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1})
				for _, entry := range entries {
					if strings.Contains(string(entry.Value), "memory-source-private-body") {
						test.Fatalf("raw memory bytes leaked into graph journal: %+v", entry)
					}
				}
			}
		})
	}
}

func TestPGRepositoryMemorySourceLegacyFields(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	snapshot := initializationPGSnapshot(test, storage, repo, "legacy memory")
	before := memorySourceGenerationPG(test, ctx, storage, repo)
	for _, query := range []string{
		`INSERT INTO memories(repo_id,snapshot_id,summary,key_facts,open_tasks,provider) VALUES($1,$2,'before',ARRAY['fact'],ARRAY['task'],'claude')`,
		`UPDATE memories SET summary='after' WHERE repo_id=$1 AND snapshot_id=$2`,
		`UPDATE memories SET key_facts=ARRAY['new fact'] WHERE repo_id=$1 AND snapshot_id=$2`,
		`UPDATE memories SET open_tasks=ARRAY['new task'] WHERE repo_id=$1 AND snapshot_id=$2`,
		`UPDATE memories SET provider='codex' WHERE repo_id=$1 AND snapshot_id=$2`,
	} {
		catalogExecPG(test, ctx, storage.pool, query, string(repo), string(snapshot.ID))
		before = memorySourceAdvancePG(test, ctx, storage, before, 1)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE memories SET created_at=created_at+interval '1 second',summary=summary,key_facts=key_facts,open_tasks=open_tasks,provider=provider WHERE repo_id=$1`, string(repo))
	memorySourceAdvancePG(test, ctx, storage, before, 0)
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM memories WHERE repo_id=$1`, string(repo))
	memorySourceAdvancePG(test, ctx, storage, before, 1)
}

func TestPGRepositoryMemorySourceContributionMemoryFields(test *testing.T) {
	for _, prefix := range []string{"pr", "git"} {
		test.Run(prefix, func(test *testing.T) {
			storage, ctx, repo := catalogPG(test)
			contributions := pgx.Identifier{prefix + "_contributions"}.Sanitize()
			memories := pgx.Identifier{prefix + "_contribution_memories"}.Sanitize()
			catalogExecPG(test, ctx, storage.pool, `INSERT INTO `+contributions+`(repo_id,id,payload) VALUES($1,repeat('a',64),'{}'),($1,repeat('b',64),'{}')`, string(repo))
			before := memorySourceGenerationPG(test, ctx, storage, repo)
			for _, query := range []string{
				`INSERT INTO ` + memories + `(repo_id,id,contribution_id,publication_id,memory_hash,memory_source,memory_pinned,payload) VALUES($1,repeat('c',64),repeat('a',64),repeat('d',32),'','',true,'{"body":"memory-source-private-body"}')`,
				`UPDATE ` + memories + ` SET payload='{"body":"changed private memory"}' WHERE repo_id=$1`,
				`UPDATE ` + memories + ` SET memory_hash='sha256:'||repeat('e',64),memory_source='sha256:'||repeat('f',64) WHERE repo_id=$1`,
				`UPDATE ` + memories + ` SET memory_source='sha256:'||repeat('1',64) WHERE repo_id=$1`,
				`UPDATE ` + memories + ` SET publication_id=repeat('2',32) WHERE repo_id=$1`,
				`UPDATE ` + memories + ` SET contribution_id=repeat('b',64) WHERE repo_id=$1`,
				`UPDATE ` + memories + ` SET id=repeat('3',64) WHERE repo_id=$1`,
			} {
				catalogExecPG(test, ctx, storage.pool, query, string(repo))
				before = memorySourceAdvancePG(test, ctx, storage, before, 1)
			}
			catalogExecPG(test, ctx, storage.pool, `UPDATE `+memories+` SET payload=payload,memory_hash=memory_hash,memory_source=memory_source,memory_pinned=memory_pinned WHERE repo_id=$1`, string(repo))
			memorySourceAdvancePG(test, ctx, storage, before, 0)
			catalogExecPG(test, ctx, storage.pool, `DELETE FROM `+memories+` WHERE repo_id=$1`, string(repo))
			memorySourceAdvancePG(test, ctx, storage, before, 1)
			entries, _ := graphSourceDrainPG(test, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1})
			for _, entry := range entries {
				if strings.Contains(string(entry.Value), "private") {
					test.Fatalf("contribution memory payload leaked into graph journal: %+v", entry)
				}
			}
		})
	}
}

func TestPGRepositoryMemorySourceCoalescesAndRollsBack(test *testing.T) {
	for _, commit := range []bool{false, true} {
		name := "rollback"
		if commit {
			name = "commit"
		}
		test.Run(name, func(test *testing.T) {
			storage, ctx, repo := catalogPG(test)
			snapshot := initializationPGSnapshot(test, storage, repo, "coalesced memory")
			hash := memorySourceBlobPG(test, ctx, storage, repo, "memory", "coalesced")
			before := memorySourceGenerationPG(test, ctx, storage, repo)
			transaction := catalogTxPG(test, ctx, storage)
			catalogExecPG(test, ctx, transaction, `SAVEPOINT abandoned_memory`)
			catalogExecPG(test, ctx, transaction, `INSERT INTO memories(repo_id,snapshot_id,summary,provider) VALUES($1,$2,'abandoned','claude')`, string(repo), string(snapshot.ID))
			catalogExecPG(test, ctx, transaction, `ROLLBACK TO SAVEPOINT abandoned_memory`)
			var sequence int64
			if err := transaction.QueryRow(ctx, `SELECT head_seq FROM repository_memory_source_state WHERE repo_id=$1`, string(repo)).Scan(&sequence); err != nil || sequence != before.Sequence {
				test.Fatalf("savepoint rollback leaked memory sequence: sequence=%d err=%v", sequence, err)
			}
			var updatesBefore int64
			if err := transaction.QueryRow(ctx, `SELECT pg_stat_get_xact_tuples_updated('repository_memory_source_state'::regclass)`).Scan(&updatesBefore); err != nil {
				test.Fatal(err)
			}
			catalogExecPG(test, ctx, transaction, `INSERT INTO memories(repo_id,snapshot_id,summary,provider) VALUES($1,$2,'coalesced','claude')`, string(repo), string(snapshot.ID))
			catalogExecPG(test, ctx, transaction, `UPDATE memories SET summary='final',key_facts=ARRAY['fact'],open_tasks=ARRAY['task'] WHERE repo_id=$1`, string(repo))
			catalogExecPG(test, ctx, transaction, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, string(hash), []byte("committed body"))
			catalogExecPG(test, ctx, transaction, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory_chunk',$2)`, string(repo), string(hash))
			var currentWriter bool
			if err := transaction.QueryRow(ctx, `SELECT head_seq,writer_xid=pg_current_xact_id() FROM repository_memory_source_state WHERE repo_id=$1`, string(repo)).Scan(&sequence, &currentWriter); err != nil || sequence != before.Sequence+1 || !currentWriter {
				test.Fatalf("transaction did not coalesce memory mutations: sequence=%d writer=%v err=%v", sequence, currentWriter, err)
			}
			var updatesAfter int64
			if err := transaction.QueryRow(ctx, `SELECT pg_stat_get_xact_tuples_updated('repository_memory_source_state'::regclass)`).Scan(&updatesAfter); err != nil || updatesAfter != updatesBefore+1 {
				test.Fatalf("transaction rewrote clock more than once: before=%d after=%d err=%v", updatesBefore, updatesAfter, err)
			}
			memorySourceAdvancePG(test, ctx, storage, before, 0)
			if commit {
				if err := transaction.Commit(ctx); err != nil {
					test.Fatal(err)
				}
				before = memorySourceAdvancePG(test, ctx, storage, before, 1)
			} else if err := transaction.Rollback(ctx); err != nil {
				test.Fatal(err)
			}
			memorySourceAdvancePG(test, ctx, storage, before, 0)
			catalogExecPG(test, ctx, storage.pool, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='memory' AND hash=$2`, string(repo), string(hash))
			memorySourceAdvancePG(test, ctx, storage, before, 1)
		})
	}
}

func TestPGRepositoryMemorySourceIgnoresConversationDocumentsQueuesAndNoOps(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	before := memorySourceGenerationPG(test, ctx, storage, repo)
	for _, kind := range []string{"doc", "chunk"} {
		hash := memorySourceBlobPG(test, ctx, storage, repo, kind, kind)
		memorySourceAdvancePG(test, ctx, storage, before, 0)
		catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, string(hash), []byte("nonmemory changed"))
		memorySourceAdvancePG(test, ctx, storage, before, 0)
		catalogExecPG(test, ctx, storage.pool, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind=$2 AND hash=$3`, string(repo), kind, string(hash))
		catalogExecPG(test, ctx, storage.pool, `DELETE FROM blobs WHERE hash=$1`, string(hash))
		memorySourceAdvancePG(test, ctx, storage, before, 0)
	}
	hash := memorySourceBlobPG(test, ctx, storage, repo, "", "unowned")
	for _, query := range []string{
		`INSERT INTO doc_range_jobs_v1(repo_id,doc_hash,state,created_at,next_attempt) VALUES($1,$2,'waiting',now(),now())`,
		`UPDATE doc_range_jobs_v1 SET attempts=attempts+1,version=version+1,next_attempt=next_attempt+interval '1 second' WHERE repo_id=$1 AND doc_hash=$2`,
		`DELETE FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2`,
	} {
		catalogExecPG(test, ctx, storage.pool, query, string(repo), string(hash))
		memorySourceAdvancePG(test, ctx, storage, before, 0)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, string(hash), []byte("unowned changed"))
	memorySourceAdvancePG(test, ctx, storage, before, 0)
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory',$2)`, string(repo), string(hash))
	before = memorySourceAdvancePG(test, ctx, storage, before, 1)
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory',$2) ON CONFLICT DO NOTHING`, string(repo), string(hash))
	catalogExecPG(test, ctx, storage.pool, `UPDATE repo_blobs SET kind=kind,hash=hash,repo_id=repo_id WHERE repo_id=$1`, string(repo))
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM memories WHERE repo_id=$1`, string(repo))
	memorySourceAdvancePG(test, ctx, storage, before, 0)
}
