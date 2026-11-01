package domain

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestCanonicalVerificationFailureRechecksCancellationAfterInvalidFrame(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	checks := 0
	ctx := canonicalAdmissionContext{Context: base, check: func() {
		checks++
		if checks == 2 {
			cancel()
		}
	}}
	err := canonicalVerificationFailure(ctx, []byte(`{"envelope":{},"events":[invalid]}`), ErrUnsupportedCIRVersion)
	if !errors.Is(err, context.Canceled) || checks != 2 {
		t.Fatal("failed compatibility framing swallowed cancellation", err, checks)
	}
}

func TestCanonicalStringFragmentsCheckCancellationBeforeScanning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	invalid := []byte(strings.Repeat(string([]byte{0x80}), canonicalCheckBytes+1))
	if err := verifyCanonicalStringFragments(ctx, invalid); !errors.Is(err, context.Canceled) {
		t.Fatal("fragment scan performed integrity work before entry cancellation", err)
	}
}

func TestCanonicalVerificationEnvelopeDepthErrorParity(t *testing.T) {
	for _, depth := range []int{9998, 9999, 10000} {
		t.Run(strconv.Itoa(depth), func(t *testing.T) {
			raw := []byte(`{"envelope":{"cir_version":"99","unknown":` + strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth) + `},"events":[]}`)
			_, want := archivedColdCanonicalVerify(context.Background(), HashContent(raw), raw)
			var verifier CanonicalDocVerifier
			proof, err := verifier.Verify(context.Background(), HashContent(raw), raw)
			if want == nil || err == nil || proof.Valid() ||
				errors.Is(err, ErrUnsupportedCIRVersion) != errors.Is(want, ErrUnsupportedCIRVersion) {
				t.Fatalf("envelope depth error changed: got %v, archived %v", err, want)
			}
		})
	}
}
