package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestMemoryReadRejectsTruncatedBranchSelectionWithoutCodeClaims(test *testing.T) {
	for _, kind := range []string{"legacy_fact", "rationale"} {
		test.Run(kind, func(test *testing.T) {
			ctx := context.Background()
			captured, request := memoryReadFixture(test, 1, 32)
			state := "review"
			if kind == "rationale" {
				digest := domain.MemoryDigest{SnapshotID: request.Selection.SnapshotID, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{{
					SourceSnapshot: request.Selection.SnapshotID,
					Claims:         []domain.MemoryClaim{{Kind: kind, Text: "historical rationale"}},
				}}}
				replaceMemoryReadDigest(test, captured, digest)
				state = "retained"
			}
			request.Selection.Branch = "main"
			request.Selection.CodeCommit = effectiveOID(5000)
			captured.selection, captured.reader.pinsCaptured = request.Selection, true
			repo := string(captured.reader.repo)
			receipt := domain.HistoryEvent{
				ID: strings.Repeat("1", 32), RepoID: repo, Kind: "pr-merge", Branch: "main",
				BranchID: domain.LegacyContextBranchID(repo, "main"), SourceBranchID: domain.LegacyContextBranchID(repo, "feature"),
				Source: request.Selection.SnapshotID, Target: request.Selection.SnapshotID, CreatedAt: time.Unix(1, 0).UTC(),
				PR: &domain.PullRequestMerge{Number: 1, BaseBranch: "main", HeadBranch: "feature", HeadSHA: effectiveOID(1), MergeSHA: effectiveOID(1)},
			}
			completion := receipt
			completionHash := sha256.Sum256([]byte(receipt.ID + ":completed"))
			completion.ID, completion.PRCompleted = fmt.Sprintf("%x", completionHash[:16]), true
			for _, event := range []domain.HistoryEvent{receipt, completion} {
				raw, err := json.Marshal(event)
				if err != nil {
					test.Fatal(err)
				}
				captured.graph.entries = append(captured.graph.entries, domain.RepositoryViewSourceEntry{Kind: "history", Key: event.ID, Sequence: 1, Value: raw})
			}
			input, err := captured.Export(ctx)
			if err != nil {
				test.Fatal(err)
			}
			restored, err := RestoreCapturedEffectiveMemory(ctx, input, memoryReadLongFactory{})
			if err != nil {
				test.Fatal("legacy restore rejected bounded selection", err)
			}
			page, err := restored.Project(ctx, request)
			if err != nil {
				test.Fatal("legacy projection rejected bounded selection", err)
			}
			if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Kind != kind || page.Items[0].Code != nil || page.Items[0].State != state {
				test.Fatalf("fixture must retain one non-code item: %+v", page)
			}
			if page.Inclusion == nil || len(page.Inclusion.Merges) != 1 || page.Inclusion.Merges[0].State != "review" || page.Inclusion.Merges[0].Reason != "git_evidence_pending" {
				test.Fatalf("legacy selection did not preserve its review state: %+v", page.Inclusion)
			}
			partial, err := BuildMemoryRead(ctx, input, memoryReadLongFactory{})
			if !errors.Is(err, domain.ErrMemoryProjectionLimit) || !reflect.DeepEqual(partial, domain.MemoryReadBundle{}) {
				test.Fatal("truncated branch selection without code claims was sealed", err)
			}
			again, err := restored.Project(ctx, request)
			if err != nil || !reflect.DeepEqual(again, page) {
				test.Fatal("strict preparation changed legacy projection", err)
			}
			complete, err := BuildMemoryRead(ctx, input, memoryReadLongFactory{missing: true})
			if err != nil || complete.Header.View.Total != 1 {
				test.Fatal("observed missing branch evidence was treated as selection exhaustion", err)
			}
			if err := domain.ValidateMemoryReadBundle(ctx, complete); err != nil {
				test.Fatal("missing-evidence result is invalid", err)
			}
		})
	}
}
