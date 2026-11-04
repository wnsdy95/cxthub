//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func displayReferenceBatchImage(test *testing.T, ctx context.Context, storage *PostgresStore, source domain.ContentHash) string {
	test.Helper()
	var image string
	err := storage.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'source',(SELECT to_jsonb(source) FROM doc_display_sources_v1 source WHERE source_hash=$1),
 'objects',(SELECT jsonb_agg(to_jsonb(object) ORDER BY hash) FROM doc_display_objects_v1 object WHERE source_hash=$1),
 'edges',(SELECT jsonb_agg(to_jsonb(edge) ORDER BY parent_hash,child_hash) FROM doc_display_object_edges_v1 edge WHERE source_hash=$1),
 'maintenance',(SELECT to_jsonb(work) FROM doc_display_maintenance_v1 work WHERE source_hash=$1))::text`, source).Scan(&image)
	if err != nil {
		test.Fatal(err)
	}
	return image
}

func TestDocDisplayReferenceBatchPGSwappedParentEdgesRollback(test *testing.T) {
	for _, covered := range []bool{false, true} {
		test.Run(fmt.Sprint(covered), func(test *testing.T) {
			storage, ctx := rootProjectionIsolatedPG(test)
			source, _ := maintenanceSource(test, ctx, storage, "batch parent membership")
			childLeft, childRight := domain.HashContent([]byte("left")), domain.HashContent([]byte("right"))
			left := maintenanceObject(test, map[string]any{"child": childLeft})
			right := maintenanceObject(test, map[string]any{"child": childRight})
			newLeaf := maintenanceObject(test, map[string]any{"text": "uncommitted leaf"})
			for index, object := range []domain.DocRangeObject{left, right} {
				if _, err := storage.pool.Exec(ctx, `INSERT INTO doc_display_objects_v1(source_hash,hash,bytes) VALUES($1,$2,$3)`, source, object.Hash, object.Bytes); err != nil {
					test.Fatal(err)
				}
				if _, err := storage.pool.Exec(ctx, `INSERT INTO doc_display_object_edges_v1(source_hash,parent_hash,child_hash) VALUES($1,$2,$3)`, source, object.Hash, []domain.ContentHash{childRight, childLeft}[index]); err != nil {
					test.Fatal(err)
				}
			}
			if covered {
				if _, err := storage.pool.Exec(ctx, `UPDATE doc_display_objects_v1 SET references_version=1 WHERE source_hash=$1`, source); err != nil {
					test.Fatal(err)
				}
			}
			test.Log("Injected malformed private reference index keeps per-parent counts and the global child set unchanged; it is not a valid published state.")
			before := displayReferenceBatchImage(test, ctx, storage, source)
			transaction, err := storage.pool.Begin(ctx)
			if err != nil {
				test.Fatal(err)
			}
			err = putDocDisplayObjects(ctx, transaction, source, []domain.DocRangeObject{left, newLeaf, right})
			rollbackPG(transaction)
			if !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("equal counts masked changed parent-child membership", covered, err)
			}
			if after := displayReferenceBatchImage(test, ctx, storage, source); after != before {
				test.Fatal("rejected batch leaked object, coverage, edge or source-revision effects")
			}
		})
	}
}

func TestDocDisplayReferenceBatchPGMixedReplayLegacyAndDuplicates(test *testing.T) {
	storage, ctx := rootProjectionIsolatedPG(test)
	source, _ := maintenanceSource(test, ctx, storage, "mixed batch")
	covered := maintenanceObject(test, map[string]any{"child": domain.HashContent([]byte("covered digest"))})
	legacy := maintenanceObject(test, map[string]any{"text": "legacy leaf"})
	fresh := maintenanceObject(test, map[string]any{"child": covered.Hash})
	maintenancePut(test, ctx, storage, source, covered)
	if _, err := storage.pool.Exec(ctx, `INSERT INTO doc_display_objects_v1(source_hash,hash,bytes) VALUES($1,$2,$3)`, source, legacy.Hash, legacy.Bytes); err != nil {
		test.Fatal(err)
	}
	identity := func() string {
		var value string
		if err := storage.pool.QueryRow(ctx, `SELECT xmin::text||':'||ctid::text FROM doc_display_objects_v1 WHERE source_hash=$1 AND hash=$2`, source, covered.Hash).Scan(&value); err != nil {
			test.Fatal(err)
		}
		return value
	}
	before := identity()
	maintenancePut(test, ctx, storage, source, covered, legacy, fresh, covered, fresh)
	if identity() != before {
		test.Fatal("covered replay rewrote an existing immutable object")
	}
	var count, indexed, edges int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE references_version=1),
 (SELECT count(*) FROM doc_display_object_edges_v1 WHERE source_hash=$1)
 FROM doc_display_objects_v1 WHERE source_hash=$1`, source).Scan(&count, &indexed, &edges); err != nil || count != 3 || indexed != 3 || edges != 2 {
		test.Fatal("mixed batch lost leaves, membership or deduplication", count, indexed, edges, err)
	}
}

func TestDocDisplayReferenceBatchPGCollisionRollsBackNewSiblings(test *testing.T) {
	storage, ctx := rootProjectionIsolatedPG(test)
	source, _ := maintenanceSource(test, ctx, storage, "batch byte collision")
	object := maintenanceObject(test, map[string]any{"text": "correct bytes"})
	newLeaf := maintenanceObject(test, map[string]any{"text": "new leaf"})
	if _, err := storage.pool.Exec(ctx, `INSERT INTO doc_display_objects_v1(source_hash,hash,bytes) VALUES($1,$2,$3)`, source, object.Hash, []byte(`{}`)); err != nil {
		test.Fatal(err)
	}
	before := displayReferenceBatchImage(test, ctx, storage, source)
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	err = putDocDisplayObjects(ctx, transaction, source, []domain.DocRangeObject{newLeaf, object})
	rollbackPG(transaction)
	if !errors.Is(err, domain.ErrIntegrity) || displayReferenceBatchImage(test, ctx, storage, source) != before {
		test.Fatal("batch replay overwrote a collision or committed a sibling", err)
	}
}
