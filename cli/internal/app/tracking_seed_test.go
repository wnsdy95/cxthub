package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTrackingFrozenPositionKeepsSeedPinAcrossLocalGraft(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty=%v", empty), func(t *testing.T) {
			ctx := context.Background()
			f := newStagingFixture(t)
			a := putBranchSeedSnapshot(t, ctx, f.store, f.git.repo.ID, "main", []domain.Event{agentMessage("user", "A conversation", 0)}, nil, &domain.MemoryDigest{Summary: "FUTURE mutable memory"})
			b := putBranchSeedSnapshot(t, ctx, f.store, f.git.repo.ID, "main", []domain.Event{agentMessage("user", "B conversation", 0)}, nil, nil)
			memory := domain.ContentHash("")
			if !empty {
				var err error
				memory, err = f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: a, Summary: "pinned M1"})
				if err != nil {
					t.Fatal(err)
				}
			}
			birth := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: f.git.repo.ID, BranchID: "R", Branch: "main", Kind: "birth", Source: a, Target: a, GitAfter: f.git.sha, MemoryHash: memory, MemoryPinned: true, CreatedAt: time.Unix(1, 0)}
			wt := sha256.Sum256([]byte(filepath.Join(f.root, ".git")))
			e := birth
			e.ID, e.Kind, e.WorktreeID = strings.Repeat("2", 32), "attach", fmt.Sprintf("%x", wt[:16])
			sa, _ := f.store.GetSnapshot(ctx, a)
			sb, _ := f.store.GetSnapshot(ctx, b)
			observed := inbound.RemoteBranchObservation{Ref: domain.Ref{RepoID: f.git.repo.ID, Kind: domain.RefBranch, Name: "main", BranchID: "R", Target: b}, History: []domain.HistoryEvent{birth}, Snapshots: []domain.Snapshot{sa, sb}}
			service := NewContextHistoryService(f.store, f.store)
			frozen, err := service.PrepareTrackingAttachment(ctx, e, observed, []string{f.git.sha})
			if err != nil {
				t.Fatal(err)
			}
			// This noncyclic, local-only edge must not reclassify the frozen observation.
			sa.GraftParents = []domain.ContentHash{b}
			sa.GraftSeq = 1
			if err := f.store.PutSnapshot(ctx, sa); err != nil {
				t.Fatal(err)
			}
			if err := service.ApplyTrackingAttachment(ctx, inbound.TrackingAttachmentInput{Attachment: frozen, SelectPosition: true}); err != nil {
				t.Fatal(err)
			}
			p, err := f.store.GetWorkingPosition(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !p.Rewound || p.MemoryHash != memory {
				t.Fatalf("local graft changed frozen historical selection: rewound=%v pinned=%v", p.Rewound, p.MemoryHash == memory)
			}
			distiller := &seedPinDistiller{}
			seed := NewBranchSeedService(f.git, f.store, distiller, nil, nil, nil).WithAgentContext(&agentPackageFixture{})
			doc, err := f.store.GetDoc(ctx, a)
			if err != nil {
				t.Fatal(err)
			}
			out, err := seed.seedAgentContext(ctx, inbound.SeedInput{Cwd: f.root, FromBranch: "main", NewBranch: "feature/pinned", Provider: domain.ProviderCodex, SkipMaterialize: true}, f.git.repo, sa, doc, domain.ProviderCodex, f.root)
			if err != nil {
				t.Fatal(err)
			}
			snap, err := f.store.GetSnapshot(ctx, out.SnapshotID)
			if err != nil {
				t.Fatal(err)
			}
			got, err := f.store.GetMemory(ctx, snap.MemoryHash)
			if err != nil {
				t.Fatal(err)
			}
			want := "pinned M1"
			if empty {
				want = ""
			}
			if got.Summary != want || distiller.calls != 0 {
				t.Fatalf("archive memory lost: got=%q want=%q distills=%d", got.Summary, want, distiller.calls)
			}
		})
	}
}
