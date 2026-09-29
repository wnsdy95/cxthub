package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/memory"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func TestStashPopMaterializerFailureDoesNotWriteMemory(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repo := domain.Repo{ID: "repo", LocalPath: t.TempDir()}
	id := putBranchSeedSnapshot(t, ctx, store, repo.ID, domain.StashBranchLabel, []domain.Event{agentMessage("user", "recover this conversation", 0)}, nil, nil)
	entry := domain.StashEntry{Snapshot: id, Provider: domain.ProviderCodex}
	if err := store.StashPush(ctx, repo.ID, entry); err != nil {
		t.Fatal(err)
	}
	sink := &recordingDigestSink{provider: domain.ProviderCodex}
	load := NewLoadSessionService(store, map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderCodex: codec.NewCodexCodec()}, map[domain.ProviderKind]outbound.SessionMaterializer{domain.ProviderCodex: &agentMaterializerFixture{err: errors.New("cannot write native session")}}, nil, memory.NewRuleDistiller(), map[domain.ProviderKind]outbound.MemorySink{domain.ProviderCodex: sink})
	svc := newTestStashService(branchSeedGit{repo: repo}, nil, nil, store, load)
	if _, err := svc.StashPop(ctx, repo.LocalPath); !errors.Is(err, domain.ErrAgentContextUnavailable) {
		t.Fatalf("fallback accepted: %v", err)
	}
	if sink.digest.SnapshotID != "" || sink.digest.Summary != "" {
		t.Fatal("failed restore wrote memory")
	}
	stack, err := store.StashList(ctx, repo.ID)
	if err != nil || len(stack) != 1 || stack[0] != entry {
		t.Fatalf("stash changed: %+v %v", stack, err)
	}
}

func TestStashPopRestoresLocalConversationWithoutCloudPackage(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repo := domain.Repo{ID: "repo", LocalPath: t.TempDir()}
	id := putBranchSeedSnapshot(t, ctx, store, repo.ID, domain.StashBranchLabel, []domain.Event{agentMessage("user", "UNPUBLISHED LOCAL WORK", 0)}, nil, nil)
	before, err := store.GetDoc(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	entry := domain.StashEntry{Snapshot: id, Provider: domain.ProviderCodex, Branch: "feature"}
	if err := store.StashPush(ctx, repo.ID, entry); err != nil {
		t.Fatal(err)
	}
	cloud := &agentPackageFixture{err: errors.New("not on server")}
	mat := &agentMaterializerFixture{}
	load := NewLoadSessionService(store, map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderCodex: codec.NewCodexCodec()}, map[domain.ProviderKind]outbound.SessionMaterializer{domain.ProviderCodex: mat}, nil, nil, nil).WithAgentContext(cloud)
	svc := newTestStashService(branchSeedGit{repo: repo}, nil, nil, store, load)
	out, err := svc.StashPop(ctx, repo.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	if out.Depth != 0 || out.ResumeCmd == "" || cloud.calls != 0 || !strings.Contains(string(mat.raw), "UNPUBLISHED LOCAL WORK") {
		t.Fatalf("original session not restored: %+v cloud=%d raw=%s", out, cloud.calls, mat.raw)
	}
	after, err := store.GetDoc(ctx, id)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("original archive changed: %v", err)
	}
	stack, err := store.StashList(ctx, repo.ID)
	if err != nil || len(stack) != 0 {
		t.Fatalf("stash not acknowledged: %+v %v", stack, err)
	}
}

func TestStashPopRetainsStackOnFailureDowngradeOrConcurrentPush(t *testing.T) {
	for _, scenario := range []string{"failure", "downgrade", "concurrent-push"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			store := storage.NewFileStore(t.TempDir())
			repo := domain.Repo{ID: "repo", LocalPath: t.TempDir()}
			entry := domain.StashEntry{Snapshot: domain.HashContent([]byte("original")), Provider: domain.ProviderCodex}
			newer := domain.StashEntry{Snapshot: domain.HashContent([]byte("newer")), Provider: domain.ProviderClaude}
			if err := store.StashPush(ctx, repo.ID, entry); err != nil {
				t.Fatal(err)
			}
			load := checkoutPrepareFunc(func(_ context.Context, in inbound.LoadInput) (inbound.LoadOutput, error) {
				if in.Mode != domain.FidelityFull || in.TargetProvider != entry.Provider || in.Ref != string(entry.Snapshot) {
					t.Fatalf("wrong restore selection: %+v", in)
				}
				switch scenario {
				case "failure":
					return inbound.LoadOutput{}, errors.New("provider failed")
				case "downgrade":
					return inbound.LoadOutput{Fidelity: domain.FidelityMemory}, nil
				default:
					if err := store.StashPush(ctx, repo.ID, newer); err != nil {
						t.Fatal(err)
					}
					return inbound.LoadOutput{ResumeCmd: "codex resume fixture", Fidelity: domain.FidelityFull}, nil
				}
			})
			svc := newTestStashService(branchSeedGit{repo: repo}, nil, nil, store, load)
			_, err := svc.StashPop(ctx, repo.LocalPath)
			if err == nil {
				t.Fatal("failed or stale restoration was acknowledged")
			}
			want := []domain.StashEntry{entry}
			if scenario == "concurrent-push" {
				if !errors.Is(err, domain.ErrSyncConflict) {
					t.Fatal(err)
				}
				want = append([]domain.StashEntry{newer}, want...)
			}
			got, err := store.StashList(ctx, repo.ID)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("stack changed: got=%+v want=%+v err=%v", got, want, err)
			}
		})
	}
}
