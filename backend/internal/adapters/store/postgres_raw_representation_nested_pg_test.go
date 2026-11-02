//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGRawRepresentationNestedMemoryHierarchy(test *testing.T) {
	fixture, storage := authorityScopePG(test)
	ctx := rootPublicationContext(fixture.ctx)
	document, canonical := canonicalDocumentBytesPG(test, domain.MemoryChunkTarget)
	docRepo, _ := rootPGRepo(test, ctx, storage)
	memoryRepo, _ := rootPGRepo(test, ctx, storage)
	rawRepo, _ := rootPGRepo(test, ctx, storage)
	if _, err := storage.PutDoc(ctx, docRepo, document); err != nil {
		test.Fatal(err)
	}
	digest := domain.MemoryDigest{SnapshotID: domain.HashContent([]byte("nested-memory-position")), Summary: string(canonical) + "x"}
	plan, chunked, err := domain.PlanMemoryChunks(digest)
	if err != nil || !chunked || len(plan.Manifest.SummaryChunks) != 2 || plan.Manifest.SummaryChunks[0] != document.Hash {
		test.Fatal("the valid memory's first actual component must be the canonical manifested document", err)
	}
	memory, err := domain.MemoryDigestHash(digest)
	if err != nil {
		test.Fatal(err)
	}
	raw, err := json.Marshal(digest)
	if err != nil || domain.HashContent(raw) != memory {
		test.Fatal(err)
	}
	if _, _, err := storage.PutChunks(ctx, memoryRepo, map[domain.ContentHash][]byte{memory: raw}); err != nil {
		test.Fatal("seed a legitimate raw memory parent without promoting its manifested child", err)
	}
	rawBarrier := &rawRepresentationHierarchyBarrierPG{retry: true, ready: make(chan struct{}), release: make(chan struct{}, 1)}
	memoryBarrier := &rawRepresentationHierarchyBarrierPG{document: memory, ready: make(chan struct{}), release: make(chan struct{}, 1)}
	defer close(rawBarrier.release)
	defer close(memoryBarrier.release)
	rawWriter := authorityScopePeerPG(test, storage, "", rawBarrier)
	memoryWriter := authorityScopePeerPG(test, storage, "", memoryBarrier)
	rawPID := authorityScopePIDPG(test, ctx, rawWriter)
	memoryPID := authorityScopePIDPG(test, ctx, memoryWriter)
	if rawPID == memoryPID || rawPID == fixture.observer.PgConn().PID() || memoryPID == fixture.observer.PgConn().PID() {
		test.Fatal("the nested hierarchy requires two actual producer backends and an independent observer")
	}
	staged, commit := make(chan struct{}), make(chan struct{}, 1)
	defer close(commit)
	bodies := map[domain.ContentHash][]byte{document.Hash: canonical, memory: raw}
	rawContext, _, rawDone := authorityScopeRunPG(test, ctx, func(work context.Context) error {
		return rawWriter.WithinRepository(work, rawRepo, func(bound context.Context) error {
			stored, deduped, err := rawWriter.PutChunks(bound, rawRepo, bodies)
			if err != nil || stored != 2 || deduped != 0 {
				return fmt.Errorf("nested raw batch counts=%d/%d: %w", stored, deduped, err)
			}
			close(staged)
			select {
			case <-commit:
				return nil
			case <-bound.Done():
				return bound.Err()
			}
		})
	})
	authorityScopeReadyPG(test, rawContext, rawBarrier.ready, rawDone)
	memoryContext, _, memoryDone := authorityScopeRunPG(test, ctx, func(work context.Context) error {
		actual, err := memoryWriter.PutMemory(work, memoryRepo, digest)
		if err != nil || actual != memory {
			return fmt.Errorf("nested memory hash=%s: %w", actual, err)
		}
		return nil
	})
	authorityScopeReadyPG(test, memoryContext, memoryBarrier.ready, memoryDone)
	rawBarrier.release <- struct{}{}
	authorityScopeWaitPG(test, rawContext, fixture.observer, memoryPID, rawPID, "pg_advisory_xact_lock", rawDone)
	memoryBarrier.release <- struct{}{}
	authorityScopeReadyPG(test, rawContext, staged, rawDone)
	commit <- struct{}{}
	authorityScopeDonePG(test, rawContext, rawDone)
	authorityScopeDonePG(test, memoryContext, memoryDone)
	if rawBarrier.attempts != 2 {
		test.Fatal("the nested raw batch must use only discovery and one retry", rawBarrier.attempts)
	}
	for hash, expected := range bodies {
		body, err := storage.GetChunk(ctx, rawRepo, hash)
		if err != nil || !bytes.Equal(body, expected) || domain.HashContent(body) != hash {
			test.Fatal("nested concurrent writers changed or lost the intended raw body", hash, err)
		}
	}
	actual, err := storage.GetMemory(ctx, memoryRepo, memory)
	if err != nil {
		test.Fatal(err)
	}
	encoded, err := json.Marshal(actual)
	if err != nil || !bytes.Equal(encoded, raw) {
		test.Fatal("nested concurrent writers changed the exact memory contents", err)
	}
	storedDoc, err := storage.GetDoc(ctx, docRepo, document.Hash)
	if err != nil {
		test.Fatal(err)
	}
	encoded, err = domain.CanonicalBytes(storedDoc.CIR)
	if err != nil || !bytes.Equal(encoded, canonical) {
		test.Fatal("promoting the nested component changed the original document", err)
	}
}
