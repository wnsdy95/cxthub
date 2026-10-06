package cli

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type deliveryHistory struct {
	inbound.ContextHistory
	events []domain.HistoryEvent
}

func (h deliveryHistory) ListHistory(context.Context, string) ([]domain.HistoryEvent, error) {
	return h.events, nil
}
func (h deliveryHistory) CurrentPosition(context.Context) (domain.WorkingPosition, error) {
	return domain.WorkingPosition{}, domain.ErrNotFound
}

type historyDeliverySync struct {
	inbound.SyncRepo
	inputs []inbound.SyncInput
	send   func(inbound.SyncInput) error
}

type slowHistoryDelivery struct {
	inbound.SyncRepo
	names []string
}

func (s *slowHistoryDelivery) Push(ctx context.Context, in inbound.SyncInput) (inbound.SyncOutput, error) {
	if in.Publication == nil || !in.Publication.HistoryOnly || len(in.Publication.Branches) != 1 {
		return inbound.SyncOutput{}, errors.New("invalid history-only scope")
	}
	name := in.Publication.Branches[0].Branch
	s.names = append(s.names, name)
	if name == "a" {
		<-ctx.Done()
		return inbound.SyncOutput{}, ctx.Err()
	}
	return inbound.SyncOutput{}, nil
}
func TestBranchHistorySlowIdentityCannotConsumeEveryTurn(t *testing.T) {
	repo := string(domain.HashContent([]byte("repo")))
	history := deliveryHistory{}
	for _, name := range []string{"a", "b"} {
		history.events = append(history.events, domain.HistoryEvent{ID: strings.Repeat(name, 32), RepoID: repo, BranchID: domain.LegacyContextBranchID(repo, name), Branch: name, Kind: "position", Target: domain.HashContent([]byte(name)), CreatedAt: time.Unix(1, 0).UTC()})
	}
	syncer := &slowHistoryDelivery{}
	c := &Container{History: history, Sync: syncer, ResolveRepo: func(context.Context, string) (domain.Repo, error) { return domain.Repo{ID: repo}, nil }}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := runBranchHistorySync(ctx, c, t.TempDir())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected pending deadline: %v", err)
	}
	if len(syncer.names) < 2 || syncer.names[0] != "a" || syncer.names[1] != "b" {
		t.Fatalf("slow identity starved independent B: %v", syncer.names)
	}
	if time.Since(start) > time.Second {
		t.Fatal("drain extended parent budget")
	}
}

func (s *historyDeliverySync) Push(_ context.Context, in inbound.SyncInput) (inbound.SyncOutput, error) {
	s.inputs = append(s.inputs, in)
	return inbound.SyncOutput{}, s.send(in)
}

func TestBranchHistoryDeliveryPreservesScopeAndMakesBoundedProgress(t *testing.T) {
	for _, mode := range []string{"dependency-first", "permanent-conflict", "cancelled", "empty"} {
		t.Run(mode, func(t *testing.T) {
			repo := string(domain.HashContent([]byte("repo")))
			history := deliveryHistory{}
			for _, name := range []string{"a", "b"} {
				history.events = append(history.events, domain.HistoryEvent{ID: strings.Repeat(name, 32), RepoID: repo, BranchID: domain.LegacyContextBranchID(repo, name), Branch: name, Kind: "position", Target: domain.HashContent([]byte(name)), CreatedAt: time.Unix(1, 0).UTC()})
			}
			if mode == "empty" {
				history.events = nil
			}
			acceptedB := false
			syncer := &historyDeliverySync{send: func(in inbound.SyncInput) error {
				if in.Ref != "" || in.Force || in.Append || !in.ForegroundOnly || in.Publication == nil || !in.Publication.HistoryOnly || len(in.Publication.Branches) != 1 {
					t.Fatalf("background drifted into tip publication: %+v", in)
				}
				branch := in.Publication.Branches[0]
				if branch.BranchID != domain.LegacyContextBranchID(repo, branch.Branch) {
					t.Fatal("identity drift")
				}
				if branch.Branch == "b" {
					acceptedB = true
					return nil
				}
				if mode == "permanent-conflict" || !acceptedB {
					return domain.ErrSyncConflict
				}
				return nil
			}}
			c := &Container{Sync: syncer, History: history, ResolveRepo: func(context.Context, string) (domain.Repo, error) { return domain.Repo{ID: repo}, nil }}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			err := runBranchHistorySync(ctx, c, t.TempDir())
			if mode == "permanent-conflict" && !errors.Is(err, domain.ErrSyncConflict) {
				t.Fatalf("missing pending failure: %v", err)
			}
			if mode == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("missing cancellation: %v", err)
			}
			if (mode == "empty" || mode == "dependency-first") && err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, in := range syncer.inputs {
				names = append(names, in.Publication.Branches[0].Branch)
			}
			var want []string
			if mode != "empty" && mode != "cancelled" {
				want = []string{"a", "b", "a"}
			}
			if !reflect.DeepEqual(names, want) {
				t.Fatalf("unbounded/missing delivery: %v want %v", names, want)
			}
		})
	}
}

func TestPostCommitWakesHistoryOnlyAfterDurableCompletion(t *testing.T) {
	for _, mode := range []string{"completed", "capture-failed", "history-failed"} {
		t.Run(mode, func(t *testing.T) {
			fail := mode == "capture-failed"
			cwd, c, _, repo, _ := publicationFixture(t)
			workingHistory := c.History
			if mode == "history-failed" {
				c.History = unavailableRewriteHistory{c.History}
			}
			c.Save = publicationSaveFunc(func(context.Context, inbound.SaveInput) (inbound.SaveOutput, error) {
				if fail {
					return inbound.SaveOutput{}, errors.New("capture unavailable")
				}
				return inbound.SaveOutput{}, domain.ErrNoActiveSession
			})
			wakes := 0
			publish := func(string) {
				wakes++
				passes := capturePasses(t, cwd)
				if len(passes) != 1 || !passes[0].Complete {
					t.Fatal("publication woke before capture completion")
				}
				events, err := c.History.ListHistory(context.Background(), repo)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, e := range events {
					found = found || e.Kind == "publish"
				}
				if found != (mode != "history-failed") {
					t.Fatal("unexpected publication history state at wake")
				}
			}
			if err := runGitHookWithPublication(context.Background(), c, cwd, []string{"post-commit"}, publish); err != nil {
				t.Fatal(err)
			}
			if mode == "history-failed" {
				c.History = workingHistory
				if err := replayPublicationsForBranches(context.Background(), c, cwd, map[string]bool{domain.LegacyContextBranchID(repo, "main"): true}); err != nil {
					t.Fatal(err)
				}
				events, err := c.History.ListHistory(context.Background(), repo)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, e := range events {
					found = found || e.Kind == "publish"
				}
				if !found {
					t.Fatal("completed pass did not recover")
				}
			}
			want := 1
			if fail {
				want = 0
			}
			if wakes != want {
				t.Fatalf("wakes=%d want=%d", wakes, want)
			}
		})
	}
}
