package domain

import (
	"context"
	"reflect"
)

type graphViewProjectionSnapshotValue Snapshot

type graphViewProjectionSnapshot struct {
	graphViewProjectionSnapshotValue
	Branches     []string      `json:"branches"`
	GraftParents []ContentHash `json:"graft_parents"`
	Models       []string      `json:"models"`
}

type graphViewProjectionCreation struct {
	GitCreation
	Command []string `json:"command"`
}

type graphViewProjectionHistory struct {
	HistoryEvent
	Creation *graphViewProjectionCreation `json:"creation,omitempty"`
}

type graphViewProjectionRows struct {
	snapshots []graphViewProjectionSnapshot
	history   []graphViewProjectionHistory
}

func graphViewProjectionFields(view *RepositoryView, operations *graphProjectionOperations, rows *graphViewProjectionRows) []graphProjectionField {
	fields := graphProjectionFields(view.Graph, operations)
	for index := range fields {
		fields[index].key = "graph." + fields[index].key
	}
	return append(fields,
		graphProjectionTypedField("archived_sessions", &view.ArchivedSessions),
		graphProjectionTypedField("default_branch", &view.DefaultBranch),
		graphProjectionTypedField("semantics", &view.Semantics),
		graphProjectionTypedField("revision", &view.Revision),
		graphProjectionTypedField("refs", &view.Refs),
		graphProjectionTypedField("snapshots", &rows.snapshots),
		graphProjectionTypedField("reflog", &view.Reflog),
		graphProjectionTypedField("history", &rows.history),
		graphProjectionTypedField("pending", &view.Pending),
		graphProjectionTypedField("unsync", &view.Unsync),
	)
}

func EncodeGraphViewProjection(ctx context.Context, repo ContentHash, view RepositoryView) ([]GraphProjectionPart, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ValidateContentHash(repo) != nil || view.Graph == nil || view.ReadGeneration != nil || view.Graph.ReadGeneration != nil ||
		view.Graph.Version != 1 || view.Graph.PositionEvent != "" || view.Graph.Revision != view.Revision {
		return nil, ErrIntegrity
	}
	budget := graphProjectionBudget{ctx: ctx}
	if err := budget.measure(reflect.ValueOf(view)); err != nil {
		return nil, err
	}
	if err := graphViewProjectionScope(ctx, repo, view); err != nil {
		return nil, err
	}
	operations, err := graphProjectionEncodeOperations(ctx, view.Graph.Operations)
	if err != nil {
		return nil, err
	}
	var rows graphViewProjectionRows
	if view.Snapshots != nil {
		rows.snapshots = make([]graphViewProjectionSnapshot, len(view.Snapshots))
	}
	for index, snapshot := range view.Snapshots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rows.snapshots[index] = graphViewProjectionSnapshot{graphViewProjectionSnapshotValue: graphViewProjectionSnapshotValue(snapshot), Branches: snapshot.Branches, GraftParents: snapshot.GraftParents, Models: snapshot.Models}
	}
	if view.History != nil {
		rows.history = make([]graphViewProjectionHistory, len(view.History))
	}
	for index, event := range view.History {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rows.history[index].HistoryEvent = event
		if event.Creation != nil {
			rows.history[index].Creation = &graphViewProjectionCreation{GitCreation: *event.Creation, Command: event.Creation.Command}
		}
	}
	return graphProjectionEncodeFields(ctx, repo, GraphViewProjectionRules, "graph-view-v2", graphViewProjectionFields(&view, &operations, &rows))
}

func DecodeGraphViewProjection(ctx context.Context, repo ContentHash, parts []GraphProjectionPart) (RepositoryView, error) {
	view := RepositoryView{Graph: &GraphState{}}
	var operations graphProjectionOperations
	var rows graphViewProjectionRows
	if err := graphProjectionDecodeFields(ctx, repo, parts, GraphViewProjectionRules, "graph-view-v2", graphViewProjectionFields(&view, &operations, &rows)); err != nil {
		return RepositoryView{}, err
	}
	if view.Graph.Version != 1 || view.Graph.PositionEvent != "" || view.Graph.Revision != view.Revision {
		return RepositoryView{}, ErrIntegrity
	}
	if err := graphProjectionDecodeOperations(ctx, view.Graph, operations); err != nil {
		return RepositoryView{}, err
	}
	if rows.snapshots != nil {
		view.Snapshots = make([]Snapshot, len(rows.snapshots))
	}
	for index, row := range rows.snapshots {
		if err := ctx.Err(); err != nil {
			return RepositoryView{}, err
		}
		snapshot := Snapshot(row.graphViewProjectionSnapshotValue)
		snapshot.Branches, snapshot.GraftParents, snapshot.Models = row.Branches, row.GraftParents, row.Models
		view.Snapshots[index] = snapshot
	}
	if rows.history != nil {
		view.History = make([]HistoryEvent, len(rows.history))
	}
	for index, row := range rows.history {
		if err := ctx.Err(); err != nil {
			return RepositoryView{}, err
		}
		event := row.HistoryEvent
		if row.Creation != nil {
			creation := row.Creation.GitCreation
			creation.Command = row.Creation.Command
			event.Creation = &creation
		}
		view.History[index] = event
	}
	if err := graphViewProjectionScope(ctx, repo, view); err != nil {
		return RepositoryView{}, err
	}
	return view, nil
}

func GraphViewProjectionPartKeys(header GraphProjectionPart) ([]string, []string, error) {
	view := RepositoryView{Graph: &GraphState{}}
	index, err := graphProjectionDecodeFieldHeader(context.Background(), header, GraphViewProjectionRules, "graph-view-v2", graphViewProjectionFields(&view, &graphProjectionOperations{}, &graphViewProjectionRows{}))
	if err != nil {
		return nil, nil, err
	}
	kinds, keys := make([]string, len(index.Parts)), make([]string, len(index.Parts))
	for position, identity := range index.Parts {
		kinds[position], keys[position] = identity.Kind, identity.Key
	}
	return kinds, keys, nil
}

func graphViewProjectionScope(ctx context.Context, repo ContentHash, view RepositoryView) error {
	for _, snapshot := range view.Snapshots {
		if snapshot.RepoID != repo {
			return ErrIntegrity
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	for _, ref := range view.Refs {
		if ref.RepoID != repo {
			return ErrIntegrity
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	for _, event := range view.History {
		if event.RepoID != string(repo) {
			return ErrIntegrity
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	for _, pending := range view.Pending {
		if pending.RepoID != repo {
			return ErrIntegrity
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	for _, unsync := range view.Unsync {
		if unsync.RepoID != repo {
			return ErrIntegrity
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	for _, archive := range view.ArchivedSessions {
		if archive.RepoID != repo {
			return ErrIntegrity
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	for _, cluster := range view.Graph.Hold {
		for _, tip := range cluster.Tips {
			if tip.RepoID != repo {
				return ErrIntegrity
			}
			if err := ctx.Err(); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}
