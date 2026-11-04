//go:build postgres

package store

import (
	"bytes"
	"cmp"
	"context"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (storage *PostgresStore) saveDocToolTreeScan(ctx context.Context, transaction pgx.Tx, owner domain.ContentHash, prior, next domain.DocEventDisplayPreparation, step domain.DocEventDisplayStep) error {
	source, scan, found := step.ToolTreeScan()
	if !found {
		if prior.Phase == "tool_tree" || next.Phase == "tool_tree_done" {
			return domain.ErrIntegrity
		}
		return nil
	}
	if prior.Phase != "tool_tree" || prior.ToolTree == nil || next.ToolTree == nil || *prior.ToolTree != source || *next.ToolTree != source || scan.Prior() != prior.Active {
		return domain.ErrIntegrity
	}
	checkpoint := scan.State()
	raw, err := checkpoint.Encode(source)
	if err != nil || domain.HashContent(raw) != next.Active || checkpoint.Done() != (next.Phase == "tool_tree_done") {
		return domain.ErrIntegrity
	}
	encoded, err := source.Encode()
	if err != nil {
		return err
	}
	scope := domain.HashContent(encoded)
	if _, err := transaction.Exec(ctx, `INSERT INTO doc_tool_tree_sources_v1(scope,source_hash,bytes,root_from,root_to)
 VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, scope, owner, encoded, source.Root.From, source.Root.To); err != nil {
		return err
	}
	var storedOwner domain.ContentHash
	var stored []byte
	var phase string
	var count *int64
	var from, to int
	if err := transaction.QueryRow(ctx, `SELECT substring(source_hash FROM 1 FOR 72),substring(bytes FROM 1 FOR $2),state,node_count,root_from,root_to
 FROM doc_tool_tree_sources_v1 WHERE scope=$1 FOR UPDATE`, scope, domain.MaxDocDisplaySourceBytes+1).Scan(&storedOwner, &stored, &phase, &count, &from, &to); err != nil {
		return mapNoRows(err)
	}
	if storedOwner != owner || !bytes.Equal(stored, encoded) || from != source.Root.From || to != source.Root.To || (phase == "complete") != (count != nil) {
		return domain.ErrIntegrity
	}
	nodes := scan.Nodes()
	slices.SortFunc(nodes, func(left, right domain.DocToolTreeNode) int { return cmp.Compare(left.Value.From, right.Value.From) })
	batch := &pgx.Batch{}
	for _, node := range nodes {
		raw, err := node.Encode(source)
		if err != nil {
			return err
		}
		var keyFrom, keyTo *int
		if node.Key != nil {
			keyFrom, keyTo = &node.Key.From, &node.Key.To
		}
		batch.Queue(`INSERT INTO doc_tool_tree_nodes_v1
 (scope,node_from,node_to,parent_from,ordinal,key_from,key_to,value_type,child_count,depth,node_hash,bytes)
 SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12
 WHERE $13::boolean OR EXISTS(SELECT 1 FROM doc_tool_tree_nodes_v1 WHERE scope=$1 AND node_from=$2)
 ON CONFLICT(scope,node_from) DO UPDATE SET node_hash=EXCLUDED.node_hash
 WHERE doc_tool_tree_nodes_v1.node_hash=EXCLUDED.node_hash AND doc_tool_tree_nodes_v1.bytes=EXCLUDED.bytes
 AND doc_tool_tree_nodes_v1.node_to=EXCLUDED.node_to AND doc_tool_tree_nodes_v1.parent_from=EXCLUDED.parent_from
 AND doc_tool_tree_nodes_v1.ordinal=EXCLUDED.ordinal AND doc_tool_tree_nodes_v1.key_from IS NOT DISTINCT FROM EXCLUDED.key_from
 AND doc_tool_tree_nodes_v1.key_to IS NOT DISTINCT FROM EXCLUDED.key_to
 AND doc_tool_tree_nodes_v1.value_type=EXCLUDED.value_type AND doc_tool_tree_nodes_v1.child_count=EXCLUDED.child_count
 AND doc_tool_tree_nodes_v1.depth=EXCLUDED.depth`, scope, node.Value.From, node.Value.To, node.Parent, node.Ordinal,
			keyFrom, keyTo, int(node.Type), node.Children, node.Depth, domain.HashContent(raw), raw, phase == "scanning")
	}
	if len(nodes) > 0 {
		results := transaction.SendBatch(ctx, batch)
		for range nodes {
			tag, err := results.Exec()
			if err != nil {
				_ = results.Close()
				return err
			}
			if tag.RowsAffected() != 1 {
				_ = results.Close()
				return domain.ErrIntegrity
			}
		}
		if err := results.Close(); err != nil {
			return err
		}
	}
	if !checkpoint.Done() {
		return nil
	}
	if phase == "complete" {
		if count == nil || *count != int64(checkpoint.NodeCount) {
			return domain.ErrIntegrity
		}
		return nil
	}
	if err := validateDocToolTreeRows(ctx, transaction, scope, source, checkpoint.NodeCount); err != nil {
		return err
	}
	tag, err := transaction.Exec(ctx, `UPDATE doc_tool_tree_sources_v1 SET state='complete',node_count=$2
 WHERE scope=$1 AND state='scanning' AND node_count IS NULL`, scope, checkpoint.NodeCount)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return nil
}

func (reader *postgresDocEventDisplayWork) ReadToolTreeNodes(ctx context.Context, source domain.DocToolTreeSource, after, limit int) (result []domain.DocToolTreeNode, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result = nil
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ctx.Value(repositoryTxKey{}) != reader.owner || !reader.storage.InReadOnlyTransaction(ctx) || reader.tree == nil || *reader.tree != source {
		return nil, domain.ErrConflict
	}
	if source.Validate() != nil || after < -1 || after >= source.Root.To || limit <= 0 || limit > 64 {
		return nil, domain.ErrValidation
	}
	encoded, err := source.Encode()
	if err != nil {
		return nil, err
	}
	scope := domain.HashContent(encoded)
	var stored []byte
	var owner domain.ContentHash
	var state string
	if err := reader.storage.db(ctx).QueryRow(ctx, `SELECT substring(source_hash FROM 1 FOR 72),substring(bytes FROM 1 FOR $2),state
 FROM doc_tool_tree_sources_v1 WHERE scope=$1`, scope, domain.MaxDocDisplaySourceBytes+1).Scan(&owner, &stored, &state); err != nil {
		return nil, mapNoRows(err)
	}
	if owner != reader.source || !bytes.Equal(stored, encoded) {
		return nil, domain.ErrIntegrity
	}
	if state != "complete" {
		return nil, domain.ErrDocRangeIndexUnavailable
	}
	rows, err := reader.storage.db(ctx).Query(ctx, `SELECT node_from,substring(node_hash FROM 1 FOR 72),substring(bytes FROM 1 FOR $4)
 FROM doc_tool_tree_nodes_v1 WHERE scope=$1 AND node_from>$2 ORDER BY node_from LIMIT $3`, scope, after, limit, domain.MaxDocToolTreeNodeBytes+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var from int
		var hash domain.ContentHash
		var raw []byte
		if err := rows.Scan(&from, &hash, &raw); err != nil {
			return nil, err
		}
		node, err := domain.DecodeDocToolTreeNode(source, hash, raw)
		if err != nil || node.Value.From != from || from <= after {
			return nil, domain.ErrIntegrity
		}
		result = append(result, node)
		after = from
	}
	return result, rows.Err()
}

var _ outbound.DocToolTreeWorkReader = (*postgresDocEventDisplayWork)(nil)

func validateDocToolTreeRows(ctx context.Context, transaction pgx.Tx, scope domain.ContentHash, source domain.DocToolTreeSource, count int) error {
	var valid bool
	err := transaction.QueryRow(ctx, `SELECT
 (SELECT count(*)=$2 FROM doc_tool_tree_nodes_v1 WHERE scope=$1)
 AND (SELECT count(*)=1 FROM doc_tool_tree_nodes_v1 WHERE scope=$1 AND parent_from=-1
   AND node_from=$3 AND node_to=$4 AND value_type=$5 AND ordinal=0 AND depth=0 AND key_from IS NULL)
 AND NOT EXISTS(SELECT 1 FROM doc_tool_tree_nodes_v1 node
   LEFT JOIN doc_tool_tree_nodes_v1 parent ON parent.scope=node.scope AND parent.node_from=node.parent_from
   WHERE node.scope=$1 AND node.parent_from>=0 AND (parent.node_from IS NULL
    OR parent.value_type NOT IN (123,91) OR node.node_from<=parent.node_from OR node.node_to>=parent.node_to
    OR node.depth<>parent.depth+1 OR (parent.value_type=123 AND (node.key_from IS NULL OR node.key_from<=parent.node_from))
    OR (parent.value_type=91 AND node.key_from IS NOT NULL)))
 AND NOT EXISTS(SELECT 1 FROM doc_tool_tree_nodes_v1 parent
   LEFT JOIN LATERAL(SELECT count(*) AS size,min(ordinal) AS first,max(ordinal) AS last
     FROM doc_tool_tree_nodes_v1 child WHERE child.scope=parent.scope AND child.parent_from=parent.node_from) children ON true
   WHERE parent.scope=$1 AND (children.size<>parent.child_count
     OR (children.size>0 AND (children.first<>0 OR children.last<>children.size-1))))`,
		scope, count, source.Root.From, source.Root.To, int(source.Type)).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return domain.ErrIntegrity
	}
	return nil
}
