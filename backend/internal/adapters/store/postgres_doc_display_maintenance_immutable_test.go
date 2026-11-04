//go:build postgres

package store

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocDisplayMaintenancePGObjectImmutability(test *testing.T) {
	storage, ctx := rootProjectionIsolatedPG(test)
	source, _ := maintenanceSource(test, ctx, storage, "immutable objects")
	otherSource, _ := maintenanceSource(test, ctx, storage, "immutable objects foreign source")
	child := maintenanceObject(test, map[string]any{"text": "original child"})
	otherChild := maintenanceObject(test, map[string]any{"text": "replacement child"})
	object := maintenanceObject(test, map[string]any{"child": child.Hash})
	replacement := maintenanceObject(test, map[string]any{"child": otherChild.Hash})
	maintenancePut(test, ctx, storage, source, child, otherChild)
	if _, err := storage.pool.Exec(ctx, `INSERT INTO doc_display_objects_v1(source_hash,hash,bytes) VALUES($1,$2,$3)`, source, object.Hash, object.Bytes); err != nil {
		test.Fatal(err)
	}
	for _, stage := range []struct {
		name     string
		coverage int16
		edges    []domain.ContentHash
	}{
		{name: "unindexed"},
		{name: "indexed", coverage: 1, edges: []domain.ContentHash{child.Hash}},
	} {
		test.Run(stage.name, func(test *testing.T) {
			if stage.coverage == 1 {
				maintenancePut(test, ctx, storage, source, object)
			}
			assertUnchanged := func(test *testing.T) {
				test.Helper()
				var raw []byte
				var coverage int16
				if err := storage.pool.QueryRow(ctx, `SELECT bytes,references_version FROM doc_display_objects_v1
 WHERE source_hash=$1 AND hash=$2`, source, object.Hash).Scan(&raw, &coverage); err != nil {
					test.Fatal("original object missing", err)
				}
				if !bytes.Equal(raw, object.Bytes) || coverage != stage.coverage {
					test.Fatal("original object bytes or reference coverage changed", string(raw), coverage)
				}
				rows, err := storage.pool.Query(ctx, `SELECT child_hash FROM doc_display_object_edges_v1
 WHERE source_hash=$1 AND parent_hash=$2 ORDER BY child_hash`, source, object.Hash)
				if err != nil {
					test.Fatal(err)
				}
				defer rows.Close()
				var edges []domain.ContentHash
				for rows.Next() {
					var hash domain.ContentHash
					if err := rows.Scan(&hash); err != nil {
						test.Fatal(err)
					}
					edges = append(edges, hash)
				}
				if err := rows.Err(); err != nil {
					test.Fatal(err)
				}
				if !slices.Equal(edges, stage.edges) {
					test.Fatal("original reference edges changed", edges, stage.edges)
				}
			}
			assertUnchanged(test)
			for _, mutation := range []struct {
				name      string
				statement string
				arguments []any
			}{
				{name: "hash", statement: `UPDATE doc_display_objects_v1 SET hash=$3 WHERE source_hash=$1 AND hash=$2`, arguments: []any{replacement.Hash}},
				{name: "bytes", statement: `UPDATE doc_display_objects_v1 SET bytes=$3 WHERE source_hash=$1 AND hash=$2`, arguments: []any{replacement.Bytes}},
				{name: "source", statement: `UPDATE doc_display_objects_v1 SET source_hash=$3 WHERE source_hash=$1 AND hash=$2`, arguments: []any{otherSource}},
				{name: "hash_and_bytes", statement: `UPDATE doc_display_objects_v1 SET hash=$3,bytes=$4 WHERE source_hash=$1 AND hash=$2`, arguments: []any{replacement.Hash, replacement.Bytes}},
			} {
				test.Run(mutation.name, func(test *testing.T) {
					arguments := append([]any{source, object.Hash}, mutation.arguments...)
					_, err := storage.pool.Exec(ctx, mutation.statement, arguments...)
					var postgresError interface{ SQLState() string }
					if !errors.As(err, &postgresError) || postgresError.SQLState() != "23514" {
						test.Fatal("object mutation must fail with SQLSTATE 23514", err)
					}
					assertUnchanged(test)
				})
			}
			for _, replay := range []struct {
				name      string
				statement string
				arguments []any
			}{
				{name: "identical_update", statement: `UPDATE doc_display_objects_v1 SET source_hash=$1,hash=$2,bytes=$3 WHERE source_hash=$1 AND hash=$2`, arguments: []any{source, object.Hash, object.Bytes}},
				{name: "upsert_replay", statement: `INSERT INTO doc_display_objects_v1(source_hash,hash,bytes) VALUES($1,$2,$3)
 ON CONFLICT(source_hash,hash) DO UPDATE SET source_hash=EXCLUDED.source_hash,hash=EXCLUDED.hash,bytes=EXCLUDED.bytes`, arguments: []any{source, object.Hash, object.Bytes}},
				{name: "coverage_only", statement: `UPDATE doc_display_objects_v1 SET references_version=references_version WHERE source_hash=$1 AND hash=$2`, arguments: []any{source, object.Hash}},
			} {
				test.Run(replay.name, func(test *testing.T) {
					tag, err := storage.pool.Exec(ctx, replay.statement, replay.arguments...)
					if err != nil || tag.RowsAffected() != 1 {
						test.Fatal("unchanged object update must remain allowed", tag.RowsAffected(), err)
					}
					assertUnchanged(test)
				})
			}
		})
	}
}
