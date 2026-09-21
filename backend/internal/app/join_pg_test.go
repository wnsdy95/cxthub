//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestPGJoinConfirmationUsesTransaction(t *testing.T) {
	svc, st, _ := collaborationPG(t)
	ctx := context.Background()
	owner := domain.User{ID: domain.NewID("user_"), Username: fmt.Sprintf("join%d", time.Now().UnixNano()), Email: "join@example.test"}
	if err := st.UpsertUser(ctx, owner); err != nil {
		t.Fatal(err)
	}
	repositoryRecord := domain.Repository{ID: domain.NewID("ws_"), OwnerID: owner.ID, OwnerUsername: owner.Username, Name: "Join", Slug: "join"}
	if err := st.CreateRepository(ctx, repositoryRecord); err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(repositoryRecord.ID))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: repositoryRecord.ID}); err != nil {
		t.Fatal(err)
	}
	p := collaborationSnapshot(t, st, repo, "P")
	h := collaborationSnapshot(t, st, repo, "H", p)
	x := collaborationSnapshot(t, st, repo, "X", p)
	tip := collaborationSnapshot(t, st, repo, "T", x)
	previous := domain.ContentHash("")
	for _, target := range []domain.ContentHash{p, x, tip, h} {
		if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: target}, previous); err != nil {
			t.Fatal(err)
		}
		previous = target
	}
	if err := st.AddGraftParents(ctx, repo, h, []domain.ContentHash{tip}); err != nil {
		t.Fatal(err)
	}
	in := inbound.JoinPreviewInput{ActorID: owner.ID, RepoID: repo, Snapshot: x}
	preview, err := svc.PreviewJoin(ctx, in)
	if err != nil || preview.Reason != "" {
		t.Fatalf("preview %+v %v", preview, err)
	}
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	other := NewService(peer, peer, nil, nil, peer)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, writer := range []*Service{svc, other} {
		go func() { <-start; _, err := writer.ConfirmJoin(ctx, confirmPreview(in, preview, true)); results <- err }()
	}
	close(start)
	wins, stale := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			wins++
		} else if errors.Is(err, domain.ErrJoinPreviewChanged) {
			stale++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || stale != 1 {
		t.Fatalf("wins %d stale %d", wins, stale)
	}
	ref, _ := st.GetRef(ctx, repo, domain.RefBranch, "main")
	snap, _ := st.GetSnapshot(ctx, repo, x)
	if ref.Target != tip || snap.GraftSeq != 1 {
		t.Fatalf("non-atomic confirmation %+v %+v", ref, snap)
	}
}

func TestPGJoinPolicyAndConcurrentWriters(t *testing.T) {
	for _, scenario := range []string{"whole", "partial", "foreign attachment", "new child", "two writers"} {
		t.Run(scenario, func(t *testing.T) {
			svc, st, repo := collaborationPG(t)
			ctx := context.Background()
			p := collaborationSnapshot(t, st, repo, "P")
			h := collaborationSnapshot(t, st, repo, "H", p)
			x := collaborationSnapshot(t, st, repo, "X", p)
			tip := collaborationSnapshot(t, st, repo, "T", x)
			previous := domain.ContentHash("")
			for _, id := range []domain.ContentHash{p, x, tip, h} {
				if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: id}, previous); err != nil {
					t.Fatal(err)
				}
				previous = id
			}
			if err := st.AddGraftParents(ctx, repo, h, []domain.ContentHash{tip}); err != nil {
				t.Fatal(err)
			}
			graph, err := svc.loadJoinGraph(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := domain.PlanJoin(graph, domain.JoinRequest{RepoID: repo, Branch: "main", Source: x, IncludeDescendants: true})
			if err != nil {
				t.Fatal(err)
			}
			mutation, err := plan.Mutation("")
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "whole", "partial":
				out, err := svc.joinForTest(ctx, inbound.JoinInput{RepoID: repo, TargetBranch: "main", Snapshot: x, IncludeDescendants: scenario == "whole"})
				if err != nil {
					t.Fatal(err)
				}
				want := tip
				if scenario == "partial" {
					want = x
					ref, err := st.GetRef(ctx, repo, domain.RefSession, out.ForkBranch)
					if err != nil || ref.Target != tip {
						t.Fatalf("lost residual: %+v %v", ref, err)
					}
				}
				if out.Head != want {
					t.Fatalf("head %s want %s", out.Head, want)
				}
				snap, err := st.GetSnapshot(ctx, repo, x)
				if err != nil || len(snap.Parents) != 1 || snap.Parents[0] != p || len(snap.GraftParents) != 1 || snap.GraftParents[0] != h {
					t.Fatalf("lineage %+v %v", snap, err)
				}
			case "foreign attachment", "new child":
				name := domain.SessionRefPrefix("other") + "attached"
				target := tip
				if scenario == "new child" {
					name = domain.SessionRefPrefix("main") + "new"
					target = collaborationSnapshot(t, st, repo, "new", tip)
				}
				if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefSession, Name: name, Target: target}, ""); err != nil {
					t.Fatal(err)
				}
				if err := st.ApplyJoin(ctx, mutation); !errors.Is(err, domain.ErrConflict) {
					t.Fatalf("stale plan accepted: %v", err)
				}
				ref, _ := st.GetRef(ctx, repo, domain.RefBranch, "main")
				snap, _ := st.GetSnapshot(ctx, repo, x)
				if ref.Target != h || snap.GraftSeq != 0 {
					t.Fatalf("rejected join changed state: %+v %+v", ref, snap)
				}
			case "two writers":
				peer, err := store.NewPostgresStore(ctx, collaborationDSN(t))
				if err != nil {
					t.Fatal(err)
				}
				defer peer.Close()
				start := make(chan struct{})
				results := make(chan error, 2)
				var wg sync.WaitGroup
				for _, writer := range []*store.PostgresStore{st, peer} {
					wg.Add(1)
					go func() { defer wg.Done(); <-start; results <- writer.ApplyJoin(ctx, mutation) }()
				}
				close(start)
				wg.Wait()
				close(results)
				wins, conflicts := 0, 0
				for err := range results {
					if err == nil {
						wins++
					} else if errors.Is(err, domain.ErrRefConflict) {
						conflicts++
					} else {
						t.Fatal(err)
					}
				}
				if wins != 1 || conflicts != 1 {
					t.Fatalf("wins %d conflicts %d", wins, conflicts)
				}
				ref, _ := st.GetRef(ctx, repo, domain.RefBranch, "main")
				snap, _ := st.GetSnapshot(ctx, repo, x)
				if ref.Target != tip || snap.GraftSeq != 1 {
					t.Fatalf("non-atomic result: %+v %+v", ref, snap)
				}
			}
		})
	}
}
