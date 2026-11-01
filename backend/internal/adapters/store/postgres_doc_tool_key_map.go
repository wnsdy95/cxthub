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

const docToolKeyNodeColumns = `node.node_from,node.node_to,node.parent_from,node.ordinal,node.key_from,node.key_to,
 node.value_type,node.child_count,node.depth,substring(node.node_hash FROM 1 FOR 72),substring(node.bytes FROM 1 FOR 2049)`

const docToolKeyMemberExtrema = `(SELECT min(earlier.node_from) FROM doc_tool_key_observations_v1 earlier
 WHERE earlier.scope=member.scope AND earlier.parent_from=member.parent_from AND earlier.digest=member.digest)=member.first_from
 AND (SELECT max(later.node_from) FROM doc_tool_key_observations_v1 later
 WHERE later.scope=member.scope AND later.parent_from=member.parent_from AND later.digest=member.digest)=member.last_from`

var _ outbound.DocToolKeyWorkReader = (*postgresDocEventDisplayWork)(nil)

type postgresDocToolKeyNode struct {
	node      domain.DocToolTreeNode
	keyFrom   *int
	keyTo     *int
	valueType int
	hash      domain.ContentHash
	raw       []byte
}

func (row *postgresDocToolKeyNode) destinations() []any {
	return []any{&row.node.Value.From, &row.node.Value.To, &row.node.Parent, &row.node.Ordinal, &row.keyFrom, &row.keyTo,
		&row.valueType, &row.node.Children, &row.node.Depth, &row.hash, &row.raw}
}

func (row *postgresDocToolKeyNode) canonical(tree domain.DocToolTreeSource) (domain.DocToolTreeNode, error) {
	if row.valueType < 0 || row.valueType > 255 || (row.keyFrom == nil) != (row.keyTo == nil) {
		return domain.DocToolTreeNode{}, domain.ErrIntegrity
	}
	row.node.Type = domain.DocToolValueType(row.valueType)
	if row.keyFrom != nil {
		row.node.Key = &domain.DocFindSpan{From: *row.keyFrom, To: *row.keyTo}
	}
	raw, err := row.node.Encode(tree)
	if err != nil || !bytes.Equal(raw, row.raw) || domain.ValidateContentHash(row.hash) != nil || domain.HashContent(raw) != row.hash {
		return domain.DocToolTreeNode{}, domain.ErrIntegrity
	}
	return row.node, nil
}

func readDocToolCanonicalNode(ctx context.Context, database interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, tree domain.DocToolTreeSource, from int) (domain.DocToolTreeNode, error) {
	scope, err := tree.Hash()
	if err != nil || from < tree.Root.From || from >= tree.Root.To {
		return domain.DocToolTreeNode{}, domain.ErrValidation
	}
	var selected postgresDocToolKeyNode
	if err := database.QueryRow(ctx, `SELECT `+docToolKeyNodeColumns+`
 FROM unnest(ARRAY[$1::text],ARRAY[$2::bigint]) requested(scope,node_from)
 CROSS JOIN LATERAL(SELECT node.* FROM doc_tool_tree_nodes_v1 node
 WHERE node.scope=requested.scope AND node.node_from=requested.node_from LIMIT 1) node`, scope, from).Scan(selected.destinations()...); err != nil {
		return domain.DocToolTreeNode{}, mapNoRows(err)
	}
	node, err := selected.canonical(tree)
	if err != nil || node.Value.From != from {
		return domain.DocToolTreeNode{}, domain.ErrIntegrity
	}
	return node, ctx.Err()
}

func (storage *PostgresStore) saveDocToolKeyMap(ctx context.Context, transaction pgx.Tx, owner domain.ContentHash, prior, next domain.DocEventDisplayPreparation, outer domain.DocEventDisplayStep) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	source, step, found := outer.ToolKeyMap()
	if !found {
		if prior.Phase == "tool_keys" || next.Phase == "tool_keys_done" {
			return domain.ErrIntegrity
		}
		return nil
	}
	if prior.Phase != "tool_keys" || prior.ToolTree == nil || next.ToolTree == nil || *prior.ToolTree != *next.ToolTree ||
		prior.ToolKeys == nil || next.ToolKeys == nil || *prior.ToolKeys != source || *next.ToolKeys != source || step.Source() != source || step.Prior() != prior.Active {
		return domain.ErrIntegrity
	}
	tree := *prior.ToolTree
	state := step.State()
	raw, err := state.Encode(tree, source)
	if err != nil || domain.HashContent(raw) != next.Active || state.Done() != (next.Phase == "tool_keys_done") || !state.Done() && next.Phase != "tool_keys" {
		return domain.ErrIntegrity
	}
	var previousRaw []byte
	if err := transaction.QueryRow(ctx, `SELECT substring(bytes FROM 1 FOR $3) FROM doc_display_objects_v1
 WHERE source_hash=$1 AND hash=$2`, owner, prior.Active, domain.MaxDocDisplayStateBytes+1).Scan(&previousRaw); err != nil {
		return mapNoRows(err)
	}
	previous, err := domain.DecodeDocToolKeyMapState(tree, source, prior.Active, previousRaw)
	if err != nil || previous.Done() || state.After < previous.After || state.Seen < previous.Seen || state.Keys < previous.Keys {
		return domain.ErrIntegrity
	}
	if err := validateDocToolKeyTree(ctx, transaction, owner, tree, source); err != nil {
		return err
	}
	encoded, err := source.Encode(tree)
	if err != nil {
		return err
	}
	scope := domain.HashContent(encoded)
	if _, err := transaction.Exec(ctx, `INSERT INTO doc_tool_key_sources_v1(scope,tree_scope,bytes)
 VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, scope, source.Tree, encoded); err != nil {
		return err
	}
	var stored []byte
	var storedTree domain.ContentHash
	var phase string
	var memberCount *int
	if err := transaction.QueryRow(ctx, `SELECT substring(tree_scope FROM 1 FOR 72),substring(bytes FROM 1 FOR $2),
 substring(state FROM 1 FOR 9),member_count FROM doc_tool_key_sources_v1 WHERE scope=$1 FOR UPDATE`,
		scope, domain.MaxDocDisplaySourceBytes+1).Scan(&storedTree, &stored, &phase, &memberCount); err != nil {
		return mapNoRows(err)
	}
	if storedTree != source.Tree || !bytes.Equal(stored, encoded) || phase != "scanning" && phase != "complete" || (phase == "complete") != (memberCount != nil) {
		return domain.ErrIntegrity
	}
	items, err := validateDocToolKeyInputs(ctx, transaction, tree, source, previous, state, step)
	if err != nil {
		return err
	}
	observations := &pgx.Batch{}
	for _, item := range items {
		nodeRaw, err := item.Node.Encode(tree)
		if err != nil {
			return err
		}
		nodeHash := domain.HashContent(nodeRaw)
		observations.Queue(`INSERT INTO doc_tool_key_observations_v1(scope,tree_scope,node_from,parent_from,digest,is_array_index,array_index,node_hash)
 SELECT $1,$2,$3,$4,$5,$6,$7,$8 WHERE $9::boolean OR
 EXISTS(SELECT 1 FROM doc_tool_key_observations_v1 WHERE scope=$1 AND node_from=$3)
 ON CONFLICT(scope,node_from) DO UPDATE SET node_hash=EXCLUDED.node_hash
 WHERE doc_tool_key_observations_v1.tree_scope=EXCLUDED.tree_scope
 AND doc_tool_key_observations_v1.parent_from=EXCLUDED.parent_from AND doc_tool_key_observations_v1.digest=EXCLUDED.digest
 AND doc_tool_key_observations_v1.is_array_index=EXCLUDED.is_array_index
 AND doc_tool_key_observations_v1.array_index=EXCLUDED.array_index AND doc_tool_key_observations_v1.node_hash=EXCLUDED.node_hash`,
			scope, source.Tree, item.Node.Value.From, item.Node.Parent, item.Digest, item.IsArrayIndex,
			int64(item.ArrayIndex), nodeHash, phase == "scanning")
	}
	if err := execDocToolKeyBatch(ctx, transaction, observations, len(items)); err != nil {
		return err
	}
	if err := upsertDocToolKeyMembers(ctx, transaction, scope, tree, items, phase == "scanning"); err != nil {
		return err
	}
	if !state.Done() {
		return ctx.Err()
	}
	count, err := validateDocToolKeyRows(ctx, transaction, scope, tree, source, state)
	if err != nil {
		return err
	}
	if phase == "complete" {
		if memberCount == nil || *memberCount != count {
			return domain.ErrIntegrity
		}
		return ctx.Err()
	}
	tag, err := transaction.Exec(ctx, `UPDATE doc_tool_key_sources_v1 SET state='complete',member_count=$2
 WHERE scope=$1 AND state='scanning' AND member_count IS NULL`, scope, count)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return ctx.Err()
}

func validateDocToolKeyInputs(ctx context.Context, transaction pgx.Tx, tree domain.DocToolTreeSource, source domain.DocToolKeyMapSource, prior, next domain.DocToolKeyMapState, step domain.DocToolKeyMapStep) ([]domain.DocToolKeyItem, error) {
	inputs, items := step.Inputs(), step.Items()
	if len(inputs) == 0 || len(inputs) > domain.DocToolKeyMapStepNodes || next.Keys-prior.Keys != len(items) {
		return nil, domain.ErrIntegrity
	}
	rows, err := transaction.Query(ctx, `SELECT `+docToolKeyNodeColumns+` FROM doc_tool_tree_nodes_v1 node
 WHERE node.scope=$1 AND node.node_from>$2 ORDER BY node.node_from LIMIT $3`, source.Tree, prior.After, len(inputs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	read, processed, keyed, after := 0, 0, 0, prior.After
	for rows.Next() {
		var selected postgresDocToolKeyNode
		if err := rows.Scan(selected.destinations()...); err != nil {
			return nil, err
		}
		node, err := selected.canonical(tree)
		if err != nil || read >= len(inputs) {
			return nil, domain.ErrIntegrity
		}
		expected, err := inputs[read].Encode(tree)
		if err != nil || !bytes.Equal(expected, selected.raw) {
			return nil, domain.ErrIntegrity
		}
		if read == 0 && prior.Current != nil {
			pending, err := prior.Current.Encode(tree)
			if err != nil || !bytes.Equal(pending, selected.raw) {
				return nil, domain.ErrIntegrity
			}
		}
		read++
		if node.Value.From > next.After {
			if read != len(inputs) || next.Current == nil {
				return nil, domain.ErrIntegrity
			}
			pending, err := next.Current.Encode(tree)
			if err != nil || !bytes.Equal(pending, selected.raw) {
				return nil, domain.ErrIntegrity
			}
			continue
		}
		processed++
		after = node.Value.From
		if node.Key == nil {
			continue
		}
		if keyed >= len(items) || items[keyed].Validate(tree) != nil {
			return nil, domain.ErrIntegrity
		}
		itemRaw, err := items[keyed].Node.Encode(tree)
		if err != nil || !bytes.Equal(itemRaw, selected.raw) {
			return nil, domain.ErrIntegrity
		}
		keyed++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if read != len(inputs) || next.Seen-prior.Seen != processed || next.After != after || keyed != len(items) ||
		(next.Current != nil) != (processed < len(inputs)) {
		return nil, domain.ErrIntegrity
	}
	return items, ctx.Err()
}

func execDocToolKeyBatch(ctx context.Context, transaction pgx.Tx, batch *pgx.Batch, count int) error {
	if count == 0 {
		return ctx.Err()
	}
	results := transaction.SendBatch(ctx, batch)
	for range count {
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
	return ctx.Err()
}

func upsertDocToolKeyMembers(ctx context.Context, transaction pgx.Tx, scope domain.ContentHash, tree domain.DocToolTreeSource, items []domain.DocToolKeyItem, scanning bool) error {
	slices.SortFunc(items, func(left, right domain.DocToolKeyItem) int {
		if order := cmp.Compare(left.Node.Parent, right.Node.Parent); order != 0 {
			return order
		}
		if order := cmp.Compare(left.Digest, right.Digest); order != 0 {
			return order
		}
		return cmp.Compare(left.Node.Value.From, right.Node.Value.From)
	})
	batch, groups := &pgx.Batch{}, 0
	for start := 0; start < len(items); {
		first := items[start]
		member, err := domain.MergeDocToolKeyMember(tree, nil, first)
		if err != nil {
			return err
		}
		end := start + 1
		for end < len(items) && items[end].Node.Parent == first.Node.Parent && items[end].Digest == first.Digest {
			member, err = domain.MergeDocToolKeyMember(tree, &member, items[end])
			if err != nil {
				return err
			}
			end++
		}
		batch.Queue(`INSERT INTO doc_tool_key_members_v1(scope,parent_from,digest,first_from,last_from,is_array_index,array_index)
 SELECT $1,$2,$3,$4,$5,$6,$7 WHERE $8::boolean OR
 EXISTS(SELECT 1 FROM doc_tool_key_members_v1 WHERE scope=$1 AND parent_from=$2 AND digest=$3)
 ON CONFLICT(scope,parent_from,digest) DO UPDATE
 SET first_from=LEAST(doc_tool_key_members_v1.first_from,EXCLUDED.first_from),last_from=GREATEST(doc_tool_key_members_v1.last_from,EXCLUDED.last_from)
 WHERE doc_tool_key_members_v1.is_array_index=EXCLUDED.is_array_index AND doc_tool_key_members_v1.array_index=EXCLUDED.array_index
 AND ($8::boolean OR (doc_tool_key_members_v1.first_from<=EXCLUDED.first_from AND doc_tool_key_members_v1.last_from>=EXCLUDED.last_from))`,
			scope, member.Last.Node.Parent, member.Last.Digest, member.First, member.Last.Node.Value.From,
			member.Last.IsArrayIndex, int64(member.Last.ArrayIndex), scanning)
		groups++
		start = end
	}
	return execDocToolKeyBatch(ctx, transaction, batch, groups)
}

func validateDocToolKeyRows(ctx context.Context, transaction pgx.Tx, scope domain.ContentHash, tree domain.DocToolTreeSource, source domain.DocToolKeyMapSource, state domain.DocToolKeyMapState) (int, error) {
	rows, err := transaction.Query(ctx, `SELECT `+docToolKeyNodeColumns+`,observation.node_from,observation.parent_from,
 substring(observation.digest FROM 1 FOR 72),observation.is_array_index,observation.array_index,substring(observation.node_hash FROM 1 FOR 72)
 FROM doc_tool_tree_nodes_v1 node LEFT JOIN doc_tool_key_observations_v1 observation
 ON observation.scope=$1 AND observation.tree_scope=node.scope AND observation.node_from=node.node_from
 WHERE node.scope=$2 ORDER BY node.node_from`, scope, source.Tree)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	nodes, keys, after := 0, 0, tree.Root.From-1
	for rows.Next() {
		var selected postgresDocToolKeyNode
		var from, parent *int
		var digest, hash *domain.ContentHash
		var isIndex *bool
		var index *int64
		if err := rows.Scan(append(selected.destinations(), &from, &parent, &digest, &isIndex, &index, &hash)...); err != nil {
			return 0, err
		}
		node, err := selected.canonical(tree)
		if err != nil || node.Value.From <= after || (node.Key != nil) != (from != nil) {
			return 0, domain.ErrIntegrity
		}
		nodes++
		after = node.Value.From
		if node.Key == nil {
			continue
		}
		if parent == nil || digest == nil || isIndex == nil || index == nil || hash == nil || *from != node.Value.From ||
			*parent != node.Parent || *hash != selected.hash || *index < 0 || *index > 4294967294 {
			return 0, domain.ErrIntegrity
		}
		item := domain.DocToolKeyItem{Node: node, Digest: *digest, IsArrayIndex: *isIndex, ArrayIndex: uint32(*index)}
		if item.Validate(tree) != nil {
			return 0, domain.ErrIntegrity
		}
		keys++
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	rows.Close()
	if nodes != source.Nodes || nodes != state.Seen || keys != state.Keys || after != state.After {
		return 0, domain.ErrIntegrity
	}
	var count int
	var valid bool
	if err := transaction.QueryRow(ctx, `WITH grouped AS (
 SELECT parent_from,digest,min(node_from) AS first_from,max(node_from) AS last_from,
 bool_and(is_array_index) AS is_array_index,bool_and(is_array_index)=bool_or(is_array_index) AS same_class,
 min(array_index) AS array_index,min(array_index)=max(array_index) AS same_index
 FROM doc_tool_key_observations_v1 WHERE scope=$1 GROUP BY parent_from,digest),
 members AS (SELECT * FROM doc_tool_key_members_v1 WHERE scope=$1)
 SELECT (SELECT count(*) FROM members),NOT EXISTS(
 SELECT 1 FROM grouped FULL JOIN members USING(parent_from,digest)
 WHERE grouped.first_from IS NULL OR members.first_from IS NULL OR NOT grouped.same_class OR NOT grouped.same_index
 OR grouped.first_from<>members.first_from OR grouped.last_from<>members.last_from
 OR grouped.is_array_index<>members.is_array_index OR grouped.array_index<>members.array_index)`, scope).Scan(&count, &valid); err != nil {
		return 0, err
	}
	if !valid {
		return 0, domain.ErrIntegrity
	}
	return count, ctx.Err()
}

func (reader *postgresDocEventDisplayWork) ReadToolKeyMembers(ctx context.Context, tree domain.DocToolTreeSource, source domain.DocToolKeyMapSource, parent int, after *domain.DocToolKeyMember, limit int) (result []domain.DocToolKeyMember, failure error) {
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
	if ctx.Value(repositoryTxKey{}) != reader.owner || !reader.storage.InReadOnlyTransaction(ctx) || reader.tree == nil ||
		*reader.tree != tree || reader.keys == nil || *reader.keys != source {
		return nil, domain.ErrConflict
	}
	return reader.storage.readDocToolKeyMembers(ctx, reader.source, tree, source, parent, after, limit)
}

func (storage *PostgresStore) readDocToolKeyMembers(ctx context.Context, owner domain.ContentHash, tree domain.DocToolTreeSource, source domain.DocToolKeyMapSource, parent int, after *domain.DocToolKeyMember, limit int) (result []domain.DocToolKeyMember, failure error) {
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
	if source.Validate(tree) != nil || parent < tree.Root.From || parent >= tree.Root.To || limit <= 0 || limit > 64 ||
		after != nil && (after.Validate(tree) != nil || after.Last.Node.Parent != parent) {
		return nil, domain.ErrValidation
	}
	if err := validateDocToolKeyCatalog(ctx, storage.db(ctx), owner, tree, source); err != nil {
		return nil, err
	}
	encoded, err := source.Encode(tree)
	if err != nil {
		return nil, err
	}
	scope := domain.HashContent(encoded)
	parentNode, err := readDocToolCanonicalNode(ctx, storage.db(ctx), tree, parent)
	if err != nil {
		if err == domain.ErrNotFound {
			return nil, domain.ErrValidation
		}
		return nil, err
	}
	if parentNode.Value.From != parent || parentNode.Type != domain.DocToolValueObject {
		return nil, domain.ErrValidation
	}
	if after != nil {
		raw, err := after.Last.Node.Encode(tree)
		if err != nil {
			return nil, err
		}
		selected, err := readDocToolCanonicalNode(ctx, storage.db(ctx), tree, after.Last.Node.Value.From)
		if err != nil {
			if err == domain.ErrNotFound {
				return nil, domain.ErrIntegrity
			}
			return nil, err
		}
		canonical, err := selected.Encode(tree)
		if err != nil || !bytes.Equal(raw, canonical) {
			return nil, domain.ErrIntegrity
		}
		var agrees bool
		if err := storage.db(ctx).QueryRow(ctx, `SELECT coalesce((SELECT member.first_from=$4 AND member.last_from=$5
 AND (NOT member.is_array_index)=(NOT $6::boolean) AND member.array_index=$7 AND observation.parent_from=member.parent_from
 AND observation.digest=member.digest AND observation.is_array_index=member.is_array_index AND observation.array_index=member.array_index
 AND observation.tree_scope=$8 AND observation.node_hash=$9
 AND initial.parent_from=member.parent_from AND initial.digest=member.digest
 AND initial.is_array_index=member.is_array_index AND initial.array_index=member.array_index AND initial.tree_scope=$8
 AND `+docToolKeyMemberExtrema+`
 FROM doc_tool_key_members_v1 member
 LEFT JOIN LATERAL (SELECT * FROM doc_tool_key_observations_v1 WHERE scope=member.scope AND node_from=member.last_from LIMIT 1) observation ON true
 LEFT JOIN LATERAL (SELECT * FROM doc_tool_key_observations_v1 WHERE scope=member.scope AND node_from=member.first_from LIMIT 1) initial ON true
 WHERE member.scope=$1 AND member.parent_from=$2 AND member.digest=$3
 AND (NOT member.is_array_index)=(NOT $6::boolean) AND member.array_index=$7 AND member.first_from=$4),false)`,
			scope, parent, after.Last.Digest, after.First, after.Last.Node.Value.From, after.Last.IsArrayIndex,
			int64(after.Last.ArrayIndex), source.Tree, domain.HashContent(raw)).Scan(&agrees); err != nil {
			return nil, err
		}
		if !agrees {
			return nil, domain.ErrIntegrity
		}
	}
	nonnumeric, index, first := false, int64(0), 0
	if after != nil {
		nonnumeric, index, first = !after.Last.IsArrayIndex, int64(after.Last.ArrayIndex), after.First
	}
	rows, err := storage.db(ctx).Query(ctx, `SELECT member.first_from,member.last_from,member.parent_from,
 substring(member.digest FROM 1 FOR 72),member.is_array_index,member.array_index,`+docToolKeyNodeColumns+`,
 observation.parent_from=member.parent_from AND observation.digest=member.digest
 AND observation.is_array_index=member.is_array_index AND observation.array_index=member.array_index
 AND observation.node_hash=node.node_hash AND observation.tree_scope=$2
 AND initial.node_from IS NOT NULL AND initial.parent_from=member.parent_from AND initial.digest=member.digest
 AND initial.is_array_index=member.is_array_index AND initial.array_index=member.array_index AND initial.tree_scope=$2
 AND `+docToolKeyMemberExtrema+`
 FROM doc_tool_key_members_v1 member
 LEFT JOIN LATERAL(SELECT observation.* FROM doc_tool_key_observations_v1 observation
   WHERE observation.scope=member.scope AND observation.node_from=member.last_from LIMIT 1) observation ON true
 LEFT JOIN LATERAL(SELECT node.* FROM doc_tool_tree_nodes_v1 node
   WHERE node.scope=observation.tree_scope AND node.node_from=observation.node_from LIMIT 1) node ON true
 LEFT JOIN LATERAL(SELECT initial.* FROM doc_tool_key_observations_v1 initial
   WHERE initial.scope=member.scope AND initial.node_from=member.first_from LIMIT 1) initial ON true
 WHERE member.scope=$1 AND member.parent_from=$3
 AND (NOT member.is_array_index,member.array_index,member.first_from)>($4::boolean,$5::bigint,$6::bigint)
 ORDER BY NOT member.is_array_index,member.array_index,member.first_from LIMIT $7`,
		scope, source.Tree, parent, nonnumeric, index, first, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var member domain.DocToolKeyMember
		var selected postgresDocToolKeyNode
		var last, storedParent int
		var numeric int64
		var agrees *bool
		prefix := []any{&member.First, &last, &storedParent, &member.Last.Digest, &member.Last.IsArrayIndex, &numeric}
		if err := rows.Scan(append(append(prefix, selected.destinations()...), &agrees)...); err != nil {
			return nil, err
		}
		node, err := selected.canonical(tree)
		if err != nil || agrees == nil || !*agrees || numeric < 0 || numeric > 4294967294 || storedParent != parent || node.Parent != parent || node.Value.From != last {
			return nil, domain.ErrIntegrity
		}
		member.Last.Node, member.Last.ArrayIndex = node, uint32(numeric)
		if member.Validate(tree) != nil || after != nil && domain.CompareDocToolKeyMembers(*after, member) >= 0 {
			return nil, domain.ErrIntegrity
		}
		result = append(result, member)
		after = &result[len(result)-1]
	}
	return result, rows.Err()
}
