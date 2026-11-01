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
)

type decodedTextReadFixture struct {
	source     DocDecodedTextSource
	generation DocDecodedTextGeneration
	objects    map[ContentHash][]byte
	raw        []byte
	pages      []DocDisplayPage
	text       string
}

func newDecodedTextReadFixture(test *testing.T, token string) decodedTextReadFixture {
	test.Helper()
	var text string
	if err := json.Unmarshal([]byte(token), &text); err != nil {
		test.Fatal(err)
	}
	prefix := `{"kind":"tool_call","input":`
	raw := []byte(prefix + token + `}`)
	source := DocDecodedTextSource{Version: 1, Profile: "public-json-string-input-v1", EventHash: HashContent(raw), EventBytes: len(raw), Token: DocFindSpan{From: len(prefix), To: len(raw) - 1}}
	hash, err := source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	fixture := decodedTextReadFixture{source: source, raw: raw, text: text}
	request := docJSONStringPageRequest{Version: 3, Source: hash, Token: source.Token, Next: source.Token.From}
	for {
		step, err := decodeDocJSONStringPage(context.Background(), request, func(_ context.Context, from, length int) ([]byte, error) {
			return raw[from : from+length], nil
		})
		if err != nil {
			test.Fatal(err)
		}
		if step.Page != nil {
			fixture.pages = append(fixture.pages, *step.Page)
			request.Index++
			request.From += len(step.Page.Text)
		}
		if step.Done {
			break
		}
		request.Next = step.Next
	}
	fixture.rebuild(test)
	if fixture.text != text {
		test.Fatal("fixture decoder differs from JSON", fixture.text, text)
	}
	return fixture
}

func (fixture *decodedTextReadFixture) rebuild(test *testing.T) {
	test.Helper()
	fixture.objects = make(map[ContentHash][]byte)
	var objects []DocRangeObject
	var hashes []ContentHash
	var text strings.Builder
	for _, page := range fixture.pages {
		raw, err := canonicalJSON(page)
		if err != nil {
			test.Fatal(err)
		}
		hash := HashContent(raw)
		fixture.objects[hash] = raw
		hashes = append(hashes, hash)
		text.WriteString(page.Text)
	}
	pages, err := buildDocRangeDirectory(context.Background(), hashes, &objects)
	if err != nil {
		test.Fatal(err)
	}
	fixture.text = text.String()
	sourceHash, err := fixture.source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	var lookupHashes []ContentHash
	entries := (len(fixture.text) + DocDecodedTextLookupStride - 1) / DocDecodedTextLookupStride
	ordinal := 0
	for start := 0; start < entries; start += DocDecodedTextLookupEntries {
		lookup := DocDecodedTextLookupPage{Version: 1, Source: sourceHash, Index: start / DocDecodedTextLookupEntries}
		for bucket := start; bucket < min(start+DocDecodedTextLookupEntries, entries); bucket++ {
			for ordinal+1 < len(fixture.pages) && fixture.pages[ordinal].From+len(fixture.pages[ordinal].Text) <= bucket*DocDecodedTextLookupStride {
				ordinal++
			}
			lookup.Pages = append(lookup.Pages, ordinal)
		}
		raw, err := lookup.Encode(fixture.source)
		if err != nil {
			test.Fatal(err)
		}
		hash := HashContent(raw)
		fixture.objects[hash] = raw
		lookupHashes = append(lookupHashes, hash)
	}
	lookup, err := buildDocRangeDirectory(context.Background(), lookupHashes, &objects)
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range objects {
		fixture.objects[object.Hash] = object.Bytes
	}
	fixture.generation = DocDecodedTextGeneration{Version: 1, Source: fixture.source, Hash: HashContent([]byte(fixture.text)), TextBytes: len(fixture.text), Pages: pages, Lookup: lookup}
	if err := fixture.generation.Validate(); err != nil {
		test.Fatal("invalid fixture generation", err)
	}
}

func (fixture decodedTextReadFixture) load(_ context.Context, hash ContentHash) ([]byte, error) {
	raw, exists := fixture.objects[hash]
	if !exists {
		return nil, errors.New("unexpected object read")
	}
	return raw, nil
}

func rewriteDecodedTextReadLookup(test *testing.T, fixture *decodedTextReadFixture, index int, mutate func(*DocDecodedTextLookupPage)) {
	test.Helper()
	hashes := make([]ContentHash, fixture.generation.Lookup.Count)
	for position := range hashes {
		hash, err := ResolveDocRangeLeaf(context.Background(), fixture.generation.Lookup, position, fixture.load)
		if err != nil {
			test.Fatal(err)
		}
		hashes[position] = hash
	}
	page, err := DecodeDocDecodedTextLookupPage(fixture.source, hashes[index], fixture.objects[hashes[index]])
	if err != nil {
		test.Fatal(err)
	}
	mutate(&page)
	raw, err := canonicalJSON(page)
	if err != nil {
		test.Fatal(err)
	}
	hashes[index] = HashContent(raw)
	fixture.objects[hashes[index]] = raw
	var objects []DocRangeObject
	fixture.generation.Lookup, err = buildDocRangeDirectory(context.Background(), hashes, &objects)
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range objects {
		fixture.objects[object.Hash] = object.Bytes
	}
}

func TestDocDecodedTextReadLookupCodec(test *testing.T) {
	fixture := newDecodedTextReadFixture(test, `"hello"`)
	hash, err := fixture.source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	page := DocDecodedTextLookupPage{Version: 1, Source: hash, Pages: []int{0, 0, 1, MaxDocRangePages - 1}}
	raw, err := page.Encode(fixture.source)
	if err != nil {
		test.Fatal(err)
	}
	if decoded, err := DecodeDocDecodedTextLookupPage(fixture.source, HashContent(raw), raw); err != nil || !reflect.DeepEqual(decoded, page) {
		test.Fatal(decoded, err)
	}
	for name, mutate := range map[string]func(*DocDecodedTextLookupPage){
		"version":     func(page *DocDecodedTextLookupPage) { page.Version = 3 },
		"source":      func(page *DocDecodedTextLookupPage) { page.Source = HashContent([]byte("other")) },
		"negative":    func(page *DocDecodedTextLookupPage) { page.Index = -1 },
		"index-limit": func(page *DocDecodedTextLookupPage) { page.Index = MaxDocRangePages },
		"empty":       func(page *DocDecodedTextLookupPage) { page.Pages = nil },
		"too-many":    func(page *DocDecodedTextLookupPage) { page.Pages = make([]int, DocDecodedTextLookupEntries+1) },
		"ordinal":     func(page *DocDecodedTextLookupPage) { page.Pages = []int{-1} },
		"limit":       func(page *DocDecodedTextLookupPage) { page.Pages = []int{MaxDocRangePages} },
		"descending":  func(page *DocDecodedTextLookupPage) { page.Pages = []int{1, 0} },
	} {
		test.Run(name, func(test *testing.T) {
			candidate := page
			candidate.Pages = append([]int(nil), page.Pages...)
			mutate(&candidate)
			if _, err := candidate.Encode(fixture.source); !errors.Is(err, ErrIntegrity) {
				test.Fatal("invalid lookup encoded", err)
			}
			encoded, err := canonicalJSON(candidate)
			if err != nil {
				test.Fatal(err)
			}
			if decoded, err := DecodeDocDecodedTextLookupPage(fixture.source, HashContent(encoded), encoded); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocDecodedTextLookupPage{}) {
				test.Fatal("invalid lookup decoded", decoded, err)
			}
		})
	}
	for name, encoded := range map[string][]byte{
		"empty":     nil,
		"oversized": bytes.Repeat([]byte(" "), MaxDocDecodedTextLookupBytes+1),
		"space":     append(bytes.Clone(raw), ' '),
		"unknown":   append(bytes.Clone(raw[:len(raw)-1]), []byte(`,"count":4}`)...),
		"duplicate": append(bytes.Clone(raw[:len(raw)-1]), []byte(`,"version":1}`)...),
		"fraction":  []byte(strings.Replace(string(raw), `"version":1`, `"version":1.0`, 1)),
	} {
		test.Run(name, func(test *testing.T) {
			if decoded, err := DecodeDocDecodedTextLookupPage(fixture.source, HashContent(encoded), encoded); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocDecodedTextLookupPage{}) {
				test.Fatal("noncanonical lookup accepted", decoded, err)
			}
		})
	}
	for _, expected := range []ContentHash{"bad", HashContent([]byte("other"))} {
		if _, err := DecodeDocDecodedTextLookupPage(fixture.source, expected, raw); !errors.Is(err, ErrIntegrity) {
			test.Fatal("wrong lookup hash accepted", err)
		}
	}
	changed := fixture.source
	changed.Profile = DocDisplayStringProfile
	if page.Validate(changed) == nil {
		test.Fatal("wrong source profile accepted")
	}
}

func TestDocDecodedTextReadBytesExactAndUnaligned(test *testing.T) {
	for _, token := range []string{
		`""`, `"  \n\t "`, `"A\n\u03b2\ud83d\ude42한\"Z"`,
		`"` + strings.Repeat(`\n`, 511) + `\u03b2` + strings.Repeat(`\n`, 1536) + `"`,
		`"` + strings.Repeat("한🙂abc", 9000) + `"`,
	} {
		fixture := newDecodedTextReadFixture(test, token)
		for _, from := range []int{0, 1, 2, 511, 512, 513, 1023, 32767, 65535, len(fixture.text)} {
			if from > len(fixture.text) {
				continue
			}
			for _, length := range []int{0, 1, 2, 4, 511, 512, 4096} {
				if length > len(fixture.text)-from {
					continue
				}
				got, err := ReadDocDecodedTextBytes(context.Background(), fixture.generation, from, length, fixture.load)
				if err != nil || len(got) != length || !bytes.Equal(got, []byte(fixture.text[from:from+length])) {
					test.Fatal("inexact byte read", len(fixture.text), from, length, got, err)
				}
			}
		}
		for index, expected := range fixture.pages {
			page, err := ReadDocDecodedTextPage(context.Background(), fixture.generation, index, fixture.load)
			if err != nil || !reflect.DeepEqual(page, expected) {
				test.Fatal("page changed", index, page, err)
			}
		}
	}
}

func TestDocDecodedTextReadPreparedGeneration(test *testing.T) {
	for _, text := range []string{
		"", " \n\t\r ", "한🙂\\\"\n",
		strings.Repeat("한🙂x\n", 14000), strings.Repeat("\\\t\n\r\"", 15000),
	} {
		raw, err := json.Marshal(map[string]any{"kind": "reasoning", "redacted_summary": text, "opaque": "not public input"})
		if err != nil {
			test.Fatal(err)
		}
		fixture := buildDecodedTextFixture(test, raw)
		load := func(_ context.Context, hash ContentHash) ([]byte, error) {
			body, exists := fixture.objects[hash]
			if !exists {
				return nil, ErrNotFound
			}
			return body, nil
		}
		for from := 0; from < len(text); from += 4093 {
			length := min(4096, len(text)-from)
			got, err := ReadDocDecodedTextBytes(context.Background(), fixture.generation, from, length, load)
			if err != nil || !bytes.Equal(got, []byte(text[from:from+length])) {
				test.Fatal("prepared byte read differs", len(text), from, err)
			}
		}
		if len(text) > 0 {
			span, err := LocateDocDecodedTextSource(context.Background(), fixture.generation, 0, len(text), load)
			if err != nil || span != (DocFindSpan{From: fixture.source.Token.From + 1, To: fixture.source.Token.To - 1}) {
				test.Fatal("prepared source span differs", len(text), span, err)
			}
		}
	}
}

func TestDocDecodedTextReadBoundsAndEmpty(test *testing.T) {
	fixture := newDecodedTextReadFixture(test, `"🙂x"`)
	for _, coordinates := range [][2]int{{-1, 1}, {0, -1}, {0, 4097}, {6, 0}, {5, 1}, {math.MaxInt, 1}, {1, math.MaxInt}, {math.MinInt, 1}} {
		got, err := ReadDocDecodedTextBytes(context.Background(), fixture.generation, coordinates[0], coordinates[1], fixture.load)
		if !errors.Is(err, ErrValidation) || got != nil {
			test.Fatal("invalid byte range accepted", coordinates, got, err)
		}
	}
	for _, coordinates := range [][2]int{{0, 0}, {-1, 1}, {0, -1}, {1, 1}, {0, 3}, {5, 1}, {math.MaxInt, 1}, {1, math.MaxInt}} {
		got, err := LocateDocDecodedTextSource(context.Background(), fixture.generation, coordinates[0], coordinates[1], fixture.load)
		if !errors.Is(err, ErrValidation) || got != (DocFindSpan{}) {
			test.Fatal("invalid source range accepted", coordinates, got, err)
		}
	}
	for _, index := range []int{-1, fixture.generation.Pages.Count, math.MaxInt} {
		got, err := ReadDocDecodedTextPage(context.Background(), fixture.generation, index, fixture.load)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(got, DocDisplayPage{}) {
			test.Fatal("invalid ordinal accepted", got, err)
		}
	}
	empty := newDecodedTextReadFixture(test, `""`)
	loads := 0
	load := func(context.Context, ContentHash) ([]byte, error) {
		loads++
		return nil, errors.New("empty read loaded an object")
	}
	if got, err := ReadDocDecodedTextBytes(context.Background(), empty.generation, 0, 0, load); err != nil || len(got) != 0 || loads != 0 {
		test.Fatal(got, err, loads)
	}
	if got, err := LocateDocDecodedTextSource(context.Background(), empty.generation, 0, 1, load); !errors.Is(err, ErrValidation) || got != (DocFindSpan{}) || loads != 0 {
		test.Fatal(got, err, loads)
	}
	if got, err := ReadDocDecodedTextBytes(context.Background(), fixture.generation, 0, 1, nil); !errors.Is(err, ErrIntegrity) || got != nil {
		test.Fatal(got, err)
	}
	if got, err := LocateDocDecodedTextSource(context.Background(), fixture.generation, 0, 4, nil); !errors.Is(err, ErrIntegrity) || got != (DocFindSpan{}) {
		test.Fatal(got, err)
	}
}

func TestDocDecodedTextReadSelectedLookupAndBoundedPages(test *testing.T) {
	fixture := newDecodedTextReadFixture(test, `"`+strings.Repeat(`\n`, 140*DocDisplayPageRuns)+`"`)
	from, length := 129*DocDisplayPageRuns+1, 4096
	dataLoads, lookupLoads := 0, 0
	load := func(ctx context.Context, hash ContentHash) ([]byte, error) {
		raw, err := fixture.load(ctx, hash)
		if err != nil {
			return nil, err
		}
		var header struct {
			Version int `json:"version"`
			Index   int `json:"index"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			test.Fatal(err)
		}
		if header.Version == 3 {
			dataLoads++
			if header.Index < 129 || header.Index > 137 {
				return nil, errors.New("unselected data page")
			}
		} else if header.Version == 1 {
			lookupLoads++
			if header.Index != 1 {
				return nil, errors.New("unselected lookup page")
			}
		}
		return raw, nil
	}
	got, err := ReadDocDecodedTextBytes(context.Background(), fixture.generation, from, length, load)
	if err != nil || !bytes.Equal(got, []byte(fixture.text[from:from+length])) || dataLoads > 10 || dataLoads != 9 || lookupLoads != 1 {
		test.Fatal("unbounded or incorrect selected read", len(got), err, dataLoads, lookupLoads)
	}
}

func TestDocDecodedTextReadInitialNeighbor(test *testing.T) {
	fixture := newDecodedTextReadFixture(test, `"`+strings.Repeat(`\n`, 511)+`\u03b2`+strings.Repeat(`\n`, 2048)+`"`)
	if len(fixture.pages[0].Text) != 513 {
		test.Fatal("fixture did not straddle the locator bucket")
	}
	for _, coordinates := range [][2]int{{512, 1}, {513, 1}, {513, 2048}} {
		got, err := ReadDocDecodedTextBytes(context.Background(), fixture.generation, coordinates[0], coordinates[1], fixture.load)
		if err != nil || !bytes.Equal(got, []byte(fixture.text[coordinates[0]:coordinates[0]+coordinates[1]])) {
			test.Fatal("initial neighbor failed", coordinates, got, err)
		}
	}
	span, err := LocateDocDecodedTextSource(context.Background(), fixture.generation, 513, 1024, fixture.load)
	start := fixture.source.Token.From + 1 + 511*2 + 6
	if err != nil || span != (DocFindSpan{From: start, To: start + 2048}) {
		test.Fatal(span, err)
	}
	if span, err := LocateDocDecodedTextSource(context.Background(), fixture.generation, 512, 1, fixture.load); !errors.Is(err, ErrValidation) || span != (DocFindSpan{}) {
		test.Fatal("unaligned source endpoint accepted", span, err)
	}
}

func TestDocDecodedTextReadSourceLiteralMapping(test *testing.T) {
	fixture := newDecodedTextReadFixture(test, `"A\n\u03b2\ud83d\ude42한\"Z"`)
	base := fixture.source.Token.From + 1
	for _, scenario := range []struct {
		from, length int
		start, end   int
	}{
		{0, 1, 0, 1}, {1, 1, 1, 3}, {2, 2, 3, 9}, {4, 4, 9, 21},
		{8, 3, 21, 24}, {11, 1, 24, 26}, {12, 1, 26, 27},
		{0, 13, 0, 27}, {1, 10, 1, 24}, {2, 6, 3, 21},
	} {
		span, err := LocateDocDecodedTextSource(context.Background(), fixture.generation, scenario.from, scenario.length, fixture.load)
		if err != nil || span != (DocFindSpan{From: base + scenario.start, To: base + scenario.end}) {
			test.Fatal(scenario, span, err)
		}
	}
}

func TestDocDecodedTextReadSourceSkipsIntermediatePages(test *testing.T) {
	fixture := newDecodedTextReadFixture(test, `"`+strings.Repeat(`\n`, 140*DocDisplayPageRuns)+`"`)
	loads := 0
	load := func(ctx context.Context, hash ContentHash) ([]byte, error) {
		raw, err := fixture.load(ctx, hash)
		if err != nil {
			return nil, err
		}
		var page DocDisplayPage
		if err := json.Unmarshal(raw, &page); err != nil {
			test.Fatal(err)
		}
		if page.Version == 3 {
			loads++
			if page.Index != 0 && page.Index != len(fixture.pages)-1 {
				return nil, errors.New("intermediate page loaded")
			}
		}
		return raw, nil
	}
	span, err := LocateDocDecodedTextSource(context.Background(), fixture.generation, 1, len(fixture.text)-2, load)
	base := fixture.source.Token.From + 1
	if err != nil || span != (DocFindSpan{From: base + 2, To: base + 2*(len(fixture.text)-1)}) || loads != 2 {
		test.Fatal("source lookup scanned intermediate pages", span, err, loads)
	}
}

func TestDocDecodedTextReadSourceRejectsNonmonotoneCover(test *testing.T) {
	fixture := newDecodedTextReadFixture(test, `"`+strings.Repeat(`\u03b2`, 2048)+`"`)
	page := &fixture.pages[2]
	shift := page.Runs[0].SourceFrom - fixture.source.Token.From - 1 - page.From
	for index := range page.Runs {
		page.Runs[index].SourceFrom -= shift
		page.Runs[index].SourceTo -= shift
	}
	fixture.rebuild(test)
	span, err := LocateDocDecodedTextSource(context.Background(), fixture.generation, 2, page.From, fixture.load)
	if !errors.Is(err, ErrIntegrity) || span != (DocFindSpan{}) {
		test.Fatal("nonmonotone source cover accepted", span, err)
	}
}

func TestDocDecodedTextReadRejectsLookupMismatch(test *testing.T) {
	for name, scenario := range map[string]struct {
		index  int
		from   int
		mutate func(*DocDecodedTextLookupPage)
	}{
		"ordinal":       {0, 0, func(page *DocDecodedTextLookupPage) { page.Index++ }},
		"short-full":    {0, 0, func(page *DocDecodedTextLookupPage) { page.Pages = page.Pages[:127] }},
		"short-final":   {1, 128 * 512, func(page *DocDecodedTextLookupPage) { page.Pages = page.Pages[:1] }},
		"long-final":    {1, 128 * 512, func(page *DocDecodedTextLookupPage) { page.Pages = append(page.Pages, 129) }},
		"out-of-range":  {1, 128 * 512, func(page *DocDecodedTextLookupPage) { page.Pages[1] = 130 }},
		"bucket-before": {1, 128 * 512, func(page *DocDecodedTextLookupPage) { page.Pages[0] = 127 }},
		"bucket-after":  {1, 128 * 512, func(page *DocDecodedTextLookupPage) { page.Pages[0] = 129 }},
	} {
		test.Run(name, func(test *testing.T) {
			fixture := newDecodedTextReadFixture(test, `"`+strings.Repeat(`\n`, 130*DocDisplayPageRuns)+`"`)
			rewriteDecodedTextReadLookup(test, &fixture, scenario.index, scenario.mutate)
			if got, err := ReadDocDecodedTextBytes(context.Background(), fixture.generation, scenario.from, 1, fixture.load); !errors.Is(err, ErrIntegrity) || got != nil {
				test.Fatal("wrong lookup accepted", got, err)
			}
			if span, err := LocateDocDecodedTextSource(context.Background(), fixture.generation, scenario.from, 1, fixture.load); !errors.Is(err, ErrIntegrity) || span != (DocFindSpan{}) {
				test.Fatal("wrong source lookup accepted", span, err)
			}
		})
	}
}

func TestDocDecodedTextReadRejectsPageAndAdjacencyMismatch(test *testing.T) {
	for name, mutate := range map[string]func(*DocDisplayPage){
		"decoded-gap": func(page *DocDisplayPage) { page.From++ },
		"raw-gap": func(page *DocDisplayPage) {
			for index := range page.Runs {
				page.Runs[index].SourceFrom++
				page.Runs[index].SourceTo++
			}
		},
		"raw-overlap": func(page *DocDisplayPage) {
			for index := range page.Runs {
				page.Runs[index].SourceFrom--
				page.Runs[index].SourceTo--
			}
		},
	} {
		test.Run(name, func(test *testing.T) {
			fixture := newDecodedTextReadFixture(test, `"`+strings.Repeat(`\n`, 1536)+`"`)
			mutate(&fixture.pages[1])
			fixture.rebuild(test)
			if _, err := ReadDocDecodedTextPage(context.Background(), fixture.generation, 1, fixture.load); err != nil {
				test.Fatal("adjacency fixture must be independently valid", err)
			}
			if got, err := ReadDocDecodedTextBytes(context.Background(), fixture.generation, 511, 2, fixture.load); !errors.Is(err, ErrIntegrity) || got != nil {
				test.Fatal("page adjacency mismatch accepted", got, err)
			}
			if span, err := LocateDocDecodedTextSource(context.Background(), fixture.generation, 511, 2, fixture.load); !errors.Is(err, ErrIntegrity) || span != (DocFindSpan{}) {
				test.Fatal("source adjacency mismatch accepted", span, err)
			}
		})
	}
	for name, mutate := range map[string]func(*DocDisplayPage){
		"version": func(page *DocDisplayPage) { page.Version = 1 },
		"source":  func(page *DocDisplayPage) { page.Source = HashContent([]byte("other source")) },
		"ordinal": func(page *DocDisplayPage) { page.Index = 1 },
		"from":    func(page *DocDisplayPage) { page.From = 1 },
	} {
		test.Run(name, func(test *testing.T) {
			fixture := newDecodedTextReadFixture(test, `"hello"`)
			mutate(&fixture.pages[0])
			fixture.rebuild(test)
			page, err := ReadDocDecodedTextPage(context.Background(), fixture.generation, 0, fixture.load)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(page, DocDisplayPage{}) {
				test.Fatal("wrong page accepted", page, err)
			}
		})
	}
	fixture := newDecodedTextReadFixture(test, `"`+strings.Repeat("x", 600)+`"`)
	fixture.pages[0].Text = fixture.pages[0].Text[:599]
	fixture.pages[0].Runs[0].To--
	fixture.pages[0].Runs[0].SourceTo--
	fixture.rebuild(test)
	if page, err := ReadDocDecodedTextPage(context.Background(), fixture.generation, 0, fixture.load); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(page, DocDisplayPage{}) {
		test.Fatal("incomplete final raw end accepted", page, err)
	}
	fixture = newDecodedTextReadFixture(test, `"`+strings.Repeat("x", 600)+`"`)
	fixture.generation.TextBytes++
	if page, err := ReadDocDecodedTextPage(context.Background(), fixture.generation, 0, fixture.load); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(page, DocDisplayPage{}) {
		test.Fatal("incomplete final decoded end accepted", page, err)
	}
}

func TestDocDecodedTextReadRejectsInvalidGenerationBeforeLoading(test *testing.T) {
	fixture := newDecodedTextReadFixture(test, `"hello"`)
	for name, mutate := range map[string]func(*DocDecodedTextGeneration){
		"version":       func(generation *DocDecodedTextGeneration) { generation.Version++ },
		"profile":       func(generation *DocDecodedTextGeneration) { generation.Source.Profile = DocDisplayStringProfile },
		"hash":          func(generation *DocDecodedTextGeneration) { generation.Hash = "bad" },
		"directory":     func(generation *DocDecodedTextGeneration) { generation.Pages.Hash = "bad" },
		"lookup":        func(generation *DocDecodedTextGeneration) { generation.Lookup.Hash = "bad" },
		"lookup-count":  func(generation *DocDecodedTextGeneration) { generation.Lookup.Count++ },
		"negative-size": func(generation *DocDecodedTextGeneration) { generation.TextBytes = -1 },
	} {
		test.Run(name, func(test *testing.T) {
			generation := fixture.generation
			mutate(&generation)
			load := func(context.Context, ContentHash) ([]byte, error) {
				test.Fatal("invalid generation loaded an object")
				return nil, ErrIntegrity
			}
			if page, err := ReadDocDecodedTextPage(context.Background(), generation, 0, load); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(page, DocDisplayPage{}) {
				test.Fatal(page, err)
			}
			if got, err := ReadDocDecodedTextBytes(context.Background(), generation, 0, 1, load); !errors.Is(err, ErrIntegrity) || got != nil {
				test.Fatal(got, err)
			}
			if span, err := LocateDocDecodedTextSource(context.Background(), generation, 0, 1, load); !errors.Is(err, ErrIntegrity) || span != (DocFindSpan{}) {
				test.Fatal(span, err)
			}
		})
	}
}

func TestDocDecodedTextReadRejectsImpossiblePagePrefix(test *testing.T) {
	fixture := newDecodedTextReadFixture(test, `"`+strings.Repeat("x", 3*DocDisplayPageTextBytes)+`"`)
	fixture.pages[1].From++
	fixture.pages[1].Runs[0].SourceFrom++
	fixture.pages[1].Runs[0].SourceTo++
	fixture.rebuild(test)
	raw, err := canonicalJSON(fixture.pages[1])
	if err != nil {
		test.Fatal(err)
	}
	if _, err := DecodeDocDecodedTextPage(fixture.source, HashContent(raw), raw); err != nil {
		test.Fatal("prefix fixture must have valid local coordinates", err)
	}
	page, err := ReadDocDecodedTextPage(context.Background(), fixture.generation, 1, fixture.load)
	if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(page, DocDisplayPage{}) {
		test.Fatal("impossible page prefix accepted", page, err)
	}
}

func TestDocDecodedTextReadFailureAndCancellationZeroOutput(test *testing.T) {
	fixture := newDecodedTextReadFixture(test, `"`+strings.Repeat(`\n`, 6000)+`"`)
	operations := map[string]func(context.Context, func(context.Context, ContentHash) ([]byte, error)) (bool, error){
		"page": func(ctx context.Context, load func(context.Context, ContentHash) ([]byte, error)) (bool, error) {
			page, err := ReadDocDecodedTextPage(ctx, fixture.generation, 0, load)
			return reflect.DeepEqual(page, DocDisplayPage{}), err
		},
		"bytes": func(ctx context.Context, load func(context.Context, ContentHash) ([]byte, error)) (bool, error) {
			got, err := ReadDocDecodedTextBytes(ctx, fixture.generation, 511, 4096, load)
			return got == nil, err
		},
		"source": func(ctx context.Context, load func(context.Context, ContentHash) ([]byte, error)) (bool, error) {
			span, err := LocateDocDecodedTextSource(ctx, fixture.generation, 1, 5998, load)
			return span == (DocFindSpan{}), err
		},
	}
	for name, operation := range operations {
		test.Run(name, func(test *testing.T) {
			calls := 0
			if _, err := operation(context.Background(), func(ctx context.Context, hash ContentHash) ([]byte, error) {
				calls++
				return fixture.load(ctx, hash)
			}); err != nil {
				test.Fatal(err)
			}
			for _, mode := range []string{"error", "corrupt", "cancel", "cancel-error"} {
				for failAt := 1; failAt <= calls; failAt++ {
					test.Run(fmt.Sprintf("%s/%d", mode, failAt), func(test *testing.T) {
						ctx, cancel := context.WithCancel(context.Background())
						defer cancel()
						failure := errors.New("load failed")
						count := 0
						zero, err := operation(ctx, func(ctx context.Context, hash ContentHash) ([]byte, error) {
							count++
							if count == failAt {
								switch mode {
								case "error":
									return nil, failure
								case "corrupt":
									return []byte(`{}`), nil
								case "cancel":
									cancel()
								case "cancel-error":
									cancel()
									return nil, failure
								}
							}
							return fixture.load(ctx, hash)
						})
						want := failure
						if mode == "corrupt" {
							want = ErrIntegrity
						} else if strings.HasPrefix(mode, "cancel") {
							want = context.Canceled
						}
						if !zero || !errors.Is(err, want) {
							test.Fatal("failed read leaked output or wrong error", zero, err)
						}
					})
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			zero, err := operation(ctx, func(context.Context, ContentHash) ([]byte, error) {
				test.Fatal("canceled read loaded an object")
				return nil, nil
			})
			if !zero || !errors.Is(err, context.Canceled) {
				test.Fatal(zero, err)
			}
		})
	}
}
