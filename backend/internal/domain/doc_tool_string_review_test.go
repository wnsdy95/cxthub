package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

type docToolStringReviewFixture struct {
	source  DocToolStringSource
	plan    DocToolStringPlan
	objects map[ContentHash][]byte
	raw     []byte
	states  []DocToolStringState
}

func (fixture docToolStringReviewFixture) load(_ context.Context, hash ContentHash) ([]byte, error) {
	raw, found := fixture.objects[hash]
	if !found {
		return nil, ErrNotFound
	}
	return bytes.Clone(raw), nil
}

func buildDocToolStringReviewFixture(test *testing.T, token string, mode DocToolStringMode) docToolStringReviewFixture {
	test.Helper()
	const prefix = `{"kind":"tool_result","output":`
	raw := []byte(prefix + token + `,"locked":{"blob":"never-public"}}`)
	source := DocToolStringSource{Version: 1, Profile: DocToolStringProfile, EventHash: HashContent(raw), EventBytes: len(raw), Token: DocFindSpan{From: len(prefix), To: len(prefix) + len(token)}, Mode: mode}
	fixture := docToolStringReviewFixture{source: source, raw: raw, objects: make(map[ContentHash][]byte)}
	state, err := NewDocToolStringState(source)
	if err != nil {
		test.Fatal(err)
	}
	put := func(objects []DocRangeObject) {
		for _, object := range objects {
			if len(object.Bytes) == 0 || len(object.Bytes) > MaxDocDisplayPageBytes || HashContent(object.Bytes) != object.Hash {
				test.Fatal("invalid immutable object")
			}
			if prior, found := fixture.objects[object.Hash]; found && !bytes.Equal(prior, object.Bytes) {
				test.Fatal("immutable object changed")
			}
			fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
		}
	}
	for count := 0; !state.Done; count++ {
		if count > 2048 {
			test.Fatal("preparation failed to terminate")
		}
		before, err := state.Encode(source)
		if err != nil {
			test.Fatal(err)
		}
		fixture.states = append(fixture.states, state)
		readBytes := 0
		step, err := StepDocToolString(context.Background(), source, state, func(_ context.Context, from, length int) ([]byte, error) {
			if from < source.Token.From || length <= 0 || from > source.Token.To-length {
				test.Fatalf("read escaped token: from=%d length=%d", from, length)
			}
			readBytes += length
			return bytes.Clone(raw[from : from+length]), nil
		}, fixture.load)
		if err != nil {
			test.Fatal("valid token rejected", count, state.Next, err)
		}
		after, err := state.Encode(source)
		if err != nil || !bytes.Equal(before, after) {
			test.Fatal("step mutated its input checkpoint", err)
		}
		if step.ReadBytes != readBytes || readBytes > DocDisplayStepSourceBytes || step.State.Next <= state.Next {
			test.Fatal("step exceeded read budget or failed to advance", readBytes, step.ReadBytes)
		}
		put(step.Objects)
		encoded, err := step.State.Encode(source)
		if err != nil {
			test.Fatal(err)
		}
		state, err = DecodeDocToolStringState(source, HashContent(encoded), encoded)
		if err != nil {
			test.Fatal("checkpoint could not restart", err)
		}
	}
	fixture.states = append(fixture.states, state)
	plan, objects, err := FinishDocToolString(context.Background(), source, state, fixture.load)
	if err != nil {
		test.Fatal(err)
	}
	put(objects)
	fixture.plan = plan
	return fixture
}

func docToolStringReviewOracle(test *testing.T, fixture docToolStringReviewFixture) *docToolDisplay {
	test.Helper()
	parser, err := newDocFindSourceParser(fixture.raw, fixture.source.Token)
	if err != nil {
		test.Fatal(err)
	}
	builder := docToolDisplayBuilder{ctx: context.Background(), parser: parser, display: docToolDisplay{sourceBytes: len(fixture.raw)}, metadata: docToolDisplayBaseBytes, budget: MaxDocToolDisplayBytes}
	if fixture.source.Mode == DocToolStringQuoted {
		err = builder.string(fixture.source.Token, true, 0)
	} else {
		err = builder.diff(fixture.source.Token, docToolStringPrefix(fixture.source.Mode))
	}
	if err != nil {
		test.Fatal(err)
	}
	builder.display.text = builder.text.String()
	return &builder.display
}

func TestDocToolStringReviewRestartTextAndProvenance(test *testing.T) {
	tokens := []string{
		`""`, `"plain"`, `"\n"`, `"\r\n\n"`, `"\uD800"`, `"\ud801\ufffd\udc00"`,
		`"\uD800\uD800\uDC00"`, `"한🙂\uD83D\uDE00\"\\\/\b\f\n\r\t\u0000\u001f"`,
		"\"<>&\u2028\u2029\"",
		`"` + strings.Repeat(`a\u0062\n\uD800x\uD83D\uDE00`, 600) + `"`,
	}
	for _, length := range []int{254, 255, 256, 257, 510, 511, 512, 1023, 1024, 4095, 4096, 32755, 32756, 32757, 32758, 32767, 32768, 65537} {
		tokens = append(tokens, `"`+strings.Repeat("a", length)+`"`)
	}
	for _, mode := range []DocToolStringMode{DocToolStringQuoted, DocToolStringAdd, DocToolStringRemove} {
		for index, token := range tokens {
			test.Run(fmt.Sprintf("%s/%d", mode, index), func(test *testing.T) {
				fixture := buildDocToolStringReviewFixture(test, token, mode)
				oracle := docToolStringReviewOracle(test, fixture)
				var text strings.Builder
				rawEnd := fixture.source.Token.From
				for pageIndex := 0; pageIndex < fixture.plan.Pages.Count; pageIndex++ {
					page, err := ReadDocToolStringPage(context.Background(), fixture.plan, pageIndex, fixture.load)
					if err != nil || page.Display.From != text.Len() || page.RawFrom != rawEnd {
						test.Fatal("resumed page lost adjacency", pageIndex, err)
					}
					mapped := page.Display
					mapped.Version = 2
					for offset, character := range mapped.Text {
						got, err := mapped.SourceRange(offset, utf8.RuneLen(character))
						want, expectedErr := oracle.SourceRange(mapped.From+offset, utf8.RuneLen(character))
						if err != nil || expectedErr != nil || got != want {
							test.Fatal("rune lost original-byte provenance", offset, got, want, err, expectedErr)
						}
					}
					text.WriteString(page.Display.Text)
					rawEnd = page.RawTo
				}
				if text.String() != oracle.Text() || rawEnd != fixture.source.Token.To {
					test.Fatal("resumption changed text or skipped raw bytes")
				}
			})
		}
	}
}

func TestDocToolStringReviewNativePageSourceRange(test *testing.T) {
	fixture := buildDocToolStringReviewFixture(test, `"a\u0062\n"`, DocToolStringQuoted)
	page, err := ReadDocToolStringPage(context.Background(), fixture.plan, 0, fixture.load)
	if err != nil {
		test.Fatal(err)
	}
	span, err := page.SourceRange(1, 1)
	want := DocFindSpan{From: fixture.source.Token.From + 1, To: fixture.source.Token.From + 2}
	if err != nil || span != want {
		test.Fatalf("valid version-%d native page cannot locate source: got=%v want=%v err=%v", page.Display.Version, span, want, err)
	}
}

func TestDocToolStringReviewSourceRejectsOverflowingSpan(test *testing.T) {
	source := DocToolStringSource{Version: 1, Profile: DocToolStringProfile, EventHash: HashContent([]byte(`"x"`)), EventBytes: 3, Token: DocFindSpan{From: 1, To: math.MinInt}, Mode: DocToolStringQuoted}
	if err := source.Validate(); !errors.Is(err, ErrIntegrity) {
		test.Fatalf("overflowing backwards source span accepted: %+v, err=%v", source.Token, err)
	}
	if _, err := source.Encode(); !errors.Is(err, ErrIntegrity) {
		test.Fatal("invalid source could be serialized", err)
	}
}

func TestDocToolStringReviewCancellationHasNoPartialEffects(test *testing.T) {
	for _, token := range []string{`"x"`, `"` + strings.Repeat(`\u0061`, 700) + `"`} {
		fixture := buildDocToolStringReviewFixture(test, token, DocToolStringQuoted)
		initial, err := NewDocToolStringState(fixture.source)
		if err != nil {
			test.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		step, err := StepDocToolString(ctx, fixture.source, initial, func(_ context.Context, from, length int) ([]byte, error) {
			cancel()
			return bytes.Clone(fixture.raw[from : from+length]), nil
		}, fixture.load)
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(step, DocToolStringStep{}) {
			test.Fatal("cancellation retained partial output", err, step.State)
		}
	}
}

func TestDocToolStringReviewInvalidTokensFailClosed(test *testing.T) {
	for _, token := range []string{`"early"tail"`, `"\q"`, `"\u123"`, `"\uD800\uQQQQ"`, "\"raw\nnewline\"", `"missing`, string([]byte{'"', 0xff, '"'})} {
		raw := []byte(token)
		source := DocToolStringSource{Version: 1, Profile: DocToolStringProfile, EventHash: HashContent(raw), EventBytes: len(raw), Token: DocFindSpan{To: len(raw)}, Mode: DocToolStringQuoted}
		state, err := NewDocToolStringState(source)
		if err != nil {
			test.Fatal(err)
		}
		step, err := StepDocToolString(context.Background(), source, state, func(_ context.Context, from, length int) ([]byte, error) {
			return bytes.Clone(raw[from : from+length]), nil
		}, nil)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(step, DocToolStringStep{}) {
			test.Fatal("malformed token returned a publishable prefix", token, err)
		}
	}
}

func TestDocToolStringReviewAssemblySmallFragmentsPreserveCovers(test *testing.T) {
	for _, mode := range []DocToolStringMode{DocToolStringQuoted, DocToolStringAdd, DocToolStringRemove} {
		fixture := buildDocToolStringReviewFixture(test, `"`+strings.Repeat(`\u0061x\n\uD800🙂\u0000\"`, 110)+`"`, mode)
		oracle := docToolStringReviewOracle(test, fixture)
		input, err := NewDocToolStringAssemblyInput(fixture.plan)
		if err != nil {
			test.Fatal(err)
		}
		for _, limit := range []struct{ bytes, runs int }{{4, 1}, {7, 2}, {511, 3}, {DocDisplayPageTextBytes, DocDisplayPageRuns}} {
			test.Run(fmt.Sprintf("%s/%d/%d", mode, limit.bytes, limit.runs), func(test *testing.T) {
				cursor := bytes.Clone(input.initial)
				var text strings.Builder
				for text.Len() < input.source.TextBytes {
					loads := 0
					page, next, err := input.read(context.Background(), cursor, limit.bytes, limit.runs, func(ctx context.Context, hash ContentHash) ([]byte, error) {
						loads++
						return fixture.load(ctx, hash)
					})
					if err != nil || page.From != text.Len() || len(page.Text) == 0 || len(page.Text) > limit.bytes || len(page.Runs) > limit.runs || loads > DocDisplayMappedReplayObjects {
						test.Fatal("fragment budget or adjacency changed", err, loads)
					}
					for offset, character := range page.Text {
						got, err := page.SourceRange(offset, utf8.RuneLen(character))
						want, expectedErr := oracle.SourceRange(page.From+offset, utf8.RuneLen(character))
						if err != nil || expectedErr != nil || got != want {
							test.Fatal("clipping changed original cover", got, want, err, expectedErr)
						}
					}
					text.WriteString(page.Text)
					cursor = bytes.Clone(next)
				}
				if text.String() != oracle.Text() {
					test.Fatal("fragment replay changed text")
				}
			})
		}
	}
}

func TestDocToolStringReviewLastCheckpointRejectsSkippedRawAndDisplay(test *testing.T) {
	fixture := buildDocToolStringReviewFixture(test, `"`+strings.Repeat(`a\u0062\n\uD800`, 700)+`"`, DocToolStringQuoted)
	if len(fixture.states) < 3 {
		test.Fatal("review fixture did not generate an intermediate checkpoint")
	}
	for _, change := range []struct {
		name   string
		mutate func(*DocToolStringState)
	}{
		{"skip-raw", func(state *DocToolStringState) { state.Next++ }},
		{"rewind-raw", func(state *DocToolStringState) { state.Next-- }},
		{"skip-display", func(state *DocToolStringState) { state.TextBytes++ }},
		{"rewind-display", func(state *DocToolStringState) { state.TextBytes-- }},
	} {
		test.Run(change.name, func(test *testing.T) {
			state := fixture.states[1]
			change.mutate(&state)
			if state.Validate(fixture.source) != nil {
				test.Fatal("test mutation was rejected before hydrated checkpoint validation")
			}
			if err := ValidateDocToolStringCheckpoint(context.Background(), fixture.source, state, fixture.load); !errors.Is(err, ErrIntegrity) {
				test.Fatal("hydrated checkpoint accepted skipped bytes", err)
			}
			rawReads := 0
			step, err := StepDocToolString(context.Background(), fixture.source, state, func(_ context.Context, from, length int) ([]byte, error) {
				rawReads++
				return bytes.Clone(fixture.raw[from : from+length]), nil
			}, fixture.load)
			if !errors.Is(err, ErrIntegrity) || rawReads != 0 || !reflect.DeepEqual(step, DocToolStringStep{}) {
				test.Fatal("invalid checkpoint read new source or retained output", err, rawReads)
			}
		})
	}
	completed := fixture.states[len(fixture.states)-1]
	lastHash := completed.Directory.Levels[0][len(completed.Directory.Levels[0])-1]
	for _, missing := range []bool{false, true} {
		plan, objects, err := FinishDocToolString(context.Background(), fixture.source, completed, func(ctx context.Context, hash ContentHash) ([]byte, error) {
			if hash == lastHash {
				if missing {
					return nil, ErrNotFound
				}
				raw, err := fixture.load(ctx, hash)
				if err != nil {
					return nil, err
				}
				raw[len(raw)-1] ^= 1
				return raw, nil
			}
			return fixture.load(ctx, hash)
		})
		if err == nil || !reflect.DeepEqual(plan, DocToolStringPlan{}) || objects != nil {
			test.Fatal("completion skipped final stored-page validation", err)
		}
	}
}

func TestDocToolStringReviewTypedWrapperRejectsCrossVersionPages(test *testing.T) {
	fixture := buildDocToolStringReviewFixture(test, `"\uD800\n"`, DocToolStringQuoted)
	page, err := ReadDocToolStringPage(context.Background(), fixture.plan, 0, fixture.load)
	if err != nil {
		test.Fatal(err)
	}
	for _, version := range []int{0, 1, 2, 3, 4, 6} {
		invalid := page
		invalid.Display.Version = version
		if _, err := invalid.SourceRange(0, 1); !errors.Is(err, ErrIntegrity) {
			test.Fatal("wrapper accepted a different page format", version, err)
		}
	}
	invalid := page
	invalid.Source = HashContent([]byte("other source"))
	if _, err := invalid.SourceRange(0, 1); !errors.Is(err, ErrIntegrity) {
		test.Fatal("wrapper accepted mismatched inner and outer sources", err)
	}
}

func TestDocToolStringReviewFactoryRejectsMalformedCursorAndCancellation(test *testing.T) {
	fixture := buildDocToolStringReviewFixture(test, `"a🙂\uD800\n"`, DocToolStringRemove)
	input, err := NewDocToolStringAssemblyInput(fixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	for _, cursor := range []docToolStringCursor{
		{Version: 1, Plan: input.source.Plan, Byte: 1},
		{Version: 1, Plan: input.source.Plan, From: 1, Byte: 1},
		{Version: 1, Plan: HashContent([]byte("other plan"))},
		{Version: 1, Plan: input.source.Plan, Page: 1},
		{Version: 1, Plan: input.source.Plan, Page: fixture.plan.Pages.Count, From: input.source.TextBytes, Byte: 1},
	} {
		raw, err := canonicalJSON(cursor)
		if err != nil {
			test.Fatal(err)
		}
		page, next, err := input.read(context.Background(), raw, 4, 1, fixture.load)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(page, DocDisplayPage{}) || next != nil {
			test.Fatal("invalid cursor returned replay output", cursor, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	page, next, err := input.read(ctx, input.initial, 4, 1, func(ctx context.Context, hash ContentHash) ([]byte, error) {
		cancel()
		return fixture.load(ctx, hash)
	})
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(page, DocDisplayPage{}) || next != nil {
		test.Fatal("factory cancellation retained a partial cursor/page", err)
	}
}
