package app

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func BenchmarkGraphBundleDecode(benchmark *testing.B) {
	ctx := context.Background()
	repo := hh("synthetic graph bundle capacity")
	for _, snapshots := range []int{100, 672, 1024} {
		for _, branches := range []int{1, 8, 32} {
			benchmark.Run(fmt.Sprintf("snapshots=%d/branches=%d", snapshots, branches), func(benchmark *testing.B) {
				view := graphProjectionEncodingView(repo, branches)
				view.Snapshots = view.Snapshots[:snapshots]
				tip := view.Snapshots[snapshots-1].ID
				view.History[0].Target = tip
				for index := range view.Refs {
					view.Refs[index].Target = tip
				}
				view.Semantics = domain.ProjectContextSemantics(view.Snapshots, view.History)
				graph, err := domain.ProjectGraphState(view, "main", "")
				if err != nil {
					benchmark.Fatal(err)
				}
				view.Graph = &graph
				parts, err := domain.EncodeGraphViewProjection(ctx, repo, view)
				if errors.Is(err, domain.ErrRepositoryViewSourceLimit) {
					benchmark.Skip("synthetic complete bundle exceeds safety bound; no serving capacity claimed")
				}
				if err != nil {
					benchmark.Fatal(err)
				}
				var bytes int64
				for _, part := range parts {
					bytes += int64(len(part.Value))
				}
				benchmark.ReportAllocs()
				benchmark.SetBytes(bytes)
				benchmark.ResetTimer()
				for range benchmark.N {
					decoded, err := domain.DecodeGraphViewProjection(ctx, repo, parts)
					if err != nil || len(decoded.Snapshots) != snapshots {
						benchmark.Fatal(err)
					}
				}
				benchmark.ReportMetric(float64(bytes), "payload-bytes")
			})
		}
	}
}
