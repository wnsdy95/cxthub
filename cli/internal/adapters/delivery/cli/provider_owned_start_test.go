package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/boundary"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type ownedLaunchReceiptWarningWriter struct {
	bytes.Buffer
	onWarning func()
}

func (w *ownedLaunchReceiptWarningWriter) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if bytes.Contains(p, []byte("native runtime launch receipt was not confirmed saved")) {
		w.onWarning()
	}
	return n, err
}

func TestDeferredOwnedStartLaunchReceiptFailureKeepsSupervising(t *testing.T) {
	for _, stage := range []string{"productive exit", "cancellation during receipt", "cancellation after warning", "runtime failure", "cleanup failure"} {
		t.Run(stage, func(t *testing.T) {
			root, _ := providerLaunchFixture(t, "claude", `while [ ! -f "$LAUNCH_LOG.release" ]; do sleep 0.01; done
printf continued > "$LAUNCH_LOG"`)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			storeErr := errors.New("PRIVATE receipt store failure")
			lifecycleErr := errors.New("PRIVATE runtime or cleanup failure")
			failures := make(chan error, 1)
			var child *exec.Cmd
			var states []string
			started, activated, cleaned, warnings := 0, 0, 0, 0
			hooks := ProviderLaunchHooks{
				PrepareDeferred: func(ctx context.Context, r ProviderLaunchRequest) (DeferredProviderLaunch, error) {
					d := deferredLaunchFixture(ctx, r, func() error {
						cleaned++
						if stage == "cleanup failure" {
							return lifecycleErr
						}
						return nil
					})
					d.SessionID, d.Failure = r.NativeSessionID(), failures
					d.Activate = func(context.Context) error { activated++; return nil }
					d.Start = func(ctx context.Context, in io.Reader, out, stderr io.Writer, terminalStarted func()) (*exec.Cmd, error) {
						started++
						var err error
						child, err = ownedStartTestCommand(r, in, out, stderr, terminalStarted)
						return child, err
					}
					return d, nil
				},
				Record: func(_ context.Context, receipt ProviderLaunchReceipt) error {
					states = append(states, receipt.State)
					if receipt.State == "runtime_launched" {
						if stage == "cancellation during receipt" {
							cancel()
						}
						return storeErr
					}
					return nil
				},
			}
			stderr := &ownedLaunchReceiptWarningWriter{onWarning: func() {
				warnings++
				if cleaned != 0 || started != 1 || activated != 1 {
					t.Errorf("warning crossed lifecycle boundary: cleaned=%d started=%d activated=%d", cleaned, started, activated)
				}
				switch stage {
				case "cancellation after warning":
					cancel()
				case "runtime failure":
					failures <- lifecycleErr
				default:
					// The child can complete more work only after the failed
					// receipt is reported, proving it remains supervised.
					if err := os.WriteFile(filepath.Join(root, "launch.log.release"), nil, 0600); err != nil {
						t.Error(err)
					}
				}
			}}
			runtime := launchTestRuntime()
			runtime.stderr = stderr
			err := runProviderLaunch(ctx, root, LaunchIntent{Provider: domain.ProviderClaude, Pull: true, ContextBudget: 200000}, hooks, runtime)
			var wantErr error
			if strings.HasPrefix(stage, "cancellation") {
				wantErr = context.Canceled
			} else if stage != "productive exit" {
				wantErr = lifecycleErr
			}
			if !errors.Is(err, wantErr) || errors.Is(err, storeErr) || started != 1 || activated != 1 || cleaned != 1 || child == nil || child.ProcessState == nil {
				t.Fatalf("err=%v started=%d activated=%d cleaned=%d child=%v", err, started, activated, cleaned, child)
			}
			wantWarnings := 1
			if stage == "cancellation during receipt" {
				wantWarnings = 0
			}
			if warnings != wantWarnings || strings.Contains(stderr.String(), "PRIVATE") {
				t.Fatalf("warnings=%d stderr=%s", warnings, stderr.String())
			}
			wantStates := []string{"runtime_prepared", "runtime_starting", "runtime_launched"}
			if wantErr != nil {
				wantStates = append(wantStates, "runtime_failed")
			}
			if !reflect.DeepEqual(states, wantStates) {
				t.Fatalf("states=%v want=%v", states, wantStates)
			}
			if stage == "productive exit" || stage == "cleanup failure" {
				raw, readErr := os.ReadFile(filepath.Join(root, "launch.log"))
				if readErr != nil || string(raw) != "continued" || !child.ProcessState.Success() {
					t.Fatalf("productive child stopped: output=%q readErr=%v state=%v", raw, readErr, child.ProcessState)
				}
			}
		})
	}
}

func TestDeferredOwnedStartRunsAfterPreparationAndUsesFrozenCallback(t *testing.T) {
	root, _ := providerLaunchFixture(t, "claude", "exit 0")
	intent := LaunchIntent{Provider: domain.ProviderClaude, Pull: true, ContextBudget: 200000}
	var returned DeferredProviderLaunch
	var events []string
	hooks := ProviderLaunchHooks{
		PrepareDeferred: func(ctx context.Context, r ProviderLaunchRequest) (DeferredProviderLaunch, error) {
			returned = deferredLaunchFixture(ctx, r, func() error { events = append(events, "cleanup"); return nil })
			returned.SessionID = r.NativeSessionID()
			returned.Activate = func(context.Context) error {
				events = append(events, "activate")
				return nil
			}
			args := append([]string(nil), returned.Args...)
			returned.Start = func(ctx context.Context, in io.Reader, out, stderr io.Writer, terminalStarted func()) (*exec.Cmd, error) {
				events = append(events, "owned_start")
				return ownedStartTestCommand(r, in, out, stderr, terminalStarted, args...)
			}
			return returned, nil
		},
		Record: func(_ context.Context, r ProviderLaunchReceipt) error {
			events = append(events, r.State)
			if r.State == "runtime_prepared" {
				returned.Start = func(context.Context, io.Reader, io.Writer, io.Writer, func()) (*exec.Cmd, error) {
					t.Error("mutated callback used")
					return nil, errors.New("mutated callback used")
				}
			}
			return nil
		},
	}
	if err := runProviderLaunch(context.Background(), root, intent, hooks, launchTestRuntime()); err != nil {
		t.Fatal(err)
	}
	want := []string{"runtime_prepared", "runtime_starting", "activate", "owned_start", "runtime_launched", "cleanup"}
	if len(events) != len(want) {
		t.Fatalf("events=%v", events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events=%v", events)
		}
	}
}

func TestDeferredOwnedStartFailureNeverFallsBack(t *testing.T) {
	root, _ := providerLaunchFixture(t, "claude", "exit 99")
	intent := LaunchIntent{Provider: domain.ProviderClaude, Pull: true, ContextBudget: 200000}
	started, cleaned := 0, 0
	hooks := ProviderLaunchHooks{
		PrepareDeferred: func(ctx context.Context, r ProviderLaunchRequest) (DeferredProviderLaunch, error) {
			d := deferredLaunchFixture(ctx, r, func() error { cleaned++; return nil })
			d.SessionID = r.NativeSessionID()
			d.Start = func(context.Context, io.Reader, io.Writer, io.Writer, func()) (*exec.Cmd, error) {
				started++
				return nil, context.Canceled
			}
			return d, nil
		}, Record: func(context.Context, ProviderLaunchReceipt) error { return nil },
	}
	err := runProviderLaunch(context.Background(), root, intent, hooks, launchTestRuntime())
	if !errors.Is(err, context.Canceled) || started != 1 || cleaned != 1 {
		t.Fatalf("err=%v started=%d cleaned=%d", err, started, cleaned)
	}
}

func TestDeferredOwnedReplacementDoesNotReadQuestionWhileOldTUIRuns(t *testing.T) {
	root, _ := providerLaunchFixture(t, "claude", `if [ "$1" = first ]; then
  trap 'printf stopped > "$LAUNCH_LOG.stopped"; exit 0' TERM
  printf ready
  while :; do sleep 0.01; done
fi`)
	intent := LaunchIntent{Provider: domain.ProviderClaude, Pull: true, ContextBudget: 200000, ProviderArgs: []string{"original question"}}
	var ids []string
	started, cleaned := 0, 0
	hooks := ProviderLaunchHooks{
		PrepareDeferred: func(ctx context.Context, r ProviderLaunchRequest) (DeferredProviderLaunch, error) {
			index := len(ids)
			if index > 1 {
				t.Fatal("extra replacement")
			}
			ids = append(ids, r.NativeSessionID())
			if index == 1 {
				if _, err := os.Stat(filepath.Join(root, "launch.log.stopped")); !os.IsNotExist(err) {
					t.Fatal("old TUI stopped before replacement preparation", err)
				}
				p, err := r.InitialPrompt()
				if err != nil || p.Present() {
					t.Fatal("original question replayed during replacement", err)
				}
			}
			d := deferredLaunchFixture(ctx, r, func() error { cleaned++; return nil })
			d.SessionID = r.NativeSessionID()
			d.Args = []string{"next"}
			if index == 0 {
				d.Args = []string{"first"}
			}
			for _, item := range d.Env {
				if strings.HasPrefix(item, "CXT_WRAPPED_SESSION_ID=") && item != "CXT_WRAPPED_SESSION_ID="+d.SessionID {
					t.Fatal("capture uses another session")
				}
			}
			args := d.Args
			d.Start = func(ctx context.Context, in io.Reader, out, stderr io.Writer, terminalStarted func()) (*exec.Cmd, error) {
				if index == 1 {
					if _, err := os.Stat(filepath.Join(root, "launch.log.stopped")); err != nil {
						return nil, errors.New("new question began before old TUI stopped")
					}
				}
				started++
				return ownedStartTestCommand(r, in, out, stderr, terminalStarted, args...)
			}
			return d, nil
		}, Record: func(context.Context, ProviderLaunchReceipt) error { return nil },
	}
	runtime := launchTestRuntime()
	runtime.stdout = &providerLaunchBoundaryWriter{root: root}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runProviderLaunch(ctx, root, intent, hooks, runtime); err != nil {
		t.Fatal(err)
	}
	if started != 2 || cleaned != 2 || len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("started=%d cleaned=%d ids=%v", started, cleaned, ids)
	}
}

func ownedStartTestCommand(r ProviderLaunchRequest, in io.Reader, out, stderr io.Writer, terminalStarted func(), args ...string) (*exec.Cmd, error) {
	cmd := exec.Command(r.Executable, args...)
	cmd.Dir, cmd.Env = r.Cwd, r.Environment(context.Background())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	terminalStarted()
	return cmd, nil
}

func TestDeferredOwnedStartReleaseRequiresDurableStartingAndActivation(t *testing.T) {
	for _, stage := range []string{"receipt failure", "receipt cancellation", "activation failure", "activation cancellation"} {
		t.Run(stage, func(t *testing.T) {
			root, _ := providerLaunchFixture(t, "claude", "exit 99")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			failure := errors.New("PRIVATE release failure")
			activated, started, cleaned := 0, 0, 0
			var receipts []ProviderLaunchReceipt
			hooks := ProviderLaunchHooks{
				PrepareDeferred: func(ctx context.Context, r ProviderLaunchRequest) (DeferredProviderLaunch, error) {
					d := deferredLaunchFixture(ctx, r, func() error { cleaned++; return nil })
					d.SessionID = r.NativeSessionID()
					d.Activate = func(context.Context) error {
						activated++
						if stage == "activation cancellation" {
							cancel()
							return nil
						}
						return failure
					}
					d.Start = func(context.Context, io.Reader, io.Writer, io.Writer, func()) (*exec.Cmd, error) {
						started++
						return nil, errors.New("first turn was released")
					}
					return d, nil
				},
				Record: func(_ context.Context, r ProviderLaunchReceipt) error {
					receipts = append(receipts, r)
					if r.State == "runtime_starting" {
						if stage == "receipt failure" {
							return failure
						}
						if stage == "receipt cancellation" {
							cancel()
						}
					}
					return nil
				},
			}
			err := runProviderLaunch(ctx, root, LaunchIntent{Provider: domain.ProviderClaude, Pull: true, ContextBudget: 200000}, hooks, launchTestRuntime())
			want := failure
			if strings.HasSuffix(stage, "cancellation") {
				want = context.Canceled
			}
			wantActivated := 0
			if strings.HasPrefix(stage, "activation") {
				wantActivated = 1
			}
			if !errors.Is(err, want) || started != 0 || cleaned != 1 || activated != wantActivated {
				t.Fatalf("err=%v started=%d activated=%d cleaned=%d", err, started, activated, cleaned)
			}
			if len(receipts) != 3 || receipts[1].State != "runtime_starting" || receipts[2].State != "runtime_failed" {
				t.Fatalf("receipts=%v", receipts)
			}
			assertRuntimeReceipts(t, receipts)
		})
	}
}

func TestDeferredOwnedStartFailureAndCancellationJoinLateChild(t *testing.T) {
	for _, cause := range []string{"runtime failure", "cancellation"} {
		t.Run(cause, func(t *testing.T) {
			root, _ := providerLaunchFixture(t, "claude", "exec sleep 60")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			failure := errors.New("PRIVATE runtime failure")
			failures := make(chan error, 1)
			returned := make(chan struct{})
			var child *exec.Cmd
			cleaned := 0
			launched := false
			hooks := ProviderLaunchHooks{
				PrepareDeferred: func(ctx context.Context, r ProviderLaunchRequest) (DeferredProviderLaunch, error) {
					d := deferredLaunchFixture(ctx, r, func() error {
						cleaned++
						select {
						case <-returned:
						default:
							t.Error("cleanup ran before starter returned")
						}
						if child == nil || child.ProcessState == nil {
							t.Error("late child was not reaped before cleanup")
						}
						return nil
					})
					d.SessionID, d.Failure = r.NativeSessionID(), failures
					d.Start = func(ctx context.Context, in io.Reader, out, stderr io.Writer, terminalStarted func()) (*exec.Cmd, error) {
						defer close(returned)
						if cause == "runtime failure" {
							failures <- failure
						} else {
							cancel()
						}
						<-ctx.Done()
						var err error
						child, err = ownedStartTestCommand(r, in, out, stderr, terminalStarted)
						// Even a child created in the cancellation race belongs to
						// the supervisor, whether returned with an error or success.
						if cause == "cancellation" {
							err = errors.Join(err, ctx.Err())
						}
						return child, err
					}
					return d, nil
				},
				Record: func(_ context.Context, r ProviderLaunchReceipt) error {
					launched = launched || r.State == "runtime_launched"
					return nil
				},
			}
			err := runProviderLaunch(ctx, root, LaunchIntent{Provider: domain.ProviderClaude, Pull: true, ContextBudget: 200000}, hooks, launchTestRuntime())
			want := failure
			if cause == "cancellation" {
				want = context.Canceled
			} else if ctx.Err() != nil {
				t.Fatal("fatal event was not observed before outer deadline")
			}
			if !errors.Is(err, want) || cleaned != 1 || launched || len(failures) != 0 {
				t.Fatalf("err=%v cleaned=%d launched=%v pending=%d", err, cleaned, launched, len(failures))
			}
		})
	}
}

func TestDeferredOwnedStartBranchReplacementJoinsBeforeRelease(t *testing.T) {
	for _, retirement := range []string{"success", "starter failure", "cleanup failure", "late failure event"} {
		t.Run(retirement, func(t *testing.T) {
			root, _ := providerLaunchFixture(t, "claude", `if [ "$1" = old ]; then exec sleep 60; fi`)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			failure := errors.New("PRIVATE retirement failure")
			failures := make(chan error, 1)
			entered := make(chan context.Context, 1)
			returned := make(chan struct{})
			var lateChild *exec.Cmd
			var requests []ProviderLaunchRequest
			var receipts []ProviderLaunchReceipt
			started, cleaned := [2]int{}, [2]int{}
			hooks := ProviderLaunchHooks{
				PrepareDeferred: func(ctx context.Context, r ProviderLaunchRequest) (DeferredProviderLaunch, error) {
					index := len(requests)
					if index > 1 {
						t.Fatal("unexpected replacement")
					}
					requests = append(requests, r)
					var old context.Context
					if index == 1 {
						old = <-entered
						if old.Err() != nil {
							t.Fatal("starter canceled before replacement preparation")
						}
						if !reflect.DeepEqual(r.Intent.ProviderArgs, []string{"--model=fixture"}) || r.Transition == nil {
							t.Fatal("restart replayed prompt or lost settings/transition")
						}
					}
					d := deferredLaunchFixture(ctx, r, func() error {
						cleaned[index]++
						if index == 0 {
							select {
							case <-returned:
							default:
								t.Error("old starter not joined before cleanup")
							}
							if lateChild == nil || lateChild.ProcessState == nil {
								t.Error("late child not reaped before cleanup")
							}
							if retirement == "cleanup failure" {
								return failure
							}
						}
						return nil
					})
					d.SessionID = r.NativeSessionID()
					if index == 0 {
						d.Failure = failures
					} else {
						validations := 0
						d.Validate = func(context.Context) error {
							validations++
							if validations <= 2 && old.Err() != nil {
								t.Error("starter canceled before replacement validation")
							}
							return nil
						}
					}
					d.Start = func(ctx context.Context, in io.Reader, out, stderr io.Writer, terminalStarted func()) (*exec.Cmd, error) {
						started[index]++
						if index == 1 {
							if cleaned[0] != 1 || lateChild.ProcessState == nil {
								return nil, errors.New("replacement released before retirement")
							}
							return ownedStartTestCommand(r, in, out, stderr, terminalStarted)
						}
						defer close(returned)
						entered <- ctx
						if err := boundary.Record(root, boundary.Boundary{PrevBranch: "main", Branch: "feature", SeedID: launchSessionID}); err != nil {
							return nil, err
						}
						<-ctx.Done()
						var err error
						lateChild, err = ownedStartTestCommand(r, in, out, stderr, terminalStarted, "old")
						if retirement == "starter failure" {
							err = errors.Join(err, failure)
						}
						if retirement == "late failure event" {
							failures <- failure
						}
						return lateChild, errors.Join(ctx.Err(), err)
					}
					return d, nil
				},
				Record: func(_ context.Context, r ProviderLaunchReceipt) error { receipts = append(receipts, r); return nil },
			}
			intent := LaunchIntent{Provider: domain.ProviderClaude, Pull: true, ContextBudget: 200000, ProviderArgs: []string{"--model=fixture", "original question"}}
			err := runProviderLaunch(ctx, root, intent, hooks, launchTestRuntime())
			wantStarted := [2]int{1, 0}
			if retirement == "success" {
				wantStarted[1] = 1
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, failure) {
				t.Fatalf("retirement failure lost: %v", err)
			}
			if ctx.Err() != nil || started != wantStarted || cleaned != [2]int{1, 1} || len(requests) != 2 {
				t.Fatalf("deadline=%v started=%v cleaned=%v preparations=%d", ctx.Err(), started, cleaned, len(requests))
			}
			if requests[0].NativeSessionID() == requests[1].NativeSessionID() {
				t.Fatal("replacement reused capture identity")
			}
			for _, r := range receipts {
				if r.State == "runtime_launched" && (retirement != "success" || r.SessionID == requests[0].NativeSessionID()) {
					t.Fatal("retired or failed starter claimed a launched TUI")
				}
			}
			assertRuntimeReceipts(t, receipts)
		})
	}
}

func TestDeferredOwnedStartPreservedWhenReplacementPreparationFails(t *testing.T) {
	for _, stage := range []string{"prepare", "receipt", "validation"} {
		t.Run(stage, func(t *testing.T) {
			root, _ := providerLaunchFixture(t, "claude", "exit 0")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			release := make(chan struct{})
			entered := make(chan context.Context, 1)
			calls, started := 0, 0
			cleaned := [2]int{}
			preparationFailed := func() error { close(release); return errors.New("PRIVATE replacement preparation") }
			hooks := ProviderLaunchHooks{
				PrepareDeferred: func(ctx context.Context, r ProviderLaunchRequest) (DeferredProviderLaunch, error) {
					index := calls
					calls++
					if index > 1 {
						t.Fatal("same failed boundary retried")
					}
					d := deferredLaunchFixture(ctx, r, func() error { cleaned[index]++; return nil })
					d.SessionID = r.NativeSessionID()
					d.Start = func(ctx context.Context, in io.Reader, out, stderr io.Writer, terminalStarted func()) (*exec.Cmd, error) {
						started++
						entered <- ctx
						if err := boundary.Record(root, boundary.Boundary{PrevBranch: "main", Branch: "feature", SeedID: launchSessionID}); err != nil {
							return nil, err
						}
						select {
						case <-release:
						case <-ctx.Done():
							return nil, ctx.Err()
						}
						if err := ctx.Err(); err != nil {
							return nil, err
						}
						return ownedStartTestCommand(r, in, out, stderr, terminalStarted)
					}
					if index == 1 {
						if old := <-entered; old.Err() != nil {
							t.Fatal("old starter canceled before preparation")
						}
						if stage == "prepare" {
							return d, preparationFailed()
						}
						validations := 0
						d.Validate = func(context.Context) error {
							validations++
							if stage == "validation" && validations == 2 {
								return preparationFailed()
							}
							return nil
						}
					}
					return d, nil
				},
				Record: func(_ context.Context, r ProviderLaunchReceipt) error {
					if calls == 2 && r.State == "runtime_prepared" && stage == "receipt" {
						return preparationFailed()
					}
					return nil
				},
			}
			err := runProviderLaunch(ctx, root, LaunchIntent{Provider: domain.ProviderClaude, Pull: true, ContextBudget: 200000}, hooks, launchTestRuntime())
			if err != nil || ctx.Err() != nil || calls != 2 || started != 1 || cleaned != [2]int{1, 1} {
				t.Fatalf("err=%v deadline=%v calls=%d started=%d cleaned=%v", err, ctx.Err(), calls, started, cleaned)
			}
		})
	}
}
