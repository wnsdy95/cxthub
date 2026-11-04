//go:build postgres

package store

import (
	"bytes"
	"context"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.DocToolOrderWorkReader = (*postgresDocEventDisplayWork)(nil)

type docToolOrdersDatabase interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (reader *postgresDocEventDisplayWork) bindToolOrders(ctx context.Context, tree domain.DocToolTreeSource, keys domain.DocToolKeyMapSource) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ctx.Value(repositoryTxKey{}) != reader.owner || !reader.storage.InReadOnlyTransaction(ctx) || reader.tree == nil ||
		*reader.tree != tree || reader.keys == nil || *reader.keys != keys || reader.orders == nil || reader.orders.Validate(tree, keys) != nil {
		return domain.ErrConflict
	}
	return nil
}

func readDocToolOrderParent(ctx context.Context, database interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, tree domain.DocToolTreeSource, parent int) (domain.DocToolTreeNode, error) {
	node, err := readDocToolCanonicalNode(ctx, database, tree, parent)
	if err != nil {
		if err == domain.ErrNotFound {
			return domain.DocToolTreeNode{}, domain.ErrValidation
		}
		return domain.DocToolTreeNode{}, err
	}
	if node.Value.From != parent || node.Type != domain.DocToolValueArray && node.Type != domain.DocToolValueObject {
		return domain.DocToolTreeNode{}, domain.ErrValidation
	}
	return node, ctx.Err()
}

func (reader *postgresDocEventDisplayWork) ReadToolOrderSource(ctx context.Context, tree domain.DocToolTreeSource, keys domain.DocToolKeyMapSource, parent int) (domain.DocToolOrderSource, error) {
	if err := reader.bindToolOrders(ctx, tree, keys); err != nil {
		return domain.DocToolOrderSource{}, err
	}
	return readDocToolOrderSource(ctx, reader.storage.db(ctx), reader.source, tree, keys, parent)
}

func readDocToolOrderSource(ctx context.Context, database docToolOrdersDatabase, owner domain.ContentHash, tree domain.DocToolTreeSource, keys domain.DocToolKeyMapSource, parent int) (result domain.DocToolOrderSource, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result = domain.DocToolOrderSource{}
		}
	}()
	if err := validateDocToolKeyCatalog(ctx, database, owner, tree, keys); err != nil {
		return result, err
	}
	node, err := readDocToolOrderParent(ctx, database, tree, parent)
	if err != nil {
		return result, err
	}
	count := node.Children
	if node.Type == domain.DocToolValueObject {
		scope, _ := keys.Hash(tree)
		if err := database.QueryRow(ctx, `SELECT count(*) FROM doc_tool_key_members_v1 WHERE scope=$1 AND parent_from=$2`, scope, parent).Scan(&count); err != nil {
			return result, err
		}
	}
	return domain.NewDocToolOrderSource(tree, keys, node, count)
}

func readDocToolOrderNodes(ctx context.Context, database docToolOrdersDatabase, owner domain.ContentHash, tree domain.DocToolTreeSource, keys domain.DocToolKeyMapSource, after, limit int) ([]domain.DocToolTreeNode, error) {
	if err := validateDocToolKeyCatalog(ctx, database, owner, tree, keys); err != nil {
		return nil, err
	}
	if after < tree.Root.From-1 || after >= tree.Root.To || limit <= 0 || limit > domain.DocToolKeyMapStepNodes {
		return nil, domain.ErrValidation
	}
	rows, err := database.Query(ctx, `SELECT `+docToolKeyNodeColumns+`
 FROM doc_tool_tree_nodes_v1 node WHERE node.scope=$1 AND node.node_from>$2 ORDER BY node.node_from LIMIT $3`, keys.Tree, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.DocToolTreeNode
	for rows.Next() {
		var selected postgresDocToolKeyNode
		if err := rows.Scan(selected.destinations()...); err != nil {
			return nil, err
		}
		node, err := selected.canonical(tree)
		if err != nil || node.Value.From <= after {
			return nil, domain.ErrIntegrity
		}
		result = append(result, node)
		after = node.Value.From
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (reader *postgresDocEventDisplayWork) ReadToolOrderEntries(ctx context.Context, tree domain.DocToolTreeSource, keys domain.DocToolKeyMapSource, source domain.DocToolOrderSource, after *domain.DocToolOrderEntry, limit int) ([]domain.DocToolOrderEntry, error) {
	if err := reader.bindToolOrders(ctx, tree, keys); err != nil {
		return nil, err
	}
	return reader.storage.readDocToolOrderEntries(ctx, reader.source, tree, keys, source, after, limit)
}

func (storage *PostgresStore) readDocToolOrderEntries(ctx context.Context, owner domain.ContentHash, tree domain.DocToolTreeSource, keys domain.DocToolKeyMapSource, source domain.DocToolOrderSource, after *domain.DocToolOrderEntry, limit int) (result []domain.DocToolOrderEntry, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result = nil
		}
	}()
	database := storage.db(ctx)
	if err := validateDocToolKeyCatalog(ctx, database, owner, tree, keys); err != nil {
		return nil, err
	}
	if source.Validate(tree, keys) != nil || limit <= 0 || limit > domain.DocToolOrderPageEntries ||
		after != nil && after.Validate(tree, keys, source) != nil {
		return nil, domain.ErrValidation
	}
	node, err := readDocToolOrderParent(ctx, database, tree, source.Parent.Value.From)
	if err != nil {
		return nil, err
	}
	canonical, _ := node.Encode(tree)
	expected, _ := source.Parent.Encode(tree)
	if !bytes.Equal(canonical, expected) {
		return nil, domain.ErrIntegrity
	}
	if node.Type == domain.DocToolValueObject {
		var cursor *domain.DocToolKeyMember
		if after != nil {
			member := after.Member()
			cursor = &member
		}
		members, err := storage.readDocToolKeyMembers(ctx, owner, tree, keys, node.Value.From, cursor, limit)
		if err != nil {
			return nil, err
		}
		for _, member := range members {
			entry := domain.NewDocToolOrderMember(member)
			if entry.Validate(tree, keys, source) != nil {
				return nil, domain.ErrIntegrity
			}
			result = append(result, entry)
		}
		return result, ctx.Err()
	}
	ordinal := -1
	if after != nil {
		ordinal = after.Node.Ordinal
		selected, err := readDocToolCanonicalNode(ctx, database, tree, after.Node.Value.From)
		if err != nil {
			if err == domain.ErrNotFound {
				return nil, domain.ErrIntegrity
			}
			return nil, err
		}
		raw, _ := after.Node.Encode(tree)
		canonical, err := selected.Encode(tree)
		if err != nil || !bytes.Equal(raw, canonical) {
			return nil, domain.ErrIntegrity
		}
	}
	rows, err := database.Query(ctx, `SELECT `+docToolKeyNodeColumns+`
 FROM unnest(ARRAY[$1::text],ARRAY[$2::bigint]) requested(scope,parent_from)
 CROSS JOIN LATERAL(SELECT node.* FROM doc_tool_tree_nodes_v1 node
 WHERE node.scope=requested.scope AND node.parent_from=requested.parent_from AND node.ordinal>$3
 ORDER BY node.ordinal LIMIT $4) node ORDER BY node.ordinal`, keys.Tree, node.Value.From, ordinal, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var selected postgresDocToolKeyNode
		if err := rows.Scan(selected.destinations()...); err != nil {
			return nil, err
		}
		child, err := selected.canonical(tree)
		if err != nil {
			return nil, err
		}
		entry := domain.DocToolOrderEntry{Node: child, First: child.Value.From}
		if entry.Validate(tree, keys, source) != nil || child.Ordinal != ordinal+1+len(result) {
			return nil, domain.ErrIntegrity
		}
		result = append(result, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(result) != min(limit, source.Count-ordinal-1) {
		return nil, domain.ErrIntegrity
	}
	return result, ctx.Err()
}

func readDocToolOrdersSet(ctx context.Context, database interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, owner domain.ContentHash, tree domain.DocToolTreeSource, keys domain.DocToolKeyMapSource, source domain.DocToolOrdersSource, lock bool) (string, *int, error) {
	raw, err := source.Encode(tree, keys)
	if err != nil {
		return "", nil, err
	}
	query := `SELECT substring(source_hash FROM 1 FOR 72),substring(tree_scope FROM 1 FOR 72),
 substring(keys_scope FROM 1 FOR 72),substring(bytes FROM 1 FOR $2),substring(state FROM 1 FOR 9),plan_count
 FROM doc_tool_order_sources_v1 WHERE scope=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	var storedOwner, storedTree, storedKeys domain.ContentHash
	var stored []byte
	var state string
	var count *int
	if err := database.QueryRow(ctx, query, domain.HashContent(raw), domain.MaxDocDisplaySourceBytes+1).
		Scan(&storedOwner, &storedTree, &storedKeys, &stored, &state, &count); err != nil {
		return "", nil, mapNoRows(err)
	}
	if storedOwner != owner || storedTree != source.Tree || storedKeys != source.Keys || !bytes.Equal(stored, raw) ||
		state != "building" && state != "complete" || (state == "complete") != (count != nil) || count != nil && (*count < 0 || *count > source.Nodes) {
		return "", nil, domain.ErrIntegrity
	}
	return state, count, ctx.Err()
}

func (reader *postgresDocEventDisplayWork) ReadToolOrderPlan(ctx context.Context, tree domain.DocToolTreeSource, keys domain.DocToolKeyMapSource, source domain.DocToolOrdersSource, parent int) (result domain.DocToolOrderPlan, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result = domain.DocToolOrderPlan{}
		}
	}()
	count, err := reader.readToolOrderCatalog(ctx, tree, keys, source)
	if err != nil {
		return result, err
	}
	node, err := readDocToolOrderParent(ctx, reader.storage.db(ctx), tree, parent)
	if err != nil {
		return result, err
	}
	if count == 0 {
		return result, domain.ErrIntegrity
	}
	scope, _ := source.Hash(tree, keys)
	var storedTree, hash domain.ContentHash
	var raw []byte
	if err := reader.storage.db(ctx).QueryRow(ctx, `SELECT substring(tree_scope FROM 1 FOR 72),
 substring(plan_hash FROM 1 FOR 72),substring(bytes FROM 1 FOR $3)
 FROM doc_tool_order_plans_v1 WHERE scope=$1 AND parent_from=$2`, scope, parent, domain.MaxDocDisplaySourceBytes+1).
		Scan(&storedTree, &hash, &raw); err != nil {
		if err == pgx.ErrNoRows {
			return result, domain.ErrIntegrity
		}
		return result, err
	}
	plan, err := domain.DecodeDocToolOrderPlan(tree, keys, hash, raw)
	if err != nil || storedTree != source.Tree {
		return result, domain.ErrIntegrity
	}
	parentRaw, _ := node.Encode(tree)
	planParent, _ := plan.Source.Parent.Encode(tree)
	if !bytes.Equal(parentRaw, planParent) {
		return result, domain.ErrIntegrity
	}
	return plan, ctx.Err()
}

func (storage *PostgresStore) saveDocToolOrders(ctx context.Context, transaction pgx.Tx, owner domain.ContentHash, prior, next domain.DocEventDisplayPreparation, outer domain.DocEventDisplayStep) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	source, step, found := outer.ToolOrders()
	if !found {
		if prior.Phase == "tool_orders" || next.Phase == "tool_orders_done" {
			return domain.ErrIntegrity
		}
		return nil
	}
	if prior.Phase != "tool_orders" || prior.ToolTree == nil || next.ToolTree == nil || *prior.ToolTree != *next.ToolTree ||
		prior.ToolKeys == nil || next.ToolKeys == nil || *prior.ToolKeys != *next.ToolKeys || prior.ToolOrders == nil ||
		next.ToolOrders == nil || *prior.ToolOrders != source || *next.ToolOrders != source || step.Source() != source || step.Prior() != prior.Active {
		return domain.ErrIntegrity
	}
	tree, keys := *prior.ToolTree, *prior.ToolKeys
	state := step.State()
	raw, err := state.Encode(tree, keys, source)
	if err != nil || domain.HashContent(raw) != next.Active || state.Done(source) != (next.Phase == "tool_orders_done") ||
		!state.Done(source) && next.Phase != "tool_orders" {
		return domain.ErrIntegrity
	}
	var previousRaw []byte
	if err := transaction.QueryRow(ctx, `SELECT substring(bytes FROM 1 FOR $3) FROM doc_display_objects_v1
 WHERE source_hash=$1 AND hash=$2`, owner, prior.Active, domain.MaxDocDisplayStateBytes+1).Scan(&previousRaw); err != nil {
		return mapNoRows(err)
	}
	previous, err := domain.DecodeDocToolOrdersState(tree, keys, source, prior.Active, previousRaw)
	if err != nil || previous.Done(source) {
		return domain.ErrIntegrity
	}
	verified, err := domain.StepDocToolOrders(ctx, tree, keys, source, previous, domain.DocToolPreparationReader{
		Nodes: func(ctx context.Context, after, limit int) ([]domain.DocToolTreeNode, error) {
			return readDocToolOrderNodes(ctx, transaction, owner, tree, keys, after, limit)
		},
		OrderSource: func(ctx context.Context, parent int) (domain.DocToolOrderSource, error) {
			return readDocToolOrderSource(ctx, transaction, owner, tree, keys, parent)
		},
		OrderEntries: func(ctx context.Context, container domain.DocToolOrderSource, after *domain.DocToolOrderEntry, limit int) ([]domain.DocToolOrderEntry, error) {
			return storage.readDocToolOrderEntries(ctx, owner, tree, keys, container, after, limit)
		},
	}, func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
		return storage.loadDocDisplayObject(ctx, owner, hash)
	})
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(step, verified) {
		return domain.ErrIntegrity
	}
	encoded, err := source.Encode(tree, keys)
	if err != nil {
		return err
	}
	scope := domain.HashContent(encoded)
	if _, err := transaction.Exec(ctx, `INSERT INTO doc_tool_order_sources_v1(scope,source_hash,tree_scope,keys_scope,bytes)
 VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, scope, owner, source.Tree, source.Keys, encoded); err != nil {
		return err
	}
	phase, count, err := readDocToolOrdersSet(ctx, transaction, owner, tree, keys, source, true)
	if err != nil {
		return err
	}
	if plan, found := verified.Plan(); found {
		planRaw, err := plan.Encode(tree, keys)
		if err != nil {
			return err
		}
		tag, err := transaction.Exec(ctx, `INSERT INTO doc_tool_order_plans_v1(scope,tree_scope,parent_from,plan_hash,bytes)
 SELECT $1,$2,$3,$4,$5 WHERE $6::boolean OR
 EXISTS(SELECT 1 FROM doc_tool_order_plans_v1 WHERE scope=$1 AND parent_from=$3)
 ON CONFLICT(scope,parent_from) DO UPDATE SET plan_hash=EXCLUDED.plan_hash
 WHERE doc_tool_order_plans_v1.tree_scope=EXCLUDED.tree_scope
 AND doc_tool_order_plans_v1.plan_hash=EXCLUDED.plan_hash AND doc_tool_order_plans_v1.bytes=EXCLUDED.bytes`,
			scope, source.Tree, plan.Source.Parent.Value.From, domain.HashContent(planRaw), planRaw, phase == "building")
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return domain.ErrIntegrity
		}
	}
	if !state.Done(source) {
		if phase == "complete" && (count == nil || state.Containers > *count) {
			return domain.ErrIntegrity
		}
		return ctx.Err()
	}
	if err := validateDocToolOrderCatalog(ctx, transaction, tree, keys, source, state.Containers); err != nil {
		return err
	}
	if phase == "complete" {
		if count == nil || *count != state.Containers {
			return domain.ErrIntegrity
		}
		return ctx.Err()
	}
	tag, err := transaction.Exec(ctx, `UPDATE doc_tool_order_sources_v1 SET state='complete',plan_count=$2
 WHERE scope=$1 AND state='building' AND plan_count IS NULL`, scope, state.Containers)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return ctx.Err()
}

func validateDocToolOrderCatalog(ctx context.Context, transaction pgx.Tx, tree domain.DocToolTreeSource, keys domain.DocToolKeyMapSource, source domain.DocToolOrdersSource, expected int) error {
	scope, err := source.Hash(tree, keys)
	if err != nil {
		return err
	}
	rows, err := transaction.Query(ctx, `SELECT `+docToolKeyNodeColumns+`,substring(plan.tree_scope FROM 1 FOR 72),
 substring(plan.plan_hash FROM 1 FOR 72),substring(plan.bytes FROM 1 FOR $3)
 FROM doc_tool_tree_nodes_v1 node LEFT JOIN doc_tool_order_plans_v1 plan
 ON plan.scope=$1 AND plan.parent_from=node.node_from
 WHERE node.scope=$2 AND node.value_type IN (123,91) ORDER BY node.node_from`,
		scope, source.Tree, domain.MaxDocDisplaySourceBytes+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var selected postgresDocToolKeyNode
		var storedTree, hash *domain.ContentHash
		var raw []byte
		if err := rows.Scan(append(selected.destinations(), &storedTree, &hash, &raw)...); err != nil {
			return err
		}
		parent, err := selected.canonical(tree)
		if err != nil || storedTree == nil || hash == nil || *storedTree != source.Tree {
			return domain.ErrIntegrity
		}
		plan, err := domain.DecodeDocToolOrderPlan(tree, keys, *hash, raw)
		if err != nil {
			return err
		}
		parentRaw, _ := parent.Encode(tree)
		planParent, _ := plan.Source.Parent.Encode(tree)
		if !bytes.Equal(parentRaw, planParent) {
			return domain.ErrIntegrity
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	var total int
	if err := transaction.QueryRow(ctx, `SELECT count(*) FROM doc_tool_order_plans_v1 WHERE scope=$1`, scope).Scan(&total); err != nil {
		return err
	}
	if count != expected || total != count {
		return domain.ErrIntegrity
	}
	return ctx.Err()
}
