package storage

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

var _ outbound.ChunkedDocumentStore = (*FileStore)(nil)

// WithVerifiedDocChunks retains a verified v2 representation through the
// synchronous upload. Shared retention leases can nest inside the app's push
// lease: each uses its own shared flock, with no process-local exclusive mutex.
func (s *FileStore) WithVerifiedDocChunks(ctx context.Context, id domain.ContentHash, use func(outbound.DocumentChunks) error) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := domain.ValidateContentHash(id); err != nil {
		return false, err
	}
	if use == nil {
		return false, fmt.Errorf("document chunk callback is nil")
	}
	var supported bool
	err := s.WithObjectsRetained(ctx, func() error {
		evidence, err := s.verifyStoredDoc(ctx, id, true)
		if err != nil {
			return err
		}
		manifest, allowed, ok, err := uploadDocManifest(ctx, evidence)
		if err != nil || !ok {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		doc := outbound.DocumentChunks{
			Hash: id, Format: manifest.Format,
			Envelope: bytes.Clone(manifest.Envelope),
			Chunks:   append([]domain.ContentHash(nil), manifest.Chunks...),
		}
		// Neither callback-owned slice can alter the loader's authorization.
		var mu sync.RWMutex
		active := true
		defer func() {
			// Wait for reads already in progress before releasing retention,
			// and fail closed after callback completion (including panic).
			mu.Lock()
			active = false
			mu.Unlock()
		}()
		doc.ReadChunk = func(readCtx context.Context, hash domain.ContentHash) ([]byte, error) {
			mu.RLock()
			defer mu.RUnlock()
			if !active {
				return nil, fmt.Errorf("document chunk callback has ended")
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if err := readCtx.Err(); err != nil {
				return nil, err
			}
			if _, ok := allowed[hash]; !ok {
				return nil, fmt.Errorf("%w: chunk is not listed in verified document", domain.ErrHashMismatch)
			}
			body, err := s.GetChunk(hash)
			if err != nil {
				return nil, err
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if err := readCtx.Err(); err != nil {
				return nil, err
			}
			if domain.HashContent(body) != hash {
				return nil, domain.ErrHashMismatch
			}
			return body, nil
		}
		supported = true
		if err := use(doc); err != nil {
			return err
		}
		return ctx.Err()
	})
	return supported, err
}

// All eligibility checks precede use, including when a valid v2 representation
// has arbitrary partitions larger than the portable wire bound. Such objects
// can still use the legacy push planner, which rechunks their canonical bytes.
// Every chunk must pass identity checks before a size-based fallback is allowed.
func uploadDocManifest(ctx context.Context, evidence storedDocEvidence) (chunkcas.Manifest, map[domain.ContentHash]struct{}, bool, error) {
	data, err := docDecompress(evidence.raw)
	if err != nil {
		return chunkcas.Manifest{}, nil, false, domain.ErrInvalidCIR
	}
	manifest, ok := chunkcas.ParseManifest(data)
	if !ok || manifest.Format != chunkcas.FormatV2 {
		// Full canonical/CIR validation has already succeeded. Raw documents
		// and v1 manifests are unsupported, not unvalidated fallback candidates.
		return chunkcas.Manifest{}, nil, false, ctx.Err()
	}
	files := make(map[domain.ContentHash]docVerifiedFile, len(evidence.proof.Files))
	for _, file := range evidence.proof.Files {
		if err := ctx.Err(); err != nil {
			return chunkcas.Manifest{}, nil, false, err
		}
		if file.Kind != "chunks" {
			continue
		}
		if previous, exists := files[file.ID]; exists && previous != file {
			return chunkcas.Manifest{}, nil, false, domain.ErrHashMismatch
		}
		files[file.ID] = file
	}
	allowed := make(map[domain.ContentHash]struct{}, len(manifest.Chunks))
	portable := true
	for _, hash := range manifest.Chunks {
		if err := ctx.Err(); err != nil {
			return chunkcas.Manifest{}, nil, false, err
		}
		file, exists := files[hash]
		if !exists || file.Body != hash || file.BodyBytes < 0 {
			return chunkcas.Manifest{}, nil, false, domain.ErrHashMismatch
		}
		if file.BodyBytes == 0 || file.BodyBytes > chunkcas.MaxPortableChunkBytes {
			portable = false
		}
		allowed[hash] = struct{}{}
	}
	return manifest, allowed, portable, ctx.Err()
}
