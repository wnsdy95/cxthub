package domain

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestLegacyNormalizationPreparationInputConsumed(test *testing.T) {
	raw := []byte(`{"envelope":{"cir_version":"2"},"events":[{"kind":"message","seq":2,"role":"user","blocks":[{"type":"text","text":"retained"}]},{"kind":"turn","seq":1,"role":"assistant"}]}`)
	want, _, err := legacyNormalizationPreparationOracle(raw)
	if err != nil {
		test.Fatal(err)
	}
	work, storage := legacyNormalizationPreparationStart(test, raw, true)
	defer work.Close()
	consumed := false
	for attempt := 0; attempt < 1024; attempt++ {
		got, err := work.InputConsumed()
		if err != nil || consumed && !got || got != (work.decoder == nil) {
			test.Fatal("input consumption changed before the actual decoder handoff", got, consumed, err)
		}
		consumed = got
		if consumed && work.phase != legacyNormalizationReady {
			if _, err := work.Source(); !errors.Is(err, ErrDocRangeIndexPending) {
				test.Fatal("input consumption minted a completed source", err)
			}
		}
		ready, err := work.Step(context.Background())
		if err != nil {
			test.Fatal(err)
		}
		if ready {
			got, err := work.InputConsumed()
			if !consumed || !got || err != nil || !bytes.Equal(storage.slots[3], want) {
				test.Fatal("input release changed the exact canonical result", got, err)
			}
			return
		}
	}
	test.Fatal("finite input-consumption path did not reach ready")
}

func TestLegacyNormalizationPreparationInputConsumedFences(test *testing.T) {
	for _, name := range []string{"capture", "closed", "terminal", "invalid-phase", "missing-decoder", "unfinished-owner"} {
		test.Run(name, func(test *testing.T) {
			work, _ := legacyNormalizationPreparationStart(test, []byte(`null`), false)
			defer work.Close()
			want := ErrIntegrity
			switch name {
			case "capture":
				capture, err := work.mutation.beginCapture()
				if err != nil {
					test.Fatal(err)
				}
				defer capture.release()
				want = ErrLegacyNormalizationBusy
			case "closed":
				work.Close()
				want = ErrConflict
			case "terminal":
				work.terminal = context.Canceled
				want = context.Canceled
			case "invalid-phase":
				work.phase = legacyNormalizationReady + 1
			case "missing-decoder":
				work.decoder = nil
			case "unfinished-owner":
				work.phase = legacyNormalizationRuns
			}
			if consumed, err := work.InputConsumed(); consumed || !errors.Is(err, want) {
				test.Fatal("invalid input-consumption boundary was accepted", consumed, err)
			}
		})
	}
}
