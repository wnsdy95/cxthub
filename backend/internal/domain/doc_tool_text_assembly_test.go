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

func newDocToolTextAssemblyFixture(test *testing.T, token string) decodedTextFixture {
	test.Helper()
	prefix := `{"kind":"tool_call","input":`
	raw := []byte(prefix + token + `,"opaque":"not displayed"}`)
	source := DocDecodedTextSource{Version: 1, Profile: DocDecodedTextProfile, EventHash: HashContent(raw), EventBytes: len(raw), Token: DocFindSpan{From: len(prefix), To: len(prefix) + len(token)}}
	return buildDecodedTextSourceFixture(test, raw, source)
}

func docToolTextAssemblyOriginalSpan(test *testing.T, fixture decodedTextFixture, from, length int) DocFindSpan {
	test.Helper()
	for _, page := range fixture.pages {
		if from >= page.From && from+length <= page.From+len(page.Text) {
			span, err := page.SourceRange(from-page.From, length)
			if err != nil {
				test.Fatal(err)
			}
			return span
		}
	}
	test.Fatal("missing original decoded coordinates", from, length)
	return DocFindSpan{}
}

func assertDocToolTextAssemblyReplay(test *testing.T, input DocDisplayAssemblyInput, fixture decodedTextFixture, skip, maxBytes, maxRuns int) {
	test.Helper()
	load := func(_ context.Context, hash ContentHash) ([]byte, error) {
		if hash == fixture.source.EventHash {
			test.Fatal("assembly attempted a raw event read")
		}
		body, found := fixture.objects[hash]
		if !found {
			return nil, ErrNotFound
		}
		return body, nil
	}
	want, err := ReadDocDecodedTextBytes(context.Background(), fixture.generation, skip, min(fixture.generation.TextBytes-skip, 4096), load)
	if err != nil {
		test.Fatal(err)
	}
	var expected strings.Builder
	for _, page := range fixture.pages {
		if page.From+len(page.Text) > skip {
			expected.WriteString(page.Text[max(0, skip-page.From):])
		}
	}
	if !strings.HasPrefix(expected.String(), string(want)) {
		test.Fatal("invalid decoded fixture")
	}
	initial := bytes.Clone(input.initial)
	cursor := bytes.Clone(initial)
	var output strings.Builder
	hash, err := input.source.Hash()
	if err != nil || input.validate() != nil {
		test.Fatal(err)
	}
	for output.Len() < input.source.TextBytes {
		prior := bytes.Clone(cursor)
		loads := 0
		page, next, err := input.read(context.Background(), cursor, maxBytes, maxRuns, func(ctx context.Context, hash ContentHash) ([]byte, error) {
			loads++
			return load(ctx, hash)
		})
		position, positionErr := input.cursor(next)
		if err != nil || positionErr != nil || len(page.Text) == 0 || len(page.Text) > maxBytes || len(page.Runs) > maxRuns || loads > DocDisplayMappedReplayObjects || page.Version != 2 || page.Source != hash || page.From != output.Len() || position != output.Len()+len(page.Text) || page.validateCoordinates(input.source.EventBytes, true) != nil {
			test.Fatal("invalid bounded text replay", page, loads, position, err, positionErr)
		}
		if !bytes.Equal(prior, cursor) || !bytes.Equal(initial, input.initial) || len(next) > 512 || bytes.Contains(next, []byte("PUBLIC_BODY_MARKER")) {
			test.Fatal("cursor was mutated, unbounded, or retained rendered text")
		}
		for offset, character := range page.Text {
			span, err := page.SourceRange(offset, utf8.RuneLen(character))
			want := docToolTextAssemblyOriginalSpan(test, fixture, skip+page.From+offset, utf8.RuneLen(character))
			if err != nil || span != want {
				test.Fatal("decoded original span changed", span, want, err)
			}
		}
		output.WriteString(page.Text)
		cursor = next
	}
	if output.String() != expected.String() {
		test.Fatal("decoded assembly changed text", output.String(), expected.String())
	}
	position, err := input.cursor(cursor)
	if err != nil || position != input.source.TextBytes {
		test.Fatal("terminal cursor invalid", position, err)
	}
	loads := 0
	page, next, err := input.read(context.Background(), cursor, 1, 1, func(context.Context, ContentHash) ([]byte, error) { loads++; return nil, ErrNotFound })
	if !errors.Is(err, ErrIntegrity) || loads != 0 || !reflect.DeepEqual(page, DocDisplayPage{}) || next != nil {
		test.Fatal("terminal read produced output or performed I/O", loads, err)
	}
}

func TestDocToolLiteralAssemblyFixedAnchorAndBudgets(test *testing.T) {
	event := HashContent([]byte("original tool event"))
	anchor := DocFindSpan{From: 7, To: 19}
	for _, text := range []string{":", "\n", " / ", "12345678", "\ud55c🙂"} {
		test.Run(text, func(test *testing.T) {
			input, err := newDocToolLiteralAssemblyInput(event, 32, text, anchor)
			if err != nil || input.validate() != nil || input.Source().EventHash != event || input.Source().EventBytes != 32 || input.Source().TextBytes != len(text) {
				test.Fatal("invalid literal input", err)
			}
			cursor := bytes.Clone(input.initial)
			var output strings.Builder
			budget := 1
			if utf8.RuneCountInString(text) != len(text) {
				budget = 4
			}
			for output.Len() < len(text) {
				page, next, err := input.read(context.Background(), cursor, budget, 1, func(context.Context, ContentHash) ([]byte, error) {
					test.Fatal("literal performed I/O")
					return nil, ErrNotFound
				})
				position, cursorErr := input.cursor(next)
				if err != nil || cursorErr != nil || page.From != output.Len() || len(page.Text) == 0 || len(page.Text) > budget || len(page.Runs) != 1 || page.Runs[0].Linear || position != output.Len()+len(page.Text) {
					test.Fatal("literal budget or cursor changed", page, err, cursorErr)
				}
				for offset, character := range page.Text {
					span, err := page.SourceRange(offset, utf8.RuneLen(character))
					if err != nil || span != anchor {
						test.Fatal("literal clipped its original anchor", span, anchor, err)
					}
				}
				output.WriteString(page.Text)
				cursor = next
			}
			if output.String() != text {
				test.Fatal("literal text changed", output.String())
			}
			page, next, err := input.read(context.Background(), cursor, 1, 1, func(context.Context, ContentHash) ([]byte, error) { return nil, ErrNotFound })
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(page, DocDisplayPage{}) || next != nil {
				test.Fatal("terminal literal read returned output", err)
			}
		})
	}
}

func TestDocToolTextAssemblyRejectsInvalidConstructors(test *testing.T) {
	event := HashContent(nil)
	for _, invalid := range []struct {
		event      ContentHash
		eventBytes int
		text       string
		anchor     DocFindSpan
	}{
		{event, 20, "", DocFindSpan{From: 1, To: 2}},
		{event, 20, "123456789", DocFindSpan{From: 1, To: 2}},
		{event, 20, "\ud55c\ud55c\ud55c", DocFindSpan{From: 1, To: 2}},
		{event, 20, string([]byte{0xff}), DocFindSpan{From: 1, To: 2}},
		{"invalid", 20, ":", DocFindSpan{From: 1, To: 2}},
		{event, 0, ":", DocFindSpan{From: 0, To: 1}},
		{event, MaxConversationDocumentBytes + 1, ":", DocFindSpan{From: 1, To: 2}},
		{event, 20, ":", DocFindSpan{From: -1, To: 2}},
		{event, 20, ":", DocFindSpan{From: 1, To: 1}},
		{event, 20, ":", DocFindSpan{From: 2, To: 1}},
		{event, 20, ":", DocFindSpan{From: 1, To: 21}},
	} {
		input, err := newDocToolLiteralAssemblyInput(invalid.event, invalid.eventBytes, invalid.text, invalid.anchor)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(input, DocDisplayAssemblyInput{}) {
			test.Fatal("invalid literal accepted", invalid, err)
		}
	}
	fixture := newDocToolTextAssemblyFixture(test, `"abc"`)
	for _, skip := range []int{-1, fixture.generation.TextBytes + 1, math.MaxInt} {
		input, err := newDocToolDecodedAssemblyInput(fixture.generation, skip)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(input, DocDisplayAssemblyInput{}) {
			test.Fatal("invalid decoded skip accepted", skip, err)
		}
	}
	invalid := fixture.generation
	invalid.Pages.Count++
	if input, err := newDocToolDecodedAssemblyInput(invalid, 0); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(input, DocDisplayAssemblyInput{}) {
		test.Fatal("invalid decoded generation accepted", err)
	}
}

func TestDocToolDecodedAssemblyExactEscapesSurrogatesAndSpans(test *testing.T) {
	for _, token := range []string{`"PUBLIC_BODY_MARKER **not markdown**"`, `" \t\n\r\u00a0 "`, `"a\"b\\c\/d\b\f\n\r\t"`, `"\uD83D\uDE42\uD800X\uDC00\u0000"`, "\"\ud55c\uae00🙂<>&\\u2028\\u2029\""} {
		fixture := newDocToolTextAssemblyFixture(test, token)
		var text string
		if json.Unmarshal([]byte(token), &text) != nil {
			test.Fatal("invalid token fixture")
		}
		for skip := range text {
			for _, limits := range [][2]int{{4, 1}, {7, 2}, {DocDisplayPageTextBytes, DocDisplayPageRuns}} {
				input, err := newDocToolDecodedAssemblyInput(fixture.generation, skip)
				if err != nil {
					test.Fatal(err)
				}
				assertDocToolTextAssemblyReplay(test, input, fixture, skip, limits[0], limits[1])
			}
		}
	}
}

func TestDocToolDecodedAssemblyLateSkippedPathUsesBoundedLookup(test *testing.T) {
	prefix := strings.Repeat(`\u0061`, 70000) + `/`
	token := `"` + prefix + strings.Repeat(`\u0062`, 1100) + "/\ud55c\\ud83d\\ude42.go\""
	fixture := newDocToolTextAssemblyFixture(test, token)
	if len(fixture.pages) <= DocRangeDirectoryFanout || fixture.generation.Lookup.Count < 2 {
		test.Fatal("fixture did not cross page and lookup directories")
	}
	boundary := fixture.pages[len(fixture.pages)-3].From
	for _, skip := range []int{70001, boundary - 1, boundary, boundary + 1} {
		input, err := newDocToolDecodedAssemblyInput(fixture.generation, skip)
		if err != nil {
			test.Fatal(err)
		}
		bucketStart := skip / DocDecodedTextLookupStride * DocDecodedTextLookupStride
		forbidden := make(map[ContentHash]bool)
		for _, page := range fixture.pages {
			if page.From+len(page.Text) <= bucketStart {
				raw, err := encodeDocDecodedTextPage(fixture.source, page)
				if err != nil {
					test.Fatal(err)
				}
				forbidden[HashContent(raw)] = true
			}
		}
		cursor := bytes.Clone(input.initial)
		var output strings.Builder
		maxLoads := 0
		for output.Len() < input.source.TextBytes {
			loads := 0
			page, next, err := input.read(context.Background(), cursor, DocDisplayPageTextBytes, DocDisplayPageRuns, func(_ context.Context, hash ContentHash) ([]byte, error) {
				loads++
				if forbidden[hash] || hash == fixture.source.EventHash {
					test.Fatal("late skip replayed an earlier decoded page or read the raw event")
				}
				return fixture.objects[hash], nil
			})
			if err != nil || len(page.Text) == 0 || loads > DocDisplayMappedReplayObjects {
				test.Fatal("late path lookup failed", loads, err)
			}
			for offset, character := range page.Text {
				span, err := page.SourceRange(offset, utf8.RuneLen(character))
				want := docToolTextAssemblyOriginalSpan(test, fixture, skip+output.Len()+offset, utf8.RuneLen(character))
				if err != nil || span != want {
					test.Fatal("late path source mapping changed", span, want, err)
				}
			}
			maxLoads = max(maxLoads, loads)
			output.WriteString(page.Text)
			cursor = next
		}
		var text string
		if json.Unmarshal([]byte(token), &text) != nil || output.String() != text[skip:] {
			test.Fatal("late path abbreviation changed text")
		}
		test.Logf("skip=%d original_pages=%d max_loads=%d", skip, len(fixture.pages), maxLoads)
	}
}

func TestDocToolDecodedAssemblyEmptyAndAllSkippedTerminalInitial(test *testing.T) {
	for _, token := range []string{`""`, `"abc"`, `"\ud83d\ude42"`} {
		fixture := newDocToolTextAssemblyFixture(test, token)
		input, err := newDocToolDecodedAssemblyInput(fixture.generation, fixture.generation.TextBytes)
		if err != nil || input.validate() != nil || input.source.TextBytes != 0 {
			test.Fatal("empty decoded child rejected", err)
		}
		assertDocToolTextAssemblyReplay(test, input, fixture, fixture.generation.TextBytes, 1, 1)
		state, err := NewDocDisplayAssemblyState(input)
		if err != nil || !state.Output.Done {
			test.Fatal("empty assembly did not start terminal", err)
		}
		generation, objects, err := FinishDocDisplayAssembly(context.Background(), input, state, nil)
		if err != nil || generation.Pages.Count != 0 || len(objects) != 0 {
			test.Fatal("empty assembly failed without object reads", err)
		}
	}
}

func TestDocToolTextAssemblyExactSmallByteAndRunBudgets(test *testing.T) {
	for _, scenario := range []struct {
		token    string
		skip     int
		maxBytes int
		maxRuns  int
		want     string
	}{
		{`"abc"`, 0, 1, 1, "a"},
		{`"abc"`, 0, 2, 1, "ab"},
		{`"abc"`, 0, 3, 1, "abc"},
		{`"abc"`, 2, 1, 1, "c"},
		{`"abcdef"`, 2, 3, 1, "cde"},
		{`"a\u0062\u0063d"`, 0, 4, 1, "a"},
		{`"a\u0062\u0063d"`, 0, 4, 2, "ab"},
		{`"a\u0062\u0063d"`, 0, 4, 3, "abc"},
		{`"a\u0062\u0063d"`, 0, 4, 4, "abcd"},
		{`"a🙂"`, 0, 2, 1, "a"},
		{`"a🙂"`, 1, 4, 1, "🙂"},
	} {
		fixture := newDocToolTextAssemblyFixture(test, scenario.token)
		input, err := newDocToolDecodedAssemblyInput(fixture.generation, scenario.skip)
		if err != nil {
			test.Fatal(err)
		}
		page, next, err := input.read(context.Background(), input.initial, scenario.maxBytes, scenario.maxRuns, func(_ context.Context, hash ContentHash) ([]byte, error) { return fixture.objects[hash], nil })
		position, cursorErr := input.cursor(next)
		if err != nil || cursorErr != nil || page.Text != scenario.want || len(page.Runs) > scenario.maxRuns || position != len(scenario.want) {
			test.Fatal("small exact budget changed", scenario, page, err, cursorErr)
		}
	}
	fixture := newDocToolTextAssemblyFixture(test, `"\ud83d\ude42x"`)
	decoded, err := newDocToolDecodedAssemblyInput(fixture.generation, 0)
	if err != nil {
		test.Fatal(err)
	}
	literal, err := newDocToolLiteralAssemblyInput(fixture.source.EventHash, fixture.source.EventBytes, "🙂x", fixture.source.Token)
	if err != nil {
		test.Fatal(err)
	}
	load := func(_ context.Context, hash ContentHash) ([]byte, error) { return fixture.objects[hash], nil }
	for _, input := range []DocDisplayAssemblyInput{decoded, literal} {
		for budget := 1; budget < 4; budget++ {
			page, next, err := input.read(context.Background(), input.initial, budget, 1, load)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(page, DocDisplayPage{}) || next != nil {
				test.Fatal("UTF-8 split retained partial output", budget, err)
			}
		}
		for _, limits := range [][2]int{{0, 1}, {-1, 1}, {DocDisplayPageTextBytes + 1, 1}, {4, 0}, {4, -1}, {4, DocDisplayPageRuns + 1}} {
			loads := 0
			page, next, err := input.read(context.Background(), input.initial, limits[0], limits[1], func(context.Context, ContentHash) ([]byte, error) { loads++; return nil, ErrNotFound })
			if !errors.Is(err, ErrIntegrity) || loads != 0 || !reflect.DeepEqual(page, DocDisplayPage{}) || next != nil {
				test.Fatal("invalid budgets performed I/O or produced output", limits, loads, err)
			}
		}
		page, next, err := input.read(context.Background(), input.initial, 4, 1, nil)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(page, DocDisplayPage{}) || next != nil {
			test.Fatal("nil loader accepted", err)
		}
	}
	for skip := 1; skip < 4; skip++ {
		input, err := newDocToolDecodedAssemblyInput(fixture.generation, skip)
		if err != nil {
			test.Fatal(err)
		}
		page, next, err := input.read(context.Background(), input.initial, 4, 1, load)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(page, DocDisplayPage{}) || next != nil {
			test.Fatal("skip inside a UTF-8 rune accepted", skip, err)
		}
	}
}

func TestDocToolTextAssemblyDescriptorsBindSourceAndPlan(test *testing.T) {
	fixture := newDocToolTextAssemblyFixture(test, `"same"`)
	input, err := newDocToolDecodedAssemblyInput(fixture.generation, 0)
	if err != nil {
		test.Fatal(err)
	}
	again, err := newDocToolDecodedAssemblyInput(fixture.generation, 0)
	if err != nil || again.Source() != input.Source() || !bytes.Equal(again.initial, input.initial) {
		test.Fatal("decoded descriptor is not deterministic", err)
	}
	otherFixture := newDocToolTextAssemblyFixture(test, `"\u0073ame"`)
	otherSource, _ := newDocToolDecodedAssemblyInput(otherFixture.generation, 0)
	otherSkip, _ := newDocToolDecodedAssemblyInput(fixture.generation, 1)
	changedGeneration := fixture.generation
	changedGeneration.Hash = HashContent([]byte("different generation"))
	otherGeneration, _ := newDocToolDecodedAssemblyInput(changedGeneration, 0)
	changedGeneration = fixture.generation
	changedGeneration.Pages.Hash = HashContent([]byte("different pages"))
	otherPages, _ := newDocToolDecodedAssemblyInput(changedGeneration, 0)
	changedGeneration = fixture.generation
	changedGeneration.Lookup.Hash = HashContent([]byte("different lookup"))
	otherLookup, _ := newDocToolDecodedAssemblyInput(changedGeneration, 0)
	for _, other := range []DocDisplayAssemblyInput{otherSource, otherSkip, otherGeneration, otherPages, otherLookup} {
		if other.Source().Plan == input.Source().Plan {
			test.Fatal("decoded plan omitted generation or skip")
		}
		loads := 0
		page, next, err := other.read(context.Background(), input.initial, 4, 1, func(context.Context, ContentHash) ([]byte, error) { loads++; return nil, ErrNotFound })
		if !errors.Is(err, ErrIntegrity) || loads != 0 || !reflect.DeepEqual(page, DocDisplayPage{}) || next != nil {
			test.Fatal("foreign decoded cursor performed I/O or produced output", loads, err)
		}
	}
	literal, _ := newDocToolLiteralAssemblyInput(fixture.source.EventHash, fixture.source.EventBytes, ":", fixture.source.Token)
	for _, scenario := range []struct {
		event      ContentHash
		eventBytes int
		text       string
		anchor     DocFindSpan
	}{
		{HashContent(nil), fixture.source.EventBytes, ":", fixture.source.Token},
		{fixture.source.EventHash, fixture.source.EventBytes + 1, ":", fixture.source.Token},
		{fixture.source.EventHash, fixture.source.EventBytes, ",", fixture.source.Token},
		{fixture.source.EventHash, fixture.source.EventBytes, ":", DocFindSpan{From: fixture.source.Token.From + 1, To: fixture.source.Token.To}},
	} {
		other, err := newDocToolLiteralAssemblyInput(scenario.event, scenario.eventBytes, scenario.text, scenario.anchor)
		if err != nil || other.Source().Plan == literal.Source().Plan {
			test.Fatal("literal plan omitted source, text, or anchor", err)
		}
		if _, err := other.cursor(literal.initial); !errors.Is(err, ErrIntegrity) {
			test.Fatal("foreign literal cursor accepted", err)
		}
	}
}

func TestDocToolDecodedAssemblyCursorTamperingAndCanonicality(test *testing.T) {
	fixture := newDocToolTextAssemblyFixture(test, `"a\ud83d\ude42z"`)
	input, _ := newDocToolDecodedAssemblyInput(fixture.generation, 1)
	var initial docToolDecodedAssemblyCursor
	if json.Unmarshal(input.initial, &initial) != nil {
		test.Fatal("invalid initial cursor")
	}
	for name, mutate := range map[string]func(*docToolDecodedAssemblyCursor){
		"version":         func(cursor *docToolDecodedAssemblyCursor) { cursor.Version++ },
		"source":          func(cursor *docToolDecodedAssemblyCursor) { cursor.Source = HashContent(nil) },
		"plan":            func(cursor *docToolDecodedAssemblyCursor) { cursor.Plan = HashContent(nil) },
		"negative-from":   func(cursor *docToolDecodedAssemblyCursor) { cursor.From = -1 },
		"past-end":        func(cursor *docToolDecodedAssemblyCursor) { cursor.From = input.source.TextBytes + 1 },
		"negative-offset": func(cursor *docToolDecodedAssemblyCursor) { cursor.Offset = -1 },
		"overflow-offset": func(cursor *docToolDecodedAssemblyCursor) { cursor.Offset = math.MaxInt },
		"offset-mismatch": func(cursor *docToolDecodedAssemblyCursor) { cursor.Offset++ },
		"from-mismatch":   func(cursor *docToolDecodedAssemblyCursor) { cursor.From++ },
		"early-terminal":  func(cursor *docToolDecodedAssemblyCursor) { cursor.Done = true },
		"missing-terminal": func(cursor *docToolDecodedAssemblyCursor) {
			cursor.From = input.source.TextBytes
			cursor.Offset = fixture.generation.TextBytes
		},
	} {
		test.Run(name, func(test *testing.T) {
			cursor := initial
			mutate(&cursor)
			raw, err := canonicalJSON(cursor)
			if err != nil {
				test.Fatal(err)
			}
			if _, err := input.cursor(raw); !errors.Is(err, ErrIntegrity) {
				test.Fatal("tampered cursor accepted", err)
			}
			loads := 0
			page, next, err := input.read(context.Background(), raw, 4, 1, func(context.Context, ContentHash) ([]byte, error) { loads++; return nil, ErrNotFound })
			if !errors.Is(err, ErrIntegrity) || loads != 0 || !reflect.DeepEqual(page, DocDisplayPage{}) || next != nil {
				test.Fatal("tampered cursor produced output or read objects", loads, err)
			}
		})
	}
	for _, raw := range [][]byte{nil, []byte("null"), bytes.Repeat([]byte(" "), MaxDocDisplaySourceBytes+1), append(bytes.Clone(input.initial), ' '), append([]byte(`{"unknown":true,`), input.initial[1:]...), append([]byte(`{"version":1,`), input.initial[1:]...), []byte(strings.Replace(string(input.initial), `"version":1`, `"version":1.0`, 1))} {
		if _, err := input.cursor(raw); !errors.Is(err, ErrIntegrity) {
			test.Fatal("noncanonical cursor accepted", string(raw), err)
		}
	}
	inside := initial
	inside.From, inside.Offset = 1, 2
	raw, _ := canonicalJSON(inside)
	page, next, err := input.read(context.Background(), raw, 4, 1, func(_ context.Context, hash ContentHash) ([]byte, error) { return fixture.objects[hash], nil })
	if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(page, DocDisplayPage{}) || next != nil {
		test.Fatal("cursor within UTF-8 rune returned output", err)
	}
}

func TestDocToolTextAssemblyCancellationFaultsAndAliases(test *testing.T) {
	fixture := newDocToolTextAssemblyFixture(test, `"PUBLIC_BODY_MARKER\n\ud83d\ude42"`)
	generation := fixture.generation
	input, _ := newDocToolDecodedAssemblyInput(generation, 1)
	initial := bytes.Clone(input.initial)
	loadCount := 0
	load := func(_ context.Context, hash ContentHash) ([]byte, error) { return fixture.objects[hash], nil }
	page, next, err := input.read(context.Background(), initial, 4, 1, func(ctx context.Context, hash ContentHash) ([]byte, error) { loadCount++; return load(ctx, hash) })
	if err != nil || loadCount == 0 {
		test.Fatal(err)
	}
	wantPage, wantNext := page, bytes.Clone(next)
	wantPage.Runs = append([]DocDisplayRun(nil), page.Runs...)
	fault := errors.New("selected prepared object failed")
	for failAt := 1; failAt <= loadCount; failAt++ {
		for _, corrupt := range []bool{false, true} {
			calls := 0
			page, next, err := input.read(context.Background(), initial, 4, 1, func(ctx context.Context, hash ContentHash) ([]byte, error) {
				calls++
				if calls == failAt {
					if corrupt {
						return append(bytes.Clone(fixture.objects[hash]), ' '), nil
					}
					return nil, fault
				}
				return load(ctx, hash)
			})
			want := fault
			if corrupt {
				want = ErrIntegrity
			}
			if !errors.Is(err, want) || !reflect.DeepEqual(page, DocDisplayPage{}) || next != nil || !bytes.Equal(initial, input.initial) {
				test.Fatal("failed object retained output or mutated input", failAt, corrupt, err)
			}
		}
	}
	literal, _ := newDocToolLiteralAssemblyInput(fixture.source.EventHash, fixture.source.EventBytes, ":", fixture.source.Token)
	for cancelAt := 0; cancelAt <= loadCount; cancelAt++ {
		ctx, cancel := context.WithCancel(context.Background())
		if cancelAt == 0 {
			cancel()
			page, next, err := literal.read(ctx, literal.initial, 1, 1, load)
			if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(page, DocDisplayPage{}) || next != nil {
				test.Fatal("cancelled literal retained output", err)
			}
		}
		calls := 0
		page, next, err := input.read(ctx, initial, 4, 1, func(ctx context.Context, hash ContentHash) ([]byte, error) {
			calls++
			if calls == cancelAt {
				cancel()
			}
			return load(ctx, hash)
		})
		cancel()
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(page, DocDisplayPage{}) || next != nil || cancelAt == 0 && calls != 0 {
			test.Fatal("cancelled decoded read retained output", cancelAt, calls, err)
		}
	}
	page.Runs[0].SourceFrom = 0
	next[0] = '['
	generation.Version, generation.TextBytes, generation.Pages.Count, generation.Lookup.Count = 99, 0, 0, 0
	generation.Source.Token.From++
	returnedSource := input.Source()
	returnedSource.Plan = HashContent(nil)
	input.initial[0] = '['
	again, cursor, err := input.read(context.Background(), initial, 4, 1, load)
	if err != nil || !reflect.DeepEqual(again, wantPage) || !bytes.Equal(cursor, wantNext) {
		test.Fatal("assembly retained a mutable input, page, or cursor alias", err)
	}
}

func TestDocToolTextAssemblyIntegratesWithSharedAssembler(test *testing.T) {
	for _, token := range []string{`""`, `"abc"`, `"a🙂"`, `"` + strings.Repeat(`\u0061\ud83d\ude42\n`, 400) + `"`} {
		fixture := newDocToolTextAssemblyFixture(test, token)
		for _, skip := range []int{0, fixture.generation.TextBytes} {
			test.Run(fmt.Sprintf("bytes-%d-skip-%d", fixture.generation.TextBytes, skip), func(test *testing.T) {
				input, err := newDocToolDecodedAssemblyInput(fixture.generation, skip)
				if err != nil {
					test.Fatal(err)
				}
				assembled := assembleDisplayFixture(test, input, fixture.objects)
				actual := assembledFixturePages(test, assembled, func(page DocDisplayPage) {
					for offset, character := range page.Text {
						span, err := page.SourceRange(offset, utf8.RuneLen(character))
						want := docToolTextAssemblyOriginalSpan(test, fixture, skip+page.From+offset, utf8.RuneLen(character))
						if err != nil || span != want {
							test.Fatal("shared assembler changed original spans", span, want, err)
						}
					}
				})
				var expected string
				if json.Unmarshal([]byte(token), &expected) != nil || actual != expected[skip:] {
					test.Fatal("shared assembler changed decoded text")
				}
			})
		}
	}
	event := HashContent([]byte("literal assembly"))
	anchor := DocFindSpan{From: 3, To: 6}
	input, err := newDocToolLiteralAssemblyInput(event, 10, " / ", anchor)
	if err != nil {
		test.Fatal(err)
	}
	assembled := assembleDisplayFixture(test, input, make(map[ContentHash][]byte))
	actual := assembledFixturePages(test, assembled, func(page DocDisplayPage) {
		span, err := page.SourceRange(0, len(page.Text))
		if err != nil || span != anchor {
			test.Fatal("shared assembler changed literal anchor", span, err)
		}
	})
	if actual != " / " {
		test.Fatal("shared assembler changed literal")
	}
}
