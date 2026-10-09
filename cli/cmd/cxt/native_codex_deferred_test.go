package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	delivcli "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/cli"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/nativecodex"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type nativeLifecycleFixture struct {
	generation func(context.Context) (nativecodex.GenerationObservation, error)
	lifecycle  func(context.Context) error
}

func (f nativeLifecycleFixture) WaitGeneration(ctx context.Context) (nativecodex.GenerationObservation, error) {
	return f.generation(ctx)
}
func (f nativeLifecycleFixture) WaitLifecycle(ctx context.Context) error { return f.lifecycle(ctx) }

func TestNativeDeferredMonitorContinuesAfterFirstTurn(t *testing.T) {
	for _, calibrationFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(calibrationFailure), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			waiting, stopped := make(chan struct{}), make(chan struct{})
			warned := make(chan struct{}, 1)
			secret := errors.New("PRIVATE transport or native path")
			f := nativeLifecycleFixture{
				generation: func(context.Context) (nativecodex.GenerationObservation, error) {
					if calibrationFailure {
						return nativecodex.GenerationObservation{}, nativecodex.ErrCalibrationPersistence
					}
					return nativecodex.GenerationObservation{Outcome: "completed"}, nil
				},
				lifecycle: func(ctx context.Context) error {
					close(waiting)
					select {
					case <-stopped:
						return secret
					case <-ctx.Done():
						return ctx.Err()
					}
				},
			}
			failures := monitorNativeCodexLifecycle(ctx, f, func() { warned <- struct{}{} })
			select {
			case <-waiting:
			case <-ctx.Done():
				t.Fatal("monitor did not continue after first turn")
			}
			select {
			case <-failures:
				t.Fatal("first success or feedback failure ended lifecycle monitoring")
			default:
			}
			if (len(warned) == 1) != calibrationFailure {
				t.Fatal("feedback warning missing or spurious")
			}
			close(stopped)
			select {
			case err := <-failures:
				if !errors.Is(err, secret) || strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), "PRIVATE") {
					t.Fatal("terminal failure missing or not redacted")
				}
			case <-ctx.Done():
				t.Fatal("later disconnect was not reported")
			}
		})
	}
}

func TestNativeDeferredMonitorClosureAndOwnerCancellation(t *testing.T) {
	for _, ownerCancel := range []bool{false, true} {
		t.Run(fmt.Sprint(ownerCancel), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			waiting := make(chan struct{})
			f := nativeLifecycleFixture{
				generation: func(ctx context.Context) (nativecodex.GenerationObservation, error) {
					close(waiting)
					if ownerCancel {
						<-ctx.Done()
					}
					return nativecodex.GenerationObservation{}, nativecodex.ErrClosed
				},
				lifecycle: func(context.Context) error { panic("generation already failed") },
			}
			failures := monitorNativeCodexLifecycle(ctx, f, func() { panic("unexpected warning") })
			<-waiting
			if ownerCancel {
				cancel()
			}
			select {
			case err, open := <-failures:
				if ownerCancel && (open || err != nil) || !ownerCancel && (!open || !errors.Is(err, nativecodex.ErrClosed)) {
					t.Fatalf("owner cancellation=%v error=%v open=%v", ownerCancel, err, open)
				}
			case <-time.After(time.Second):
				t.Fatal("monitor did not finish")
			}
		})
	}
}

func TestNativeDeferredMonitorConfirmedClientExit(t *testing.T) {
	for _, afterTurn := range []bool{false, true} {
		t.Run(fmt.Sprint(afterTurn), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			f := nativeLifecycleFixture{
				generation: func(context.Context) (nativecodex.GenerationObservation, error) {
					if !afterTurn {
						return nativecodex.GenerationObservation{}, nativecodex.ErrClientExit
					}
					return nativecodex.GenerationObservation{Outcome: "completed"}, nil
				},
				lifecycle: func(context.Context) error {
					if !afterTurn {
						t.Error("exit before first turn continued lifecycle wait")
					}
					return nativecodex.ErrClientExit
				},
			}
			select {
			case err, open := <-monitorNativeCodexLifecycle(ctx, f, func() { t.Error("unexpected warning") }):
				if open || err != nil {
					t.Fatal("confirmed exit reported as fatal", err)
				}
			case <-ctx.Done():
				t.Fatal("confirmed exit did not finish monitoring")
			}
		})
	}
}

func TestNativeDeferredCleanupJoinsFailuresAndRunsOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, last := errors.New("PRIVATE native process"), errors.New("PRIVATE capture registry")
	var order []string
	closer := func(name string, err error) func() error {
		return func() error {
			if ctx.Err() == nil {
				t.Fatal("cleanup did not cancel owner before closing resources")
			}
			order = append(order, name)
			return err
		}
	}
	cleanup := nativeCodexRuntimeCleanup(cancel, closer("handoff", nil), closer("session", first), closer("capture", last))
	for i := 0; i < 2; i++ {
		err := cleanup()
		if !errors.Is(err, first) || !errors.Is(err, last) || strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), "PRIVATE") {
			t.Fatal("cleanup discarded a failure or exposed private details")
		}
	}
	if !reflect.DeepEqual(order, []string{"handoff", "session", "capture"}) {
		t.Fatalf("cleanup repeated or stopped early: %v", order)
	}
}

func TestNativeDeferredPersistenceSeparatesRuntimeInjectionAndOutcome(t *testing.T) {
	f := newNativeGenerationBridgeFixture(t)
	prepared := f.prepared(t, "PRIVATE_INITIAL_QUESTION")
	root := t.TempDir()
	var records []delivcli.ProviderLaunchReceipt
	record := func(ctx context.Context, r delivcli.ProviderLaunchReceipt) error {
		records = append(records, r)
		return recordProviderLaunchReceipt(ctx, root, r)
	}
	p, err := persistNativeGeneration(context.Background(), root, f.bridge.expected, f.returned, prepared, f.bridge.expectedNativeWindow, record)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].State != "package_prepared" || records[0].Acceptance != "unknown" {
		t.Fatal("premature delivery claim")
	}
	path := filepath.Join(root, ".cxt", "input-packages", strings.TrimPrefix(string(f.returned.ID), "sha256:")+".json")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("private durable package missing", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(raw), "PRIVATE_INITIAL_QUESTION") {
		t.Fatal("initial question leaked", err)
	}
	var saved domain.AgentContextPackage
	if json.Unmarshal(raw, &saved) != nil || saved.ID != f.returned.ID {
		t.Fatal("wrong durable package")
	}
	ack := nativecodex.InjectionReceipt{ThreadID: f.bridge.expected.ID, Items: 1, UTF8Bytes: len(prepared.History[0].Text), Acknowledged: true, ProviderAcceptance: "unverified"}
	bad := ack
	bad.ThreadID = "other"
	if err := p.BeforeRelease(context.Background(), bad); err == nil || len(records) != 1 {
		t.Fatal("wrong thread recorded")
	}
	if err := p.BeforeRelease(context.Background(), ack); err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[1].State != "injected_ready" || records[1].Acceptance != "unknown" {
		t.Fatal("injection acknowledged as acceptance")
	}
	o := f.observation("completed")
	if err := p.Observe(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 || records[2].State != "first_turn_observed" || records[2].Acceptance != "first_turn_completed" {
		t.Fatal("completion not correlated")
	}
	for _, r := range records {
		raw, _ := json.Marshal(r)
		if strings.Contains(string(raw), "PRIVATE_") || strings.Contains(string(raw), "payload_hash") {
			t.Fatal("private text escaped receipt")
		}
	}
}

func TestNativeDeferredPersistenceFailuresDoNotGrantRelease(t *testing.T) {
	for _, stage := range []string{"package", "prepared receipt", "injection receipt", "outcome receipt"} {
		t.Run(stage, func(t *testing.T) {
			f := newNativeGenerationBridgeFixture(t)
			prepared := f.prepared(t, "PRIVATE_QUESTION")
			root := t.TempDir()
			if stage == "package" {
				if err := os.WriteFile(filepath.Join(root, ".cxt"), []byte("block directory"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			record := func(_ context.Context, r delivcli.ProviderLaunchReceipt) error {
				calls++
				if stage == "prepared receipt" && r.State == "package_prepared" || stage == "injection receipt" && r.State == "injected_ready" || stage == "outcome receipt" && r.State == "first_turn_observed" {
					return errors.New("PRIVATE_STORAGE_FAILURE")
				}
				return nil
			}
			p, err := persistNativeGeneration(context.Background(), root, f.bridge.expected, f.returned, prepared, f.bridge.expectedNativeWindow, record)
			if stage == "package" || stage == "prepared receipt" {
				if err == nil || p.BeforeRelease != nil {
					t.Fatal("failed preparation granted release")
				}
				if stage == "package" && calls != 0 {
					t.Fatal("receipt without package")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if stage == "injection receipt" {
					err = p.BeforeRelease(context.Background(), nativecodex.InjectionReceipt{ThreadID: f.bridge.expected.ID, Items: 1, UTF8Bytes: len(prepared.History[0].Text), Acknowledged: true, ProviderAcceptance: "unverified"})
				} else {
					err = p.Observe(context.Background(), f.observation("completed"))
					var failure interface{ CalibrationPersistenceFailure() bool }
					if !errors.As(err, &failure) || !failure.CalibrationPersistenceFailure() {
						t.Fatal("receipt outage would terminate productive conversation")
					}
				}
				if err == nil {
					t.Fatal("failed persistence reported success")
				}
			}
			if strings.Contains(err.Error(), "PRIVATE_") {
				t.Fatal("private failure escaped")
			}
		})
	}
}

type nativeMeasuredFixture domain.AgentHostCapability

func (c nativeMeasuredFixture) AgentCapability(context.Context, domain.ProviderKind, string) (domain.AgentHostCapability, error) {
	return domain.AgentHostCapability(c), nil
}

func TestNativeDeferredRevalidatesCalibrationBeforeRelease(t *testing.T) {
	f := newNativeGenerationBridgeFixture(t)
	f.prepared(t, "question")
	if err := validateNativeMeasuredBudget(context.Background(), nativeMeasuredFixture(f.capability), f.returned); err != nil {
		t.Fatal(err)
	}
	c := f.capability
	c.Calibration.OverheadTokens = 20000
	if err := validateNativeMeasuredBudget(context.Background(), nativeMeasuredFixture(c), f.returned); err == nil {
		t.Fatal("stale observation budget accepted")
	}
}

func TestNativeDeferredPinsWorktreeWithoutReadingSharedMain(t *testing.T) {
	for _, mutation := range []string{"none", "same commit branch", "new commit"} {
		t.Run(mutation, func(t *testing.T) {
			f := newEmptyBootstrapFixture(t, false)
			check, err := pinNativeWorkingPosition(context.Background(), f.runtime, f.root)
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "same commit branch":
				selectionGit(t, f.root, "switch", "-qc", "other")
			case "new commit":
				selectionGit(t, f.root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "next")
			}
			err = check(context.Background())
			if (err == nil) != (mutation == "none") {
				t.Fatalf("local mutation=%s err=%v", mutation, err)
			}
			f.mu.Lock()
			reads := f.reads
			f.mu.Unlock()
			if reads != 0 {
				t.Fatal("idle launch pinned server main")
			}
		})
	}
}
