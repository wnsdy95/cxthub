package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func assertDocMessageJSONParity(test testing.TB, value any, encode func() ([]byte, error)) {
	test.Helper()
	before, beforeErr := json.Marshal(value)
	want, wantErr := canonicalJSON(value)
	got, gotErr := encode()
	if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) || reflect.TypeOf(gotErr) != reflect.TypeOf(wantErr) || !bytes.Equal(got, want) || (got == nil) != (want == nil) {
		test.Fatalf("message wire mismatch: got %q (%v), want %q (%v)", got, gotErr, want, wantErr)
	}
	after, afterErr := json.Marshal(value)
	if !bytes.Equal(before, after) || fmt.Sprint(beforeErr) != fmt.Sprint(afterErr) {
		test.Fatal("message wire conversion mutated its input")
	}
}

func messageJSONTestInputs(number int, shape uint16, source, kind, hash string) (DocMessageInputPage, DocMessageDisplayPage, DocMessageDisplayState) {
	var inputs []DocMessageBlockInput
	var parts []DocMessageDisplayPart
	if shape != 0 {
		inputs = make([]DocMessageBlockInput, int(shape)%(DocMessageInputPageEntries+2))
		parts = make([]DocMessageDisplayPart, int(shape)%(DocMessageDisplayPageParts+2))
	}
	for index := range inputs {
		entry := DocMessageBlockInput{Index: number ^ index, Anchor: DocFindSpan{From: number, To: ^number}, IsText: index%2 == 0}
		if shape&1 != 0 {
			entry.Type = &DocFindSpan{From: 0, To: 0}
		}
		if shape&2 != 0 {
			entry.Name = &DocFindSpan{From: ^number, To: number}
		}
		if shape&4 != 0 {
			entry.Text = entry.Type
			if entry.Text == nil {
				entry.Text = &DocFindSpan{From: number, To: number}
			}
		}
		inputs[index] = entry
	}
	for index := range parts {
		parts[index] = DocMessageDisplayPart{Index: number ^ index, Block: ^number, From: number, Separator: index % 3, TextBytes: ^number, Kind: DocMessageDisplayKind(kind), Plan: ContentHash(hash), First: DocFindSpan{From: number, To: ^number}}
	}
	directory := DocRangeDirectoryAccumulator{Count: number}
	if shape != 0 {
		directory.Levels = [][]ContentHash{nil, {}, {ContentHash(hash)}}
	}
	return DocMessageInputPage{Version: number, Source: ContentHash(source), Index: ^number, Entries: inputs},
		DocMessageDisplayPage{Version: number, Source: ContentHash(source), Index: ^number, Parts: parts},
		DocMessageDisplayState{Version: number, Source: ContentHash(source), Next: ^number, Count: number, TextBytes: ^number, Breaks: number, Entries: parts, Directory: directory}
}

func checkDocMessageJSONInputs(test testing.TB, number int, shape uint16, source, kind, hash string) {
	test.Helper()
	input, display, state := messageJSONTestInputs(number, shape, source, kind, hash)
	assertDocMessageJSONParity(test, input, func() ([]byte, error) { return marshalDocMessageInputPage(input) })
	assertDocMessageJSONParity(test, display, func() ([]byte, error) { return marshalDocMessageDisplayPage(display) })
	assertDocMessageJSONParity(test, state, func() ([]byte, error) { return marshalDocMessageDisplayState(state) })
}

func TestMarshalDocMessageJSONCanonicalParity(test *testing.T) {
	maximum := int(^uint(0) >> 1)
	for _, number := range []int{0, 1, -1, maximum, -maximum - 1} {
		for _, shape := range []uint16{0, 1, 2, 3, 4, 7, 8, DocMessageInputPageEntries, DocMessageInputPageEntries + 1, DocMessageInputPageEntries + 2} {
			test.Run(fmt.Sprintf("%d/%d", number, shape), func(test *testing.T) {
				checkDocMessageJSONInputs(test, number, shape, "source", "markdown", "hash")
			})
		}
	}
	for caseIndex, text := range []string{"", "\ud55c\uae00🙂 e\u0301", "<>&\u2028\u2029\x00\x01\"\\/", "\ufffd", "\xff", "\xed\xa0\x80", "\ufffd\xff<&>"} {
		for _, field := range []string{"source", "kind", "hash"} {
			test.Run(fmt.Sprintf("text-%d/%s", caseIndex, field), func(test *testing.T) {
				source, kind, hash := "source", "markdown", "hash"
				switch field {
				case "source":
					source = text
				case "kind":
					kind = text
				case "hash":
					hash = text
				}
				checkDocMessageJSONInputs(test, 1, 7, source, kind, hash)
			})
		}
	}
}

func FuzzMarshalDocMessageJSONCanonicalParity(fuzz *testing.F) {
	fuzz.Add("source", "markdown", "hash", int64(1), uint16(7))
	fuzz.Add("\xff", "\ufffd", "\xed\xa0\x80", int64(1<<53+1), uint16(65))
	fuzz.Add("\ud55c\uae00🙂", "<>&\u2028\u2029\x00\"\\/", "\ufffd\xff", int64(-1<<63), uint16(0))
	fuzz.Add("valid", "valid", "valid", int64(1<<63-1), uint16(66))
	fuzz.Fuzz(func(test *testing.T, source, kind, hash string, number int64, shape uint16) {
		checkDocMessageJSONInputs(test, int(number), shape, source, kind, hash)
	})
}

func BenchmarkDocMessageJSON(benchmark *testing.B) {
	input, display, state := messageJSONBenchmarkInputs(benchmark)
	for _, candidate := range []struct {
		name   string
		value  any
		encode func() ([]byte, error)
	}{
		{"input", input, func() ([]byte, error) { return marshalDocMessageInputPage(input) }},
		{"display", display, func() ([]byte, error) { return marshalDocMessageDisplayPage(display) }},
		{"state", state, func() ([]byte, error) { return marshalDocMessageDisplayState(state) }},
	} {
		benchmark.Run(candidate.name, func(benchmark *testing.B) {
			benchmark.Run("oracle", func(benchmark *testing.B) {
				benchmark.ReportAllocs()
				for benchmark.Loop() {
					if raw, err := canonicalJSON(candidate.value); err != nil || len(raw) == 0 {
						benchmark.Fatal(err)
					}
				}
			})
			benchmark.Run("typed", func(benchmark *testing.B) {
				benchmark.ReportAllocs()
				for benchmark.Loop() {
					if raw, err := candidate.encode(); err != nil || len(raw) == 0 {
						benchmark.Fatal(err)
					}
				}
			})
		})
	}
}

func messageJSONBenchmarkInputs(test testing.TB) (DocMessageInputPage, DocMessageDisplayPage, DocMessageDisplayState) {
	test.Helper()
	hash := ContentHash("sha256:" + strings.Repeat("a", 64))
	source := DocMessageInputSource{Version: 1, Profile: DocMessageInputProfile, EventHash: hash, EventBytes: 10000, Blocks: DocFindSpan{From: 0, To: 9999}}
	inputHash, err := source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	input := DocMessageInputPage{Version: 1, Source: inputHash, Entries: make([]DocMessageBlockInput, DocMessageInputPageEntries)}
	for index := range input.Entries {
		from := 2 + index*100
		input.Entries[index] = DocMessageBlockInput{Index: index, Anchor: DocFindSpan{From: from, To: from + 90}, Type: &DocFindSpan{From: from + 1, To: from + 7}, Text: &DocFindSpan{From: from + 8, To: from + 12}, IsText: true}
	}
	displaySource := DocMessageDisplaySource{Version: 1, Profile: DocMessageDisplayProfile, Input: DocMessageInputGeneration{Version: 1, Source: source, Count: len(input.Entries), Pages: DocRangeDirectoryRoot{Hash: hash, Count: 1}}}
	displayHash, err := displaySource.Hash()
	if err != nil {
		test.Fatal(err)
	}
	display := DocMessageDisplayPage{Version: 1, Source: displayHash, Parts: make([]DocMessageDisplayPart, DocMessageDisplayPageParts)}
	for index := range display.Parts {
		separator := 2
		if index == 0 {
			separator = 0
		}
		display.Parts[index] = DocMessageDisplayPart{Index: index, Block: index, From: max(0, 3*index-2), Separator: separator, TextBytes: 1, Kind: DocMessageDisplayMarkdown, Plan: hash, First: DocFindSpan{From: 11 + index*100, To: 12 + index*100}}
	}
	entries := display.Parts[:DocMessageDisplayPageParts-1]
	state := DocMessageDisplayState{Version: 1, Source: displayHash, Next: len(entries), Count: len(entries), TextBytes: entries[len(entries)-1].end(), Breaks: 2, Entries: entries}
	if input.Validate(source) != nil || display.Validate(displaySource) != nil || state.Validate(displaySource) != nil {
		test.Fatal("benchmark fixture no longer satisfies the production validators")
	}
	return input, display, state
}
