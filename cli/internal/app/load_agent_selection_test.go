package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func TestLoadAgentContextPreservesSourceBranchAndDetachedHash(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repo := string(agentHash("load selection repo"))
	a := putBranchSeedSnapshot(t, ctx, store, repo, "A", []domain.Event{agentMessage("user", "A", 0)}, nil, nil)
	b := putBranchSeedSnapshot(t, ctx, store, repo, "B", []domain.Event{agentMessage("user", "B", 0)}, nil, nil)
	putBranchSeedRef(t, ctx, store, repo, "A", a)
	putBranchSeedRef(t, ctx, store, repo, "B", b)
	if err := store.PutRef(ctx, domain.Ref{RepoID: repo, Kind: domain.RefHEAD, Name: "HEAD", Symbolic: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRef(ctx, domain.Ref{RepoID: repo, Kind: domain.RefTag, Name: "release", Target: b}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, ref, branch, wantBranch string
		want                          domain.ContentHash
	}{
		{"branch B while HEAD A", "B", "", "B", b},
		{"hash B while HEAD A", string(b), "", "", b},
		{"hash at HEAD still detached", string(a), "", "", a},
		{"tag remains detached", "release", "", "", b},
		{"HEAD keeps branch", "HEAD", "", "A", a},
		{"implicit HEAD keeps branch", "", "", "A", a},
		{"checkout resolved source", string(b), "B", "B", b},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepared := &agentPackageFixture{}
			mat := &agentMaterializerFixture{}
			load := NewLoadSessionService(store, map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderCodex: codec.NewCodexCodec()}, map[domain.ProviderKind]outbound.SessionMaterializer{domain.ProviderCodex: mat}, nil, nil, nil).WithAgentContext(prepared).WithAgentCodePosition(agentCodeFixture{})
			pin := &domain.AgentMemoryPin{}
			_, err := load.Load(ctx, inbound.LoadInput{RepoID: repo, Cwd: t.TempDir(), Ref: tc.ref, Branch: tc.branch, MemoryPin: pin, TargetProvider: domain.ProviderCodex})
			if err != nil {
				t.Fatal(err)
			}
			if prepared.seen.Branch != tc.wantBranch || prepared.seen.SnapshotID != tc.want || !reflect.DeepEqual(prepared.seen.MemoryPin, pin) || mat.calls != 1 {
				t.Fatalf("wrong preparation identity: %+v", prepared.seen)
			}
		})
	}
}

func TestHistoryPinnedHashNeverInheritsHeadIntegration(t *testing.T) {
	s, local := makeHistoryQuery()
	remote := &historyRemote{err: errors.New("request captured")}
	s.remote = remote
	for _, server := range []bool{false, true} {
		out, err := s.QueryHistory(context.Background(), inbound.HistoryQueryInput{Server: server, Position: local.snaps[1].ID})
		if server {
			if !errors.Is(err, remote.err) || remote.input.Branch != "" || remote.input.Position != string(local.snaps[1].ID) {
				t.Fatalf("hash inherited HEAD branch: %+v %v", remote.input, err)
			}
		} else if err != nil || out.Selection.Branch != "" || out.Position != local.snaps[1].ID {
			t.Fatal(out, err)
		}
	}
}

func TestHistoryPinnedHashRejectsServerBranchSubstitution(t *testing.T) {
	s, local := makeHistoryQuery()
	position := local.snaps[1].ID
	s.remote = &historyRemote{view: domain.ContextQueryView{Version: 1, Branch: "main", Position: position, StateHash: agentHash("state"), Snapshots: []domain.Snapshot{local.snaps[1]}}}
	if _, err := s.QueryHistory(context.Background(), inbound.HistoryQueryInput{Server: true, Position: position}); !errors.Is(err, domain.ErrSelectionChanged) {
		t.Fatal("server substituted HEAD scope for detached hash", err)
	}
}
