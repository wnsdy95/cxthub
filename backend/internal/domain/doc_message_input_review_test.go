package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

func messageInputReviewSource(test *testing.T, raw []byte) (DocMessageInputSource, DocFieldDiscoveryState, DocRangeEntry) {
	test.Helper()
	discovery, err := discoverDocFieldReader(context.Background(), HashContent(raw), len(raw), bytes.NewReader(raw))
	if err != nil {
		test.Fatal(err)
	}
	metadata, err := docRangeEntry(0, 0, raw, 0)
	if err != nil {
		test.Fatal(err)
	}
	source, err := discovery.MessageInputSource(metadata)
	if err != nil {
		test.Fatal(err)
	}
	return source, discovery, metadata
}

func messageInputReviewLoad(objects map[ContentHash][]byte) func(context.Context, ContentHash) ([]byte, error) {
	return func(_ context.Context, hash ContentHash) ([]byte, error) {
		raw, found := objects[hash]
		if !found {
			return nil, ErrNotFound
		}
		return bytes.Clone(raw), nil
	}
}

func messageInputReviewPage(test *testing.T, fixture messageInputFixture, index int) DocMessageInputPage {
	test.Helper()
	load := messageInputReviewLoad(fixture.objects)
	hash, err := ResolveDocRangeLeaf(context.Background(), fixture.generation.Pages, index, load)
	if err != nil {
		test.Fatal(err)
	}
	page, err := DecodeDocMessageInputPage(fixture.source, hash, fixture.objects[hash])
	if err != nil {
		test.Fatal(err)
	}
	return page
}

func TestDocMessageInputReviewAdmissionRequiresCompletedMatchingDiscovery(test *testing.T) {
	raw := []byte(`{"kind":"message","blocks":[{"type":"text","text":"safe"}],"opaque":"` + strings.Repeat("x", 100) + `"}`)
	source, discovery, metadata := messageInputReviewSource(test, raw)
	for name, mutate := range map[string]func(*DocRangeEntry){
		"event-hash":  func(entry *DocRangeEntry) { entry.Hash = HashContent([]byte("other event")) },
		"event-bytes": func(entry *DocRangeEntry) { entry.Length-- },
		"kind":        func(entry *DocRangeEntry) { entry.Kind = EventToolResult },
	} {
		test.Run(name, func(test *testing.T) {
			changed := metadata
			mutate(&changed)
			if _, err := discovery.MessageInputSource(changed); !errors.Is(err, ErrIntegrity) {
				test.Fatal("mismatching range metadata admitted", err)
			}
		})
	}
	initial, err := NewDocFieldDiscoveryState(HashContent(raw), len(raw))
	if err != nil {
		test.Fatal(err)
	}
	step, err := AdvanceDocFieldDiscovery(context.Background(), initial, raw[:64])
	if err != nil {
		test.Fatal(err)
	}
	partial, err := step.State()
	if err != nil {
		test.Fatal(err)
	}
	if _, err := partial.MessageInputSource(metadata); !errors.Is(err, ErrIntegrity) {
		test.Fatal("unfinished discovery admitted", err)
	}
	for _, flags := range []struct{ compact, seed bool }{{true, false}, {false, true}, {true, true}} {
		changed := metadata
		changed.CompactSummary, changed.SeedSummary = flags.compact, flags.seed
		selected, err := discovery.MessageInputSource(changed)
		if err != nil || !selected.Summary || selected.Blocks != source.Blocks {
			test.Fatal("matching summary metadata not preserved", selected, err)
		}
	}
}

func TestDocMessageInputReviewCanonicalDecodersAndProfileIsolation(test *testing.T) {
	fixture := buildMessageInputFixture(test, []byte(`{"kind":"message","blocks":[{"type":"text","text":"safe"}]}`))
	page := messageInputReviewPage(test, fixture, 0)
	for _, scenario := range []struct {
		name   string
		value  any
		limit  int
		decode func(ContentHash, []byte) error
	}{
		{"source", fixture.source, MaxDocDisplaySourceBytes, func(hash ContentHash, raw []byte) error {
			_, err := DecodeDocMessageInputSource(hash, raw)
			return err
		}},
		{"state", fixture.state, MaxDocDisplayStateBytes, func(hash ContentHash, raw []byte) error {
			_, err := DecodeDocMessageInputState(fixture.source, hash, raw)
			return err
		}},
		{"page", page, MaxDocMessageInputPageBytes, func(hash ContentHash, raw []byte) error {
			_, err := DecodeDocMessageInputPage(fixture.source, hash, raw)
			return err
		}},
		{"generation", fixture.generation, MaxDocDisplaySourceBytes, func(hash ContentHash, raw []byte) error {
			_, err := DecodeDocMessageInputGeneration(hash, raw)
			return err
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			raw, err := canonicalJSON(scenario.value)
			if err != nil || scenario.decode(HashContent(raw), raw) != nil {
				test.Fatal("canonical fixture rejected", err)
			}
			for _, malformed := range [][]byte{
				append(bytes.Clone(raw), ' '),
				append([]byte(`{"unknown":true,`), raw[1:]...),
				append([]byte(`{"version":1,`), raw[1:]...),
				bytes.Replace(raw, []byte(`"version"`), []byte(`"Version"`), 1),
				bytes.Repeat([]byte(" "), scenario.limit+1),
			} {
				if err := scenario.decode(HashContent(malformed), malformed); !errors.Is(err, ErrIntegrity) {
					test.Fatal("noncanonical or oversized descriptor accepted", err)
				}
			}
			if err := scenario.decode(HashContent([]byte("other descriptor")), raw); !errors.Is(err, ErrIntegrity) {
				test.Fatal("descriptor hash mismatch accepted", err)
			}
		})
	}
	for _, profile := range []string{DocDecodedTextProfile, DocDisplayStringProfile, "PUBLIC-MESSAGE-INPUT-V1", ""} {
		changed := fixture.source
		changed.Profile = profile
		if changed.Validate() == nil || fixture.state.Validate(changed) == nil || page.Validate(changed) == nil {
			test.Fatal("foreign profile accepted", profile)
		}
	}
	for _, bound := range []int{math.MinInt, -1, math.MaxInt} {
		changed := fixture.source
		changed.Blocks.From = bound
		if !errors.Is(changed.Validate(), ErrIntegrity) {
			test.Fatal("out-of-event block span accepted", bound)
		}
		changed = fixture.source
		changed.Blocks.To = bound
		if !errors.Is(changed.Validate(), ErrIntegrity) {
			test.Fatal("overflowed block span accepted", bound)
		}
	}
}

func TestDocMessageInputReviewRejectsImpossiblePageOrdinals(test *testing.T) {
	raw := []byte(`{"kind":"message","blocks":[` + strings.Repeat(`{},`, DocMessageInputPageEntries) + `{}]}`)
	fixture := buildMessageInputFixture(test, raw)
	first := messageInputReviewPage(test, fixture, 0)
	last := messageInputReviewPage(test, fixture, 1)
	last.Entries[0].Anchor = first.Entries[0].Anchor
	malformed, err := canonicalJSON(last)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := DecodeDocMessageInputPage(fixture.source, HashContent(malformed), malformed); !errors.Is(err, ErrIntegrity) {
		test.Errorf("page ordinal %d accepted block %d at first block's anchor %+v: %v", last.Index, last.Entries[0].Index, last.Entries[0].Anchor, err)
	}
	firstRaw, err := first.Encode(fixture.source)
	if err != nil {
		test.Fatal(err)
	}
	objects := map[ContentHash][]byte{HashContent(firstRaw): firstRaw, HashContent(malformed): malformed}
	var accumulator DocRangeDirectoryAccumulator
	for _, hash := range []ContentHash{HashContent(firstRaw), HashContent(malformed)} {
		var staged []DocRangeObject
		accumulator, staged, err = accumulator.Append(context.Background(), hash)
		if err != nil {
			test.Fatal(err)
		}
		for _, object := range staged {
			objects[object.Hash] = object.Bytes
		}
	}
	root, staged, err := accumulator.Finish(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range staged {
		objects[object.Hash] = object.Bytes
	}
	generation := fixture.generation
	generation.Pages = root
	inputs, err := ReadDocMessageInputs(context.Background(), generation, 1, messageInputReviewLoad(objects))
	if !errors.Is(err, ErrIntegrity) || len(inputs) != 0 {
		test.Errorf("selected page returned ordinal 64 from ordinal 0's anchor: inputs=%d error=%v", len(inputs), err)
	}
}

func TestDocMessageInputReviewRejectsOverlappingFieldTokens(test *testing.T) {
	fixture := buildMessageInputFixture(test, []byte(`{"kind":"message","blocks":[{"type":"text","name":"asset","text":"body"}]}`))
	for _, field := range []string{"text", "name"} {
		test.Run(field, func(test *testing.T) {
			page := messageInputReviewPage(test, fixture, 0)
			if field == "text" {
				page.Entries[0].Text = page.Entries[0].Type
			} else {
				page.Entries[0].Name = page.Entries[0].Type
			}
			raw, err := canonicalJSON(page)
			if err != nil {
				test.Fatal(err)
			}
			if _, err := DecodeDocMessageInputPage(fixture.source, HashContent(raw), raw); !errors.Is(err, ErrIntegrity) {
				test.Fatal("distinct direct fields accepted the same JSON token", err)
			}
		})
	}
}

func TestDocMessageInputReviewMaximumDepthPartialCheckpoint(test *testing.T) {
	const nested = docFieldDiscoveryDepth - 3
	raw := []byte(`{"kind":"message","blocks":[` + strings.Repeat(`{},`, DocMessageInputPageEntries-1) + `{"opaque":` + strings.Repeat("[", nested) + `"` + strings.Repeat("PRIVATE", 20000) + `"` + strings.Repeat("]", nested) + `,"type":"text","text":"safe"}]}`)
	if !json.Valid(raw) {
		test.Fatal("maximum-depth event is not valid JSON")
	}
	source, _, _ := messageInputReviewSource(test, raw)
	initial, err := NewDocMessageInputState(source)
	if err != nil {
		test.Fatal(err)
	}
	step, err := StepDocMessageInput(context.Background(), source, initial, func(_ context.Context, from, length int) ([]byte, error) {
		if from <= source.Blocks.From || length <= 0 || length > 4096 || from > source.Blocks.To-length {
			test.Fatal("out-of-source read", from, length)
		}
		return raw[from : from+length], nil
	}, nil)
	if err != nil {
		test.Fatal(err)
	}
	state := step.State
	if step.ReadBytes != DocDisplayStepSourceBytes || state.Syntax == nil || len(state.Syntax.Stack) != nested+1 || len(state.Entries) != DocMessageInputPageEntries-1 {
		test.Fatal("did not checkpoint inside maximum-depth opaque value", state.Next, state.Count, err)
	}
	encoded, err := state.Encode(source)
	if err != nil || len(encoded) > MaxDocDisplayStateBytes || bytes.Contains(encoded, []byte("PRIVATE")) {
		test.Fatal("unbounded or body-retaining checkpoint", len(encoded), err)
	}
	restarted, err := DecodeDocMessageInputState(source, HashContent(encoded), encoded)
	if err != nil || !reflect.DeepEqual(state, restarted) {
		test.Fatal("partial syntax failed canonical restart", err)
	}
	for _, stack := range [][]byte{append(bytes.Clone(state.Syntax.Stack), 0), bytes.Repeat([]byte{docFieldObjectValue}, docFieldDiscoveryDepth+1)} {
		changed := restarted
		syntax := *restarted.Syntax
		syntax.Stack = stack
		changed.Syntax = &syntax
		if !errors.Is(changed.Validate(source), ErrIntegrity) {
			test.Fatal("invalid partial parser stack admitted")
		}
	}
	fixture := buildMessageInputFixture(test, raw)
	inputs := messageFixtureInputs(test, fixture)
	text, err := inputs[len(inputs)-1].TextSource()
	if err != nil || len(inputs) != DocMessageInputPageEntries || messageFixtureString(test, raw, text) != "safe" {
		test.Fatal("maximum-depth resume lost selected fields", len(inputs), err)
	}
}

func TestDocMessageInputReviewLastPageCheckpointLinks(test *testing.T) {
	raw := []byte(`{"kind":"message","blocks":[` + strings.Repeat(`{"type":"text","text":"safe"},`, DocMessageInputPageEntries*2) + `{}]}`)
	source, _, _ := messageInputReviewSource(test, raw)
	initial, err := NewDocMessageInputState(source)
	if err != nil {
		test.Fatal(err)
	}
	step, err := StepDocMessageInput(context.Background(), source, initial, func(_ context.Context, from, length int) ([]byte, error) {
		return raw[from : from+length], nil
	}, nil)
	if err != nil || step.State.Count != DocMessageInputStepEntries || step.State.Done() {
		test.Fatal("expected exact full-page checkpoint before next block", err)
	}
	objects := make(map[ContentHash][]byte)
	for _, object := range step.Objects {
		objects[object.Hash] = object.Bytes
	}
	load := messageInputReviewLoad(objects)
	if err := ValidateDocMessageInputCheckpoint(context.Background(), source, step.State, load); err != nil {
		test.Fatal("valid last-page checkpoint rejected", err)
	}
	for name, mutate := range map[string]func(*DocMessageInputState){
		"last-to":     func(state *DocMessageInputState) { state.LastTo-- },
		"next":        func(state *DocMessageInputState) { state.Next = state.LastTo - 1 },
		"count":       func(state *DocMessageInputState) { state.Count-- },
		"empty-first": func(state *DocMessageInputState) { state.Phase = docMessageInputFirst },
	} {
		test.Run(name, func(test *testing.T) {
			changed := step.State
			mutate(&changed)
			if err := ValidateDocMessageInputCheckpoint(context.Background(), source, changed, load); !errors.Is(err, ErrIntegrity) {
				test.Fatal("inconsistent checkpoint cursor accepted", err)
			}
		})
	}
	fixture := buildMessageInputFixture(test, raw)
	if err := ValidateDocMessageInputCheckpoint(context.Background(), source, fixture.state, messageInputReviewLoad(fixture.objects)); err != nil {
		test.Fatal("valid final partial page rejected", err)
	}
	before, err := step.State.Encode(source)
	if err != nil {
		test.Fatal(err)
	}
	result, err := StepDocMessageInput(context.Background(), source, step.State, func(context.Context, int, int) ([]byte, error) {
		test.Fatal("source read before checkpoint verification")
		return nil, nil
	}, func(context.Context, ContentHash) ([]byte, error) { return nil, ErrNotFound })
	if !errors.Is(err, ErrNotFound) || !reflect.DeepEqual(result, DocMessageInputStep{}) {
		test.Fatal("missing checkpoint page returned partial progress", err)
	}
	after, err := step.State.Encode(source)
	if err != nil || !bytes.Equal(before, after) {
		test.Fatal("failed checkpoint validation mutated retry state", err)
	}
}

func TestDocMessageInputReviewRejectsActiveBlockAtPreviousEnd(test *testing.T) {
	raw := []byte(`{"kind":"message","blocks":[` + strings.Repeat(`{"opaque":0},`, DocMessageInputPageEntries) + `{"opaque":"` + strings.Repeat("x", DocDisplayStepSourceBytes+100) + `","type":"text","text":"safe"}]}`)
	source, _, _ := messageInputReviewSource(test, raw)
	initial, err := NewDocMessageInputState(source)
	if err != nil {
		test.Fatal(err)
	}
	read := func(_ context.Context, from, length int) ([]byte, error) { return raw[from : from+length], nil }
	step, err := StepDocMessageInput(context.Background(), source, initial, read, nil)
	if err != nil || step.State.Syntax == nil || step.State.Directory.Count != 1 || len(step.State.Entries) != 0 || step.State.BlockFrom != step.State.LastTo+1 {
		test.Fatal("expected active block just after a flushed page", err)
	}
	objects := make(map[ContentHash][]byte)
	for _, object := range step.Objects {
		objects[object.Hash] = object.Bytes
	}
	changed := step.State
	changed.BlockFrom = changed.LastTo
	if err := changed.Validate(source); !errors.Is(err, ErrIntegrity) {
		test.Error("active block accepted previous block's end as its opening brace", err)
	}
	if err := ValidateDocMessageInputCheckpoint(context.Background(), source, changed, messageInputReviewLoad(objects)); !errors.Is(err, ErrIntegrity) {
		test.Error("checkpoint accepted active block starting on the preceding comma", err)
	}
	resumed, err := StepDocMessageInput(context.Background(), source, changed, read, messageInputReviewLoad(objects))
	if errors.Is(err, ErrIntegrity) {
		return
	}
	if err != nil || !resumed.State.Done() {
		test.Fatal("unexpected resume result", err)
	}
	for _, object := range resumed.Objects {
		objects[object.Hash] = object.Bytes
	}
	generation, staged, err := FinishDocMessageInput(context.Background(), source, resumed.State)
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range staged {
		objects[object.Hash] = object.Bytes
	}
	inputs, err := ReadDocMessageInputs(context.Background(), generation, 1, messageInputReviewLoad(objects))
	if err != nil {
		test.Fatal(err)
	}
	if len(inputs) != 1 {
		test.Fatal("unexpected selected input count", len(inputs))
	}
	if raw[inputs[0].Anchor().From] != '{' {
		test.Fatalf("corrupted cursor produced a readable non-object anchor: inputs=%d anchor=%+v", len(inputs), inputs[0].Anchor())
	}
}

func TestDocMessageInputReviewDuplicateCaseAndNullSemantics(test *testing.T) {
	for _, scenario := range []struct {
		name, block, text, blockType, nameValue string
		isText, invalid                         bool
	}{
		{name: "corrected-type", block: `{"type":{},"ty\u0070e":"text","text":"safe"}`, text: "safe", isText: true},
		{name: "invalid-final-type", block: `{"type":"text","type":false,"text":"safe"}`, invalid: true},
		{name: "null-final-type", block: `{"type":"text","type":null,"text":"ignored"}`},
		{name: "case-value", block: `{"type":"Text","text":"ignored"}`, blockType: "Text"},
		{name: "case-key", block: `{"TYPE":"text","text":"ignored"}`},
		{name: "corrected-text", block: `{"type":"text","text":[],"te\u0078t":"safe"}`, text: "safe", isText: true},
		{name: "invalid-final-text", block: `{"type":"text","text":"safe","text":0}`, invalid: true},
		{name: "null-final-text", block: `{"type":"text","text":"old","text":null}`, isText: true},
		{name: "corrected-name", block: `{"type":"image","name":{},"na\u006de":"asset"}`, blockType: "image", nameValue: "asset"},
		{name: "invalid-final-name", block: `{"type":"image","name":"asset","name":false}`, invalid: true},
		{name: "null-final-name", block: `{"type":"image","name":"old","name":null}`, blockType: "image"},
		{name: "unused-name", block: `{"type":"text","name":{},"text":"safe"}`, text: "safe", isText: true},
		{name: "nested-fields", block: `{"type":"image","opaque":{"type":"text","text":"locked","name":"locked"}}`, blockType: "image"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			raw := []byte(`{"kind":"message","blocks":[` + scenario.block + `]}`)
			if scenario.invalid {
				source, _, _ := messageInputReviewSource(test, raw)
				initial, err := NewDocMessageInputState(source)
				if err != nil {
					test.Fatal(err)
				}
				result, err := StepDocMessageInput(context.Background(), source, initial, func(_ context.Context, from, length int) ([]byte, error) {
					return raw[from : from+length], nil
				}, nil)
				if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(result, DocMessageInputStep{}) {
					test.Fatal("invalid final exact field admitted", err)
				}
				return
			}
			fixture := buildMessageInputFixture(test, raw)
			inputs := messageFixtureInputs(test, fixture)
			if len(inputs) != 1 || inputs[0].IsText() != scenario.isText {
				test.Fatal("wrong exact block type", len(inputs))
			}
			if scenario.isText {
				text, err := inputs[0].TextSource()
				if err != nil || messageFixtureString(test, raw, text) != scenario.text {
					test.Fatal("wrong final exact text field", err)
				}
			} else {
				blockType, name, err := inputs[0].PlaceholderSources()
				if err != nil || messageFixtureString(test, raw, blockType) != scenario.blockType || messageFixtureString(test, raw, name) != scenario.nameValue {
					test.Fatal("wrong final exact placeholder fields", err)
				}
			}
		})
	}
	for _, suffix := range []string{``, `,"blocks":null`, `,"blo\u0063ks":null`} {
		raw := []byte(`{"kind":"message","blocks":[{"type":"text","text":"old"}]` + suffix + `}`)
		fixture := buildMessageInputFixture(test, raw)
		if (suffix != "") != fixture.source.Empty || (suffix != "") != (fixture.generation.Count == 0) {
			test.Fatal("final null failed to clear prior blocks", suffix, fixture.source)
		}
	}
}

func TestDocMessageInputReviewSelectedReadsAndCancellation(test *testing.T) {
	fixture := buildMessageInputFixture(test, []byte(`{"kind":"message","blocks":[`+strings.Repeat(`{},`, DocMessageInputPageEntries*2)+`{}]}`))
	load := messageInputReviewLoad(fixture.objects)
	selectedHash, err := ResolveDocRangeLeaf(context.Background(), fixture.generation.Pages, 1, load)
	if err != nil {
		test.Fatal(err)
	}
	loads := 0
	inputs, err := ReadDocMessageInputs(context.Background(), fixture.generation, 1, func(ctx context.Context, hash ContentHash) ([]byte, error) {
		loads++
		if hash != fixture.generation.Pages.Hash && hash != selectedHash {
			test.Fatal("unselected descriptor page read", hash)
		}
		return load(ctx, hash)
	})
	if err != nil || len(inputs) != DocMessageInputPageEntries || inputs[0].Index() != DocMessageInputPageEntries || loads != 2 {
		test.Fatal("selected page read lost ordinal or exceeded its path", len(inputs), loads, err)
	}
	for _, index := range []int{-1, fixture.generation.Pages.Count, math.MaxInt} {
		if _, err := ReadDocMessageInputs(context.Background(), fixture.generation, index, func(context.Context, ContentHash) ([]byte, error) {
			test.Fatal("invalid page index caused an object read")
			return nil, nil
		}); !errors.Is(err, ErrIntegrity) {
			test.Fatal("invalid selected index accepted", index, err)
		}
	}
	for _, cancelAt := range []int{0, 1, 2} {
		ctx, cancel := context.WithCancel(context.Background())
		if cancelAt == 0 {
			cancel()
		}
		loads := 0
		inputs, err := ReadDocMessageInputs(ctx, fixture.generation, 1, func(ctx context.Context, hash ContentHash) ([]byte, error) {
			loads++
			if loads == cancelAt {
				cancel()
			}
			return load(ctx, hash)
		})
		cancel()
		if !errors.Is(err, context.Canceled) || inputs != nil || loads > cancelAt {
			test.Fatal("cancelled selected read leaked descriptors or continued loading", cancelAt, loads, len(inputs), err)
		}
	}
}
