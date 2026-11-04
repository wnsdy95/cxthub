//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func memoryCapturePGRead(test *testing.T, storage *PostgresStore, ctx context.Context, repo, hash domain.ContentHash, limit int, expected error) domain.MemoryDigest {
	test.Helper()
	var digest domain.MemoryDigest
	err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		digest = memoryCaptureTestRead(test, storage, bound, memoryCaptureTestKey{repo, "memory", hash}, limit, expected)
		return nil
	})
	if err != nil {
		test.Fatal(err)
	}
	return digest
}

func memoryCapturePGObject(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash, kind string, hash domain.ContentHash, raw []byte) {
	test.Helper()
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO blobs(hash,bytes) VALUES($1,$2) ON CONFLICT(hash) DO UPDATE SET bytes=EXCLUDED.bytes`, string(hash), raw)
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, string(repo), kind, string(hash))
}

func TestPGMemoryCaptureRoundTripAndTransactionFence(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	for _, chunked := range []bool{false, true} {
		digest := memoryCaptureTestDigest()
		digest.SnapshotID = repo
		if chunked {
			digest.Summary = strings.Repeat(string(repo), 2048)
			digest.Fragments = []domain.MemoryFragment{{SourceSnapshot: repo, Summary: strings.Repeat("fragment", 12<<10)}}
		}
		hash, err := storage.PutMemory(ctx, repo, digest)
		if err != nil {
			test.Fatal(err)
		}
		key := memoryCaptureTestKey{repo, "memory", hash}
		memoryCaptureTestRead(test, storage, ctx, key, 1<<20, domain.ErrConflict)
		if err := storage.WithinRepository(ctx, repo, func(write context.Context) error {
			memoryCaptureTestRead(test, storage, write, key, 1<<20, domain.ErrConflict)
			return storage.WithinReadSnapshot(write, func(nested context.Context) error {
				memoryCaptureTestRead(test, storage, nested, key, 1<<20, domain.ErrConflict)
				return nil
			})
		}); err != nil {
			test.Fatal(err)
		}
		if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
			peer := &PostgresStore{pool: storage.pool}
			memoryCaptureTestRead(test, peer, bound, key, 1<<20, domain.ErrConflict)
			canceled, cancel := context.WithCancel(bound)
			cancel()
			memoryCaptureTestRead(test, storage, canceled, key, 1<<20, context.Canceled)
			return nil
		}); err != nil {
			test.Fatal(err)
		}
		for iteration := range 2 {
			actual := memoryCapturePGRead(test, storage, ctx, repo, hash, 1<<20, nil)
			if !reflect.DeepEqual(actual, digest) {
				test.Fatal("bounded PostgreSQL roundtrip changed memory")
			}
			if iteration == 0 {
				var raw []byte
				if err := storage.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, string(hash)).Scan(&raw); err != nil {
					test.Fatal(err)
				}
				catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, string(hash), docCompress(raw))
			}
		}
	}
}

func TestPGMemoryCaptureOwnershipAndIntegrity(test *testing.T) {
	for _, scenario := range []string{"root-unowned", "component-unowned", "component-wrong-kind", "root-corrupt", "component-corrupt", "bad-compression"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, repo := catalogPG(test)
			digest := memoryCaptureTestDigest()
			digest.SnapshotID = repo
			digest.Summary = strings.Repeat(string(repo), 2048)
			hash, err := storage.PutMemory(ctx, repo, digest)
			if err != nil {
				test.Fatal(err)
			}
			plan, _, err := domain.PlanMemoryChunks(digest)
			if err != nil {
				test.Fatal(err)
			}
			foreign := memorySourceRepoPG(test, ctx, storage, "unowned")
			memoryCapturePGRead(test, storage, ctx, foreign, hash, 1<<20, domain.ErrNotFound)
			expected := domain.ErrIntegrity
			switch scenario {
			case "root-unowned":
				catalogExecPG(test, ctx, storage.pool, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='memory' AND hash=$2`, string(repo), string(hash))
				expected = domain.ErrNotFound
			case "component-unowned", "component-wrong-kind":
				catalogExecPG(test, ctx, storage.pool, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='memory_chunk' AND hash=$2`, string(repo), string(plan.Order[0]))
				catalogExecPG(test, ctx, storage.pool, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory_chunk',$2)`, string(foreign), string(plan.Order[0]))
				if scenario == "component-wrong-kind" {
					catalogExecPG(test, ctx, storage.pool, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory',$2)`, string(repo), string(plan.Order[0]))
				}
			case "root-corrupt":
				catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, string(hash), []byte(`{"format":"cxt-memory-chunks-v99"}`))
			case "component-corrupt":
				catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, string(plan.Order[0]), docCompress([]byte("corrupt")))
			case "bad-compression":
				catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, string(hash), []byte{0x28, 0xb5, 0x2f, 0xfd, 0xff})
			}
			memoryCapturePGRead(test, storage, ctx, repo, hash, 1<<20, expected)
		})
	}
}

func TestPGMemoryCaptureExpansionAndWireBounds(test *testing.T) {
	for _, scenario := range []string{"stored-root", "decoded-root", "stored-component", "decoded-component", "repeated", "exact-wire"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, repo := catalogPG(test)
			digest := memoryCaptureTestDigest()
			digest.SnapshotID = repo
			body := []byte(strings.Repeat("<", 2048))
			component := domain.HashContent(body)
			manifest := domain.MemoryChunkManifest{
				Format: domain.MemoryChunkFormatV1, SnapshotID: repo, Provider: digest.Provider,
				KeyFacts: digest.KeyFacts, OpenTasks: digest.OpenTasks, SummaryChunks: []domain.ContentHash{component},
			}
			if scenario == "repeated" {
				for range 31 {
					manifest.SummaryChunks = append(manifest.SummaryChunks, component)
				}
			}
			digest.Summary = strings.Repeat(string(body), len(manifest.SummaryChunks))
			hash, err := domain.MemoryDigestHash(digest)
			if err != nil {
				test.Fatal(err)
			}
			root := memoryCaptureTestJSON(test, manifest)
			stored := docCompress(body)
			limit := 4096
			switch scenario {
			case "stored-root":
				root = []byte(strings.Repeat("x", limit+1))
			case "decoded-root":
				root = docCompress([]byte(strings.Repeat("x", 4<<20)))
			case "stored-component":
				stored = []byte(strings.Repeat("x", limit+1))
			case "decoded-component":
				stored = docCompress([]byte(strings.Repeat("x", 4<<20)))
			case "exact-wire":
				limit = len(memoryCaptureTestJSON(test, digest))
			}
			memoryCapturePGObject(test, storage, ctx, repo, "memory", hash, root)
			memoryCapturePGObject(test, storage, ctx, repo, "memory_chunk", component, stored)
			if scenario == "exact-wire" {
				memoryCapturePGRead(test, storage, ctx, repo, hash, limit-1, domain.ErrMemoryProjectionLimit)
				actual := memoryCapturePGRead(test, storage, ctx, repo, hash, limit, nil)
				if !reflect.DeepEqual(actual, digest) {
					test.Fatal("exact escaped wire limit changed digest")
				}
			} else {
				memoryCapturePGRead(test, storage, ctx, repo, hash, limit, domain.ErrMemoryProjectionLimit)
			}
		})
	}
}

func TestPGMemoryCaptureCumulativeStoredByteLimit(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	digest, objects, key, limit := memoryCapturePaddedComponents(test)
	for identity, raw := range objects {
		memoryCapturePGObject(test, storage, ctx, repo, identity.kind, identity.hash, raw)
	}
	memoryCapturePGRead(test, storage, ctx, repo, key.hash, limit, domain.ErrMemoryProjectionLimit)
	if actual := memoryCapturePGRead(test, storage, ctx, repo, key.hash, limit*2, nil); !reflect.DeepEqual(actual, digest) {
		test.Fatal("larger stored-byte budget changed PostgreSQL logical memory")
	}
}

func memoryCapturePGPins(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash, prefix string, count int) []domain.PRContributionMemory {
	test.Helper()
	contribution := strings.TrimPrefix(string(domain.HashContent([]byte(string(repo)+prefix))), "sha256:")
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO `+prefix+`_contributions(repo_id,id,payload) VALUES($1,$2,'{}') ON CONFLICT DO NOTHING`, string(repo), contribution)
	pins := make([]domain.PRContributionMemory, count)
	for position := range pins {
		pins[position] = (domain.PRContributionMemory{RepoID: repo, ContributionID: contribution, PublicationID: fmt.Sprintf("%032x", position+1), MemoryPinned: true}).WithID()
	}
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO `+prefix+`_contribution_memories(repo_id,id,contribution_id,publication_id,memory_hash,memory_source,memory_pinned,payload)
 SELECT $1,payload->>'id',payload->>'contribution_id',payload->>'publication_id',payload->>'memory_hash',payload->>'memory_source',(payload->>'memory_pinned')::boolean,payload
 FROM jsonb_array_elements($2::jsonb) AS payload`, string(repo), memoryCaptureTestJSON(test, pins))
	return pins
}

func memoryCapturePGReadPins(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash, expected error) ([]domain.PRContributionMemory, []domain.GitContributionMemory) {
	test.Helper()
	var prPins []domain.PRContributionMemory
	var gitPins []domain.GitContributionMemory
	err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		var err error
		prPins, gitPins, err = storage.ReadMemoryPinsForCapture(bound, repo)
		if !errors.Is(err, expected) || (err != nil && (prPins != nil || gitPins != nil)) {
			test.Fatalf("pin capture: error=%v want=%v counts=%d/%d", err, expected, len(prPins), len(gitPins))
		}
		return nil
	})
	if err != nil {
		test.Fatal(err)
	}
	return prPins, gitPins
}

func TestPGMemoryCapturePinsRoundTripAndCombinedRecordLimit(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	prPins, gitPins := memoryCapturePGReadPins(test, storage, ctx, repo, nil)
	if prPins == nil || gitPins == nil || len(prPins)+len(gitPins) != 0 {
		test.Fatal("empty pin capture")
	}
	memoryCapturePGPins(test, storage, ctx, repo, "pr", maxMemoryCaptureRecords/2)
	memoryCapturePGPins(test, storage, ctx, repo, "git", maxMemoryCaptureRecords/2)
	prPins, gitPins = memoryCapturePGReadPins(test, storage, ctx, repo, nil)
	if len(prPins) != maxMemoryCaptureRecords/2 || len(gitPins) != maxMemoryCaptureRecords/2 {
		test.Fatal("combined exact record boundary", len(prPins), len(gitPins))
	}
	for _, pin := range prPins {
		if pin.RepoID != repo || pin.Validate() != nil {
			test.Fatal("invalid captured PR pin")
		}
	}
	for _, pin := range gitPins {
		if pin.RepoID != repo || pin.Validate() != nil {
			test.Fatal("invalid captured Git pin")
		}
	}
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO git_contribution_memories(repo_id,id,contribution_id,publication_id,memory_hash,memory_source,memory_pinned,payload)
 SELECT repo_id,repeat('f',64),contribution_id,publication_id,memory_hash,memory_source,memory_pinned,payload FROM git_contribution_memories WHERE repo_id=$1 LIMIT 1`, string(repo))
	memoryCapturePGReadPins(test, storage, ctx, repo, domain.ErrMemoryProjectionLimit)
}

func TestPGMemoryCapturePinsByteAndIntegrityLimits(test *testing.T) {
	for _, scenario := range []string{"oversized-pr", "oversized-git", "combined-bytes", "invalid-pin", "wrong-repo", "column-mismatch"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, repo := catalogPG(test)
			prPins := memoryCapturePGPins(test, storage, ctx, repo, "pr", 1)
			memoryCapturePGPins(test, storage, ctx, repo, "git", 1)
			expected := domain.ErrIntegrity
			switch scenario {
			case "oversized-pr", "oversized-git":
				table := strings.TrimPrefix(scenario, "oversized-") + "_contribution_memories"
				catalogExecPG(test, ctx, storage.pool, `UPDATE `+table+` SET payload=payload||jsonb_build_object('padding',repeat('x',$2)) WHERE repo_id=$1`, string(repo), maxMemoryCapturePinBytes)
				expected = domain.ErrMemoryProjectionLimit
			case "combined-bytes":
				for _, prefix := range []string{"pr", "git"} {
					catalogExecPG(test, ctx, storage.pool, `UPDATE `+prefix+`_contribution_memories SET payload=payload||jsonb_build_object('padding',repeat('x',$2)) WHERE repo_id=$1`, string(repo), maxMemoryCapturePinBytes/2)
				}
				expected = domain.ErrMemoryProjectionLimit
			case "invalid-pin":
				catalogExecPG(test, ctx, storage.pool, `UPDATE pr_contribution_memories SET payload='{}' WHERE repo_id=$1`, string(repo))
			case "wrong-repo":
				pin := prPins[0]
				pin.RepoID = domain.HashContent([]byte("different repository"))
				pin = pin.WithID()
				catalogExecPG(test, ctx, storage.pool, `UPDATE pr_contribution_memories SET payload=$2 WHERE repo_id=$1`, string(repo), memoryCaptureTestJSON(test, pin))
			case "column-mismatch":
				catalogExecPG(test, ctx, storage.pool, `UPDATE pr_contribution_memories SET publication_id=repeat('e',32) WHERE repo_id=$1`, string(repo))
			}
			memoryCapturePGReadPins(test, storage, ctx, repo, expected)
		})
	}
}

func TestPGMemoryCapturePinsTransactionFence(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	if _, _, err := storage.ReadMemoryPinsForCapture(ctx, repo); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("unbound pin read", err)
	}
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		_, _, err := storage.ReadMemoryPinsForCapture(bound, repo)
		if !errors.Is(err, domain.ErrConflict) {
			test.Fatal("write transaction pin read", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		peer := &PostgresStore{pool: storage.pool}
		if _, _, err := peer.ReadMemoryPinsForCapture(bound, repo); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("foreign transaction pin read", err)
		}
		canceled, cancel := context.WithCancel(bound)
		cancel()
		if _, _, err := storage.ReadMemoryPinsForCapture(canceled, repo); !errors.Is(err, context.Canceled) {
			test.Fatal("canceled pin read", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}

type memoryCapturePGPinTrace struct {
	pgx.Tx
	copied int
	denied int
	limits []int
}

func (trace *memoryCapturePGPinTrace) Query(ctx context.Context, query string, arguments ...any) (pgx.Rows, error) {
	trace.limits = append(trace.limits, arguments[1].(int))
	rows, err := trace.Tx.Query(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	return &memoryCapturePGPinRows{Rows: rows, trace: trace}, nil
}

type memoryCapturePGPinRows struct {
	pgx.Rows
	trace *memoryCapturePGPinTrace
}

func (rows *memoryCapturePGPinRows) Scan(destinations ...any) error {
	if err := rows.Rows.Scan(destinations...); err != nil {
		return err
	}
	raw := *destinations[len(destinations)-1].(*[]byte)
	if raw == nil {
		rows.trace.denied++
	}
	rows.trace.copied += len(raw)
	return nil
}

func TestPGMemoryCapturePinsSuppressOversizedPayloadBeforeScan(test *testing.T) {
	for _, combined := range []bool{false, true} {
		storage, ctx, repo := catalogPG(test)
		memoryCapturePGPins(test, storage, ctx, repo, "pr", 1)
		padding := maxMemoryCapturePinBytes
		if combined {
			padding /= 2
			memoryCapturePGPins(test, storage, ctx, repo, "git", 1)
			catalogExecPG(test, ctx, storage.pool, `UPDATE git_contribution_memories SET payload=payload||jsonb_build_object('padding',repeat('x',$2)) WHERE repo_id=$1`, string(repo), padding)
		}
		catalogExecPG(test, ctx, storage.pool, `UPDATE pr_contribution_memories SET payload=payload||jsonb_build_object('padding',repeat('x',$2)) WHERE repo_id=$1`, string(repo), padding)
		if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
			original := bound.Value(repositoryTxKey{}).(*repositoryTx)
			trace := &memoryCapturePGPinTrace{Tx: original.Tx}
			traced := context.WithValue(bound, repositoryTxKey{}, &repositoryTx{Tx: trace, owner: storage, readOnly: true})
			prPins, gitPins, err := storage.ReadMemoryPinsForCapture(traced, repo)
			if !errors.Is(err, domain.ErrMemoryProjectionLimit) || prPins != nil || gitPins != nil || trace.denied != 1 || trace.copied > maxMemoryCapturePinBytes {
				test.Fatalf("actual SQL byte fence: combined=%v copied=%d suppressed=%d err=%v", combined, trace.copied, trace.denied, err)
			}
			limits := []int{maxMemoryCaptureRecords + 1}
			if combined {
				limits = append(limits, maxMemoryCaptureRecords)
				if trace.copied == 0 {
					test.Fatal("combined SQL budget fixture did not consume PR bytes")
				}
			} else if trace.copied != 0 {
				test.Fatal("oversized payload reached client", trace.copied)
			}
			if !reflect.DeepEqual(trace.limits, limits) {
				test.Fatal("SQL did not request remaining rows plus one", trace.limits)
			}
			return nil
		}); err != nil {
			test.Fatal(err)
		}
	}
}
