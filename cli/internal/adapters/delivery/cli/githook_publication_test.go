package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/boundary"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/branchjournal"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type publicationCheckout func(context.Context, inbound.CheckoutInput) (inbound.CheckoutOutput, error)

func (f publicationCheckout) Checkout(ctx context.Context, in inbound.CheckoutInput) (inbound.CheckoutOutput, error) {
	return f(ctx, in)
}

type publicationList func(context.Context, inbound.ListInput) (inbound.ListOutput, error)

func (f publicationList) List(ctx context.Context, in inbound.ListInput) (inbound.ListOutput, error) {
	return f(ctx, in)
}

// The remote is only an identity: all publication is intercepted, and this
// fixture starts neither a detached helper nor a provider process.
func checkoutPublicationFixture(t *testing.T) (string, *Container, *branchjournal.Journal, string) {
	t.Helper()
	home := t.TempDir()
	for name, value := range map[string]string{
		"HOME": home, "CODEX_HOME": filepath.Join(home, ".codex"), "CLAUDE_CONFIG_DIR": filepath.Join(home, ".claude"),
		"CXT_REMOTE": "", "CXT_CARRY": "", "CXT_KEEP_SESSION": "", "CXT_WRAPPED": "1", "CXT_WRAPPED_AGENT": "codex",
		"CXT_WRAPPER_PID": strconv.Itoa(os.Getppid()), "CXT_WRAPPER_TRANSITION_PROTOCOL": "prepare-first-v1",
	} {
		t.Setenv(name, value)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex", "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	cwd, c, _, _, _ := historyFixtureWithRemote(t, "http://127.0.0.1:1/fixture/publication")
	c.Sync = fixedBriefingSync{}
	oid := gitOut(cwd, "rev-parse", "HEAD")
	if err := runBirthVote(t, cwd, c, "prepared", strings.Repeat("0", 40)+" "+oid+" refs/heads/feature"); err != nil {
		t.Fatal(err)
	}
	j, err := branchjournal.Open(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	ops, err := j.List()
	if err != nil || len(ops) != 1 {
		t.Fatalf("prepared journal: %v %v", ops, err)
	}
	commitBirthJournal(t, cwd, ops[0].Event.ID)
	runLifecycleGit(t, cwd, "switch", "-c", "feature")
	store := storage.NewWorktreeFileStore(cwd, gitOut(cwd, "rev-parse", "--absolute-git-dir"), "feature", oid)
	c.History = app.NewContextHistoryService(store, store)
	return cwd, c, j, oid
}

func TestPostCheckoutPublicationFollowsPreparationAndCommit(t *testing.T) {
	for _, mode := range []string{"replay", "activation", "replay-and-activation", "preparation-fails", "cancelled", "boundary-fails"} {
		t.Run(mode, func(t *testing.T) {
			cwd, c, journal, oid := checkoutPublicationFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "boundary-fails" {
				if err := os.Mkdir(filepath.Join(cwd, ".cxt", "boundary.json"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "activation" {
				// This birth was already applied by a previous invocation. Only
				// activation inside contextSwitch should request publication now.
				if err := replayBranchOperationsForRefWithPublication(ctx, c, cwd, "refs/heads/feature", func(string) {}); err != nil {
					t.Fatal(err)
				}
			}
			var order []string
			published := 0
			prepared := false
			c.Checkout = publicationCheckout(func(ctx context.Context, in inbound.CheckoutInput) (inbound.CheckoutOutput, error) {
				if published != 0 || in.SkipMaterialize {
					t.Fatal("publication interleaved wrapper preparation or materialization was skipped")
				}
				order = append(order, "prepare")
				switch mode {
				case "preparation-fails":
					return inbound.CheckoutOutput{}, domain.ErrSelectionChanged
				case "cancelled":
					cancel()
					return inbound.CheckoutOutput{}, ctx.Err()
				}
				prepared = true
				id := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
				return inbound.CheckoutOutput{
					Head: domain.HashContent([]byte("prepared target")), WrittenPath: filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "rollout-2026-10-05T00-00-00-"+id+".jsonl"),
					ResumeCmd: "codex resume " + id, ActivatedBranch: mode != "replay",
				}, nil
			})
			list := c.List
			c.List = publicationList(func(ctx context.Context, in inbound.ListInput) (inbound.ListOutput, error) {
				if prepared {
					// Settings preparation runs after the activation request but
					// before the durable boundary is committed.
					order = append(order, "finish preparation")
					if published != 0 {
						t.Fatal("activation published before foreground commit")
					}
				}
				return list.List(ctx, in)
			})
			publish := func(dir string) {
				published++
				order = append(order, "publish")
				if dir != cwd {
					t.Fatal("publication escaped its worktree")
				}
				_, committed := boundary.Load(cwd)
				if committed != (prepared && mode != "boundary-fails") {
					t.Fatal("publication preceded commit or failed preparation committed a boundary")
				}
			}
			if err := runGitHookWithPublication(ctx, c, cwd, []string{"post-checkout", oid, oid, "1"}, publish); err != nil {
				t.Fatal(err)
			}
			want := []string{"prepare", "publish"}
			if prepared {
				want = []string{"prepare", "finish preparation", "publish"}
			}
			if published != 1 || !reflect.DeepEqual(order, want) {
				t.Fatalf("publication order = %v, want %v; starts = %d", order, want, published)
			}
			ops, err := journal.List()
			if err != nil || len(ops) != 1 || ops[0].Phase != "applied" {
				t.Fatalf("durable birth lost after foreground return: %v %v", ops, err)
			}
		})
	}
}

func TestPostCheckoutFailedReplayKeepsQueuedVote(t *testing.T) {
	cwd, c, journal, oid := checkoutPublicationFixture(t)
	ops, err := journal.List()
	if err != nil {
		t.Fatal(err)
	}
	op := ops[0]
	op.Binding = &branchjournal.BindingIntent{Kind: "unavailable"}
	if err := journal.Transaction(context.Background(), func() error { return journal.Save(op) }); err != nil {
		t.Fatal(err)
	}
	c.Checkout = publicationCheckout(func(context.Context, inbound.CheckoutInput) (inbound.CheckoutOutput, error) {
		t.Fatal("failed replay reached preparation")
		return inbound.CheckoutOutput{}, nil
	})
	published := 0
	if err := runGitHookWithPublication(context.Background(), c, cwd, []string{"post-checkout", oid, oid, "1"}, func(string) { published++ }); err != nil {
		t.Fatal(err)
	}
	ops, err = journal.List()
	if err != nil || len(ops) != 1 || ops[0].Phase != "committed" || ops[0].LastError == "" || published != 0 {
		t.Fatalf("failed vote not retained for retry: %v %v, starts=%d", ops, err, published)
	}
}

func TestPostCheckoutPartialReplayPublishesAppliedVoteAndKeepsFailureQueued(t *testing.T) {
	cwd, c, journal, oid := checkoutPublicationFixture(t)
	ops, err := journal.List()
	if err != nil {
		t.Fatal(err)
	}
	failed := ops[0]
	failed.Event.ID = strings.Repeat("f", 32)
	failed.Event.CreatedAt = failed.Event.CreatedAt.Add(time.Second)
	failed.Binding = &branchjournal.BindingIntent{Kind: "unavailable"}
	if err := journal.Transaction(context.Background(), func() error { return journal.Save(failed) }); err != nil {
		t.Fatal(err)
	}
	c.Checkout = publicationCheckout(func(context.Context, inbound.CheckoutInput) (inbound.CheckoutOutput, error) {
		t.Fatal("partial replay failure reached preparation")
		return inbound.CheckoutOutput{}, nil
	})
	published := 0
	if err := runGitHookWithPublication(context.Background(), c, cwd, []string{"post-checkout", oid, oid, "1"}, func(string) { published++ }); err != nil {
		t.Fatal(err)
	}
	ops, err = journal.List()
	if err != nil || len(ops) != 2 || ops[0].Phase != "applied" || ops[1].Phase != "committed" || ops[1].LastError == "" || published != 1 {
		t.Fatalf("partial failure lost applied publication or queued retry: %v %v, starts=%d", ops, err, published)
	}
}

type publicationSelectionFailure struct {
	inbound.ContextHistory
	selected bool
}

func (h *publicationSelectionFailure) SelectPosition(ctx context.Context, p domain.WorkingPosition) error {
	if err := h.ContextHistory.SelectPosition(ctx, p); err != nil {
		return err
	}
	h.selected = true
	return nil
}

func (h *publicationSelectionFailure) CurrentPosition(ctx context.Context) (domain.WorkingPosition, error) {
	if h.selected {
		return domain.WorkingPosition{}, domain.ErrSelectionChanged
	}
	return h.ContextHistory.CurrentPosition(ctx)
}

func TestPostCheckoutSelectionFailureStillPublishesAppliedBirth(t *testing.T) {
	cwd, c, journal, oid := checkoutPublicationFixture(t)
	c.History = &publicationSelectionFailure{ContextHistory: c.History}
	published := 0
	err := runGitHookWithPublication(context.Background(), c, cwd, []string{"post-checkout", oid, oid, "1"}, func(string) { published++ })
	if !errors.Is(err, domain.ErrSelectionChanged) || published != 1 {
		t.Fatalf("selection failure lost publication or was hidden: %v, starts=%d", err, published)
	}
	ops, err := journal.List()
	if err != nil || len(ops) != 1 || ops[0].Phase != "applied" {
		t.Fatalf("applied vote lost: %v %v", ops, err)
	}
	if _, ok := boundary.Load(cwd); ok {
		t.Fatal("failed selection created a restart boundary")
	}
}

func TestPostCheckoutEarlyReturnStillPublishesAppliedBirth(t *testing.T) {
	for _, mode := range []string{"file-checkout", "missing-args", "keep-session", "carry", "prepare-mode"} {
		t.Run(mode, func(t *testing.T) {
			cwd, c, journal, oid := checkoutPublicationFixture(t)
			args := []string{"post-checkout", oid, oid, "1"}
			switch mode {
			case "file-checkout":
				args[3] = "0"
			case "missing-args":
				args = args[:1]
			case "keep-session":
				t.Setenv("CXT_KEEP_SESSION", "1")
			case "carry":
				t.Setenv("CXT_CARRY", "1")
			case "prepare-mode":
				if err := remotecfg.SetCheckoutMode(context.Background(), cwd, remotecfg.CheckoutPrepare); err != nil {
					t.Fatal(err)
				}
			}
			c.Checkout = publicationCheckout(func(context.Context, inbound.CheckoutInput) (inbound.CheckoutOutput, error) {
				t.Fatal("early return reached preparation")
				return inbound.CheckoutOutput{}, nil
			})
			published := 0
			if err := runGitHookWithPublication(context.Background(), c, cwd, args, func(string) { published++ }); err != nil {
				t.Fatal(err)
			}
			ops, err := journal.List()
			if err != nil || len(ops) != 1 || ops[0].Phase != "applied" || published != 1 {
				t.Fatalf("early return lost applied publication: %v %v, starts=%d", ops, err, published)
			}
			if _, ok := boundary.Load(cwd); ok {
				t.Fatal("early return created a restart boundary")
			}
		})
	}
}

func TestStandaloneReplayPublishesImmediatelyWithoutWaitingForGit(t *testing.T) {
	cwd, c, journal, _ := checkoutPublicationFixture(t)
	ops, err := journal.List()
	if err != nil {
		t.Fatal(err)
	}
	op := ops[0]
	// A live creator must not become a dependency of foreground replay. The
	// detached branch-replay handler owns its separate wait-for-Git behavior.
	op.GitPID = strconv.Itoa(os.Getpid())
	if err := journal.Transaction(context.Background(), func() error { return journal.Save(op) }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	published := 0
	if err := replayBranchOperationsForRefWithPublication(ctx, c, cwd, "", func(dir string) {
		published++
		if dir != cwd {
			t.Fatal("publication escaped its worktree")
		}
	}); err != nil {
		t.Fatal(err)
	}
	if published != 1 || ctx.Err() != nil {
		t.Fatalf("standalone replay deferred publication or waited for live Git: starts=%d, ctx=%v", published, ctx.Err())
	}
	if err := replayBranchOperationsForRefWithPublication(ctx, c, cwd, "", func(string) { published++ }); err != nil {
		t.Fatal(err)
	}
	if published != 1 {
		t.Fatal("already-applied replay started redundant publication")
	}
}
