package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func preparedPageOverlapTestPage() DocDisplayPage {
	return DocDisplayPage{Version: 2, Text: "ab한🙂cde끝z", Runs: []DocDisplayRun{
		{To: 2, SourceFrom: 100, SourceTo: 102, Linear: true},
		{To: 5, SourceFrom: 60, SourceTo: 66},
		{To: 9, SourceFrom: 400, SourceTo: 412},
		{To: 12, SourceFrom: 20, SourceTo: 23, Linear: true},
		{To: 15, SourceFrom: 300, SourceTo: 306},
		{To: 16, SourceFrom: 5, SourceTo: 6, Linear: true},
	}}
}

func newPreparedPageOverlapTestInput(test testing.TB, pages ...DocDisplayPage) DocDisplayAssemblyInput {
	test.Helper()
	source := DocDisplayProjectionSource{Version: 1, Profile: DocDisplayProjectionProfile, EventHash: HashContent([]byte("prepared page event")), EventBytes: 4096, Plan: HashContent([]byte("prepared page plan"))}
	prepared := append([]DocDisplayPage(nil), pages...)
	for index := range prepared {
		prepared[index].Version, prepared[index].Index, prepared[index].From = 2, index, source.TextBytes
		source.TextBytes += len(prepared[index].Text)
	}
	input, err := newDocPreparedPageAssemblyInput(source, len(prepared), 1, func(_ context.Context, index int, _ func(context.Context, ContentHash) ([]byte, error)) (DocDisplayPage, error) {
		if index < 0 || index >= len(prepared) {
			return DocDisplayPage{}, ErrIntegrity
		}
		return prepared[index], nil
	})
	if err != nil {
		test.Fatal(err)
	}
	return input
}

func preparedPageOverlapTestEncode(test testing.TB, cursor docPreparedPageCursor) json.RawMessage {
	test.Helper()
	raw, err := canonicalJSON(cursor)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func preparedPageOverlapTestLoad(context.Context, ContentHash) ([]byte, error) {
	return nil, ErrNotFound
}

func assertPreparedPageOverlapTestCover(test testing.TB, display, page DocDisplayPage, start int) {
	test.Helper()
	for offset, character := range page.Text {
		for end := offset + utf8.RuneLen(character); end <= len(page.Text); end++ {
			if end < len(page.Text) && !utf8.RuneStart(page.Text[end]) {
				continue
			}
			got, err := page.SourceRange(offset, end-offset)
			want, failure := display.SourceRange(start+offset, end-offset)
			if err != nil || failure != nil || got != want {
				test.Fatal("fragment source cover changed", start, offset, end, got, want, err, failure)
			}
		}
	}
}

func TestDocPreparedPageOverlapBoundariesAndNonmonotonicCover(test *testing.T) {
	display := preparedPageOverlapTestPage()
	input := newPreparedPageOverlapTestInput(test, display)
	source, err := input.source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	for _, scenario := range []struct {
		name     string
		from     int
		maxBytes int
		maxRuns  int
		text     string
	}{
		{"first-byte", 0, 1, 1, "a"},
		{"inside-linear", 1, 1, 1, "b"},
		{"before-four-byte-rune", 1, 6, 2, "b한"},
		{"exact-run-boundary", 2, 7, 1, "한"},
		{"four-byte-rune", 5, 7, 1, "🙂"},
		{"mapped-to-linear", 5, 7, 2, "🙂cde"},
		{"clipped-linear-end", 9, 2, 1, "cd"},
		{"clipped-linear-start", 10, 6, DocDisplayPageRuns, "de끝z"},
		{"run-limit-before-last", 12, 4, 1, "끝"},
		{"last-byte", 15, 1, 1, "z"},
		{"clipped-multiple-runs", 1, DocDisplayPageTextBytes, 4, "b한🙂cde"},
		{"whole-page", 0, DocDisplayPageTextBytes, DocDisplayPageRuns, display.Text},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			cursor := docPreparedPageCursor{Version: 1, Plan: input.source.Plan, Byte: scenario.from, From: scenario.from}
			raw := preparedPageOverlapTestEncode(test, cursor)
			before := bytes.Clone(raw)
			page, next, err := input.read(context.Background(), raw, scenario.maxBytes, scenario.maxRuns, preparedPageOverlapTestLoad)
			if err != nil || page.Version != 2 || page.Source != source || page.From != scenario.from || page.Text != scenario.text || len(page.Runs) > scenario.maxRuns || len(page.Text) > scenario.maxBytes || !bytes.Equal(raw, before) {
				test.Fatal("incorrect bounded overlap", page, err)
			}
			assertPreparedPageOverlapTestCover(test, display, page, scenario.from)
			cursor.Byte += len(page.Text)
			cursor.From += len(page.Text)
			if cursor.Byte == len(display.Text) {
				cursor.Page, cursor.Byte = 1, 0
			}
			if !bytes.Equal(next, preparedPageOverlapTestEncode(test, cursor)) {
				test.Fatal("continuation changed", string(next), cursor)
			}
		})
	}
}

func TestDocPreparedPageOverlapDenseLateRuns(test *testing.T) {
	display := DocDisplayPage{Version: 2, Text: strings.Repeat("x", DocDisplayPageRuns)}
	for index := 0; index < DocDisplayPageRuns; index++ {
		anchor := 2 * (DocDisplayPageRuns - index)
		display.Runs = append(display.Runs, DocDisplayRun{To: index + 1, SourceFrom: anchor, SourceTo: anchor + 1, Linear: true})
	}
	input := newPreparedPageOverlapTestInput(test, display)
	for _, start := range []int{0, 1, DocDisplayPageRuns / 2, DocDisplayPageRuns - 2, DocDisplayPageRuns - 1} {
		raw := preparedPageOverlapTestEncode(test, docPreparedPageCursor{Version: 1, Plan: input.source.Plan, Byte: start, From: start})
		page, next, err := input.read(context.Background(), raw, 3, 2, preparedPageOverlapTestLoad)
		width := min(2, len(display.Text)-start)
		position, cursorErr := input.cursor(next)
		if err != nil || cursorErr != nil || page.From != start || page.Text != display.Text[start:start+width] || len(page.Runs) != width || position != start+width {
			test.Fatal("late run selection changed", start, page, err, cursorErr)
		}
		assertPreparedPageOverlapTestCover(test, display, page, start)
	}
}

func TestDocPreparedPageOverlapPageTransitions(test *testing.T) {
	first := preparedPageOverlapTestPage()
	last := DocDisplayPage{Version: 2, Text: "尾xy", Runs: []DocDisplayRun{{To: 3, SourceFrom: 900, SourceTo: 906}, {To: 5, SourceFrom: 100, SourceTo: 102, Linear: true}}}
	input := newPreparedPageOverlapTestInput(test, first, last)
	cursor := docPreparedPageCursor{Version: 1, Plan: input.source.Plan, Byte: len(first.Text) - 1, From: len(first.Text) - 1}
	for _, expected := range []struct {
		text string
		page int
		byte int
		from int
	}{
		{"z", 1, 0, len(first.Text)},
		{"尾", 1, 3, len(first.Text) + 3},
		{"xy", 2, 0, input.source.TextBytes},
	} {
		raw := preparedPageOverlapTestEncode(test, cursor)
		page, next, err := input.read(context.Background(), raw, 3, DocDisplayPageRuns, preparedPageOverlapTestLoad)
		if err != nil || page.Text != expected.text || page.From != cursor.From {
			test.Fatal("page transition changed", page, err)
		}
		cursor.Page, cursor.Byte, cursor.From = expected.page, expected.byte, expected.from
		if !bytes.Equal(next, preparedPageOverlapTestEncode(test, cursor)) {
			test.Fatal("page transition cursor changed", string(next), cursor)
		}
	}
	page, next, err := input.read(context.Background(), preparedPageOverlapTestEncode(test, cursor), 3, 1, preparedPageOverlapTestLoad)
	if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(page, DocDisplayPage{}) || next != nil {
		test.Fatal("terminal continuation returned output", page, next, err)
	}
}

func TestDocPreparedPageOverlapValidatesUnselectedRuns(test *testing.T) {
	for name, mutate := range map[string]func(*DocDisplayPage){
		"prefix-source": func(page *DocDisplayPage) { page.Runs[0].SourceFrom = -1 },
		"prefix-linear": func(page *DocDisplayPage) { page.Runs[0].SourceTo++ },
		"prefix-order":  func(page *DocDisplayPage) { page.Runs[1].To = page.Runs[0].To },
		"prefix-rune":   func(page *DocDisplayPage) { page.Runs[1].To = 3 },
		"suffix-source": func(page *DocDisplayPage) { page.Runs[len(page.Runs)-1].SourceTo = 4097 },
		"suffix-order": func(page *DocDisplayPage) {
			page.Runs[len(page.Runs)-1].To = page.Runs[len(page.Runs)-2].To
		},
		"missing-tail": func(page *DocDisplayPage) { page.Runs = page.Runs[:len(page.Runs)-1] },
		"suffix-utf8":  func(page *DocDisplayPage) { page.Text = page.Text[:len(page.Text)-1] + "\xff" },
	} {
		test.Run(name, func(test *testing.T) {
			display := preparedPageOverlapTestPage()
			mutate(&display)
			input := newPreparedPageOverlapTestInput(test, display)
			raw := preparedPageOverlapTestEncode(test, docPreparedPageCursor{Version: 1, Plan: input.source.Plan, Byte: 9, From: 9})
			before := bytes.Clone(raw)
			page, next, err := input.read(context.Background(), raw, 2, 1, preparedPageOverlapTestLoad)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(page, DocDisplayPage{}) || next != nil || !bytes.Equal(raw, before) {
				test.Fatal("unselected corruption returned output", page, next, err)
			}
		})
	}
}

func TestDocPreparedPageOverlapInvalidBoundariesAndBudgetsYieldZero(test *testing.T) {
	input := newPreparedPageOverlapTestInput(test, preparedPageOverlapTestPage())
	for _, scenario := range []struct {
		from     int
		maxBytes int
		maxRuns  int
	}{
		{3, 4, 1},
		{6, 4, 1},
		{2, 2, 1},
		{5, 3, 1},
		{9, 0, 1},
		{9, -1, 1},
		{9, DocDisplayPageTextBytes + 1, 1},
		{9, 2, 0},
		{9, 2, -1},
		{9, 2, DocDisplayPageRuns + 1},
	} {
		raw := preparedPageOverlapTestEncode(test, docPreparedPageCursor{Version: 1, Plan: input.source.Plan, Byte: scenario.from, From: scenario.from})
		before := bytes.Clone(raw)
		page, next, err := input.read(context.Background(), raw, scenario.maxBytes, scenario.maxRuns, preparedPageOverlapTestLoad)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(page, DocDisplayPage{}) || next != nil || !bytes.Equal(raw, before) {
			test.Fatal("invalid selection returned output", scenario, page, next, err)
		}
	}
}

func TestDocPreparedPageOverlapFailureAndCancellationYieldZero(test *testing.T) {
	display := preparedPageOverlapTestPage()
	source := newPreparedPageOverlapTestInput(test, display).source
	failure := errors.New("prepared page read failed")
	for _, scenario := range []struct {
		name   string
		before bool
		during bool
		want   error
	}{
		{"page-error", false, false, failure},
		{"cancel-before", true, false, context.Canceled},
		{"cancel-during", false, true, context.Canceled},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reads := 0
			input, err := newDocPreparedPageAssemblyInput(source, 1, 1, func(context.Context, int, func(context.Context, ContentHash) ([]byte, error)) (DocDisplayPage, error) {
				reads++
				if scenario.during {
					cancel()
					return display, nil
				}
				return display, failure
			})
			if err != nil {
				test.Fatal(err)
			}
			if scenario.before {
				cancel()
			}
			raw := preparedPageOverlapTestEncode(test, docPreparedPageCursor{Version: 1, Plan: source.Plan, Byte: 9, From: 9})
			before := bytes.Clone(raw)
			page, next, err := input.read(ctx, raw, 2, 1, preparedPageOverlapTestLoad)
			if !errors.Is(err, scenario.want) || !reflect.DeepEqual(page, DocDisplayPage{}) || next != nil || !bytes.Equal(raw, before) || scenario.before && reads != 0 || !scenario.before && reads != 1 {
				test.Fatal("failed read leaked output or changed its cursor", page, next, err, reads)
			}
		})
	}
}

func TestDocPreparedPageOverlapOwnedConcurrentReplay(test *testing.T) {
	fixture := prepareDocToolStringFixture(test, `"`+strings.Repeat(`x\uD83D\uDE42한\n`, 12)+`"`, DocToolStringQuoted)
	input, err := NewDocToolStringAssemblyInput(fixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	load := func(_ context.Context, hash ContentHash) ([]byte, error) {
		raw, found := fixture.objects[hash]
		if !found {
			return nil, ErrNotFound
		}
		return bytes.Clone(raw), nil
	}
	_, late, err := input.read(context.Background(), input.initial, 32, 8, load)
	if err != nil {
		test.Fatal(err)
	}
	for _, raw := range []json.RawMessage{input.initial, late} {
		before := bytes.Clone(raw)
		page, next, err := input.read(context.Background(), raw, 7, 2, load)
		if err != nil {
			test.Fatal(err)
		}
		const workers = 12
		results := make(chan error, workers)
		for range workers {
			go func() {
				replayed, continuation, err := input.read(context.Background(), raw, 7, 2, load)
				if err == nil && (!reflect.DeepEqual(replayed, page) || !bytes.Equal(continuation, next)) {
					err = errors.New("same-input replay changed a prepared fragment")
				}
				if err == nil {
					replayed.Runs[0].SourceFrom = -1
					continuation[0] = '!'
				}
				results <- err
			}()
		}
		for range workers {
			if err := <-results; err != nil {
				test.Fatal(err)
			}
		}
		replayed, continuation, err := input.read(context.Background(), raw, 7, 2, load)
		if err != nil || !reflect.DeepEqual(replayed, page) || !bytes.Equal(continuation, next) || !bytes.Equal(raw, before) {
			test.Fatal("output mutation changed the owned replay", replayed, continuation, err)
		}
	}
}
