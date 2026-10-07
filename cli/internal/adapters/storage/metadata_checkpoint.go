package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

const (
	metadataCheckpointNamespace        = "metadata-checkpoints"
	metadataCheckpointHeadFormat       = "cxt.metadata-checkpoint.head"
	metadataCheckpointPageFormat       = "cxt.metadata-checkpoint.page"
	metadataCheckpointPageSize         = 256
	metadataCheckpointInitialThreshold = 128
)

// Revision checksums every head field, including page order, generation and
// the adaptive compaction threshold. Page hashes bind the complete payload.
type metadataCheckpointHeadPayload struct {
	Format       string               `json:"format"`
	Version      int                  `json:"version"`
	RepoID       string               `json:"repo_id"`
	Remote       string               `json:"remote"`
	Pages        []domain.ContentHash `json:"pages"`
	PageCount    int                  `json:"page_count"`
	CompactAfter int                  `json:"compact_after"`
	Generation   uint64               `json:"generation"`
}

type metadataCheckpointHead struct {
	metadataCheckpointHeadPayload
	Revision domain.ContentHash `json:"revision"`
}

type metadataCheckpointPage struct {
	Format    string               `json:"format"`
	Version   int                  `json:"version"`
	RepoID    string               `json:"repo_id"`
	Remote    string               `json:"remote"`
	Snapshots []domain.Snapshot    `json:"snapshots"`
	Removed   []domain.ContentHash `json:"removed"`
}

func (s *FileStore) metadataCheckpointPath(repo, remote string) (string, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repo)); err != nil {
		return "", err
	}
	if remote == "" {
		return "", domain.ErrInvalidRef
	}
	key := domain.HashContent([]byte(repo + "\x00" + remote))
	return filepath.Join(s.storeDir(), metadataCheckpointNamespace, hexOf(key)+".json"), nil
}

func (s *FileStore) metadataCheckpointPagePath(hash domain.ContentHash) string {
	return filepath.Join(s.storeDir(), metadataCheckpointNamespace, "pages", hexOf(hash)+".json")
}

// Both records have a canonical JSON representation. Requiring exact encoding
// also rejects duplicate keys, case aliases, omitted fields and unknown fields.
func decodeMetadataCheckpoint(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil || decoder.Decode(new(json.RawMessage)) != io.EOF {
		return domain.ErrHashMismatch
	}
	canonical, err := json.Marshal(value)
	if err != nil || !bytes.Equal(raw, canonical) {
		return domain.ErrHashMismatch
	}
	return nil
}

func metadataCheckpointHeadRevision(head metadataCheckpointHeadPayload) (domain.ContentHash, error) {
	raw, err := json.Marshal(head)
	if err != nil {
		return "", err
	}
	return domain.HashContent(raw), nil
}

func (s *FileStore) readMetadataCheckpointHead(ctx context.Context, repo, remote string) (metadataCheckpointHead, error) {
	if err := ctx.Err(); err != nil {
		return metadataCheckpointHead{}, err
	}
	path, err := s.metadataCheckpointPath(repo, remote)
	if err != nil {
		return metadataCheckpointHead{}, err
	}
	raw, err := readCxtFile(path)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return metadataCheckpointHead{}, ctxErr
	}
	if errors.Is(err, os.ErrNotExist) {
		return metadataCheckpointHead{metadataCheckpointHeadPayload: metadataCheckpointHeadPayload{
			Format: metadataCheckpointHeadFormat, Version: 1, RepoID: repo, Remote: remote,
			CompactAfter: metadataCheckpointInitialThreshold,
		}}, nil
	}
	if err != nil {
		return metadataCheckpointHead{}, err
	}
	var head metadataCheckpointHead
	if decodeMetadataCheckpoint(raw, &head) != nil || head.Format != metadataCheckpointHeadFormat || head.Version != 1 || head.RepoID != repo || head.Remote != remote ||
		head.Generation == 0 || head.PageCount != len(head.Pages) || head.PageCount < 0 || head.CompactAfter < metadataCheckpointInitialThreshold || head.PageCount > head.CompactAfter {
		return metadataCheckpointHead{}, domain.ErrHashMismatch
	}
	for _, hash := range head.Pages {
		if err := ctx.Err(); err != nil {
			return metadataCheckpointHead{}, err
		}
		if err := domain.ValidateContentHash(hash); err != nil {
			return metadataCheckpointHead{}, err
		}
	}
	revision, err := metadataCheckpointHeadRevision(head.metadataCheckpointHeadPayload)
	if err != nil {
		return metadataCheckpointHead{}, err
	}
	if revision != head.Revision {
		return metadataCheckpointHead{}, domain.ErrHashMismatch
	}
	return head, ctx.Err()
}

func validateMetadataCheckpointBatch(ctx context.Context, repo string, snapshots []domain.Snapshot, removed []domain.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(snapshots)+len(removed) == 0 || len(snapshots) > metadataCheckpointPageSize || len(removed) > metadataCheckpointPageSize-len(snapshots) {
		return domain.ErrHashMismatch
	}
	seen := make(map[domain.ContentHash]bool, len(snapshots))
	for _, snap := range snapshots {
		if err := ctx.Err(); err != nil {
			return err
		}
		if snap.RepoID != repo || seen[snap.ID] {
			return domain.ErrHashMismatch
		}
		if err := validateSnapshotRefs(snap); err != nil {
			return err
		}
		seen[snap.ID] = true
	}
	for _, id := range removed {
		if err := ctx.Err(); err != nil {
			return err
		}
		if domain.ValidateContentHash(id) != nil || seen[id] {
			return domain.ErrHashMismatch
		}
		seen[id] = true
	}
	return nil
}

func encodeMetadataCheckpointPage(ctx context.Context, repo, remote string, snapshots []domain.Snapshot, removed []domain.ContentHash) ([]byte, error) {
	if err := validateMetadataCheckpointBatch(ctx, repo, snapshots, removed); err != nil {
		return nil, err
	}
	ordered := append([]domain.Snapshot(nil), snapshots...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	orderedRemoved := append([]domain.ContentHash(nil), removed...)
	sort.Strings(orderedRemoved)
	return json.Marshal(metadataCheckpointPage{metadataCheckpointPageFormat, 1, repo, remote, ordered, orderedRemoved})
}

func (s *FileStore) readMetadataCheckpointPage(ctx context.Context, repo, remote string, hash domain.ContentHash) (metadataCheckpointPage, error) {
	if err := ctx.Err(); err != nil {
		return metadataCheckpointPage{}, err
	}
	if err := domain.ValidateContentHash(hash); err != nil {
		return metadataCheckpointPage{}, err
	}
	raw, err := readCxtFile(s.metadataCheckpointPagePath(hash))
	if ctxErr := ctx.Err(); ctxErr != nil {
		return metadataCheckpointPage{}, ctxErr
	}
	if errors.Is(err, os.ErrNotExist) {
		return metadataCheckpointPage{}, domain.ErrHashMismatch
	}
	if err != nil {
		return metadataCheckpointPage{}, err
	}
	if domain.HashContent(raw) != hash {
		return metadataCheckpointPage{}, domain.ErrHashMismatch
	}
	var page metadataCheckpointPage
	if decodeMetadataCheckpoint(raw, &page) != nil || page.Format != metadataCheckpointPageFormat || page.Version != 1 || page.RepoID != repo || page.Remote != remote {
		return metadataCheckpointPage{}, domain.ErrHashMismatch
	}
	if err := validateMetadataCheckpointBatch(ctx, repo, page.Snapshots, page.Removed); err != nil {
		return metadataCheckpointPage{}, err
	}
	for i := 1; i < len(page.Snapshots); i++ {
		if page.Snapshots[i-1].ID >= page.Snapshots[i].ID {
			return metadataCheckpointPage{}, domain.ErrHashMismatch
		}
	}
	for i := 1; i < len(page.Removed); i++ {
		if page.Removed[i-1] >= page.Removed[i] {
			return metadataCheckpointPage{}, domain.ErrHashMismatch
		}
	}
	return page, nil
}

func (s *FileStore) storeMetadataCheckpointPage(ctx context.Context, repo, remote string, snapshots []domain.Snapshot, removed []domain.ContentHash) (domain.ContentHash, error) {
	raw, err := encodeMetadataCheckpointPage(ctx, repo, remote, snapshots, removed)
	if err != nil {
		return "", err
	}
	hash := domain.HashContent(raw)
	path := s.metadataCheckpointPagePath(hash)
	existing, err := readCxtFile(path)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	if err == nil {
		// Never overwrite an existing immutable page, even when it is corrupt.
		if domain.HashContent(existing) != hash || !bytes.Equal(existing, raw) {
			return "", domain.ErrHashMismatch
		}
		return hash, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := writeAtomic(path, raw); err != nil {
		return "", err
	}
	return hash, nil
}

func (s *FileStore) mergeMetadataCheckpointPages(ctx context.Context, head metadataCheckpointHead, next []domain.Snapshot, removed []domain.ContentHash) ([]domain.Snapshot, error) {
	merged := make(map[domain.ContentHash]domain.Snapshot)
	for _, hash := range head.Pages {
		page, err := s.readMetadataCheckpointPage(ctx, head.RepoID, head.Remote, hash)
		if err != nil {
			return nil, err
		}
		for _, snap := range page.Snapshots {
			merged[snap.ID] = snap
		}
		for _, id := range page.Removed {
			delete(merged, id)
		}
	}
	for _, snap := range next {
		merged[snap.ID] = snap
	}
	for _, id := range removed {
		delete(merged, id)
	}
	snapshots := make([]domain.Snapshot, 0, len(merged))
	for _, snap := range merged {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snap)
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].ID < snapshots[j].ID })
	return snapshots, ctx.Err()
}

func (s *FileStore) ReadMetadataCheckpoint(ctx context.Context, repo, remote string) (outbound.MetadataCheckpoint, error) {
	head, err := s.readMetadataCheckpointHead(ctx, repo, remote)
	if err != nil {
		return outbound.MetadataCheckpoint{}, err
	}
	value := outbound.MetadataCheckpoint{Version: 1, RepoID: repo, Remote: remote, Revision: head.Revision}
	if head.Revision == "" {
		return value, nil
	}
	value.Snapshots, err = s.mergeMetadataCheckpointPages(ctx, head, nil, nil)
	if err != nil {
		return outbound.MetadataCheckpoint{}, err
	}
	return value, nil
}

func (s *FileStore) AppendMetadataCheckpoint(ctx context.Context, expected domain.ContentHash, repo, remote string, snapshots []domain.Snapshot, removed []domain.ContentHash) (domain.ContentHash, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path, err := s.metadataCheckpointPath(repo, remote)
	if err != nil {
		return "", err
	}
	if err := domain.ValidateOptionalContentHash(expected); err != nil {
		return "", err
	}
	if err := validateMetadataCheckpointBatch(ctx, repo, snapshots, removed); err != nil {
		return "", err
	}
	var revision domain.ContentHash
	err = s.withMutationLock(ctx, metadataCheckpointNamespace, filepath.Base(path), func() error {
		head, err := s.readMetadataCheckpointHead(ctx, repo, remote)
		if err != nil {
			return err
		}
		if head.Revision != expected {
			return domain.ErrSyncConflict
		}
		if head.Generation == ^uint64(0) {
			return domain.ErrHashMismatch
		}
		// A monotonic generation fences old writers even when compaction returns
		// to an identical page list (including an empty live catalog).
		head.Generation++
		page, err := s.storeMetadataCheckpointPage(ctx, repo, remote, snapshots, removed)
		if err != nil {
			return err
		}
		if len(head.Pages) >= head.CompactAfter {
			// Verify the old pages under the head lock before compacting. Retain
			// unreferenced pages: readers of older heads and failed publications
			// remain safe without a concurrent garbage collection protocol.
			merged, err := s.mergeMetadataCheckpointPages(ctx, head, snapshots, removed)
			if err != nil {
				return err
			}
			head.Pages = nil
			for start := 0; start < len(merged); start += metadataCheckpointPageSize {
				page, err := s.storeMetadataCheckpointPage(ctx, repo, remote, merged[start:min(start+metadataCheckpointPageSize, len(merged))], nil)
				if err != nil {
					return err
				}
				head.Pages = append(head.Pages, page)
			}
			head.CompactAfter = max(metadataCheckpointInitialThreshold, 2*len(head.Pages))
		} else {
			head.Pages = append(head.Pages, page)
		}
		head.PageCount = len(head.Pages)
		head.Revision, err = metadataCheckpointHeadRevision(head.metadataCheckpointHeadPayload)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(head)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writeAtomic(path, raw); err != nil {
			return err
		}
		revision = head.Revision
		return nil
	})
	if err != nil {
		return "", err
	}
	return revision, nil
}

var _ outbound.MetadataCheckpointStore = (*FileStore)(nil)
