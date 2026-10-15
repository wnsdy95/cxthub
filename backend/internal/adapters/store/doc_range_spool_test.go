package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
