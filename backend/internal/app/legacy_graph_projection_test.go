package app

import (
	"context"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestLegacyGraphProjectionRetainsBoundSourceWithoutServiceCallback(test *testing.T) {
	reader, view, expected := capturedPRGraphFixture(test)
	service := &Service{projectionEvidence: reader}
	projection, err := service.legacyGraphProjection()
	if err != nil {
		test.Fatal(err)
	}
	service.projectionEvidence = &capturedGraphEvidence{metadata: domain.Repo{ID: reader.metadata.ID}, err: domain.ErrIntegrity}
	service.meta = &poisonedGraphMetadata{}
	actual, err := projection.Project(context.Background(), view, "")
	if err != nil || !reflect.DeepEqual(actual, expected) {
		test.Fatal("actual projection rediscovered a parent Service or changed PR inclusion", err)
	}
	if service.branchCache.Stats().Entries != 0 || service.graphCache.Stats().Entries != 0 {
		test.Fatal("captured read wrote process projection caches")
	}
}

func TestLegacyGraphProjectionScopePreservesPinnedAndWriteBoundaries(test *testing.T) {
	scope := &legacyGraphProjectionScope{}
	ctx := systemTestContext()
	if scope.IsPinnedRead(ctx) {
		test.Fatal("missing transaction state became cacheable")
	}
	state := &graphCacheReadState{readOnly: true}
	scope.state = state
	if !scope.IsPinnedRead(ctx) {
		test.Fatal("read-only snapshot was not recognized")
	}
	writing := context.WithValue(ctx, afterCommitKey{}, &afterCommitActions{})
	if scope.IsPinnedRead(writing) {
		test.Fatal("write transaction became cacheable")
	}
	state.readOnly = false
	if scope.IsPinnedRead(ctx) {
		test.Fatal("unbound read became cacheable")
	}
}

func TestLegacyGraphPositionPreservesPureDataContract(test *testing.T) {
	_, view, expected := capturedPRGraphFixture(test)
	if len(expected.Positions) == 0 {
		test.Fatal("historical fixture has no selectable position")
	}
	position := expected.Positions[0].EventID
	want, err := domain.ProjectGraphState(view, view.DefaultBranch, position)
	if err != nil {
		test.Fatal(err)
	}
	want.BranchContexts = map[string]domain.BranchContext{}
	want.Integrations = []domain.GraphIntegration{}
	actual, err := (&Service{}).projectGraphState(context.Background(), view, position)
	if err != nil || !reflect.DeepEqual(actual, want) {
		test.Fatal("historical selection required live repository dependencies", err)
	}
}
