package domain

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"
)

func legacyNativeEventLifecycleRejectedStep(test *testing.T, fixture *legacyNativeCanonicalOwnerFixture, ctx context.Context, expected error) {
	test.Helper()
	reads := len(fixture.buffer.reads)
	frame := fixture.frame
	input := &legacyNormalizationInput{ctx: ctx, source: fixture.source}
	event, complete, err := fixture.native.step(ctx, input, &fixture.frame)
	if !errors.Is(err, expected) || complete || !reflect.DeepEqual(event, CIREvent{}) {
		test.Fatal("rejected native event step exposed a value or lost error precedence", complete, err, expected)
	}
	if input.failure != nil || input.loads != 0 || input.workBytes != 0 || input.semanticBytes != 0 || input.fields != 0 ||
		input.values != 0 || input.unbounded || input.pages[0] != nil || input.pages[1] != nil ||
		len(fixture.buffer.reads) != reads || fixture.frame != frame {
		test.Fatal("rejected native event step consumed source, budget, or scanner state")
	}
}

func TestLegacyNativeEventLifecycleCanceledAdmission(test *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, expire := context.WithDeadline(context.Background(), time.Time{})
	defer expire()
	for _, scenario := range []struct {
		name     string
		ctx      context.Context
		expected error
	}{
		{name: "before-admission", ctx: canceled, expected: context.Canceled},
		{name: "after-admission", ctx: &legacyNativeCanonicalOwnerContext{Context: context.Background(), limit: 1}, expected: context.Canceled},
		{name: "deadline", ctx: expired, expected: context.DeadlineExceeded},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := legacyNativeCanonicalOwnerStart(test, `{"kind":"turn","seq":1}`)
			expected := fixture.drain(test)
			before := legacyNormalizationCaptureRQ87Observe(test, fixture.native.object, &fixture.frame, fixture.source.Bytes)
			retained, err := fixture.native.retained()
			if err != nil {
				test.Fatal(err)
			}
			legacyNativeEventLifecycleRejectedStep(test, fixture, scenario.ctx, scenario.expected)
			if fixture.native.canonicalReady {
				test.Fatal("admitted canceled step retained stale canonical readiness")
			}
			if !reflect.DeepEqual(before, legacyNormalizationCaptureRQ87Observe(test, fixture.native.object, &fixture.frame, fixture.source.Bytes)) {
				test.Fatal("canceled step changed accepted parser state")
			}
			if after, err := fixture.native.retained(); err != nil || after != retained {
				test.Fatal("canceled step changed retained accounting", after, retained, err)
			}
			legacyNativeCanonicalOwnerReject(test, fixture.native, ErrIntegrity)
			legacyNormalizationCaptureRQ87Idle(test, fixture.native.object.mutation)
			if resumed := fixture.drain(test); !reflect.DeepEqual(resumed, expected) {
				test.Fatal("canceled step could not resume the accepted event")
			}
			owner, err := fixture.native.takeCanonicalOwner(context.Background())
			if err != nil || owner == nil || fixture.native.canonicalReady {
				test.Fatal("resumed event could not transfer canonical ownership", err)
			}
			defer owner.release()
			claimed, _, err := owner.claim(context.Background())
			if err != nil || !reflect.DeepEqual(claimed, expected) {
				test.Fatal("resumed canonical owner changed the accepted event", err)
			}
		})
	}
}

func TestLegacyNativeEventLifecycleFrozenCancellation(test *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, operation := range []string{"step", "release", "reset"} {
		test.Run(operation, func(test *testing.T) {
			fixture := legacyNativeCanonicalOwnerStart(test, `{"kind":"turn","seq":1}`)
			expected := fixture.drain(test)
			scope := fixture.native.object.mutation
			capture, err := scope.beginCapture()
			if err != nil {
				test.Fatal(err)
			}
			defer func() {
				if capture != nil {
					capture.release()
				}
			}()
			shared, err := scope.beginCapture()
			if err != nil {
				test.Fatal(err)
			}
			defer func() {
				if shared != nil {
					shared.release()
				}
			}()
			if shared != capture || capture.references.Load() != 2 {
				test.Fatal("shared capture did not retain the frozen native owner")
			}
			before := legacyNormalizationCaptureRQ87Observe(test, fixture.native.object, &fixture.frame, fixture.source.Bytes)
			unchanged := func() {
				test.Helper()
				if !fixture.native.canonicalReady || !capture.valid(scope) ||
					!reflect.DeepEqual(before, legacyNormalizationCaptureRQ87Observe(test, fixture.native.object, &fixture.frame, fixture.source.Bytes)) {
					test.Fatal("rejected mutation changed the frozen native owner")
				}
			}
			legacyNativeEventLifecycleRejectedStep(test, fixture, canceled, context.Canceled)
			unchanged()
			legacyNativeEventLifecycleRejectedStep(test, fixture, context.Background(), ErrLegacyNormalizationBusy)
			unchanged()
			legacyNormalizationCaptureRQ87Busy(test, fixture.native.releaseValues())
			unchanged()
			legacyNormalizationCaptureRQ87Busy(test, fixture.native.reset(0, fixture.source.Bytes))
			unchanged()
			owner, err := fixture.native.takeCanonicalOwner(canceled)
			if owner != nil || !errors.Is(err, context.Canceled) {
				test.Fatal("frozen owner transfer lost cancellation precedence", err)
			}
			unchanged()
			legacyNativeCanonicalOwnerReject(test, fixture.native, ErrLegacyNormalizationBusy)
			unchanged()
			shared.release()
			shared = nil
			if capture.references.Load() != 1 {
				test.Fatal("rejected mutation changed capture references")
			}
			legacyNativeEventLifecycleRejectedStep(test, fixture, canceled, context.Canceled)
			unchanged()
			capture.release()
			capture = nil
			legacyNormalizationCaptureRQ87Idle(test, scope)
			switch operation {
			case "step":
				legacyNativeEventLifecycleRejectedStep(test, fixture, canceled, context.Canceled)
			case "release":
				if err := fixture.native.releaseValues(); err != nil {
					test.Fatal("released capture still blocked native release", err)
				}
			case "reset":
				if err := fixture.native.reset(0, fixture.source.Bytes); err != nil {
					test.Fatal("released capture still blocked native reset", err)
				}
			}
			if fixture.native.canonicalReady {
				test.Fatal("admitted lifecycle operation retained canonical readiness")
			}
			if operation != "step" && !reflect.DeepEqual(fixture.native.decoded, wire{}) {
				test.Fatal("admitted release or reset retained decoded values")
			}
			legacyNativeCanonicalOwnerReject(test, fixture.native, ErrIntegrity)
			legacyNormalizationCaptureRQ87Idle(test, scope)
			if err := fixture.native.reset(0, fixture.source.Bytes); err != nil {
				test.Fatal("native event could not restart after capture release", err)
			}
			if resumed := fixture.drain(test); !reflect.DeepEqual(resumed, expected) {
				test.Fatal("capture release changed the resumed event")
			}
		})
	}
}

func TestLegacyNativeEventLifecycleCanceledGatePrecedence(test *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, scenario := range []struct {
		name     string
		block    func(*legacyNormalizationNativeEvent)
		expected error
	}{
		{name: "closed", block: func(native *legacyNormalizationNativeEvent) { native.object.mutation.stop() }, expected: ErrConflict},
		{name: "missing-scope", block: func(native *legacyNormalizationNativeEvent) { native.object.mutation = nil }, expected: ErrIntegrity},
		{name: "activity-overflow", block: func(native *legacyNormalizationNativeEvent) { native.object.mutation.activity.Store(math.MaxInt64) }, expected: ErrIntegrity},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := legacyNativeCanonicalOwnerStart(test, `{"kind":"turn","seq":1}`)
			fixture.drain(test)
			scenario.block(fixture.native)
			before := legacyNormalizationCaptureRQ87Observe(test, fixture.native.object, &fixture.frame, fixture.source.Bytes)
			legacyNativeEventLifecycleRejectedStep(test, fixture, canceled, context.Canceled)
			if !fixture.native.canonicalReady {
				test.Fatal("canceled step mutated native readiness without admission")
			}
			legacyNativeEventLifecycleRejectedStep(test, fixture, context.Background(), scenario.expected)
			if !fixture.native.canonicalReady || !reflect.DeepEqual(before, legacyNormalizationCaptureRQ87Observe(test, fixture.native.object, &fixture.frame, fixture.source.Bytes)) {
				test.Fatal("failed admission changed native ownership")
			}
		})
	}
	for _, native := range []*legacyNormalizationNativeEvent{nil, {canonicalReady: true}} {
		event, complete, err := native.step(canceled, nil, nil)
		if !errors.Is(err, context.Canceled) || complete || !reflect.DeepEqual(event, CIREvent{}) || native != nil && !native.canonicalReady {
			test.Fatal("canceled step inspected or changed missing native state", complete, err)
		}
	}
}
