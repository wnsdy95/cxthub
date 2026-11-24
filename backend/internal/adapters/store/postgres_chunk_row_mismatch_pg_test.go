//go:build postgres

package store

import (
	"bytes"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// A part of a conversation is stored under the hash of its bytes, once for
// every repository. A row already under that hash that holds neither those
// bytes nor a list of parts that add up to them is not trusted and not
// replaced: the upload is refused and the repository is not given the part.
func TestPGChunkUploadRefusesARowThatIsNotTheChunk(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := rootPGRepo(t, ctx, storage)
	for name, stored := range map[string][]byte{
		"other bytes": []byte("synthetic: not what the hash names"),
		// What an earlier version of the server stored for the summary of a capture.
		"a list of parts of a capture summary": []byte(`{"format":"cxt-memory-chunks-v3","snapshot_id":"","key_facts":[],"open_tasks":[],"provider":"claude"}`),
		"a list of parts with nothing in it":   []byte(`{"format":"cxt-doc-chunks-v2","envelope":{},"chunks":[]}`),
	} {
		body := []byte(`{"kind":"message","seq":0,"synthetic":"` + name + `"}`)
		hash := domain.HashContent(body)
		if _, err := storage.pool.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, string(hash), docCompress(stored)); err != nil {
			t.Fatal(name, err)
		}
		written, known, err := storage.PutChunks(ctx, repo, map[domain.ContentHash][]byte{hash: body})
		if !errors.Is(err, domain.ErrIntegrity) || written != 0 || known != 0 {
			t.Errorf("%s: %d written, %d known, %v", name, written, known, err)
		}
		var kept []byte
		if err := storage.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, string(hash)).Scan(&kept); err != nil {
			t.Fatal(name, err)
		}
		if raw, err := docDecompress(kept); err != nil || !bytes.Equal(raw, stored) {
			t.Errorf("%s: the row was replaced: %q %v", name, raw, err)
		}
		var owned int
		if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM repo_blobs WHERE repo_id=$1 AND hash=$2`, string(repo), string(hash)).Scan(&owned); err != nil || owned != 0 {
			t.Errorf("%s: the repository was given the part: %d %v", name, owned, err)
		}
		if _, err := storage.GetChunk(ctx, repo, hash); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s: the part can be read: %v", name, err)
		}
	}

	// The same bytes again are known, not written.
	body := []byte(`{"kind":"message","seq":1}`)
	hash := domain.HashContent(body)
	if written, known, err := storage.PutChunks(ctx, repo, map[domain.ContentHash][]byte{hash: body}); err != nil || written != 1 || known != 0 {
		t.Fatalf("a new part: %d written, %d known, %v", written, known, err)
	}
	if written, known, err := storage.PutChunks(ctx, repo, map[domain.ContentHash][]byte{hash: body}); err != nil || written != 0 || known != 1 {
		t.Fatalf("the same part again: %d written, %d known, %v", written, known, err)
	}
	if got, err := storage.GetChunk(ctx, repo, hash); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("the part read back: %q %v", got, err)
	}
}
