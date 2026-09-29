package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func TestPersonalWorkLoadPassesExplicitSelectionWithoutFallback(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	id := putBranchSeedSnapshot(t, ctx, store, "repo", "main", []domain.Event{agentMessage("user", "raw archive", 0)}, nil, &domain.MemoryDigest{Summary: "memory"})
	p := &agentPackageFixture{}
	mat := &agentMaterializerFixture{}
	loader := NewLoadSessionService(store, map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderCodex: codec.NewCodexCodec()}, map[domain.ProviderKind]outbound.SessionMaterializer{domain.ProviderCodex: mat}, nil, nil, nil).WithAgentContext(p).WithAgentCodePosition(agentCodeFixture{})
	in := inbound.LoadInput{Ref: string(id), Cwd: t.TempDir(), WorkStatePath: "handoff.json", PersonalScope: domain.PersonalWorkScope{ActorID: "alice", SessionID: "selected-session", WorktreeID: "tree"}, TargetProvider: domain.ProviderCodex}
	if _, err := loader.Load(ctx, in); err != nil {
		t.Fatal(err)
	}
	if p.seen.WorkStatePath != in.WorkStatePath || p.seen.PersonalScope != in.PersonalScope || mat.calls != 1 {
		t.Fatalf("selection lost: %+v", p.seen)
	}
	p.err = errors.New("source validation failed")
	if _, err := loader.Load(ctx, in); err == nil || mat.calls != 1 {
		t.Fatal("personal failure fell back to replay")
	}
	for _, mode := range []string{"full", "reconstructed", "memory"} {
		in.Mode = mode
		if _, err := loader.Load(ctx, in); err == nil {
			t.Fatal("accepted personal handoff with legacy mode")
		}
	}
	in.Mode = ""
	in.PreferPendingTail = true
	if _, err := loader.Load(ctx, in); err == nil {
		t.Fatal("selected implicit pending session")
	}
	in.PreferPendingTail = false
	loader.agentContext = nil
	if _, err := loader.Load(ctx, in); err == nil {
		t.Fatal("missing preparer fell back to replay")
	}
}
