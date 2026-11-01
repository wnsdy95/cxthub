package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

type docToolStringFixture struct {
	plan    DocToolStringPlan
	states  []DocToolStringState
	objects map[ContentHash][]byte
	reads   int
	maxRaw  int
	raw     []byte
}

func docToolStringTestSource(token string, mode DocToolStringMode) (DocToolStringSource, []byte) {
	prefix := `{"kind":"tool_result","output":{"selected":`
	raw := []byte(prefix + token + `},"opaque":{"output":"NEVER_PUBLIC_BODY_MARKER"}}`)
	return DocToolStringSource{Version: 1, Profile: DocToolStringProfile, EventHash: HashContent(raw), EventBytes: len(raw), Token: DocFindSpan{From: len(prefix), To: len(prefix) + len(token)}, Mode: mode}, raw
}

func prepareDocToolStringFixture(test testing.TB, token string, mode DocToolStringMode) docToolStringFixture {
	test.Helper()
	ctx := context.Background()
	source, raw := docToolStringTestSource(token, mode)
	encoded, err := source.Encode()
	if err != nil {
		test.Fatal(err)
	}
	source, err = DecodeDocToolStringSource(HashContent(encoded), encoded)
	if err != nil {
		test.Fatal(err)
	}
	state, err := NewDocToolStringState(source)
	if err != nil {
		test.Fatal(err)
	}
	fixture := docToolStringFixture{objects: make(map[ContentHash][]byte), raw: raw}
	load := func(_ context.Context, hash ContentHash) ([]byte, error) {
		value, found := fixture.objects[hash]
		if !found {
			return nil, ErrNotFound
		}
		return bytes.Clone(value), nil
	}
	put := func(objects []DocRangeObject) {
		for _, object := range objects {
			if len(object.Bytes) > MaxDocDisplayPageBytes || HashContent(object.Bytes) != object.Hash {
				test.Fatal("invalid immutable object")
			}
			fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
		}
	}
	for !state.Done {
		before, _ := state.Encode(source)
		fixture.states = append(fixture.states, state)
		readBytes := 0
		step, err := StepDocToolString(ctx, source, state, func(_ context.Context, from, length int) ([]byte, error) {
			if from < source.Token.From || length <= 0 || from > source.Token.To-length {
				test.Fatal("read escaped selected token", from, length)
			}
			readBytes += length
			fixture.reads++
			return bytes.Clone(raw[from : from+length]), nil
		}, load)
		if err != nil || step.ReadBytes != readBytes || readBytes > DocDisplayStepSourceBytes {
			test.Fatal("step failed or exceeded budget", len(fixture.states), step.ReadBytes, readBytes, err)
		}
		fixture.maxRaw = max(fixture.maxRaw, readBytes)
		after, _ := state.Encode(source)
		if !bytes.Equal(before, after) {
			test.Fatal("previous checkpoint mutated")
		}
		put(step.Objects)
		encoded, err := step.State.Encode(source)
		if err != nil || len(encoded) > MaxDocDisplayStateBytes || bytes.Equal(before, encoded) || bytes.Contains(encoded, []byte("PUBLIC_BODY_MARKER")) {
			test.Fatal("invalid resumable checkpoint", err)
		}
		state, err = DecodeDocToolStringState(source, HashContent(encoded), encoded)
		if err != nil || ValidateDocToolStringCheckpoint(ctx, source, state, load) != nil {
			test.Fatal("checkpoint restart failed", err)
		}
	}
	fixture.states = append(fixture.states, state)
	plan, objects, err := FinishDocToolString(ctx, source, state, load)
	if err != nil {
		test.Fatal(err)
	}
	put(objects)
	encoded, err = plan.Encode()
	if err != nil {
		test.Fatal(err)
	}
	fixture.plan, err = DecodeDocToolStringPlan(HashContent(encoded), encoded)
	if err != nil {
		test.Fatal(err)
	}
	return fixture
}

func docToolStringSmallOracle(test testing.TB, source DocToolStringSource, raw []byte) *docToolDisplay {
	test.Helper()
	builder := docToolDisplayBuilder{ctx: context.Background(), parser: docFindSourceParser{raw: raw}, display: docToolDisplay{sourceBytes: len(raw)}, metadata: docToolDisplayBaseBytes, budget: MaxDocToolDisplayBytes}
	var err error
	if source.Mode == DocToolStringQuoted {
		err = builder.string(source.Token, true, 0)
	} else {
		err = builder.diff(source.Token, docToolStringPrefix(source.Mode))
	}
	if err != nil {
		test.Fatal(err)
	}
	builder.display.text = builder.text.String()
	return &builder.display
}

func TestDocToolStringModesMatchOriginalTextAndEveryRune(test *testing.T) {
	tokens := []string{
		`""`, `"plain"`, `"**not markdown** [label](url)"`, `"\n\r\nlast\n"`,
		`"\u003c\u0026\u003e\u2028\u2029\u0000\u000b\b\f\n\r\t\"\\\/\ufffd"`,
		`"\ud83d\ude42\uac00\r"`, `"\uD800\udfff\ud800\uD800\udc00\ufffd"`,
		`"한글🙂/<&>"`, `"\ud800x\udfff"`,
	}
	for _, mode := range []DocToolStringMode{DocToolStringQuoted, DocToolStringAdd, DocToolStringRemove} {
		for position, token := range tokens {
			test.Run(fmt.Sprintf("%s/%d", mode, position), func(test *testing.T) {
				fixture := prepareDocToolStringFixture(test, token, mode)
				oracle := docToolStringSmallOracle(test, fixture.plan.Source, fixture.raw)
				var actual strings.Builder
				for index := 0; index < fixture.plan.Pages.Count; index++ {
					page, err := ReadDocToolStringPage(context.Background(), fixture.plan, index, func(_ context.Context, hash ContentHash) ([]byte, error) { return fixture.objects[hash], nil })
					if err != nil || page.Display.From != actual.Len() {
						test.Fatal(err)
					}
					actual.WriteString(page.Display.Text)
					for offset, character := range page.Display.Text {
						got, err := page.SourceRange(offset, utf8.RuneLen(character))
						want, expectedErr := oracle.SourceRange(page.Display.From+offset, utf8.RuneLen(character))
						if err != nil || expectedErr != nil || got != want {
							test.Fatal("source mismatch", offset, got, want, err, expectedErr)
						}
					}
				}
				if actual.String() != oracle.Text() {
					test.Fatalf("display = %q; want %q", actual.String(), oracle.Text())
				}
			})
		}
	}
}

func TestDocToolStringLargeRestartAndBoundedAssembly(test *testing.T) {
	for _, mode := range []DocToolStringMode{DocToolStringQuoted, DocToolStringAdd, DocToolStringRemove} {
		test.Run(string(mode), func(test *testing.T) {
			token := `"` + strings.Repeat(`PUBLIC_BODY_MARKER\uD800\ud83d\ude42\n\r\u0000🙂`, 3200) + `\n"`
			fixture := prepareDocToolStringFixture(test, token, mode)
			input, err := NewDocToolStringAssemblyInput(fixture.plan)
			if err != nil {
				test.Fatal(err)
			}
			assembled := assembleDisplayFixture(test, input, fixture.objects)
			actual := assembledFixturePages(test, assembled, nil)
			var decoded string
			if json.Unmarshal([]byte(token), &decoded) != nil {
				test.Fatal("invalid fixture")
			}
			want := docToolStringPrefix(mode) + strings.ReplaceAll(decoded, "\n", "\n"+docToolStringPrefix(mode))
			if mode == DocToolStringQuoted {
				want = `"` + strings.Repeat(`PUBLIC_BODY_MARKER\ud800🙂\n\r\u0000🙂`, 3200) + `\n"`
			}
			if actual != want || len(fixture.states) < 5 {
				test.Fatal("large text changed", len(actual), len(want), len(fixture.states))
			}
			loads := 0
			last, err := ReadDocToolStringPage(context.Background(), fixture.plan, fixture.plan.Pages.Count-1, func(_ context.Context, hash ContentHash) ([]byte, error) { loads++; return fixture.objects[hash], nil })
			if err != nil || last.RawTo != fixture.plan.Source.Token.To || loads > 4 {
				test.Fatal("late lookup scanned the prefix", loads, err)
			}
			test.Logf("raw=%d text=%d token_pages=%d checkpoints=%d final_pages=%d max_step_raw=%d late_loads=%d", len(fixture.raw), len(actual), fixture.plan.Pages.Count, len(fixture.states), assembled.generation.Pages.Count, fixture.maxRaw, loads)
		})
	}
}

func TestDocToolStringErrorsDiscardAllOutputs(test *testing.T) {
	ctx := context.Background()
	source, raw := docToolStringTestSource(`"`+strings.Repeat(`abc\n`, 2000)+`"`, DocToolStringAdd)
	state, _ := NewDocToolStringState(source)
	failure := errors.New("injected read failure")
	cases := []struct {
		name string
		load func(context.Context, int, int) ([]byte, error)
	}{
		{"failure-after-prefix", func(context.Context, int, int) ([]byte, error) { return nil, failure }},
		{"short-read", func(_ context.Context, from, length int) ([]byte, error) { return raw[from : from+length-1], nil }},
		{"bad-source", func(_ context.Context, from, length int) ([]byte, error) {
			value := bytes.Clone(raw[from : from+length])
			value[0] = 'x'
			return value, nil
		}},
	}
	for _, scenario := range cases {
		test.Run(scenario.name, func(test *testing.T) {
			step, err := StepDocToolString(ctx, source, state, scenario.load, nil)
			if err == nil || !reflect.DeepEqual(step, DocToolStringStep{}) {
				test.Fatal("error retained partial output", err, step)
			}
		})
	}
	cancelled, cancel := context.WithCancel(ctx)
	step, err := StepDocToolString(cancelled, source, state, func(_ context.Context, from, length int) ([]byte, error) {
		cancel()
		return raw[from : from+length], nil
	}, nil)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(step, DocToolStringStep{}) {
		test.Fatal("cancellation retained partial output", err)
	}
	for _, span := range []DocFindSpan{{From: math.MaxInt, To: math.MinInt}, {From: -1, To: 2}, {From: 1, To: math.MinInt}} {
		invalid := source
		invalid.Token = span
		if invalid.Validate() == nil {
			test.Fatal("invalid token bounds accepted", span)
		}
	}
}

func TestDocToolStringCheckpointRejectsCorruptionBeforeSourceReads(test *testing.T) {
	fixture := prepareDocToolStringFixture(test, `"`+strings.Repeat(`body\n`, 5000)+`"`, DocToolStringRemove)
	source := fixture.plan.Source
	state := fixture.states[1]
	load := func(_ context.Context, hash ContentHash) ([]byte, error) { return fixture.objects[hash], nil }
	for _, mutation := range []struct {
		name   string
		change func(*DocToolStringState)
	}{
		{"raw-position", func(state *DocToolStringState) { state.Next++ }},
		{"text-position", func(state *DocToolStringState) { state.TextBytes++ }},
		{"source", func(state *DocToolStringState) { state.Source = HashContent([]byte("other")) }},
	} {
		test.Run(mutation.name, func(test *testing.T) {
			changed := state
			mutation.change(&changed)
			reads := 0
			step, err := StepDocToolString(context.Background(), source, changed, func(_ context.Context, from, length int) ([]byte, error) {
				reads++
				return fixture.raw[from : from+length], nil
			}, load)
			if !errors.Is(err, ErrIntegrity) || reads != 0 || !reflect.DeepEqual(step, DocToolStringStep{}) {
				test.Fatal("corrupt checkpoint resumed raw input", reads, err)
			}
		})
	}
	reads := 0
	step, err := StepDocToolString(context.Background(), source, state, func(_ context.Context, from, length int) ([]byte, error) {
		reads++
		return fixture.raw[from : from+length], nil
	}, func(_ context.Context, hash ContentHash) ([]byte, error) {
		return append(bytes.Clone(fixture.objects[hash]), ' '), nil
	})
	if !errors.Is(err, ErrIntegrity) || reads != 0 || !reflect.DeepEqual(step, DocToolStringStep{}) {
		test.Fatal("corrupt page resumed raw input", reads, err)
	}
	completed := fixture.states[len(fixture.states)-1]
	plan, objects, err := FinishDocToolString(context.Background(), source, completed, func(context.Context, ContentHash) ([]byte, error) { return nil, ErrNotFound })
	if !errors.Is(err, ErrNotFound) || !reflect.DeepEqual(plan, DocToolStringPlan{}) || len(objects) != 0 {
		test.Fatal("completion ignored missing checkpoint page", err)
	}
}

func TestDocToolStringAssemblyRejectsInvalidCursorAndCorruption(test *testing.T) {
	fixture := prepareDocToolStringFixture(test, `"`+strings.Repeat(`x\uD800\n`, 1500)+`"`, DocToolStringQuoted)
	input, err := NewDocToolStringAssemblyInput(fixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	for _, change := range []func(*docToolStringCursor){
		func(cursor *docToolStringCursor) { cursor.Byte = 1 },
		func(cursor *docToolStringCursor) { cursor.Plan = HashContent(nil) },
		func(cursor *docToolStringCursor) { cursor.Page = fixture.plan.Pages.Count },
	} {
		var cursor docToolStringCursor
		if json.Unmarshal(input.initial, &cursor) != nil {
			test.Fatal("invalid initial cursor")
		}
		change(&cursor)
		raw, _ := canonicalJSON(cursor)
		loads := 0
		page, next, err := input.read(context.Background(), raw, 200, 10, func(_ context.Context, hash ContentHash) ([]byte, error) { loads++; return fixture.objects[hash], nil })
		if !errors.Is(err, ErrIntegrity) || loads != 0 || !reflect.DeepEqual(page, DocDisplayPage{}) || len(next) != 0 {
			test.Fatal("bad cursor performed I/O or returned output", loads, err)
		}
	}
	failure := errors.New("object failure")
	page, next, err := input.read(context.Background(), input.initial, 200, 10, func(context.Context, ContentHash) ([]byte, error) { return nil, failure })
	if !errors.Is(err, failure) || !reflect.DeepEqual(page, DocDisplayPage{}) || len(next) != 0 {
		test.Fatal("object error returned partial output", err)
	}
}

func TestDocToolStringLargePlainAndEscapedBoundaryTokens(test *testing.T) {
	for _, width := range []int{1023, 4095, 32755, 65533, 65534, 65535, 65536} {
		for _, suffix := range []string{`\ud83d\ude42`, `🙂`, `\ud800`, `\n`, ``} {
			token := `"` + strings.Repeat("a", width) + suffix + `"`
			fixture := prepareDocToolStringFixture(test, token, DocToolStringQuoted)
			var result strings.Builder
			for index := 0; index < fixture.plan.Pages.Count; index++ {
				page, err := ReadDocToolStringPage(context.Background(), fixture.plan, index, func(_ context.Context, hash ContentHash) ([]byte, error) { return fixture.objects[hash], nil })
				if err != nil {
					test.Fatal(width, suffix, err)
				}
				result.WriteString(page.Display.Text)
			}
			oracle := docToolStringSmallOracle(test, fixture.plan.Source, fixture.raw)
			if result.String() != oracle.Text() {
				test.Fatal("boundary changed output", width, suffix)
			}
		}
	}
}

func TestDocToolStringLateReadsOnlySelectedDirectoryPath(test *testing.T) {
	for _, count := range []int{5000, 100000} {
		fixture := prepareDocToolStringFixture(test, `"`+strings.Repeat(`\u0061`, count)+`"`, DocToolStringAdd)
		loads, readBytes := 0, 0
		page, err := ReadDocToolStringPage(context.Background(), fixture.plan, fixture.plan.Pages.Count-1, func(_ context.Context, hash ContentHash) ([]byte, error) {
			loads++
			raw := fixture.objects[hash]
			readBytes += len(raw)
			return raw, nil
		})
		if err != nil || loads > 3 || readBytes > MaxDocDisplayPageBytes+2*MaxDocRangeDirectoryBytes || page.RawTo != fixture.plan.Source.Token.To {
			test.Fatal("selected-page cost scaled with complete token", count, loads, readBytes, err)
		}
		test.Logf("decoded_runes=%d pages=%d late_object_loads=%d late_bytes=%d", count, fixture.plan.Pages.Count, loads, readBytes)
	}
}

func TestDocToolDisplayJSONOverflowDoesNotChangeSourceFind(test *testing.T) {
	raw := []byte(`{"kind":"tool_result","output":[1e1000]}`)
	display, err := BuildDocToolDisplay(context.Background(), raw)
	if err != nil || display.Text() != "[\n  null\n]" {
		test.Fatal("JS tool display did not normalize overflow", err)
	}
	if _, err := LocateDocFindTextSource(raw, 0, 0, 1); !errors.Is(err, ErrIntegrity) {
		test.Fatal("tool display changed typed source-find number validation", err)
	}
}

func FuzzDocToolStringTransformParity(fuzz *testing.F) {
	for _, seed := range []string{"", "x\r\n", "한🙂\x00\t\"\\", "**literal**"} {
		fuzz.Add(seed, uint8(0))
	}
	fuzz.Fuzz(func(test *testing.T, text string, selector uint8) {
		if len(text) > 2048 {
			test.Skip()
		}
		token, err := json.Marshal(text)
		if err != nil {
			test.Fatal(err)
		}
		mode := []DocToolStringMode{DocToolStringQuoted, DocToolStringAdd, DocToolStringRemove}[int(selector)%3]
		fixture := prepareDocToolStringFixture(test, string(token), mode)
		var normalized string
		if json.Unmarshal(token, &normalized) != nil {
			test.Fatal("invalid normalized string")
		}
		want := docToolStringPrefix(mode) + strings.ReplaceAll(normalized, "\n", "\n"+docToolStringPrefix(mode))
		if mode == DocToolStringQuoted {
			var quoted strings.Builder
			quoted.WriteByte('"')
			for _, character := range normalized {
				if character < 0x20 || character == '"' || character == '\\' {
					escaped, err := json.Marshal(string(character))
					if err != nil {
						test.Fatal(err)
					}
					quoted.Write(escaped[1 : len(escaped)-1])
				} else {
					quoted.WriteRune(character)
				}
			}
			quoted.WriteByte('"')
			want = quoted.String()
		}
		var actual strings.Builder
		for index := 0; index < fixture.plan.Pages.Count; index++ {
			page, err := ReadDocToolStringPage(context.Background(), fixture.plan, index, func(_ context.Context, hash ContentHash) ([]byte, error) { return fixture.objects[hash], nil })
			if err != nil {
				test.Fatal(err)
			}
			actual.WriteString(page.Display.Text)
		}
		if actual.String() != want {
			test.Fatal("tool transform differs from oracle")
		}
	})
}
