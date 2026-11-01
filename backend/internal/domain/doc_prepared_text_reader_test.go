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

type preparedTextReaderFixture struct {
	source  ContentHash
	text    string
	pages   []DocDisplayPage
	lookup  DocRangeDirectoryRoot
	objects map[ContentHash][]byte
}

func newPreparedTextReaderFixture(test *testing.T, text string) preparedTextReaderFixture {
	test.Helper()
	fixture := preparedTextReaderFixture{source: HashContent([]byte("separate prepared summary profile")), text: text, objects: make(map[ContentHash][]byte)}
	for from := 0; from < len(text); from += 513 {
		index := len(fixture.pages)
		body := text[from:min(from+513, len(text))]
		fixture.pages = append(fixture.pages, DocDisplayPage{
			Version: 2, Source: fixture.source, Index: index, From: from, Text: body,
			Runs: []DocDisplayRun{{To: len(body), SourceFrom: index * 100000, SourceTo: index*100000 + len(body), Linear: true}},
		})
	}
	entries := (len(text) + DocDecodedTextLookupStride - 1) / DocDecodedTextLookupStride
	var hashes []ContentHash
	for start := 0; start < entries; start += DocDecodedTextLookupEntries {
		page := DocDecodedTextLookupPage{Version: 1, Source: fixture.source, Index: start / DocDecodedTextLookupEntries}
		for bucket := start; bucket < min(start+DocDecodedTextLookupEntries, entries); bucket++ {
			page.Pages = append(page.Pages, bucket*DocDecodedTextLookupStride/513)
		}
		raw, err := page.encodeForSource(fixture.source)
		if err != nil {
			test.Fatal(err)
		}
		hash := HashContent(raw)
		hashes = append(hashes, hash)
		fixture.objects[hash] = raw
	}
	var objects []DocRangeObject
	var err error
	fixture.lookup, err = buildDocRangeDirectory(context.Background(), hashes, &objects)
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range objects {
		fixture.objects[object.Hash] = object.Bytes
	}
	return fixture
}

func (fixture preparedTextReaderFixture) load(_ context.Context, hash ContentHash) ([]byte, error) {
	raw, exists := fixture.objects[hash]
	if !exists {
		return nil, ErrNotFound
	}
	return raw, nil
}

func (fixture preparedTextReaderFixture) readPage(_ context.Context, index int) (DocDisplayPage, error) {
	if index < 0 || index >= len(fixture.pages) {
		return DocDisplayPage{}, ErrIntegrity
	}
	return fixture.pages[index], nil
}

func adjacentPreparedTextTestPages(first, last DocDisplayPage) bool {
	return last.Index == first.Index+1 && last.From == first.From+len(first.Text)
}

func (fixture preparedTextReaderFixture) reader(test *testing.T) *docPreparedTextReader {
	test.Helper()
	reader, err := newDocPreparedTextReader(len(fixture.text), len(fixture.pages), fixture.lookup, fixture.source, fixture.load, fixture.readPage, adjacentPreparedTextTestPages)
	if err != nil {
		test.Fatal(err)
	}
	return reader
}

func TestDocPreparedTextLookupCodecPreservesLiteralBytes(test *testing.T) {
	fixture := newDecodedTextReadFixture(test, `"hello"`)
	source, err := fixture.source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	page := DocDecodedTextLookupPage{Version: 1, Source: source, Pages: []int{0, 0, 1}}
	expected := []byte(fmt.Sprintf(`{"index":0,"pages":[0,0,1],"source":%q,"version":1}`, source))
	literal, err := page.Encode(fixture.source)
	if err != nil || !bytes.Equal(literal, expected) {
		test.Fatal("literal canonical bytes changed", string(literal), err)
	}
	prepared, err := page.encodeForSource(source)
	if err != nil || !bytes.Equal(prepared, expected) {
		test.Fatal("generic canonical bytes differ", string(prepared), err)
	}
	decoded, err := decodeDocTextLookupPage(source, HashContent(expected), expected)
	if err != nil || !reflect.DeepEqual(decoded, page) {
		test.Fatal(decoded, err)
	}
	for name, mutate := range map[string]func(*DocDecodedTextSource){
		"version": func(source *DocDecodedTextSource) { source.Version++ },
		"profile": func(source *DocDecodedTextSource) { source.Profile = "prepared-summary-v1" },
		"hash":    func(source *DocDecodedTextSource) { source.EventHash = "bad" },
		"bounds":  func(source *DocDecodedTextSource) { source.Token.To = source.EventBytes + 1 },
	} {
		test.Run(name, func(test *testing.T) {
			invalid := fixture.source
			mutate(&invalid)
			encoded, err := canonicalJSON(invalid)
			if err != nil {
				test.Fatal(err)
			}
			candidate := page
			candidate.Source = HashContent(encoded)
			raw, err := candidate.encodeForSource(candidate.Source)
			if err != nil {
				test.Fatal("private codec rejected a separate source hash", err)
			}
			if err := candidate.Validate(invalid); !errors.Is(err, ErrIntegrity) {
				test.Fatal("literal validator accepted invalid source", err)
			}
			if got, err := candidate.Encode(invalid); !errors.Is(err, ErrIntegrity) || got != nil {
				test.Fatal("literal encoder accepted invalid source", got, err)
			}
			if got, err := DecodeDocDecodedTextLookupPage(invalid, HashContent(raw), raw); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(got, DocDecodedTextLookupPage{}) {
				test.Fatal("literal decoder accepted invalid source", got, err)
			}
		})
	}
}

func TestDocPreparedTextLookupCodecRejectsInvalidObjects(test *testing.T) {
	source := HashContent([]byte("summary source"))
	page := DocDecodedTextLookupPage{Version: 1, Source: source, Pages: []int{0}}
	raw, err := page.encodeForSource(source)
	if err != nil {
		test.Fatal(err)
	}
	for _, invalid := range []ContentHash{"", "bad", HashContent([]byte("other source"))} {
		if err := page.validateForSource(invalid); !errors.Is(err, ErrIntegrity) {
			test.Fatal("invalid source validated", invalid, err)
		}
		if got, err := page.encodeForSource(invalid); !errors.Is(err, ErrIntegrity) || got != nil {
			test.Fatal("invalid source encoded", invalid, err)
		}
		if got, err := decodeDocTextLookupPage(invalid, HashContent(raw), raw); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(got, DocDecodedTextLookupPage{}) {
			test.Fatal("invalid source decoded", invalid, got, err)
		}
	}
	for name, encoded := range map[string][]byte{
		"empty":     nil,
		"oversized": bytes.Repeat([]byte(" "), MaxDocDecodedTextLookupBytes+1),
		"space":     append(bytes.Clone(raw), ' '),
		"unknown":   append(bytes.Clone(raw[:len(raw)-1]), []byte(`,"extra":1}`)...),
		"duplicate": append(bytes.Clone(raw[:len(raw)-1]), []byte(`,"version":1}`)...),
		"fraction":  []byte(strings.Replace(string(raw), `"version":1`, `"version":1.0`, 1)),
	} {
		test.Run(name, func(test *testing.T) {
			got, err := decodeDocTextLookupPage(source, HashContent(encoded), encoded)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(got, DocDecodedTextLookupPage{}) {
				test.Fatal("noncanonical private lookup accepted", got, err)
			}
		})
	}
	for _, expected := range []ContentHash{"bad", HashContent([]byte("other object"))} {
		if got, err := decodeDocTextLookupPage(source, expected, raw); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(got, DocDecodedTextLookupPage{}) {
			test.Fatal("wrong object hash accepted", got, err)
		}
	}
}

func TestDocPreparedTextReaderNonliteralSourceExactRanges(test *testing.T) {
	fixture := newPreparedTextReaderFixture(test, strings.Repeat("한🙂x\n", 16000))
	if adjacentDocDecodedTextPages(fixture.pages[0], fixture.pages[1]) {
		test.Fatal("fixture must require nonliteral source adjacency")
	}
	for _, from := range []int{0, 1, 511, 512, 513, 514, 65535, 65536, 131071, 131072, len(fixture.text) - 1, len(fixture.text)} {
		test.Run(fmt.Sprint(from), func(test *testing.T) {
			test.Parallel()
			reader := fixture.reader(test)
			for _, length := range []int{0, 1, 2, 4, 511, 512, 4096} {
				if length > len(fixture.text)-from {
					continue
				}
				got, err := reader.readBytes(context.Background(), from, length)
				if err != nil || !bytes.Equal(got, []byte(fixture.text[from:from+length])) {
					test.Fatal("inexact prepared read", from, length, got, err)
				}
			}
			if from < len(fixture.text) {
				got, err := reader.selectPage(context.Background(), from)
				if err != nil || !reflect.DeepEqual(got, fixture.pages[from/513]) {
					test.Fatal("wrong selected page", from, got, err)
				}
			}
		})
	}
}

func TestDocPreparedTextReaderTwoEntryCachesAndScope(test *testing.T) {
	fixture := newPreparedTextReaderFixture(test, strings.Repeat("x", 513*257))
	reader := fixture.reader(test)
	pageLoads, lookupLoads := 0, 0
	reader.readPage = func(ctx context.Context, index int) (DocDisplayPage, error) {
		pageLoads++
		return fixture.readPage(ctx, index)
	}
	reader.load = func(ctx context.Context, hash ContentHash) ([]byte, error) {
		raw, err := fixture.load(ctx, hash)
		var header DocDecodedTextLookupPage
		if err == nil && json.Unmarshal(raw, &header) == nil && header.Version == 1 {
			lookupLoads++
		}
		return raw, err
	}
	for _, index := range []int{0, 1, 0, 2, 0} {
		if _, err := reader.page(context.Background(), index); err != nil {
			test.Fatal(err)
		}
		if _, err := reader.lookup(context.Background(), index); err != nil {
			test.Fatal(err)
		}
	}
	if pageLoads != 4 || lookupLoads != 4 || len(reader.pages) != 2 || len(reader.lookups) != 2 {
		test.Fatal("parsed page caches are not bounded to two entries", pageLoads, lookupLoads)
	}
	loadsBefore := pageLoads + lookupLoads
	if got, err := reader.readBytes(context.Background(), 0, 6); err != nil || string(got) != "xxxxxx" || pageLoads+lookupLoads != loadsBefore {
		test.Fatal("warm range reparsed cached pages", got, err)
	}
	fresh, err := newDocPreparedTextReader(len(fixture.text), len(fixture.pages), fixture.lookup, fixture.source,
		func(context.Context, ContentHash) ([]byte, error) { return nil, ErrNotFound }, fixture.readPage, adjacentPreparedTextTestPages)
	if err != nil {
		test.Fatal(err)
	}
	if got, err := fresh.readBytes(context.Background(), 0, 6); !errors.Is(err, ErrNotFound) || got != nil {
		test.Fatal("new operation reused an old lookup", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := reader.page(ctx, 0); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, DocDisplayPage{}) {
		test.Fatal("page cache bypassed cancellation", got, err)
	}
	if got, err := reader.lookup(ctx, 0); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, DocDecodedTextLookupPage{}) {
		test.Fatal("lookup cache bypassed cancellation", got, err)
	}
	if got, err := reader.readBytes(ctx, 0, 6); !errors.Is(err, context.Canceled) || got != nil || pageLoads+lookupLoads != loadsBefore {
		test.Fatal("range cache bypassed cancellation", got, err)
	}
}

func TestDocPreparedTextReaderRejectsInvalidConstructionBeforeIO(test *testing.T) {
	fixture := newPreparedTextReaderFixture(test, strings.Repeat("x", 1024))
	for name, mutate := range map[string]func(*docPreparedTextReader){
		"negative-bytes": func(reader *docPreparedTextReader) { reader.textBytes = -1 },
		"byte-overflow":  func(reader *docPreparedTextReader) { reader.textBytes = math.MaxInt },
		"byte-limit":     func(reader *docPreparedTextReader) { reader.textBytes = MaxConversationDocumentBytes + 1 },
		"capacity":       func(reader *docPreparedTextReader) { reader.textBytes = 2*DocDisplayPageTextBytes + 1 },
		"negative-pages": func(reader *docPreparedTextReader) { reader.pageCount = -1 },
		"page-overflow":  func(reader *docPreparedTextReader) { reader.pageCount = math.MaxInt },
		"no-pages":       func(reader *docPreparedTextReader) { reader.pageCount = 0 },
		"short-pages":    func(reader *docPreparedTextReader) { reader.pageCount = 3 },
		"empty":          func(reader *docPreparedTextReader) { reader.textBytes = 0 },
		"source":         func(reader *docPreparedTextReader) { reader.source = "bad" },
		"lookup-hash":    func(reader *docPreparedTextReader) { reader.lookupRoot.Hash = "bad" },
		"lookup-count":   func(reader *docPreparedTextReader) { reader.lookupRoot.Count++ },
		"loader":         func(reader *docPreparedTextReader) { reader.load = nil },
		"page-reader":    func(reader *docPreparedTextReader) { reader.readPage = nil },
		"adjacency":      func(reader *docPreparedTextReader) { reader.adjacent = nil },
	} {
		test.Run(name, func(test *testing.T) {
			candidate := *fixture.reader(test)
			candidate.load = func(context.Context, ContentHash) ([]byte, error) {
				test.Fatal("invalid constructor loaded an object")
				return nil, nil
			}
			candidate.readPage = func(context.Context, int) (DocDisplayPage, error) {
				test.Fatal("invalid constructor read a data page")
				return DocDisplayPage{}, nil
			}
			mutate(&candidate)
			got, err := newDocPreparedTextReader(candidate.textBytes, candidate.pageCount, candidate.lookupRoot, candidate.source, candidate.load, candidate.readPage, candidate.adjacent)
			if !errors.Is(err, ErrIntegrity) || got != nil {
				test.Fatal("invalid reader constructed", got, err)
			}
		})
	}
	empty := newPreparedTextReaderFixture(test, "").reader(test)
	if got, err := empty.readBytes(context.Background(), 0, 0); err != nil || got == nil || len(got) != 0 {
		test.Fatal("empty prepared text failed", got, err)
	}
}

func TestDocPreparedTextReaderRejectsInvalidRangesBeforeIO(test *testing.T) {
	fixture := newPreparedTextReaderFixture(test, strings.Repeat("x", 6000))
	reader := fixture.reader(test)
	reader.load = func(context.Context, ContentHash) ([]byte, error) {
		test.Fatal("invalid range loaded an object")
		return nil, nil
	}
	reader.readPage = func(context.Context, int) (DocDisplayPage, error) {
		test.Fatal("invalid range read a page")
		return DocDisplayPage{}, nil
	}
	for _, coordinates := range [][2]int{{-1, 0}, {0, -1}, {0, 4097}, {math.MaxInt, 1}, {0, math.MaxInt}, {len(fixture.text), 1}, {len(fixture.text) + 1, 0}} {
		if got, err := reader.readBytes(context.Background(), coordinates[0], coordinates[1]); !errors.Is(err, ErrValidation) || got != nil {
			test.Fatal("invalid byte range accepted", coordinates, got, err)
		}
	}
	for _, from := range []int{-1, len(fixture.text), math.MaxInt} {
		if got, err := reader.selectPage(context.Background(), from); !errors.Is(err, ErrValidation) || !reflect.DeepEqual(got, DocDisplayPage{}) {
			test.Fatal("invalid selection accepted", from, got, err)
		}
	}
	for _, index := range []int{-1, len(fixture.pages), math.MaxInt} {
		if got, err := reader.page(context.Background(), index); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(got, DocDisplayPage{}) {
			test.Fatal("invalid page ordinal accepted", index, got, err)
		}
	}
}

func TestDocPreparedTextReaderRejectsMalformedCallbackPages(test *testing.T) {
	fixture := newPreparedTextReaderFixture(test, strings.Repeat("x", 513*3))
	for name, mutate := range map[string]func(*DocDisplayPage){
		"source":    func(page *DocDisplayPage) { page.Source = HashContent([]byte("other source")) },
		"index":     func(page *DocDisplayPage) { page.Index++ },
		"negative":  func(page *DocDisplayPage) { page.From = -1 },
		"beyond":    func(page *DocDisplayPage) { page.From = math.MaxInt },
		"empty":     func(page *DocDisplayPage) { page.Text = "" },
		"oversized": func(page *DocDisplayPage) { page.Text = strings.Repeat("x", DocDisplayPageTextBytes+1) },
	} {
		test.Run(name, func(test *testing.T) {
			reader := fixture.reader(test)
			reader.readPage = func(ctx context.Context, index int) (DocDisplayPage, error) {
				page, err := fixture.readPage(ctx, index)
				mutate(&page)
				return page, err
			}
			if got, err := reader.readBytes(context.Background(), 0, 1); !errors.Is(err, ErrIntegrity) || got != nil {
				test.Fatal("malformed callback page accepted", got, err)
			}
			reader.readPage = fixture.readPage
			if got, err := reader.readBytes(context.Background(), 0, 1); err != nil || string(got) != "x" {
				test.Fatal("malformed page entered parsed cache", got, err)
			}
		})
	}
	for _, coordinates := range [][2]int{{513, 1}, {512, 2}} {
		reader := fixture.reader(test)
		reader.adjacent = func(DocDisplayPage, DocDisplayPage) bool { return false }
		if got, err := reader.readBytes(context.Background(), coordinates[0], coordinates[1]); !errors.Is(err, ErrIntegrity) || got != nil {
			test.Fatal("source-specific adjacency was not enforced", coordinates, got, err)
		}
	}
}

func TestDocPreparedTextReaderFailureAndCancellationZeroOutput(test *testing.T) {
	fixture := newPreparedTextReaderFixture(test, strings.Repeat("x", 6000))
	failure := errors.New("prepared page failed")
	for _, mode := range []string{"error", "cancel", "cancel-error"} {
		for failAt := 0; failAt < 9; failAt++ {
			test.Run(fmt.Sprintf("%s/%d", mode, failAt), func(test *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				reader := fixture.reader(test)
				reader.readPage = func(ctx context.Context, index int) (DocDisplayPage, error) {
					if index == failAt {
						if strings.HasPrefix(mode, "cancel") {
							cancel()
						}
						if mode != "cancel" {
							return DocDisplayPage{}, failure
						}
					}
					return fixture.readPage(ctx, index)
				}
				got, err := reader.readBytes(ctx, 511, 4096)
				want := failure
				if strings.HasPrefix(mode, "cancel") {
					want = context.Canceled
				}
				if got != nil || !errors.Is(err, want) {
					test.Fatal("failed prepared read leaked output or wrong error", got, err)
				}
			})
		}
	}
}
