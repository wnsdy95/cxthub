package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func legacyCompatibilityIndexPath(directory string, repo, hash domain.ContentHash) string {
	return filepath.Join(directory, "repos", strings.TrimPrefix(string(repo), "sha256:"), "read-index-v2", strings.TrimPrefix(string(hash), "sha256:"))
}

func legacyCompatibilityWrite(test *testing.T, path string, raw []byte) {
	test.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		test.Fatal(err)
	}
}

func legacyCompatibilityRemoveIndex(test *testing.T, directory string, repo, hash domain.ContentHash) {
	test.Helper()
	for _, suffix := range []string{"", ".search", ".filter"} {
		if err := os.Remove(legacyCompatibilityIndexPath(directory, repo, hash) + suffix); err != nil && !os.IsNotExist(err) {
			test.Fatal(err)
		}
	}
}

func legacyCompatibilityMonolithic(test *testing.T, directory string, repo domain.ContentHash, doc domain.SessionDoc) {
	test.Helper()
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		test.Fatal(err)
	}
	legacyCompatibilityWrite(test, rootReadObjectPath(directory, repo, "docs", doc.Hash), raw)
}

type legacyCompatibilityFileState struct {
	Body     []byte
	Mode     fs.FileMode
	Modified time.Time
}

func legacyCompatibilityFiles(test *testing.T, directory string) map[string]legacyCompatibilityFileState {
	test.Helper()
	files := map[string]legacyCompatibilityFileState{}
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, failure error) error {
		if failure != nil {
			return failure
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		state := legacyCompatibilityFileState{Mode: info.Mode(), Modified: info.ModTime()}
		if !entry.IsDir() {
			state.Body, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		files[path] = state
		return nil
	})
	if err != nil {
		test.Fatal(err)
	}
	return files
}

func TestDocFindLegacyCompatibilityColdAndMonolithicReadsDoNotWrite(test *testing.T) {
	for _, representation := range []string{"missing_index_chunked", "missing_index_monolithic", "indexed_monolithic"} {
		test.Run(representation, func(test *testing.T) {
			ctx, repo, directory := systemTestContext(), h('1'), test.TempDir()
			storage := store.NewFSStore(directory)
			doc := seedLegacyFindDocument(test, storage, repo, []domain.CIREvent{
				historyMessage(domain.RoleUser, "first needle"),
				{Kind: domain.EventReasoning, RedactedSummary: " \t", Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted_content", Blob: "opaque-only needle"}},
				{Kind: domain.EventToolResult, Output: "second needle"},
			})
			if representation != "indexed_monolithic" {
				legacyCompatibilityRemoveIndex(test, directory, repo, doc.Hash)
			}
			if representation != "missing_index_chunked" {
				legacyCompatibilityMonolithic(test, directory, repo, doc)
				if err := os.RemoveAll(filepath.Join(directory, "repos", strings.TrimPrefix(string(repo), "sha256:"), "objects", "chunks")); err != nil {
					test.Fatal(err)
				}
			}
			before := legacyCompatibilityFiles(test, directory)
			for _, readOnly := range []bool{false, true} {
				read := ctx
				if readOnly {
					read = outbound.WithDocReadOnly(read)
				}
				spy := &countedReadStore{FSStore: store.NewFSStore(directory)}
				service := NewService(spy.FSStore, spy, nil, nil, nil)
				page, err := service.FindDocPage(read, repo, doc.Hash, domain.DocFindPageRequest{Legacy: true, Query: "needle", To: 3, Total: 3})
				if err != nil || page.Hash != doc.Hash || page.Projection != domain.DocLegacyFindProjection || page.Generation != "" || page.NextCursor != nil || page.Index != 3 || len(page.Matches) != 2 {
					test.Fatalf("readonly=%t: legacy source unavailable or changed: %+v, %v", readOnly, page, err)
				}
				if page.Matches[0].Index != 0 || page.Matches[1].Index != 2 || page.Matches[0].RevealCursor != "" || page.Matches[1].RevealCursor != "" || spy.whole != 1 {
					test.Fatalf("readonly=%t: wrong public matches or fallback reads: %+v, whole=%d", readOnly, page.Matches, spy.whole)
				}
				if after := legacyCompatibilityFiles(test, directory); !reflect.DeepEqual(after, before) {
					test.Fatalf("readonly=%t: find created an index, repacked, or rewrote stored bytes", readOnly)
				}
			}
		})
	}
}

func TestDocFindLegacyCompatibilityMissingBaseIndexDoesNotWrite(test *testing.T) {
	for _, monolithic := range []bool{false, true} {
		for _, readOnly := range []bool{true, false} {
			name := "chunked"
			if monolithic {
				name = "monolithic"
			}
			if readOnly {
				name += "/readonly"
			} else {
				name += "/default"
			}
			test.Run(name, func(test *testing.T) {
				ctx, repo, directory := systemTestContext(), h('1'), test.TempDir()
				storage := store.NewFSStore(directory)
				shared := historyMessage(domain.RoleUser, "shared needle")
				base := seedLegacyFindDocument(test, storage, repo, []domain.CIREvent{shared})
				doc := seedLegacyFindDocument(test, storage, repo, []domain.CIREvent{shared, historyMessage(domain.RoleAssistant, "new needle")})
				for _, source := range []domain.SessionDoc{base, doc} {
					legacyCompatibilityRemoveIndex(test, directory, repo, source.Hash)
					if monolithic {
						legacyCompatibilityMonolithic(test, directory, repo, source)
					}
				}
				before := legacyCompatibilityFiles(test, directory)
				if readOnly {
					ctx = outbound.WithDocReadOnly(ctx)
				}
				service := NewService(storage, storage, nil, nil, nil)
				page, err := service.FindDocPage(ctx, repo, doc.Hash, domain.DocFindPageRequest{Legacy: true, Query: "needle", Base: base.Hash, From: 1, To: 2, Total: 2, Inherited: 1})
				if err != nil || page.Hash != doc.Hash || page.NextCursor != nil || len(page.Matches) != 1 || page.Matches[0].Index != 1 {
					test.Errorf("missing base index prevented exact inherited-prefix search: %+v, %v", page, err)
				}
				if after := legacyCompatibilityFiles(test, directory); !reflect.DeepEqual(after, before) {
					test.Error("missing base index caused persistent writes or repacking")
				}
			})
		}
	}
}

func TestDocFindLegacyCompatibilityOwnershipAndCorruptionFailClosed(test *testing.T) {
	for _, scenario := range []string{"foreign_indexed", "foreign_monolithic", "removed_owner", "corrupt_chunk", "corrupt_chunk_without_index", "corrupt_index_hash", "corrupt_monolithic"} {
		test.Run(scenario, func(test *testing.T) {
			ctx, repo, directory := systemTestContext(), h('1'), test.TempDir()
			storage := store.NewFSStore(directory)
			doc := seedLegacyFindDocument(test, storage, repo, []domain.CIREvent{historyMessage(domain.RoleUser, "needle")})
			manifest, err := storage.GetDocManifest(ctx, repo, doc.Hash)
			if err != nil || len(manifest.Chunks) != 1 {
				test.Fatalf("expected a single-chunk fixture: %+v, %v", manifest, err)
			}
			if strings.Contains(scenario, "monolithic") {
				legacyCompatibilityMonolithic(test, directory, repo, doc)
				legacyCompatibilityRemoveIndex(test, directory, repo, doc.Hash)
			}
			if scenario == "corrupt_chunk_without_index" {
				legacyCompatibilityRemoveIndex(test, directory, repo, doc.Hash)
			}
			service := NewService(storage, storage, nil, nil, nil)
			request := domain.DocFindPageRequest{Legacy: true, Query: "needle", To: 1, Total: 1}
			if page, err := service.FindDocPage(ctx, repo, doc.Hash, request); err != nil || len(page.Matches) != 1 {
				test.Fatalf("valid source failed before mutation: %+v, %v", page, err)
			}
			expected := domain.ErrIntegrity
			switch scenario {
			case "foreign_indexed", "foreign_monolithic":
				foreign := h('2')
				for _, pair := range [][2]string{
					{rootReadObjectPath(directory, repo, "chunks", manifest.Chunks[0]), rootReadObjectPath(directory, foreign, "chunks", manifest.Chunks[0])},
					{legacyCompatibilityIndexPath(directory, repo, doc.Hash), legacyCompatibilityIndexPath(directory, foreign, doc.Hash)},
				} {
					raw, err := os.ReadFile(pair[0])
					if os.IsNotExist(err) {
						continue
					}
					if err != nil {
						test.Fatal(err)
					}
					legacyCompatibilityWrite(test, pair[1], raw)
				}
				repo, expected = foreign, domain.ErrNotFound
			case "removed_owner":
				if err := os.Remove(rootReadObjectPath(directory, repo, "docs", doc.Hash)); err != nil {
					test.Fatal(err)
				}
				expected = domain.ErrNotFound
			case "corrupt_chunk", "corrupt_chunk_without_index":
				legacyCompatibilityWrite(test, rootReadObjectPath(directory, repo, "chunks", manifest.Chunks[0]), []byte(`{"kind":"message","seq":0,"role":"user","blocks":[{"type":"text","text":"needle tampered"}]}`))
			case "corrupt_index_hash":
				index, err := domain.BuildDocReadIndex(doc)
				if err != nil {
					test.Fatal(err)
				}
				index.Events[0].Hash = domain.HashContent([]byte("tampered event"))
				raw, err := json.Marshal(index)
				if err != nil {
					test.Fatal(err)
				}
				legacyCompatibilityWrite(test, legacyCompatibilityIndexPath(directory, repo, doc.Hash), raw)
			case "corrupt_monolithic":
				doc.CIR.Events[0].Blocks[0].Text = "tampered needle"
				legacyCompatibilityMonolithic(test, directory, repo, doc)
			}
			before := legacyCompatibilityFiles(test, directory)
			page, err := service.FindDocPage(outbound.WithDocReadOnly(ctx), repo, doc.Hash, request)
			if !errors.Is(err, expected) || !reflect.DeepEqual(page, domain.DocFindPage{}) {
				test.Fatalf("unowned or corrupt source returned results: %+v, %v; want %v", page, err, expected)
			}
			if after := legacyCompatibilityFiles(test, directory); !reflect.DeepEqual(after, before) {
				test.Fatal("failed read mutated persistent storage")
			}
		})
	}
}

func TestDocFindLegacyCompatibilityRejectsUnverifiedLockedSources(test *testing.T) {
	for _, event := range []string{
		`{"kind":"tool_result","seq":0,"output":"visible","locked":{"provider":"codex","scheme":"encrypted_content","blob":"opaque-only needle"}}`,
		`{"kind":"message","seq":0,"role":"assistant","blocks":[{"type":"text","text":"visible"}],"locked":{"provider":"codex","scheme":"encrypted_content","blob":"opaque-only needle"}}`,
	} {
		var decoded domain.CIREvent
		if err := json.Unmarshal([]byte(event), &decoded); err != nil {
			test.Fatal(err)
		}
		test.Run(string(decoded.Kind), func(test *testing.T) {
			ctx, repo, directory := systemTestContext(), h('1'), test.TempDir()
			raw := []byte(`{"envelope":{"cir_version":"2","source_provider":"codex"},"events":[` + event + `]}`)
			hash := domain.HashContent(raw)
			legacyCompatibilityWrite(test, rootReadObjectPath(directory, repo, "docs", hash), raw)
			storage := store.NewFSStore(directory)
			before := legacyCompatibilityFiles(test, directory)
			page, err := NewService(storage, storage, nil, nil, nil).FindDocPage(outbound.WithDocReadOnly(ctx), repo, hash, domain.DocFindPageRequest{Legacy: true, Query: "needle", To: 1, Total: 1})
			if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(page, domain.DocFindPage{}) {
				test.Fatalf("unverified union exposed opaque provider state: %+v, %v", page, err)
			}
			if after := legacyCompatibilityFiles(test, directory); !reflect.DeepEqual(after, before) {
				test.Fatal("rejected union changed persistent storage")
			}
		})
	}
}

func TestDocFindLegacyCompatibilityRootCannotDowngrade(test *testing.T) {
	for _, mode := range []string{"root_snapshot", "missing_snapshot", "legacy_snapshot"} {
		test.Run(mode, func(test *testing.T) {
			ctx, repo, directory := systemTestContext(), h('1'), test.TempDir()
			storage := store.NewFSStore(directory)
			var identity *domain.DocumentIdentity
			if mode == "root_snapshot" {
				root := domain.DocumentIdentityRootV1
				identity = &root
			} else if mode == "legacy_snapshot" {
				legacy := domain.DocumentIdentityLegacy
				identity = &legacy
			}
			doc, _ := seedRootReadDocWithIdentity(test, directory, storage, repo, identity, historyMessage(domain.RoleUser, "needle"))
			raw, err := domain.CanonicalBytes(doc.CIR)
			if err != nil {
				test.Fatal(err)
			}
			index, err := domain.BuildDocReadIndex(domain.SessionDoc{Hash: domain.HashContent(raw), CIR: doc.CIR})
			if err != nil {
				test.Fatal(err)
			}
			index.Hash = doc.Hash
			encoded, err := json.Marshal(index)
			if err != nil {
				test.Fatal(err)
			}
			legacyCompatibilityWrite(test, legacyCompatibilityIndexPath(directory, repo, doc.Hash), encoded)
			before := legacyCompatibilityFiles(test, directory)
			spy := &countedReadStore{FSStore: storage}
			service := NewService(storage, spy, nil, nil, nil)
			expected := domain.ErrUnsupportedDocumentIdentity
			if mode == "root_snapshot" {
				expected = domain.ErrDocVisibleProjectionUnsupported
			}
			for _, query := range []string{"needle", "absent"} {
				page, err := service.FindDocPage(ctx, repo, doc.Hash, domain.DocFindPageRequest{Legacy: true, Query: query, To: 1, Total: 1})
				if !errors.Is(err, expected) || !reflect.DeepEqual(page, domain.DocFindPage{}) || spy.whole != 0 || spy.chunks != 0 {
					test.Fatalf("root downgraded through a legacy index: %+v, %v, whole=%d chunks=%d", page, err, spy.whole, spy.chunks)
				}
			}
			if after := legacyCompatibilityFiles(test, directory); !reflect.DeepEqual(after, before) {
				test.Fatal("root rejection rewrote persistent storage")
			}
		})
	}
}

type legacyCompatibilityChunkStore struct {
	*store.FSStore
	chunkReads int
	failHash   domain.ContentHash
	failure    error
}

func (storage *legacyCompatibilityChunkStore) GetChunk(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
	storage.chunkReads++
	if hash == storage.failHash && storage.failure != nil {
		return nil, storage.failure
	}
	raw, err := storage.FSStore.GetChunk(ctx, repo, hash)
	if err == nil && hash == storage.failHash {
		raw = bytes.Clone(raw)
		raw[0] ^= 1
	}
	return raw, err
}

func TestLegacyEventRangeReaderCompatibilitySingleChunkCache(test *testing.T) {
	ctx, repo := systemTestContext(), h('1')
	storage := store.NewFSStore(test.TempDir())
	doc := seedLegacyFindDocument(test, storage, repo, []domain.CIREvent{historyMessage(domain.RoleUser, "first needle"), historyMessage(domain.RoleAssistant, "second needle")})
	manifest, err := storage.GetDocManifest(ctx, repo, doc.Hash)
	if err != nil || len(manifest.Chunks) != 1 {
		test.Fatalf("expected single chunk: %+v, %v", manifest, err)
	}
	body, err := storage.GetChunk(ctx, repo, manifest.Chunks[0])
	if err != nil {
		test.Fatal(err)
	}
	spy := &countedReadStore{FSStore: storage}
	reader, err := NewService(storage, spy, nil, nil, nil).eventRangeReader(outbound.WithDocReadOnly(ctx), repo, doc.Hash)
	if err != nil || spy.whole != 0 || spy.chunks != 0 {
		test.Fatalf("reader eagerly loaded bytes: %v, whole=%d chunks=%d", err, spy.whole, spy.chunks)
	}
	for _, span := range [][2]int{{0, 17}, {len(body) - 23, 23}, {7, 31}, {0, 17}} {
		actual, err := reader(span[0], span[1])
		if err != nil || !bytes.Equal(actual, body[span[0]:span[0]+span[1]]) || spy.whole != 0 || spy.chunks != 1 {
			test.Fatalf("single-chunk read lost bytes or cache reuse: %v, whole=%d chunks=%d", err, spy.whole, spy.chunks)
		}
		actual[0] ^= 1
	}
}

func TestLegacyEventRangeReaderCompatibilityCacheSurvivesFailedChunkAndBacktracks(test *testing.T) {
	for _, mode := range []string{"missing_chunk", "corrupt_chunk", "backtrack"} {
		test.Run(mode, func(test *testing.T) {
			ctx, repo := systemTestContext(), h('1')
			storage := store.NewFSStore(test.TempDir())
			doc := seedLegacyFindDocument(test, storage, repo, []domain.CIREvent{{Kind: domain.EventToolResult, Output: strings.Repeat("range reader payload ", domain.ChunkTarget/10)}})
			manifest, err := storage.GetDocManifest(ctx, repo, doc.Hash)
			if err != nil || len(manifest.Chunks) < 2 {
				test.Fatalf("expected multiple chunks: %+v, %v", manifest, err)
			}
			first, err := storage.GetChunk(ctx, repo, manifest.Chunks[0])
			if err != nil {
				test.Fatal(err)
			}
			second, err := storage.GetChunk(ctx, repo, manifest.Chunks[1])
			if err != nil {
				test.Fatal(err)
			}
			spy := &legacyCompatibilityChunkStore{FSStore: storage}
			if mode != "backtrack" {
				spy.failHash = manifest.Chunks[1]
			}
			if mode == "missing_chunk" {
				spy.failure = domain.ErrNotFound
			}
			reader, err := NewService(storage, spy, nil, nil, nil).eventRangeReader(ctx, repo, doc.Hash)
			if err != nil {
				test.Fatal(err)
			}
			actual, err := reader(0, 17)
			if err != nil || !bytes.Equal(actual, first[:17]) {
				test.Fatalf("first chunk read failed: %v", err)
			}
			actual, err = reader(domain.ChunkTarget, 17)
			if mode == "backtrack" {
				if err != nil || !bytes.Equal(actual, second[:17]) {
					test.Fatalf("second chunk read failed: %v", err)
				}
			} else {
				expected := domain.ErrIntegrity
				if spy.failure != nil {
					expected = spy.failure
				}
				if !errors.Is(err, expected) || actual != nil {
					test.Fatalf("failed chunk returned bytes: %q, %v", actual, err)
				}
			}
			actual, err = reader(0, 17)
			expectedReads := 2
			if mode == "backtrack" {
				expectedReads = 3
			}
			if err != nil || !bytes.Equal(actual, first[:17]) || spy.chunkReads != expectedReads {
				test.Fatalf("cache changed after second chunk: %q, %v, reads=%d want=%d", actual, err, spy.chunkReads, expectedReads)
			}
		})
	}
}
