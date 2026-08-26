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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
func (s *FileStore) getDocChunked(hash domain.ContentHash, data []byte) ([]byte, bool, error) {
	man, isMan := chunkcas.ParseManifest(data)
	if !isMan {
		return nil, false, nil
	}
	chunks := make([][]byte, 0, len(man.Chunks))
	for _, ch := range man.Chunks {
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
		chunks = append(chunks, c)
	}
	// Reassembly integrity: compare identity hash (entire canonical) — detect chunk corruption/manifest manipulation.
	cb, err := chunkcas.AssembleChunks(man, chunks, hash)
	if err != nil {
		return nil, true, err
	}
	return cb, true, nil
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

// RepackDocs converts legacy full doc to chunk storage with the same hash, cleans up orphan chunks (leftover from DeleteDoc).
// Returns: (number of conversions, bytes saved). Each conversion is only replaced after reassembly==original validation (lossless).
func (s *FileStore) RepackDocs() (converted int, saved int64, err error) {
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
		if e.IsDir() {
			continue
		}
		hash := domain.ContentHash("sha256:" + e.Name())
		if domain.ValidateContentHash(hash) != nil {
			continue
		}
		path := filepath.Join(docsDir, e.Name())
		raw, rerr := readCxtFile(path)
		if rerr != nil {
			continue
		}
		data, derr := docDecompress(raw)
		if derr != nil {
			continue
		}
		if cb, isMan, chunkErr := s.getDocChunked(hash, data); isMan {
			var man chunkcas.Manifest
			if json.Unmarshal(data, &man) != nil {
				continue
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
			// Irregular byte storage (old version record): parsing→canonical recalculation matches —
			// repack canonical (normalization included). Recalculation mismatch suggests contamination — preserved (fsck's job).
			var cir domain.CIRDocument
			if json.Unmarshal(data, &cir) != nil {
				continue
			}
			cb, cerr := domain.CanonicalBytes(cir)
			if cerr != nil || domain.HashContent(cb) != hash {
				continue
			}
			data = cb
		}
		before := int64(len(raw))
		ok, added, perr := s.putDocChunked(hash, data)
		if perr != nil {
			return converted, saved, perr
		}
		if !ok {
			continue // insufficient shape assumption — monolithic maintenance
		}
		var man chunkcas.Manifest
		if mraw, rerr := readCxtFile(path); rerr == nil {
			if mdata, derr := docDecompress(mraw); derr == nil && json.Unmarshal(mdata, &man) == nil {
				for _, ch := range man.Chunks {
					live[ch] = true
				}
				saved += before - int64(len(mraw)) - added
			}
		}
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
