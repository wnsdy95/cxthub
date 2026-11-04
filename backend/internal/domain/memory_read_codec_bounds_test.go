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
)

func TestMemoryReadHeaderValidation(test *testing.T) {
	items := memoryReadTestItems(1)
	original := memoryReadTestBuild(test, memoryReadTestHeader(test, items), items).Header
	cases := []struct {
		name   string
		mutate func(*MemoryReadHeader)
		want   error
	}{
		{"version", func(header *MemoryReadHeader) { header.Version++ }, ErrIntegrity},
		{"rules", func(header *MemoryReadHeader) { header.Rules++ }, ErrIntegrity},
		{"capture", func(header *MemoryReadHeader) { header.CaptureID = "not-a-hash" }, ErrIntegrity},
		{"source version", func(header *MemoryReadHeader) { header.Source.Version++ }, ErrIntegrity},
		{"source graph", func(header *MemoryReadHeader) { header.Source.Graph.Scope = "wrong" }, ErrIntegrity},
		{"source epoch", func(header *MemoryReadHeader) { header.Source.Epoch = "00000000-0000-0000-0000-000000000000" }, ErrIntegrity},
		{"source sequence", func(header *MemoryReadHeader) { header.Source.Sequence = -1 }, ErrIntegrity},
		{"selection", func(header *MemoryReadHeader) { header.View.Selection.CodeCommit = "HEAD" }, ErrIntegrity},
		{"branch and pin", func(header *MemoryReadHeader) {
			header.View.Selection.Branch = "main"
			header.View.Selection.MemoryHash = HashContent(nil)
		}, ErrIntegrity},
		{"state", func(header *MemoryReadHeader) { header.View.StateHash = "" }, ErrIntegrity},
		{"lineage", func(header *MemoryReadHeader) { header.View.LineageHash = "" }, ErrIntegrity},
		{"delivery", func(header *MemoryReadHeader) { header.View.DeliveryStateHash = "" }, ErrIntegrity},
		{"content", func(header *MemoryReadHeader) { header.View.Content = "all" }, ErrIntegrity},
		{"empty page items", func(header *MemoryReadHeader) { header.View.Items = []EffectiveMemoryItem{} }, ErrIntegrity},
		{"page items", func(header *MemoryReadHeader) { header.View.Items = items }, ErrIntegrity},
		{"cursor", func(header *MemoryReadHeader) { header.View.NextCursor = "next" }, ErrIntegrity},
		{"negative total", func(header *MemoryReadHeader) { header.View.Total = -1 }, ErrIntegrity},
		{"item cap", func(header *MemoryReadHeader) { header.View.Total = MaxMemoryReadItems + 1 }, ErrMemoryProjectionLimit},
		{"chunk cap", func(header *MemoryReadHeader) { header.Chunks = make([]MemoryReadChunkRef, MaxMemoryReadChunks+1) }, ErrMemoryProjectionLimit},
		{"dependency cap", func(header *MemoryReadHeader) { header.Dependencies = make([]MemoryReadDependency, 4097) }, ErrMemoryProjectionLimit},
		{"dependency hash", func(header *MemoryReadHeader) { header.Dependencies = []MemoryReadDependency{{Hash: "bad"}} }, ErrIntegrity},
		{"duplicate dependency", func(header *MemoryReadHeader) {
			header.Dependencies = []MemoryReadDependency{{Hash: HashContent(nil)}, {Hash: HashContent(nil), Available: true}}
		}, ErrIntegrity},
		{"negative start", func(header *MemoryReadHeader) { header.Chunks[0].Start = -1 }, ErrIntegrity},
		{"zero count", func(header *MemoryReadHeader) { header.Chunks[0].Count = 0 }, ErrIntegrity},
		{"negative count", func(header *MemoryReadHeader) { header.Chunks[0].Count = -1 }, ErrIntegrity},
		{"chunk items", func(header *MemoryReadHeader) { header.Chunks[0].Count = MaxMemoryReadChunkItems + 1 }, ErrMemoryProjectionLimit},
		{"chunk bytes", func(header *MemoryReadHeader) { header.Chunks[0].Bytes = MaxMemoryReadChunkBytes + 1 }, ErrMemoryProjectionLimit},
		{"negative bytes", func(header *MemoryReadHeader) { header.Chunks[0].Bytes = -1 }, ErrIntegrity},
		{"short body", func(header *MemoryReadHeader) { header.Chunks[0].Bytes = 2 }, ErrIntegrity},
		{"chunk hash", func(header *MemoryReadHeader) { header.Chunks[0].Hash = "bad" }, ErrIntegrity},
		{"large string", func(header *MemoryReadHeader) { header.Origin = strings.Repeat("x", MaxMemoryReadHeaderBytes+1) }, ErrMemoryProjectionLimit},
		{"expanded JSON string", func(header *MemoryReadHeader) { header.Origin = strings.Repeat("&", MaxMemoryReadHeaderBytes/6) }, ErrMemoryProjectionLimit},
		{"unicode", func(header *MemoryReadHeader) { header.Origin = string([]byte{0xff}) }, ErrIntegrity},
		{"nested unicode", func(header *MemoryReadHeader) { header.View.Inclusion = &BranchContext{Reason: string([]byte{0xff})} }, ErrIntegrity},
		{"nested slice", func(header *MemoryReadHeader) {
			header.View.Inclusion = &BranchContext{Roots: make([]ContentHash, MaxMemoryReadHeaderBytes+1)}
		}, ErrMemoryProjectionLimit},
	}
	for _, entry := range cases {
		test.Run(entry.name, func(test *testing.T) {
			header := memoryReadTestCopy(test, original)
			entry.mutate(&header)
			if raw, err := MemoryReadHeaderBytes(context.Background(), header); !errors.Is(err, entry.want) || raw != nil {
				test.Fatal("invalid header accepted", err)
			}
			if err := ValidateMemoryReadBundle(context.Background(), MemoryReadBundle{Header: header}); !errors.Is(err, entry.want) {
				test.Fatal("bundle skipped header validation", err)
			}
		})
	}
	header := memoryReadTestCopy(test, original)
	header.Dependencies = []MemoryReadDependency{{Hash: ContentHash("sha256:" + strings.Repeat("b", 64))}, {Hash: ContentHash("sha256:" + strings.Repeat("a", 64))}}
	if _, err := MemoryReadHeaderBytes(context.Background(), header); !errors.Is(err, ErrIntegrity) {
		test.Fatal("stored unordered dependencies accepted", err)
	}
	memoryReadTestBuild(test, header, items)
}

func memoryReadTestSizedItem(test *testing.T, size int) EffectiveMemoryItem {
	test.Helper()
	item := memoryReadTestItems(1)[0]
	item.Text = ""
	raw, err := json.Marshal(item)
	if err != nil || len(raw) > size {
		test.Fatal("invalid sized item fixture", err)
	}
	item.Text = strings.Repeat("x", size-len(raw))
	return item
}

func TestMemoryReadPreflightAccountsForWireStructure(test *testing.T) {
	items := memoryReadTestItems(1)
	for _, value := range []any{items[0], memoryReadTestHeader(test, items), &BranchContext{Merges: []BranchContextMerge{{Order: -1}}}} {
		raw, err := json.Marshal(value)
		if err != nil {
			test.Fatal(err)
		}
		remaining := len(raw)
		if err := memoryReadMeasure(context.Background(), reflect.ValueOf(value), &remaining); err != nil || remaining != 0 {
			test.Fatal("ASCII preflight size diverged from JSON structure", remaining, err)
		}
	}
	remaining := 1024
	value := BranchContext{Merges: make([]BranchContextMerge, 100)}
	if err := memoryReadMeasure(context.Background(), reflect.ValueOf(value), &remaining); !errors.Is(err, ErrMemoryProjectionLimit) {
		test.Fatal("nested empty fields bypassed pre-allocation accounting", err)
	}
}

func TestMemoryReadExactItemAndChunkBounds(test *testing.T) {
	item := memoryReadTestSizedItem(test, 256<<10)
	items := []EffectiveMemoryItem{item}
	bundle := memoryReadTestBuild(test, memoryReadTestHeader(test, items), items)
	if len(bundle.Chunks) != 1 || len(bundle.Chunks[0].Bytes) != MaxMemoryReadChunkBytes {
		test.Fatal("exact page-size item did not fit its chunk")
	}
	items = append(items, memoryReadTestItems(1)[0])
	bundle = memoryReadTestBuild(test, memoryReadTestHeader(test, items), items)
	if len(bundle.Chunks) != 2 || bundle.Header.Chunks[1].Start != 1 {
		test.Fatal("byte bound did not split consecutive items")
	}
	items = []EffectiveMemoryItem{memoryReadTestSizedItem(test, 128<<10), memoryReadTestSizedItem(test, (128<<10)-1)}
	bundle = memoryReadTestBuild(test, memoryReadTestHeader(test, items), items)
	if len(bundle.Chunks) != 1 || len(bundle.Chunks[0].Bytes) != MaxMemoryReadChunkBytes {
		test.Fatal("separator was not counted in exact chunk boundary")
	}
	items[1].Text += "x"
	bundle = memoryReadTestBuild(test, memoryReadTestHeader(test, items), items)
	if len(bundle.Chunks) != 2 {
		test.Fatal("separator allowed an oversized chunk")
	}
	items = []EffectiveMemoryItem{item}
	items[0].Text += "x"
	if built, err := BuildMemoryReadBundle(context.Background(), memoryReadTestHeader(test, items), items); !errors.Is(err, ErrMemoryProjectionLimit) || !reflect.DeepEqual(built, MemoryReadBundle{}) {
		test.Fatal("oversized serialized item accepted", err)
	}
	items[0].Text = strings.Repeat("&", (256<<10)/6)
	if _, err := BuildMemoryReadBundle(context.Background(), memoryReadTestHeader(test, items), items); !errors.Is(err, ErrMemoryProjectionLimit) {
		test.Fatal("JSON escaping bypassed item byte limit", err)
	}
	items[0].Text = "small"
	items[0].PublicationIDs = make([]string, 21000)
	memoryReadTestBuild(test, memoryReadTestHeader(test, items), items)
}

func TestMemoryReadHeaderAndAggregateBounds(test *testing.T) {
	header := memoryReadTestHeader(test, nil)
	header.Origin = ""
	raw, err := MemoryReadHeaderBytes(context.Background(), header)
	if err != nil {
		test.Fatal(err)
	}
	header.Origin = strings.Repeat("x", MaxMemoryReadHeaderBytes-len(raw))
	raw, err = MemoryReadHeaderBytes(context.Background(), header)
	if err != nil || len(raw) != MaxMemoryReadHeaderBytes {
		test.Fatal("exact header bound rejected", err)
	}
	header.Origin += "x"
	if _, err := MemoryReadHeaderBytes(context.Background(), header); !errors.Is(err, ErrMemoryProjectionLimit) {
		test.Fatal("oversized header accepted", err)
	}
	header = memoryReadTestHeader(test, nil)
	header.View.Total = 256
	for index := range 256 {
		header.Chunks = append(header.Chunks, MemoryReadChunkRef{Hash: HashContent(nil), Start: index, Count: 1, Bytes: MaxMemoryReadChunkBytes})
	}
	for range 4 {
		raw, err = canonicalJSON(header)
		if err != nil {
			test.Fatal(err)
		}
		header.Chunks[255].Bytes = MaxMemoryReadBytes - len(raw) - 255*MaxMemoryReadChunkBytes
	}
	raw, err = MemoryReadHeaderBytes(context.Background(), header)
	if err != nil || len(raw)+header.Chunks[255].Bytes+255*MaxMemoryReadChunkBytes != MaxMemoryReadBytes {
		test.Fatal("exact combined header/body bound rejected", err)
	}
	header.Chunks[255].Bytes++
	if _, err := MemoryReadHeaderBytes(context.Background(), header); !errors.Is(err, ErrMemoryProjectionLimit) {
		test.Fatal("combined byte overflow accepted", err)
	}
	items := make([]EffectiveMemoryItem, 256)
	item := memoryReadTestSizedItem(test, 256<<10)
	for index := range items {
		items[index] = item
	}
	if _, err := BuildMemoryReadBundle(context.Background(), memoryReadTestHeader(test, items), items); !errors.Is(err, ErrMemoryProjectionLimit) {
		test.Fatal("build ignored cumulative chunk bytes", err)
	}
}

func TestMemoryReadItemDependencyAndChunkCaps(test *testing.T) {
	items := memoryReadTestItems(MaxMemoryReadItems)
	memoryReadTestBuild(test, memoryReadTestHeader(test, items), items)
	items = append(items, items[0])
	if _, err := BuildMemoryReadBundle(context.Background(), memoryReadTestHeader(test, items), items); !errors.Is(err, ErrMemoryProjectionLimit) {
		test.Fatal("item cap not enforced", err)
	}
	header := memoryReadTestHeader(test, nil)
	for index := range 4096 {
		header.Dependencies = append(header.Dependencies, MemoryReadDependency{Hash: ContentHash(fmt.Sprintf("sha256:%064x", index)), Available: index%2 == 0})
	}
	memoryReadTestBuild(test, header, nil)
	header.View.Total = MaxMemoryReadChunks
	for index := range MaxMemoryReadChunks {
		header.Chunks = append(header.Chunks, MemoryReadChunkRef{Hash: HashContent(nil), Start: index, Count: 1, Bytes: 512})
	}
	if _, err := MemoryReadHeaderBytes(context.Background(), header); err != nil {
		test.Fatal("exact chunk cap rejected", err)
	}
}

func TestMemoryReadStrictBoundedChunkJSON(test *testing.T) {
	items := memoryReadTestItems(1)
	original := memoryReadTestBuild(test, memoryReadTestHeader(test, items), items)
	valid := string(original.Chunks[0].Bytes)
	cases := map[string]string{
		"null array":             "null",
		"object":                 `{}`,
		"null item":              `[null]`,
		"empty array":            `[]`,
		"missing item fields":    `[{}]`,
		"extra item":             valid[:len(valid)-1] + "," + valid[1:],
		"unknown field":          strings.Replace(valid, `"kind":`, `"unknown":"lost","kind":`, 1),
		"duplicate key":          strings.Replace(valid, `"kind":`, `"kind":"decision","kind":`, 1),
		"duplicate escaped key":  strings.Replace(valid, `"kind":`, `"\u006bind":"decision","kind":`, 1),
		"case folded field":      strings.Replace(valid, `"id":`, `"ID":`, 1),
		"missing field":          strings.Replace(valid, `,"reason":"untyped_historical_text"`, "", 1),
		"null scalar":            strings.Replace(valid, `"review"`, `null`, 1),
		"wrong type":             strings.Replace(valid, `"review"`, `true`, 1),
		"negative part":          strings.Replace(valid, `"kind":`, `"part":-1,"kind":`, 1),
		"fractional part":        strings.Replace(valid, `"kind":`, `"part":0.5,"kind":`, 1),
		"overflow part":          strings.Replace(valid, `"kind":`, `"part":9223372036854775808,"kind":`, 1),
		"invalid ID":             strings.Replace(valid, string(items[0].ID), "bad", 1),
		"invalid source":         strings.Replace(valid, string(items[0].SourceSnapshot), "bad", 1),
		"invalid text hash":      strings.Replace(valid, string(items[0].TextHash), "bad", 1),
		"invalid kind":           strings.Replace(valid, `"legacy_summary"`, `"instruction"`, 1),
		"invalid state":          strings.Replace(valid, `"review"`, `"verified"`, 1),
		"code without scope":     strings.Replace(valid, `"legacy_summary"`, `"code"`, 1),
		"trailing object":        valid + `{}`,
		"trailing array":         valid + `[]`,
		"trailing garbage":       valid + ` trailing`,
		"invalid UTF8":           strings.Replace(valid, "historical text 0", string([]byte{0xff}), 1),
		"lone high surrogate":    strings.Replace(valid, "historical text 0", `\ud800`, 1),
		"lone low surrogate":     strings.Replace(valid, "historical text 0", `\udfff`, 1),
		"invalid surrogate pair": strings.Replace(valid, "historical text 0", `\ud800\u0061`, 1),
		"deep wrong shape":       strings.Replace(valid, `"historical text 0"`, strings.Repeat("[", 1000)+"0"+strings.Repeat("]", 1000), 1),
	}
	for name, value := range cases {
		test.Run(name, func(test *testing.T) {
			header := memoryReadTestCopy(test, original.Header)
			raw := []byte(value)
			header.Chunks[0].Hash, header.Chunks[0].Bytes = HashContent(raw), len(raw)
			if decoded, err := DecodeMemoryReadChunk(context.Background(), header, 0, raw); !errors.Is(err, ErrIntegrity) || decoded != nil {
				test.Fatal("malformed JSON was accepted or partially decoded", err)
			}
		})
	}
	for _, text := range []string{`\ud83d\ude00`, `literal \\ud800`, `\ufffd`, `\"quoted\"`} {
		header := memoryReadTestCopy(test, original.Header)
		raw := []byte(strings.Replace(valid, "historical text 0", text, 1))
		header.Chunks[0].Hash, header.Chunks[0].Bytes = HashContent(raw), len(raw)
		if _, err := DecodeMemoryReadChunk(context.Background(), header, 0, raw); err != nil {
			test.Fatal("valid JSON string rejected", text, err)
		}
	}
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, original.Chunks[0].Bytes, "", "  "); err != nil {
		test.Fatal(err)
	}
	header := memoryReadTestCopy(test, original.Header)
	raw := formatted.Bytes()
	header.Chunks[0].Hash, header.Chunks[0].Bytes = HashContent(raw), len(raw)
	if decoded, err := DecodeMemoryReadChunk(context.Background(), header, 0, raw); err != nil || !reflect.DeepEqual(decoded, items) {
		test.Fatal("strict decoding incorrectly required compact JSON", err)
	}
	if decoded, err := DecodeMemoryReadChunk(context.Background(), original.Header, 0, make([]byte, MaxMemoryReadChunkBytes+1)); !errors.Is(err, ErrMemoryProjectionLimit) || decoded != nil {
		test.Fatal("unbounded chunk decode", err)
	}
}

type memoryReadTestContext struct {
	context.Context
	checks      int
	cancelAfter int
}

func (ctx *memoryReadTestContext) Err() error {
	ctx.checks++
	if ctx.checks >= ctx.cancelAfter {
		return context.Canceled
	}
	return nil
}

func TestMemoryReadCancellation(test *testing.T) {
	items := memoryReadTestItems(130)
	header := memoryReadTestHeader(test, items)
	bundle := memoryReadTestBuild(test, header, items)
	for _, cancelAfter := range []int{1, 100, 500, 1000} {
		ctx := &memoryReadTestContext{Context: context.Background(), cancelAfter: cancelAfter}
		if built, err := BuildMemoryReadBundle(ctx, header, items); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(built, MemoryReadBundle{}) {
			test.Fatal("build lost cancellation or returned a partial bundle", cancelAfter, err)
		}
		ctx = &memoryReadTestContext{Context: context.Background(), cancelAfter: cancelAfter}
		if err := ValidateMemoryReadBundle(ctx, bundle); !errors.Is(err, context.Canceled) {
			test.Fatal("validation lost cancellation", cancelAfter, err)
		}
		ctx = &memoryReadTestContext{Context: context.Background(), cancelAfter: cancelAfter}
		if decoded, err := DecodeMemoryReadChunk(ctx, bundle.Header, 0, bundle.Chunks[0].Bytes); !errors.Is(err, context.Canceled) || decoded != nil {
			test.Fatal("decode lost cancellation or returned partial items", cancelAfter, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if raw, err := MemoryReadHeaderBytes(ctx, bundle.Header); !errors.Is(err, context.Canceled) || raw != nil {
		test.Fatal("header serialization ignored cancellation", err)
	}
}
