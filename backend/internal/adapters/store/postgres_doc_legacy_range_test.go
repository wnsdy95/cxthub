//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyRangeTestRow struct {
	values []any
	err    error
}

func (row legacyRangeTestRow) Scan(destinations ...any) error {
	if row.err != nil {
		return row.err
	}
	if len(destinations) != len(row.values) {
		return fmt.Errorf("unexpected scan columns: %d, want %d", len(destinations), len(row.values))
	}
	for position, value := range row.values {
		destination := reflect.ValueOf(destinations[position]).Elem()
		if value == nil {
			destination.SetZero()
		} else {
			destination.Set(reflect.ValueOf(value))
		}
	}
	return nil
}

type legacyRangeTestRows struct {
	pgx.Rows
	rows     []legacyRangeTestRow
	position int
	err      error
}

func (rows *legacyRangeTestRows) Next() bool {
	if rows.position >= len(rows.rows) {
		return false
	}
	rows.position++
	return true
}

func (rows *legacyRangeTestRows) Scan(destinations ...any) error {
	return rows.rows[rows.position-1].Scan(destinations...)
}

func (rows *legacyRangeTestRows) Close()     {}
func (rows *legacyRangeTestRows) Err() error { return rows.err }

type legacyRangeTestBatchResult struct {
	pgx.BatchResults
	err error
}

func (result legacyRangeTestBatchResult) Close() error { return result.err }

type legacyRangeTestTx struct {
	pgx.Tx
	test  *testing.T
	row   func(string, []any) pgx.Row
	query func(string, []any) (pgx.Rows, error)
	exec  func(string, []any) (pgconn.CommandTag, error)
	batch func(*pgx.Batch) error
}

func (transaction *legacyRangeTestTx) QueryRow(_ context.Context, statement string, arguments ...any) pgx.Row {
	transaction.test.Helper()
	if transaction.row == nil {
		transaction.test.Fatal("unexpected row query", statement)
	}
	return transaction.row(statement, arguments)
}

func (transaction *legacyRangeTestTx) Query(_ context.Context, statement string, arguments ...any) (pgx.Rows, error) {
	transaction.test.Helper()
	if transaction.query == nil {
		transaction.test.Fatal("unexpected rows query", statement)
	}
	return transaction.query(statement, arguments)
}

func (transaction *legacyRangeTestTx) Exec(_ context.Context, statement string, arguments ...any) (pgconn.CommandTag, error) {
	transaction.test.Helper()
	if transaction.exec == nil {
		transaction.test.Fatal("unexpected write", statement)
	}
	return transaction.exec(statement, arguments)
}

func (transaction *legacyRangeTestTx) SendBatch(_ context.Context, batch *pgx.Batch) pgx.BatchResults {
	transaction.test.Helper()
	if transaction.batch == nil {
		transaction.test.Fatal("unexpected batch", batch.Len())
	}
	return legacyRangeTestBatchResult{err: transaction.batch(batch)}
}

func legacyRangeTestContext(test *testing.T) (*PostgresStore, context.Context, *legacyRangeTestTx, domain.ContentHash) {
	test.Helper()
	storage := &PostgresStore{}
	repo := domain.HashContent([]byte(test.Name()))
	transaction := &legacyRangeTestTx{test: test}
	ctx := context.WithValue(context.Background(), repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: storage, repo: repo})
	return storage, ctx, transaction, repo
}

func TestLegacyDocRangePublicationOnlyWritesBoundedMetadata(test *testing.T) {
	for _, count := range []int{0, domain.DocRangePageEvents*129 + 1} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			storage, ctx, transaction, repo := legacyRangeTestContext(test)
			verified := readBlockDoc(test, test.Name(), "", count)
			before := verified.Bytes()
			var generation domain.DocRangeGeneration
			writes := 0
			transaction.exec = func(statement string, arguments []any) (pgconn.CommandTag, error) {
				writes++
				if writes == 1 {
					if !strings.HasPrefix(statement, "INSERT INTO doc_range_generations_v2") ||
						!strings.Contains(statement, "source.doc_range_source_version") ||
						!strings.Contains(statement, "owned.repo_id=$1 AND owned.kind='doc' AND owned.hash=$2") ||
						strings.Contains(statement, "generation_id=") || arguments[0] != repo || arguments[1] != verified.Hash() {
						test.Fatal("missing source, ownership, or incarnation fence", statement, arguments)
					}
					var err error
					generation, err = domain.DecodeDocRangeGeneration(verified.Hash(), arguments[2].(domain.ContentHash), arguments[3].([]byte))
					if err != nil {
						test.Fatal(err)
					}
				} else if writes != 2 || statement != "DELETE FROM doc_range_objects_v2 WHERE doc_hash=$1" || arguments[0] != verified.Hash() {
					test.Fatal("publication touched source, v1, or visible state", statement)
				}
				return pgconn.NewCommandTag("INSERT 0 1"), nil
			}
			objects := make(map[domain.ContentHash][]byte)
			batches := 0
			transaction.batch = func(batch *pgx.Batch) error {
				batches++
				if batch.Len() == 0 || batch.Len() > 128 {
					test.Fatal("unbounded legacy metadata batch", batch.Len())
				}
				for _, query := range batch.QueuedQueries {
					if query.SQL != "INSERT INTO doc_range_objects_v2(doc_hash,hash,bytes) VALUES($1,$2,$3) ON CONFLICT DO NOTHING" || query.Arguments[0] != verified.Hash() {
						test.Fatal("batch wrote non-metadata state", query)
					}
					hash, body := query.Arguments[1].(domain.ContentHash), query.Arguments[2].([]byte)
					if len(body) == 0 || len(body) > domain.MaxDocRangePageBytes || domain.HashContent(body) != hash {
						test.Fatal("invalid legacy metadata object", hash, len(body))
					}
					objects[hash] = bytes.Clone(body)
				}
				return nil
			}
			if err := storage.putLegacyRangeGeneration(ctx, repo, verified); err != nil {
				test.Fatal(err)
			}
			if writes != 2 || generation.Hash != verified.Hash() || generation.Version != 3 || generation.Identity() != domain.DocumentIdentityLegacy ||
				generation.SourceIdentity == nil || generation.ChunkCount() != 0 || generation.ChunkPages != (domain.DocRangeDirectoryRoot{}) ||
				generation.EventCount != int64(count) || !bytes.Equal(before, verified.Bytes()) {
				test.Fatal("publication changed the legacy source identity", generation, writes)
			}
			if count > 0 && batches < 2 {
				test.Fatal("fixture did not exercise bounded batch publication", batches)
			}
			seen := 0
			for position := 0; position < generation.EventPages.Count; position++ {
				hash, err := domain.ResolveDocRangeLeaf(ctx, generation.EventPages, position, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
					return objects[hash], nil
				})
				if err != nil {
					test.Fatal("directory object was not published", err)
				}
				entries, err := generation.DecodePage(position, hash, objects[hash])
				if err != nil {
					test.Fatal("page object was not published", err)
				}
				seen += len(entries)
			}
			if seen != count {
				test.Fatal("legacy metadata lost events", seen, count)
			}
		})
	}
}

func TestLegacyDocRangePublicationRejectsInvalidProofOrContext(test *testing.T) {
	storage, ctx, _, repo := legacyRangeTestContext(test)
	legacy := readBlockDoc(test, test.Name(), "", 1)
	root := rootPublicationProof(test, rootFixture(test, "root"))
	for _, verified := range []domain.VerifiedSessionDoc{{}, root} {
		if err := storage.putLegacyRangeGeneration(ctx, repo, verified); !errors.Is(err, domain.ErrIntegrity) {
			test.Fatal("unverified or Root input admitted", err)
		}
	}
	foreign := &PostgresStore{}
	for _, invalid := range []context.Context{
		context.Background(), outbound.WithDocReadOnly(ctx),
		context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{owner: storage, repo: repo, readOnly: true}),
		context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{owner: storage, repo: domain.HashContent([]byte("wrong repo"))}),
		context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{owner: foreign, repo: repo}),
	} {
		if err := storage.putLegacyRangeGeneration(invalid, repo, legacy); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("unfenced legacy publication admitted", err)
		}
		if err := storage.ensureLegacyRangeGeneration(invalid, repo, legacy.Hash()); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("unfenced legacy build admitted", err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := storage.putLegacyRangeGeneration(canceled, repo, legacy); !errors.Is(err, context.Canceled) {
		test.Fatal("canceled publication admitted", err)
	}
}

func TestLegacyDocRangeReadinessIsOnlyOwnedVersionedPresence(test *testing.T) {
	for _, present := range []bool{false, true} {
		test.Run(fmt.Sprint(present), func(test *testing.T) {
			storage, ctx, transaction, repo := legacyRangeTestContext(test)
			hash := domain.HashContent([]byte(test.Name()))
			transaction.row = func(statement string, arguments []any) pgx.Row {
				if statement == docRangeJobReceiptSQL {
					if !reflect.DeepEqual(arguments, []any{repo, hash, domain.DocumentIdentityRootV1, domain.DocumentIdentityLegacy}) {
						test.Fatal("range receipt did not admit both typed sources", arguments)
					}
					now := time.Now()
					return legacyRangeTestRow{values: []any{repo, hash, domain.ContentHash(""), "completed", "", 1, int64(2), now, now, (*time.Time)(nil)}}
				}
				if statement != docRangeReadySQL || !strings.Contains(statement, "generation.source_version=root.doc_range_source_version") ||
					strings.Contains(statement, "descriptor") || strings.Contains(statement, ".bytes") || len(arguments) != 2 {
					test.Fatal("readiness read body/proof or bypassed the version fence", statement, arguments)
				}
				return legacyRangeTestRow{values: []any{present}}
			}
			if ready, err := storage.HasDocRangeIndex(ctx, repo, hash); err != nil || ready != present {
				test.Fatal("readiness did not preserve its presence-only contract", ready, err)
			}
			job, err := storage.docRangeJobReceipt(ctx, repo, hash)
			if present && (err != nil || job.State != "completed") || !present && !errors.Is(err, domain.ErrNotFound) {
				test.Fatal("receipt did not track current generation presence", job, err)
			}
		})
	}
}

func TestLegacyDocRangeBuildVerifiesOriginalLargeSourceBeforePublication(test *testing.T) {
	for _, damage := range []string{"none", "source", "wrong generation identity"} {
		test.Run(damage, func(test *testing.T) {
			storage, ctx, transaction, repo := legacyRangeTestContext(test)
			verified := readBlockDoc(test, test.Name(), strings.Repeat("legacy", maxStoredRangeRootBytes/6+1), 1)
			raw := verified.Bytes()
			if len(raw) <= maxStoredRangeRootBytes {
				test.Fatal("fixture is not larger than the Root read bound")
			}
			if damage == "source" {
				raw = bytes.Replace(raw, []byte("legacy"), []byte("broken"), 1)
			}
			reads := 0
			transaction.row = func(statement string, arguments []any) pgx.Row {
				switch {
				case strings.HasPrefix(statement, "SELECT source.doc_range_source_version,substring(source.bytes"):
					reads++
					if !strings.HasSuffix(statement, "FOR UPDATE OF owned,source") || arguments[2] != 2*maxLegacyReadBytes+1 {
						test.Fatal("legacy source was not locked before verification", statement)
					}
					return legacyRangeTestRow{values: []any{int64(7), raw}}
				case strings.HasPrefix(statement, "SELECT generation.generation_id::text"):
					if reads == 0 {
						test.Fatal("metadata was consulted before locking original source")
					}
					if damage == "wrong generation identity" {
						generation, _, err := domain.BuildLegacyDocRangeGeneration(ctx, verified)
						if err != nil {
							test.Fatal(err)
						}
						generation.Version, generation.SourceIdentity = 2, nil
						generation.ChunkPages = domain.DocRangeDirectoryRoot{Hash: domain.HashContent([]byte("wrong scheme chunks")), Count: (generation.ChunkCount() + domain.DocRangeChunkPageSize - 1) / domain.DocRangeChunkPageSize}
						descriptor, err := generation.Encode()
						if err != nil {
							test.Fatal(err)
						}
						incarnation, hash := "11111111-2222-3333-4444-555555555555", domain.HashContent(descriptor)
						return legacyRangeTestRow{values: []any{&incarnation, int64(7), &hash, descriptor}}
					}
					return legacyRangeTestRow{values: []any{nil, int64(7), nil, []byte(nil)}}
				default:
					test.Fatal("unexpected legacy build query", statement, arguments)
					return legacyRangeTestRow{err: domain.ErrConflict}
				}
			}
			writes := 0
			transaction.exec = func(statement string, _ []any) (pgconn.CommandTag, error) {
				writes++
				if !strings.HasPrefix(statement, "INSERT INTO doc_range_generations_v2") && statement != "DELETE FROM doc_range_objects_v2 WHERE doc_hash=$1" {
					test.Fatal("builder rewrote source or published visible state", statement)
				}
				return pgconn.NewCommandTag("INSERT 0 1"), nil
			}
			transaction.batch = func(*pgx.Batch) error { return nil }
			err := storage.ensureLegacyRangeGeneration(ctx, repo, verified.Hash())
			if damage == "none" {
				if err != nil || writes != 2 {
					test.Fatal("valid large legacy source was truncated or not published", err, writes)
				}
			} else if !errors.Is(err, domain.ErrIntegrity) || writes != 0 {
				test.Fatal("damaged original source or wrong-scheme evidence was overwritten", err, writes)
			}
			if reads != 1 {
				test.Fatal("legacy source was not read through its bounded source lock", reads)
			}
		})
	}
}

func TestLegacyDocRangeChunkLocksAreOwnedSortedAndBounded(test *testing.T) {
	_, ctx, transaction, repo := legacyRangeTestContext(test)
	hashes := make([]domain.ContentHash, 133)
	for position := range hashes {
		hashes[position] = domain.HashContent([]byte(fmt.Sprint(position)))
	}
	manifest := domain.DocChunkManifest{Format: domain.ChunkFormatV2, Envelope: json.RawMessage(`{"cir_version":"1"}`), Chunks: append(slices.Clone(hashes), hashes[0], hashes[0])}
	slices.Sort(hashes)
	var locked []domain.ContentHash
	transaction.query = func(statement string, arguments []any) (pgx.Rows, error) {
		batch := arguments[1].([]domain.ContentHash)
		if len(batch) == 0 || len(batch) > 128 || arguments[0] != repo ||
			!strings.Contains(statement, "owned.repo_id=$1 AND owned.kind='chunk' AND owned.hash=ANY($2::text[])") ||
			!strings.HasSuffix(statement, "ORDER BY chunk.hash FOR SHARE OF owned,chunk") {
			test.Fatal("legacy chunks were not locked through current ownership", statement, arguments)
		}
		locked = append(locked, batch...)
		return &legacyRangeTestRows{rows: make([]legacyRangeTestRow, len(batch))}, nil
	}
	if err := lockLegacyRangeChunks(ctx, ctx.Value(repositoryTxKey{}).(*repositoryTx), repo, manifest); err != nil {
		test.Fatal(err)
	}
	if !slices.Equal(locked, hashes) {
		test.Fatal("chunk locks were duplicated or unordered", locked)
	}
	transaction.query = func(string, []any) (pgx.Rows, error) { return &legacyRangeTestRows{}, nil }
	if err := lockLegacyRangeChunks(ctx, ctx.Value(repositoryTxKey{}).(*repositoryTx), repo, manifest); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("missing owned chunk was not rejected", err)
	}
}

func TestLegacyDocRangeBuildStreamsCurrentV2Chunks(test *testing.T) {
	for _, damaged := range []bool{false, true} {
		test.Run(fmt.Sprint(damaged), func(test *testing.T) {
			storage, ctx, transaction, repo := legacyRangeTestContext(test)
			storage.ConfigureLegacyByteRangePublication(true)
			verified := readBlockDoc(test, test.Name(), strings.Repeat("stream tail", domain.ChunkTarget/4), domain.DocRangePageEvents+1)
			plan, chunked := verified.ChunkPlan()
			if !chunked || len(plan.Manifest.Chunks) < 2 {
				test.Fatal("fixture is not a multi-chunk v2 source")
			}
			source, err := json.Marshal(plan.Manifest)
			if err != nil {
				test.Fatal(err)
			}
			transaction.query = func(statement string, arguments []any) (pgx.Rows, error) {
				if !strings.HasSuffix(statement, "ORDER BY chunk.hash FOR SHARE OF owned,chunk") || arguments[0] != repo {
					test.Fatal("streamed source chunks were not locked in order", statement, arguments)
				}
				return &legacyRangeTestRows{rows: make([]legacyRangeTestRow, len(arguments[1].([]domain.ContentHash)))}, nil
			}
			chunkReads := 0
			transaction.row = func(statement string, arguments []any) pgx.Row {
				switch {
				case strings.HasPrefix(statement, "SELECT source.doc_range_source_version,substring(source.bytes"):
					return legacyRangeTestRow{values: []any{int64(5), docCompress(source)}}
				case strings.HasPrefix(statement, "SELECT generation.generation_id::text"):
					return legacyRangeTestRow{values: []any{nil, int64(5), nil, []byte(nil)}}
				case strings.HasPrefix(statement, "SELECT substring(b.bytes FROM 1 FOR $3)"):
					if !strings.HasSuffix(statement, "FOR SHARE OF rb,b") || arguments[2] != 2*maxLegacyReadBytes+1 {
						test.Fatal("streaming verifier did not use the bounded locked chunk reader", statement)
					}
					chunkReads++
					body := plan.Bodies[arguments[1].(domain.ContentHash)]
					if damaged {
						body = []byte("corrupt current chunk")
					}
					return legacyRangeTestRow{values: []any{docCompress(body)}}
				default:
					test.Fatal("streaming path attempted a materialized document read", statement)
					return legacyRangeTestRow{err: domain.ErrConflict}
				}
			}
			writes := 0
			var published []byte
			transaction.exec = func(statement string, arguments []any) (pgconn.CommandTag, error) {
				writes++
				if writes == 1 && strings.HasPrefix(statement, "INSERT INTO doc_range_generations_v2") {
					published = bytes.Clone(arguments[3].([]byte))
				} else if writes != 2 || statement != "DELETE FROM doc_range_objects_v2 WHERE doc_hash=$1" {
					test.Fatal("streamed legacy build modified source or visible state", statement)
				}
				return pgconn.NewCommandTag("INSERT 0 1"), nil
			}
			transaction.batch = func(*pgx.Batch) error { return nil }
			err = storage.ensureLegacyRangeGeneration(ctx, repo, verified.Hash())
			if damaged {
				if !errors.Is(err, domain.ErrIntegrity) || writes != 0 {
					test.Fatal("corrupt streamed bytes published metadata", err, writes)
				}
				return
			}
			if err != nil || chunkReads != len(plan.Manifest.Chunks) || writes != 2 {
				test.Fatal("streaming verification did not cover every current source occurrence", err, chunkReads, writes)
			}
			generation, _, err := domain.BuildLegacyDocRangeGeneration(ctx, verified)
			if err != nil {
				test.Fatal(err)
			}
			expected, err := generation.Encode()
			if err != nil {
				test.Fatal(err)
			}
			current, err := domain.DecodeDocRangeGeneration(verified.Hash(), domain.HashContent(published), published)
			if err != nil || current.Version != 4 || current.ChunkCount() != len(plan.Manifest.Chunks) || current.ChunkPages.Count == 0 {
				test.Fatal("streamed source did not publish an original chunk directory", current, err)
			}
			current.Version, current.ChunkPages = 3, domain.DocRangeDirectoryRoot{}
			metadata, err := current.Encode()
			if err != nil || !bytes.Equal(metadata, expected) {
				test.Fatal("streamed metadata changed the original legacy descriptor", err)
			}
		})
	}
}

func TestLegacyDocRangeBoundaryTypedSourcesAndIncarnationFence(test *testing.T) {
	storage, ctx, transaction, repo := legacyRangeTestContext(test)
	legacy, _, err := domain.BuildLegacyDocRangeGeneration(ctx, readBlockDoc(test, test.Name(), "", 1))
	if err != nil {
		test.Fatal(err)
	}
	index, err := domain.BuildDocRangeIndex(ctx, rootPublicationProof(test, rootFixture(test, "root")))
	if err != nil {
		test.Fatal(err)
	}
	root, _, err := domain.BuildDocRangeGeneration(ctx, index)
	if err != nil {
		test.Fatal(err)
	}
	for _, pair := range [][2]domain.DocRangeGeneration{{root, legacy}, {legacy, root}, {legacy, legacy}} {
		mismatch := false
		transaction.query = func(statement string, arguments []any) (pgx.Rows, error) {
			if !strings.Contains(statement, "snapshot.doc_identity IN ($3,$6)") || strings.Contains(statement, "root.bytes") ||
				arguments[3] != domain.MaxDocRangeGenerationBytes+1 || arguments[5] != domain.DocumentIdentityLegacy {
				test.Fatal("boundary sources were not typed bounded metadata", statement, arguments)
			}
			rows := &legacyRangeTestRows{}
			for position, generation := range pair {
				if position == 1 && generation.Hash == pair[0].Hash {
					continue
				}
				raw, err := generation.Encode()
				if err != nil {
					test.Fatal(err)
				}
				identity := generation.Identity()
				if mismatch && position == 0 {
					if identity == domain.DocumentIdentityLegacy {
						identity = domain.DocumentIdentityRootV1
					} else {
						identity = domain.DocumentIdentityLegacy
					}
				}
				incarnation, hash := "11111111-2222-3333-4444-555555555555", domain.HashContent(raw)
				rows.rows = append(rows.rows, legacyRangeTestRow{values: []any{generation.Hash, int64(3), identity, &incarnation, &hash, raw}})
			}
			return rows, nil
		}
		sources, err := storage.readDocRangeBoundarySources(ctx, repo, pair[0].Hash, pair[1].Hash)
		if err != nil || sources[0].generation.Identity() != pair[0].Identity() || sources[1].generation.Identity() != pair[1].Identity() {
			test.Fatal("mixed typed sources could not be read", sources, err)
		}
		if !sources[0].sameGeneration(sources[0]) {
			test.Fatal("same generation failed its publication fence")
		}
		changed := sources[0]
		changed.generationID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		if sources[0].sameGeneration(changed) {
			test.Fatal("delete/reinsert with the same descriptor bypassed the incarnation fence")
		}
		mismatch = true
		if _, err := storage.readDocRangeBoundarySources(ctx, repo, pair[0].Hash, pair[1].Hash); !errors.Is(err, domain.ErrIntegrity) {
			test.Fatal("boundary trusted a mismatched snapshot identity", err)
		}
	}
}
