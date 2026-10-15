package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocViewLocalCorpus(t *testing.T) {
	source := os.Getenv("CXT_RANGE_CORPUS_DIR")
	hash := domain.ContentHash(os.Getenv("CXT_RANGE_CORPUS_HASH"))
	if source == "" && hash == "" {
		t.Skip("opt-in: provide CXT_RANGE_CORPUS_DIR and CXT_RANGE_CORPUS_HASH; only a temporary copy is modified")
	}
	if !filepath.IsAbs(source) || domain.ValidateContentHash(hash) != nil {
		t.Fatal("requires an absolute CLI .cxt directory and full document hash")
	}
	read := func(path string, limit int64) []byte {
		t.Helper()
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		raw, err := io.ReadAll(io.LimitReader(file, limit+1))
		if err != nil || int64(len(raw)) > limit {
			t.Fatal("invalid bounded source read", err)
		}
		return raw
	}
	name := strings.TrimPrefix(string(hash), "sha256:")
	rootPath := filepath.Join(source, "objects", "docs", name)
	stored := read(rootPath, 2*domain.MaxConversationManifestBytes)
	raw := stored
	if len(raw) >= 4 && bytes.Equal(raw[:4], []byte{0x28, 0xb5, 0x2f, 0xfd}) {
		decoder, err := zstd.NewReader(bytes.NewReader(raw), zstd.WithDecoderMaxMemory(2*domain.MaxConversationManifestBytes))
		if err != nil {
			t.Fatal(err)
		}
		defer decoder.Close()
		raw, err = io.ReadAll(io.LimitReader(decoder, domain.MaxConversationManifestBytes+1))
		if err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := domain.DecodeConversationManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := domain.ConversationManifestHash(manifest)
	if err != nil || actual != hash {
		t.Fatal("source root identity mismatch", err)
	}
	directory, repo := t.TempDir(), domain.HashContent([]byte(t.Name()))
	inputs := map[string]domain.ContentHash{}
	copyObject := func(kind string, object domain.ContentHash, limit int64) {
		t.Helper()
		path := filepath.Join(source, "objects", kind, strings.TrimPrefix(string(object), "sha256:"))
		if _, copied := inputs[path]; copied {
			return
		}
		body := read(path, limit)
		inputs[path] = domain.HashContent(body)
		destination := rootReadObjectPath(directory, repo, kind, object)
		if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destination, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	copyObject("docs", hash, 2*domain.MaxConversationManifestBytes)
	for _, chunk := range manifest.Chunks {
		copyObject("chunks", chunk.Hash, 2*domain.ConversationManifestChunkBytes)
	}
	ctx, cancel := context.WithTimeout(systemTestContext(), 3*time.Minute)
	defer cancel()
	storage := store.NewFSStore(directory)
	if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: hash, RepoID: repo, DocHash: hash, DocIdentity: domain.DocumentIdentityRootV1, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := storage.EnsureDocRangeIndex(ctx, repo, hash); err != nil {
		t.Fatal(err)
	}
	t.Logf("source bytes=%d events=%d chunks=%d cold_index=%s; local copy only", manifest.StreamBytes, manifest.EventCount, len(manifest.Chunks), time.Since(started))
	header, err := storage.DocRangeIndex(ctx, repo, hash)
	if err != nil {
		t.Fatal(err)
	}
	tools, largest, largestKind := 0, 0, domain.EventKind("")
	for position := range header.Pages {
		entries, err := storage.DocRangePage(ctx, repo, header, position)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.Kind == domain.EventToolCall || entry.Kind == domain.EventToolResult {
				tools++
			}
			if entry.Length > largest {
				largest, largestKind = entry.Length, entry.Kind
			}
		}
	}
	t.Logf("tools=%d largest_event_bytes=%d largest_event_kind=%s", tools, largest, largestKind)
	reader := &docViewCountStore{FSStore: storage}
	service := NewService(storage, reader, nil, nil, nil)
	for _, mode := range []string{"chat", "prompts", "all"} {
		for _, start := range []int{0, max(0, int(manifest.EventCount)/2), max(0, int(manifest.EventCount)-512)} {
			offset := start
			for batch := 0; batch < 3 && offset >= 0; batch++ {
				reader.chunkReads, reader.fullReads, reader.pageReads = 0, 0, 0
				started := time.Now()
				page, err := service.ReadDocViewEvents(ctx, repo, hash, "", offset, 20, mode)
				elapsed := time.Since(started)
				if err != nil {
					t.Fatal(mode, offset, err)
				}
				inline, visible := 0, 0
				for _, event := range page.Events {
					if !event.Deferred {
						inline++
					}
					if mode == "all" || event.Event.Kind == domain.EventMessage && (mode == "chat" || event.Event.Role == domain.RoleUser && !event.Event.CompactSummary) {
						visible++
					}
				}
				encoded, err := json.Marshal(page)
				if err != nil || len(encoded) > 1<<20 || visible > 20 || reader.fullReads != 0 || reader.chunkReads > 2*inline || reader.pageReads > 5 || len(page.Events) > docViewScanEvents || page.Next >= 0 && page.Next <= offset {
					t.Fatalf("page bounds: mode=%s offset=%d visible=%d inline=%d chunks=%d indexes=%d bytes=%d err=%v", mode, offset, visible, inline, reader.chunkReads, reader.pageReads, len(encoded), err)
				}
				t.Logf("mode=%s offset=%d events=%d visible=%d bytes=%d chunks=%d index_pages=%d elapsed=%s", mode, offset, len(page.Events), visible, len(encoded), reader.chunkReads, reader.pageReads, elapsed)
				offset = page.Next
			}
		}
	}
	for path, before := range inputs {
		if domain.HashContent(read(path, 2*domain.ConversationManifestChunkBytes)) != before {
			t.Fatal("source bytes changed")
		}
	}
}
