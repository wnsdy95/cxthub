// doc_chunks.go — Server-side session doc chunk CAS (symmetric contract with client storage/doc_chunks.go).
//
// doc is a comprehensive transcript object up to the capture point, so the entire thing is
// rewritten every time the session grows (empirically verified 97%). The session is append-only,
// so only the storage layer is chunked:
//
//	repos/<r>/objects/docs/<hash>   = zstd(mani{format, envelope, chunks[]})  ← or legacy zstd(comprehensive)
//	repos/<r>/objects/chunks/<h_i>  = zstd(v2 canonical event-stream byte range; v1 remains readable)
//
// The integrity hash (DocHash=Snapshot.ID) remains the same as the comprehensive canonical standard — protocol·validation unchanged,
// legacy comprehensives can be repacked losslessly with the same hash. Pre-write reassembly==original validation (mismatch triggers
// comprehensive fallback), read-time reassembly hash comparison detects chunk corruption.
// Chunks are isolated within the repo directory — maintaining ownership isolation (preventing content sharing across repos).
package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *FSStore) chunkPath(repoID, hash domain.ContentHash) string {
	return filepath.Join(s.repoDir(repoID), "objects", "chunks", hexOf(hash))
}

// putDocChunked stores canonical bytes as chunk+manifest (returns false — comprehensive fallback if not possible).
func (s *FSStore) putDocChunked(repoID, h domain.ContentHash, cb []byte) (bool, int64, error) {
	plan, ok := domain.PlanDocChunks(cb)
	if !ok {
		return false, 0, nil
	}
	var added int64
	for _, ch := range plan.Order {
		p := s.chunkPath(repoID, ch)
		if exists(p) {
			raw, err := os.ReadFile(p)
			if err != nil {
				return false, added, err
			}
			existing, err := docDecompress(raw)
			if err != nil || !bytes.Equal(existing, plan.Bodies[ch]) {
				return false, added, domain.ErrIntegrity
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
	return true, added, writeAtomic(s.docPath(repoID, h), docCompress(mb))
}

// getDocChunked reassembles manifest from chunks. If isManifest=false, it's a legacy comprehensive.
func (s *FSStore) getDocChunked(repoID, hash domain.ContentHash, data []byte) ([]byte, bool, error) {
	man, isMan := domain.ParseDocChunkManifest(data)
	if !isMan {
		return nil, false, nil
	}
	chunks := make([][]byte, 0, len(man.Chunks))
	for _, ch := range man.Chunks {
		if err := validateHash(ch); err != nil {
			return nil, true, domain.ErrIntegrity
		}
		raw, err := os.ReadFile(s.chunkPath(repoID, ch))
		if err != nil {
			return nil, true, fmt.Errorf("%w: doc %s missing chunk %s", domain.ErrNotFound, hash, ch)
		}
		c, err := docDecompress(raw)
		if err != nil {
			return nil, true, domain.ErrIntegrity
		}
		chunks = append(chunks, c)
	}
	cb, err := domain.AssembleDocChunks(man, chunks, hash)
	if err != nil {
		return nil, true, fmt.Errorf("%w: chunked doc %s reassembly hash mismatch", domain.ErrIntegrity, hash)
	}
	return cb, true, nil
}

// HasChunks returns the repo owner's chunk existence list (negotiate delta negotiation).
func (s *FSStore) HasChunks(_ context.Context, repoID domain.ContentHash, hashes []domain.ContentHash) ([]domain.ContentHash, error) {
	if err := validateHash(repoID); err != nil {
		return nil, err
	}
	var have []domain.ContentHash
	for _, ch := range hashes {
		if err := validateHash(ch); err != nil {
			return nil, err
		}
		if exists(s.chunkPath(repoID, ch)) {
			have = append(have, ch)
		}
	}
	return have, nil
}

// PutChunks stores bounded wire chunks that arrive before the complete doc. Paths are
// internal to the repo, so the same hash doesn't grant implicit read permissions to other repos.
func (s *FSStore) PutChunks(ctx context.Context, repoID domain.ContentHash, chunks map[domain.ContentHash][]byte) (int, int, error) {
	if err := validateHash(repoID); err != nil {
		return 0, 0, err
	}
	stored, deduped := 0, 0
	for hash, body := range chunks {
		if err := ctx.Err(); err != nil {
			return stored, deduped, err
		}
		if err := validateHash(hash); err != nil {
			return stored, deduped, err
		}
		if domain.HashContent(body) != hash {
			return stored, deduped, domain.ErrIntegrity
		}
		path := s.chunkPath(repoID, hash)
		exists, err := regularObjectExists(path)
		if err != nil {
			return stored, deduped, err
		}
		if exists {
			got, err := s.GetChunk(ctx, repoID, hash)
			if err != nil || !bytes.Equal(got, body) {
				return stored, deduped, domain.ErrIntegrity
			}
			deduped++
			continue
		}
		if err := writeAtomic(path, docCompress(body)); err != nil {
			return stored, deduped, err
		}
		stored++
	}
	return stored, deduped, nil
}

// GetChunk returns the repo owner's chunk body (uncompressed) for pull chunk wire.
func (s *FSStore) GetChunk(_ context.Context, repoID, hash domain.ContentHash) ([]byte, error) {
	if err := validateHashes(repoID, hash); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(s.chunkPath(repoID, hash))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	return docDecompress(raw)
}

// GetDocManifest returns the doc's chunk manifest for pull delta (returns canonical plan even if stored as comprehensive).
// Infeasible cases signal ErrNotFound (caller-side comprehensive fallback).
func (s *FSStore) GetDocManifest(ctx context.Context, repoID, hash domain.ContentHash) (domain.DocChunkManifest, error) {
	if err := validateHashes(repoID, hash); err != nil {
		return domain.DocChunkManifest{}, err
	}
	raw, err := os.ReadFile(s.docPath(repoID, hash))
	if err != nil {
		if os.IsNotExist(err) {
			return domain.DocChunkManifest{}, domain.ErrNotFound
		}
		return domain.DocChunkManifest{}, err
	}
	data, err := docDecompress(raw)
	if err != nil {
		return domain.DocChunkManifest{}, domain.ErrIntegrity
	}
	if man, isMan := domain.ParseDocChunkManifest(data); isMan {
		return man, nil
	}
	// Legacy comprehensive: canonical normalization then plan calculation — chunks are not stored,
	// so save chunks here for GetChunk fallback (lazy repack on request).
	if domain.HashContent(data) != hash {
		var cir domain.CIRDocument
		if json.Unmarshal(data, &cir) != nil {
			return domain.DocChunkManifest{}, domain.ErrIntegrity
		}
		cb, cerr := domain.CanonicalBytes(cir)
		if cerr != nil || domain.HashContent(cb) != hash {
			return domain.DocChunkManifest{}, domain.ErrIntegrity
		}
		data = cb
	}
	ok, _, perr := s.putDocChunked(repoID, hash, data)
	if perr != nil || !ok {
		return domain.DocChunkManifest{}, domain.ErrNotFound
	}
	raw2, err := os.ReadFile(s.docPath(repoID, hash))
	if err != nil {
		return domain.DocChunkManifest{}, err
	}
	data2, err := docDecompress(raw2)
	if err != nil {
		return domain.DocChunkManifest{}, domain.ErrIntegrity
	}
	if man, isMan := domain.ParseDocChunkManifest(data2); isMan {
		return man, nil
	}
	return domain.DocChunkManifest{}, domain.ErrNotFound
}

// RepackDocs converts the legacy monolithic docs of the entire repo into chunked form with the same hash, cleans up orphan chunks (cxtd repack). Each transformation is replaced only after hash validation (lossless).
func (s *FSStore) RepackDocs() (converted int, saved int64, err error) {
	reposDir := filepath.Join(s.dataDir, "repos")
	repos, err := os.ReadDir(reposDir)
	if os.IsNotExist(err) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	for _, r := range repos {
		if !r.IsDir() {
			continue
		}
		repoID := domain.ContentHash("sha256:" + r.Name())
		if validateHash(repoID) != nil {
			continue
		}
		c, sv, rerr := s.repackRepo(repoID)
		converted += c
		saved += sv
		if rerr != nil {
			return converted, saved, rerr
		}
	}
	return converted, saved, nil
}

func (s *FSStore) repackRepo(repoID domain.ContentHash) (converted int, saved int64, err error) {
	docsDir := filepath.Join(s.repoDir(repoID), "objects", "docs")
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
		if validateHash(hash) != nil {
			continue
		}
		path := filepath.Join(docsDir, e.Name())
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		data, derr := docDecompress(raw)
		if derr != nil {
			continue
		}
		if cb, isMan, chunkErr := s.getDocChunked(repoID, hash, data); isMan {
			var man domain.DocChunkManifest
			if json.Unmarshal(data, &man) != nil {
				continue
			}
			if chunkErr != nil {
				for _, ch := range man.Chunks {
					live[ch] = true
				}
				continue
			}
			if man.Format == domain.ChunkFormatV2 {
				for _, ch := range man.Chunks {
					live[ch] = true
				}
				continue
			}
			data = cb
		}
		if domain.HashContent(data) != hash {
			// Legacy non-canonical storage (server records before canonical sorting — empirically verified 162/214): raw bytes may differ, but if parsing→canonical recalculation matches the hash, it is a valid doc. Also normalizes storage by repacking into canonical form. If recalculation also mismatches, it is corrupted — fsck's job.
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
		ok, added, perr := s.putDocChunked(repoID, hash, data)
		if perr != nil {
			return converted, saved, perr
		}
		if !ok {
			continue
		}
		if mraw, rerr := os.ReadFile(path); rerr == nil {
			var man domain.DocChunkManifest
			if mdata, derr := docDecompress(mraw); derr == nil && json.Unmarshal(mdata, &man) == nil {
				for _, ch := range man.Chunks {
					live[ch] = true
				}
				saved += before - int64(len(mraw)) - added
			}
		}
		converted++
	}
	// Orphan chunk mark&sweep — recently created files (concurrent push possibility) are deferred.
	chunksDir := filepath.Join(s.repoDir(repoID), "objects", "chunks")
	centries, cerr := os.ReadDir(chunksDir)
	if cerr != nil {
		return converted, saved, nil
	}
	for _, e := range centries {
		if e.IsDir() {
			continue
		}
		ch := domain.ContentHash("sha256:" + e.Name())
		if validateHash(ch) != nil || live[ch] {
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
