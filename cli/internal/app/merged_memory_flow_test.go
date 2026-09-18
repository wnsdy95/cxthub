package app

import (
	"context"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type mergedMemorySink struct {
	provider domain.ProviderKind
	digest   domain.MemoryDigest
}

func (s *mergedMemorySink) Provider() domain.ProviderKind { return s.provider }
func (s *mergedMemorySink) Inject(_ context.Context, d domain.MemoryDigest, _ string) (string, error) {
	s.digest = d
	return "memory", nil
}

type mergedPositionStore struct {
	*storage.FileStore
	position domain.WorkingPosition
}

func (s *mergedPositionStore) GetWorkingPosition(context.Context) (domain.WorkingPosition, error) {
	return s.position, nil
}

func TestMergedMemoryFlowsToBothProvidersWithoutRewritingHistory(t *testing.T) {
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		t.Run(string(provider), func(t *testing.T) {
			ctx := context.Background()
			st := storage.NewFileStore(t.TempDir())
			repo := domain.Repo{ID: string(domain.HashContent([]byte("flow repo"))), LocalPath: t.TempDir(), DefaultBranch: "main"}
			base := putBranchSeedSnapshot(t, ctx, st, repo.ID, "main", []domain.Event{seedMessage("user", "base raw transcript", 0)}, nil, &domain.MemoryDigest{Summary: "BASE MERGED DECISION"})
			feature := putBranchSeedSnapshot(t, ctx, st, repo.ID, "feature", []domain.Event{seedMessage("user", "feature raw transcript", 0)}, nil, &domain.MemoryDigest{Summary: "FEATURE DECISION"})
			snap, err := st.GetSnapshot(ctx, feature)
			if err != nil {
				t.Fatal(err)
			}
			archivedHash := snap.MemoryHash
			snap.GraftSeq = 1
			snap.Grafted = true
			snap.GraftParents = []domain.ContentHash{base}
			if err := st.PutSnapshot(ctx, snap); err != nil {
				t.Fatal(err)
			}
			putBranchSeedRef(t, ctx, st, repo.ID, "main", feature)
			sink := &mergedMemorySink{provider: provider}
			selected := &mergedPositionStore{FileStore: st, position: domain.WorkingPosition{Snapshot: feature, MemoryHash: archivedHash, MemoryPinned: true, Rewound: false}}
			load := NewLoadSessionService(selected, nil, nil, nil, stubDistiller{d: domain.MemoryDigest{Summary: "FEATURE DECISION"}}, map[domain.ProviderKind]outbound.MemorySink{provider: sink})
			doc, err := st.GetDoc(ctx, feature)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := load.loadMemory(ctx, doc.CIR, snap, provider, repo.LocalPath, nil); err != nil {
				t.Fatal(err)
			}
			assertMerged := func(text string) {
				t.Helper()
				for _, want := range []string{"BASE MERGED DECISION", "FEATURE DECISION"} {
					if !strings.Contains(text, want) {
						t.Fatalf("missing %s in %q", want, text)
					}
				}
			}
			assertMerged(sink.digest.Summary)
			offline, found, readErr := ReadProjectedMemory(ctx, selected, feature)
			if readErr != nil || !found {
				t.Fatalf("offline projection: %v", readErr)
			}
			assertMerged(offline.Summary)
			replay, ok := load.portableReplaySeed(ctx, doc.CIR, snap, provider, repo.LocalPath, 96<<10, nil)
			if !ok {
				t.Fatal("portable replay lost memory")
			}
			assertMerged(replay.Blocks[0].Text)
			if len(replay.Blocks[0].Text) > 96<<10 {
				t.Fatal("unbounded portable replay")
			}
			seed := NewBranchSeedService(branchSeedGit{repo: repo}, st, stubDistiller{d: domain.MemoryDigest{Summary: "FEATURE DECISION"}}, nil, nil, nil)
			born, err := seed.Seed(ctx, inbound.SeedInput{Cwd: repo.LocalPath, FromBranch: "main", NewBranch: "feature/next", Provider: provider})
			if err != nil {
				t.Fatal(err)
			}
			seedDoc, err := st.GetDoc(ctx, born.SnapshotID)
			if err != nil {
				t.Fatal(err)
			}
			assertMerged(seedDoc.CIR.Events[0].Blocks[0].Text)
			if eventsJSONBytes(seedDoc.CIR.Events) > seedBudgetBytes {
				t.Fatal("unbounded branch seed")
			}
			// Rewinding uses the exact memory observed at that old code position.
			selected.position.Rewound = true
			offline, found, readErr = ReadProjectedMemory(ctx, selected, feature)
			if readErr != nil || !found || offline.Summary != "FEATURE DECISION" {
				t.Fatalf("offline rewind leaked later merge: %q %v", offline.Summary, readErr)
			}
			if _, err := load.loadMemory(ctx, doc.CIR, snap, provider, repo.LocalPath, nil); err != nil {
				t.Fatal(err)
			}
			if sink.digest.Summary != "FEATURE DECISION" {
				t.Fatalf("later merge leaked into historical selection: %q", sink.digest.Summary)
			}
			replay, ok = load.portableReplaySeed(ctx, doc.CIR, snap, provider, repo.LocalPath, 96<<10, nil)
			if !ok || strings.Contains(replay.Blocks[0].Text, "BASE MERGED DECISION") {
				t.Fatal("rewound replay imported later merge")
			}
			after, err := st.GetSnapshot(ctx, feature)
			if err != nil {
				t.Fatal(err)
			}
			if after.MemoryHash != archivedHash {
				t.Fatal("loading rewrote archived source memory")
			}
		})
	}
}
