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

type compositeAssemblyTestChild struct {
	input DocDisplayAssemblyInput
	page  DocDisplayPage
	reads int
}

func compositeAssemblyTestSource() DocDisplayProjectionSource {
	return DocDisplayProjectionSource{Version: 1, Profile: DocDisplayProjectionProfile, EventHash: HashContent([]byte("composite event")), EventBytes: MaxConversationDocumentBytes, Plan: HashContent([]byte("parent descriptor"))}
}

func newCompositeAssemblyTestChild(test *testing.T, event DocDisplayProjectionSource, name, text string, mapped bool) *compositeAssemblyTestChild {
	test.Helper()
	source := event
	source.Plan, source.TextBytes = HashContent([]byte(name)), len(text)
	hash, err := source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	fixture := &compositeAssemblyTestChild{page: DocDisplayPage{Version: 2, Source: hash, Text: text}}
	if text == "" {
		initial, err := canonicalJSON(docPreparedPageCursor{Version: 1, Plan: source.Plan})
		if err != nil {
			test.Fatal(err)
		}
		fixture.input = DocDisplayAssemblyInput{source: source, initial: initial}
		fixture.input.cursor = func(raw json.RawMessage) (int, error) {
			if !bytes.Equal(raw, initial) {
				return 0, ErrIntegrity
			}
			return 0, nil
		}
		fixture.input.read = func(context.Context, json.RawMessage, int, int, func(context.Context, ContentHash) ([]byte, error)) (DocDisplayPage, json.RawMessage, error) {
			fixture.reads++
			return DocDisplayPage{}, nil, ErrIntegrity
		}
		return fixture
	}
	anchor := min(100, source.EventBytes-len(text))
	if mapped {
		for offset, character := range text {
			fixture.page.Runs = append(fixture.page.Runs, DocDisplayRun{To: offset + utf8.RuneLen(character), SourceFrom: anchor + offset, SourceTo: min(source.EventBytes, anchor+offset+6)})
		}
	} else {
		fixture.page.Runs = []DocDisplayRun{{To: len(text), SourceFrom: anchor, SourceTo: anchor + len(text), Linear: true}}
	}
	fixture.input, err = newDocPreparedPageAssemblyInput(source, 1, 1, func(context.Context, int, func(context.Context, ContentHash) ([]byte, error)) (DocDisplayPage, error) {
		fixture.reads++
		return fixture.page, nil
	})
	if err != nil {
		test.Fatal(err)
	}
	return fixture
}

func compositeAssemblyTestInput(test *testing.T, source DocDisplayProjectionSource, children ...DocDisplayAssemblyInput) DocDisplayAssemblyInput {
	test.Helper()
	for _, child := range children {
		source.TextBytes += child.source.TextBytes
	}
	input, err := newDocCompositeAssemblyInput(source, children)
	if err != nil || input.Source() != source || input.validate() != nil {
		test.Fatal("invalid composite input", err)
	}
	return input
}

func compositeAssemblyTestLoad(context.Context, ContentHash) ([]byte, error) {
	return nil, ErrNotFound
}

func compositeAssemblyTestDecode(test *testing.T, raw json.RawMessage) docCompositeAssemblyCursor {
	test.Helper()
	var cursor docCompositeAssemblyCursor
	if err := json.Unmarshal(raw, &cursor); err != nil {
		test.Fatal(err)
	}
	return cursor
}

func compositeAssemblyTestEncode(test *testing.T, value any) json.RawMessage {
	test.Helper()
	raw, err := canonicalJSON(value)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func assertCompositeAssemblyTestFailure(test *testing.T, input DocDisplayAssemblyInput, ctx context.Context, raw json.RawMessage, maxBytes, maxRuns int, load func(context.Context, ContentHash) ([]byte, error), want error) {
	test.Helper()
	before := bytes.Clone(raw)
	page, next, err := input.read(ctx, raw, maxBytes, maxRuns, load)
	if !errors.Is(err, want) || next != nil || !reflect.DeepEqual(page, DocDisplayPage{}) || !bytes.Equal(before, raw) {
		test.Fatal("failed composite read leaked output or changed its cursor", err, next, page)
	}
}

func TestDocCompositeAssemblyBoundedFragmentsAndEmptyTransitions(test *testing.T) {
	source := compositeAssemblyTestSource()
	for _, limits := range [][2]int{{1, 1}, {3, 2}, {4, 1}, {19, 3}, {DocDisplayPageTextBytes, DocDisplayPageRuns}} {
		test.Run(fmt.Sprint(limits), func(test *testing.T) {
			text := "ab\ud55c🙂cd"
			if limits[0] < utf8.UTFMax {
				text = "abcdef"
			}
			left := newCompositeAssemblyTestChild(test, source, "left", text, true)
			right := newCompositeAssemblyTestChild(test, source, "right", "last", false)
			empty := newCompositeAssemblyTestChild(test, source, "empty", "", false)
			input := compositeAssemblyTestInput(test, source, empty.input, left.input, empty.input, empty.input, right.input, empty.input)
			cursor := bytes.Clone(input.initial)
			if compositeAssemblyTestDecode(test, cursor).Part != 1 {
				test.Fatal("leading empty child was not skipped")
			}
			var output strings.Builder
			for steps := 0; output.Len() < input.source.TextBytes; steps++ {
				if steps >= input.source.TextBytes {
					test.Fatal("composite replay did not progress")
				}
				before := bytes.Clone(cursor)
				reads := left.reads + right.reads
				page, next, err := input.read(context.Background(), cursor, limits[0], limits[1], compositeAssemblyTestLoad)
				position, cursorErr := input.cursor(next)
				if err != nil || cursorErr != nil || page.Version != 2 || page.From != output.Len() || len(page.Text) == 0 || len(page.Text) > limits[0] || len(page.Runs) > limits[1] || position != page.From+len(page.Text) || left.reads+right.reads != reads+1 || empty.reads != 0 || !bytes.Equal(cursor, before) || bytes.Equal(cursor, next) || len(next) > MaxDocDisplaySourceBytes {
					test.Fatal("unbounded, stalled, or mutating read", err, cursorErr, page, position)
				}
				hash, _ := input.source.Hash()
				if page.Source != hash {
					test.Fatal("child source was not rebased")
				}
				selected, base := left, 0
				if page.From >= len(text) {
					selected, base = right, len(text)
				}
				for offset, character := range page.Text {
					width := utf8.RuneLen(character)
					got, err := page.SourceRange(offset, width)
					want, failure := selected.page.SourceRange(page.From-base+offset, width)
					if err != nil || failure != nil || got != want {
						test.Fatal("child provenance changed", got, want, err, failure)
					}
				}
				output.WriteString(page.Text)
				cursor = next
			}
			if output.String() != text+"last" {
				test.Fatal("composite text changed", output.String())
			}
			terminal := compositeAssemblyTestDecode(test, cursor)
			if terminal.Part != 6 || terminal.From != input.source.TextBytes || string(terminal.Child) != "null" {
				test.Fatal("invalid composite terminal", terminal)
			}
			assertCompositeAssemblyTestFailure(test, input, context.Background(), cursor, 4, 1, compositeAssemblyTestLoad, ErrIntegrity)
		})
	}
}

func TestDocCompositeAssemblyZeroPageInputs(test *testing.T) {
	source := compositeAssemblyTestSource()
	empty := newCompositeAssemblyTestChild(test, source, "empty", "", false)
	for _, children := range [][]DocDisplayAssemblyInput{nil, {empty.input}, {empty.input, empty.input}} {
		input := compositeAssemblyTestInput(test, source, children...)
		state, err := NewDocDisplayAssemblyState(input)
		if err != nil || !state.Output.Done || state.Validate(input) != nil {
			test.Fatal("empty composite did not start terminal", err)
		}
		generation, objects, err := FinishDocDisplayAssembly(context.Background(), input, state, nil)
		if err != nil || generation.Pages.Count != 0 || generation.Source != source || len(objects) != 0 {
			test.Fatal("empty composite failed to finish", err)
		}
		assertCompositeAssemblyTestFailure(test, input, context.Background(), input.initial, 1, 1, compositeAssemblyTestLoad, ErrIntegrity)
	}
	if empty.reads != 0 {
		test.Fatal("empty child was read")
	}
}

func TestDocCompositeAssemblyRejectsInvalidConstruction(test *testing.T) {
	source := compositeAssemblyTestSource()
	child := newCompositeAssemblyTestChild(test, source, "child", "abc", false).input
	source.TextBytes = child.source.TextBytes
	for name, mutate := range map[string]func(*DocDisplayProjectionSource, *[]DocDisplayAssemblyInput){
		"source":        func(source *DocDisplayProjectionSource, _ *[]DocDisplayAssemblyInput) { source.Version++ },
		"short-total":   func(source *DocDisplayProjectionSource, _ *[]DocDisplayAssemblyInput) { source.TextBytes-- },
		"long-total":    func(source *DocDisplayProjectionSource, _ *[]DocDisplayAssemblyInput) { source.TextBytes++ },
		"missing-child": func(_ *DocDisplayProjectionSource, children *[]DocDisplayAssemblyInput) { *children = nil },
		"too-many": func(_ *DocDisplayProjectionSource, children *[]DocDisplayAssemblyInput) {
			*children = make([]DocDisplayAssemblyInput, docCompositeAssemblyMaxInputs+1)
		},
		"invalid-child": func(_ *DocDisplayProjectionSource, children *[]DocDisplayAssemblyInput) {
			(*children)[0] = DocDisplayAssemblyInput{}
		},
		"event": func(_ *DocDisplayProjectionSource, children *[]DocDisplayAssemblyInput) {
			(*children)[0].source.EventHash = HashContent(nil)
		},
		"event-bytes": func(_ *DocDisplayProjectionSource, children *[]DocDisplayAssemblyInput) {
			(*children)[0].source.EventBytes--
		},
		"empty-event": func(source *DocDisplayProjectionSource, children *[]DocDisplayAssemblyInput) {
			empty := newCompositeAssemblyTestChild(test, *source, "empty", "", false).input
			empty.source.EventHash = HashContent(nil)
			*children = append(*children, empty)
		},
		"empty-length": func(source *DocDisplayProjectionSource, children *[]DocDisplayAssemblyInput) {
			empty := newCompositeAssemblyTestChild(test, *source, "empty", "", false).input
			empty.source.TextBytes = 1
			*children = append(*children, empty)
		},
		"noncanonical-child": func(_ *DocDisplayProjectionSource, children *[]DocDisplayAssemblyInput) {
			(*children)[0].initial = append(bytes.Clone(child.initial), ' ')
			(*children)[0].cursor = func(json.RawMessage) (int, error) { return 0, nil }
		},
		"oversized-wrapper": func(_ *DocDisplayProjectionSource, children *[]DocDisplayAssemblyInput) {
			(*children)[0].initial = compositeAssemblyTestEncode(test, map[string]string{"padding": strings.Repeat("x", MaxDocDisplaySourceBytes-40)})
			(*children)[0].cursor = func(json.RawMessage) (int, error) { return 0, nil }
		},
	} {
		test.Run(name, func(test *testing.T) {
			invalidSource, children := source, []DocDisplayAssemblyInput{child}
			mutate(&invalidSource, &children)
			input, err := newDocCompositeAssemblyInput(invalidSource, children)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(input, DocDisplayAssemblyInput{}) {
				test.Fatal("invalid construction leaked an input", err)
			}
		})
	}
	children := make([]DocDisplayAssemblyInput, docCompositeAssemblyMaxInputs)
	for index := range children {
		children[index] = child
	}
	source.TextBytes = len(children) * child.source.TextBytes
	if _, err := newDocCompositeAssemblyInput(source, children); err != nil {
		test.Fatal("sixteen children were rejected", err)
	}
}

func TestDocCompositeAssemblyRejectsCursorTransplantsBeforeReads(test *testing.T) {
	source := compositeAssemblyTestSource()
	left := newCompositeAssemblyTestChild(test, source, "left", "abc", false)
	right := newCompositeAssemblyTestChild(test, source, "right", "xyz", false)
	empty := newCompositeAssemblyTestChild(test, source, "empty", "", false)
	input := compositeAssemblyTestInput(test, source, left.input, empty.input, right.input)
	start := compositeAssemblyTestDecode(test, input.initial)
	variants := map[string]json.RawMessage{
		"missing":    nil,
		"oversized":  bytes.Repeat([]byte("x"), MaxDocDisplaySourceBytes+1),
		"whitespace": append(bytes.Clone(input.initial), ' '),
		"duplicate":  append([]byte(`{"version":1,`), input.initial[1:]...),
		"unknown":    append([]byte(`{"unknown":null,`), input.initial[1:]...),
		"fraction":   bytes.Replace(input.initial, []byte(`"version":1`), []byte(`"version":1.0`), 1),
	}
	for name, mutate := range map[string]func(*docCompositeAssemblyCursor){
		"version":        func(cursor *docCompositeAssemblyCursor) { cursor.Version++ },
		"plan":           func(cursor *docCompositeAssemblyCursor) { cursor.Plan = HashContent(nil) },
		"parts":          func(cursor *docCompositeAssemblyCursor) { cursor.Parts = HashContent(nil) },
		"negative-part":  func(cursor *docCompositeAssemblyCursor) { cursor.Part = -1 },
		"oversized-part": func(cursor *docCompositeAssemblyCursor) { cursor.Part = 4 },
		"part":           func(cursor *docCompositeAssemblyCursor) { cursor.Part = 2 },
		"empty-part":     func(cursor *docCompositeAssemblyCursor) { cursor.Part = 1 },
		"offset":         func(cursor *docCompositeAssemblyCursor) { cursor.From++ },
		"child":          func(cursor *docCompositeAssemblyCursor) { cursor.Child = right.input.initial },
		"absent-child":   func(cursor *docCompositeAssemblyCursor) { cursor.Child = nil },
		"terminal-child": func(cursor *docCompositeAssemblyCursor) {
			cursor.Child = compositeAssemblyTestEncode(test, docPreparedPageCursor{Version: 1, Plan: left.input.source.Plan, Page: 1, From: 3})
			cursor.From = 3
		},
		"false-terminal":      func(cursor *docCompositeAssemblyCursor) { cursor.Part, cursor.Child = 3, nil },
		"terminal-with-child": func(cursor *docCompositeAssemblyCursor) { cursor.Part, cursor.From = 3, input.source.TextBytes },
		"terminal-offset": func(cursor *docCompositeAssemblyCursor) {
			cursor.Part, cursor.From, cursor.Child = 3, input.source.TextBytes-1, nil
		},
	} {
		cursor := start
		mutate(&cursor)
		variants[name] = compositeAssemblyTestEncode(test, cursor)
	}
	foreignSource := source
	foreignSource.Plan = HashContent([]byte("foreign parent descriptor"))
	variants["parent"] = compositeAssemblyTestInput(test, foreignSource, left.input, empty.input, right.input).initial
	variants["reordered"] = compositeAssemblyTestInput(test, source, right.input, empty.input, left.input).initial
	changedEmpty := newCompositeAssemblyTestChild(test, source, "foreign-empty-plan", "", false)
	variants["empty-generation"] = compositeAssemblyTestInput(test, source, left.input, changedEmpty.input, right.input).initial
	for name, raw := range variants {
		test.Run(name, func(test *testing.T) {
			position, err := input.cursor(raw)
			if !errors.Is(err, ErrIntegrity) || position != 0 {
				test.Fatal("invalid cursor leaked a position", position, err)
			}
			loads := 0
			assertCompositeAssemblyTestFailure(test, input, context.Background(), raw, 4, 1, func(context.Context, ContentHash) ([]byte, error) {
				loads++
				return nil, ErrNotFound
			}, ErrIntegrity)
			if loads != 0 || left.reads != 0 || right.reads != 0 || empty.reads != 0 {
				test.Fatal("forged cursor reached child reads", loads)
			}
		})
	}
}

func TestDocCompositeAssemblyUTF8AndInvalidBudgets(test *testing.T) {
	source := compositeAssemblyTestSource()
	child := newCompositeAssemblyTestChild(test, source, "unicode", "a\ud55c🙂z", false)
	input := compositeAssemblyTestInput(test, source, child.input)
	page, next, err := input.read(context.Background(), input.initial, 2, 1, compositeAssemblyTestLoad)
	if err != nil || page.Text != "a" {
		test.Fatal("short buffer lost fitting ASCII", page, err)
	}
	assertCompositeAssemblyTestFailure(test, input, context.Background(), next, 2, 1, compositeAssemblyTestLoad, ErrIntegrity)
	for _, offset := range []int{2, 3, 5, 6, 7} {
		cursor := compositeAssemblyTestDecode(test, input.initial)
		cursor.From = offset
		cursor.Child = compositeAssemblyTestEncode(test, docPreparedPageCursor{Version: 1, Plan: child.input.source.Plan, Byte: offset, From: offset})
		assertCompositeAssemblyTestFailure(test, input, context.Background(), compositeAssemblyTestEncode(test, cursor), 4, 1, compositeAssemblyTestLoad, ErrIntegrity)
	}
	reads := child.reads
	for _, limits := range [][2]int{{0, 1}, {-1, 1}, {DocDisplayPageTextBytes + 1, 1}, {4, 0}, {4, -1}, {4, DocDisplayPageRuns + 1}} {
		assertCompositeAssemblyTestFailure(test, input, context.Background(), input.initial, limits[0], limits[1], compositeAssemblyTestLoad, ErrIntegrity)
	}
	assertCompositeAssemblyTestFailure(test, input, context.Background(), input.initial, 4, 1, nil, ErrIntegrity)
	if child.reads != reads {
		test.Fatal("invalid budget reached a child")
	}
}

func TestDocCompositeAssemblyAliasesAndSelectedLateChild(test *testing.T) {
	source := compositeAssemblyTestSource()
	children := make([]DocDisplayAssemblyInput, docCompositeAssemblyMaxInputs)
	fixtures := make([]*compositeAssemblyTestChild, len(children))
	for index := range children {
		fixtures[index] = newCompositeAssemblyTestChild(test, source, fmt.Sprint(index), strings.Repeat("x", 20000), false)
		children[index] = fixtures[index].input
	}
	input := compositeAssemblyTestInput(test, source, children...)
	initial := bytes.Clone(input.initial)
	start := compositeAssemblyTestDecode(test, initial)
	start.Part, start.From, start.Child = len(children)-1, (len(children)-1)*20000, bytes.Clone(children[len(children)-1].initial)
	late := compositeAssemblyTestEncode(test, start)
	children[len(children)-1] = DocDisplayAssemblyInput{}
	fixtures[len(fixtures)-1].input.initial[0] = '!'
	input.initial[0] = '!'
	page, next, err := input.read(context.Background(), late, 3, 1, compositeAssemblyTestLoad)
	if err != nil || page.Text != "xxx" || page.From != start.From {
		test.Fatal("caller aliases changed the selected child", err, page)
	}
	if position, err := input.cursor(next); err != nil || position != start.From+3 {
		test.Fatal("late cursor lost its outer offset", position, err)
	}
	page.Runs[0].SourceFrom = -1
	next[0] = '!'
	replayed, again, err := input.read(context.Background(), late, 3, 1, compositeAssemblyTestLoad)
	if err != nil || replayed.Runs[0].SourceFrom != 100 || bytes.Equal(again, next) || fixtures[len(fixtures)-1].page.Runs[0].SourceFrom != 100 {
		test.Fatal("returned runs or cursor aliased captured state", err)
	}
	for index, fixture := range fixtures {
		want := 0
		if index == len(fixtures)-1 {
			want = 2
		}
		if fixture.reads != want {
			test.Fatal("late child replayed a prefix", index, fixture.reads)
		}
	}
	if _, err := input.cursor(initial); err != nil {
		test.Fatal("public initial cursor alias changed captured children", err)
	}
	if !bytes.Equal(late, compositeAssemblyTestEncode(test, start)) {
		test.Fatal("selected cursor mutated")
	}
	test.Logf("children=%d preceding_bytes=%d selected_child_reads=%d prefix_child_reads=0 cursor_bytes=%d", len(fixtures), start.From, fixtures[len(fixtures)-1].reads, len(again))
}

func TestDocCompositeAssemblyFaultsAndCancellationYieldZero(test *testing.T) {
	source := compositeAssemblyTestSource()
	fault := errors.New("selected child fault")
	for _, mode := range []string{"fault", "cancel-before", "cancel-after", "cancel-and-fault", "load-fault", "load-cancel"} {
		test.Run(mode, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			child := newCompositeAssemblyTestChild(test, source, "fault child", "abcd", false)
			read := child.input.read
			calls := 0
			child.input.read = func(ctx context.Context, raw json.RawMessage, maxBytes, maxRuns int, load func(context.Context, ContentHash) ([]byte, error)) (DocDisplayPage, json.RawMessage, error) {
				calls++
				page, next, err := read(ctx, raw, maxBytes, maxRuns, load)
				if err != nil {
					return page, next, err
				}
				if strings.HasPrefix(mode, "load-") {
					_, err = load(ctx, HashContent(nil))
					return page, next, err
				}
				if strings.Contains(mode, "cancel") {
					cancel()
				}
				if mode == "fault" || mode == "cancel-and-fault" {
					err = fault
				}
				return page, next, err
			}
			input := compositeAssemblyTestInput(test, source, child.input)
			if mode == "cancel-before" {
				cancel()
			}
			want := fault
			if strings.Contains(mode, "cancel") {
				want = context.Canceled
			}
			assertCompositeAssemblyTestFailure(test, input, ctx, input.initial, 2, 1, func(context.Context, ContentHash) ([]byte, error) {
				if mode == "load-cancel" {
					cancel()
					return []byte("ignored"), nil
				}
				return nil, fault
			}, want)
			if mode == "cancel-before" && calls != 0 || mode != "cancel-before" && calls != 1 {
				test.Fatal("unexpected child read count", calls)
			}
		})
	}
}

func TestDocCompositeAssemblyRejectsInvalidChildOutput(test *testing.T) {
	source := compositeAssemblyTestSource()
	for name, mutate := range map[string]func(*DocDisplayPage, *json.RawMessage){
		"version":     func(page *DocDisplayPage, _ *json.RawMessage) { page.Version = 1 },
		"source":      func(page *DocDisplayPage, _ *json.RawMessage) { page.Source = HashContent(nil) },
		"offset":      func(page *DocDisplayPage, _ *json.RawMessage) { page.From++ },
		"bytes":       func(page *DocDisplayPage, _ *json.RawMessage) { page.Text += "x" },
		"runs":        func(page *DocDisplayPage, _ *json.RawMessage) { page.Runs = append(page.Runs, page.Runs[0]) },
		"coordinates": func(page *DocDisplayPage, _ *json.RawMessage) { page.Runs[0].SourceFrom = -1 },
		"empty":       func(page *DocDisplayPage, _ *json.RawMessage) { page.Text, page.Runs = "", nil },
		"unchanged": func(_ *DocDisplayPage, next *json.RawMessage) {
			*next = compositeAssemblyTestEncode(test, docPreparedPageCursor{Version: 1, Plan: HashContent([]byte("child"))})
		},
		"foreign":      func(_ *DocDisplayPage, next *json.RawMessage) { *next = json.RawMessage(`{"foreign":true}`) },
		"noncanonical": func(_ *DocDisplayPage, next *json.RawMessage) { *next = append(bytes.Clone(*next), ' ') },
		"oversized": func(_ *DocDisplayPage, next *json.RawMessage) {
			*next = bytes.Repeat([]byte("x"), MaxDocDisplaySourceBytes+1)
		},
	} {
		test.Run(name, func(test *testing.T) {
			child := newCompositeAssemblyTestChild(test, source, "child", "abcd", false)
			read := child.input.read
			child.input.read = func(ctx context.Context, raw json.RawMessage, maxBytes, maxRuns int, load func(context.Context, ContentHash) ([]byte, error)) (DocDisplayPage, json.RawMessage, error) {
				page, next, err := read(ctx, raw, maxBytes, maxRuns, load)
				if err == nil {
					mutate(&page, &next)
				}
				return page, next, err
			}
			input := compositeAssemblyTestInput(test, source, child.input)
			assertCompositeAssemblyTestFailure(test, input, context.Background(), input.initial, 2, 1, compositeAssemblyTestLoad, ErrIntegrity)
		})
	}
}

func TestDocCompositeAssemblyRealChildrenAndAssembly(test *testing.T) {
	stringFixture := prepareDocToolStringFixture(test, "\"a\\uD83D\\uDE42\\n\ud55c\uae00\"", DocToolStringQuoted)
	stringInput, err := NewDocToolStringAssemblyInput(stringFixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	valueFixture := newDocToolValuePlanFixture(test, "{\"name\":\"\ud55c🙂\",\"nested\":[true,null,17]}")
	valueInput, err := NewDocToolValueAssemblyInput(valueFixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	mappedFixture := buildDecodedMarkdownFixture(test, []byte("{\"kind\":\"reasoning\",\"redacted_summary\":\"**a\\uD83D\\uDE42** [\ud55c\uae00](https://hidden.test)\"}"))
	mappedInput, err := NewDocDecodedMarkdownAssemblyInput(mappedFixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	mappedOracle, err := BuildDocEventDisplay(context.Background(), mappedFixture.decoded.raw)
	if err != nil {
		test.Fatal(err)
	}
	for _, fixture := range []struct {
		name    string
		input   DocDisplayAssemblyInput
		objects map[ContentHash][]byte
		text    string
	}{
		{name: "string", input: stringInput, objects: stringFixture.objects, text: docToolStringSmallOracle(test, stringFixture.plan.Source, stringFixture.raw).Text()},
		{name: "value", input: valueInput, objects: valueFixture.objects, text: valueFixture.oracle.Text()},
		{name: "mapped", input: mappedInput, objects: mappedFixture.decoded.objects, text: mappedOracle.Text()},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			source := fixture.input.source
			source.Plan, source.TextBytes = HashContent([]byte("parent/"+fixture.name)), 0
			empty := newCompositeAssemblyTestChild(test, source, "empty", "", false)
			input := compositeAssemblyTestInput(test, source, empty.input, fixture.input, empty.input, fixture.input, empty.input)
			load := func(_ context.Context, hash ContentHash) ([]byte, error) {
				raw, found := fixture.objects[hash]
				if !found {
					return nil, ErrNotFound
				}
				return bytes.Clone(raw), nil
			}
			cursor, childCursor := bytes.Clone(input.initial), bytes.Clone(fixture.input.initial)
			var output strings.Builder
			for steps := 0; output.Len() < input.source.TextBytes; steps++ {
				if steps > input.source.TextBytes {
					test.Fatal("real composite did not progress")
				}
				child, childNext, childErr := fixture.input.read(context.Background(), childCursor, 4, 1, load)
				page, next, err := input.read(context.Background(), cursor, 4, 1, load)
				if err != nil || childErr != nil || page.From != output.Len() || page.Text != child.Text || !reflect.DeepEqual(page.Runs, child.Runs) {
					test.Fatal("composite changed an actual child fragment", err, childErr, page, child)
				}
				output.WriteString(page.Text)
				cursor, childCursor = next, childNext
				if output.Len() == fixture.input.source.TextBytes {
					childCursor = bytes.Clone(fixture.input.initial)
				}
			}
			if output.String() != fixture.text+fixture.text {
				test.Fatal("real composite text differs", output.String())
			}
			assembled := assembleDisplayFixture(test, input, fixture.objects)
			if got := assembledFixturePages(test, assembled, nil); got != output.String() {
				test.Fatal("assembled composite differs", got)
			}
			assertCompositeAssemblyTestFailure(test, input, context.Background(), input.initial, 4, 1, func(context.Context, ContentHash) ([]byte, error) {
				return []byte(`{"wrong":"generation"}`), nil
			}, ErrIntegrity)
		})
	}
}

func TestDocCompositeAssemblyEmptyMappedChild(test *testing.T) {
	fixture := buildDecodedMarkdownFixture(test, []byte(`{"kind":"reasoning","redacted_summary":""}`))
	empty, err := NewDocDecodedMarkdownAssemblyInput(fixture.plan)
	if err != nil || empty.source.TextBytes != 0 || fixture.plan.Markdown.Pages.Count != 0 {
		test.Fatal("fixture did not produce a zero-page mapped child", err)
	}
	source := empty.source
	source.Plan = HashContent([]byte("empty mapped parent"))
	tail := newCompositeAssemblyTestChild(test, source, "tail", "ascii", false)
	input := compositeAssemblyTestInput(test, source, empty, tail.input, empty)
	page, next, err := input.read(context.Background(), input.initial, 1, 1, compositeAssemblyTestLoad)
	position, cursorErr := input.cursor(next)
	if err != nil || cursorErr != nil || page.Text != "a" || page.From != 0 || position != 1 || tail.reads != 1 {
		test.Fatal("zero-page child blocked short ASCII replay", err, cursorErr, page)
	}
	input = compositeAssemblyTestInput(test, source, empty, empty)
	state, err := NewDocDisplayAssemblyState(input)
	if err != nil || !state.Output.Done {
		test.Fatal("zero-page composite did not finish immediately", err)
	}
}

func TestDocCompositeAssemblyBoundsGrowingCursor(test *testing.T) {
	type paddedCursor struct {
		Plan    ContentHash `json:"plan"`
		From    int         `json:"from"`
		Padding string      `json:"padding,omitempty"`
	}
	source := compositeAssemblyTestSource()
	childSource := source
	childSource.Plan, childSource.TextBytes = HashContent([]byte("padded child")), 4
	hash, err := childSource.Hash()
	if err != nil {
		test.Fatal(err)
	}
	child := DocDisplayAssemblyInput{source: childSource, initial: compositeAssemblyTestEncode(test, paddedCursor{Plan: childSource.Plan})}
	child.cursor = func(raw json.RawMessage) (int, error) {
		var cursor paddedCursor
		if len(raw) > MaxDocDisplaySourceBytes || json.Unmarshal(raw, &cursor) != nil || cursor.Plan != childSource.Plan || cursor.From < 0 || cursor.From > childSource.TextBytes {
			return 0, ErrIntegrity
		}
		return cursor.From, nil
	}
	childNext := compositeAssemblyTestEncode(test, paddedCursor{Plan: childSource.Plan, From: 2, Padding: strings.Repeat("x", MaxDocDisplaySourceBytes-150)})
	if len(childNext) > MaxDocDisplaySourceBytes {
		test.Fatal("padded child cursor itself exceeded the budget")
	}
	child.read = func(context.Context, json.RawMessage, int, int, func(context.Context, ContentHash) ([]byte, error)) (DocDisplayPage, json.RawMessage, error) {
		return DocDisplayPage{Version: 2, Source: hash, Text: "ab", Runs: []DocDisplayRun{{To: 2, SourceFrom: 100, SourceTo: 102, Linear: true}}}, childNext, nil
	}
	input := compositeAssemblyTestInput(test, source, child)
	assertCompositeAssemblyTestFailure(test, input, context.Background(), input.initial, 2, 1, compositeAssemblyTestLoad, ErrIntegrity)
}

func TestDocCompositeAssemblyCancellationDuringCursorValidation(test *testing.T) {
	source := compositeAssemblyTestSource()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	child := newCompositeAssemblyTestChild(test, source, "cursor cancel", "abcd", false)
	decode := child.input.cursor
	child.input.cursor = func(raw json.RawMessage) (int, error) {
		position, err := decode(raw)
		if position > 0 {
			cancel()
		}
		return position, err
	}
	input := compositeAssemblyTestInput(test, source, child.input)
	assertCompositeAssemblyTestFailure(test, input, ctx, input.initial, 2, 1, compositeAssemblyTestLoad, context.Canceled)
}

func TestDocCompositeAssemblyChildReadReceivesCursorCopy(test *testing.T) {
	source := compositeAssemblyTestSource()
	child := newCompositeAssemblyTestChild(test, source, "mutating child", "abcd", false)
	read := child.input.read
	var returned json.RawMessage
	child.input.read = func(ctx context.Context, raw json.RawMessage, maxBytes, maxRuns int, load func(context.Context, ContentHash) ([]byte, error)) (DocDisplayPage, json.RawMessage, error) {
		page, next, err := read(ctx, raw, maxBytes, maxRuns, load)
		raw[0] = '!'
		returned = next
		return page, next, err
	}
	input := compositeAssemblyTestInput(test, source, child.input)
	initial := bytes.Clone(input.initial)
	page, next, err := input.read(context.Background(), initial, 2, 1, compositeAssemblyTestLoad)
	if err != nil || page.Text != "ab" || !bytes.Equal(initial, input.initial) {
		test.Fatal("child reader mutated an outer cursor", err)
	}
	returned[0] = '!'
	page, _, err = input.read(context.Background(), next, 2, 1, compositeAssemblyTestLoad)
	if err != nil || page.Text != "cd" {
		test.Fatal("child cursor return aliased the outer continuation", err)
	}
}

func TestDocCompositeAssemblyConcurrentDeterministicReplay(test *testing.T) {
	fixture := prepareDocToolStringFixture(test, "\"abc\\uD83D\\uDE42\ud55c\uae00\"", DocToolStringQuoted)
	child, err := NewDocToolStringAssemblyInput(fixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	source := child.source
	source.Plan, source.TextBytes = HashContent([]byte("concurrent parent")), 0
	input := compositeAssemblyTestInput(test, source, child, child)
	load := func(_ context.Context, hash ContentHash) ([]byte, error) {
		raw, found := fixture.objects[hash]
		if !found {
			return nil, ErrNotFound
		}
		return bytes.Clone(raw), nil
	}
	page, next, err := input.read(context.Background(), input.initial, 4, 1, load)
	if err != nil {
		test.Fatal(err)
	}
	const workers = 12
	results := make(chan error, workers)
	for range workers {
		go func() {
			replayed, continuation, err := input.read(context.Background(), input.initial, 4, 1, load)
			if err == nil && (!reflect.DeepEqual(replayed, page) || !bytes.Equal(continuation, next)) {
				err = errors.New("concurrent replay changed a fragment")
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
}
