//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestLegacyByteStagePGCorruptSelectedObjectNeverReturnsContent(test *testing.T) {
	for _, damage := range []string{"hash", "short", "oversized"} {
		test.Run(damage, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteStagePGSource(test, storage, ctx, 512<<10)
			job := legacyByteStagePGClaim(test, storage, ctx, fixture)
			stage, err := storage.stageLegacyByteBackfill(ctx, job, 8)
			if err != nil || !stage.complete {
				test.Fatal(stage, err)
			}
			public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			body, descriptor := legacyByteStagePGBlock(fixture, 0)
			body = bytes.Clone(body)
			switch damage {
			case "hash":
				body[0] ^= 1
			case "short":
				body = body[:len(body)-1]
			case "oversized":
				body = append(body, bytes.Repeat([]byte{'x'}, domain.ConversationManifestChunkBytes)...)
			}
			transaction, err := storage.pool.Begin(ctx)
			if err != nil {
				test.Fatal(err)
			}
			defer func() { _ = transaction.Rollback(context.WithoutCancel(ctx)) }()
			rows, err := transaction.Query(ctx, `SELECT conname,pg_get_constraintdef(oid) FROM pg_constraint
 WHERE conrelid='doc_legacy_byte_stage_objects_v1'::regclass AND contype='c' ORDER BY conname`)
			if err != nil {
				test.Fatal(err)
			}
			var constraints []struct{ name, definition string }
			for rows.Next() {
				var constraint struct{ name, definition string }
				if err := rows.Scan(&constraint.name, &constraint.definition); err != nil {
					test.Fatal(err)
				}
				constraints = append(constraints, constraint)
			}
			rows.Close()
			if rows.Err() != nil || len(constraints) != 2 {
				test.Fatal("unexpected owned fixture constraints", constraints, rows.Err())
			}
			for _, constraint := range constraints {
				if _, err := transaction.Exec(ctx, `ALTER TABLE doc_legacy_byte_stage_objects_v1 DROP CONSTRAINT `+pgx.Identifier{constraint.name}.Sanitize()); err != nil {
					test.Fatal(err)
				}
			}
			if _, err := transaction.Exec(ctx, `ALTER TABLE doc_legacy_byte_stage_objects_v1 DISABLE TRIGGER guard_legacy_byte_stage_objects`); err != nil {
				test.Fatal(err)
			}
			if _, err := transaction.Exec(ctx, `UPDATE doc_legacy_byte_stage_objects_v1 SET bytes=$3 WHERE doc_hash=$1 AND hash=$2`, fixture.hash, descriptor.Hash, body); err != nil {
				test.Fatal(err)
			}
			if _, err := transaction.Exec(ctx, `ALTER TABLE doc_legacy_byte_stage_objects_v1 ENABLE TRIGGER guard_legacy_byte_stage_objects`); err != nil {
				test.Fatal(err)
			}
			for _, constraint := range constraints {
				if _, err := transaction.Exec(ctx, `ALTER TABLE doc_legacy_byte_stage_objects_v1 ADD CONSTRAINT `+pgx.Identifier{constraint.name}.Sanitize()+` `+constraint.definition+` NOT VALID`); err != nil {
					test.Fatal(err)
				}
			}
			if err := transaction.Commit(ctx); err != nil {
				test.Fatal(err)
			}
			defer func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				for _, statement := range []string{`DELETE FROM snapshots WHERE doc_hash=$1`, `DELETE FROM repo_blobs WHERE hash=$1`, `DELETE FROM blobs WHERE hash=$1`} {
					if _, err := storage.pool.Exec(cleanup, statement, fixture.hash); err != nil {
						test.Error(err)
					}
				}
				for _, constraint := range constraints {
					if _, err := storage.pool.Exec(cleanup, `ALTER TABLE doc_legacy_byte_stage_objects_v1 VALIDATE CONSTRAINT `+pgx.Identifier{constraint.name}.Sanitize()); err != nil {
						test.Error(err)
					}
				}
			}()
			if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
				got, err := storage.readLegacyByteStageBlock(read, fixture.repo, fixture.hash, stage, 0, descriptor)
				if !errors.Is(err, domain.ErrIntegrity) || len(got) != 0 {
					test.Fatal("corrupt selected cache bytes escaped validation", damage, len(got), err)
				}
				return nil
			}); err != nil {
				test.Fatal(err)
			}
			if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
				test.Fatal("corrupt derived data changed the original or its public generation")
			}
		})
	}
}
