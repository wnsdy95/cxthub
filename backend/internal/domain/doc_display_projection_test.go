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

func projectionTestSource(textBytes int) DocDisplayProjectionSource {
	return DocDisplayProjectionSource{Version: 1, Profile: DocDisplayProjectionProfile, EventHash: HashContent([]byte("event")), EventBytes: 4096, Plan: HashContent([]byte("public formatter plan")), TextBytes: textBytes}
}

func projectionTestPage(test *testing.T, source DocDisplayProjectionSource, index, from int, text string) DocDisplayPage {
	test.Helper()
	hash, err := source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	return DocDisplayPage{Version: 2, Source: hash, Index: index, From: from, Text: text, Runs: []DocDisplayRun{{To: len(text), SourceFrom: 10, SourceTo: 11}}}
}

func TestDocDisplayProjectionMappedCoordinates(test *testing.T) {
	source := projectionTestSource(len("β : 🙂A"))
	page := projectionTestPage(test, source, 0, 0, "β : 🙂A")
	page.Runs = []DocDisplayRun{
		{To: 2, SourceFrom: 100, SourceTo: 102, Linear: true},
		{To: 5, SourceFrom: 70, SourceTo: 71},
		{To: 9, SourceFrom: 20, SourceTo: 32},
		{To: 10, SourceFrom: 500, SourceTo: 501, Linear: true},
	}
	if err := page.ValidateProjection(source); err != nil {
		test.Fatal(err)
	}
	for _, scenario := range []struct {
		from, length int
		want         DocFindSpan
	}{
		{0, 2, DocFindSpan{From: 100, To: 102}},
		{2, 3, DocFindSpan{From: 70, To: 71}},
		{5, 4, DocFindSpan{From: 20, To: 32}},
		{0, 9, DocFindSpan{From: 20, To: 102}},
		{0, 10, DocFindSpan{From: 20, To: 501}},
		{9, 1, DocFindSpan{From: 500, To: 501}},
	} {
		if got, err := page.SourceRange(scenario.from, scenario.length); err != nil || got != scenario.want {
			test.Fatal(scenario, got, err)
		}
	}
	for _, coordinate := range [][2]int{{-1, 1}, {0, 0}, {1, 1}, {5, 3}, {10, 1}, {math.MaxInt, 1}, {1, math.MaxInt}} {
		if got, err := page.SourceRange(coordinate[0], coordinate[1]); !errors.Is(err, ErrValidation) || got != (DocFindSpan{}) {
			test.Fatal("invalid UTF-8 coordinates accepted", coordinate, got, err)
		}
	}
	encoded, err := page.EncodeProjection(source)
	if err != nil {
		test.Fatal(err)
	}
	decoded, err := DecodeDocDisplayProjectionPage(source, HashContent(encoded), encoded)
	if err != nil || !reflect.DeepEqual(page, decoded) {
		test.Fatal(decoded, err)
	}
	source.Plan = HashContent([]byte("different public plan"))
	if _, err := DecodeDocDisplayProjectionPage(source, HashContent(encoded), encoded); !errors.Is(err, ErrIntegrity) {
		test.Fatal("old output accepted under a different plan", err)
	}
}

func TestDocDisplayProjectionMetadataAndPageBounds(test *testing.T) {
	valid := projectionTestSource(5)
	for _, mutate := range []func(*DocDisplayProjectionSource){
		func(source *DocDisplayProjectionSource) { source.Version = 2 },
		func(source *DocDisplayProjectionSource) { source.Profile = DocDisplayStringProfile },
		func(source *DocDisplayProjectionSource) { source.Plan = "" },
		func(source *DocDisplayProjectionSource) { source.EventHash = "bad" },
		func(source *DocDisplayProjectionSource) { source.EventBytes = math.MinInt },
		func(source *DocDisplayProjectionSource) { source.EventBytes = math.MaxInt },
		func(source *DocDisplayProjectionSource) { source.TextBytes = -1 },
		func(source *DocDisplayProjectionSource) { source.TextBytes = MaxConversationDocumentBytes + 1 },
	} {
		source := valid
		mutate(&source)
		if _, err := source.Encode(); !errors.Is(err, ErrIntegrity) {
			test.Fatal("invalid source accepted", source, err)
		}
	}
	page := projectionTestPage(test, valid, 0, 0, "hello")
	for _, mutate := range []func(*DocDisplayPage){
		func(page *DocDisplayPage) { page.Version = 1 },
		func(page *DocDisplayPage) { page.Index = math.MaxInt },
		func(page *DocDisplayPage) { page.From = math.MaxInt },
		func(page *DocDisplayPage) { page.Index = 1 },
		func(page *DocDisplayPage) { page.Text = "helloo" },
		func(page *DocDisplayPage) { page.Text = "\xffello" },
		func(page *DocDisplayPage) { page.Runs = nil },
		func(page *DocDisplayPage) { page.Runs[0].SourceFrom = -1 },
		func(page *DocDisplayPage) { page.Runs[0].SourceTo = valid.EventBytes + 1 },
		func(page *DocDisplayPage) { page.Runs[0].SourceTo = page.Runs[0].SourceFrom },
		func(page *DocDisplayPage) { page.Runs[0].Linear = true },
		func(page *DocDisplayPage) { page.Runs[0].To = 4 },
	} {
		candidate := page
		candidate.Runs = append([]DocDisplayRun(nil), page.Runs...)
		mutate(&candidate)
		if _, err := candidate.EncodeProjection(valid); !errors.Is(err, ErrIntegrity) {
			test.Fatal("invalid page accepted", candidate, err)
		}
	}
	for _, text := range []string{strings.Repeat("x", DocDisplayPageTextBytes+1), strings.Repeat("<", DocDisplayPageTextBytes)} {
		source := projectionTestSource(len(text))
		candidate := projectionTestPage(test, source, 0, 0, text)
		if _, err := candidate.EncodeProjection(source); !errors.Is(err, ErrIntegrity) {
			test.Fatal("text/serialized budget exceeded", len(text), err)
		}
	}
	text := strings.Repeat("🙂", DocDisplayPageRuns-1)
	source := projectionTestSource(len(text) + 1)
	if err := projectionTestPage(test, source, 0, 0, text).ValidateProjection(source); !errors.Is(err, ErrIntegrity) {
		test.Fatal("nonterminal page has fewer than 512 runes", err)
	}
	source.TextBytes--
	if err := projectionTestPage(test, source, 0, 0, text).ValidateProjection(source); err != nil {
		test.Fatal("small terminal page rejected", err)
	}
}

func TestDocDisplayProjectionRejectsPartialPublishAndCanceledEffects(test *testing.T) {
	source := projectionTestSource(2 * DocDisplayPageRuns)
	state, err := NewDocDisplayProjectionState(source)
	if err != nil {
		test.Fatal(err)
	}
	if _, objects, err := FinishDocDisplayProjection(context.Background(), source, state); !errors.Is(err, ErrIntegrity) || objects != nil {
		test.Fatal("partial state published", err)
	}
	page := projectionTestPage(test, source, 0, 0, strings.Repeat("x", DocDisplayPageRuns))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, objects, err := AppendDocDisplayProjectionPage(ctx, source, state, page); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, DocDisplayProjectionState{}) || objects != nil {
		test.Fatal("canceled step exposed effects", got, objects, err)
	}
	if got, objects, err := FinishDocDisplayProjection(ctx, source, state); !errors.Is(err, context.Canceled) || got != (DocDisplayProjectionGeneration{}) || objects != nil {
		test.Fatal("canceled finish exposed effects", got, objects, err)
	}
	page.From = DocDisplayPageRuns
	if _, _, err := AppendDocDisplayProjectionPage(context.Background(), source, state, page); !errors.Is(err, ErrIntegrity) {
		test.Fatal("initial gap accepted", err)
	}
	state.TextBytes = DocDisplayPageRuns
	if _, err := state.Encode(source); !errors.Is(err, ErrIntegrity) {
		test.Fatal("zero-page checkpoint skipped output", err)
	}
}

func TestDocDisplayProjectionLiteralFormatRemainsStrict(test *testing.T) {
	source, pages, _ := displayWindowFixture(test, "A🙂B")
	page := pages[0]
	before, err := page.Encode(source)
	if err != nil {
		test.Fatal(err)
	}
	if page.Version != 1 || bytes.Contains(before, []byte("plan")) {
		test.Fatal("literal encoding changed", string(before))
	}
	want := fmt.Sprintf(`{"from":0,"index":0,"runs":[{"linear":true,"source_from":%d,"source_to":%d,"to":6}],"source":%q,"text":"A🙂B","version":1}`, source.Token.From+1, source.Token.To-1, page.Source)
	if string(before) != want {
		test.Fatal("literal canonical page bytes changed", string(before), want)
	}
	if decoded, err := DecodeDocDisplayPage(source, HashContent(before), before); err != nil || !reflect.DeepEqual(decoded, page) {
		test.Fatal(decoded, err)
	}
	page.Runs = []DocDisplayRun{{To: 1, SourceFrom: source.Token.From + 1, SourceTo: source.Token.From + 2, Linear: true}, {To: 6, SourceFrom: source.Token.From + 3, SourceTo: source.Token.To - 1}}
	if _, err := page.SourceRange(0, 6); !errors.Is(err, ErrIntegrity) {
		test.Fatal("literal gaps accepted", err)
	}
	page = pages[0]
	page.Version = 2
	if _, err := page.Encode(source); !errors.Is(err, ErrIntegrity) {
		test.Fatal("projection version accepted by literal encoder", err)
	}
	encoded, _ := json.Marshal(page)
	if _, err := DecodeDocDisplayPage(source, HashContent(encoded), encoded); !errors.Is(err, ErrIntegrity) {
		test.Fatal("projection page downgraded to literal", err)
	}
}

func TestDocDisplayProjectionMetadataDecodersRejectAlternateBytes(test *testing.T) {
	source := projectionTestSource(0)
	state, err := NewDocDisplayProjectionState(source)
	if err != nil {
		test.Fatal(err)
	}
	generation, _, err := FinishDocDisplayProjection(context.Background(), source, state)
	if err != nil {
		test.Fatal(err)
	}
	for _, scenario := range []struct {
		name   string
		encode func() ([]byte, error)
		decode func(ContentHash, []byte) error
	}{
		{"source", source.Encode, func(hash ContentHash, raw []byte) error {
			_, err := DecodeDocDisplayProjectionSource(hash, raw)
			return err
		}},
		{"state", func() ([]byte, error) { return state.Encode(source) }, func(hash ContentHash, raw []byte) error {
			_, err := DecodeDocDisplayProjectionState(source, hash, raw)
			return err
		}},
		{"generation", generation.Encode, func(hash ContentHash, raw []byte) error {
			_, err := DecodeDocDisplayProjectionGeneration(hash, raw)
			return err
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			raw, err := scenario.encode()
			if err != nil || scenario.decode(HashContent(raw), raw) != nil {
				test.Fatal("round trip failed", err)
			}
			var object map[string]any
			if err := json.Unmarshal(raw, &object); err != nil {
				test.Fatal(err)
			}
			object["unknown"] = true
			unknown, err := canonicalJSON(object)
			if err != nil {
				test.Fatal(err)
			}
			for _, altered := range [][]byte{nil, append(bytes.Clone(raw), ' '), unknown, []byte(`{"version":1,"version":1}`), bytes.Repeat([]byte("x"), MaxDocDisplayStateBytes+1)} {
				if err := scenario.decode(HashContent(altered), altered); !errors.Is(err, ErrIntegrity) {
					test.Fatal("noncanonical metadata accepted", len(altered), err)
				}
			}
			if err := scenario.decode(HashContent([]byte("wrong object")), raw); !errors.Is(err, ErrIntegrity) {
				test.Fatal("wrong hash accepted", err)
			}
		})
	}
}

func FuzzDocDisplayProjectionPageDecode(fuzz *testing.F) {
	source := projectionTestSource(6)
	hash, _ := source.Hash()
	page := DocDisplayPage{Version: 2, Source: hash, Text: "a🙂b", Runs: []DocDisplayRun{{To: 1, SourceFrom: 100, SourceTo: 101, Linear: true}, {To: 5, SourceFrom: 10, SourceTo: 22}, {To: 6, SourceFrom: 300, SourceTo: 301, Linear: true}}}
	raw, _ := page.EncodeProjection(source)
	fuzz.Add(raw)
	fuzz.Add([]byte(`{"version":2}`))
	fuzz.Fuzz(func(test *testing.T, raw []byte) {
		if len(raw) > MaxDocDisplayPageBytes+1 {
			test.Skip()
		}
		decoded, err := DecodeDocDisplayProjectionPage(source, HashContent(raw), raw)
		if err != nil {
			if !reflect.DeepEqual(decoded, DocDisplayPage{}) {
				test.Fatal("failed decode exposed partial page")
			}
			return
		}
		encoded, err := decoded.EncodeProjection(source)
		if err != nil || !bytes.Equal(encoded, raw) || !utf8.ValidString(decoded.Text) {
			test.Fatal("accepted noncanonical page", err)
		}
		for offset := range decoded.Text {
			_, width := utf8.DecodeRuneInString(decoded.Text[offset:])
			span, err := decoded.SourceRange(offset, width)
			if err != nil || span.From < 0 || span.To > source.EventBytes || span.From >= span.To {
				test.Fatal("accepted invalid mapping", span, err)
			}
		}
	})
}
