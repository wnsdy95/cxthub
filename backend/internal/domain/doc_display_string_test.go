package domain

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestDocDisplayMaximumFrontierCheckpointIsBounded(test *testing.T) {
	source := DocDisplayStringSource{Version: 1, Profile: DocDisplayStringProfile, EventHash: HashContent([]byte("large event")), EventBytes: MaxConversationDocumentBytes, Token: DocFindSpan{From: 10, To: MaxConversationDocumentBytes - 1}}
	state, err := NewDocDisplayStringState(source)
	if err != nil {
		test.Fatal(err)
	}
	state.Next = source.Token.To - 1
	pages := (source.Token.To - source.Token.From - 3) / DocDisplayPageRuns
	state.TextBytes = (pages-1)*DocDisplayPageRuns + 1
	state.HasContent = true
	state.Directory.Count = pages
	for remaining := state.Directory.Count; remaining > 0; remaining /= DocRangeDirectoryFanout {
		hashes := make([]ContentHash, remaining%DocRangeDirectoryFanout)
		for index := range hashes {
			hashes[index] = HashContent([]byte("directory item"))
		}
		state.Directory.Levels = append(state.Directory.Levels, hashes)
	}
	raw, err := state.Encode(source)
	if err != nil || len(raw) > MaxDocDisplayStateBytes {
		test.Fatal("frontier grew with all page IDs", len(raw), err)
	}
	decoded, err := DecodeDocDisplayStringState(source, HashContent(raw), raw)
	if err != nil || !reflect.DeepEqual(decoded, state) {
		test.Fatal("checkpoint changed", err)
	}
	test.Logf("pages=%d frontier_levels=%d checkpoint_bytes=%d", state.Directory.Count, len(state.Directory.Levels), len(raw))
}

func TestDocDisplayStringMetadataDecodersRejectTampering(test *testing.T) {
	source := DocDisplayStringSource{Version: 1, Profile: DocDisplayStringProfile, EventHash: HashContent([]byte("event")), EventBytes: 100, Token: DocFindSpan{From: 50, To: 60}}
	raw, err := source.Encode()
	if err != nil {
		test.Fatal(err)
	}
	if decoded, err := DecodeDocDisplayStringSource(HashContent(raw), raw); err != nil || decoded != source {
		test.Fatal(decoded, err)
	}
	if decoded, err := DecodeDocDisplayStringSource(HashContent(raw), append(append([]byte{}, raw...), ' ')); !errors.Is(err, ErrIntegrity) || decoded != (DocDisplayStringSource{}) {
		test.Fatal(decoded, err)
	}
	var object map[string]any
	if json.Unmarshal(raw, &object) != nil {
		test.Fatal("invalid source")
	}
	object["unknown"] = true
	unknown, err := canonicalJSON(object)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := DecodeDocDisplayStringSource(HashContent(unknown), unknown); !errors.Is(err, ErrIntegrity) {
		test.Fatal("accepted unknown source property", err)
	}
	emptySource := source
	emptySource.Empty = true
	generation := DocDisplayStringGeneration{Version: 1, Source: emptySource}
	encoded, err := generation.Encode()
	if err != nil {
		test.Fatal(err)
	}
	if decoded, err := DecodeDocDisplayStringGeneration(HashContent(encoded), encoded); err != nil || decoded != generation {
		test.Fatal(decoded, err)
	}
	if decoded, err := DecodeDocDisplayStringGeneration(HashContent(raw), encoded); !errors.Is(err, ErrIntegrity) || decoded != (DocDisplayStringGeneration{}) {
		test.Fatal(decoded, err)
	}
	for _, bad := range []DocDisplayStringSource{
		{}, {Version: 2, Profile: DocDisplayStringProfile, EventHash: source.EventHash, EventBytes: 100, Token: source.Token},
		{Version: 1, Profile: "display-v1", EventHash: source.EventHash, EventBytes: 100, Token: source.Token},
		{Version: 1, Profile: DocDisplayStringProfile, EventHash: source.EventHash, EventBytes: 100, Token: DocFindSpan{From: 50, To: 101}},
		{Version: 1, Profile: DocDisplayStringProfile, EventHash: source.EventHash, EventBytes: 100, Token: DocFindSpan{From: 10, To: math.MinInt}},
		{Version: 1, Profile: DocDisplayStringProfile, EventHash: source.EventHash, EventBytes: 100, Token: DocFindSpan{From: math.MaxInt, To: 50}},
	} {
		if _, err := bad.Encode(); !errors.Is(err, ErrIntegrity) {
			test.Fatal("invalid source accepted", bad, err)
		}
	}
}

func TestDocDisplayPageRejectsUnvalidatedCoordinates(test *testing.T) {
	for _, runs := range [][]DocDisplayRun{
		nil, {{To: 3, SourceFrom: -1, SourceTo: 2, Linear: true}}, {{To: 4, SourceFrom: 0, SourceTo: 4, Linear: true}},
		{{To: 1, SourceFrom: 0, SourceTo: 1, Linear: true}, {To: 3, SourceFrom: 99, SourceTo: 101, Linear: true}},
		{{To: 3, SourceFrom: 0, SourceTo: 6, Linear: false}},
	} {
		page := DocDisplayPage{Text: "abc", Runs: runs}
		if span, err := page.SourceRange(0, 3); !errors.Is(err, ErrIntegrity) || span != (DocFindSpan{}) {
			test.Fatal("unvalidated coordinates accepted", span, err)
		}
	}
}

func TestDocDisplayStringRejectsSkippedInitialProgress(test *testing.T) {
	source, pages, _ := displayWindowFixture(test, "ABCDE")
	state, err := NewDocDisplayStringState(source)
	if err != nil {
		test.Fatal(err)
	}
	state.Next = source.Token.From + 3
	if err := state.Validate(source); !errors.Is(err, ErrIntegrity) {
		test.Fatal("zero-page checkpoint skipped data", err)
	}
	if _, err := state.Encode(source); !errors.Is(err, ErrIntegrity) {
		test.Fatal("skipped checkpoint serialized", err)
	}
	page := pages[0]
	page.Text = "CDE"
	page.Runs = []DocDisplayRun{{To: 3, SourceFrom: source.Token.From + 3, SourceTo: source.Token.To - 1, Linear: true}}
	if err := page.Validate(source); !errors.Is(err, ErrIntegrity) {
		test.Fatal("first page skipped prefix", err)
	}
}

func TestDocDisplayEncodedBudgetCoversCanonicalSerializer(test *testing.T) {
	characters := []rune{'<', '>', '&', '\u0085', '\u00a0', '\u2028', '\u2029', '\ufeff', '\ufffd', '\ud55c', '🙂', '\U0010ffff'}
	for character := rune(0); character < 128; character++ {
		characters = append(characters, character)
	}
	for _, character := range characters {
		encoded, err := canonicalJSON(string(character))
		if err != nil || len(encoded)-2 > docDisplayEncodedRuneBytes(character) {
			test.Fatal("serialized rune exceeds preparation budget", character, len(encoded), err)
		}
	}
	run := DocDisplayRun{To: DocDisplayPageTextBytes, SourceFrom: MaxConversationDocumentBytes - 2, SourceTo: MaxConversationDocumentBytes - 1, Linear: false}
	encoded, err := canonicalJSON(run)
	if err != nil || len(encoded)+1 > docDisplayEncodedRunBytes {
		test.Fatal("run metadata exceeds preparation budget", len(encoded), err)
	}
	page := DocDisplayPage{Version: 1, Source: HashContent([]byte("source")), Index: MaxDocRangePages - 1, From: MaxConversationDocumentBytes, Text: "", Runs: nil}
	encoded, err = canonicalJSON(page)
	if err != nil || len(encoded) > docDisplayEncodedHeaderBytes {
		test.Fatal("page envelope exceeds preparation budget", len(encoded), err)
	}
}
