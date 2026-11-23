//go:build postgres

package store

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocReadBindingDefensiveValues(test *testing.T) {
	for _, version := range []int64{0, 1, 1<<63 - 1} {
		test.Run(fmt.Sprint(version), func(test *testing.T) {
			want := domain.DocReadGenerationBinding{SourceID: "4b94be7c-fae7-4c49-8ee5-bd944755d1ce", SourceVersion: version, RangeGeneration: domain.HashContent([]byte(test.Name()))}
			if err := want.Validate(); err != nil {
				test.Fatal("valid source binding rejected", want, err)
			}
			ranges := &postgresDocRangeGenerationRead{binding: want}
			for _, reader := range []interface {
				Binding() domain.DocReadGenerationBinding
			}{ranges} {
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
