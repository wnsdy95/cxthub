package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func validateMemoryDigestRefs(digest domain.MemoryDigest) error {
	if err := domain.ValidateOptionalContentHash(digest.SnapshotID); err != nil {
		return err
	}
	for _, fragment := range digest.Fragments {
		if err := validateHash(fragment.SourceSnapshot); err != nil {
			return err
		}
	}
	return nil
}

func (s *FSStore) memoryChunkPath(repoID, hash domain.ContentHash) string {
	return filepath.Join(s.repoDir(repoID), "objects", "memory_chunks", hexOf(hash))
}

func (s *FSStore) putMemoryChunked(repoID, hash domain.ContentHash, digest domain.MemoryDigest) (bool, int64, error) {
	plan, ok, err := domain.PlanMemoryChunks(digest)
	if err != nil || !ok {
		return false, 0, err
	}
	var added int64
	for _, chunkHash := range plan.Order {
		path := s.memoryChunkPath(repoID, chunkHash)
		body := plan.Bodies[chunkHash]
		if exists(path) {
			raw, err := os.ReadFile(path)
			if err != nil {
				return false, added, err
			}
			existing, err := docDecompress(raw)
			if err != nil || !bytes.Equal(existing, body) {
				return false, added, domain.ErrIntegrity
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
	if err := writeAtomic(s.memPath(repoID, hash), manifest); err != nil {
		return false, added, err
	}
	return true, added, nil
}

func (s *FSStore) getMemoryChunked(repoID, hash domain.ContentHash, data []byte) (domain.MemoryDigest, bool, error) {
	manifest, isManifest, err := domain.ParseMemoryChunkManifest(data)
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
		if err := validateHash(chunkHash); err != nil {
			return domain.MemoryDigest{}, true, err
		}
		raw, err := os.ReadFile(s.memoryChunkPath(repoID, chunkHash))
		if err != nil {
			if os.IsNotExist(err) {
				return domain.MemoryDigest{}, true, fmt.Errorf("%w: memory %s missing component %s", domain.ErrIntegrity, hash, chunkHash)
			}
			return domain.MemoryDigest{}, true, err
		}
		body, err := docDecompress(raw)
		if err != nil {
			return domain.MemoryDigest{}, true, domain.ErrIntegrity
		}
		bodies[chunkHash] = body
	}
	digest, err := domain.AssembleMemoryChunks(manifest, bodies)
	if err != nil {
		return domain.MemoryDigest{}, true, err
	}
	if err := validateMemoryDigestRefs(digest); err != nil {
		return domain.MemoryDigest{}, true, err
	}
	got, err := domain.MemoryDigestHash(digest)
	if err != nil || got != hash {
		return domain.MemoryDigest{}, true, domain.ErrIntegrity
	}
	return digest, true, nil
}

// RepackMemories upgrades every FS repo's large legacy memory object and
// removes the obsolete full memmeta copy only when an authoritative snapshot
// pointer and a valid blob prove the data remains reachable.
func (s *FSStore) RepackMemories() (converted int, saved int64, err error) {
	reposDir := filepath.Join(s.dataDir, "repos")
	repos, err := os.ReadDir(reposDir)
	if os.IsNotExist(err) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	for _, repoEntry := range repos {
		if !repoEntry.IsDir() {
			continue
		}
		repoID, ok := hashFromObjectName(repoEntry.Name())
		if !ok {
			continue
		}
		count, reclaimed, repackErr := s.repackMemoryRepo(repoID)
		converted += count
		saved += reclaimed
		if repackErr != nil {
			return converted, saved, repackErr
		}
	}
	return converted, saved, nil
}

func (s *FSStore) repackMemoryRepo(repoID domain.ContentHash) (converted int, saved int64, err error) {
	memoriesDir := filepath.Join(s.repoDir(repoID), "objects", "memories")
	entries, readDirErr := os.ReadDir(memoriesDir)
	if readDirErr != nil && !os.IsNotExist(readDirErr) {
		return 0, 0, readDirErr
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
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			continue
		}
		if _, isManifest, manifestErr := s.getMemoryChunked(repoID, hash, data); isManifest {
			var manifest domain.MemoryChunkManifest
			if json.Unmarshal(data, &manifest) == nil {
				for _, chunkHash := range append(manifest.SummaryChunks, manifest.FragmentChunks...) {
					live[chunkHash] = true
				}
				if manifest.Format != domain.MemoryChunkFormatV1 {
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
		chunked, added, putErr := s.putMemoryChunked(repoID, hash, digest)
		if putErr != nil {
			return converted, saved, putErr
		}
		if !chunked {
			continue
		}
		manifestData, readErr := os.ReadFile(path)
		if readErr != nil {
			return converted, saved, readErr
		}
		manifest, isManifest, parseErr := domain.ParseMemoryChunkManifest(manifestData)
		if parseErr != nil || !isManifest {
			return converted, saved, domain.ErrIntegrity
		}
		for _, chunkHash := range append(manifest.SummaryChunks, manifest.FragmentChunks...) {
			live[chunkHash] = true
		}
		saved += before - int64(len(manifestData)) - added
		converted++
	}

	if !sweepSafe {
		metaSaved, cleanupErr := s.cleanupRedundantMemoryMeta(context.Background(), repoID)
		return converted, saved + metaSaved, cleanupErr
	}
	chunksDir := filepath.Join(s.repoDir(repoID), "objects", "memory_chunks")
	chunkEntries, chunkErr := os.ReadDir(chunksDir)
	if chunkErr != nil && !os.IsNotExist(chunkErr) {
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

	metaSaved, cleanupErr := s.cleanupRedundantMemoryMeta(context.Background(), repoID)
	saved += metaSaved
	if cleanupErr != nil {
		return converted, saved, cleanupErr
	}
	return converted, saved, nil
}

func (s *FSStore) cleanupRedundantMemoryMeta(ctx context.Context, repoID domain.ContentHash) (int64, error) {
	dir := filepath.Join(s.repoDir(repoID), "memmeta")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var saved int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		snapshotID, ok := hashFromObjectName(strings.TrimSuffix(entry.Name(), ".json"))
		if !ok {
			continue
		}
		meta, err := s.GetMemoryMeta(ctx, repoID, snapshotID)
		if err != nil {
			continue
		}
		memoryHash, err := domain.MemoryDigestHash(meta)
		if err != nil {
			continue
		}
		snapshot, err := s.GetSnapshot(ctx, repoID, snapshotID)
		if err != nil || snapshot.MemoryHash != memoryHash {
			continue
		}
		stored, err := s.GetMemory(ctx, repoID, memoryHash)
		if err != nil || stored.SnapshotID != snapshotID {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return saved, err
		}
		saved += info.Size()
	}
	return saved, nil
}

func (s *FSStore) RepackObjects() (converted int, saved int64, err error) {
	docs, docSaved, err := s.RepackDocs()
	if err != nil {
		return docs, docSaved, err
	}
	memories, memorySaved, err := s.RepackMemories()
	return docs + memories, docSaved + memorySaved, err
}
