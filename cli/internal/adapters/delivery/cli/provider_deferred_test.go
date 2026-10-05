package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func deferredLaunchFixture(ctx context.Context, r ProviderLaunchRequest, cleanup func() error) DeferredProviderLaunch {
	return DeferredProviderLaunch{
		// A synthetic private transport deliberately differs from resumeArgs.
		Args: append([]string{"--owned-runtime", launchSessionID}, r.Intent.ProviderArgs...),
		Env:  r.Environment(ctx), SessionID: launchSessionID,
		Validate: func(ctx context.Context) error { return ctx.Err() }, Activate: func(ctx context.Context) error { return ctx.Err() }, Cleanup: cleanup,
	}
}

func deferredLaunchIntent() LaunchIntent {
	return LaunchIntent{Provider: domain.ProviderCodex, Pull: true, ContextBudget: 200000, ProviderArgs: []string{"--model", "fixture", "initial private task"}}
}

func assertRuntimeReceipts(t *testing.T, receipts []ProviderLaunchReceipt) {
	t.Helper()
	for _, r := range receipts {
		if !strings.HasPrefix(r.State, "runtime_") || r.Acceptance != "unknown" || r.PackageHash != "" || r.Budget != nil || r.Bootstrap != nil ||
			r.SelectedTokens != 0 || r.TokenMeasurement != "" || r.RequestedBudget != 0 || r.CodeCommit != "" || r.SourceRevision != "" || r.Capability != "" || r.TurnID != "" || r.Outcome != "" {
			t.Fatalf("runtime receipt claimed package delivery: %+v", r)
		}
		raw, err := json.Marshal(r)
		if err != nil || strings.Contains(string(raw), "PRIVATE") || strings.Contains(string(raw), "initial private task") {
			t.Fatalf("runtime receipt leaked input: %s / %v", raw, err)
		}
	}
}

func TestDeferredProviderLaunchOwnsInvocationAndEnvironment(t *testing.T) {
	root, _ := providerLaunchFixture(t, "codex", `printf '%s\n' "$@" >> "$LAUNCH_LOG"
printf '%s\n' "$DEFERRED_MARKER" "$CXT_WRAPPER_TRANSITION_PROTOCOL" "$CXT_WRAPPED" "$CXT_WRAPPED_SESSION_ID" > "$LAUNCH_LOG.env"`)
	t.Setenv("DEFERRED_MARKER", "captured")
	t.Setenv("CXT_WRAPPED_SESSION_ID", "inherited-private-session")
	t.Setenv("CXT_WRAPPER_TRANSITION_PROTOCOL", "stale")
	var returned DeferredProviderLaunch
	var wantArgs []string
	var receipts []ProviderLaunchReceipt
	cleaned := 0
	hooks := ProviderLaunchHooks{
		Prepare: func(context.Context, ProviderLaunchRequest) (PreparedProviderLaunch, error) {
			t.Fatal("deferred route fell back to materialization")
			return PreparedProviderLaunch{}, nil
		},
		PrepareDeferred: func(ctx context.Context, req ProviderLaunchRequest) (DeferredProviderLaunch, error) {
			first := req.Environment(ctx)
			first[0] = "PRIVATE_MUTATION=1"
			if reflect.DeepEqual(first, req.Environment(ctx)) {
				t.Fatal("environment aliases caller")
			}
			returned = deferredLaunchFixture(ctx, req, func() error { cleaned++; return nil })
			wantArgs = append([]string(nil), returned.Args...)
			return returned, nil
		},
		Record: func(_ context.Context, r ProviderLaunchReceipt) error {
			receipts = append(receipts, r)
			if r.State == "runtime_prepared" {
				returned.Args[0] = "PRIVATE_MUTATION"
				returned.Env[0] = "PRIVATE_MUTATION=1"
				returned.Activate = func(context.Context) error { return errors.New("PRIVATE activation replaced") }
				t.Setenv("DEFERRED_MARKER", "changed after preparation")
			}
			return nil
		},
	}
	if err := runProviderLaunch(context.Background(), root, deferredLaunchIntent(), hooks, launchTestRuntime()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "launch.log"))
	if err != nil || string(raw) != strings.Join(wantArgs, "\n")+"\n" {
		t.Fatalf("invocation changed or index warming ran: %q / %v", raw, err)
	}
	env, err := os.ReadFile(filepath.Join(root, "launch.log.env"))
	if err != nil || string(env) != "captured\nprepare-first-v1\n1\n\n" {
		t.Fatalf("TUI environment differed from prepared runtime: %q / %v", env, err)
	}
	if cleaned != 1 || len(receipts) != 2 || receipts[0].State != "runtime_prepared" || receipts[1].State != "runtime_launched" || receipts[1].SessionID != launchSessionID {
		t.Fatalf("cleanup=%d receipts=%+v", cleaned, receipts)
	}
	assertRuntimeReceipts(t, receipts)
}

func TestDeferredProviderLaunchFailuresNeverStartOrFallback(t *testing.T) {
	for _, failure := range []string{"prepare", "validate", "receipt", "last validation", "environment", "start", "failure channel", "missing activation"} {
		t.Run(failure, func(t *testing.T) {
			root, bin := providerLaunchFixture(t, "codex", "")
			cleaned, validations := 0, 0
			var receipts []ProviderLaunchReceipt
			secret := errors.New("PRIVATE native path and prompt")
			hooks := ProviderLaunchHooks{
				Prepare: func(context.Context, ProviderLaunchRequest) (PreparedProviderLaunch, error) {
					t.Fatal("fallback attempted")
					return PreparedProviderLaunch{}, nil
				},
				PrepareDeferred: func(ctx context.Context, r ProviderLaunchRequest) (DeferredProviderLaunch, error) {
					d := deferredLaunchFixture(ctx, r, func() error { cleaned++; return nil })
					d.Validate = func(context.Context) error {
						validations++
						if failure == "validate" || failure == "last validation" && validations == 2 {
							return secret
						}
						return nil
					}
					if failure == "prepare" {
						return d, secret
					}
					if failure == "environment" {
						d.Env = append(d.Env, "PRIVATE_MUTATION=1")
					}
					if failure == "missing activation" {
						d.Activate = nil
					}
					if failure == "failure channel" {
						ch := make(chan error, 1)
						ch <- secret
						d.Failure = ch
					}
					return d, nil
				},
				Record: func(_ context.Context, r ProviderLaunchReceipt) error {
					receipts = append(receipts, r)
					if r.State == "runtime_prepared" {
						if failure == "receipt" {
							return secret
						}
						if failure == "start" {
							return os.Remove(bin)
						}
					}
					return nil
				},
			}
			err := runProviderLaunch(context.Background(), root, deferredLaunchIntent(), hooks, launchTestRuntime())
			if err == nil || strings.Contains(fmt.Sprintf("%+v %#v", err, err), "PRIVATE") || cleaned != 1 {
				t.Fatalf("err=%v cleanup=%d", err, cleaned)
			}
			if _, err := os.Stat(filepath.Join(root, "launch.log")); !os.IsNotExist(err) {
				t.Fatal("child started")
			}
			assertRuntimeReceipts(t, receipts)
			for _, r := range receipts {
				if r.State == "runtime_launched" {
					t.Fatal("failed start claimed launch")
				}
			}
		})
	}
}

func TestDeferredProviderLaunchCancellationCleansUp(t *testing.T) {
	for _, stage := range []string{"prepare", "prepared", "launched"} {
		t.Run(stage, func(t *testing.T) {
			root, _ := providerLaunchFixture(t, "codex", `trap 'exit 0' TERM
while :; do sleep 0.01; done`)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cleaned := 0
			var receipts []ProviderLaunchReceipt
			hooks := ProviderLaunchHooks{
				PrepareDeferred: func(ctx context.Context, r ProviderLaunchRequest) (DeferredProviderLaunch, error) {
					d := deferredLaunchFixture(ctx, r, func() error { cleaned++; return nil })
					if stage == "prepare" {
						cancel()
					}
					return d, nil
				},
				Record: func(_ context.Context, r ProviderLaunchReceipt) error {
					receipts = append(receipts, r)
					if r.State == "runtime_"+stage {
						cancel()
					}
					return nil
				},
			}
			err := runProviderLaunch(ctx, root, deferredLaunchIntent(), hooks, launchTestRuntime())
			if !errors.Is(err, context.Canceled) || cleaned != 1 || receipts[len(receipts)-1].State != "runtime_failed" {
				t.Fatalf("err=%v cleanup=%d receipts=%+v", err, cleaned, receipts)
			}
			assertRuntimeReceipts(t, receipts)
		})
	}
}

type deferredReadyWriter struct {
	once  sync.Once
	ready func()
}

func (w *deferredReadyWriter) Write(p []byte) (int, error) { w.once.Do(w.ready); return len(p), nil }

func TestDeferredProviderLaunchFailureChannel(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(fmt.Sprint("closed=", closed), func(t *testing.T) {
			root, _ := providerLaunchFixture(t, "codex", `trap 'printf stopped >> "$LAUNCH_LOG"; exit 0' TERM
printf ready
while [ ! -f "$LAUNCH_LOG.release" ]; do sleep 0.01; done
printf normal >> "$LAUNCH_LOG"`)
			failure := make(chan error, 1)
			cleaned := 0
			var receipts []ProviderLaunchReceipt
			hooks := ProviderLaunchHooks{
				PrepareDeferred: func(ctx context.Context, r ProviderLaunchRequest) (DeferredProviderLaunch, error) {
					d := deferredLaunchFixture(ctx, r, func() error { cleaned++; return nil })
					d.Failure = failure
					return d, nil
				},
				Record: func(_ context.Context, r ProviderLaunchReceipt) error { receipts = append(receipts, r); return nil },
			}
			runtime := launchTestRuntime()
			var output bytes.Buffer
			runtime.stderr = &output
			runtime.stdout = &deferredReadyWriter{ready: func() {
				if closed {
					close(failure)
					_ = os.WriteFile(filepath.Join(root, "launch.log.release"), nil, 0600)
				} else {
					failure <- errors.New("PRIVATE handoff protocol error")
				}
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := runProviderLaunch(ctx, root, deferredLaunchIntent(), hooks, runtime)
			if closed && err != nil || !closed && err == nil || cleaned != 1 || strings.Contains(output.String(), "PRIVATE") {
				t.Fatalf("err=%v cleanup=%d output=%s", err, cleaned, output.String())
			}
			raw, _ := os.ReadFile(filepath.Join(root, "launch.log"))
			want := "stopped"
			if closed {
				want = "normal"
			}
			if string(raw) != want {
				t.Fatalf("child lifecycle=%q", raw)
			}
			assertRuntimeReceipts(t, receipts)
		})
	}
}

func TestDeferredProviderRestartPreservesChildOnPreparationFailure(t *testing.T) {
	for _, stage := range []string{"prepare", "receipt", "last validation"} {
		t.Run(stage, func(t *testing.T) {
			root, _ := providerLaunchFixture(t, "codex", `trap 'printf killed >> "$LAUNCH_LOG"; exit 1' TERM
printf ready
while [ ! -f "$LAUNCH_LOG.release" ]; do sleep 0.01; done
sleep 0.05
printf continued >> "$LAUNCH_LOG"`)
			calls := 0
			cleaned := [2]int{}
			var receipts []ProviderLaunchReceipt
			fail := func() error {
				if err := os.WriteFile(filepath.Join(root, "launch.log.release"), nil, 0600); err != nil {
					return err
				}
				return errors.New("PRIVATE runtime startup or validation failure")
			}
			hooks := ProviderLaunchHooks{
				PrepareDeferred: func(ctx context.Context, req ProviderLaunchRequest) (DeferredProviderLaunch, error) {
					calls++
					if calls > 2 {
						t.Fatal("retried the same failed boundary")
					}
					index := calls - 1
					d := deferredLaunchFixture(ctx, req, func() error { cleaned[index]++; return nil })
					if index == 0 {
						return d, nil
					}
					if req.Transition == nil || !req.Intent.Pull || req.Intent.ContextBudget != 200000 || !reflect.DeepEqual(req.Intent.ProviderArgs, []string{"--model", "fixture"}) {
						t.Fatal("restart lost launch policy")
					}
					if stage == "prepare" {
						return d, fail()
					}
					validations := 0
					d.Validate = func(context.Context) error {
						validations++
						if stage == "last validation" && validations == 2 {
							return fail()
						}
						return nil
					}
					return d, nil
				},
				Record: func(_ context.Context, r ProviderLaunchReceipt) error {
					receipts = append(receipts, r)
					if calls == 2 && r.State == "runtime_prepared" && stage == "receipt" {
						return fail()
					}
					return nil
				},
			}
			runtime := launchTestRuntime()
			runtime.stdout = &providerLaunchBoundaryWriter{root: root}
			var output bytes.Buffer
			runtime.stderr = &output
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := runProviderLaunch(ctx, root, deferredLaunchIntent(), hooks, runtime); err != nil {
				t.Fatal(err)
			}
			raw, _ := os.ReadFile(filepath.Join(root, "launch.log"))
			if string(raw) != "continued" || calls != 2 || cleaned != [2]int{1, 1} || strings.Contains(output.String(), "PRIVATE") || !strings.Contains(output.String(), "current session was preserved") {
				t.Fatalf("child=%q calls=%d cleaned=%v output=%s", raw, calls, cleaned, output.String())
			}
			assertRuntimeReceipts(t, receipts)
		})
	}
}

func TestDeferredProviderRestartPreparesBeforeStoppingAndRemovesInitialPrompt(t *testing.T) {
	root, _ := providerLaunchFixture(t, "codex", `printf '%s\n' "$@" >> "$LAUNCH_LOG"
if [ "$2" = "`+launchSessionID+`" ]; then
  trap 'printf stopped > "$LAUNCH_LOG.stopped"; exit 0' TERM
  printf ready
  while :; do sleep 0.01; done
fi`)
	var requests []ProviderLaunchRequest
	var receipts []ProviderLaunchReceipt
	cleaned := [2]int{}
	validatedBeforeStop := false
	hooks := ProviderLaunchHooks{
		PrepareDeferred: func(ctx context.Context, req ProviderLaunchRequest) (DeferredProviderLaunch, error) {
			requests = append(requests, req)
			index := len(requests) - 1
			if index > 1 {
				t.Fatal("unexpected extra runtime")
			}
			d := deferredLaunchFixture(ctx, req, func() error { cleaned[index]++; return nil })
			if index == 1 {
				d.Args[1], d.SessionID = restartedSessionID, restartedSessionID
				validations := 0
				d.Validate = func(context.Context) error {
					validations++
					if validations == 2 {
						_, err := os.Stat(filepath.Join(root, "launch.log.stopped"))
						validatedBeforeStop = os.IsNotExist(err)
						if !validatedBeforeStop {
							return domain.ErrSelectionChanged
						}
					}
					return nil
				}
			}
			return d, nil
		},
		Record: func(_ context.Context, r ProviderLaunchReceipt) error { receipts = append(receipts, r); return nil },
	}
	runtime := launchTestRuntime()
	runtime.stdout = &providerLaunchBoundaryWriter{root: root}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runProviderLaunch(ctx, root, deferredLaunchIntent(), hooks, runtime); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[1].Transition == nil || requests[1].Transition.Branch != "feature" || !requests[1].Intent.Pull || requests[1].Intent.ContextBudget != 200000 ||
		!reflect.DeepEqual(requests[1].Intent.ProviderArgs, []string{"--model", "fixture"}) || !validatedBeforeStop || cleaned != [2]int{1, 1} {
		t.Fatalf("requests=%+v validated=%v cleaned=%v", requests, validatedBeforeStop, cleaned)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "launch.log"))
	if strings.Count(string(raw), "initial private task") != 1 || strings.Count(string(raw), "--owned-runtime") != 2 || !strings.Contains(string(raw), restartedSessionID) {
		t.Fatalf("restart invocation=%q", raw)
	}
	if len(receipts) != 4 || receipts[2].SessionID != restartedSessionID || receipts[3].State != "runtime_launched" {
		t.Fatalf("receipts=%+v", receipts)
	}
	assertRuntimeReceipts(t, receipts)
}

func TestDeferredProviderCleanupFailurePreventsReplacement(t *testing.T) {
	for _, stage := range []string{"retire", "prepare replacement", "validate replacement"} {
		t.Run(stage, func(t *testing.T) {
			root, _ := providerLaunchFixture(t, "codex", `printf '%s\n' "$2" >> "$LAUNCH_LOG"
trap 'exit 0' TERM
printf ready
while :; do sleep 0.01; done`)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			firstErr, nextErr := errors.New("PRIVATE original cleanup"), errors.New("PRIVATE replacement cleanup")
			var receipts []ProviderLaunchReceipt
			cleaned, activated := [2]int{}, [2]int{}
			calls := 0
			hooks := ProviderLaunchHooks{
				PrepareDeferred: func(ctx context.Context, req ProviderLaunchRequest) (DeferredProviderLaunch, error) {
					index := calls
					calls++
					if index > 1 {
						t.Fatal("retried after failed cleanup")
					}
					d := deferredLaunchFixture(ctx, req, func() error {
						cleaned[index]++
						if index == 0 {
							return firstErr
						}
						return nextErr
					})
					d.Activate = func(context.Context) error { activated[index]++; return nil }
					if index == 1 {
						d.Args[1], d.SessionID = restartedSessionID, restartedSessionID
						if stage == "prepare replacement" {
							return d, errors.New("PRIVATE prepare failure")
						}
						validations := 0
						d.Validate = func(context.Context) error {
							validations++
							if stage == "validate replacement" && validations == 2 {
								return errors.New("PRIVATE validation failure")
							}
							return nil
						}
					}
					return d, nil
				},
				Record: func(_ context.Context, r ProviderLaunchReceipt) error { receipts = append(receipts, r); return nil },
			}
			runtime := launchTestRuntime()
			runtime.stdout = &providerLaunchBoundaryWriter{root: root}
			var output bytes.Buffer
			runtime.stderr = &output
			err := runProviderLaunch(ctx, root, deferredLaunchIntent(), hooks, runtime)
			if !errors.Is(err, firstErr) || !errors.Is(err, nextErr) || !errors.Is(err, errProviderCleanup) || ctx.Err() != nil {
				t.Fatalf("cleanup failure missing or retirement hung: %v", err)
			}
			if strings.Contains(fmt.Sprintf("%v %+v %#v %s", err, err, err, output.String()), "PRIVATE") {
				t.Fatal("cleanup failure exposed private data")
			}
			if calls != 2 || cleaned != [2]int{1, 1} || activated != [2]int{1, 0} {
				t.Fatalf("calls=%d cleaned=%v activated=%v", calls, cleaned, activated)
			}
			raw, _ := os.ReadFile(filepath.Join(root, "launch.log"))
			if string(raw) != launchSessionID+"\n" {
				t.Fatal("replacement TUI started after failed retirement")
			}
			assertRuntimeReceipts(t, receipts)
			for _, r := range receipts {
				if r.SessionID == restartedSessionID && r.State == "runtime_launched" {
					t.Fatal("unused replacement was claimed launched")
				}
			}
		})
	}
}

func TestDeferredPreparationJoinsCleanupFailureBeforeReturning(t *testing.T) {
	root, bin := providerLaunchFixture(t, "codex", "")
	primaryErr := errors.New("PRIVATE preparation failure")
	cleanupErr := errors.New("PRIVATE cleanup failure")
	recordErr := errors.New("PRIVATE receipt failure")
	cleaned, recorded := 0, 0
	var failed ProviderLaunchReceipt
	hooks := ProviderLaunchHooks{
		PrepareDeferred: func(ctx context.Context, req ProviderLaunchRequest) (DeferredProviderLaunch, error) {
			d := deferredLaunchFixture(ctx, req, func() error { cleaned++; return cleanupErr })
			return d, primaryErr
		},
		Record: func(_ context.Context, receipt ProviderLaunchReceipt) error {
			recorded++
			failed = receipt
			if cleaned != 1 {
				t.Error("failed receipt preceded cleanup")
			}
			return recordErr
		},
	}
	req := ProviderLaunchRequest{Cwd: root, Executable: bin, Intent: deferredLaunchIntent()}
	prepared, _, err := prepareDeferredProviderLaunch(context.Background(), req, hooks)
	for _, cause := range []error{primaryErr, cleanupErr, recordErr, errProviderCleanup, domain.ErrDeliveryFailed} {
		if !errors.Is(err, cause) {
			t.Errorf("lost failure %v: %v", cause, err)
		}
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v %s", err, err, err, failed.Failure), "PRIVATE") {
		t.Fatal("preparation or cleanup failure leaked private data")
	}
	if failed.State != "runtime_failed" || recorded != 1 || cleaned != 1 {
		t.Fatalf("receipt=%s recorded=%d cleaned=%d", failed.State, recorded, cleaned)
	}
	// A later owner can observe the same cleanup failure without running it again.
	if cached := cleanupProviderLaunch(prepared); !errors.Is(cached, cleanupErr) || !errors.Is(cached, errProviderCleanup) || cleaned != 1 {
		t.Fatalf("cleanup result lost or repeated: err=%v cleaned=%d", cached, cleaned)
	}
}

func TestDeferredProviderCleanupFailurePropagatesOnEveryExit(t *testing.T) {
	for _, stage := range []string{"success", "prepare", "last validation", "receipt", "cancel"} {
		t.Run(stage, func(t *testing.T) {
			root, _ := providerLaunchFixture(t, "codex", "")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cleanupErr, primaryErr := errors.New("PRIVATE cleanup"), errors.New("PRIVATE primary")
			cleaned := 0
			var receipts []ProviderLaunchReceipt
			hooks := ProviderLaunchHooks{
				PrepareDeferred: func(ctx context.Context, req ProviderLaunchRequest) (DeferredProviderLaunch, error) {
					d := deferredLaunchFixture(ctx, req, func() error { cleaned++; return cleanupErr })
					if stage == "prepare" {
						return d, primaryErr
					}
					validations := 0
					d.Validate = func(context.Context) error {
						validations++
						if stage == "last validation" && validations == 2 {
							return primaryErr
						}
						return nil
					}
					return d, nil
				},
				Record: func(_ context.Context, r ProviderLaunchReceipt) error {
					receipts = append(receipts, r)
					if r.State == "runtime_prepared" {
						if stage == "receipt" {
							return primaryErr
						}
						if stage == "cancel" {
							cancel()
						}
					}
					return nil
				},
			}
			err := runProviderLaunch(ctx, root, deferredLaunchIntent(), hooks, launchTestRuntime())
			if !errors.Is(err, cleanupErr) || cleaned != 1 || strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), "PRIVATE") {
				t.Fatalf("cleanup error lost or leaked, count=%d err=%v", cleaned, err)
			}
			if stage == "cancel" && !errors.Is(err, context.Canceled) || stage != "cancel" && stage != "success" && !errors.Is(err, primaryErr) {
				t.Fatal("primary failure lost")
			}
			if receipts[len(receipts)-1].State != "runtime_failed" {
				t.Fatal("failed cleanup claimed success")
			}
			assertRuntimeReceipts(t, receipts)
		})
	}
}

func TestDeferredProviderLaunchRejectsPackageClaimsAndInvocationMutation(t *testing.T) {
	root, bin := providerLaunchFixture(t, "codex", "")
	req := ProviderLaunchRequest{Cwd: root, Executable: bin, Intent: deferredLaunchIntent()}
	hooks := ProviderLaunchHooks{
		PrepareDeferred: func(ctx context.Context, r ProviderLaunchRequest) (DeferredProviderLaunch, error) {
			return deferredLaunchFixture(ctx, r, func() error { return nil }), nil
		},
		Record: func(context.Context, ProviderLaunchReceipt) error { return nil },
	}
	p, _, err := prepareProviderLaunch(context.Background(), req, hooks)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Cleanup()
	for name, mutate := range map[string]func(*PreparedProviderLaunch){
		"package":     func(p *PreparedProviderLaunch) { p.PackageHash = domain.HashContent([]byte("fake")) },
		"budget":      func(p *PreparedProviderLaunch) { p.Budget = &domain.AgentContextBudget{} },
		"tokens":      func(p *PreparedProviderLaunch) { p.SelectedTokens = 1 },
		"capability":  func(p *PreparedProviderLaunch) { p.Capability = "verified_for_preparation" },
		"code":        func(p *PreparedProviderLaunch) { p.CodeCommit = strings.Repeat("a", 40) },
		"source":      func(p *PreparedProviderLaunch) { p.SourceRevision = "fake" },
		"outer args":  func(p *PreparedProviderLaunch) { p.Args = []string{"fake"} },
		"owned args":  func(p *PreparedProviderLaunch) { p.Deferred.Args[0] = "fake" },
		"environment": func(p *PreparedProviderLaunch) { p.Deferred.Env = append(p.Deferred.Env, "FAKE=1") },
		"session":     func(p *PreparedProviderLaunch) { p.Deferred.SessionID = restartedSessionID },
		"not owned":   func(p *PreparedProviderLaunch) { p.Deferred.owned = nil },
	} {
		t.Run(name, func(t *testing.T) {
			copy := p
			d := *p.Deferred
			d.Args = append([]string(nil), d.Args...)
			d.Env = append([]string(nil), d.Env...)
			copy.Deferred = &d
			mutate(&copy)
			if err := validatePreparedProviderLaunch(req, copy); err == nil {
				t.Fatal("invalid deferred proof accepted")
			}
		})
	}
	for _, intent := range []LaunchIntent{{Provider: domain.ProviderClaude, Pull: true}, {Provider: domain.ProviderCodex}, {Provider: domain.ProviderCodex, Pull: true, ProviderArgs: []string{"resume", launchSessionID}}} {
		other := req
		other.Intent = intent
		if validatePreparedProviderLaunch(other, p) == nil {
			t.Fatal("deferred runtime accepted outside fresh Codex history")
		}
	}
}

func TestDeferredProviderErrorDistinguishesUnavailableBindingWithoutLeaking(t *testing.T) {
	unknown := fmt.Errorf("PRIVATE catalog: %w", domain.ErrProviderCapabilityUnknown)
	err := redactDeferredProviderError(errors.Join(redactDeferredProviderError(unknown), errors.New("PRIVATE receipt failure")))
	if !errors.Is(err, domain.ErrProviderCapabilityUnknown) || !strings.Contains(err.Error(), "cxt load --context-budget") || strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), "PRIVATE") {
		t.Fatalf("unsafe or unhelpful error: %v", err)
	}
}

func TestDeferredProviderActivationWaitsForLaunchedReceipt(t *testing.T) {
	for _, stage := range []string{"success", "receipt failure", "activation failure", "receipt cancellation"} {
		t.Run(stage, func(t *testing.T) {
			root, _ := providerLaunchFixture(t, "codex", `trap 'printf stopped > "$LAUNCH_LOG"; exit 0' TERM
printf question-ready
while [ ! -f "$LAUNCH_LOG.activate" ]; do sleep 0.01; done
printf model-started > "$LAUNCH_LOG"`)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ready := make(chan struct{})
			runtime := launchTestRuntime()
			runtime.stdout = &deferredReadyWriter{ready: func() { close(ready) }}
			var receipts []ProviderLaunchReceipt
			var events []string
			activated, cleaned := 0, 0
			persisted := false
			hooks := ProviderLaunchHooks{
				PrepareDeferred: func(ctx context.Context, req ProviderLaunchRequest) (DeferredProviderLaunch, error) {
					d := deferredLaunchFixture(ctx, req, func() error {
						cleaned++
						events = append(events, "cleanup")
						return nil
					})
					d.Activate = func(ctx context.Context) error {
						activated++
						events = append(events, "activate")
						if !persisted || activated != 1 {
							t.Fatal("activation preceded persistence or ran more than once")
						}
						if stage == "activation failure" {
							return errors.New("PRIVATE activation failure")
						}
						if err := ctx.Err(); err != nil {
							return err
						}
						return os.WriteFile(filepath.Join(root, "launch.log.activate"), nil, 0600)
					}
					return d, nil
				},
				Record: func(ctx context.Context, r ProviderLaunchReceipt) error {
					receipts = append(receipts, r)
					events = append(events, r.State)
					if r.State != "runtime_launched" {
						return nil
					}
					// The child has already submitted its synthetic argv question
					// while this receipt write is still outstanding.
					select {
					case <-ready:
					case <-ctx.Done():
						return ctx.Err()
					}
					if activated != 0 {
						t.Fatal("child question activated before lifecycle persistence")
					}
					if stage == "receipt failure" {
						return errors.New("PRIVATE lifecycle receipt failure")
					}
					persisted = true
					if stage == "receipt cancellation" {
						cancel()
					}
					return nil
				},
			}
			err := runProviderLaunch(ctx, root, deferredLaunchIntent(), hooks, runtime)
			wantEvents := []string{"runtime_prepared", "runtime_launched"}
			if stage == "success" || stage == "activation failure" {
				wantEvents = append(wantEvents, "activate")
			}
			wantEvents = append(wantEvents, "cleanup")
			if stage != "success" {
				wantEvents = append(wantEvents, "runtime_failed")
			}
			if (err == nil) != (stage == "success") || cleaned != 1 || !reflect.DeepEqual(events, wantEvents) {
				t.Fatalf("err=%v cleanup=%d events=%v want=%v", err, cleaned, events, wantEvents)
			}
			if err != nil && strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("activation or receipt failure leaked input")
			}
			raw, readErr := os.ReadFile(filepath.Join(root, "launch.log"))
			want := "stopped"
			if stage == "success" {
				want = "model-started"
			}
			if readErr != nil || string(raw) != want {
				t.Fatalf("model released despite failed activation: %q / %v", raw, readErr)
			}
			assertRuntimeReceipts(t, receipts)
		})
	}
}
