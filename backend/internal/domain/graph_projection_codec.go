package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"
)

type graphProjectionHeader struct {
	Version    int                           `json:"version"`
	Rules      int                           `json:"rules"`
	Projection string                        `json:"projection"`
	RepoID     ContentHash                   `json:"repo_id"`
	Parts      []graphProjectionPartIdentity `json:"parts"`
}

type graphProjectionPartIdentity struct {
	Kind   string      `json:"kind"`
	Key    string      `json:"key"`
	Digest ContentHash `json:"digest"`
}

type graphProjectionValue[Value any] struct {
	Value Value `json:"value"`
}

type graphProjectionField struct {
	key    string
	value  any
	decode func(context.Context, []byte, *graphProjectionBudget) error
}

type graphProjectionOperations struct {
	Births []GraphBirth           `json:"births"`
	Merges []graphProjectionMerge `json:"merges"`
}

type graphProjectionMerge struct {
	ID                string        `json:"id"`
	EventID           string        `json:"event_id"`
	Before            ContentHash   `json:"before"`
	After             ContentHash   `json:"after"`
	Source            ContentHash   `json:"source"`
	SourceArms        []ContentHash `json:"source_arms"`
	Branch            string        `json:"branch"`
	Scope             string        `json:"scope"`
	From              string        `json:"from"`
	CreatedAt         time.Time     `json:"created_at"`
	PRNumber          int           `json:"pr_number,omitempty"`
	HistoricalOnly    bool          `json:"historical_only"`
	Withdrawn         bool          `json:"withdrawn"`
	RepresentedGrafts []ContentHash `json:"represented_grafts"`
	RedirectChildren  []ContentHash `json:"redirect_children"`
	SourceBirth       string        `json:"source_birth,omitempty"`
	LifecycleBirth    string        `json:"lifecycle_birth,omitempty"`
	Identity          string        `json:"identity"`
}

func graphProjectionTypedField[Value any](key string, value *Value) graphProjectionField {
	return graphProjectionField{
		key: key, value: graphProjectionValue[*Value]{Value: value},
		decode: func(ctx context.Context, raw []byte, budget *graphProjectionBudget) error {
			var row graphProjectionValue[Value]
			if err := graphProjectionJSONSchema(ctx, raw, reflect.TypeFor[graphProjectionValue[Value]](), budget); err != nil {
				return err
			}
			if err := DecodeRepositorySourceJSON(ctx, raw, &row); err != nil {
				return err
			}
			*value = row.Value
			return nil
		},
	}
}

func graphProjectionFields(graph *GraphState, operations *graphProjectionOperations) []graphProjectionField {
	return []graphProjectionField{
		graphProjectionTypedField("integrations", &graph.Integrations),
		graphProjectionTypedField("branch_contexts", &graph.BranchContexts),
		graphProjectionTypedField("branch_snapshots", &graph.BranchSnapshots),
		graphProjectionTypedField("continuations", &graph.Continuations),
		graphProjectionTypedField("operations", operations),
		graphProjectionTypedField("version", &graph.Version),
		graphProjectionTypedField("revision", &graph.Revision),
		graphProjectionTypedField("position_event", &graph.PositionEvent),
		graphProjectionTypedField("primary_branch", &graph.PrimaryBranch),
		graphProjectionTypedField("snapshot_ids", &graph.SnapshotIDs),
		graphProjectionTypedField("graph_ids", &graph.GraphIDs),
		graphProjectionTypedField("committed_ids", &graph.CommittedIDs),
		graphProjectionTypedField("historical_ids", &graph.HistoricalIDs),
		graphProjectionTypedField("shared_ids", &graph.SharedIDs),
		graphProjectionTypedField("pushed_ids", &graph.PushedIDs),
		graphProjectionTypedField("unpushed_ids", &graph.UnpushedIDs),
		graphProjectionTypedField("uncommitted_ids", &graph.UncommittedIDs),
		graphProjectionTypedField("tagged_ids", &graph.TaggedIDs),
		graphProjectionTypedField("archived_only_ids", &graph.ArchivedOnlyIDs),
		graphProjectionTypedField("ahead_ids", &graph.AheadIDs),
		graphProjectionTypedField("ahead_tips", &graph.AheadTips),
		graphProjectionTypedField("markers", &graph.Markers),
		graphProjectionTypedField("branch_heads", &graph.BranchHeads),
		graphProjectionTypedField("ref_scopes", &graph.RefScopes),
		graphProjectionTypedField("snapshot_scopes", &graph.SnapshotScopes),
		graphProjectionTypedField("scope_labels", &graph.ScopeLabels),
		graphProjectionTypedField("hold", &graph.Hold),
		graphProjectionTypedField("orphan_sessions", &graph.OrphanSessions),
		graphProjectionTypedField("hold_counts", &graph.HoldCounts),
		graphProjectionTypedField("positions", &graph.Positions),
		graphProjectionTypedField("previous", &graph.Previous),
	}
}

func EncodeGraphProjection(ctx context.Context, repo ContentHash, graph GraphState) ([]GraphProjectionPart, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ValidateContentHash(repo) != nil || graph.Version != 1 || graph.PositionEvent != "" || graph.ReadGeneration != nil {
		return nil, ErrIntegrity
	}
	budget := graphProjectionBudget{ctx: ctx}
	if err := budget.measure(reflect.ValueOf(graph)); err != nil {
		return nil, err
	}
	operations, err := graphProjectionEncodeOperations(ctx, graph.Operations)
	if err != nil {
		return nil, err
	}
	return graphProjectionEncodeFields(ctx, repo, GraphProjectionRules, "graph-v1", graphProjectionFields(&graph, &operations))
}

func graphProjectionEncodeOperations(ctx context.Context, graph GraphOperations) (graphProjectionOperations, error) {
	operations := graphProjectionOperations{Births: graph.Births}
	if graph.Merges != nil {
		operations.Merges = make([]graphProjectionMerge, len(graph.Merges))
	}
	for index, merge := range graph.Merges {
		if err := ctx.Err(); err != nil {
			return graphProjectionOperations{}, err
		}
		operations.Merges[index] = graphProjectionMerge{
			ID: merge.ID, EventID: merge.EventID, Before: merge.Before, After: merge.After,
			Source: merge.Source, SourceArms: merge.SourceArms, Branch: merge.Branch, Scope: merge.Scope,
			From: merge.From, CreatedAt: merge.CreatedAt, PRNumber: merge.PRNumber,
			HistoricalOnly: merge.HistoricalOnly, Withdrawn: merge.Withdrawn,
			RepresentedGrafts: merge.RepresentedGrafts, RedirectChildren: merge.RedirectChildren,
			SourceBirth: merge.SourceBirth, LifecycleBirth: merge.LifecycleBirth, Identity: merge.identity,
		}
	}
	return operations, nil
}

func graphProjectionEncodeFields(ctx context.Context, repo ContentHash, rules int, projection string, fields []graphProjectionField) ([]GraphProjectionPart, error) {
	header := graphProjectionHeader{Version: 1, Rules: rules, Projection: projection, RepoID: repo, Parts: make([]graphProjectionPartIdentity, 0, len(fields))}
	parts := make([]GraphProjectionPart, 0, len(fields)+1)
	size := 0
	for _, field := range fields {
		part, err := graphProjectionEncodePart(ctx, "field", field.key, field.value, &size)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
		header.Parts = append(header.Parts, graphProjectionPartIdentity{Kind: part.Kind, Key: part.Key, Digest: HashContent(part.Value)})
	}
	part, err := graphProjectionEncodePart(ctx, "header", "view", header, &size)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append(parts, part), nil
}

func graphProjectionEncodePart(ctx context.Context, kind, key string, value any, size *int) (GraphProjectionPart, error) {
	budget := graphProjectionBudget{ctx: ctx, size: *size + len(kind) + len(key) + 128}
	if err := budget.measure(reflect.ValueOf(value)); err != nil {
		return GraphProjectionPart{}, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return GraphProjectionPart{}, fmt.Errorf("%w: graph projection JSON: %v", ErrIntegrity, err)
	}
	*size += len(kind) + len(key) + len(raw) + 128
	if *size > MaxPendingProjectionOutputBytes {
		return GraphProjectionPart{}, ErrRepositoryViewSourceLimit
	}
	return GraphProjectionPart{Kind: kind, Key: key, Value: raw}, ctx.Err()
}

func graphProjectionDecodeHeader(ctx context.Context, part GraphProjectionPart) (graphProjectionHeader, error) {
	return graphProjectionDecodeFieldHeader(ctx, part, GraphProjectionRules, "graph-v1", graphProjectionFields(&GraphState{}, &graphProjectionOperations{}))
}

func graphProjectionDecodeFieldHeader(ctx context.Context, part GraphProjectionPart, rules int, projection string, fields []graphProjectionField) (graphProjectionHeader, error) {
	var header graphProjectionHeader
	if err := ctx.Err(); err != nil {
		return header, err
	}
	if len(part.Value) > MaxPendingProjectionOutputBytes-len("headerview")-128 {
		return header, ErrRepositoryViewSourceLimit
	}
	if part.Kind != "header" || part.Key != "view" {
		return header, ErrIntegrity
	}
	budget := graphProjectionBudget{ctx: ctx}
	if err := graphProjectionJSONSchema(ctx, part.Value, reflect.TypeFor[graphProjectionHeader](), &budget); err != nil {
		return header, err
	}
	if err := DecodeRepositorySourceJSON(ctx, part.Value, &header); err != nil {
		return header, err
	}
	if header.Version != 1 || header.Rules != rules || header.Projection != projection || ValidateContentHash(header.RepoID) != nil || len(header.Parts) != len(fields) {
		return graphProjectionHeader{}, ErrIntegrity
	}
	wanted := make(map[string]bool, len(fields))
	for _, field := range fields {
		wanted[field.key] = true
	}
	for _, identity := range header.Parts {
		if err := ctx.Err(); err != nil {
			return graphProjectionHeader{}, err
		}
		if identity.Kind != "field" || !wanted[identity.Key] || ValidateContentHash(identity.Digest) != nil {
			return graphProjectionHeader{}, ErrIntegrity
		}
		delete(wanted, identity.Key)
	}
	return header, nil
}

func GraphProjectionPartKeys(header GraphProjectionPart) ([]string, []string, error) {
	index, err := graphProjectionDecodeHeader(context.Background(), header)
	if err != nil {
		return nil, nil, err
	}
	kinds, keys := make([]string, len(index.Parts)), make([]string, len(index.Parts))
	for position, identity := range index.Parts {
		kinds[position], keys[position] = identity.Kind, identity.Key
	}
	return kinds, keys, nil
}

func DecodeGraphProjection(ctx context.Context, repo ContentHash, parts []GraphProjectionPart) (GraphState, error) {
	var graph GraphState
	var operations graphProjectionOperations
	if err := graphProjectionDecodeFields(ctx, repo, parts, GraphProjectionRules, "graph-v1", graphProjectionFields(&graph, &operations)); err != nil {
		return GraphState{}, err
	}
	if graph.Version != 1 || graph.PositionEvent != "" || graph.ReadGeneration != nil {
		return GraphState{}, ErrIntegrity
	}
	if err := graphProjectionDecodeOperations(ctx, &graph, operations); err != nil {
		return GraphState{}, err
	}
	return graph, nil
}

func graphProjectionDecodeFields(ctx context.Context, repo ContentHash, parts []GraphProjectionPart, rules int, projection string, fields []graphProjectionField) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ValidateContentHash(repo) != nil {
		return ErrIntegrity
	}
	if len(parts) > MaxPendingProjectionOutputRows {
		return ErrRepositoryViewSourceLimit
	}
	size := 0
	for _, part := range parts {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, length := range []int{len(part.Kind), len(part.Key), len(part.Value), 128} {
			if length > MaxPendingProjectionOutputBytes-size {
				return ErrRepositoryViewSourceLimit
			}
			size += length
		}
	}
	rows := make(map[[2]string]GraphProjectionPart, len(parts))
	for _, part := range parts {
		if err := ctx.Err(); err != nil {
			return err
		}
		identity := [2]string{part.Kind, part.Key}
		if _, exists := rows[identity]; exists || (part.Kind != "field" && part.Kind != "header") || part.Key == "" || len(part.Key) > MaxRepositoryViewSourceKeyBytes {
			return ErrIntegrity
		}
		rows[identity] = part
	}
	header, err := graphProjectionDecodeFieldHeader(ctx, rows[[2]string{"header", "view"}], rules, projection, fields)
	if err != nil {
		return err
	}
	if header.RepoID != repo || len(parts) != len(header.Parts)+1 {
		return ErrIntegrity
	}
	byKey := make(map[string]graphProjectionField, len(fields))
	for _, field := range fields {
		byKey[field.key] = field
	}
	budget := graphProjectionBudget{ctx: ctx}
	for _, identity := range header.Parts {
		part, exists := rows[[2]string{identity.Kind, identity.Key}]
		if !exists {
			return ErrIntegrity
		}
		field := byKey[identity.Key]
		if err := field.decode(ctx, part.Value, &budget); err != nil {
			return fmt.Errorf("%w: graph projection field %s", err, identity.Key)
		}
		canonicalSize := 0
		canonical, err := graphProjectionEncodePart(ctx, part.Kind, part.Key, field.value, &canonicalSize)
		if err != nil {
			return err
		}
		if HashContent(canonical.Value) != identity.Digest {
			return fmt.Errorf("%w: graph projection digest %s", ErrIntegrity, identity.Key)
		}
	}
	return ctx.Err()
}

func graphProjectionDecodeOperations(ctx context.Context, graph *GraphState, operations graphProjectionOperations) error {
	graph.Operations.Births = operations.Births
	if operations.Merges != nil {
		graph.Operations.Merges = make([]GraphMerge, len(operations.Merges))
	}
	for index, merge := range operations.Merges {
		if err := ctx.Err(); err != nil {
			return err
		}
		graph.Operations.Merges[index] = GraphMerge{
			ID: merge.ID, EventID: merge.EventID, Before: merge.Before, After: merge.After,
			Source: merge.Source, SourceArms: merge.SourceArms, Branch: merge.Branch, Scope: merge.Scope,
			From: merge.From, CreatedAt: merge.CreatedAt, PRNumber: merge.PRNumber,
			HistoricalOnly: merge.HistoricalOnly, Withdrawn: merge.Withdrawn,
			RepresentedGrafts: merge.RepresentedGrafts, RedirectChildren: merge.RedirectChildren,
			SourceBirth: merge.SourceBirth, LifecycleBirth: merge.LifecycleBirth, identity: merge.Identity,
		}
	}
	return ctx.Err()
}
