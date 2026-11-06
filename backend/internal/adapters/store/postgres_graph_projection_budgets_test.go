//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func graphProjectionLargeBudgetParts(t *testing.T, repo domain.ContentHash) (domain.RepositoryView, []domain.GraphProjectionPart) {
	t.Helper()
	view := domain.RepositoryView{Graph: &domain.GraphState{Version: 1}, DefaultBranch: "main"}
	for index := range 4096 {
		identifier := domain.HashContent([]byte(fmt.Sprintf("large-budget-%d", index)))
		view.Snapshots = append(view.Snapshots, domain.Snapshot{ID: identifier, RepoID: repo, DocHash: identifier,
			Message: strings.Repeat("metadata ", 128), Branches: []string{"main", "topic"}})
		view.Graph.SnapshotIDs = append(view.Graph.SnapshotIDs, identifier)
		view.Graph.GraphIDs = append(view.Graph.GraphIDs, identifier)
		view.Graph.CommittedIDs = append(view.Graph.CommittedIDs, identifier)
		view.Graph.SharedIDs = append(view.Graph.SharedIDs, identifier)
	}
	parts, err := domain.EncodeGraphViewProjection(context.Background(), repo, view)
	if err != nil {
		t.Fatal(err)
	}
	var size int
	for _, part := range parts {
		size += len(part.Value) + len(part.Kind) + len(part.Key) + 128
	}
	if size <= domain.MaxPendingProjectionOutputBytes || size >= domain.MaxGraphViewProjectionOutputBytes {
		t.Fatal("fixture is not between output byte bounds", size)
	}
	return view, parts
}

func TestGraphProjectionOutputBudgetsStayFamilySpecific(t *testing.T) {
	ctx := context.Background()
	repo := domain.HashContent([]byte(t.Name()))
	pending := (*PostgresStore)(nil).pendingProjection()
	graph := (*PostgresStore)(nil).graphProjection()
	if pending.outputBytes() != 4<<20 || pending.outputRows() != 20000 ||
		graph.outputBytes() != domain.MaxGraphViewProjectionOutputBytes || graph.outputRows() != domain.MaxGraphViewProjectionOutputParts {
		t.Fatal("projection families share output limits")
	}
	_, parts := graphProjectionLargeBudgetParts(t, repo)
	if err := graph.validateParts(ctx, repo, parts); err != nil {
		t.Fatal("graph store rejected large valid bundle", err)
	}
	if err := pending.validateParts(ctx, repo, parts); !errors.Is(err, domain.ErrRepositoryViewSourceLimit) {
		t.Fatal("pending store accepted graph-sized output", err)
	}
	for _, projection := range []projectionStore{pending, graph} {
		parts := make([]domain.PendingProjectionPart, projection.outputRows()+1)
		if err := projection.validateParts(ctx, repo, parts); !errors.Is(err, domain.ErrRepositoryViewSourceLimit) {
			t.Fatal("part-row budget not enforced", projection.spec.scope, err)
		}
	}
}

func TestPGGraphProjectionLargeBundlePublishesQualifiesAndServes(t *testing.T) {
	storage, ctx, repo := graphProjectionPG(t)
	job := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
	view, parts := graphProjectionLargeBudgetParts(t, repo)
	if err := storage.PublishGraphProjection(ctx, job, parts); err != nil {
		t.Fatal("large graph publication failed", err)
	}
	read, err := storage.ReadGraphProjection(ctx, repo)
	if err != nil || !read.Ready {
		t.Fatal("large candidate read failed", err)
	}
	limited := storage.graphProjection()
	limited.spec.maxOutputRows = len(parts) - 1
	err = storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		_, err := limited.readReadyPartsFrom(bound, storage.db(bound), repo, read.Dataset, read.Applied.Sequence)
		return err
	})
	if !errors.Is(err, domain.ErrRepositoryViewSourceLimit) {
		t.Fatal("candidate read ignored its configured part-row budget", err)
	}
	decoded, err := domain.DecodeGraphViewProjection(ctx, repo, read.Parts)
	if err != nil || !reflect.DeepEqual(decoded, view) {
		t.Fatal("large candidate changed", err)
	}
	if err := storage.ConfigureGraphQualification(ctx, repo, true); err != nil {
		t.Fatal(err)
	}
	serving := graphServingPromotePG(t, ctx, storage, read.QualificationKey())
	if err := domain.ValidateGraphReadQualification(ctx, serving); err != nil {
		t.Fatal("large serving qualification failed", err)
	}
	decoded, err = domain.DecodeGraphViewProjection(ctx, repo, serving.Parts)
	if err != nil || !reflect.DeepEqual(decoded, view) {
		t.Fatal("large serving bundle changed", err)
	}
}
