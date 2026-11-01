//go:build postgres

package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const docDisplayCatalogRetirementBatch = domain.DocDisplayMaintenanceBatch / 4

func retireDocDisplayCatalog(ctx context.Context, transaction pgx.Tx, source domain.ContentHash, epoch string, result *domain.DocDisplayMaintenanceStep) error {
	if _, err := transaction.Exec(ctx, `SELECT cxthub_bind_doc_catalog_collector($1,$2::uuid)`, source, epoch); err != nil {
		return err
	}
	var phase, scope string
	if err := transaction.QueryRow(ctx, `SELECT retire_phase,retire_scope FROM doc_display_catalog_lifecycle_v1
 WHERE source_hash=$1 AND catalog_epoch=$2::uuid AND state='retiring'`, source, epoch).Scan(&phase, &scope); err != nil {
		return mapNoRows(err)
	}
	var err error
	finishing := phase == "empty"
	switch phase {
	case "orders":
		scope, err = retirementDocDisplayScope(ctx, transaction, `SELECT scope FROM doc_tool_order_sources_v1
 WHERE source_hash=$1 ORDER BY scope LIMIT 1`, source, scope)
		if err == nil && scope == "" {
			phase = "keys"
		} else if err == nil {
			var deleted int64
			deleted, err = deleteDocDisplayCatalogRows(ctx, transaction, `DELETE FROM doc_tool_order_plans_v1
 WHERE scope=$1 AND parent_from IN (SELECT parent_from FROM doc_tool_order_plans_v1 WHERE scope=$1 ORDER BY parent_from LIMIT $2)`, scope)
			result.CatalogDeleted = int(deleted)
			if err == nil && deleted == 0 {
				err = deleteDocDisplayCatalogHeader(ctx, transaction, "doc_tool_order_sources_v1", scope, result)
				scope = ""
			}
		}
	case "keys":
		scope, phase, err = retireDocDisplayKeyScope(ctx, transaction, source, scope, result)
	case "trees":
		scope, err = retirementDocDisplayScope(ctx, transaction, `SELECT scope FROM doc_tool_tree_sources_v1
 WHERE source_hash=$1 ORDER BY scope LIMIT 1`, source, scope)
		if err == nil && scope == "" {
			phase = "empty"
		} else if err == nil {
			var deleted int64
			deleted, err = deleteDocDisplayCatalogRows(ctx, transaction, `DELETE FROM doc_tool_tree_nodes_v1
 WHERE scope=$1 AND node_from IN (SELECT node_from FROM doc_tool_tree_nodes_v1 WHERE scope=$1 ORDER BY node_from LIMIT $2)`, scope)
			result.CatalogDeleted = int(deleted)
			if err == nil && deleted == 0 {
				err = deleteDocDisplayCatalogHeader(ctx, transaction, "doc_tool_tree_sources_v1", scope, result)
				scope = ""
			}
		}
	case "empty":
		_, err = transaction.Exec(ctx, `SELECT cxthub_finish_doc_catalog_retirement($1,$2::uuid)`, source, epoch)
	default:
		err = domain.ErrIntegrity
	}
	if err != nil {
		return err
	}
	if !finishing {
		if _, err := transaction.Exec(ctx, `UPDATE doc_display_catalog_lifecycle_v1 SET retire_phase=$3,retire_scope=$4
 WHERE source_hash=$1 AND catalog_epoch=$2::uuid AND state='retiring'`, source, epoch, phase, scope); err != nil {
			return err
		}
	}
	result.Phase, result.CatalogPhase = "catalog_retirement", phase
	_, err = transaction.Exec(ctx, `UPDATE doc_display_maintenance_v1 SET due_at=clock_timestamp() WHERE source_hash=$1`, source)
	return err
}

func retireDocDisplayKeyScope(ctx context.Context, transaction pgx.Tx, source domain.ContentHash, after string, result *domain.DocDisplayMaintenanceStep) (string, string, error) {
	var tree, scope string
	err := transaction.QueryRow(ctx, `SELECT scope FROM doc_tool_tree_sources_v1
 WHERE source_hash=$1 AND scope>$2 ORDER BY scope LIMIT 1`, source, after).Scan(&tree)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "trees", nil
	}
	if err != nil {
		return "", "", err
	}
	err = transaction.QueryRow(ctx, `SELECT scope FROM doc_tool_key_sources_v1
 WHERE tree_scope=$1 ORDER BY scope LIMIT 1`, tree).Scan(&scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return tree, "keys", nil
	}
	if err != nil {
		return "", "", err
	}
	if err := retireDocDisplayKeyGroup(ctx, transaction, scope, result); err != nil {
		return "", "", err
	}
	if result.CatalogDeleted == 0 {
		if err := deleteDocDisplayCatalogHeader(ctx, transaction, "doc_tool_key_sources_v1", scope, result); err != nil {
			return "", "", err
		}
	}
	return after, "keys", nil
}

func retirementDocDisplayScope(ctx context.Context, transaction pgx.Tx, query string, source domain.ContentHash, scope string) (string, error) {
	if scope != "" {
		return scope, ctx.Err()
	}
	err := transaction.QueryRow(ctx, query, source).Scan(&scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return scope, err
}

func deleteDocDisplayCatalogRows(ctx context.Context, transaction pgx.Tx, query, scope string) (int64, error) {
	tag, err := transaction.Exec(ctx, query, scope, docDisplayCatalogRetirementBatch)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() < 0 || tag.RowsAffected() > docDisplayCatalogRetirementBatch {
		return 0, domain.ErrIntegrity
	}
	return tag.RowsAffected(), nil
}

func deleteDocDisplayCatalogHeader(ctx context.Context, transaction pgx.Tx, table, scope string, result *domain.DocDisplayMaintenanceStep) error {
	tag, err := transaction.Exec(ctx, `DELETE FROM `+table+` WHERE scope=$1`, scope)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrIntegrity
	}
	result.CatalogDeleted++
	return nil
}

func retireDocDisplayKeyGroup(ctx context.Context, transaction pgx.Tx, scope string, result *domain.DocDisplayMaintenanceStep) error {
	var parent, first, last int64
	var digest string
	err := transaction.QueryRow(ctx, `SELECT parent_from,digest,first_from,last_from FROM doc_tool_key_members_v1
 WHERE scope=$1 ORDER BY parent_from,digest LIMIT 1`, scope).Scan(&parent, &digest, &first, &last)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var endpoints bool
	if err := transaction.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM doc_tool_key_observations_v1 WHERE scope=$1 AND node_from=$4 AND parent_from=$2 AND digest=$3)
 AND EXISTS(SELECT 1 FROM doc_tool_key_observations_v1 WHERE scope=$1 AND node_from=$5 AND parent_from=$2 AND digest=$3)`, scope, parent, digest, first, last).Scan(&endpoints); err != nil {
		return err
	}
	if !endpoints {
		return domain.ErrIntegrity
	}
	tag, err := transaction.Exec(ctx, `DELETE FROM doc_tool_key_observations_v1 WHERE scope=$1 AND node_from IN
 (SELECT node_from FROM doc_tool_key_observations_v1 WHERE scope=$1 AND parent_from=$2 AND digest=$3
 AND node_from<>$4 AND node_from<>$5 ORDER BY node_from LIMIT $6)`, scope, parent, digest, first, last, docDisplayCatalogRetirementBatch)
	if err != nil {
		return err
	}
	if tag.RowsAffected() < 0 || tag.RowsAffected() > docDisplayCatalogRetirementBatch {
		return domain.ErrIntegrity
	}
	result.CatalogDeleted = int(tag.RowsAffected())
	if result.CatalogDeleted != 0 {
		return nil
	}
	tag, err = transaction.Exec(ctx, `DELETE FROM doc_tool_key_members_v1 WHERE scope=$1 AND parent_from=$2 AND digest=$3`, scope, parent, digest)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrIntegrity
	}
	tag, err = transaction.Exec(ctx, `DELETE FROM doc_tool_key_observations_v1 WHERE scope=$1 AND node_from IN ($2,$3)`, scope, first, last)
	if err != nil {
		return err
	}
	expected := int64(2)
	if first == last {
		expected = 1
	}
	if tag.RowsAffected() != expected {
		return domain.ErrIntegrity
	}
	result.CatalogDeleted = int(expected) + 1
	return nil
}
