package repositoryquery

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type graphProjectionCommitOnlyEvidence struct {
	CapturedEvidenceReader
	test        *testing.T
	commits     evidencequery.GitTreeReader
	commitReads int
}

func (reader *graphProjectionCommitOnlyEvidence) GetGitCommitTree(ctx context.Context, repo domain.ContentHash, origin, commit string) (domain.GitCommitTree, error) {
	reader.commitReads++
	return reader.commits.GetGitCommitTree(ctx, repo, origin, commit)
}

func (reader *graphProjectionCommitOnlyEvidence) GetGitTreeNode(context.Context, domain.ContentHash, string, string) (domain.GitTreeNode, error) {
	reader.test.Helper()
	reader.test.Fatal("graph projection depends on excluded git_tree payloads")
	return domain.GitTreeNode{}, domain.ErrIntegrity
}

func (reader *graphProjectionCommitOnlyEvidence) GetGitDelta(context.Context, domain.ContentHash, string, string, string) (domain.GitCommitDelta, error) {
	reader.test.Helper()
	reader.test.Fatal("graph projection depends on excluded git_delta payloads")
	return domain.GitCommitDelta{}, domain.ErrIntegrity
}

func TestGraphProjectionSourceProfilePreservesContributionsWithoutFileEvidence(test *testing.T) {
	for _, captured := range []bool{false, true} {
		for _, historical := range []bool{false, true} {
			test.Run(fmt.Sprintf("captured=%t/historical=%t", captured, historical), func(test *testing.T) {
				baseline := newLegacyProjectFixture(test)
				fixture := newLegacyProjectFixture(test)
				baseline.projection.captured, fixture.projection.captured = captured, captured
				position := ""
				if historical {
					position = fixture.position
				}
				want, err := baseline.projection.Project(context.Background(), baseline.view, position)
				if err != nil {
					test.Fatal(err)
				}
				guard := &graphProjectionCommitOnlyEvidence{test: test, commits: fixture.trees}
				fixture.projection.trees, fixture.projection.deltas = guard, guard
				got, err := fixture.projection.Project(context.Background(), fixture.view, position)
				if err != nil || !reflect.DeepEqual(got, want) {
					test.Fatal("file evidence exclusion changed graph semantics", err)
				}
				if historical {
					if guard.commitReads != 0 || fixture.evidence.prCalls != 0 || fixture.evidence.gitCalls != 0 {
						test.Fatal("historical selection accessed current evidence")
					}
				} else {
					if guard.commitReads != 4 || fixture.evidence.prCalls != 1 || fixture.evidence.gitCalls != 1 || len(got.BranchContexts["main"].Merges) != 2 || len(got.Integrations) != 1 {
						test.Fatal("fixture did not exercise commit ancestry and both contribution types")
					}
				}
			})
		}
	}
}

func TestGraphProjectionSourceProfileCapturedEntryPointsNeverReadFileEvidence(test *testing.T) {
	checkpoint, view, reader, resources := capturedQueryFixture(test)
	view.History[1].GitAfter = legacyProjectOID(4)
	guard := &graphProjectionCommitOnlyEvidence{CapturedEvidenceReader: reader, test: test, commits: reader}
	want, err := ProjectCapturedRepositoryView(context.Background(), checkpoint, view, reader, resources)
	if err != nil {
		test.Fatal(err)
	}
	got, err := ProjectCapturedRepositoryView(context.Background(), checkpoint, view, guard, resources)
	if err != nil || !reflect.DeepEqual(got, want) || guard.commitReads == 0 {
		test.Fatal("captured projection changed semantics or bypassed commit evidence", err)
	}
	guard.commitReads = 0
	position := view.History[1].ID
	wantPosition, err := ProjectCapturedGraph(context.Background(), checkpoint, view, reader, position, resources)
	if err != nil {
		test.Fatal(err)
	}
	gotPosition, err := ProjectCapturedGraph(context.Background(), checkpoint, view, guard, position, resources)
	if err != nil || !reflect.DeepEqual(gotPosition, wantPosition) || guard.commitReads != 0 {
		test.Fatal("captured position changed semantics or accessed current proof data", err)
	}
}
