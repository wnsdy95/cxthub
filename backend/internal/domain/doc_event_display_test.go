package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
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

func eventDisplayTestPages(test *testing.T, fixture *eventDisplayTestFixture, generation DocDisplayProjectionGeneration, oracle DocFindDisplayText) string {
	test.Helper()
	var output strings.Builder
	for index := 0; index < generation.Pages.Count; index++ {
		page, err := ReadDocDisplayProjectionPage(context.Background(), generation, index, fixture.loadObject)
		if err != nil || page.Index != index || page.From != output.Len() || page.ValidateProjection(generation.Source) != nil || fixture.opaqueMarker != "" && strings.Contains(page.Text, fixture.opaqueMarker) {
			test.Fatal("invalid published page", index, err)
		}
		if oracle != nil {
			for offset, character := range page.Text {
				actual, err := page.SourceRange(offset, utf8.RuneLen(character))
				want, failure := oracle.SourceRange(page.From+offset, utf8.RuneLen(character))
				if err != nil || failure != nil || actual != want || actual.From < 0 || actual.To > len(fixture.raw) {
					test.Fatalf("original rune span changed: page=%d offset=%d got=%v want=%v err=%v/%v", index, offset, actual, want, err, failure)
				}
			}
		}
		output.WriteString(page.Text)
	}
	if output.Len() != generation.Source.TextBytes || oracle != nil && output.String() != oracle.Text() {
		test.Fatalf("published text differs: bytes=%d expected=%d", output.Len(), generation.Source.TextBytes)
	}
	return output.String()
}

func TestDocEventDisplaySmallOracleAndOriginalRuneSpans(test *testing.T) {
	for _, scenario := range []struct {
		name string
		body string
	}{
		{"markdown", `{"kind":"reasoning","opaque":{"secret":"LOCKED_PRIVATE_BODY"},"redacted_summary":"# PUBLIC_BODY_MARKER **한글🙂**\n\n[label](https://hidden.test) and ` + "`code`" + `"}`},
		{"missing", `{"kind":"reasoning","opaque":{"redacted_summary":"LOCKED_PRIVATE_BODY"}}`},
		{"null", `{"kind":"reasoning","redacted_summary":null,"opaque":"LOCKED_PRIVATE_BODY"}`},
		{"empty", `{"kind":"reasoning","redacted_summary":""}`},
		{"whitespace", `{"kind":"reasoning","redacted_summary":" \r\n\t\u00a0\u2000\u2028\ufeff "}`},
		{"zero_markdown_output", `{"kind":"reasoning","redacted_summary":"<!-- PUBLIC_BODY_MARKER -->"}`},
		{"duplicates_last_string", `{"kind":"tool_result","kind":"reasoning","redacted_summary":false,"redacted_summary":"ignored","redacted\u005fsummary":"**PUBLIC_BODY_MARKER last**"}`},
		{"duplicates_last_null", `{"kind":"reasoning","redacted_summary":"ignored","redacted_summary":null}`},
		{"escaped_unicode", `{"k\u0069nd":"reasoning","redacted\u005fsummary":"# **\uD55C\uAE00\uD83D\uDE42** [label](https://hidden.test)\n- \uD800 \uDC00 \"quoted\" \\ end"}`},
		{"collapsed_whitespace", `{"kind":"reasoning","redacted_summary":"# first  **bold**\n\n> quote\n\n- list\n1. next\n\n` + "```" + `\n**literal**\r\n` + "```" + `"}`},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			raw := []byte(" \r\n\t " + scenario.body + "\r\n ")
			entry, err := docRangeEntry(0, 0, raw, 0)
			if err != nil {
				test.Fatal(err)
			}
			oracle, err := BuildDocEventDisplay(context.Background(), raw)
			if err != nil {
				test.Fatal(err)
			}
			fixture := newEventDisplayTestFixture(test, raw, entry.EmptyReasoning)
			generation := fixture.finish(test)
			eventDisplayTestPages(test, fixture, generation, oracle)
			for _, phase := range []string{"decoding", "markdown", "assembly"} {
				if (fixture.steps[phase] > 0) == entry.EmptyReasoning {
					test.Fatalf("unexpected empty reasoning route: empty=%t phase=%s steps=%d", entry.EmptyReasoning, phase, fixture.steps[phase])
				}
			}
		})
	}
}

func TestDocEventDisplayPaddedOpaqueDiscoveryResumes(test *testing.T) {
	raw := []byte(strings.Repeat(" \n", (64<<10)/2-23) + `{"opaque":{"secret":"` + strings.Repeat(eventDisplayTestOpaque, 4200) + `"},"kind":"reasoning","redacted_summary":"# **PUBLIC_BODY_MARKER \uD83D\uDE42** [label](https://hidden.test)"}` + "\r\n")
	fixture := newEventDisplayTestFixture(test, raw, false)
	generation := fixture.finish(test)
	if fixture.steps["discovery"] < 3 || fixture.maxRaw != 64<<10 || fixture.rawLoads["markdown"] != 0 || fixture.rawLoads["assembly"] != 0 {
		test.Fatal("opaque or padded input did not use bounded multi-step discovery", fixture.steps, fixture.rawLoads, fixture.maxRaw)
	}
	public := []byte(`{"kind":"reasoning","redacted_summary":"# **PUBLIC_BODY_MARKER \uD83D\uDE42** [label](https://hidden.test)"}`)
	oracle, err := BuildDocEventDisplay(context.Background(), public)
	if err != nil || eventDisplayTestPages(test, fixture, generation, nil) != oracle.Text() {
		test.Fatal("opaque input changed public display", err)
	}
	for phase, state := range fixture.checkpoints {
		result, err := ValidateDocEventDisplayPublication(context.Background(), state, fixture.loadObject)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(result, DocDisplayProjectionGeneration{}) {
			test.Fatal("incomplete phase became publishable", phase, err)
		}
	}
}

func TestDocEventDisplaySmallMultiPageOracleAndOriginalRuneSpans(test *testing.T) {
	raw := []byte(" \r\n" + `{"kind":"reasoning","opaque":"LOCKED_PRIVATE_BODY","redacted_summary":"**PUBLIC_BODY_MARKER ` + strings.Repeat(`\u0061\uD83D\uDE42`, 600) + `**"}` + "\t ")
	oracle, err := BuildDocEventDisplay(context.Background(), raw)
	if err != nil {
		test.Fatal(err)
	}
	fixture := newEventDisplayTestFixture(test, raw, false)
	generation := fixture.finish(test)
	if generation.Pages.Count < 2 || fixture.steps["decoding"] < 2 || fixture.steps["assembly"] < 2 {
		test.Fatal("escaped reasoning did not cross output page boundaries", generation.Pages.Count, fixture.steps)
	}
	eventDisplayTestPages(test, fixture, generation, oracle)
}

func TestDocEventDisplayMissingAndNullRequireAuthoritativeEmptyMetadata(test *testing.T) {
	for _, summary := range []string{"", `,"redacted_summary":null`} {
		for _, empty := range []bool{false, true} {
			test.Run(fmt.Sprintf("summary=%q/empty=%t", summary, empty), func(test *testing.T) {
				raw := []byte(`{"kind":"reasoning","opaque":{"redacted_summary":"LOCKED_PRIVATE_BODY"}` + summary + `}`)
				fixture := newEventDisplayTestFixture(test, raw, empty)
				if !empty {
					if err := fixture.advance(test); !errors.Is(err, ErrDocFindDisplayUnavailable) || fixture.state.Phase != "discovery" || len(fixture.objects) != 0 {
						test.Fatal("missing summary inferred empty without authoritative metadata", err)
					}
					return
				}
				generation := fixture.finish(test)
				if generation.Source.TextBytes != 0 || generation.Pages.Count != 0 || fixture.state.Decoded != "" || fixture.steps["decoding"] != 0 || eventDisplayTestPages(test, fixture, generation, nil) != "" {
					test.Fatal("authoritative empty reasoning exposed content")
				}
			})
		}
	}
	fixture := newEventDisplayTestFixture(test, []byte(`{"kind":"reasoning","redacted_summary":" \t\r\n "}`), false)
	generation := fixture.finish(test)
	if generation.Source.TextBytes != 0 || generation.Pages.Count != 0 || fixture.state.Decoded == "" || fixture.steps["assembly"] == 0 {
		test.Fatal("whitespace incorrectly bypassed the nonempty metadata route")
	}
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
	raw := []byte(`{"kind":"reasoning","opaque":"` + strings.Repeat(eventDisplayTestOpaque, 4000) + `","redacted_summary":"# **PUBLIC_BODY_MARKER 한글🙂** [label](https://hidden.test)"}`)
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

func TestDocEventDisplaySealedStepDoesNotAliasPriorDecodingInput(test *testing.T) {
	raw := []byte(`{"kind":"reasoning","redacted_summary":"**PUBLIC_BODY_MARKER ` + strings.Repeat(`\u0061\uD83D\uDE42`, 600) + `**"}`)
	fixture := newEventDisplayTestFixture(test, raw, false)
	for fixture.state.Phase == "discovery" {
		if err := fixture.advance(test); err != nil {
			test.Fatal(err)
		}
	}
	prior := fixture.state
	if prior.Phase != "decoding" || prior.Input == nil {
		test.Fatal("fixture did not reach decoding")
	}
	originalInput := *prior.Input
	priorBytes, err := prior.Encode()
	if err != nil {
		test.Fatal(err)
	}
	step, err := StepDocEventDisplay(context.Background(), prior, fixture.loadRaw, fixture.loadObject)
	if err != nil {
		test.Fatal(err)
	}
	wantState, wantObjects := step.Preparation(), step.Objects()
	if wantState.Phase != "decoding" || wantState.Input == nil || *wantState.Input != originalInput || wantState.Active == prior.Active || step.Prior() != HashContent(priorBytes) {
		test.Fatal("fixture did not produce an unfinished decoding step")
	}
	wantBytes, err := wantState.Encode()
	if err != nil {
		test.Fatal(err)
	}
	prior.Input.Token.From++
	prior.Input.EventHash = HashContent([]byte("mutated prior input"))
	prior.Input.EventBytes++
	actual := step.Preparation()
	actualBytes, err := actual.Encode()
	if err != nil || !reflect.DeepEqual(actual, wantState) || actual.Input == nil || *actual.Input != originalInput || !bytes.Equal(actualBytes, wantBytes) || !reflect.DeepEqual(step.Objects(), wantObjects) || step.Prior() != HashContent(priorBytes) {
		test.Fatal("mutating prior input changed the sealed decoding step", err)
	}
	for _, object := range step.Objects() {
		fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
	}
	fixture.state = actual
	generation := fixture.finish(test)
	if got := eventDisplayTestPages(test, fixture, generation, nil); got != eventDisplayTestPublic+" "+strings.Repeat("a🙂", 600) {
		test.Fatal("sealed step could not resume after prior input mutation")
	}
}

func FuzzDocEventDisplayPreparationParity(fuzz *testing.F) {
	for _, text := range []string{
		"PUBLIC_BODY_MARKER normal 한글🙂",
		"",
		" \t\r\n\u00a0\u2000\u2028\ufeff ",
		"first\n\nsecond\r\nthird",
		"**strong 한글🙂**",
		"[label](https://hidden.test)",
		"```go\n**literal**\r\n```",
		`\uD800 \uDC00 \uD83D\uDE42`,
		"\xed\xa0\x80 \xed\xb0\x80",
		"\x00\b\f\t\r\n\"\\\x1f",
		eventDisplayTestOpaque,
		"small-private-state",
	} {
		fuzz.Add(text)
	}
	fuzz.Fuzz(func(test *testing.T, text string) {
		if len(text) > 1024 {
			return
		}
		raw, err := json.Marshal(map[string]any{
			"kind": EventReasoning, "redacted_summary": text, "opaque": "small-private-state",
		})
		if err != nil {
			test.Fatal("small reasoning event could not be encoded", err)
		}
		oracle, err := BuildDocEventDisplay(context.Background(), raw)
		if err != nil {
			test.Fatal("small valid reasoning event failed the display oracle", err)
		}
		fixture := newEventDisplayTestFixture(test, raw, false)
		fixture.opaqueMarker = ""
		generation := fixture.finish(test)
		eventDisplayTestPages(test, fixture, generation, oracle)
	})
}

func TestDocEventDisplayPublicationBindsOriginAndRejectsPartialResults(test *testing.T) {
	for _, empty := range []bool{false, true} {
		test.Run(fmt.Sprintf("empty=%t", empty), func(test *testing.T) {
			summary := `,"redacted_summary":"**PUBLIC_BODY_MARKER**"`
			if empty {
				summary = `,"redacted_summary":null`
			}
			fixture := newEventDisplayTestFixture(test, []byte(`{"kind":"reasoning","opaque":"LOCKED_PRIVATE_BODY"`+summary+`}`), empty)
			generation := fixture.finish(test)
			eventDisplayTestPages(test, fixture, generation, nil)
			for _, mutate := range []func(*DocEventDisplayPreparation){
				func(state *DocEventDisplayPreparation) { state.Source.EventHash = HashContent([]byte("other event")) },
				func(state *DocEventDisplayPreparation) { state.Source.EventBytes++ },
				func(state *DocEventDisplayPreparation) { state.Plan = HashContent([]byte("other plan")) },
			} {
				state := fixture.state
				mutate(&state)
				result, err := ValidateDocEventDisplayPublication(context.Background(), state, fixture.loadObject)
				if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(result, DocDisplayProjectionGeneration{}) {
					test.Fatal("publication accepted a different origin or plan", err)
				}
			}
			loads := 0
			_, err := ValidateDocEventDisplayPublication(context.Background(), fixture.state, func(ctx context.Context, hash ContentHash) ([]byte, error) {
				loads++
				return fixture.loadObject(ctx, hash)
			})
			if err != nil {
				test.Fatal(err)
			}
			for target := 0; target <= loads; target++ {
				for _, cancelLoad := range []bool{false, true} {
					ctx, cancel := context.WithCancel(context.Background())
					count := 0
					failure := errors.New("publication load failed")
					if target == 0 {
						cancel()
					}
					result, err := ValidateDocEventDisplayPublication(ctx, fixture.state, func(_ context.Context, hash ContentHash) ([]byte, error) {
						count++
						if count == target {
							if !cancelLoad {
								return nil, failure
							}
							cancel()
						}
						return bytes.Clone(fixture.objects[hash]), nil
					})
					expected := failure
					if target == 0 || cancelLoad {
						expected = context.Canceled
					}
					cancel()
					if !errors.Is(err, expected) || !reflect.DeepEqual(result, DocDisplayProjectionGeneration{}) || count != target {
						test.Fatal("failed publication exposed a partial generation", target, count, err)
					}
				}
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

func TestDocEventDisplayLargeFormattedReasoningResumesLateAssembly(test *testing.T) {
	const lines = 1200
	line := "PUBLIC_BODY_MARKER " + strings.Repeat("readable ", 6) + "한글🙂 label"
	want := strings.TrimSuffix(strings.Repeat(line+"\n", lines), "\n")
	tokenLine := "# **PUBLIC_BODY_MARKER " + strings.Repeat("readable ", 6) + `한글\uD83D\uDE42** [label](https://hidden.test)\n`
	raw := []byte(" \n\t" + `{"opaque":{"secret":"` + strings.Repeat(eventDisplayTestOpaque, 6000) + `"},"kind":"reasoning","redacted_summary":"` + strings.Repeat(tokenLine, lines) + `"}` + "\r\n")
	fixture := newEventDisplayTestFixture(test, raw, false)
	for {
		if err := fixture.advance(test); err != nil {
			test.Fatal("large event preparation failed", fixture.state.Phase, err)
		}
		if fixture.state.Phase == "completed" {
			test.Fatal("large assembly never exposed a late resumable checkpoint")
		}
		if fixture.state.Phase == "assembly" {
			var checkpoint DocDisplayAssemblyState
			plan, err := DecodeDocDecodedMarkdownPlan(fixture.state.Plan, fixture.objects[fixture.state.Plan])
			if err != nil {
				test.Fatal(err)
			}
			input, err := NewDocDecodedMarkdownAssemblyInput(plan)
			if err == nil {
				checkpoint, err = DecodeDocDisplayAssemblyState(input, fixture.state.Active, fixture.objects[fixture.state.Active])
			}
			if err != nil {
				test.Fatal(err)
			}
			if checkpoint.Output.Directory.Count >= 2 && checkpoint.Output.TextBytes > len(want)/2 {
				break
			}
		}
	}
	encoded, err := fixture.state.Encode()
	if err != nil {
		test.Fatal(err)
	}
	resumed := newEventDisplayTestFixture(test, raw, false)
	resumed.state, err = DecodeDocEventDisplayPreparation(HashContent(encoded), bytes.Clone(encoded))
	if err != nil {
		test.Fatal(err)
	}
	for hash, body := range fixture.objects {
		resumed.objects[hash] = bytes.Clone(body)
	}
	generation := resumed.finish(test)
	if got := eventDisplayTestPages(test, resumed, generation, nil); got != want {
		test.Fatalf("large resumed display changed: bytes=%d want=%d", len(got), len(want))
	}
	if fixture.steps["discovery"] < 2 || fixture.steps["decoding"] < 2 || fixture.steps["markdown"] < 2 || fixture.steps["assembly"] < 2 || resumed.steps["assembly"] == 0 || resumed.rawBytes != 0 || fixture.maxRaw > 64<<10 {
		test.Fatal("large event did not exercise bounded, object-only late resumption", fixture.steps, resumed.steps, resumed.rawBytes, fixture.maxRaw)
	}
	loads := 0
	last, err := ReadDocDisplayProjectionPage(context.Background(), generation, generation.Pages.Count-1, func(ctx context.Context, hash ContentHash) ([]byte, error) {
		loads++
		return resumed.loadObject(ctx, hash)
	})
	if err != nil || last.From <= len(want)/2 || last.Text != want[last.From:] || loads > 5 {
		test.Fatal("late page scanned preceding output or changed text", loads, err)
	}
	foundEmoji := false
	for offset, character := range last.Text {
		if character != '🙂' {
			continue
		}
		span, err := last.SourceRange(offset, utf8.RuneLen(character))
		if err != nil || span.From < 0 || span.To > len(raw) || string(raw[span.From:span.To]) != `\uD83D\uDE42` {
			test.Fatal("late escaped Unicode lost original event coordinates", span, err)
		}
		foundEmoji = true
		break
	}
	if !foundEmoji {
		test.Fatal("late page did not exercise escaped Unicode provenance")
	}
	test.Logf("event_bytes=%d public_bytes=%d pages=%d steps=%v resumed_steps=%v max_raw=%d max_outer=%d max_active=%d late_page_loads=%d", len(raw), len(want), generation.Pages.Count, fixture.steps, resumed.steps, fixture.maxRaw, max(fixture.maxOuter, resumed.maxOuter), max(fixture.maxActive, resumed.maxActive), loads)
}
