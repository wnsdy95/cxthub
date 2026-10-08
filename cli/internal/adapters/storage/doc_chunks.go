// doc_chunks.go — Session doc chunk CAS storage layer.
//
// Issue (empirically verified): doc is an immutable object representing the entire transcript up to that point, so it is rehashed and rewritten in its entirety with each hook capture — 1,120MB local store with only 28MB unique content (97% duplication, one session has 124 docs/804MB). Sessions grow append-only, so prefixes are the same across captures.
//
// Design Principle: **Immutable Identity, Chunkify Storage Layer Only.** DocHash (==Snapshot.ID) is the hash of the entire canonical bytes — the snap.ID==DocHash invariant (verified, dedup, server protocol basis) is not disturbed, so the identity of the existing object is not broken, and the entire doc can be repacked (repack) into the same hash as chunks.
//
// Layout:
//
//	objects/docs/<hash>   = zstd(manifest JSON {format, envelope, chunks[]})  ← or legacy zstd(entire)
//	objects/chunks/<h_i>  = zstd(v2 canonical event-stream byte range; v1 whole-event group remains readable)
//
// Canonical bytes are exactly `{"envelope":<env>,"events":[<e1>,<e2>,…]}` form (key sorting: envelope < events, compact) so the envelope fragment + event fragment list can be reassembled byte-by-byte. On write, reassembly==validation, and if mismatched, fallback to full storage (fail-safe — even if the shape assumption is broken, data is always stored correctly). On read, compare the hash of the reassembly result with the requested hash to detect chunk corruption (free of charge).
//
// v2 chunk boundaries are fixed offsets in the canonical events-array interior. Closed chunks from append-only growth are the same bytes in subsequent captures → same hash for deduplication, and only the last open chunk is rewritten (capture increase ≈ delta + one open chunk), even when one event is larger than the transport bound.
package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// putDocChunked stores canonical bytes as chunks+manifest.
// Returns false if reassembly does not match the original byte-for-byte (caller should fallback to full storage).
func (s *FileStore) putDocChunked(h domain.ContentHash, cb []byte) (bool, int64, error) {
	plan, ok := chunkcas.PlanDoc(cb)
	if !ok {
		return false, 0, nil // shape assumption failure/reassembly mismatch — fallback to full storage
	}
	var added int64
	for _, ch := range plan.Order {
		p := s.objectPath("chunks", ch)
		if fileExists(p) {
			raw, err := readCxtFile(p)
			if err != nil {
				return false, added, err
			}
			existing, err := docDecompress(raw)
			if err != nil || !bytes.Equal(existing, plan.Bodies[ch]) {
				return false, added, domain.ErrHashMismatch
			}
		} else {
			compressed := docCompress(plan.Bodies[ch])
			if err := writeAtomic(p, compressed); err != nil {
				return false, added, err
			}
			added += int64(len(compressed))
		}
	}
	mb, err := json.Marshal(plan.Manifest)
	if err != nil {
		return false, added, err
	}
	return true, added, writeAtomic(s.objectPath("docs", h), docCompress(mb))
}

// getDocChunked reconstructs canonical bytes from manifest in chunks.
// Returns (bytes, isManifest, err) — isManifest=false means data is not a manifest (legacy full).
func (s *FileStore) getDocChunked(ctx context.Context, hash domain.ContentHash, data []byte) ([]byte, bool, error) {
	return s.getDocChunkedObserved(ctx, hash, data, nil)
}

// observe sees exactly the stored bytes and their decoded body used for reconstruction.
// A verification receipt must never describe a separate, potentially raced read.
func (s *FileStore) getDocChunkedObserved(ctx context.Context, hash domain.ContentHash, data []byte, observe func(string, domain.ContentHash, []byte, []byte)) ([]byte, bool, error) {
	man, isMan := chunkcas.ParseManifest(data)
	if !isMan {
		return nil, false, nil
	}
	chunks := make([][]byte, 0, len(man.Chunks))
	for _, ch := range man.Chunks {
		if err := ctx.Err(); err != nil {
			return nil, true, err
		}
		if err := domain.ValidateContentHash(ch); err != nil {
			return nil, true, domain.ErrInvalidCIR
		}
		raw, err := readCxtFile(s.objectPath("chunks", ch))
		if err != nil {
			return nil, true, fmt.Errorf("%w: doc %s missing chunk %s", domain.ErrNotFound, hash, ch)
		}
		c, err := docDecompress(raw)
		if err != nil {
			return nil, true, domain.ErrInvalidCIR
		}
		if observe != nil {
			observe("chunks", ch, raw, c)
		}
		chunks = append(chunks, c)
	}
	// Reassembly integrity: compare identity hash (entire canonical) — detect chunk corruption/manifest manipulation.
	cb, err := chunkcas.AssembleChunks(man, chunks, hash)
	if err != nil {
		return nil, true, err
	}
	return cb, true, ctx.Err()
}

func (s *FileStore) readStoredDoc(ctx context.Context, hash domain.ContentHash, observe func(string, domain.ContentHash, []byte, []byte)) ([]byte, bool, error) {
	raw, err := readCxtFile(s.objectPath("docs", hash))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, domain.ErrNotFound
		}
		return nil, false, err
	}
	data, err := legacyDocumentData(ctx, raw)
	if err != nil {
		return nil, false, err
	}
	if observe != nil {
		observe("docs", hash, raw, data)
	}
	if cb, isManifest, err := s.getDocChunkedObserved(ctx, hash, data, observe); isManifest {
		return cb, true, err
	}
	return data, false, ctx.Err()
}

// HasChunk checks local chunk existence (pull delta negotiation — body retrieval).
func (s *FileStore) HasChunk(hash domain.ContentHash) bool {
	if domain.ValidateContentHash(hash) != nil {
		return false
	}
	return fileExists(s.objectPath("chunks", hash))
}

// GetChunk returns the local chunk body (uncompressed) for pull reassembly.
func (s *FileStore) GetChunk(hash domain.ContentHash) ([]byte, error) {
	if err := domain.ValidateContentHash(hash); err != nil {
		return nil, err
	}
	raw, err := readCxtFile(s.objectPath("chunks", hash))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	return docDecompress(raw)
}

// PutChunk stages a verified immutable chunk before the document completes.
// A retry can reuse it; collection may reclaim unreferenced staging after the
// active retention lease ends, in which case the next pull safely refetches it.
func (s *FileStore) PutChunk(ctx context.Context, hash domain.ContentHash, body []byte) error {
	if err := domain.ValidateContentHash(hash); err != nil {
		return err
	}
	if len(body) == 0 || domain.HashContent(body) != hash {
		return domain.ErrHashMismatch
	}
	return s.WithObjectsRetained(ctx, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := s.objectPath("chunks", hash)
		if fileExists(path) {
			current, err := s.GetChunk(hash)
			if err != nil {
				return err
			}
			if !bytes.Equal(current, body) {
				return domain.ErrHashMismatch
			}
			return nil
		}
		return writeAtomic(path, docCompress(body))
	})
}

// RepackDocs converts legacy full doc to chunk storage with the same hash, cleans up orphan chunks (leftover from DeleteDoc).
// Returns: (number of conversions, bytes saved). Each conversion is only replaced after reassembly==original validation (lossless).
func (s *FileStore) RepackDocs() (converted int, saved int64, err error) {
	_, err = s.withOSLock(context.Background(), "object-retention", "repo", syscall.LOCK_EX, true, func() error {
		var inner error
		converted, saved, inner = s.repackDocs()
		return inner
	})
	return
}

func (s *FileStore) repackDocs() (converted int, saved int64, err error) {
	docsDir := filepath.Join(s.storeDir(), "objects", "docs")
	entries, err := os.ReadDir(docsDir)
	if os.IsNotExist(err) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	live := map[domain.ContentHash]bool{}
	for _, e := range entries {
		hash := domain.ContentHash("sha256:" + e.Name())
		if domain.ValidateContentHash(hash) != nil {
			continue
		}
		if e.IsDir() {
			return converted, saved, fmt.Errorf("%w: document object is a directory", domain.ErrInvalidCIR)
		}
		path := filepath.Join(docsDir, e.Name())
		raw, rerr := readCxtFile(path)
		if rerr != nil {
			return converted, saved, rerr
		}
		root, rerr := storedRootDeclaration(context.Background(), bytes.NewReader(raw))
		if rerr != nil {
			return converted, saved, rerr
		}
		if root {
			data, err := boundedRootData(context.Background(), bytes.NewReader(raw), domain.MaxConversationManifestBytes)
			if err != nil {
				return converted, saved, err
			}
			manifest, err := domain.DecodeConversationManifest(data)
			if err != nil {
				return converted, saved, err
			}
			if _, err := s.verifyRootDocument(context.Background(), domain.DocumentRef{Hash: hash, Identity: domain.DocumentIdentityRootV1}, manifest, false); err != nil {
				return converted, saved, err
			}
			for _, chunk := range manifest.Chunks {
				live[chunk.Hash] = true
			}
			continue // Roots keep their exact manifest, partitions and identity.
		}
		data, derr := docDecompress(raw)
		if derr != nil {
			return converted, saved, derr
		}
		_, knownManifest := chunkcas.ParseManifest(data)
		if err := validateRepackDocShape(data, knownManifest); err != nil {
			return converted, saved, err
		}
		if cb, isMan, chunkErr := s.getDocChunked(context.Background(), hash, data); isMan {
			var man chunkcas.Manifest
			if err := json.Unmarshal(data, &man); err != nil {
				return converted, saved, err
			}
			if chunkErr != nil {
				// Preserve every referenced body when an existing manifest cannot be
				// reassembled. Repack is maintenance, never corruption repair.
				for _, ch := range man.Chunks {
					live[ch] = true
				}
				continue
			}
			if man.Format == chunkcas.FormatV2 {
				for _, ch := range man.Chunks {
					live[ch] = true
				}
				continue
			}
			// A valid v1 manifest is a migration source. Reassemble first, then
			// atomically replace only the manifest after all v2 chunks exist.
			data = cb
		}
		// Legacy monolithic: hash verification then chunk transformation.
		if domain.HashContent(data) != hash {
			// Older noncanonical records may still have the canonical CIR hash.
			// Otherwise preserve the object and stop before destructive sweeping.
			var cir domain.CIRDocument
			if json.Unmarshal(data, &cir) != nil {
				return converted, saved, domain.ErrInvalidCIR
			}
			cb, cerr := domain.CanonicalBytes(cir)
			if cerr != nil || domain.HashContent(cb) != hash {
				return converted, saved, domain.ErrHashMismatch
			}
			data = cb
		}
		before := int64(len(raw))
		ok, added, perr := s.putDocChunked(hash, data)
		if perr != nil {
			return converted, saved, perr
		}
		if !ok {
			// A self-hashed unknown object is not a verified legacy monolith.
			// Establish its empty dependency set before allowing any sweep.
			if _, err := decodeStoredSessionDoc(context.Background(), hash, data, false); err != nil {
				return converted, saved, err
			}
			continue
		}
		storedBytes, err := markRepackedDoc(path, live)
		if err != nil {
			return converted, saved, err
		}
		saved += before - storedBytes - added
		converted++
	}
	// Orphan chunk cleanup (mark&sweep): all chunks from manifests marked above.
	// Do not delete new chunks (not yet marked) written during scan, so
	// skip recently created files (less than 10 minutes old) — next repack will clean them up.
	chunksDir := filepath.Join(s.storeDir(), "objects", "chunks")
	centries, cerr := os.ReadDir(chunksDir)
	if cerr != nil {
		return converted, saved, nil
	}
	for _, e := range centries {
		if e.IsDir() {
			continue
		}
		ch := domain.ContentHash("sha256:" + e.Name())
		if domain.ValidateContentHash(ch) != nil || live[ch] {
			continue
		}
		fi, ferr := e.Info()
		if ferr != nil || time.Since(fi.ModTime()) < 10*time.Minute {
			continue
		}
		saved += fi.Size()
		_ = os.Remove(filepath.Join(chunksDir, e.Name()))
	}
	return converted, saved, nil
}

// GC must classify a known representation before a typed CIR decode can drop
// unknown dependency fields. Only top-level keys are restricted here; legacy
// whitespace, key order/case and nested CIR decoding retain their behavior.
// Scan keys without making another copy of the complete events array.
func validateRepackDocShape(data []byte, manifest bool) error {
	if !json.Valid(data) || !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		return domain.ErrInvalidCIR
	}
	seen := map[string]bool{}
	depth, start := 0, 0
	quoted, escaped := false, false
	for i, ch := range data {
		if quoted {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				quoted = false
				if depth != 1 {
					continue
				}
				j := i + 1
				for j < len(data) && (data[j] == ' ' || data[j] == '\n' || data[j] == '\r' || data[j] == '\t') {
					j++
				}
				if j == len(data) || data[j] != ':' {
					continue
				}
				var key string
				if i-start > 256 || json.Unmarshal(data[start:i+1], &key) != nil {
					return domain.ErrInvalidCIR
				}
				key = strings.ToLower(key)
				allowed := key == "envelope" || (!manifest && key == "events") || (manifest && (key == "format" || key == "chunks"))
				if !allowed || seen[key] {
					return fmt.Errorf("%w: unknown or ambiguous document representation", domain.ErrInvalidCIR)
				}
				seen[key] = true
			}
			continue
		}
		switch ch {
		case '"':
			quoted, start = true, i
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
	}
	if !seen["envelope"] {
		return domain.ErrInvalidCIR
	}
	return nil
}

// Conversion is not complete until its current descriptor supplies a known
// dependency set. A failed reread must abort before accounting or sweeping.
func markRepackedDoc(path string, live map[domain.ContentHash]bool) (int64, error) {
	raw, err := readCxtFile(path)
	if err != nil {
		return 0, err
	}
	data, err := docDecompress(raw)
	if err != nil {
		return 0, err
	}
	manifest, ok := chunkcas.ParseManifest(data)
	if !ok || manifest.Format != chunkcas.FormatV2 {
		return 0, domain.ErrInvalidCIR
	}
	if err := validateRepackDocShape(data, true); err != nil {
		return 0, err
	}
	for _, hash := range manifest.Chunks {
		if err := domain.ValidateContentHash(hash); err != nil {
			return 0, err
		}
	}
	for _, hash := range manifest.Chunks {
		live[hash] = true
	}
	return int64(len(raw)), nil
}
