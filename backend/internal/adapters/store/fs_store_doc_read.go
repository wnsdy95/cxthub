package store

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *FSStore) readIndexPath(repo, hash domain.ContentHash) string {
	return filepath.Join(s.repoDir(repo), "read-index-v1", hexOf(hash))
}

func (s *FSStore) putReadIndex(repo domain.ContentHash, doc domain.SessionDoc) error {
	idx, err := domain.BuildDocReadIndex(doc)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	// Search text and event offsets have different read paths. Keep the viewer
	// projection small even when a transcript contains a large seed prompt.
	if err = writeAtomic(s.readIndexPath(repo, doc.Hash)+".search", docCompress(raw)); err != nil {
		return err
	}
	bits := make([]byte, 32768)
	for i := range idx.Events {
		for _, slot := range searchTrigrams(strings.ToLower(idx.Events[i].Text)) {
			bits[slot/8] |= 1 << (slot % 8)
		}
		idx.Events[i].Text = ""
	}
	if err = writeAtomic(s.readIndexPath(repo, doc.Hash)+".filter", bits); err != nil {
		return err
	}
	raw, err = json.Marshal(idx)
	if err != nil {
		return err
	}
	return writeAtomic(s.readIndexPath(repo, doc.Hash), docCompress(raw))
}

func (s *FSStore) DocReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
	var idx domain.DocReadIndex
	if err := validateHashes(repo, hash); err != nil {
		return idx, err
	}
	// An index never grants ownership of a missing/foreign document.
	if _, err := os.Stat(s.docPath(repo, hash)); os.IsNotExist(err) {
		return idx, domain.ErrNotFound
	} else if err != nil {
		return idx, err
	}
	raw, err := os.ReadFile(s.readIndexPath(repo, hash))
	if err == nil {
		raw, err = docDecompress(raw)
		if err == nil {
			err = json.Unmarshal(raw, &idx)
		}
		if err == nil && idx.Version == 1 && idx.Hash == hash {
			return idx, nil
		}
		return idx, domain.ErrIntegrity
	}
	if !os.IsNotExist(err) {
		return idx, err
	}
	if err = ctx.Err(); err != nil {
		return idx, err
	}
	doc, err := s.GetDoc(ctx, repo, hash)
	if err != nil {
		return idx, err
	}
	// Normalize old v1 manifests to verified v2 so offsets remain portable.
	cb, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		return idx, err
	}
	if len(doc.CIR.Events) > 0 {
		if _, _, err = s.putDocChunked(repo, hash, cb); err != nil {
			return idx, err
		}
	}
	if err = s.putReadIndex(repo, doc); err != nil {
		return idx, err
	}
	return s.DocReadIndex(ctx, repo, hash)
}

func (s *FSStore) SearchDocEvents(ctx context.Context, repo, hash domain.ContentHash, q string, after, limit int) ([]domain.DocEventIndex, error) {
	if err := validateHashes(repo, hash); err != nil {
		return nil, err
	}
	if _, err := os.Stat(s.docPath(repo, hash)); os.IsNotExist(err) {
		return nil, domain.ErrNotFound
	} else if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := s.readIndexPath(repo, hash)
	bits, err := os.ReadFile(path + ".filter")
	if os.IsNotExist(err) {
		if _, err = s.DocReadIndex(ctx, repo, hash); err != nil {
			return nil, err
		}
		bits, err = os.ReadFile(path + ".filter")
	}
	if err != nil {
		return nil, err
	}
	if len(bits) != 32768 {
		return nil, domain.ErrIntegrity
	}
	// Bloom filtering has no false negatives; exact matching still happens below.
	for _, slot := range searchTrigrams(q) {
		if bits[slot/8]&(1<<(slot%8)) == 0 {
			return []domain.DocEventIndex{}, nil
		}
	}
	raw, err := os.ReadFile(path + ".search")
	if err != nil {
		return nil, err
	}
	raw, err = docDecompress(raw)
	if err != nil {
		return nil, err
	}
	var idx domain.DocReadIndex
	if err = json.Unmarshal(raw, &idx); err != nil {
		return nil, err
	}
	if idx.Hash != hash || idx.Version != 1 {
		return nil, domain.ErrIntegrity
	}
	out := []domain.DocEventIndex{}
	for _, e := range idx.Events {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if e.Index > after && strings.Contains(strings.ToLower(e.Text), q) && e.Text != "" {
			out = append(out, e)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

// Byte trigrams also preserve literal Unicode substring matching. Queries shorter
// than three bytes bypass this accelerator and use the exact search projection.
func searchTrigrams(s string) []uint32 {
	out := make([]uint32, 0, len(s))
	h := fnv.New32a()
	for i := 0; i+3 <= len(s); i++ {
		h.Reset()
		_, _ = h.Write([]byte(s[i : i+3]))
		out = append(out, h.Sum32()%(32768*8))
	}
	return out
}

// BackfillReadIndexes walks doc ownership, including pending and retained bodies.
// It is safe to interrupt; only complete projections are published atomically.
func (s *FSStore) BackfillReadIndexes(ctx context.Context, progress func(int)) error {
	repos, err := os.ReadDir(filepath.Join(s.dataDir, "repos"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	n := 0
	for _, entry := range repos {
		repo, ok := hashFromObjectName(entry.Name())
		if !ok || !entry.IsDir() {
			continue
		}
		docs, err := os.ReadDir(filepath.Join(s.repoDir(repo), "objects", "docs"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, doc := range docs {
			hash, ok := hashFromObjectName(doc.Name())
			if !ok || doc.IsDir() {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := s.DocReadIndex(ctx, repo, hash); err != nil {
				return err
			}
			n++
			progress(n)
		}
	}
	return nil
}
