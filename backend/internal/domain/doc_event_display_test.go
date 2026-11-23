package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const eventDisplayTestPublic = "PUBLIC_BODY_MARKER"
const eventDisplayTestOpaque = "LOCKED_PRIVATE_BODY"

type eventDisplayTestFixture struct {
	raw          []byte
	state        DocEventDisplayPreparation
	objects      map[ContentHash][]byte
	checkpoints  map[string]DocEventDisplayPreparation
	steps        map[string]int
	rawLoads     map[string]int
	rawBytes     int
	maxRaw       int
	maxOuter     int
	maxActive    int
	opaqueMarker string
}

func newEventDisplayTestFixture(test *testing.T, raw []byte, empty bool) *eventDisplayTestFixture {
	test.Helper()
	state, err := NewDocEventDisplayPreparation(DocRangeEntry{
		DocEventIndex: DocEventIndex{Hash: HashContent(raw), Length: len(raw)},
		Kind:          EventReasoning, EmptyReasoning: empty,
	})
	if err != nil {
		test.Fatal(err)
	}
	return &eventDisplayTestFixture{
		raw: raw, state: state, objects: make(map[ContentHash][]byte), opaqueMarker: eventDisplayTestOpaque,
		checkpoints: make(map[string]DocEventDisplayPreparation),
		steps:       make(map[string]int), rawLoads: make(map[string]int),
	}
}

func (fixture *eventDisplayTestFixture) loadRaw(ctx context.Context, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if offset < 0 || length <= 0 || length > 64<<10 || offset > len(fixture.raw)-length {
		return nil, ErrIntegrity
	}
	fixture.rawLoads[fixture.state.Phase]++
	fixture.rawBytes += length
	fixture.maxRaw = max(fixture.maxRaw, length)
	return bytes.Clone(fixture.raw[offset : offset+length]), nil
}

func (fixture *eventDisplayTestFixture) loadObject(ctx context.Context, hash ContentHash) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, found := fixture.objects[hash]
	if !found {
		return nil, ErrNotFound
	}
	return bytes.Clone(raw), nil
}

func eventDisplayTestNoBodies(test *testing.T, raw []byte) {
	test.Helper()
	for _, marker := range []string{eventDisplayTestPublic, eventDisplayTestOpaque} {
		if bytes.Contains(raw, []byte(marker)) {
			test.Fatalf("checkpoint exposed body marker %q", marker)
		}
	}
}

func (fixture *eventDisplayTestFixture) restore(test *testing.T, state DocEventDisplayPreparation) DocEventDisplayPreparation {
	test.Helper()
	raw, err := state.Encode()
	if err != nil || len(raw) > 2048 {
		test.Fatalf("unbounded outer checkpoint: bytes=%d err=%v", len(raw), err)
	}
	eventDisplayTestNoBodies(test, raw)
	fixture.maxOuter = max(fixture.maxOuter, len(raw))
	restored, err := DecodeDocEventDisplayPreparation(HashContent(raw), bytes.Clone(raw))
	if err != nil || !reflect.DeepEqual(restored, state) {
		test.Fatalf("outer checkpoint did not survive restart: phase=%s err=%v", state.Phase, err)
	}
	if state.Active == "" {
		return restored
	}
	active, err := fixture.loadObject(context.Background(), state.Active)
	if err != nil || len(active) > 64<<10 {
		test.Fatalf("unbounded active checkpoint: phase=%s bytes=%d err=%v", state.Phase, len(active), err)
	}
	eventDisplayTestNoBodies(test, active)
	fixture.maxActive = max(fixture.maxActive, len(active))
	var encoded []byte
	switch state.Phase {
	case "discovery":
		var inner DocFieldDiscoveryState
		inner, err = DecodeDocFieldDiscoveryState(state.Active, active)
		if err == nil {
			encoded, err = inner.Encode()
		}
	case "decoding":
		var inner DocDecodedTextState
		inner, err = DecodeDocDecodedTextState(*state.Input, state.Active, active)
		if err == nil {
			encoded, err = inner.Encode(*state.Input)
		}
	case "markdown":
		var inner DocMarkdownPlanState
		inner, err = DecodeDocMarkdownPlanState(state.Active, active)
		if err == nil {
			encoded, err = inner.Encode()
		}
	case "assembly":
		var plan DocDecodedMarkdownPlan
		plan, err = DecodeDocDecodedMarkdownPlan(state.Plan, fixture.objects[state.Plan])
		if err == nil {
			var input DocDisplayAssemblyInput
			input, err = NewDocDecodedMarkdownAssemblyInput(plan)
			if err == nil {
				var inner DocDisplayAssemblyState
				inner, err = DecodeDocDisplayAssemblyState(input, state.Active, active)
				if err == nil {
					encoded, err = inner.Encode(input)
				}
			}
		}
	default:
		test.Fatalf("unexpected active phase %q", state.Phase)
	}
	if err != nil || !bytes.Equal(active, encoded) {
		test.Fatalf("active checkpoint did not survive restart: phase=%s err=%v", state.Phase, err)
	}
	return restored
}

func eventDisplayTestZeroStep(test *testing.T, step DocEventDisplayStep) {
	test.Helper()
	if !reflect.DeepEqual(step, DocEventDisplayStep{}) || step.Prior() != "" || len(step.Objects()) != 0 || !reflect.DeepEqual(step.Preparation(), DocEventDisplayPreparation{}) {
		test.Fatal("failed operation exposed a partial step")
	}
}

func (fixture *eventDisplayTestFixture) advance(test *testing.T) error {
	test.Helper()
	fixture.state = fixture.restore(test, fixture.state)
	prior, err := fixture.state.Encode()
	if err != nil {
		test.Fatal(err)
	}
	fixture.checkpoints[fixture.state.Phase] = fixture.state
	if fixture.steps[fixture.state.Phase] >= 4096 {
		test.Fatalf("phase did not complete: %s", fixture.state.Phase)
	}
	fixture.steps[fixture.state.Phase]++
	beforeRaw := fixture.rawBytes
	step, failure := StepDocEventDisplay(context.Background(), fixture.state, fixture.loadRaw, fixture.loadObject)
	after, err := fixture.state.Encode()
	if err != nil || !bytes.Equal(prior, after) {
		test.Fatal("step mutated its prior checkpoint", err)
	}
	if failure != nil {
		eventDisplayTestZeroStep(test, step)
		return failure
	}
	if step.Prior() != HashContent(prior) || fixture.rawBytes-beforeRaw > 64<<10 {
		test.Fatal("step lost its predecessor or exceeded the raw read budget")
	}
	objects := step.Objects()
	if len(objects) > 32 {
		test.Fatalf("unbounded step object count: %d", len(objects))
	}
	total := 0
	for _, object := range objects {
		total += len(object.Bytes)
		if len(object.Bytes) == 0 || len(object.Bytes) > MaxDocDisplayPageBytes || HashContent(object.Bytes) != object.Hash || fixture.opaqueMarker != "" && bytes.Contains(object.Bytes, []byte(fixture.opaqueMarker)) {
			test.Fatal("staging object is invalid or exposes opaque content")
		}
		if previous, found := fixture.objects[object.Hash]; found && !bytes.Equal(previous, object.Bytes) {
			test.Fatal("step changed an immutable object")
		}
		fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
	}
	if total > 1<<20 {
		test.Fatalf("unbounded step object bytes: %d", total)
	}
	fixture.state = fixture.restore(test, step.Preparation())
	next, err := fixture.state.Encode()
	if err != nil || bytes.Equal(prior, next) {
		test.Fatal("successful step did not progress", err)
	}
	return nil
}

func (fixture *eventDisplayTestFixture) finish(test *testing.T) DocDisplayProjectionGeneration {
	test.Helper()
	for fixture.state.Phase != "completed" {
		if err := fixture.advance(test); err != nil {
			test.Fatalf("event preparation failed: phase=%s err=%v", fixture.state.Phase, err)
		}
	}
	generation, err := ValidateDocEventDisplayPublication(context.Background(), fixture.state, fixture.loadObject)
	if err != nil || generation.Source.EventHash != HashContent(fixture.raw) || generation.Source.EventBytes != len(fixture.raw) || generation.Source.Plan != fixture.state.Plan {
		test.Fatal("publication lost its event or plan binding", err)
	}
	encoded, err := generation.Encode()
	if err != nil || HashContent(encoded) != fixture.state.Generation {
		test.Fatal("publication differs from the completed generation", err)
	}
	eventDisplayTestNoBodies(test, encoded)
	return generation
}

func TestDocEventDisplayInvalidTypesAndRawInputFailWithoutResult(test *testing.T) {
	for _, body := range []string{
		`{"kind":"reasoning","redacted_summary":false}`,
		`{"kind":"reasoning","redacted_summary":42}`,
		`{"kind":"reasoning","redacted_summary":[]}`,
		`{"kind":"reasoning","redacted_summary":{}}`,
		`{"kind":"reasoning","redacted_summary":"first","redacted_summary":false}`,
		`{"kind":false,"redacted_summary":"visible"}`,
		`{"kind":"tool_result","redacted_summary":"visible"}`,
		`{"kind":"reasoning","redacted_summary":"bad\u12xz"}`,
		`{"kind":"reasoning","redacted_summary":"unfinished"`,
		`{"kind":"reasoning","redacted_summary":"visible"} null`,
	} {
		test.Run(body, func(test *testing.T) {
			for _, empty := range []bool{false, true} {
				fixture := newEventDisplayTestFixture(test, []byte(body), empty)
				if err := fixture.advance(test); !errors.Is(err, ErrIntegrity) || len(fixture.objects) != 0 {
					test.Fatal("invalid raw event was accepted", empty, err)
				}
			}
		})
	}
}

func TestDocEventDisplayCancellationAndLoadFailuresAreAtomic(test *testing.T) {
	raw := []byte(`{"kind":"reasoning","opaque":"` + strings.Repeat(eventDisplayTestOpaque, 4000) + "\",\"redacted_summary\":\"# **PUBLIC_BODY_MARKER \ud55c\uae00🙂** [label](https://hidden.test)\"}")
	fixture := newEventDisplayTestFixture(test, raw, false)
	fixture.finish(test)
	loadFailure := errors.New("event display loader failed")
	for _, phase := range []string{"discovery", "decoding", "markdown", "assembly"} {
		state := fixture.checkpoints[phase]
		test.Run(phase, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			calls := 0
			step, err := StepDocEventDisplay(ctx, state,
				func(context.Context, int, int) ([]byte, error) { calls++; return nil, loadFailure },
				func(context.Context, ContentHash) ([]byte, error) { calls++; return nil, loadFailure })
			if !errors.Is(err, context.Canceled) || calls != 0 {
				test.Fatal("pre-canceled step performed work", calls, err)
			}
			eventDisplayTestZeroStep(test, step)
			calls = 0
			_, err = StepDocEventDisplay(context.Background(), state,
				func(ctx context.Context, offset, length int) ([]byte, error) {
					calls++
					return fixture.loadRaw(ctx, offset, length)
				},
				func(ctx context.Context, hash ContentHash) ([]byte, error) {
					calls++
					return fixture.loadObject(ctx, hash)
				})
			if err != nil || calls == 0 {
				test.Fatal("successful control step failed", err)
			}
			for target := 1; target <= calls; target++ {
				for _, cancelLoad := range []bool{false, true} {
					test.Run(fmt.Sprintf("load=%d/cancel=%t", target, cancelLoad), func(test *testing.T) {
						ctx, cancel := context.WithCancel(context.Background())
						defer cancel()
						count := 0
						intercept := func() error {
							count++
							if count != target {
								return nil
							}
							if cancelLoad {
								cancel()
								return nil
							}
							return loadFailure
						}
						before, err := state.Encode()
						if err != nil {
							test.Fatal(err)
						}
						step, err := StepDocEventDisplay(ctx, state,
							func(_ context.Context, offset, length int) ([]byte, error) {
								if err := intercept(); err != nil {
									return nil, err
								}
								return bytes.Clone(raw[offset : offset+length]), nil
							},
							func(_ context.Context, hash ContentHash) ([]byte, error) {
								if err := intercept(); err != nil {
									return nil, err
								}
								return bytes.Clone(fixture.objects[hash]), nil
							})
						expected := loadFailure
						if cancelLoad {
							expected = context.Canceled
						}
						if !errors.Is(err, expected) || count != target {
							test.Fatal("load interruption was not propagated", count, target, err)
						}
						eventDisplayTestZeroStep(test, step)
						after, err := state.Encode()
						if err != nil || !bytes.Equal(before, after) {
							test.Fatal("failed step changed its prior state", err)
						}
					})
				}
			}
		})
	}
}

func TestDocEventDisplaySealedStepGettersProtectMutations(test *testing.T) {
	fixture := newEventDisplayTestFixture(test, []byte(`{"kind":"reasoning","opaque":"`+strings.Repeat(eventDisplayTestOpaque, 4000)+`","redacted_summary":"**PUBLIC_BODY_MARKER**"}`), false)
	fixture.finish(test)
	for _, phase := range []string{"discovery", "decoding", "markdown", "assembly"} {
		test.Run(phase, func(test *testing.T) {
			step, err := StepDocEventDisplay(context.Background(), fixture.checkpoints[phase], fixture.loadRaw, fixture.loadObject)
			if err != nil || len(step.Objects()) == 0 {
				test.Fatal("could not obtain a sealed step", err)
			}
			wantState, wantObjects, wantPrior := step.Preparation(), step.Objects(), step.Prior()
			mutated := step.Preparation()
			mutated.Phase, mutated.Source.EventBytes = "forged", 1
			if mutated.Input != nil {
				mutated.Input.Token.From++
				mutated.Input.EventHash = HashContent([]byte("forged input"))
			}
			objects := step.Objects()
			objects[0].Bytes[0] ^= 1
			objects[0].Hash = HashContent([]byte("forged object"))
			objects = append(objects, DocRangeObject{Hash: "forged"})
			if !reflect.DeepEqual(step.Preparation(), wantState) || !reflect.DeepEqual(step.Objects(), wantObjects) || step.Prior() != wantPrior {
				test.Fatal("getter mutation changed a sealed step")
			}
		})
	}
}

func TestDocEventDisplayPublicationRejectsValidForeignObjects(test *testing.T) {
	for _, empty := range []bool{false, true} {
		test.Run(fmt.Sprintf("empty=%t", empty), func(test *testing.T) {
			summary := `,"redacted_summary":"**PUBLIC_BODY_MARKER**"`
			if empty {
				summary = `,"redacted_summary":null`
			}
			fixture := newEventDisplayTestFixture(test, []byte(`{"kind":"reasoning","opaque":"LOCKED_PRIVATE_BODY"`+summary+`}`), empty)
			generation := fixture.finish(test)
			other := newEventDisplayTestFixture(test, []byte(`{"kind":"reasoning","opaque":"different private origin"`+summary+`}`), empty)
			other.finish(test)
			for hash, body := range other.objects {
				fixture.objects[hash] = bytes.Clone(body)
			}
			state := fixture.state
			state.Generation = other.state.Generation
			result, err := ValidateDocEventDisplayPublication(context.Background(), state, fixture.loadObject)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(result, DocDisplayProjectionGeneration{}) {
				test.Fatal("publication accepted a valid generation from another event", err)
			}
			state = fixture.state
			state.Plan = other.state.Plan
			forged := generation
			forged.Source.Plan = state.Plan
			encoded, err := forged.Encode()
			if err != nil {
				test.Fatal(err)
			}
			state.Generation = HashContent(encoded)
			fixture.objects[state.Generation] = bytes.Clone(encoded)
			result, err = ValidateDocEventDisplayPublication(context.Background(), state, fixture.loadObject)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(result, DocDisplayProjectionGeneration{}) {
				test.Fatal("publication accepted a valid plan from another event", err)
			}
		})
	}
}
