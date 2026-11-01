package domain

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type decodedTextFixture struct {
	source        DocDecodedTextSource
	state         DocDecodedTextState
	generation    DocDecodedTextGeneration
	objects       map[ContentHash][]byte
	raw           []byte
	pages         []DocDisplayPage
	steps         int
	maxCheckpoint int
	maxObjects    int
	maxRawRead    int
}

func buildDecodedTextFixture(test *testing.T, raw []byte) decodedTextFixture {
	test.Helper()
	discovery, err := NewDocFieldDiscoveryState(HashContent(raw), len(raw))
	if err != nil {
		test.Fatal(err)
	}
	for !discovery.Done {
		length := min(DocFieldDiscoveryStepBytes, len(raw)-discovery.Next)
		step, err := AdvanceDocFieldDiscovery(context.Background(), discovery, raw[discovery.Next:discovery.Next+length])
		if err != nil {
			test.Fatal("source discovery", err)
		}
		encoded, err := step.State()
		if err != nil {
			test.Fatal(err)
		}
		discovery = encoded
	}
	source, err := discovery.ReasoningTextSource()
	if err != nil {
		test.Fatal("source selection", err)
	}
	return buildDecodedTextSourceFixture(test, raw, source)
}

func buildDecodedTextSourceFixture(test *testing.T, raw []byte, source DocDecodedTextSource) decodedTextFixture {
	test.Helper()
	state, err := NewDocDecodedTextState(source)
	if err != nil {
		test.Fatal(err)
	}
	fixture := decodedTextFixture{source: source, objects: make(map[ContentHash][]byte), raw: raw}
	for !state.Done {
		fixture.steps++
		before, err := state.Encode(source)
		if err != nil {
			test.Fatal("checkpoint before", err)
		}
		reads, objectReads := 0, 0
		step, err := StepDocDecodedText(context.Background(), source, state,
			func(_ context.Context, from, length int) ([]byte, error) {
				if from < source.Token.From || length <= 0 || length > 4096 || from > source.Token.To-length {
					test.Fatal("decoded source overread", from, length, source.Token)
				}
				reads += length
				return raw[from : from+length], nil
			},
			func(_ context.Context, hash ContentHash) ([]byte, error) {
				objectReads++
				body, found := fixture.objects[hash]
				if !found {
					return nil, ErrNotFound
				}
				return body, nil
			})
		if err != nil {
			test.Fatal("decode step", fixture.steps, state.Next, state.TextBytes, err)
		}
		if reads != step.ReadBytes || reads > DocDisplayStepSourceBytes || objectReads > 5 {
			test.Fatal("unbounded decoding", reads, step.ReadBytes, objectReads)
		}
		fixture.maxRawRead, fixture.maxObjects = max(fixture.maxRawRead, reads), max(fixture.maxObjects, objectReads)
		after, err := state.Encode(source)
		if err != nil || !bytes.Equal(before, after) {
			test.Fatal("source checkpoint mutated", err)
		}
		for _, object := range step.Objects {
			if HashContent(object.Bytes) != object.Hash || len(object.Bytes) > max(MaxDocDisplayPageBytes, MaxDocRangeDirectoryBytes) {
				test.Fatal("invalid staged object")
			}
			fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
		}
		if step.Page != nil {
			fixture.pages = append(fixture.pages, *step.Page)
		}
		encoded, err := step.State.Encode(source)
		if err != nil || bytes.Equal(before, encoded) {
			test.Fatal("decode made no progress", err)
		}
		fixture.maxCheckpoint = max(fixture.maxCheckpoint, len(encoded))
		state, err = DecodeDocDecodedTextState(source, HashContent(encoded), encoded)
		if err != nil {
			test.Fatal("cannot resume decode", err)
		}
	}
	generation, objects, err := FinishDocDecodedText(context.Background(), source, state)
	if err != nil {
		test.Fatal("cannot complete decoded input", err)
	}
	for _, object := range objects {
		fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
	}
	fixture.state, fixture.generation = state, generation
	return fixture
}

func TestDocDecodedTextReasoningSourceRequiresVerifiedLastRootField(test *testing.T) {
	raw := []byte(`{"kind":"reasoning","redacted_summary":"old","opaque":{"redacted_summary":"locked"},"redacted\u005fsummary":"**safe**"}`)
	fixture := buildDecodedTextFixture(test, raw)
	if string(raw[fixture.source.Token.From:fixture.source.Token.To]) != `"**safe**"` || fixture.generation.Hash != HashContent([]byte("**safe**")) {
		test.Fatal("did not select final root public token", fixture.source, fixture.generation.Hash)
	}
	for _, raw := range [][]byte{
		[]byte(`{"kind":"reasoning","opaque":{"redacted_summary":"locked"}}`),
		[]byte(`{"kind":"reasoning","redacted_summary":null}`),
		[]byte(`{"kind":"tool_result","output":"not a reasoning source"}`),
		[]byte(`{"kind":"message","blocks":[]}`),
	} {
		discovery, err := discoverDocFieldReader(context.Background(), HashContent(raw), len(raw), bytes.NewReader(raw))
		if err != nil {
			test.Fatal(err)
		}
		if source, err := discovery.ReasoningTextSource(); !errors.Is(err, ErrDocFindDisplayUnavailable) || source != (DocDecodedTextSource{}) {
			test.Fatal("unsupported source accepted", source, err)
		}
	}
	discovery, err := NewDocFieldDiscoveryState(HashContent(raw), len(raw))
	if err != nil {
		test.Fatal(err)
	}
	if _, err := discovery.ReasoningTextSource(); !errors.Is(err, ErrIntegrity) {
		test.Fatal("unfinished discovery accepted", err)
	}
}

func TestDocDecodedTextRetainsWhitespaceEscapesAndSurrogates(test *testing.T) {
	for _, token := range []string{`""`, `" \t\n\r\u00a0 "`, `"a\"b\\c\/d\b\f\n\r\t"`, `"\uD83D\uDE42\uD800X\uDC00\u0000"`, "\"\ud55c\uae00🙂<>&\""} {
		raw := []byte(`{"kind":"reasoning","redacted_summary":` + token + `}`)
		fixture := buildDecodedTextFixture(test, raw)
		var want string
		if json.Unmarshal([]byte(token), &want) != nil {
			test.Fatal("bad token fixture")
		}
		var got strings.Builder
		for _, page := range fixture.pages {
			got.WriteString(page.Text)
			if page.Version != 3 {
				test.Fatal("wrong page profile", page.Version)
			}
		}
		if got.String() != want || fixture.generation.Hash != HashContent([]byte(want)) || fixture.generation.TextBytes != len(want) {
			test.Fatal("decoded body changed", got.String(), want, fixture.generation)
		}
	}
}

func TestDocDecodedTextLargeHashAndLocatorResume(test *testing.T) {
	for _, text := range []string{strings.Repeat("\ud55c\uae00🙂content\n", 14000), strings.Repeat("\\\t\n\r\"", 18000), strings.Repeat("x", 64*1024)} {
		raw, err := json.Marshal(map[string]any{"kind": "reasoning", "redacted_summary": text, "opaque": strings.Repeat("locked", 60000)})
		if err != nil {
			test.Fatal(err)
		}
		fixture := buildDecodedTextFixture(test, raw)
		if fixture.generation.Hash != HashContent([]byte(text)) || fixture.generation.TextBytes != len(text) || fixture.generation.Pages.Count != len(fixture.pages) {
			test.Fatal("large decoded digest changed", fixture.generation)
		}
		encoded, err := fixture.generation.Encode()
		if err != nil {
			test.Fatal(err)
		}
		decoded, err := DecodeDocDecodedTextGeneration(HashContent(encoded), encoded)
		if err != nil || decoded != fixture.generation {
			test.Fatal("generation roundtrip", err)
		}
		test.Logf("event_bytes=%d decoded_bytes=%d pages=%d lookup_pages=%d steps=%d checkpoint_bytes=%d max_raw_read=%d max_hash_tail_object_loads=%d", len(raw), len(text), len(fixture.pages), fixture.generation.Lookup.Count, fixture.steps, fixture.maxCheckpoint, fixture.maxRawRead, fixture.maxObjects)
	}
}

func TestDocDecodedTextNonAlignedDigestResumesFromVerifiedPageTail(test *testing.T) {
	raw := []byte(`{"kind":"reasoning","redacted_summary":"a` + strings.Repeat(`\uD83D\uDE42`, 1800) + `"}`)
	fixture := buildDecodedTextFixture(test, raw)
	want := "a" + strings.Repeat("🙂", 1800)
	if fixture.generation.Hash != HashContent([]byte(want)) || fixture.maxObjects == 0 || fixture.pages[0].From+len(fixture.pages[0].Text)%64 == 0 {
		test.Fatal("fixture did not exercise prior staged tail", fixture.generation.Hash, fixture.maxObjects)
	}
	for _, coordinate := range [][2]int{{2044, 8}, {63, 64}, {4093, 20}} {
		got, err := ReadDocDecodedTextBytes(context.Background(), fixture.generation, coordinate[0], coordinate[1],
			func(_ context.Context, hash ContentHash) ([]byte, error) { return fixture.objects[hash], nil })
		if err != nil || !bytes.Equal(got, []byte(want[coordinate[0]:coordinate[0]+coordinate[1]])) {
			test.Fatal("byte-unaligned digest range changed", coordinate, got, err)
		}
	}
	test.Logf("decoded_bytes=%d pages=%d hash_tail_object_loads=%d checkpoint_bytes=%d", len(want), len(fixture.pages), fixture.maxObjects, fixture.maxCheckpoint)
}

func TestDocDecodedTextMixedFormatsRejectAndFailedStepHasNoEffects(test *testing.T) {
	raw := []byte(`{"kind":"reasoning","redacted_summary":"hello"}`)
	fixture := buildDecodedTextFixture(test, raw)
	page := fixture.pages[0]
	encoded, err := encodeDocDecodedTextPage(fixture.source, page)
	if err != nil {
		test.Fatal(err)
	}
	literal := DocDisplayStringSource{Version: 1, Profile: DocDisplayStringProfile, EventHash: fixture.source.EventHash, EventBytes: fixture.source.EventBytes, Token: fixture.source.Token}
	if _, err := DecodeDocDisplayPage(literal, HashContent(encoded), encoded); !errors.Is(err, ErrIntegrity) {
		test.Fatal("decoded input downgraded to display literal", err)
	}
	page.Version = 1
	alternate, _ := canonicalJSON(page)
	if _, err := DecodeDocDecodedTextPage(fixture.source, HashContent(alternate), alternate); !errors.Is(err, ErrIntegrity) {
		test.Fatal("display literal accepted as decoded input", err)
	}
	state, err := NewDocDecodedTextState(fixture.source)
	if err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	step, err := StepDocDecodedText(ctx, fixture.source, state, func(context.Context, int, int) ([]byte, error) { test.Fatal("cancelled before IO"); return nil, nil }, nil)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(step, DocDecodedTextStep{}) {
		test.Fatal("cancelled step exposed data", step, err)
	}
}

func TestDocDecodedTextMaximumFrontierCheckpointShapeIsBounded(test *testing.T) {
	source := DocDecodedTextSource{Version: 1, Profile: DocDecodedTextProfile, EventHash: HashContent([]byte("synthetic maximum metadata")), EventBytes: MaxConversationDocumentBytes, Token: DocFindSpan{To: MaxConversationDocumentBytes}}
	state, err := NewDocDecodedTextState(source)
	if err != nil {
		test.Fatal(err)
	}
	pages := MaxConversationDocumentBytes/DocDisplayPageRuns - 1
	frontier := func(count int) DocRangeDirectoryAccumulator {
		accumulator := DocRangeDirectoryAccumulator{Count: count}
		for remaining := count; remaining > 0; remaining /= DocRangeDirectoryFanout {
			level := make([]ContentHash, remaining%DocRangeDirectoryFanout)
			for index := range level {
				level[index] = HashContent([]byte("synthetic frontier item"))
			}
			accumulator.Levels = append(accumulator.Levels, level)
		}
		return accumulator
	}
	state.Directory = frontier(pages)
	state.TextBytes = pages * DocDisplayPageRuns
	state.Next, state.HashNext = state.TextBytes+1, state.TextBytes
	binary.BigEndian.PutUint64(state.HashState[len(state.HashState)-8:], uint64(state.HashNext))
	buckets := (state.TextBytes + DocDecodedTextLookupStride - 1) / DocDecodedTextLookupStride
	state.LookupDirectory = frontier(buckets / DocDecodedTextLookupEntries)
	state.LookupEntries = make([]int, buckets%DocDecodedTextLookupEntries)
	for index := range state.LookupEntries {
		state.LookupEntries[index] = pages - 1
	}
	encoded, err := state.Encode(source)
	if err != nil || len(encoded) > MaxDocDisplayStateBytes {
		test.Fatal("frontiers grew beyond checkpoint bound", len(encoded), err)
	}
	if decoded, err := DecodeDocDecodedTextState(source, HashContent(encoded), encoded); err != nil || !reflect.DeepEqual(decoded, state) {
		test.Fatal("maximum shape did not round trip", err)
	}
	test.Logf("logical_data_pages=%d locator_pages=%d checkpoint_bytes=%d", pages, state.LookupDirectory.Count, len(encoded))
}

func TestDocDecodedTextPrivateReaderCacheEndsWithOperation(test *testing.T) {
	fixture := buildDecodedTextFixture(test, []byte(`{"kind":"reasoning","redacted_summary":"cached public body"}`))
	loads := 0
	reader, err := newDocDecodedTextReader(fixture.generation, func(_ context.Context, hash ContentHash) ([]byte, error) {
		loads++
		return fixture.objects[hash], nil
	})
	if err != nil {
		test.Fatal(err)
	}
	first, err := reader.readBytes(context.Background(), 0, 6)
	if err != nil || string(first) != "cached" {
		test.Fatal(first, err)
	}
	previous := loads
	second, err := reader.readBytes(context.Background(), 0, 6)
	if err != nil || !bytes.Equal(first, second) || loads != previous {
		test.Fatal("validated immutable page was reparsed", loads, previous, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := reader.readBytes(ctx, 0, 6); !errors.Is(err, context.Canceled) || got != nil {
		test.Fatal("cache bypassed cancellation", got, err)
	}
	got, err := ReadDocDecodedTextBytes(context.Background(), fixture.generation, 0, 6,
		func(context.Context, ContentHash) ([]byte, error) { return nil, ErrNotFound })
	if !errors.Is(err, ErrNotFound) || got != nil {
		test.Fatal("next operation reused an old page", got, err)
	}
}
