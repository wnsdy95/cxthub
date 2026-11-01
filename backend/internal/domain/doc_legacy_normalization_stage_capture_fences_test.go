package domain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const legacyNormalizationStageFenceBudget int64 = 16 << 20

func legacyNormalizationStageFenceStart(test *testing.T, prepared bool) (*LegacyNormalizationPreparation, *LegacyNormalizationCheckpointCapture, *legacySourceProofBuffer, LegacyNormalizationParserCheckpointIO) {
	test.Helper()
	event := `{"kind":"turn","seq":7}`
	if prepared {
		event = `{"kind":"tool_result","seq":7,"call_id":"stage-fences","output":{"payload":"` + strings.Repeat("x", 2*LegacyCanonicalEncodingBytes) + `","z-tail":{"remaining":true}}}`
	}
	work, buffer := legacyCanonicalInitialCheckpointStage(test, event+`,{"kind":"turn","seq":9}`)
	if prepared {
		legacyNormalizationPreparationAdvance(test, work, func() bool {
			return work.phase == legacyNormalizationStage && work.next == 0 && work.pending[0].Encoding.preparation == nil
		})
		if work.pending[0].Encoding.complete || work.pending[0].Encoding.frames.length() == 0 {
			test.Fatal("actual Stage fixture did not retain unfinished prepared output")
		}
	}
	capture, err := work.BeginCheckpoint(context.Background())
	if err != nil {
		test.Fatal("actual Stage capture failed", err)
	}
	test.Cleanup(capture.Close)
	store, _ := legacyNativeCheckpointStoreFixture(test)
	return work, capture, buffer, LegacyNormalizationParserCheckpointIO{Put: store.Put, Read: store.Read}
}

func legacyNormalizationStageFenceRetained(test *testing.T, work *LegacyNormalizationPreparation, capture *LegacyNormalizationCheckpointCapture) int64 {
	test.Helper()
	retained, err := work.RetainedBytes()
	extra, extraErr := capture.RetainedBytes()
	if err != nil || extraErr != nil || retained < 0 || extra < 0 {
		test.Fatal("actual Stage retention failed", retained, extra, err, extraErr)
	}
	return retained + extra
}

func legacyNormalizationStageFenceFinish(test *testing.T, capture *LegacyNormalizationCheckpointCapture, binding ContentHash, store LegacyNormalizationParserCheckpointIO, parser bool) LegacyNormalizationParserCheckpointRef {
	test.Helper()
	step := capture.StageStep
	if parser {
		step = capture.ParserStep
	}
	for range legacyDecoderCheckpointSteps {
		ref, complete, err := step(context.Background(), binding, store, legacyNormalizationStageFenceBudget)
		if err != nil {
			test.Fatal("actual Stage capture did not finish", parser, err)
		}
		if retained := legacyNormalizationStageFenceRetained(test, capture.work, capture); retained > legacyNormalizationStageFenceBudget {
			test.Fatal("capture exceeded its current aggregate budget", retained)
		}
		if _, err := capture.work.Step(context.Background()); !errors.Is(err, ErrLegacyNormalizationBusy) {
			test.Fatal("capture lost its shared preparation freeze", err)
		}
		if complete {
			body, err := store.Read(context.Background(), ref.Hash, ref.Bytes)
			var state struct {
				Binding ContentHash `json:"binding"`
			}
			if err != nil || json.Unmarshal(body, &state) != nil || state.Binding != binding {
				test.Fatal("checkpoint root lost its pinned binding", ref, state.Binding, err)
			}
			return ref
		}
	}
	test.Fatal("actual Stage capture exceeded its finite step allowance")
	return LegacyNormalizationParserCheckpointRef{}
}

func legacyNormalizationStageFenceReleased(test *testing.T, work *LegacyNormalizationPreparation, capture *LegacyNormalizationCheckpointCapture) {
	test.Helper()
	if !capture.closed.Load() || capture.parserBusy.Load() || capture.work != nil || capture.capture != nil || capture.stage != nil || capture.parser != nil ||
		work.mutation.activity.Load() != 0 || work.mutation.capture.Load() != nil {
		test.Fatal("failed or closed Stage capture retained its shared mutation grant")
	}
	if _, err := capture.Control(context.Background()); !errors.Is(err, ErrConflict) {
		test.Fatal("released Stage capture remained usable", err)
	}
}

func TestLegacyNormalizationStageCapturePinsBothCallOrders(test *testing.T) {
	for _, order := range []string{"stage-first", "parser-first", "stage-repeat"} {
		for _, boundary := range []string{"pending", "ready"} {
			for _, change := range []string{"same", "binding", "budget"} {
				test.Run(order+"/"+boundary+"/"+change, func(test *testing.T) {
					work, capture, buffer, store := legacyNormalizationStageFenceStart(test, false)
					binding := HashContent(buffer.body)
					reads := len(buffer.reads)
					first, second := capture.StageStep, capture.ParserStep
					if order == "parser-first" {
						first, second = second, first
					} else if order == "stage-repeat" {
						second = first
					}
					if boundary == "ready" {
						legacyNormalizationStageFenceFinish(test, capture, binding, store, order == "parser-first")
					} else if ref, complete, err := first(context.Background(), binding, store, legacyNormalizationStageFenceBudget); err != nil || complete || ref != (LegacyNormalizationParserCheckpointRef{}) {
						test.Fatal("actual first checkpoint step did not remain pending", ref, complete, err)
					}
					if capture.parserBinding != binding || capture.parserBudget != legacyNormalizationStageFenceBudget {
						test.Fatal("first checkpoint step did not pin the shared binding and budget")
					}
					nextBinding, budget := binding, legacyNormalizationStageFenceBudget
					if change == "binding" {
						nextBinding = HashContent([]byte("different Stage source"))
					} else if change == "budget" {
						budget--
					}
					ref, complete, err := second(context.Background(), nextBinding, store, budget)
					if change != "same" {
						if !errors.Is(err, ErrIntegrity) || complete || ref != (LegacyNormalizationParserCheckpointRef{}) {
							test.Fatal("shared capture accepted a changed binding or budget", ref, complete, err)
						}
						legacyNormalizationStageFenceReleased(test, work, capture)
						return
					}
					if err != nil {
						test.Fatal("same binding and budget were rejected", err)
					}
					root := legacyNormalizationStageFenceFinish(test, capture, binding, store, false)
					legacyNormalizationStageFenceFinish(test, capture, binding, store, true)
					again, complete, err := capture.StageStep(context.Background(), binding, store, budget)
					if err != nil || !complete || again != root || len(buffer.reads) != reads {
						test.Fatal("cached Stage root changed or replayed source input", again, complete, err)
					}
					capture.Close()
					legacyNormalizationStageFenceReleased(test, work, capture)
				})
			}
		}
	}
}

func TestLegacyNormalizationStageCaptureCurrentAggregateBudget(test *testing.T) {
	for _, boundary := range []string{"pending", "ready"} {
		for _, owner := range []string{"preparation", "capture"} {
			test.Run(boundary+"/"+owner, func(test *testing.T) {
				work, capture, buffer, store := legacyNormalizationStageFenceStart(test, false)
				binding := HashContent(buffer.body)
				writes := 0
				put := store.Put
				store.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
					writes++
					return put(ctx, hash, body)
				}
				if boundary == "ready" {
					legacyNormalizationStageFenceFinish(test, capture, binding, store, false)
				} else if _, complete, err := capture.StageStep(context.Background(), binding, store, legacyNormalizationStageFenceBudget); err != nil || complete {
					test.Fatal("actual Stage checkpoint did not start", complete, err)
				}
				if owner == "preparation" {
					previous := work.files[0].buffer
					work.files[0].buffer = make([]byte, len(previous), int(legacyNormalizationStageFenceBudget/2)+1)
					copy(work.files[0].buffer, previous)
				} else {
					capture.control.HashState = make([]byte, 0, int(legacyNormalizationStageFenceBudget/2)+1)
				}
				if retained := legacyNormalizationStageFenceRetained(test, work, capture); retained <= legacyNormalizationStageFenceBudget {
					test.Fatal("fixture did not exceed the current aggregate budget", retained)
				}
				before := writes
				ref, complete, err := capture.StageStep(context.Background(), binding, store, legacyNormalizationStageFenceBudget)
				if !errors.Is(err, ErrDocumentResourceLimit) || complete || ref != (LegacyNormalizationParserCheckpointRef{}) || writes != before {
					test.Fatal("Stage used a stale aggregate budget or cached ready root", ref, complete, err, writes-before)
				}
				legacyNormalizationStageFenceReleased(test, work, capture)
			})
		}
	}
}

func TestLegacyNormalizationStageCaptureRebudgetsExistingChildren(test *testing.T) {
	for _, kind := range []string{"initial", "prepared"} {
		test.Run(kind, func(test *testing.T) {
			work, capture, buffer, store := legacyNormalizationStageFenceStart(test, kind == "prepared")
			binding := HashContent(buffer.body)
			if _, complete, err := capture.StageStep(context.Background(), binding, store, legacyNormalizationStageFenceBudget); err != nil || complete || capture.stage == nil {
				test.Fatal("actual Stage checkpoint did not create its child", complete, err)
			}
			initial, prepared := capture.stage.initial, capture.stage.child
			var childBytes, previousBudget int64
			var err error
			if kind == "initial" {
				if initial == nil {
					test.Fatal("actual initial Stage child is missing")
				}
				childBytes, err = initial.retainedBytes()
				previousBudget = initial.budget
			} else {
				if prepared == nil {
					test.Fatal("actual prepared Stage child is missing")
				}
				childBytes, err = prepared.retainedBytes()
				previousBudget = prepared.budget
			}
			if err != nil {
				test.Fatal(err)
			}
			if _, complete, err := capture.ParserStep(context.Background(), binding, store, legacyNormalizationStageFenceBudget); err != nil || complete || capture.parser == nil {
				test.Fatal("actual sibling parser did not retain checkpoint state", complete, err)
			}
			stageBytes, err := capture.stage.retainedBytes()
			if err != nil {
				test.Fatal(err)
			}
			remaining := legacyNormalizationStageFenceBudget - legacyNormalizationStageFenceRetained(test, work, capture)
			if remaining+childBytes >= previousBudget {
				test.Fatal("sibling parser did not reduce the child's available budget")
			}
			if _, complete, err := capture.StageStep(context.Background(), binding, store, legacyNormalizationStageFenceBudget); err != nil || complete {
				test.Fatal("Stage did not continue after sibling parser allocation", complete, err)
			}
			if capture.stage.budget != remaining+stageBytes || initial != nil && initial.budget != remaining+childBytes || prepared != nil && prepared.budget != remaining+childBytes {
				test.Fatal("Stage retained constructor-only budgets or deducted its own retention twice")
			}
			legacyNormalizationStageFenceFinish(test, capture, binding, store, false)
			legacyNormalizationStageFenceFinish(test, capture, binding, store, true)
			capture.Close()
			legacyNormalizationStageFenceReleased(test, work, capture)
		})
	}
}

func TestLegacyNormalizationStageCaptureRebudgetsDeepPreparedValues(test *testing.T) {
	for _, path := range []string{"fields", "frame"} {
		test.Run(path, func(test *testing.T) {
			work, capture, buffer, store := legacyNormalizationStageFenceStart(test, true)
			binding := HashContent(buffer.body)
			var target *legacyCanonicalPreparedValueWrite
			var expected int64
			observed := false
			put := store.Put
			store.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
				if expected != 0 {
					observed = true
					if target.budget != expected {
						test.Fatal("deep writer used stale headroom before storage callback", target.budget, expected)
					}
				}
				return put(ctx, hash, body)
			}
			for range legacyDecoderCheckpointSteps {
				if capture.stage != nil && capture.stage.child != nil {
					child := capture.stage.child
					if path == "fields" && child.fields != nil {
						target = child.fields.value
					} else if path == "frame" && child.frame != nil {
						target = child.frame.value
					}
					if target != nil && target.frame != nil && target.frame.phase == legacyCanonicalPreparedWriteEmit {
						break
					}
				}
				if _, complete, err := capture.StageStep(context.Background(), binding, store, legacyNormalizationStageFenceBudget); err != nil || complete {
					test.Fatal("actual Stage did not reach the deep writer", path, complete, err)
				}
			}
			if target == nil || target.frame == nil || target.frame.phase != legacyCanonicalPreparedWriteEmit {
				test.Fatal("deep writer boundary is missing", path)
			}
			before := target.budget
			if _, complete, err := capture.ParserStep(context.Background(), binding, store, legacyNormalizationStageFenceBudget); err != nil || complete || capture.parser == nil {
				test.Fatal("sibling parser did not allocate its checkpoint state", complete, err)
			}
			retained, err := target.retainedBytes()
			if err != nil {
				test.Fatal(err)
			}
			expected = legacyNormalizationStageFenceBudget - legacyNormalizationStageFenceRetained(test, work, capture) + retained
			if expected >= before {
				test.Fatal("sibling parser did not reduce deep writer headroom", expected, before)
			}
			if _, complete, err := capture.StageStep(context.Background(), binding, store, legacyNormalizationStageFenceBudget); err != nil || complete || !observed {
				test.Fatal("deep writer did not use the refreshed budget at emission", complete, observed, err)
			}
			expected = 0
			legacyNormalizationStageFenceFinish(test, capture, binding, store, false)
			legacyNormalizationStageFenceFinish(test, capture, binding, store, true)
		})
	}
}

func TestLegacyNormalizationStageCaptureCallbackFences(test *testing.T) {
	for _, action := range []string{"cancel", "close", "owner-close", "grant", "source", "write-failure", "budget-pending", "budget-ready", "reenter"} {
		test.Run(action, func(test *testing.T) {
			work, capture, buffer, store := legacyNormalizationStageFenceStart(test, false)
			binding := HashContent(buffer.body)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			put := store.Put
			entered := false
			store.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
				if action == "budget-ready" {
					var state struct {
						Kind string `json:"kind"`
					}
					if json.Unmarshal(body, &state) != nil || state.Kind != "legacy-normalization-stage" {
						return put(ctx, hash, body)
					}
				}
				entered = true
				if _, err := work.Step(ctx); !errors.Is(err, ErrLegacyNormalizationBusy) {
					test.Fatal("Stage callback could mutate its preparation", err)
				}
				switch action {
				case "cancel", "close":
					capture.control.HashState = make([]byte, 0, int(legacyNormalizationStageFenceBudget/2)+1)
					if action == "cancel" {
						cancel()
					} else {
						capture.Close()
					}
				case "owner-close":
					work.Close()
				case "grant":
					work.mutation.capture.Store(nil)
				case "source":
					work.pending[0].Encoding.complete = true
				case "write-failure":
					return ErrNotFound
				case "budget-pending", "budget-ready":
					capture.control.HashState = make([]byte, 0, int(legacyNormalizationStageFenceBudget/2)+1)
				case "reenter":
					if _, _, err := capture.StageStep(ctx, binding, store, legacyNormalizationStageFenceBudget); !errors.Is(err, ErrLegacyNormalizationBusy) {
						test.Fatal("Stage callback reentered StageStep", err)
					}
					if _, _, err := capture.ParserStep(ctx, binding, store, legacyNormalizationStageFenceBudget); !errors.Is(err, ErrLegacyNormalizationBusy) {
						test.Fatal("Stage callback reentered ParserStep", err)
					}
				}
				return put(ctx, hash, body)
			}
			var ref LegacyNormalizationParserCheckpointRef
			var complete bool
			var failure error
			for range legacyDecoderCheckpointSteps {
				ref, complete, failure = capture.StageStep(ctx, binding, store, legacyNormalizationStageFenceBudget)
				if failure != nil || complete {
					break
				}
			}
			if !entered {
				test.Fatal("actual Stage checkpoint never reached the requested callback")
			}
			if action == "reenter" {
				if failure != nil || !complete || ref == (LegacyNormalizationParserCheckpointRef{}) {
					test.Fatal("Stage failed after denying callback reentry", ref, complete, failure)
				}
				capture.Close()
			} else {
				want := error(ErrConflict)
				switch action {
				case "cancel":
					want = context.Canceled
				case "write-failure":
					want = ErrNotFound
				case "budget-pending", "budget-ready":
					want = ErrDocumentResourceLimit
				}
				if !errors.Is(failure, want) || complete || ref != (LegacyNormalizationParserCheckpointRef{}) {
					test.Fatal("Stage callback lost its current-state or budget fence", ref, complete, failure, want)
				}
			}
			legacyNormalizationStageFenceReleased(test, work, capture)
		})
	}
}
