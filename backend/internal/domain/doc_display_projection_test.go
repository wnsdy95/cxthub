package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
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

func TestDocDisplayProjectionRestartDirectoryAndSelectedRead(test *testing.T) {
	for _, count := range []int{0, 1, 129, 16385} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			text := strings.Repeat("z", DocDisplayPageRuns)
			source := projectionTestSource(count * len(text))
			state, err := NewDocDisplayProjectionState(source)
			if err != nil {
				test.Fatal(err)
			}
			stored := make(map[ContentHash][]byte)
			var hashes []ContentHash
			maxCheckpoint := 0
			for index := 0; index < count; index++ {
				page := projectionTestPage(test, source, index, state.TextBytes, text)
				before, err := state.Encode(source)
				if err != nil {
					test.Fatal(err)
				}
				next, objects, err := AppendDocDisplayProjectionPage(context.Background(), source, state, page)
				if err != nil {
					test.Fatal(err)
				}
				docDisplayStringTestStore(test, stored, objects)
				encoded, err := page.EncodeProjection(source)
				if err != nil {
					test.Fatal(err)
				}
				hashes = append(hashes, HashContent(encoded))
				after, err := state.Encode(source)
				if err != nil || !bytes.Equal(before, after) {
					test.Fatal("input frontier mutated", err)
				}
				checkpoint, err := next.Encode(source)
				if err != nil {
					test.Fatal(err)
				}
				maxCheckpoint = max(maxCheckpoint, len(checkpoint))
				state, err = DecodeDocDisplayProjectionState(source, HashContent(checkpoint), checkpoint)
				if err != nil {
					test.Fatal(err)
				}
			}
			generation, objects, err := FinishDocDisplayProjection(context.Background(), source, state)
			if err != nil || generation.Pages.Count != count {
				test.Fatal(generation, err)
			}
			docDisplayStringTestStore(test, stored, objects)
			var batchObjects []DocRangeObject
			batch, err := buildDocRangeDirectory(context.Background(), hashes, &batchObjects)
			if err != nil || generation.Pages != batch {
				test.Fatal("batch and resumed directory differ", generation.Pages, batch, err)
			}
			encoded, err := generation.Encode()
			if err != nil {
				test.Fatal(err)
			}
			if decoded, err := DecodeDocDisplayProjectionGeneration(HashContent(encoded), encoded); err != nil || decoded != generation {
				test.Fatal(decoded, err)
			}
			loadCount := 0
			load := func(_ context.Context, hash ContentHash) ([]byte, error) {
				loadCount++
				return stored[hash], nil
			}
			if count > 0 {
				page, err := ReadDocDisplayProjectionPage(context.Background(), generation, count-1, load)
				if err != nil || page.Index != count-1 || page.Text != text || page.From+len(page.Text) != source.TextBytes || loadCount > 4 {
					test.Fatal(page.Index, loadCount, err)
				}
			} else if _, err := ReadDocDisplayProjectionPage(context.Background(), generation, 0, load); !errors.Is(err, ErrIntegrity) || loadCount != 0 {
				test.Fatal("empty generation read objects", loadCount, err)
			}
			test.Logf("pages=%d displayed_bytes=%d max_checkpoint_bytes=%d selected_object_loads=%d", count, source.TextBytes, maxCheckpoint, loadCount)
		})
	}
}

func TestDocDisplayProjectionCrossPageSearchRetainsReorderedCovers(test *testing.T) {
	for _, query := range []string{"Needle🙂", strings.Repeat("a", MaxDocFindQueryRunes-1) + "b"} {
		prefix := strings.Repeat("x", DocDisplayPageRuns-3)
		if len(query) == MaxDocFindQueryRunes {
			prefix = strings.Repeat("x", DocDisplayPageRuns-MaxDocFindQueryRunes+1)
		}
		text := prefix + query + strings.Repeat("z", 20)
		split := DocDisplayPageRuns
		source := projectionTestSource(len(text))
		previous := projectionTestPage(test, source, 0, 0, text[:split])
		previous.Runs[0].SourceFrom, previous.Runs[0].SourceTo = 800, 801
		current := projectionTestPage(test, source, 1, split, text[split:])
		current.Runs[0].SourceFrom, current.Runs[0].SourceTo = 20, 21
		window, err := BuildDocDisplayProjectionSearchWindow(source, &previous, current)
		if err != nil {
			test.Fatal(err)
		}
		previous.Runs[0].SourceFrom = 0
		current.Runs[0].SourceTo = 4096
		matches, next, err := FindDocDisplayWindow(context.Background(), window, strings.ToLower(query), 0, 20)
		if err != nil || next != len(text) || len(matches) != 1 || matches[0].Offset != len(prefix) || matches[0].SourceRange == nil || *matches[0].SourceRange != (DocFindSpan{From: 20, To: 801}) {
			test.Fatal(matches, next, err)
		}
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

func TestDocDisplayProjectionExistingToolGoldensUseSharedCoordinates(test *testing.T) {
	raw, err := os.ReadFile("../../../schemas/fixtures/tool-display-v1.json")
	if err != nil {
		test.Fatal(err)
	}
	var fixture struct {
		Cases []struct{ Name, Source, Text string } `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil || len(fixture.Cases) < 25 {
		test.Fatal("invalid tool goldens", err)
	}
	for _, scenario := range fixture.Cases {
		test.Run(scenario.Name, func(test *testing.T) {
			display, err := BuildDocToolDisplay(context.Background(), []byte(scenario.Source))
			if err != nil || display.Text() != scenario.Text {
				test.Fatal("fixture changed", err)
			}
			source := projectionTestSource(len(scenario.Text))
			source.EventHash, source.EventBytes = HashContent([]byte(scenario.Source)), len(scenario.Source)
			if scenario.Text == "" {
				state, err := NewDocDisplayProjectionState(source)
				if err != nil || !state.Done || state.Directory.Count != 0 {
					test.Fatal("empty fixture produced a page", state, err)
				}
				return
			}
			mapped := display.(*docToolDisplay)
			page := projectionTestPage(test, source, 0, 0, display.Text())
			page.Runs = nil
			for index := 0; index < mapped.spanCount; index++ {
				span := mapped.span(index)
				page.Runs = append(page.Runs, DocDisplayRun{To: int(span.end), SourceFrom: int(span.sourceFrom), SourceTo: int(span.sourceTo &^ docToolDisplayLinear), Linear: span.sourceTo&docToolDisplayLinear != 0})
			}
			if err := page.ValidateProjection(source); err != nil {
				test.Fatal("fixture could not use mapped page", err)
			}
			window, err := BuildDocDisplayProjectionSearchWindow(source, nil, page)
			if err != nil || window.Text() != scenario.Text {
				test.Fatal(window.Text(), err)
			}
			for offset, character := range scenario.Text {
				width := utf8.RuneLen(character)
				want, err := display.SourceRange(offset, width)
				got, failure := page.SourceRange(offset, width)
				if err != nil || failure != nil || got != want {
					test.Fatal("mapped page changed a fixture source cover", offset, got, want, err, failure)
				}
			}
		})
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
