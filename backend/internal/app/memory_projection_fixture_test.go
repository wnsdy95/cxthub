package app

import (
	"context"
	"encoding/json"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"os"
	"slices"
	"strings"
	"testing"
)

type projectionFixture struct {
	Name, Root string
	Snapshots  []struct {
		ID, Summary                           string
		Parents, Grafts, Pinned, Facts, Tasks []string
		Fragments                             []struct {
			Source, Summary string
			Claims          []domain.MemoryClaim
		}
	}
	Want, Absent, Facts, Tasks, Claims []string
}

func TestMemoryProjectionSharedFixtures(t *testing.T) {
	raw, err := os.ReadFile("../../../schemas/testdata/memory-projection.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []projectionFixture
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			ctx := context.Background()
			hash := func(s string) domain.ContentHash { return domain.HashContent([]byte(s)) }
			hashes := func(items []string) []domain.ContentHash {
				var ids []domain.ContentHash
				for _, s := range items {
					ids = append(ids, hash(s))
				}
				return ids
			}
			repo := hash("fixture repository")
			svc, st := newFsckSvc(t)
			for _, input := range fixture.Snapshots {
				snap := domain.Snapshot{ID: hash(input.ID), RepoID: repo, DocHash: hash(input.ID), Parents: hashes(input.Parents), GraftParents: hashes(input.Grafts)}
				if len(snap.GraftParents) > 0 {
					snap.GraftSeq = 1
				}
				if input.Summary != "" || len(input.Fragments) > 0 {
					d := domain.MemoryDigest{SnapshotID: snap.ID, Summary: input.Summary, KeyFacts: input.Facts, OpenTasks: input.Tasks}
					for _, f := range input.Fragments {
						d.Fragments = append(d.Fragments, domain.MemoryFragment{SourceSnapshot: hash(f.Source), Summary: f.Summary, Claims: f.Claims})
					}
					if d.HasMemoryClaims() {
						d.ClaimsVersion = domain.MemoryClaimsVersion
					}
					if len(input.Pinned) > 0 {
						d.GraftCoverage = &domain.MemoryGraftCoverage{ProjectionVersion: domain.MemoryProjectionVersion, PinnedSources: hashes(input.Pinned)}
					}
					var err error
					snap.MemoryHash, err = st.PutMemory(ctx, repo, d)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := st.PutSnapshot(ctx, snap); err != nil {
					t.Fatal(err)
				}
			}
			result, err := svc.GetMemoryProjection(ctx, repo, hash(fixture.Root))
			if err != nil {
				t.Fatal(err)
			}
			if !result.Found {
				t.Fatal("no projection")
			}
			got := result.Digest
			for _, want := range fixture.Claims {
				count := 0
				for _, f := range got.Fragments {
					for _, c := range f.Claims {
						if c.Text == want {
							count++
						}
					}
				}
				if count != 1 || got.ClaimsVersion != 1 {
					t.Fatalf("typed claim %q count=%d version=%d", want, count, got.ClaimsVersion)
				}
			}
			for _, want := range fixture.Want {
				if strings.Count(got.Summary, want) != 1 {
					t.Fatalf("want %q exactly once in %q", want, got.Summary)
				}
			}
			for _, absent := range fixture.Absent {
				if strings.Contains(got.Summary, absent) {
					t.Fatalf("unexpected %q in %q", absent, got.Summary)
				}
			}
			for _, want := range fixture.Facts {
				if !slices.Contains(got.KeyFacts, want) {
					t.Fatalf("missing fact %q: %v", want, got.KeyFacts)
				}
			}
			for _, want := range fixture.Tasks {
				if !slices.Contains(got.OpenTasks, want) {
					t.Fatalf("missing task %q: %v", want, got.OpenTasks)
				}
			}
		})
	}
}
