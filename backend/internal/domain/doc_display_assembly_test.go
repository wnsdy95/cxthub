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

type assemblyTestCursor struct {
	Version  int         `json:"version"`
	Source   ContentHash `json:"source"`
	Position int         `json:"position"`
	Phase    int         `json:"phase,omitempty"`
}

type assemblyTestRequest struct {
	Position int
	Bytes    int
	Runs     int
}

type assemblyTestFixture struct {
	input    DocDisplayAssemblyInput
	text     string
	chunk    int
	mapped   bool
	consumed int
	requests []assemblyTestRequest
	objects  map[ContentHash][]byte
}

func newAssemblyTestFixture(test *testing.T, text string, chunk int, mapped bool) *assemblyTestFixture {
	test.Helper()
	fixture := &assemblyTestFixture{text: text, chunk: chunk, mapped: mapped, objects: make(map[ContentHash][]byte)}
	source := DocDisplayProjectionSource{Version: 1, Profile: DocDisplayProjectionProfile, EventHash: HashContent([]byte(text)), EventBytes: MaxConversationDocumentBytes, Plan: HashContent([]byte("assembly unit fragments")), TextBytes: len(text)}
	hash, err := source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	fixture.input.source = source
	fixture.input.initial = fixture.cursor(test, 0)
	fixture.input.cursor = func(raw json.RawMessage) (int, error) {
		var cursor assemblyTestCursor
		if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || json.Unmarshal(raw, &cursor) != nil || cursor.Version != 1 || cursor.Source != hash || cursor.Position < 0 || cursor.Position > len(text) || cursor.Position < len(text) && !utf8.RuneStart(text[cursor.Position]) {
			return 0, ErrIntegrity
		}
		canonical, err := canonicalJSON(cursor)
		if err != nil || !bytes.Equal(canonical, raw) {
			return 0, ErrIntegrity
		}
		return cursor.Position, nil
	}
	fixture.input.read = func(ctx context.Context, raw json.RawMessage, maxBytes, maxRuns int, _ func(context.Context, ContentHash) ([]byte, error)) (DocDisplayPage, json.RawMessage, error) {
		if err := ctx.Err(); err != nil {
			return DocDisplayPage{}, nil, err
		}
		position, err := fixture.input.cursor(raw)
		fixture.requests = append(fixture.requests, assemblyTestRequest{Position: position, Bytes: maxBytes, Runs: maxRuns})
		if err != nil || position != fixture.consumed || maxBytes <= 0 || maxBytes > DocDisplayPageTextBytes || maxRuns <= 0 || maxRuns > DocDisplayPageRuns {
			return DocDisplayPage{}, nil, ErrIntegrity
		}
		limit := min(len(text), position+maxBytes)
		if fixture.chunk > 0 {
			limit = min(limit, position+fixture.chunk)
		}
		page := DocDisplayPage{Version: 2, Source: hash, From: position}
		end := position
		for end < limit && (!fixture.mapped || len(page.Runs) < maxRuns) {
			_, width := utf8.DecodeRuneInString(text[end:])
			if end+width > limit {
				break
			}
			end += width
			if fixture.mapped {
				page.Runs = append(page.Runs, DocDisplayRun{To: end - position, SourceFrom: source.EventBytes - 1, SourceTo: source.EventBytes})
			}
		}
		if end == position {
			return DocDisplayPage{}, nil, ErrIntegrity
		}
		page.Text = text[position:end]
		if !fixture.mapped {
			page.Runs = []DocDisplayRun{{To: len(page.Text), SourceFrom: position, SourceTo: end, Linear: true}}
		}
		fixture.consumed = end
		return page, fixture.cursor(test, end), nil
	}
	return fixture
}

func (fixture *assemblyTestFixture) cursor(test *testing.T, position int) json.RawMessage {
	test.Helper()
	hash, err := fixture.input.source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	raw, err := canonicalJSON(assemblyTestCursor{Version: 1, Source: hash, Position: position})
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func (fixture *assemblyTestFixture) load(_ context.Context, hash ContentHash) ([]byte, error) {
	raw, found := fixture.objects[hash]
	if !found {
		return nil, ErrNotFound
	}
	return raw, nil
}

func (fixture *assemblyTestFixture) save(test *testing.T, objects []DocRangeObject) {
	test.Helper()
	for _, object := range objects {
		if len(object.Bytes) == 0 || len(object.Bytes) > MaxDocDisplayPageBytes || HashContent(object.Bytes) != object.Hash {
			test.Fatal("invalid staged object", len(object.Bytes), object.Hash)
		}
		fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
	}
}

func assemblyTestEncode(test *testing.T, input DocDisplayAssemblyInput, state DocDisplayAssemblyState) []byte {
	test.Helper()
	raw, err := state.Encode(input)
	if err != nil || len(raw) > MaxDocDisplayStateBytes {
		test.Fatal("invalid checkpoint", len(raw), err)
	}
	return raw
}

func (fixture *assemblyTestFixture) step(test *testing.T, state DocDisplayAssemblyState) DocDisplayAssemblyState {
	test.Helper()
	before := assemblyTestEncode(test, fixture.input, state)
	reads := len(fixture.requests)
	step, err := StepDocDisplayAssembly(context.Background(), fixture.input, state, fixture.load)
	if err != nil || len(fixture.requests)-reads > 1 {
		test.Fatal("step failed or replayed more than once", len(fixture.requests)-reads, err)
	}
	if !bytes.Equal(before, assemblyTestEncode(test, fixture.input, state)) {
		test.Fatal("step changed its prior checkpoint")
	}
	fixture.save(test, step.Objects)
	raw := assemblyTestEncode(test, fixture.input, step.State)
	state, err = DecodeDocDisplayAssemblyState(fixture.input, HashContent(raw), raw)
	if err != nil || ValidateDocDisplayAssemblyCheckpoint(context.Background(), fixture.input, state, fixture.load) != nil {
		test.Fatal("checkpoint did not restart", err)
	}
	return state
}

func (fixture *assemblyTestFixture) build(test *testing.T) (DocDisplayAssemblyState, DocDisplayProjectionGeneration, []DocDisplayPage) {
	test.Helper()
	state, err := NewDocDisplayAssemblyState(fixture.input)
	if err != nil {
		test.Fatal(err)
	}
	for steps := 0; !state.Output.Done; steps++ {
		if steps > len(fixture.text)+1 {
			test.Fatal("assembly stopped making progress")
		}
		state = fixture.step(test, state)
	}
	generation, objects, err := FinishDocDisplayAssembly(context.Background(), fixture.input, state, fixture.load)
	if err != nil {
		test.Fatal(err)
	}
	fixture.save(test, objects)
	var joined strings.Builder
	var pages []DocDisplayPage
	for index := 0; index < generation.Pages.Count; index++ {
		page, err := ReadDocDisplayProjectionPage(context.Background(), generation, index, fixture.load)
		if err != nil || page.From != joined.Len() || !utf8.ValidString(page.Text) || index < generation.Pages.Count-1 && utf8.RuneCountInString(page.Text) < DocDisplayPageRuns {
			test.Fatal("invalid sealed page", index, err)
		}
		joined.WriteString(page.Text)
		pages = append(pages, page)
	}
	if joined.String() != fixture.text || state.Pending != "" || fixture.consumed != len(fixture.text) {
		test.Fatal("assembly lost display text or cursor progress")
	}
	return state, generation, pages
}

func TestDocDisplayAssemblyRestartsShortFragments(test *testing.T) {
	text := strings.Repeat("양🙂abc<", 150)
	fixture := newAssemblyTestFixture(test, text, 11, false)
	state, err := NewDocDisplayAssemblyState(fixture.input)
	if err != nil {
		test.Fatal(err)
	}
	state = fixture.step(test, state)
	position, err := fixture.input.cursor(state.Cursor)
	if err != nil || position == 0 || state.Output.TextBytes != 0 || state.Pending == "" {
		test.Fatal("short fragment was not staged separately", state, err)
	}
	checkpoint := assemblyTestEncode(test, fixture.input, state)
	if bytes.Contains(checkpoint, []byte("양")) || bytes.Contains(checkpoint, []byte(`"text"`)) || bytes.Contains(checkpoint, []byte(`"runs"`)) {
		test.Fatal("checkpoint contains body text")
	}
	if _, err := DecodeDocDisplayProjectionPage(fixture.input.source, state.Pending, fixture.objects[state.Pending]); !errors.Is(err, ErrIntegrity) {
		test.Fatal("short nonterminal pending page was publicly decodable", err)
	}
	for !state.Output.Done {
		state = fixture.step(test, state)
	}
	if fixture.consumed != len(text) || len(fixture.requests) < 100 {
		test.Fatal("fixture did not exercise short bounded fragments")
	}
	generation, objects, err := FinishDocDisplayAssembly(context.Background(), fixture.input, state, fixture.load)
	if err != nil || generation.Source != fixture.input.source {
		test.Fatal(err)
	}
	fixture.save(test, objects)
	var joined strings.Builder
	for index := 0; index < generation.Pages.Count; index++ {
		page, err := ReadDocDisplayProjectionPage(context.Background(), generation, index, fixture.load)
		if err != nil {
			test.Fatal(err)
		}
		joined.WriteString(page.Text)
		if len(page.Runs) != 1 || !page.Runs[0].Linear {
			test.Fatal("adjacent linear fragment runs were not coalesced")
		}
	}
	if joined.String() != text {
		test.Fatal("resumed display differs from its fragments")
	}
}

func TestDocDisplayAssemblyLongTextAndEncodedBudgets(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		text   string
		mapped bool
	}{
		{"linear", strings.Repeat("plain🙂", 18_000), false},
		{"escaped-linear", strings.Repeat("<>&\x00\"\\\u2028🙂", 8_000), false},
		{"escaped-mapped", strings.Repeat("<>&\x00\u2028🙂", 1_000), true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newAssemblyTestFixture(test, scenario.text, 0, scenario.mapped)
			_, generation, pages := fixture.build(test)
			if generation.Pages.Count < 2 {
				test.Fatal("long text did not span sealed pages")
			}
			for _, page := range pages {
				raw, err := page.EncodeProjection(fixture.input.source)
				if err != nil || len(raw) > MaxDocDisplayPageBytes {
					test.Fatal("encoded page exceeded assembly budget", len(raw), err)
				}
			}
			for _, request := range fixture.requests {
				if request.Bytes > len(scenario.text)-request.Position || request.Bytes*6+request.Runs*docDisplayAssemblyRunBytes > MaxDocDisplayPageBytes {
					test.Fatal("replay request exceeded conservative budget", request)
				}
			}
			test.Logf("bytes=%d reads=%d pages=%d", len(scenario.text), len(fixture.requests), len(pages))
		})
	}
	worst, err := canonicalJSON(DocDisplayRun{To: DocDisplayPageTextBytes, SourceFrom: MaxConversationDocumentBytes, SourceTo: MaxConversationDocumentBytes, Linear: false})
	if err != nil || len(worst)+1 > docDisplayAssemblyRunBytes {
		test.Fatal("run encoding reservation is not conservative", len(worst), err)
	}
}

func TestDocDisplayAssemblyServesExactMinimumRunes(test *testing.T) {
	fixture := newAssemblyTestFixture(test, strings.Repeat("🙂", DocDisplayPageRuns*2+1), 4, true)
	_, generation, pages := fixture.build(test)
	if generation.Pages.Count != 3 || len(fixture.requests) != DocDisplayPageRuns*2+1 {
		test.Fatal("unexpected fragment or page count", generation.Pages.Count, len(fixture.requests))
	}
	for index, page := range pages {
		want := DocDisplayPageRuns
		if index == len(pages)-1 {
			want = 1
		}
		if utf8.RuneCountInString(page.Text) != want || len(page.Runs) != want {
			test.Fatal("minimum-rune page boundary changed", index, len(page.Runs))
		}
	}
}

func TestDocDisplayAssemblySealsPendingWithoutReplay(test *testing.T) {
	for _, scenario := range []struct {
		name  string
		text  string
		total int
	}{
		{"minimum", strings.Repeat("a", DocDisplayPageRuns), DocDisplayPageRuns + 1},
		{"byte-capacity", strings.Repeat("a", DocDisplayPageTextBytes), DocDisplayPageTextBytes + 1},
		{"terminal-short", "🙂", len("🙂")},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newAssemblyTestFixture(test, scenario.text+strings.Repeat("z", scenario.total-len(scenario.text)), 1, false)
			state, err := NewDocDisplayAssemblyState(fixture.input)
			if err != nil {
				test.Fatal(err)
			}
			pending := DocDisplayPage{Version: 2, Source: state.Source, Text: scenario.text, Runs: []DocDisplayRun{{To: len(scenario.text), SourceFrom: 0, SourceTo: len(scenario.text), Linear: true}}}
			raw, err := encodeDocDisplayAssemblyPending(fixture.input.source, pending)
			if err != nil {
				test.Fatal(err)
			}
			state.Pending, state.Cursor = HashContent(raw), fixture.cursor(test, len(scenario.text))
			fixture.objects[state.Pending] = raw
			before := assemblyTestEncode(test, fixture.input, state)
			step, err := StepDocDisplayAssembly(context.Background(), fixture.input, state, fixture.load)
			if err != nil || len(fixture.requests) != 0 || step.State.Pending != "" || step.State.Output.TextBytes != len(scenario.text) || !bytes.Equal(step.State.Cursor, state.Cursor) || !bytes.Equal(before, assemblyTestEncode(test, fixture.input, state)) {
				test.Fatal("full pending page was replayed or lost progress", err)
			}
			fixture.save(test, step.Objects)
			if err := ValidateDocDisplayAssemblyCheckpoint(context.Background(), fixture.input, step.State, fixture.load); err != nil {
				test.Fatal(err)
			}
		})
	}
}

func TestDocDisplayAssemblyZeroSourceAndMissingInput(test *testing.T) {
	fixture := newAssemblyTestFixture(test, "", 0, false)
	state, generation, pages := fixture.build(test)
	if !state.Output.Done || generation.Pages.Count != 0 || len(pages) != 0 || len(fixture.requests) != 0 || len(fixture.objects) != 0 {
		test.Fatal("empty display replayed input or staged objects")
	}
	if err := ValidateDocDisplayAssemblyCheckpoint(context.Background(), fixture.input, state, nil); err != nil {
		test.Fatal(err)
	}
	if result, objects, err := FinishDocDisplayAssembly(context.Background(), fixture.input, state, nil); err != nil || result.Pages.Count != 0 || len(objects) != 0 {
		test.Fatal(result, objects, err)
	}
	if step, err := StepDocDisplayAssembly(context.Background(), fixture.input, state, nil); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(step, DocDisplayAssemblyStep{}) {
		test.Fatal("completed state was replayed", step, err)
	}
	for index, mutate := range []func(*DocDisplayAssemblyInput){
		func(input *DocDisplayAssemblyInput) { *input = DocDisplayAssemblyInput{} },
		func(input *DocDisplayAssemblyInput) { input.source = DocDisplayProjectionSource{} },
		func(input *DocDisplayAssemblyInput) { input.initial = nil },
		func(input *DocDisplayAssemblyInput) {
			input.initial = bytes.Repeat([]byte("x"), MaxDocDisplaySourceBytes+1)
		},
		func(input *DocDisplayAssemblyInput) { input.cursor = nil },
		func(input *DocDisplayAssemblyInput) { input.read = nil },
		func(input *DocDisplayAssemblyInput) {
			input.cursor = func(json.RawMessage) (int, error) { return 1, nil }
		},
	} {
		input := fixture.input
		mutate(&input)
		if invalid, err := NewDocDisplayAssemblyState(input); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(invalid, DocDisplayAssemblyState{}) {
			test.Fatal("invalid input accepted", index, err)
		}
	}
}

func TestDocDisplayAssemblyRejectsBodyFreeCursorProgress(test *testing.T) {
	for _, pending := range []bool{false, true} {
		test.Run(fmt.Sprintf("pending-%t", pending), func(test *testing.T) {
			fixture := newAssemblyTestFixture(test, strings.Repeat("a", DocDisplayPageRuns+1), 1, false)
			state, err := NewDocDisplayAssemblyState(fixture.input)
			if err != nil {
				test.Fatal(err)
			}
			if pending {
				state = fixture.step(test, state)
			}
			calls := 0
			fixture.input.read = func(_ context.Context, raw json.RawMessage, _, _ int, _ func(context.Context, ContentHash) ([]byte, error)) (DocDisplayPage, json.RawMessage, error) {
				calls++
				var cursor assemblyTestCursor
				if err := json.Unmarshal(raw, &cursor); err != nil {
					return DocDisplayPage{}, nil, err
				}
				cursor.Phase++
				next, err := canonicalJSON(cursor)
				return DocDisplayPage{Version: 2, Source: cursor.Source, From: cursor.Position}, next, err
			}
			before := assemblyTestEncode(test, fixture.input, state)
			step, err := StepDocDisplayAssembly(context.Background(), fixture.input, state, fixture.load)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(step, DocDisplayAssemblyStep{}) || calls != 1 || !bytes.Equal(before, assemblyTestEncode(test, fixture.input, state)) {
				test.Fatal("zero-text read changed state or produced effects", err)
			}
		})
	}
}

func TestDocDisplayAssemblyRequiresExactInitialCursor(test *testing.T) {
	for _, text := range []string{"", "body"} {
		fixture := newAssemblyTestFixture(test, text, 1, false)
		state, err := NewDocDisplayAssemblyState(fixture.input)
		if err != nil {
			test.Fatal(err)
		}
		var cursor assemblyTestCursor
		if err := json.Unmarshal(state.Cursor, &cursor); err != nil {
			test.Fatal(err)
		}
		cursor.Phase++
		state.Cursor, err = canonicalJSON(cursor)
		if err != nil {
			test.Fatal(err)
		}
		if position, err := fixture.input.cursor(state.Cursor); err != nil || position != 0 {
			test.Fatal("fixture must have a source-bound zero-position cursor", err)
		}
		if err := state.Validate(fixture.input); !errors.Is(err, ErrIntegrity) {
			test.Fatal("different zero-position cursor accepted as initial", err)
		}
		if step, err := StepDocDisplayAssembly(context.Background(), fixture.input, state, fixture.load); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(step, DocDisplayAssemblyStep{}) || len(fixture.requests) != 0 {
			test.Fatal("forged initial cursor replayed input", err)
		}
	}
}

func assemblyTestCheckpointWithPendingAndSealed(test *testing.T) (*assemblyTestFixture, DocDisplayAssemblyState) {
	test.Helper()
	fixture := newAssemblyTestFixture(test, strings.Repeat("x", 3_000), DocDisplayPageRuns, true)
	state, err := NewDocDisplayAssemblyState(fixture.input)
	if err != nil {
		test.Fatal(err)
	}
	state = fixture.step(test, state)
	fixture.chunk = 8
	state = fixture.step(test, state)
	if state.Output.Directory.Count != 1 || state.Output.TextBytes != DocDisplayPageRuns || state.Pending == "" {
		test.Fatal("fixture needs a pending and a sealed page")
	}
	return fixture, state
}

func assemblyTestReplacePage(test *testing.T, fixture *assemblyTestFixture, state *DocDisplayAssemblyState, pending bool, mutate func(*DocDisplayPage)) {
	test.Helper()
	hash := state.Pending
	if !pending {
		hash = state.Output.Directory.Levels[0][0]
	}
	var page DocDisplayPage
	if err := json.Unmarshal(fixture.objects[hash], &page); err != nil {
		test.Fatal(err)
	}
	mutate(&page)
	raw, err := canonicalJSON(page)
	if err != nil {
		test.Fatal(err)
	}
	hash = HashContent(raw)
	fixture.objects[hash] = raw
	if pending {
		state.Pending = hash
	} else {
		state.Output.Directory.Levels[0][0] = hash
	}
}

func TestDocDisplayAssemblyRejectsCorruptCheckpointsBeforeReplay(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		mutate func(*testing.T, *assemblyTestFixture, *DocDisplayAssemblyState)
		want   error
	}{
		{"version", func(_ *testing.T, _ *assemblyTestFixture, state *DocDisplayAssemblyState) { state.Version++ }, ErrIntegrity},
		{"source", func(_ *testing.T, _ *assemblyTestFixture, state *DocDisplayAssemblyState) {
			state.Source = HashContent([]byte("other source"))
		}, ErrIntegrity},
		{"input-source", func(_ *testing.T, fixture *assemblyTestFixture, _ *DocDisplayAssemblyState) {
			fixture.input.source.Plan = HashContent([]byte("other plan"))
		}, ErrIntegrity},
		{"output-source", func(_ *testing.T, _ *assemblyTestFixture, state *DocDisplayAssemblyState) {
			state.Output.Source = HashContent([]byte("other source"))
		}, ErrIntegrity},
		{"cursor-json", func(_ *testing.T, _ *assemblyTestFixture, state *DocDisplayAssemblyState) {
			state.Cursor = json.RawMessage(`{"broken":`)
		}, ErrIntegrity},
		{"cursor-source", func(test *testing.T, _ *assemblyTestFixture, state *DocDisplayAssemblyState) {
			var cursor assemblyTestCursor
			if err := json.Unmarshal(state.Cursor, &cursor); err != nil {
				test.Fatal(err)
			}
			cursor.Source = HashContent([]byte("other cursor source"))
			state.Cursor, _ = canonicalJSON(cursor)
		}, ErrIntegrity},
		{"cursor-pending-end", func(test *testing.T, fixture *assemblyTestFixture, state *DocDisplayAssemblyState) {
			state.Cursor = fixture.cursor(test, DocDisplayPageRuns+9)
		}, ErrIntegrity},
		{"cursor-skips-pending", func(test *testing.T, fixture *assemblyTestFixture, state *DocDisplayAssemblyState) {
			state.Cursor = fixture.cursor(test, DocDisplayPageRuns)
		}, ErrIntegrity},
		{"cursor-before-output", func(test *testing.T, fixture *assemblyTestFixture, state *DocDisplayAssemblyState) {
			state.Cursor = fixture.cursor(test, DocDisplayPageRuns-1)
		}, ErrIntegrity},
		{"pending-missing-hash", func(_ *testing.T, _ *assemblyTestFixture, state *DocDisplayAssemblyState) { state.Pending = "" }, ErrIntegrity},
		{"pending-invalid-hash", func(_ *testing.T, _ *assemblyTestFixture, state *DocDisplayAssemblyState) { state.Pending = "invalid" }, ErrIntegrity},
		{"pending-missing-object", func(_ *testing.T, fixture *assemblyTestFixture, state *DocDisplayAssemblyState) {
			delete(fixture.objects, state.Pending)
		}, ErrNotFound},
		{"pending-hash-mismatch", func(_ *testing.T, fixture *assemblyTestFixture, state *DocDisplayAssemblyState) {
			fixture.objects[state.Pending] = []byte(`{}`)
		}, ErrIntegrity},
		{"last-missing-object", func(_ *testing.T, fixture *assemblyTestFixture, state *DocDisplayAssemblyState) {
			delete(fixture.objects, state.Output.Directory.Levels[0][0])
		}, ErrNotFound},
		{"last-hash-mismatch", func(_ *testing.T, fixture *assemblyTestFixture, state *DocDisplayAssemblyState) {
			fixture.objects[state.Output.Directory.Levels[0][0]] = []byte(`{}`)
		}, ErrIntegrity},
		{"pending-version", func(test *testing.T, fixture *assemblyTestFixture, state *DocDisplayAssemblyState) {
			assemblyTestReplacePage(test, fixture, state, true, func(page *DocDisplayPage) { page.Version = 1 })
		}, ErrIntegrity},
		{"pending-source", func(test *testing.T, fixture *assemblyTestFixture, state *DocDisplayAssemblyState) {
			assemblyTestReplacePage(test, fixture, state, true, func(page *DocDisplayPage) { page.Source = HashContent([]byte("other page source")) })
		}, ErrIntegrity},
		{"pending-index", func(test *testing.T, fixture *assemblyTestFixture, state *DocDisplayAssemblyState) {
			assemblyTestReplacePage(test, fixture, state, true, func(page *DocDisplayPage) { page.Index, page.From = 2, DocDisplayPageRuns*2 })
		}, ErrIntegrity},
		{"pending-from", func(test *testing.T, fixture *assemblyTestFixture, state *DocDisplayAssemblyState) {
			assemblyTestReplacePage(test, fixture, state, true, func(page *DocDisplayPage) { page.From++ })
		}, ErrIntegrity},
		{"pending-end", func(test *testing.T, fixture *assemblyTestFixture, state *DocDisplayAssemblyState) {
			assemblyTestReplacePage(test, fixture, state, true, func(page *DocDisplayPage) { page.Text += "x"; page.Runs[len(page.Runs)-1].To++ })
		}, ErrIntegrity},
		{"last-version", func(test *testing.T, fixture *assemblyTestFixture, state *DocDisplayAssemblyState) {
			assemblyTestReplacePage(test, fixture, state, false, func(page *DocDisplayPage) { page.Version = 1 })
		}, ErrIntegrity},
		{"last-source", func(test *testing.T, fixture *assemblyTestFixture, state *DocDisplayAssemblyState) {
			assemblyTestReplacePage(test, fixture, state, false, func(page *DocDisplayPage) { page.Source = HashContent([]byte("other page source")) })
		}, ErrIntegrity},
		{"last-index", func(test *testing.T, fixture *assemblyTestFixture, state *DocDisplayAssemblyState) {
			assemblyTestReplacePage(test, fixture, state, false, func(page *DocDisplayPage) { page.Index, page.From = 1, DocDisplayPageRuns })
		}, ErrIntegrity},
		{"last-from", func(test *testing.T, fixture *assemblyTestFixture, state *DocDisplayAssemblyState) {
			assemblyTestReplacePage(test, fixture, state, false, func(page *DocDisplayPage) { page.From++ })
		}, ErrIntegrity},
		{"last-end", func(test *testing.T, fixture *assemblyTestFixture, state *DocDisplayAssemblyState) {
			assemblyTestReplacePage(test, fixture, state, false, func(page *DocDisplayPage) { page.Text += "x"; page.Runs[len(page.Runs)-1].To++ })
		}, ErrIntegrity},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture, state := assemblyTestCheckpointWithPendingAndSealed(test)
			scenario.mutate(test, fixture, &state)
			before := state
			before.Cursor = bytes.Clone(state.Cursor)
			before.Output.Directory = state.Output.Directory.clone()
			reads := len(fixture.requests)
			step, err := StepDocDisplayAssembly(context.Background(), fixture.input, state, fixture.load)
			if !errors.Is(err, scenario.want) || !reflect.DeepEqual(step, DocDisplayAssemblyStep{}) || len(fixture.requests) != reads || !reflect.DeepEqual(state, before) {
				test.Fatal("corruption replayed input or escaped with effects", err)
			}
			if err := ValidateDocDisplayAssemblyCheckpoint(context.Background(), fixture.input, state, fixture.load); !errors.Is(err, scenario.want) {
				test.Fatal("checkpoint validation accepted corruption", err)
			}
		})
	}
}

func TestDocDisplayAssemblyCanonicalCodecsAndCaps(test *testing.T) {
	fixture, state := assemblyTestCheckpointWithPendingAndSealed(test)
	raw := assemblyTestEncode(test, fixture.input, state)
	for index, malformed := range [][]byte{
		nil,
		append(bytes.Clone(raw), ' '),
		append([]byte(`{"unknown":true,`), raw[1:]...),
		append([]byte(`{"version":1,`), raw[1:]...),
		bytes.Repeat([]byte("x"), MaxDocDisplayStateBytes+1),
	} {
		if decoded, err := DecodeDocDisplayAssemblyState(fixture.input, HashContent(malformed), malformed); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocDisplayAssemblyState{}) {
			test.Fatal("noncanonical checkpoint accepted", index, err)
		}
	}
	if _, err := DecodeDocDisplayAssemblyState(fixture.input, HashContent([]byte("wrong")), raw); !errors.Is(err, ErrIntegrity) {
		test.Fatal("checkpoint hash mismatch accepted", err)
	}
	pageRaw := fixture.objects[state.Pending]
	for index, malformed := range [][]byte{
		nil,
		append(bytes.Clone(pageRaw), ' '),
		append([]byte(`{"unknown":true,`), pageRaw[1:]...),
		append([]byte(`{"version":2,`), pageRaw[1:]...),
		bytes.Repeat([]byte("x"), MaxDocDisplayPageBytes+1),
	} {
		if decoded, err := decodeDocDisplayAssemblyPending(fixture.input.source, HashContent(malformed), malformed); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocDisplayPage{}) {
			test.Fatal("noncanonical pending page accepted", index, err)
		}
	}
	for _, length := range []int{12_000, DocDisplayPageTextBytes} {
		large := newAssemblyTestFixture(test, strings.Repeat("<", length), 0, false)
		hash, err := large.input.source.Hash()
		if err != nil {
			test.Fatal(err)
		}
		page := DocDisplayPage{Version: 2, Source: hash, Text: large.text, Runs: []DocDisplayRun{{To: length, SourceFrom: 0, SourceTo: length, Linear: true}}}
		encoded, err := encodeDocDisplayAssemblyPending(large.input.source, page)
		if length == 12_000 {
			if err != nil || len(encoded) <= MaxDocDisplayStateBytes || len(encoded) > MaxDocDisplayPageBytes {
				test.Fatal("pending payload incorrectly uses the checkpoint cap", len(encoded), err)
			}
			if decoded, err := decodeDocDisplayAssemblyPending(large.input.source, HashContent(encoded), encoded); err != nil || decoded.Text != large.text {
				test.Fatal(err)
			}
		} else if !errors.Is(err, ErrIntegrity) || encoded != nil {
			test.Fatal("pending payload exceeded its encoded cap", len(encoded), err)
		}
	}
}

func TestDocDisplayAssemblyRejectsInvalidReadWithoutEffects(test *testing.T) {
	for index, mutate := range []func(*DocDisplayPage, *json.RawMessage){
		func(page *DocDisplayPage, _ *json.RawMessage) { page.Version++ },
		func(page *DocDisplayPage, _ *json.RawMessage) { page.Source = HashContent([]byte("other source")) },
		func(page *DocDisplayPage, _ *json.RawMessage) { page.From++ },
		func(page *DocDisplayPage, _ *json.RawMessage) { page.Text = "\xff" },
		func(page *DocDisplayPage, _ *json.RawMessage) { page.Runs = nil },
		func(page *DocDisplayPage, _ *json.RawMessage) { page.Runs[0].To-- },
		func(page *DocDisplayPage, _ *json.RawMessage) { page.Runs[0].SourceFrom = -1 },
		func(page *DocDisplayPage, _ *json.RawMessage) { page.Runs[0].SourceTo++ },
		func(_ *DocDisplayPage, cursor *json.RawMessage) { *cursor = json.RawMessage(`{}`) },
		func(_ *DocDisplayPage, cursor *json.RawMessage) { *cursor = append(bytes.Clone(*cursor), ' ') },
	} {
		test.Run(fmt.Sprint(index), func(test *testing.T) {
			fixture := newAssemblyTestFixture(test, strings.Repeat("a", DocDisplayPageRuns+1), 16, false)
			state, err := NewDocDisplayAssemblyState(fixture.input)
			if err != nil {
				test.Fatal(err)
			}
			read := fixture.input.read
			fixture.input.read = func(ctx context.Context, raw json.RawMessage, maxBytes, maxRuns int, load func(context.Context, ContentHash) ([]byte, error)) (DocDisplayPage, json.RawMessage, error) {
				page, cursor, err := read(ctx, raw, maxBytes, maxRuns, load)
				mutate(&page, &cursor)
				return page, cursor, err
			}
			before := assemblyTestEncode(test, fixture.input, state)
			step, err := StepDocDisplayAssembly(context.Background(), fixture.input, state, fixture.load)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(step, DocDisplayAssemblyStep{}) || !bytes.Equal(before, assemblyTestEncode(test, fixture.input, state)) {
				test.Fatal("invalid fragment produced effects or mutated prior", err)
			}
		})
	}
}

func TestDocDisplayAssemblyCancellationAndReadErrorsAreAtomic(test *testing.T) {
	test.Run("before-read", func(test *testing.T) {
		fixture := newAssemblyTestFixture(test, "body", 1, false)
		state, err := NewDocDisplayAssemblyState(fixture.input)
		if err != nil {
			test.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		step, err := StepDocDisplayAssembly(ctx, fixture.input, state, fixture.load)
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(step, DocDisplayAssemblyStep{}) || len(fixture.requests) != 0 {
			test.Fatal(step, err)
		}
	})
	for _, failure := range []error{context.Canceled, ErrNotFound} {
		test.Run(failure.Error(), func(test *testing.T) {
			fixture := newAssemblyTestFixture(test, strings.Repeat("a", DocDisplayPageRuns+1), 0, false)
			state, err := NewDocDisplayAssemblyState(fixture.input)
			if err != nil {
				test.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			read := fixture.input.read
			fixture.input.read = func(ctx context.Context, raw json.RawMessage, maxBytes, maxRuns int, load func(context.Context, ContentHash) ([]byte, error)) (DocDisplayPage, json.RawMessage, error) {
				page, cursor, err := read(ctx, raw, maxBytes, maxRuns, load)
				raw[0] = 'x'
				if errors.Is(failure, context.Canceled) {
					cancel()
					return page, cursor, err
				}
				return page, cursor, failure
			}
			before := assemblyTestEncode(test, fixture.input, state)
			step, err := StepDocDisplayAssembly(ctx, fixture.input, state, fixture.load)
			if !errors.Is(err, failure) || !reflect.DeepEqual(step, DocDisplayAssemblyStep{}) || !bytes.Equal(before, assemblyTestEncode(test, fixture.input, state)) {
				test.Fatal("read cancellation or error changed prior state", err)
			}
		})
	}
	for _, pending := range []bool{false, true} {
		test.Run(fmt.Sprintf("checkpoint-load-pending-%t", pending), func(test *testing.T) {
			fixture, state := assemblyTestCheckpointWithPendingAndSealed(test)
			selected := state.Output.Directory.Levels[0][0]
			if pending {
				selected = state.Pending
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			load := func(ctx context.Context, hash ContentHash) ([]byte, error) {
				raw, err := fixture.load(ctx, hash)
				if hash == selected {
					cancel()
				}
				return raw, err
			}
			before := assemblyTestEncode(test, fixture.input, state)
			reads := len(fixture.requests)
			step, err := StepDocDisplayAssembly(ctx, fixture.input, state, load)
			if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(step, DocDisplayAssemblyStep{}) || len(fixture.requests) != reads || !bytes.Equal(before, assemblyTestEncode(test, fixture.input, state)) {
				test.Fatal("checkpoint cancellation replayed input or produced effects", err)
			}
			if err := ValidateDocDisplayAssemblyCheckpoint(ctx, fixture.input, state, load); !errors.Is(err, context.Canceled) {
				test.Fatal(err)
			}
			if generation, objects, err := FinishDocDisplayAssembly(ctx, fixture.input, state, load); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(generation, DocDisplayProjectionGeneration{}) || objects != nil {
				test.Fatal(generation, objects, err)
			}
		})
	}
}

func TestDocDisplayAssemblyFinishVerifiesCompletedCheckpoint(test *testing.T) {
	fixture := newAssemblyTestFixture(test, strings.Repeat("a", DocDisplayPageRuns*2), DocDisplayPageRuns, true)
	state, _, _ := fixture.build(test)
	before := assemblyTestEncode(test, fixture.input, state)
	last := state.Output.Directory.Levels[0][len(state.Output.Directory.Levels[0])-1]
	fixture.objects[last] = []byte(`{}`)
	if generation, objects, err := FinishDocDisplayAssembly(context.Background(), fixture.input, state, fixture.load); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(generation, DocDisplayProjectionGeneration{}) || objects != nil {
		test.Fatal("finish accepted a corrupt sealed page", generation, objects, err)
	}
	if !bytes.Equal(before, assemblyTestEncode(test, fixture.input, state)) {
		test.Fatal("failed finish changed the checkpoint")
	}
}
