//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocVisiblePGDescriptorIntegrityAndStaleness(t *testing.T) {
	for _, damage := range []string{"hash", "hash bound", "document", "range binding", "events", "noncanonical", "range descriptor", "range hash", "source version", "range source version", "missing visible", "missing range"} {
		t.Run(damage, func(t *testing.T) {
			storage, ctx, repo, fixture := docVisiblePGFixture(t, 1)
			generation, err := storage.readVisibleGeneration(ctx, repo, fixture.hash)
			if err != nil {
				t.Fatal(err)
			}
			want := domain.ErrIntegrity
			switch damage {
			case "document":
				generation.Hash = domain.HashContent([]byte("foreign document"))
			case "range binding":
				generation.RangeGeneration = domain.HashContent([]byte("foreign range"))
			case "events":
				generation.Events++
				root := generation.Views["all"]
				root.Units++
				generation.Views["all"] = root
			}
			raw, err := generation.Encode()
			if err != nil {
				t.Fatal(err)
			}
			if damage == "noncanonical" {
				raw = append(raw, '\n')
			}
			hash := domain.HashContent(raw)
			if damage == "hash" {
				hash = fixture.hash
			}
			if damage == "hash bound" {
				hash += domain.ContentHash(strings.Repeat("a", 4096))
			}
			if _, err := storage.pool.Exec(ctx, `UPDATE doc_visible_generations_v1 SET descriptor_hash=$2,descriptor=$3 WHERE doc_hash=$1`, fixture.hash, hash, raw); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "range descriptor":
				_, err = storage.pool.Exec(ctx, `UPDATE doc_range_generations_v2 SET descriptor=$2 WHERE doc_hash=$1`, fixture.hash, []byte("{}"))
			case "range hash":
				_, err = storage.pool.Exec(ctx, `UPDATE doc_visible_generations_v1 SET range_generation_hash=$2 WHERE doc_hash=$1`, fixture.hash, fixture.hash)
				want = domain.ErrDocRangeIndexUnavailable
			case "source version":
				_, err = storage.pool.Exec(ctx, `UPDATE doc_visible_generations_v1 SET source_version=source_version+1 WHERE doc_hash=$1`, fixture.hash)
				want = domain.ErrDocRangeIndexUnavailable
			case "range source version":
				_, err = storage.pool.Exec(ctx, `UPDATE doc_range_generations_v2 SET source_version=source_version+1 WHERE doc_hash=$1`, fixture.hash)
				want = domain.ErrDocRangeIndexUnavailable
			case "missing visible":
				_, err = storage.pool.Exec(ctx, `DELETE FROM doc_visible_generations_v1 WHERE doc_hash=$1`, fixture.hash)
				want = domain.ErrDocRangeIndexUnavailable
			case "missing range":
				_, err = storage.pool.Exec(ctx, `DELETE FROM doc_range_generations_v2 WHERE doc_hash=$1`, fixture.hash)
				want = domain.ErrDocRangeIndexUnavailable
			}
			if err != nil {
				t.Fatal(err)
			}
			docVisiblePGRead(t, storage, ctx, repo, fixture.hash, want)
		})
	}
}

func TestDocVisiblePGSelectedObjectIntegrity(t *testing.T) {
	for _, damage := range []string{"missing", "bytes", "oversized", "decode", "unselected"} {
		t.Run(damage, func(t *testing.T) {
			storage, ctx, repo, fixture := docVisiblePGFixture(t, 257)
			generation, err := storage.readVisibleGeneration(ctx, repo, fixture.hash)
			if err != nil {
				t.Fatal(err)
			}
			var selected []domain.ContentHash
			_, _, err = domain.ReadDocVisibleUnit(ctx, generation, "all", 0, false, func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
				selected = append(selected, hash)
				var raw []byte
				err := storage.pool.QueryRow(ctx, `SELECT bytes FROM doc_range_objects_v2 WHERE doc_hash=$1 AND hash=$2`, fixture.hash, hash).Scan(&raw)
				return raw, err
			})
			if err != nil || len(selected) != 2 {
				t.Fatal("selected path", selected, err)
			}
			leaf := selected[1]
			switch damage {
			case "missing":
				_, err = storage.pool.Exec(ctx, `DELETE FROM doc_range_objects_v2 WHERE doc_hash=$1 AND hash=$2`, fixture.hash, leaf)
			case "bytes", "unselected":
				_, err = storage.pool.Exec(ctx, `UPDATE doc_range_objects_v2 SET bytes=$3 WHERE doc_hash=$1 AND hash=$2`, fixture.hash, leaf, []byte("{}"))
			case "oversized":
				_, err = storage.pool.Exec(ctx, `UPDATE doc_range_objects_v2 SET bytes=$3 WHERE doc_hash=$1 AND hash=$2`, fixture.hash, leaf, bytes.Repeat([]byte(" "), domain.MaxDocVisibleObjectBytes+1))
			case "decode":
				raw := []byte("{}")
				hash := domain.HashContent(raw)
				_, err = storage.pool.Exec(ctx, `INSERT INTO doc_range_objects_v2(doc_hash,hash,bytes) VALUES($1,$2,$3)`, fixture.hash, hash, raw)
				if err != nil {
					t.Fatal(err)
				}
				var directory []byte
				if err := storage.pool.QueryRow(ctx, `SELECT bytes FROM doc_range_objects_v2 WHERE doc_hash=$1 AND hash=$2`, fixture.hash, selected[0]).Scan(&directory); err != nil {
					t.Fatal(err)
				}
				directory = bytes.ReplaceAll(directory, []byte(leaf), []byte(hash))
				root := generation.Views["all"]
				root.Hash = domain.HashContent(directory)
				generation.Views["all"] = root
				if _, err := storage.pool.Exec(ctx, `INSERT INTO doc_range_objects_v2(doc_hash,hash,bytes) VALUES($1,$2,$3)`, fixture.hash, root.Hash, directory); err != nil {
					t.Fatal(err)
				}
				descriptor, encodeErr := generation.Encode()
				if encodeErr != nil {
					t.Fatal(encodeErr)
				}
				_, err = storage.pool.Exec(ctx, `UPDATE doc_visible_generations_v1 SET descriptor_hash=$2,descriptor=$3 WHERE doc_hash=$1`, fixture.hash, domain.HashContent(descriptor), descriptor)
			}
			if err != nil {
				t.Fatal(err)
			}
			if damage == "unselected" {
				if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
					read, err := storage.OpenDocVisibleGeneration(bound, repo, fixture.hash)
					if err != nil {
						return err
					}
					_, _, err = read.Unit(bound, "all", 256, false)
					return err
				}); err != nil {
					t.Fatal("read touched unselected corruption", err)
				}
			}
			docVisiblePGRead(t, storage, ctx, repo, fixture.hash, domain.ErrIntegrity)
		})
	}
}

func TestDocVisiblePGEmptyDocument(t *testing.T) {
	storage, ctx, repo, fixture := docVisiblePGFixture(t, 0)
	docVisiblePGRead(t, storage, ctx, repo, fixture.hash, nil)
	var count int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM doc_range_objects_v2 WHERE doc_hash=$1`, fixture.hash).Scan(&count); err != nil || count != 0 {
		t.Fatal("empty document emitted objects", count, err)
	}
}

func TestDocVisiblePGObjectLimitDoesNotRelaxRangeLimit(t *testing.T) {
	storage, ctx, repo, fixture := docVisiblePGFixture(t, 1)
	raw := bytes.Repeat([]byte("a"), domain.MaxDocVisibleObjectBytes)
	hash := domain.HashContent(raw)
	if _, err := storage.pool.Exec(ctx, `INSERT INTO doc_range_objects_v2(doc_hash,hash,bytes) VALUES($1,$2,$3)`, fixture.hash, hash, raw); err != nil {
		t.Fatal(err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		opened, err := storage.OpenDocVisibleGeneration(bound, repo, fixture.hash)
		if err != nil {
			return err
		}
		read := opened.(*postgresDocVisibleGenerationRead)
		loaded, err := read.loadObject(bound, hash)
		if err != nil || !bytes.Equal(loaded, raw) {
			t.Fatal("32KiB visible object rejected", err)
		}
		if _, err := read.objects.loadDirectory(bound, hash); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatal("visible cache bypassed 16KiB range directory limit", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
