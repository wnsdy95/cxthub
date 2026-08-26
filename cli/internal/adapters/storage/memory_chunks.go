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

func validateMemoryDigestRefs(digest domain.MemoryDigest) error {
	if err := domain.ValidateOptionalContentHash(digest.SnapshotID); err != nil {
		return err
	}
	if err := domain.ValidateOptionalContentHash(digest.PreviousMemoryHash); err != nil {
		return err
	}
	for _, fragment := range digest.Fragments {
		if err := domain.ValidateContentHash(fragment.SourceSnapshot); err != nil {
			return err
		}
	}
	if coverage := digest.GraftCoverage; coverage != nil {
		if coverage.ProjectionVersion == 0 || coverage.GraftSeq > domain.MaxGraftSeq {
			return domain.ErrHashMismatch
		}
		if coverage.ProjectionComplete && coverage.LineageFingerprint == "" {
			return domain.ErrHashMismatch
		}
		if coverage.LineageFingerprint != "" {
			if err := domain.ValidateContentHash(coverage.LineageFingerprint); err != nil {
				return err
			}
		}
		for _, parent := range coverage.GraftParents {
			if err := domain.ValidateContentHash(parent); err != nil {
				return err
			}
		}
		for _, source := range coverage.PinnedSources {
			if err := domain.ValidateContentHash(source); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *FileStore) putMemoryChunked(hash domain.ContentHash, digest domain.MemoryDigest) (bool, int64, error) {
	plan, ok, err := chunkcas.PlanMemory(digest)
	if err != nil || !ok {
		return false, 0, err
	}
	var added int64
	for _, chunkHash := range plan.Order {
		path := s.objectPath("memory_chunks", chunkHash)
		body := plan.Bodies[chunkHash]
		if fileExists(path) {
			raw, err := readCxtFile(path)
			if err != nil {
				return false, added, err
			}
			existing, err := docDecompress(raw)
			if err != nil || !bytes.Equal(existing, body) {
				return false, added, domain.ErrHashMismatch
			}
			continue
		}
		compressed := docCompress(body)
		if err := writeAtomic(path, compressed); err != nil {
			return false, added, err
		}
		added += int64(len(compressed))
	}
	manifest, err := json.Marshal(plan.Manifest)
	if err != nil {
		return false, added, err
	}
	if err := writeAtomic(s.objectPath("memories", hash), manifest); err != nil {
		return false, added, err
	}
	return true, added, nil
}

func (s *FileStore) getMemoryChunked(hash domain.ContentHash, data []byte) (domain.MemoryDigest, bool, error) {
	manifest, isManifest, err := chunkcas.ParseMemoryManifest(data)
	if !isManifest {
		return domain.MemoryDigest{}, false, nil
	}
	if err != nil {
		return domain.MemoryDigest{}, true, err
	}
	bodies := make(map[domain.ContentHash][]byte, len(manifest.SummaryChunks)+len(manifest.FragmentChunks))
	all := append(append([]domain.ContentHash{}, manifest.SummaryChunks...), manifest.FragmentChunks...)
	for _, chunkHash := range all {
		if _, loaded := bodies[chunkHash]; loaded {
			continue
		}
		if err := domain.ValidateContentHash(chunkHash); err != nil {
			return domain.MemoryDigest{}, true, err
		}
		raw, err := readCxtFile(s.objectPath("memory_chunks", chunkHash))
		if err != nil {
			if os.IsNotExist(err) {
				return domain.MemoryDigest{}, true, fmt.Errorf("%w: memory %s missing component %s", domain.ErrHashMismatch, hash, chunkHash)
			}
			return domain.MemoryDigest{}, true, err
		}
		body, err := docDecompress(raw)
		if err != nil {
			return domain.MemoryDigest{}, true, domain.ErrHashMismatch
		}
		bodies[chunkHash] = body
	}
	digest, err := chunkcas.AssembleMemory(manifest, bodies)
	if err != nil {
		return domain.MemoryDigest{}, true, err
	}
	if err := validateMemoryDigestRefs(digest); err != nil {
		return domain.MemoryDigest{}, true, err
	}
	got, err := domain.MemoryDigestHash(digest)
	if err != nil || got != hash {
		return domain.MemoryDigest{}, true, domain.ErrHashMismatch
	}
	return digest, true, nil
}

// RepackMemories converts large legacy MemoryDigest JSON objects into 64 KiB
// component CAS manifests. MemoryDigestHash remains unchanged. Existing and
// corrupt manifests are never rewritten by maintenance, and a grace window
// protects chunks concurrently created during mark-and-sweep.
func (s *FileStore) RepackMemories() (converted int, saved int64, err error) {
	memoriesDir := filepath.Join(s.storeDir(), "objects", "memories")
	entries, err := os.ReadDir(memoriesDir)
	if os.IsNotExist(err) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	live := map[domain.ContentHash]bool{}
	sweepSafe := true
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		hash, ok := hashFromObjectName(entry.Name())
		if !ok {
			continue
		}
		path := filepath.Join(memoriesDir, entry.Name())
		data, readErr := readCxtFile(path)
		if readErr != nil {
			continue
		}
		if _, isManifest, manifestErr := s.getMemoryChunked(hash, data); isManifest {
			var manifest chunkcas.MemoryManifest
			if json.Unmarshal(data, &manifest) == nil {
				for _, chunkHash := range append(manifest.SummaryChunks, manifest.FragmentChunks...) {
					live[chunkHash] = true
				}
				if !chunkcas.SupportedMemoryFormat(manifest.Format) {
					sweepSafe = false
				}
			} else {
				sweepSafe = false
			}
			if manifestErr != nil {
				continue
			}
			continue
		}
		var digest domain.MemoryDigest
		if json.Unmarshal(data, &digest) != nil {
			continue
		}
		got, hashErr := domain.MemoryDigestHash(digest)
		if hashErr != nil || got != hash {
			continue
		}
		before := int64(len(data))
		chunked, added, putErr := s.putMemoryChunked(hash, digest)
		if putErr != nil {
			return converted, saved, putErr
		}
		if !chunked {
			continue
		}
		manifestData, readErr := readCxtFile(path)
		if readErr != nil {
			return converted, saved, readErr
		}
		manifest, isManifest, parseErr := chunkcas.ParseMemoryManifest(manifestData)
		if parseErr != nil || !isManifest {
			return converted, saved, domain.ErrHashMismatch
		}
		for _, chunkHash := range append(manifest.SummaryChunks, manifest.FragmentChunks...) {
			live[chunkHash] = true
		}
		saved += before - int64(len(manifestData)) - added
		converted++
	}

	if !sweepSafe {
		return converted, saved, nil
	}
	chunksDir := filepath.Join(s.storeDir(), "objects", "memory_chunks")
	chunkEntries, chunkErr := os.ReadDir(chunksDir)
	if os.IsNotExist(chunkErr) {
		return converted, saved, nil
	}
	if chunkErr != nil {
		return converted, saved, chunkErr
	}
	for _, entry := range chunkEntries {
		if entry.IsDir() {
			continue
		}
		chunkHash, ok := hashFromObjectName(entry.Name())
		if !ok || live[chunkHash] {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || time.Since(info.ModTime()) < 10*time.Minute {
			continue
		}
		saved += info.Size()
		_ = os.Remove(filepath.Join(chunksDir, entry.Name()))
	}
	return converted, saved, nil
}

// RepackObjects upgrades every chunkable local object type.
func (s *FileStore) RepackObjects() (converted int, saved int64, err error) {
	docs, docSaved, err := s.RepackDocs()
	if err != nil {
		return docs, docSaved, err
	}
	memories, memorySaved, err := s.RepackMemories()
	return docs + memories, docSaved + memorySaved, err
}
