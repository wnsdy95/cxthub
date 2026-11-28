package backendclient

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func TestOptionalAssetNoContentMapsToNotFound(t *testing.T) {
	repo := string(domain.HashContent([]byte("repo")))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	c := NewBackendClient(func() string { return ts.URL }, func() string { return "" })
	if _, err := c.PullSettings(context.Background(), repo, "claude"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("PullSettings 204 error = %v, want ErrNotFound", err)
	}
	if _, err := c.PullSecrets(context.Background(), repo); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("PullSecrets 204 error = %v, want ErrNotFound", err)
	}
}

func TestChunkUploadBatchesEnforceRawAndCountBounds(t *testing.T) {
	countChunks := make([]chunkObjWire, maxChunkWireObjects+1)
	for i := range countChunks {
		body := []byte{byte(i + 1)}
		countChunks[i] = chunkObjWire{Hash: domain.HashContent(body), Data: body}
	}
	batches, err := chunkUploadBatches(countChunks)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 || len(batches[0]) != maxChunkWireObjects || len(batches[1]) != 1 {
		t.Fatalf("count batches = %v", []int{len(batches[0]), len(batches[1])})
	}

	rawChunks := make([]chunkObjWire, 4)
	for i := range rawChunks {
		body := bytes.Repeat([]byte{byte(i + 1)}, 1<<20)
		rawChunks[i] = chunkObjWire{Hash: domain.HashContent(body), Data: body}
	}
	batches, err = chunkUploadBatches(rawChunks)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 || len(batches[0]) != 2 || len(batches[1]) != 2 {
		t.Fatalf("raw batches sizes = %d/%d", len(batches[0]), len(batches[1]))
	}

	oversized := bytes.Repeat([]byte{'x'}, maxChunkWireRawBytes+1)
	if _, err := chunkUploadBatches([]chunkObjWire{{Hash: domain.HashContent(oversized), Data: oversized}}); err == nil {
		t.Fatal("oversized single chunk was accepted")
	}
}
