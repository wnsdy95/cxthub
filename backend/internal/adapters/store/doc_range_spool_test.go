package store

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func rangeSpoolTempDir(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	t.Setenv("TMPDIR", directory)
	t.Setenv("TMP", directory)
	t.Setenv("TEMP", directory)
	return directory
}

func assertRangeSpoolsRemoved(t *testing.T, directory string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(directory, "cxthub-doc-range-*"))
	if err != nil || len(matches) != 0 {
		t.Fatal("range spool leaked", matches, err)
	}
}

func TestDocRangeSpoolBoundedReadsAndCleanup(t *testing.T) {
	directory := rangeSpoolTempDir(t)
	spool, err := newDocRangeSpool()
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	info, err := spool.file.Stat()
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("spool permissions", info, err)
	}
	ctx := context.Background()
	for _, raw := range [][]byte{[]byte("first"), []byte("second")} {
		if err := spool.Append(ctx, raw); err != nil {
			t.Fatal(err)
		}
		raw[0] = 'x'
	}
	first, err := spool.Read(ctx, 0)
	if err != nil || string(first) != "first" {
		t.Fatal(string(first), err)
	}
	second, err := spool.Read(ctx, 1)
	if err != nil || string(second) != "second" || string(first) != "first" {
		t.Fatal("read reused a caller buffer", err)
	}
	if err := spool.Append(ctx, make([]byte, domain.MaxDocRangePageBytes+1)); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("oversized page", err)
	}
	if _, err := spool.Read(ctx, 2); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("out of bounds", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := spool.Append(canceled, []byte("canceled")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := spool.Read(canceled, 0); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := spool.Close(); err != nil {
		t.Fatal(err)
	}
	if err := spool.Close(); err != nil {
		t.Fatal("idempotent close", err)
	}
	assertRangeSpoolsRemoved(t, directory)
	if err := spool.Append(ctx, []byte("closed")); err == nil {
		t.Fatal("closed spool accepted a write")
	}
	if _, err := spool.Read(ctx, 0); err == nil {
		t.Fatal("closed spool accepted a read")
	}
}

type docRangeSpoolTestWriter struct {
	writer io.Writer
	calls  int
	short  bool
}

func (writer *docRangeSpoolTestWriter) Write(raw []byte) (int, error) {
	writer.calls++
	if writer.short {
		return len(raw) - 1, nil
	}
	return writer.writer.Write(raw)
}

func TestDocRangeSpoolBuffersBoundedWritesAndInterleavedReads(t *testing.T) {
	directory := rangeSpoolTempDir(t)
	spool, err := newDocRangeSpool()
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	writer := &docRangeSpoolTestWriter{writer: spool.file}
	spool.writer = bufio.NewWriterSize(writer, domain.MaxDocRangePageBytes)
	ctx := context.Background()
	pages := [][]byte{bytes.Repeat([]byte("a"), 100), bytes.Repeat([]byte("b"), 200)}
	for _, raw := range pages {
		if err := spool.Append(ctx, raw); err != nil {
			t.Fatal(err)
		}
	}
	if writer.calls != 0 || spool.writer.Buffered() != 300 || spool.writer.Size() != domain.MaxDocRangePageBytes {
		t.Fatal("small pages were not coalesced within the bounded buffer")
	}
	if _, err := spool.Read(ctx, len(pages)); !errors.Is(err, domain.ErrIntegrity) || writer.calls != 0 {
		t.Fatal("invalid read flushed staged bytes", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := spool.Flush(canceled); !errors.Is(err, context.Canceled) || writer.calls != 0 {
		t.Fatal("canceled flush wrote staged bytes", err)
	}
	first, err := spool.Read(ctx, 0)
	if err != nil || !bytes.Equal(first, pages[0]) || writer.calls != 1 {
		t.Fatal("read did not flush exactly once", err)
	}
	pages = append(pages, bytes.Repeat([]byte("c"), domain.MaxDocRangePageBytes), []byte("tail"))
	for _, raw := range pages[2:] {
		if err := spool.Append(ctx, raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := spool.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	for position, want := range pages {
		got, err := spool.Read(ctx, position)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("append after read changed page", position, err)
		}
	}
	if writer.calls != 3 || spool.writer.Buffered() != 0 {
		t.Fatal("unexpected write batching", writer.calls)
	}
	if _, err := spool.file.WriteAt([]byte("z"), 0); err != nil {
		t.Fatal(err)
	}
	changed, err := spool.Read(ctx, 0)
	if err != nil || changed[0] != 'z' || first[0] != 'a' {
		t.Fatal("read masked current bytes or aliased earlier result", err)
	}
	if err := spool.Append(ctx, []byte("discarded")); err != nil {
		t.Fatal(err)
	}
	if err := spool.Close(); err != nil || writer.calls != 3 {
		t.Fatal("close wrote unused staged bytes", err)
	}
	if err := spool.Flush(ctx); !errors.Is(err, os.ErrClosed) {
		t.Fatal("closed spool accepted flush", err)
	}
	assertRangeSpoolsRemoved(t, directory)
}

func TestDocRangeSpoolFlushFailureRemainsFailed(t *testing.T) {
	directory := rangeSpoolTempDir(t)
	spool, err := newDocRangeSpool()
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	writer := &docRangeSpoolTestWriter{short: true}
	spool.writer = bufio.NewWriterSize(writer, domain.MaxDocRangePageBytes)
	ctx := context.Background()
	if err := spool.Append(ctx, []byte("staged")); err != nil {
		t.Fatal(err)
	}
	if err := spool.Flush(ctx); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("flush hid short write", err)
	}
	if err := spool.Append(ctx, []byte("later")); !errors.Is(err, io.ErrShortWrite) || len(spool.offsets) != 2 {
		t.Fatal("failed spool accepted another page", err)
	}
	if _, err := spool.Read(ctx, 0); !errors.Is(err, io.ErrShortWrite) || writer.calls != 1 {
		t.Fatal("failed spool returned partially flushed bytes", err)
	}
	if err := spool.Close(); err != nil {
		t.Fatal(err)
	}
	assertRangeSpoolsRemoved(t, directory)
}

func manyShortRangeFixture(t *testing.T) rootReaderFixture {
	t.Helper()
	fixture := rootFixture(t, "")
	const count = 210000
	var stream bytes.Buffer
	for position := 0; position < count; position++ {
		if position > 0 {
			stream.WriteByte(',')
		}
		fmt.Fprintf(&stream, `{"kind":"turn","role":"user","seq":%d}`, position)
	}
	chunks := []domain.ConversationManifestChunk{}
	fixture.bodies = map[domain.ContentHash][]byte{}
	raw := stream.Bytes()
	for offset := 0; offset < len(raw); offset += domain.ConversationManifestChunkBytes {
		body := raw[offset:min(offset+domain.ConversationManifestChunkBytes, len(raw))]
		hash := domain.HashContent(body)
		chunks = append(chunks, domain.ConversationManifestChunk{Hash: hash, Bytes: int64(len(body))})
		fixture.bodies[hash] = body
	}
	var err error
	fixture.manifest, err = domain.NewConversationManifest(fixture.manifest.Envelope, chunks, count)
	if err != nil {
		t.Fatal(err)
	}
	fixture.hash, err = domain.ConversationManifestHash(fixture.manifest)
	if err != nil {
		t.Fatal(err)
	}
	fixture.manifestBytes, err = domain.CanonicalConversationManifest(fixture.manifest)
	if err != nil {
		t.Fatal(err)
	}
	fixture.canonical = append([]byte(`{"envelope":`), fixture.manifest.Envelope...)
	fixture.canonical = append(fixture.canonical, []byte(`,"events":[`)...)
	fixture.canonical = append(fixture.canonical, raw...)
	fixture.canonical = append(fixture.canonical, ']', '}')
	return fixture
}

func TestDocRangeSpoolManyShortEventsBeyond32MiB(t *testing.T) {
	directory := rangeSpoolTempDir(t)
	fixture := manyShortRangeFixture(t)
	ctx := context.Background()
	lazy, err := domain.BuildDocRangeIndex(ctx, rootPublicationProof(t, fixture))
	if err != nil {
		t.Fatal(err)
	}
	spool, err := newDocRangeSpool()
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	var verifier domain.CanonicalDocVerifier
	streamed, err := verifier.VerifyConversationRangeIndex(ctx, fixture.hash, fixture.manifest, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
		return fixture.bodies[hash], nil
	}, spool)
	if err != nil {
		t.Fatal(err)
	}
	info, err := spool.file.Stat()
	if err != nil || info.Size() <= domain.MaxDocRangeIndexBytes {
		t.Fatal("fixture did not exceed old aggregate cap", info, err)
	}
	header := streamed.Header()
	if err := matchRangeHeader(header, lazy.Header()); err != nil || len(spool.offsets) != len(header.Pages)+1 {
		t.Fatal("lazy and streaming headers differ", err)
	}
	for position := range header.Pages {
		left, err := lazy.Page(ctx, position)
		if err != nil {
			t.Fatal(err)
		}
		right, err := streamed.Page(ctx, position)
		if err != nil || !bytes.Equal(left, right) {
			t.Fatal("lazy and streaming pages differ", position, err)
		}
	}
	if _, err := spool.file.WriteAt([]byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := streamed.Page(ctx, 0); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("spool corruption escaped proof checks", err)
	}
	if err := spool.Close(); err != nil {
		t.Fatal(err)
	}
	assertRangeSpoolsRemoved(t, directory)
	t.Logf("validated %d events, %d source bytes, %d page bytes in %d bounded pages", fixture.manifest.EventCount, fixture.manifest.StreamBytes, info.Size(), len(header.Pages))
}
