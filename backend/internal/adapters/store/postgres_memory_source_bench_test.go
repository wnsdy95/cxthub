//go:build postgres

package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func BenchmarkPGMemorySourceGrantBatch(bench *testing.B) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		bench.Skip("CXT_TEST_DSN unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	storage, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		bench.Fatal(err)
	}
	defer storage.Close()
	if _, err := storage.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil {
		bench.Fatal(err)
	}
	repo := domain.HashContent([]byte(domain.NewID("memory-source-benchmark-")))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		bench.Fatal(err)
	}
	hashes := make([]string, 1024)
	for index := range hashes {
		hashes[index] = string(domain.HashContent([]byte(fmt.Sprintf("%s/%d", repo, index))))
	}
	if _, err := storage.pool.Exec(ctx, `INSERT INTO blobs(hash,bytes) SELECT hash,$2::bytea FROM unnest($1::text[]) AS fixture(hash)`, hashes, []byte("memory-source-benchmark")); err != nil {
		bench.Fatal(err)
	}
	for _, size := range []int{32, 1024} {
		for _, kind := range []string{"chunk", "memory_chunk"} {
			bench.Run(fmt.Sprintf("%s/%d", kind, size), func(bench *testing.B) {
				bench.ReportAllocs()
				for iteration := 0; iteration < bench.N; iteration++ {
					transaction, err := storage.pool.Begin(ctx)
					if err != nil {
						bench.Fatal(err)
					}
					_, err = transaction.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) SELECT $1,$2,hash FROM unnest($3::text[]) AS fixture(hash)`, string(repo), kind, hashes[:size])
					rollbackErr := transaction.Rollback(ctx)
					if err != nil || rollbackErr != nil {
						bench.Fatalf("grant=%v rollback=%v", err, rollbackErr)
					}
				}
			})
		}
	}
	generation, err := storage.ReadRepositoryMemorySourceGeneration(ctx, repo)
	if err != nil || generation.Sequence != 0 {
		bench.Fatalf("rolled back benchmark advanced generation: %+v %v", generation, err)
	}
}
