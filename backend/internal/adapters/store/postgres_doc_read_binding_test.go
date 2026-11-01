//go:build postgres

package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestDocReadBindingDefensiveValues(test *testing.T) {
	for _, version := range []int64{0, 1, 1<<63 - 1} {
		test.Run(fmt.Sprint(version), func(test *testing.T) {
			want := domain.DocReadGenerationBinding{SourceID: "4b94be7c-fae7-4c49-8ee5-bd944755d1ce", SourceVersion: version, RangeGeneration: domain.HashContent([]byte(test.Name()))}
			if err := want.Validate(); err != nil {
				test.Fatal("valid source binding rejected", want, err)
			}
			ranges := &postgresDocRangeGenerationRead{binding: want}
			visible := &postgresDocVisibleGenerationRead{binding: want}
			for _, reader := range []interface {
				Binding() domain.DocReadGenerationBinding
			}{ranges, visible} {
				changed := reader.Binding()
				if changed != want {
					test.Fatal("binding changed source version or generation", changed, want)
				}
				changed.SourceID = "changed"
				changed.SourceVersion = -1
				changed.RangeGeneration = "changed"
				if reader.Binding() != want {
					test.Fatal("binding exposed private state", reader.Binding(), changed)
				}
			}
		})
	}
}

func TestDocReadBindingValidation(test *testing.T) {
	valid := domain.DocReadGenerationBinding{
		SourceID:        "4b94be7c-fae7-4c49-8ee5-bd944755d1ce",
		RangeGeneration: domain.HashContent([]byte(test.Name())),
	}
	for _, sourceID := range []string{"", "not-a-uuid", "00000000-0000-0000-0000-000000000000", "4b94be7cfae74c498ee5bd944755d1ce", "4b94be7c-fae7-4c49-8ee5-bd944755d1cg"} {
		binding := valid
		binding.SourceID = sourceID
		if err := binding.Validate(); !errors.Is(err, domain.ErrIntegrity) {
			test.Fatal("invalid source ID accepted", sourceID, err)
		}
	}
	for _, binding := range []domain.DocReadGenerationBinding{
		{},
		{SourceID: valid.SourceID, SourceVersion: -1, RangeGeneration: valid.RangeGeneration},
		{SourceID: valid.SourceID},
		{SourceID: valid.SourceID, RangeGeneration: "invalid"},
	} {
		if err := binding.Validate(); !errors.Is(err, domain.ErrIntegrity) {
			test.Fatal("invalid binding accepted", binding, err)
		}
	}
}

func TestDocReadBindingPGMigrationAudit(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	var payloadColumn, generationColumn bool
	if err := storage.pool.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='blobs'::regclass AND attname='doc_range_source_id' AND NOT attisdropped),
 EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='doc_range_generations_v2'::regclass AND attname='generation_id' AND atttypid='uuid'::regtype AND attnotnull AND NOT attisdropped)`).Scan(&payloadColumn, &generationColumn); err != nil {
		test.Fatal(err)
	}
	if payloadColumn || !generationColumn {
		test.Fatal("incarnation migration altered payload schema or omitted generation UUID", payloadColumn, generationColumn)
	}
	for _, name := range []string{
		"0095_doc_range_generation.sql",
		"0096_doc_range_boundaries.sql",
		"0097_doc_visible_generations.sql",
		"0098_doc_read_source_incarnation.sql",
	} {
		raw, err := os.ReadFile("../../../../schemas/db/migrations/" + name)
		if err != nil {
			test.Fatal(err)
		}
		var checksum string
		if err := storage.pool.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version=$1`, name).Scan(&checksum); err != nil {
			test.Fatal(err)
		}
		if want := fmt.Sprintf("%x", sha256.Sum256(raw)); checksum != want {
			test.Fatal("migration audit checksum changed", name, checksum, want)
		}
	}
	if count, err := storage.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil || count != 0 {
		test.Fatal("migration replay was not idempotent", count, err)
	}
}

func TestDocReadBindingPGGenerationIDCannotBeReplaced(test *testing.T) {
	storage, ctx, repo, fixture := docVisiblePGFixture(test, 1)
	var sourceID string
	if err := storage.pool.QueryRow(ctx, `SELECT generation_id::text FROM doc_range_generations_v2 WHERE doc_hash=$1`, fixture.hash).Scan(&sourceID); err != nil {
		test.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE doc_range_generations_v2 SET generation_id=gen_random_uuid() WHERE doc_hash=$1`,
		`UPDATE doc_range_generations_v2 SET generation_id=DEFAULT WHERE doc_hash=$1`,
		`UPDATE doc_range_generations_v2 SET generation_id=NULL WHERE doc_hash=$1`,
		`UPDATE doc_range_generations_v2 SET generation_id='00000000-0000-0000-0000-000000000000' WHERE doc_hash=$1`,
	} {
		if _, err := storage.pool.Exec(ctx, statement, fixture.hash); err != nil {
			test.Fatal("ordinary source update failed", statement, err)
		}
		var currentID string
		var version int64
		if err := storage.pool.QueryRow(ctx, `SELECT generation_id::text,source_version FROM doc_range_generations_v2 WHERE doc_hash=$1`, fixture.hash).Scan(&currentID, &version); err != nil {
			test.Fatal(err)
		}
		if currentID != sourceID || version != 0 {
			test.Fatal("update replaced generation incarnation or changed version fence", currentID, sourceID, version)
		}
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET hash=hash WHERE hash=$1`, fixture.hash); err != nil {
		test.Fatal(err)
	}
	if err := storage.WithinRepository(ctx, repo, func(write context.Context) error {
		return storage.ensureRangeGeneration(write, repo, fixture.hash)
	}); err != nil {
		test.Fatal(err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		ranges, err := storage.OpenDocRangeGeneration(bound, repo, fixture.hash)
		if err != nil {
			return err
		}
		visible, err := storage.OpenDocVisibleGeneration(bound, repo, fixture.hash)
		if err != nil {
			return err
		}
		if ranges.Binding().SourceID != sourceID || ranges.Binding().SourceVersion != 1 || ranges.Binding() != visible.Binding() {
			test.Fatal("republished reader lost immutable source ID", ranges.Binding(), visible.Binding(), sourceID)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}

func TestDocReadBindingPGDeleteReinsertChangesIncarnation(test *testing.T) {
	storage, ctx, repo, fixture := docVisiblePGFixture(test, 129)
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		ranges, err := storage.OpenDocRangeGeneration(bound, repo, fixture.hash)
		if err != nil {
			return err
		}
		visible, err := storage.OpenDocVisibleGeneration(bound, repo, fixture.hash)
		if err != nil {
			return err
		}
		before := ranges.Binding()
		if err := before.Validate(); err != nil {
			return err
		}
		if visible.Binding() != before || before.SourceVersion != 0 {
			test.Fatal("initial binding differs", before, visible.Binding())
		}
		if _, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, repo, fixture.hash); err != nil {
			return err
		}
		if _, err := storage.pool.Exec(ctx, `DELETE FROM blobs WHERE hash=$1`, fixture.hash); err != nil {
			return err
		}
		seedRootPG(test, ctx, storage, repo, fixture)
		if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
			return err
		}
		if err := storage.WithinReadSnapshot(ctx, func(fresh context.Context) error {
			nextRanges, err := storage.OpenDocRangeGeneration(fresh, repo, fixture.hash)
			if err != nil {
				return err
			}
			nextVisible, err := storage.OpenDocVisibleGeneration(fresh, repo, fixture.hash)
			if err != nil {
				return err
			}
			after := nextRanges.Binding()
			if err := after.Validate(); err != nil {
				return err
			}
			if nextVisible.Binding() != after || before.SourceID == after.SourceID || before.SourceVersion != after.SourceVersion || before.RangeGeneration != after.RangeGeneration {
				test.Fatal("delete/reinsert did not change only the source incarnation", before, after, nextVisible.Binding())
			}
			if !reflect.DeepEqual(ranges.Descriptor(), nextRanges.Descriptor()) || !reflect.DeepEqual(visible.Descriptor(), nextVisible.Descriptor()) {
				test.Fatal("same-content reincarnation changed descriptors")
			}
			return nil
		}); err != nil {
			return err
		}
		if ranges.Binding() != before || visible.Binding() != before {
			test.Fatal("replacement incarnation escaped the original snapshot")
		}
		pinnedRanges, err := storage.OpenDocRangeGeneration(bound, repo, fixture.hash)
		if err != nil {
			return err
		}
		pinnedVisible, err := storage.OpenDocVisibleGeneration(bound, repo, fixture.hash)
		if err != nil {
			return err
		}
		if pinnedRanges.Binding() != before || pinnedVisible.Binding() != before {
			test.Fatal("reopened reader combined source incarnations across snapshots")
		}
		if _, err := ranges.ReadPage(bound, 1); err != nil {
			return err
		}
		_, _, err = visible.Unit(bound, "all", 128, false)
		return err
	}); err != nil {
		test.Fatal(err)
	}
}

func TestDocReadBindingPGSameContentSourceChange(test *testing.T) {
	for _, mutation := range []string{"same root bytes", "root metadata"} {
		test.Run(mutation, func(test *testing.T) {
			storage, ctx, repo, fixture := docVisiblePGFixture(test, 129)
			if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
				ranges, err := storage.OpenDocRangeGeneration(bound, repo, fixture.hash)
				if err != nil {
					return err
				}
				visible, err := storage.OpenDocVisibleGeneration(bound, repo, fixture.hash)
				if err != nil {
					return err
				}
				rangeDescriptor, visibleDescriptor := ranges.Descriptor(), visible.Descriptor()
				raw, err := rangeDescriptor.Encode()
				if err != nil {
					return err
				}
				var sourceID string
				if err := storage.db(bound).QueryRow(bound, `SELECT generation_id::text FROM doc_range_generations_v2 WHERE doc_hash=$1`, fixture.hash).Scan(&sourceID); err != nil {
					return err
				}
				before := domain.DocReadGenerationBinding{SourceID: sourceID, SourceVersion: 0, RangeGeneration: domain.HashContent(raw)}
				if err := before.Validate(); err != nil {
					return err
				}
				if ranges.Binding() != before || visible.Binding() != before || visibleDescriptor.RangeGeneration != before.RangeGeneration {
					test.Fatal("readers did not bind the same initial source and validated range descriptor", ranges.Binding(), visible.Binding(), before)
				}
				if mutation == "same root bytes" {
					_, err = storage.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.hash)
				} else {
					_, err = storage.pool.Exec(ctx, `UPDATE blobs SET hash=hash WHERE hash=$1`, fixture.hash)
				}
				if err != nil {
					return err
				}
				if err := storage.WithinReadSnapshot(ctx, func(fresh context.Context) error {
					if read, err := storage.OpenDocRangeGeneration(fresh, repo, fixture.hash); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) || read != nil {
						test.Fatal("fresh range reader accepted a stale source incarnation", read, err)
					}
					if read, err := storage.OpenDocVisibleGeneration(fresh, repo, fixture.hash); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) || read != nil {
						test.Fatal("fresh visible reader accepted a stale source incarnation", read, err)
					}
					return nil
				}); err != nil {
					return err
				}
				if err := storage.WithinRepository(ctx, repo, func(write context.Context) error {
					return storage.ensureRangeGeneration(write, repo, fixture.hash)
				}); err != nil {
					return err
				}
				if err := storage.WithinReadSnapshot(ctx, func(fresh context.Context) error {
					nextRanges, err := storage.OpenDocRangeGeneration(fresh, repo, fixture.hash)
					if err != nil {
						return err
					}
					nextVisible, err := storage.OpenDocVisibleGeneration(fresh, repo, fixture.hash)
					if err != nil {
						return err
					}
					after := nextRanges.Binding()
					if err := after.Validate(); err != nil {
						return err
					}
					if after.SourceVersion != before.SourceVersion+1 || after.RangeGeneration != before.RangeGeneration || nextVisible.Binding() != after || after == before {
						test.Fatal("same-content rebuild did not invalidate the old binding", nextRanges.Binding(), nextVisible.Binding(), before)
					}
					if (after.SourceID == before.SourceID) != (mutation == "root metadata") {
						test.Fatal("generation incarnation did not follow row lifetime", mutation, before, after)
					}
					if !reflect.DeepEqual(nextRanges.Descriptor(), rangeDescriptor) || !reflect.DeepEqual(nextVisible.Descriptor(), visibleDescriptor) {
						test.Fatal("source-incarnation change altered content-addressed descriptors")
					}
					return nil
				}); err != nil {
					return err
				}
				pinnedRanges, err := storage.OpenDocRangeGeneration(bound, repo, fixture.hash)
				if err != nil {
					return err
				}
				pinnedVisible, err := storage.OpenDocVisibleGeneration(bound, repo, fixture.hash)
				if err != nil {
					return err
				}
				if ranges.Binding() != before || visible.Binding() != before || pinnedRanges.Binding() != before || pinnedVisible.Binding() != before {
					test.Fatal("current-source change escaped the pinned snapshot")
				}
				if entries, err := ranges.ReadPage(bound, 1); err != nil || len(entries) != 1 {
					test.Fatal("pinned range objects changed", entries, err)
				}
				if unit, ordinal, err := visible.Unit(bound, "all", 128, false); err != nil || ordinal != 128 || unit.From != 128 {
					test.Fatal("pinned visible objects changed", unit, ordinal, err)
				}
				return nil
			}); err != nil {
				test.Fatal(err)
			}
		})
	}
}

func TestDocReadBindingPGReaderScopes(test *testing.T) {
	storage, ctx, repo, fixture := docVisiblePGFixture(test, 1)
	foreign, _ := rootPGRepo(test, ctx, storage)
	peer := &PostgresStore{pool: storage.pool}
	checkOpen := func(bound context.Context, owner *PostgresStore, repository domain.ContentHash, want error) {
		test.Helper()
		if read, err := owner.OpenDocRangeGeneration(bound, repository, fixture.hash); !errors.Is(err, want) || read != nil {
			test.Fatal("range binding escaped reader scope", read, err, want)
		}
		if read, err := owner.OpenDocVisibleGeneration(bound, repository, fixture.hash); !errors.Is(err, want) || read != nil {
			test.Fatal("visible binding escaped reader scope", read, err, want)
		}
	}
	checkOpen(ctx, storage, repo, domain.ErrConflict)
	if err := storage.WithinRepository(ctx, repo, func(write context.Context) error {
		checkOpen(write, storage, repo, domain.ErrConflict)
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	for _, options := range []pgx.TxOptions{
		{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly},
		{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly},
		{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadWrite},
	} {
		transaction, err := storage.pool.BeginTx(ctx, options)
		if err != nil {
			test.Fatal(err)
		}
		bound := context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: storage, readOnly: true})
		checkOpen(bound, storage, repo, domain.ErrConflict)
		rollbackPG(transaction)
	}
	var retainedRanges outbound.DocRangeGenerationRead
	var retainedVisible outbound.DocVisibleGenerationRead
	var retainedContext context.Context
	var binding domain.DocReadGenerationBinding
	checkReads := func(bound context.Context, want error) {
		test.Helper()
		_, hashErr := retainedRanges.PageHash(bound, 0)
		_, pageErr := retainedRanges.ReadPage(bound, 0)
		_, chunkErr := retainedRanges.Chunk(bound, 0)
		_, _, unitErr := retainedVisible.Unit(bound, "all", 0, false)
		for _, err := range []error{hashErr, pageErr, chunkErr, unitErr} {
			if !errors.Is(err, want) {
				test.Fatal("bound reader changed its scope fence", err, want)
			}
		}
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		checkOpen(bound, peer, repo, domain.ErrConflict)
		checkOpen(bound, storage, foreign, domain.ErrNotFound)
		var err error
		retainedRanges, err = storage.OpenDocRangeGeneration(bound, repo, fixture.hash)
		if err != nil {
			return err
		}
		retainedVisible, err = storage.OpenDocVisibleGeneration(bound, repo, fixture.hash)
		if err != nil {
			return err
		}
		retainedContext, binding = bound, retainedRanges.Binding()
		if retainedVisible.Binding() != binding {
			test.Fatal("range and visible readers disagree on their binding")
		}
		checkReads(bound, nil)
		checkReads(ctx, domain.ErrConflict)
		if err := peer.WithinReadSnapshot(ctx, func(foreign context.Context) error {
			checkReads(foreign, domain.ErrConflict)
			return nil
		}); err != nil {
			return err
		}
		canceled, cancel := context.WithCancel(bound)
		cancel()
		checkOpen(canceled, storage, repo, context.Canceled)
		checkReads(canceled, context.Canceled)
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	checkOpen(retainedContext, storage, repo, pgx.ErrTxClosed)
	checkReads(retainedContext, pgx.ErrTxClosed)
	if retainedRanges.Binding() != binding || retainedVisible.Binding() != binding {
		test.Fatal("closing the reader changed its captured binding value")
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		checkReads(bound, domain.ErrConflict)
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}
