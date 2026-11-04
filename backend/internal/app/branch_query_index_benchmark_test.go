package app

import (
	"context"
	"fmt"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func BenchmarkBranchCodeProjection(b *testing.B) {
	for _, size := range []struct{ branches, events int }{{32, 256}, {128, 512}} {
		b.Run(fmt.Sprintf("branches=%d/history=%d", size.branches, size.branches*size.events), func(b *testing.B) {
			repo := hh("indexed-query-benchmark")
			refs := make([]domain.Ref, size.branches)
			history := make([]domain.HistoryEvent, 0, size.branches*size.events)
			for branch := range refs {
				identity := fmt.Sprintf("branch-%d", branch)
				for event := range size.events {
					target := hh(fmt.Sprintf("%d/%d", branch, event))
					history = append(history, domain.HistoryEvent{ID: fmt.Sprintf("%032x", len(history)+1), RepoID: string(repo), BranchID: identity, Branch: identity, Kind: "position", Target: target, GitAfter: effectiveOID(1)})
					refs[branch] = domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: identity, BranchID: identity, Target: target}
				}
			}
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				evidence := &codeEvidence{repo: repo}
				index, err := newBranchHistoryIndex(ctx, repo, history)
				if err != nil {
					b.Fatal(err)
				}
				for _, ref := range refs {
					got, err := resolvedBranchCodeFromIndex(ctx, ref, index, evidence)
					if err != nil || got != effectiveOID(1) {
						b.Fatalf("projection = %q, %v", got, err)
					}
				}
			}
		})
	}
}
