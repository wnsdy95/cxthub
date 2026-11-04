//go:build postgres

package store

import (
	"bytes"
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docToolTreeCatalogRow struct {
	owner domain.ContentHash
	raw   []byte
	state string
	count *int
	from  int
	to    int
}

type docToolOrderCatalogRead struct {
	owner  *repositoryTx
	source domain.ContentHash
	tree   domain.DocToolTreeSource
	keys   domain.DocToolKeyMapSource
	orders domain.DocToolOrdersSource
	plans  int
}

func (reader *postgresDocEventDisplayWork) readToolOrderCatalog(ctx context.Context, tree domain.DocToolTreeSource, keys domain.DocToolKeyMapSource, source domain.DocToolOrdersSource) (int, error) {
	if err := reader.bindToolOrders(ctx, tree, keys); err != nil {
		return 0, err
	}
	if *reader.orders != source {
		return 0, domain.ErrConflict
	}
	var proof *docToolOrderCatalogRead
	if reader.orderCatalog != nil {
		proof = reader.orderCatalog.Load()
	}
	if proof != nil {
		if err := reader.checkTransaction(ctx); err != nil {
			return 0, err
		}
		if proof.owner != reader.owner || proof.source != reader.source || proof.tree != tree || proof.keys != keys || proof.orders != source {
			return 0, domain.ErrConflict
		}
		return proof.plans, nil
	}
	database := reader.storage.db(ctx)
	if err := validateDocToolKeyCatalog(ctx, database, reader.source, tree, keys); err != nil {
		return 0, err
	}
	state, count, err := readDocToolOrdersSet(ctx, database, reader.source, tree, keys, source, false)
	if err == domain.ErrNotFound || err == nil && state != "complete" {
		return 0, domain.ErrDocRangeIndexUnavailable
	}
	if err != nil {
		return 0, err
	}
	if count == nil {
		return 0, domain.ErrIntegrity
	}
	if reader.orderCatalog != nil {
		if err := reader.checkTransaction(ctx); err != nil {
			return 0, err
		}
		reader.orderCatalog.CompareAndSwap(nil, &docToolOrderCatalogRead{
			owner: reader.owner, source: reader.source, tree: tree, keys: keys, orders: source, plans: *count,
		})
	}
	return *count, nil
}

func (row *docToolTreeCatalogRow) destinations() []any {
	return []any{&row.owner, &row.raw, &row.state, &row.count, &row.from, &row.to}
}

func (row docToolTreeCatalogRow) validate(owner domain.ContentHash, tree domain.DocToolTreeSource, source domain.DocToolKeyMapSource, encoded []byte) error {
	if row.owner != owner || !bytes.Equal(encoded, row.raw) || row.from != tree.Root.From || row.to != tree.Root.To || (row.state == "complete") != (row.count != nil) {
		return domain.ErrIntegrity
	}
	if row.state != "complete" {
		return domain.ErrDocRangeIndexUnavailable
	}
	if *row.count != source.Nodes {
		return domain.ErrIntegrity
	}
	return nil
}

func validateDocToolKeyTree(ctx context.Context, database interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, owner domain.ContentHash, tree domain.DocToolTreeSource, source domain.DocToolKeyMapSource) error {
	encoded, err := tree.Encode()
	if err != nil || source.Validate(tree) != nil || domain.ValidateContentHash(owner) != nil {
		return domain.ErrIntegrity
	}
	var row docToolTreeCatalogRow
	if err := database.QueryRow(ctx, `SELECT substring(source_hash FROM 1 FOR 72),substring(bytes FROM 1 FOR $2),
 substring(state FROM 1 FOR 9),node_count,root_from,root_to FROM doc_tool_tree_sources_v1 WHERE scope=$1`,
		source.Tree, domain.MaxDocDisplaySourceBytes+1).Scan(row.destinations()...); err != nil {
		return mapNoRows(err)
	}
	return row.validate(owner, tree, source, encoded)
}

func validateDocToolKeyCatalog(ctx context.Context, database interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, owner domain.ContentHash, tree domain.DocToolTreeSource, source domain.DocToolKeyMapSource) error {
	treeRaw, err := tree.Encode()
	if err != nil || source.Validate(tree) != nil || domain.ValidateContentHash(owner) != nil {
		return domain.ErrIntegrity
	}
	keyRaw, err := source.Encode(tree)
	if err != nil {
		return err
	}
	var row docToolTreeCatalogRow
	var exists bool
	var storedTree domain.ContentHash
	var stored []byte
	var state string
	var count *int
	if err := database.QueryRow(ctx, `SELECT substring(tree.source_hash FROM 1 FOR 72),substring(tree.bytes FROM 1 FOR $3),
 substring(tree.state FROM 1 FOR 9),tree.node_count,tree.root_from,tree.root_to,
 keys.scope IS NOT NULL,coalesce(substring(keys.tree_scope FROM 1 FOR 72),''),substring(keys.bytes FROM 1 FOR $3),
 coalesce(substring(keys.state FROM 1 FOR 9),''),keys.member_count
 FROM doc_tool_tree_sources_v1 tree LEFT JOIN doc_tool_key_sources_v1 keys ON keys.scope=$2 WHERE tree.scope=$1`,
		source.Tree, domain.HashContent(keyRaw), domain.MaxDocDisplaySourceBytes+1).
		Scan(append(row.destinations(), &exists, &storedTree, &stored, &state, &count)...); err != nil {
		return mapNoRows(err)
	}
	if err := row.validate(owner, tree, source, treeRaw); err != nil {
		return err
	}
	if !exists {
		return domain.ErrNotFound
	}
	if storedTree != source.Tree || !bytes.Equal(stored, keyRaw) || state != "scanning" && state != "complete" ||
		(state == "complete") != (count != nil) || count != nil && (*count < 0 || *count > source.Nodes) {
		return domain.ErrIntegrity
	}
	if state != "complete" {
		return domain.ErrDocRangeIndexUnavailable
	}
	return ctx.Err()
}
