package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type seedPinPreparer func(context.Context, inbound.PrepareAgentContextInput) (domain.AgentContextPackage, error)

func (f seedPinPreparer) PrepareAgentContext(ctx context.Context, in inbound.PrepareAgentContextInput) (domain.AgentContextPackage, error) {
	return f(ctx, in)
}

func (seedPinPreparer) ValidateAgentContextDelivery(context.Context, string, domain.AgentContextSelection) error {
	return nil
}

type seedPinDistiller struct{ calls int }

func (d *seedPinDistiller) Distill(context.Context, domain.CIRDocument, *domain.NativeMemory) (domain.MemoryDigest, error) {
	d.calls++
	return domain.MemoryDigest{Summary: "FUTURE redistilled source conversation"}, nil
}

func TestAgentContextSeedPersistsArchivePinIndependentlyOfInjectedMain(t *testing.T) {
	for _, kind := range []string{"selected owner", "ancestor owner", "empty"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f := newStagingFixture(t)
			ancestor := putBranchSeedSnapshot(t, ctx, f.store, f.git.repo.ID, "main", []domain.Event{agentMessage("user", "ancestor", 0)}, nil, nil)
			source := putBranchSeedSnapshot(t, ctx, f.store, f.git.repo.ID, "main", []domain.Event{agentMessage("user", "source conversation", 0)}, []domain.ContentHash{ancestor}, &domain.MemoryDigest{Summary: "FUTURE mutable source attachment"})
			putBranchSeedRef(t, ctx, f.store, f.git.repo.ID, "main", source)
			owner := source
			if kind == "ancestor owner" {
				owner = ancestor
			}
			old := domain.MergeDigests(domain.MemoryDigest{}, domain.MemoryDigest{SnapshotID: owner, Summary: "historical memory", KeyFacts: []string{"old constraint"}, OpenTasks: []string{"old task"}})
			old.PreviousMemoryHash = agentHash("earlier memory")
			hash, err := f.store.PutMemory(ctx, old)
			if err != nil {
				t.Fatal(err)
			}
			pin := domain.AgentMemoryPin{SnapshotID: owner, MemoryHash: hash}
			if kind == "empty" {
				pin, old = domain.AgentMemoryPin{}, domain.MemoryDigest{}
			}
			{
				if err := f.store.PutWorkingPosition(ctx, domain.WorkingPosition{RepoID: f.git.repo.ID, Branch: "main", GitCommit: f.git.sha, Snapshot: source, MemoryHash: pin.MemoryHash, MemorySource: pin.SnapshotID, MemoryPinned: true, Rewound: true}); err != nil {
					t.Fatal(err)
				}
			}
			prepare := seedPinPreparer(func(ctx context.Context, in inbound.PrepareAgentContextInput) (domain.AgentContextPackage, error) {
				if in.SnapshotID != source || in.Branch != "main" || !in.LatestMain {
					t.Fatal("wrong seed source", in)
				}
				p, err := (&agentPackageFixture{}).PrepareAgentContext(ctx, in)
				p.ID, _ = p.Digest()
				return p, err
			})
			distiller := &seedPinDistiller{}
			seed := NewBranchSeedService(f.git, f.store, distiller, nil, nil, nil).WithAgentContext(prepare)
			out, err := seed.Seed(ctx, inbound.SeedInput{Cwd: f.root, FromBranch: "main", NewBranch: "feature/pinned", Provider: domain.ProviderCodex, SkipMaterialize: true})
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
			old.SnapshotID, old.PreviousMemoryHash = out.SnapshotID, ""
			if !reflect.DeepEqual(got, old) || distiller.calls != 0 {
				t.Fatalf("stored seed differs from prepared pin: got=%+v want=%+v distills=%d", got, old, distiller.calls)
			}
			if !reflect.DeepEqual(snap.Parents, []domain.ContentHash{source}) {
				t.Fatal("changed archival source", snap.Parents)
			}
			original, err := f.store.GetMemory(ctx, hash)
			if err != nil || original.SnapshotID != owner || original.PreviousMemoryHash != agentHash("earlier memory") {
				t.Fatal("seed rewrote the historical memory object", original, err)
			}
			current, err := f.store.GetSnapshot(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			latest, err := f.store.GetMemory(ctx, current.MemoryHash)
			if err != nil || latest.Summary != "FUTURE mutable source attachment" {
				t.Fatal("seed replaced the source attachment", latest, err)
			}
		})
	}
}

func TestAgentContextSeedRejectsUnavailableOrMismatchedPreparedPin(t *testing.T) {
	for _, failure := range []string{"missing", "wrong owner", "wrong repository", "malformed", "different source", "different branch"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			f := newStagingFixture(t)
			source := putBranchSeedSnapshot(t, ctx, f.store, f.git.repo.ID, "main", []domain.Event{agentMessage("user", "source", 0)}, nil, &domain.MemoryDigest{Summary: "FUTURE attachment"})
			putBranchSeedRef(t, ctx, f.store, f.git.repo.ID, "main", source)
			foreign := putBranchSeedSnapshot(t, ctx, f.store, "another repo", "main", []domain.Event{agentMessage("user", "foreign", 0)}, nil, nil)
			owner := source
			if failure == "wrong owner" || failure == "wrong repository" {
				owner = foreign
			}
			hash, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: owner, Summary: "historical"})
			if err != nil {
				t.Fatal(err)
			}
			pin := domain.AgentMemoryPin{SnapshotID: source, MemoryHash: hash}
			switch failure {
			case "missing":
				pin.MemoryHash = agentHash("unavailable pin")
			case "wrong repository":
				pin.SnapshotID = foreign
			case "malformed":
				pin.MemoryHash = ""
			}
			prepare := seedPinPreparer(func(ctx context.Context, in inbound.PrepareAgentContextInput) (domain.AgentContextPackage, error) {
				p, err := (&agentPackageFixture{}).PrepareAgentContext(ctx, in)
				p.Content.Selection.MemoryPin = &pin
				if failure == "different source" {
					p.Content.Selection.SnapshotID = foreign
				}
				if failure == "different branch" {
					p.Content.Selection.Branch = "other"
				}
				p.ID, _ = p.Digest()
				return p, err
			})
			mat, distiller := &agentMaterializerFixture{}, &seedPinDistiller{}
			seed := NewBranchSeedService(f.git, f.store, distiller, map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderCodex: codec.NewCodexCodec()}, map[domain.ProviderKind]outbound.SessionMaterializer{domain.ProviderCodex: mat}, nil).WithAgentContext(prepare)
			before, err := f.store.ReadCheckoutState(ctx, f.git.repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, err = seed.Seed(ctx, inbound.SeedInput{Cwd: f.root, FromBranch: "main", NewBranch: "feature/rejected", Provider: domain.ProviderCodex})
			if err == nil || mat.calls != 0 || distiller.calls != 0 {
				t.Fatalf("invalid pin had side effects or fell back: err=%v materializations=%d distills=%d", err, mat.calls, distiller.calls)
			}
			if _, err := f.store.GetRef(ctx, f.git.repo.ID, domain.RefBranch, "feature/rejected"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("invalid pin published branch", err)
			}
			after, err := f.store.ReadCheckoutState(ctx, f.git.repo.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("invalid pin changed selection", err)
			}
		})
	}
}
