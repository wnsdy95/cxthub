package app

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func copyRootReadLocalFixture(test *testing.T, source, dir string) rootReadLargeFixture {
	test.Helper()
	fixture := copyRootReadLocalObject(test, source, dir, os.Getenv("CXT_TEST_ROOT_READ_TARGET"))
	if base := os.Getenv("CXT_TEST_ROOT_READ_BASE"); base != "" {
		previous := copyRootReadLocalObject(test, source, dir, base)
		fixture.Base, fixture.BaseEvents = previous.Target, previous.Events
	}
	return fixture
}

func copyRootReadLocalObject(test *testing.T, source, dir, target string) rootReadLargeFixture {
	test.Helper()
	hash := domain.ContentHash("sha256:" + strings.TrimPrefix(target, "sha256:"))
	if domain.ValidateContentHash(hash) != nil {
		test.Fatal("invalid local fixture target hash")
	}
	stored, err := os.ReadFile(filepath.Join(source, "docs", strings.TrimPrefix(string(hash), "sha256:")))
	if err != nil {
		test.Fatal(err)
	}
	raw := stored
	if len(raw) >= 4 && bytes.Equal(raw[:4], []byte{0x28, 0xb5, 0x2f, 0xfd}) {
		decoder, err := zstd.NewReader(bytes.NewReader(raw), zstd.WithDecoderConcurrency(1), zstd.WithDecodeBuffersBelow(0), zstd.WithDecoderMaxMemory(8<<20), zstd.WithDecoderMaxWindow(8<<20))
		if err != nil {
			test.Fatal("local fixture decompressor")
		}
		raw, err = io.ReadAll(io.LimitReader(decoder, domain.MaxConversationManifestBytes+1))
		decoder.Close()
		if err != nil {
			test.Fatal("local fixture manifest decompression")
		}
	}
	manifest, err := domain.DecodeConversationManifest(raw)
	if err != nil {
		test.Fatal("local fixture is not a strict root manifest")
	}
	got, err := domain.ConversationManifestHash(manifest)
	if err != nil || got != hash {
		test.Fatal("local fixture root mismatch")
	}
	repo := h('1')
	for _, kind := range []string{"docs", "chunks"} {
		if err := os.MkdirAll(filepath.Dir(rootReadObjectPath(dir, repo, kind, hash)), 0700); err != nil {
			test.Fatal(err)
		}
	}
	if err := os.WriteFile(rootReadObjectPath(dir, repo, "docs", hash), stored, 0600); err != nil {
		test.Fatal(err)
	}
	var storedBytes int64
	seen := map[domain.ContentHash]bool{}
	for _, chunk := range manifest.Chunks {
		if seen[chunk.Hash] {
			continue
		}
		seen[chunk.Hash] = true
		destination := rootReadObjectPath(dir, repo, "chunks", chunk.Hash)
		if _, err := os.Stat(destination); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			test.Fatal(err)
		}
		input, err := os.Open(filepath.Join(source, "chunks", strings.TrimPrefix(string(chunk.Hash), "sha256:")))
		if err != nil {
			test.Fatal(err)
		}
		output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			input.Close()
			test.Fatal(err)
		}
		count, copyErr := io.Copy(output, input)
		inputErr, outputErr := input.Close(), output.Close()
		if copyErr != nil || inputErr != nil || outputErr != nil {
			test.Fatal("local fixture chunk copy failed")
		}
		storedBytes += count
	}
	storage := store.NewFSStore(dir)
	if err := storage.PutSnapshot(systemTestContext(), domain.Snapshot{ID: hash, RepoID: repo, DocHash: hash, DocIdentity: domain.DocumentIdentityRootV1, Branch: "main"}); err != nil {
		test.Fatal(err)
	}
	test.Logf("private fixture hash=%s stream=%d events=%d chunks=%d copied_chunk_bytes=%d", hash, manifest.StreamBytes, manifest.EventCount, len(manifest.Chunks), storedBytes)
	return rootReadLargeFixture{Repo: repo, Target: hash, Bytes: manifest.StreamBytes, Events: int(manifest.EventCount), Chunks: len(manifest.Chunks)}
}

func measureRootReadLocalShape(test *testing.T, storage *store.FSStore, fixture rootReadLargeFixture) {
	test.Helper()
	ctx := systemTestContext()
	doc, err := storage.ReadVerifiedDoc(ctx, fixture.Repo, fixture.Target)
	if err != nil {
		test.Fatal("local fixture verification failed")
	}
	plan, err := doc.PlanReadIndexContext(ctx)
	if err != nil {
		test.Fatal(err)
	}
	index, err := plan.BuildRangesContext(ctx)
	if err != nil || len(index.Events) != fixture.Events {
		test.Fatal("local fixture shape mismatch")
	}
	lengths := make([]int, len(index.Events))
	var overMiB, over16MiB int
	for position, event := range index.Events {
		lengths[position] = event.Length
		if event.Length > 1<<20 {
			overMiB++
		}
		if event.Length > 16<<20 {
			over16MiB++
		}
	}
	slices.Sort(lengths)
	if len(lengths) > 0 {
		test.Logf("hash=%s event_bytes p50=%d p95=%d p99=%d max=%d over_1MiB=%d over_16MiB=%d", fixture.Target, lengths[len(lengths)/2], lengths[len(lengths)*95/100], lengths[len(lengths)*99/100], lengths[len(lengths)-1], overMiB, over16MiB)
	}
}
