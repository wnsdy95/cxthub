//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestLegacyBytePreparePGBoundaryRechecksFailedValidation(test *testing.T) {
	for _, repaired := range []bool{false, true} {
		test.Run(fmt.Sprintf("repaired=%t", repaired), func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteStagePGSource(test, storage, ctx, 128<<10)
			job := legacyByteStagePGClaim(test, storage, ctx, fixture)
			damaged := bytes.Replace(fixture.raw, []byte(`"seq":2`), []byte(`"seq":3`), 1)
			if bytes.Equal(damaged, fixture.raw) || domain.HashContent(damaged) == fixture.hash {
				test.Fatal("fixture did not preserve a hash-mismatched source")
			}
			var version int64
			if err := storage.pool.QueryRow(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1 RETURNING doc_range_source_version`, fixture.hash, damaged).Scan(&version); err != nil {
				test.Fatal(err)
			}
			work, cancel := context.WithCancel(ctx)
			paused := &legacyBytePrepareBoundaryContext{Context: work, entered: make(chan struct{}), resume: make(chan struct{})}
			type result struct {
				session legacyByteBackfillSession
				err     error
			}
			results, stopped := make(chan result, 1), make(chan struct{})
			test.Cleanup(func() {
				cancel()
				paused.unblock()
				<-stopped
			})
			go func() {
				defer close(stopped)
				session, err := storage.prepareLegacyByteBackfill(paused, job)
				results <- result{session: session, err: err}
			}()
			select {
			case <-paused.entered:
			case <-stopped:
				test.Fatal("invalid source did not reach the actual hash checkpoint", (<-results).err)
			case <-ctx.Done():
				test.Fatal("hash checkpoint was not reached", ctx.Err())
			}
			if repaired {
				transaction, err := storage.pool.Begin(ctx)
				if err != nil {
					test.Fatal(err)
				}
				defer rollbackPG(transaction)
				if _, err := transaction.Exec(ctx, `SET LOCAL lock_timeout='250ms'`); err != nil {
					test.Fatal(err)
				}
				var next int64
				if err := transaction.QueryRow(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1 RETURNING doc_range_source_version`, fixture.hash, fixture.raw).Scan(&next); err != nil || next != version+1 {
					test.Fatal("repair did not replace the exact source while old verification was paused", next, version, err)
				}
				if err := transaction.Commit(ctx); err != nil {
					test.Fatal(err)
				}
			}
			private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
			public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			paused.unblock()
			var got result
			select {
			case got = <-results:
				<-stopped
			case <-ctx.Done():
				test.Fatal("failed verification did not finish", ctx.Err())
			}
			want := domain.ErrIntegrity
			if repaired {
				want = domain.ErrConflict
			}
			if !errors.Is(got.err, want) || !reflect.ValueOf(got.session).IsZero() {
				test.Fatalf("validation outcome was assigned to the wrong source generation: got=%v want=%v", got.err, want)
			}
			if legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) != private || legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
				test.Fatal("failed validation changed private progress, source, publication or job state")
			}
		})
	}
}
