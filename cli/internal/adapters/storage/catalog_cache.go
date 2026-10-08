package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

const (
	catalogCacheNamespace        = "catalog-cache"
	catalogCacheHeadFormat       = "cxt.catalog-cache.head"
	catalogCachePageFormat       = "cxt.catalog-cache.page"
	catalogCacheImageFormat      = "cxt.catalog-cache.image"
	catalogCacheCompleteFormat   = "cxt.catalog-cache.complete"
	catalogCacheImageSize        = 256
	catalogCacheInitialThreshold = 128
)

// The mutable head has constant fields, independent of received page count.
// Completed indexes live in an immutable descriptor; an unfinished run is an
// immutable backward-linked chain. Checkpoint is repeated here so partial
// staging needs neither the descriptor nor its page payloads. Full reads and
// final promotion verify it against the descriptor. Generation prevents ABA.
// This is the first released catalog-cache format; earlier development layouts
// are rejected by strict decoding rather than silently migrated or repaired.
type catalogCacheHeadPayload struct {
	Format       string                    `json:"format"`
	Version      int                       `json:"version"`
	RepoID       string                    `json:"repo_id"`
	Remote       string                    `json:"remote"`
	Completed    domain.ContentHash        `json:"completed"`
	Checkpoint   *domain.CatalogCheckpoint `json:"checkpoint"`
	PendingTail  domain.ContentHash        `json:"pending_tail"`
	PendingCount uint64                    `json:"pending_count"`
	Generation   uint64                    `json:"generation"`
}

// Published only at a completed run, never rewritten for each partial page.
// Image is a compacted complete image; Pages are completed runs since Image.
type catalogCacheComplete struct {
	Format          string                    `json:"format"`
	Version         int                       `json:"version"`
	RepoID          string                    `json:"repo_id"`
	Remote          string                    `json:"remote"`
	Checkpoint      *domain.CatalogCheckpoint `json:"checkpoint"`
	ImageCheckpoint *domain.CatalogCheckpoint `json:"image_checkpoint"`
	Image           []domain.ContentHash      `json:"image"`
	Pages           []domain.ContentHash      `json:"pages"`
	CompactAfter    int                       `json:"compact_after"`
}

type loadedCatalogCache struct {
	head     catalogCacheHead
	complete catalogCacheComplete
	pending  []domain.ContentHash // Chronological, verified chain references.
	value    outbound.CatalogCache
}

type catalogCacheHead struct {
	catalogCacheHeadPayload
	Revision domain.ContentHash `json:"revision"`
}

// Received pages retain their exact semantic payload, including opaque cursors.
// Image records are produced only by compaction of a validated complete image.
type catalogCacheRecord struct {
	Format   string                `json:"format"`
	Version  int                   `json:"version"`
	RepoID   string                `json:"repo_id"`
	Remote   string                `json:"remote"`
	Previous domain.ContentHash    `json:"previous"`
	Index    uint64                `json:"index"`
	Page     *domain.CatalogPage   `json:"page"`
	Entries  []domain.CatalogEntry `json:"entries"`
}

func (s *FileStore) catalogCachePath(repo, remote string) (string, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repo)); err != nil {
		return "", err
	}
	if remote == "" {
		return "", domain.ErrInvalidRef
	}
	key := domain.HashContent([]byte(repo + "\x00" + remote))
	return filepath.Join(s.storeDir(), catalogCacheNamespace, hexOf(key)+".json"), nil
}

func (s *FileStore) catalogCacheRecordPath(hash domain.ContentHash) string {
	return filepath.Join(s.storeDir(), catalogCacheNamespace, "pages", hexOf(hash)+".json")
}

func (s *FileStore) catalogCacheCompletePath(hash domain.ContentHash) string {
	return filepath.Join(s.storeDir(), catalogCacheNamespace, "completed", hexOf(hash)+".json")
}

func catalogCacheRevision(head catalogCacheHeadPayload) (domain.ContentHash, error) {
	raw, err := json.Marshal(head)
	if err != nil {
		return "", err
	}
	return domain.HashContent(raw), nil
}

func sameCatalogCheckpoint(a, b *domain.CatalogCheckpoint) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func (s *FileStore) readCatalogCacheHead(ctx context.Context, repo, remote string) (catalogCacheHead, error) {
	if err := ctx.Err(); err != nil {
		return catalogCacheHead{}, err
	}
	path, err := s.catalogCachePath(repo, remote)
	if err != nil {
		return catalogCacheHead{}, err
	}
	raw, err := readCxtFile(path)
	if err := ctx.Err(); err != nil {
		return catalogCacheHead{}, err
	}
	if errors.Is(err, os.ErrNotExist) {
		return catalogCacheHead{catalogCacheHeadPayload: catalogCacheHeadPayload{
			Format: catalogCacheHeadFormat, Version: 1, RepoID: repo, Remote: remote,
		}}, nil
	}
	if err != nil {
		return catalogCacheHead{}, err
	}
	var head catalogCacheHead
	if decodeMetadataCheckpoint(raw, &head) != nil || head.Format != catalogCacheHeadFormat || head.Version != 1 || head.RepoID != repo || head.Remote != remote || head.Generation == 0 {
		return catalogCacheHead{}, domain.ErrHashMismatch
	}
	if head.Checkpoint != nil && domain.ValidateCatalogCheckpoint(repo, *head.Checkpoint) != nil {
		return catalogCacheHead{}, domain.ErrHashMismatch
	}
	if (head.Checkpoint == nil) != (head.Completed == "") || (head.PendingCount == 0) != (head.PendingTail == "") || head.PendingCount > head.Generation {
		return catalogCacheHead{}, domain.ErrHashMismatch
	}
	if domain.ValidateOptionalContentHash(head.Completed) != nil || domain.ValidateOptionalContentHash(head.PendingTail) != nil {
		return catalogCacheHead{}, domain.ErrHashMismatch
	}
	revision, err := catalogCacheRevision(head.catalogCacheHeadPayload)
	if err != nil {
		return catalogCacheHead{}, err
	}
	if revision != head.Revision {
		return catalogCacheHead{}, domain.ErrHashMismatch
	}
	return head, nil
}

func (s *FileStore) readCatalogCacheRecord(ctx context.Context, repo, remote string, hash domain.ContentHash) (catalogCacheRecord, error) {
	if err := ctx.Err(); err != nil {
		return catalogCacheRecord{}, err
	}
	if domain.ValidateContentHash(hash) != nil {
		return catalogCacheRecord{}, domain.ErrHashMismatch
	}
	raw, err := readCxtFile(s.catalogCacheRecordPath(hash))
	if err := ctx.Err(); err != nil {
		return catalogCacheRecord{}, err
	}
	if errors.Is(err, os.ErrNotExist) {
		return catalogCacheRecord{}, domain.ErrHashMismatch
	}
	if err != nil {
		return catalogCacheRecord{}, err
	}
	var record catalogCacheRecord
	if domain.HashContent(raw) != hash || decodeMetadataCheckpoint(raw, &record) != nil || record.Version != 1 || record.RepoID != repo || record.Remote != remote {
		return catalogCacheRecord{}, domain.ErrHashMismatch
	}
	switch record.Format {
	case catalogCachePageFormat:
		if record.Page == nil || record.Entries != nil || record.Index == 0 || (record.Index == 1) != (record.Previous == "") || domain.ValidateOptionalContentHash(record.Previous) != nil {
			return catalogCacheRecord{}, domain.ErrHashMismatch
		}
	case catalogCacheImageFormat:
		if record.Page != nil || len(record.Entries) == 0 || len(record.Entries) > catalogCacheImageSize || record.Previous != "" || record.Index != 0 {
			return catalogCacheRecord{}, domain.ErrHashMismatch
		}
	default:
		return catalogCacheRecord{}, domain.ErrHashMismatch
	}
	return record, nil
}

func (s *FileStore) storeCatalogCacheRecord(ctx context.Context, record catalogCacheRecord) (domain.ContentHash, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	hash := domain.HashContent(raw)
	if err := writeCatalogCacheImmutable(ctx, s.catalogCacheRecordPath(hash), raw); err != nil {
		return "", err
	}
	return hash, nil
}

func writeCatalogCacheImmutable(ctx context.Context, path string, raw []byte) error {
	existing, err := readCxtFile(path)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err == nil {
		if !bytes.Equal(existing, raw) {
			return domain.ErrHashMismatch
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := writeAtomic(path, raw); err != nil {
		return err
	}
	return nil
}

func (s *FileStore) storeCatalogCacheComplete(ctx context.Context, complete catalogCacheComplete) (domain.ContentHash, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(complete)
	if err != nil {
		return "", err
	}
	hash := domain.HashContent(raw)
	if err := writeCatalogCacheImmutable(ctx, s.catalogCacheCompletePath(hash), raw); err != nil {
		return "", err
	}
	return hash, nil
}

func (s *FileStore) readCatalogCacheComplete(ctx context.Context, head catalogCacheHead) (catalogCacheComplete, error) {
	if err := ctx.Err(); err != nil {
		return catalogCacheComplete{}, err
	}
	if head.Completed == "" {
		return catalogCacheComplete{Format: catalogCacheCompleteFormat, Version: 1, RepoID: head.RepoID, Remote: head.Remote, CompactAfter: catalogCacheInitialThreshold}, nil
	}
	raw, err := readCxtFile(s.catalogCacheCompletePath(head.Completed))
	if err := ctx.Err(); err != nil {
		return catalogCacheComplete{}, err
	}
	if errors.Is(err, os.ErrNotExist) {
		return catalogCacheComplete{}, domain.ErrHashMismatch
	}
	if err != nil {
		return catalogCacheComplete{}, err
	}
	var complete catalogCacheComplete
	if domain.HashContent(raw) != head.Completed || decodeMetadataCheckpoint(raw, &complete) != nil || complete.Format != catalogCacheCompleteFormat || complete.Version != 1 || complete.RepoID != head.RepoID || complete.Remote != head.Remote || complete.CompactAfter < catalogCacheInitialThreshold || !sameCatalogCheckpoint(complete.Checkpoint, head.Checkpoint) {
		return catalogCacheComplete{}, domain.ErrHashMismatch
	}
	if complete.ImageCheckpoint == nil && len(complete.Image) != 0 {
		return catalogCacheComplete{}, domain.ErrHashMismatch
	}
	if cp := complete.ImageCheckpoint; cp != nil && (domain.ValidateCatalogCheckpoint(head.RepoID, *cp) != nil || cp.Epoch != head.Checkpoint.Epoch || cp.Sequence > head.Checkpoint.Sequence) {
		return catalogCacheComplete{}, domain.ErrHashMismatch
	}
	for _, list := range [][]domain.ContentHash{complete.Image, complete.Pages} {
		for _, hash := range list {
			if err := ctx.Err(); err != nil {
				return catalogCacheComplete{}, err
			}
			if domain.ValidateContentHash(hash) != nil {
				return catalogCacheComplete{}, domain.ErrHashMismatch
			}
		}
	}
	return complete, nil
}

// Walk backwards without preallocating from an untrusted count. Exact indexes,
// the terminating link and a visited set detect missing, repeated or reordered
// records. Only verified records are reversed for chronological validation.
func (s *FileStore) readCatalogCachePending(ctx context.Context, head catalogCacheHead) ([]domain.ContentHash, []domain.CatalogPage, error) {
	var hashes []domain.ContentHash
	var pages []domain.CatalogPage
	seen := make(map[domain.ContentHash]bool)
	hash, remaining := head.PendingTail, head.PendingCount
	for hash != "" {
		if remaining == 0 || seen[hash] {
			return nil, nil, domain.ErrHashMismatch
		}
		seen[hash] = true
		record, err := s.readCatalogCacheRecord(ctx, head.RepoID, head.Remote, hash)
		if err != nil {
			return nil, nil, err
		}
		if record.Format != catalogCachePageFormat || record.Page.Checkpoint != nil || record.Index != remaining {
			return nil, nil, domain.ErrHashMismatch
		}
		hashes = append(hashes, hash)
		pages = append(pages, *record.Page)
		hash = record.Previous
		remaining--
	}
	if remaining != 0 {
		return nil, nil, domain.ErrHashMismatch
	}
	slices.Reverse(hashes)
	slices.Reverse(pages)
	return hashes, pages, ctx.Err()
}

func catalogCacheEntryLess(a, b domain.CatalogEntry) bool {
	return a.Kind < b.Kind || a.Kind == b.Kind && a.Key < b.Key
}

func applyCatalogCacheEntries(image map[string]domain.CatalogEntry, entries []domain.CatalogEntry) {
	for _, entry := range entries {
		key := domain.CatalogEntryIdentity(entry)
		if entry.Deleted {
			delete(image, key)
		} else {
			image[key] = entry
		}
	}
}

func sortedCatalogCacheEntries(image map[string]domain.CatalogEntry) []domain.CatalogEntry {
	entries := make([]domain.CatalogEntry, 0, len(image))
	for _, entry := range image {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return catalogCacheEntryLess(entries[i], entries[j]) })
	return entries
}

// The domain owns page order and checkpoint semantics. The adapter also rejects
// a repeated opaque cursor anywhere in a run, so a restart cannot loop forever.
func validateCatalogCachePage(repo string, after *domain.CatalogCheckpoint, previous *domain.CatalogPage, page domain.CatalogPage, cursors map[string]bool, seen map[string]int64) error {
	if err := domain.ValidateCatalogPage(repo, after, previous, page); err != nil {
		return err
	}
	for _, entry := range page.Entries {
		key := domain.CatalogEntryIdentity(entry)
		if sequence, exists := seen[key]; exists && (page.Mode == "baseline" || sequence == entry.Sequence) {
			return domain.ErrHashMismatch
		}
		seen[key] = entry.Sequence
	}
	if page.NextCursor != "" {
		if cursors[page.NextCursor] {
			return domain.ErrHashMismatch
		}
		cursors[page.NextCursor] = true
	}
	return nil
}

// Loading validates all referenced current bytes, including the completed image
// when a replacement baseline is in progress. Final promotion and reset use
// this path; neither may silently repair a corrupt old image. Partial staging
// checks only the previous tail and cannot acknowledge the image.
func (s *FileStore) loadCatalogCache(ctx context.Context, repo, remote string) (loadedCatalogCache, error) {
	head, err := s.readCatalogCacheHead(ctx, repo, remote)
	if err != nil {
		return loadedCatalogCache{}, err
	}
	complete, err := s.readCatalogCacheComplete(ctx, head)
	if err != nil {
		return loadedCatalogCache{}, err
	}
	value := outbound.CatalogCache{Version: 1, RepoID: repo, Remote: remote, Revision: head.Revision, Checkpoint: head.Checkpoint}
	image := make(map[string]domain.CatalogEntry)
	var lastImageEntry *domain.CatalogEntry
	for _, hash := range complete.Image {
		record, err := s.readCatalogCacheRecord(ctx, repo, remote, hash)
		if err != nil {
			return loadedCatalogCache{}, err
		}
		if record.Format != catalogCacheImageFormat {
			return loadedCatalogCache{}, domain.ErrHashMismatch
		}
		for _, entry := range record.Entries {
			if err := ctx.Err(); err != nil {
				return loadedCatalogCache{}, err
			}
			if domain.ValidateCatalogEntry(repo, entry) != nil || entry.Deleted || entry.Sequence > complete.ImageCheckpoint.Sequence || lastImageEntry != nil && !catalogCacheEntryLess(*lastImageEntry, entry) {
				return loadedCatalogCache{}, domain.ErrHashMismatch
			}
			lastImageEntry = &entry
			key := domain.CatalogEntryIdentity(entry)
			if _, exists := image[key]; exists {
				return loadedCatalogCache{}, domain.ErrHashMismatch
			}
			image[key] = entry
		}
	}
	checkpoint := complete.ImageCheckpoint
	var previous *domain.CatalogPage
	var after *domain.CatalogCheckpoint
	cursors := make(map[string]bool)
	seen := make(map[string]int64)
	var previousHash domain.ContentHash
	var index uint64
	for _, hash := range complete.Pages {
		record, err := s.readCatalogCacheRecord(ctx, repo, remote, hash)
		if err != nil {
			return loadedCatalogCache{}, err
		}
		if record.Format != catalogCachePageFormat {
			return loadedCatalogCache{}, domain.ErrHashMismatch
		}
		page := record.Page
		if previous == nil {
			previousHash = ""
			index = 0
		}
		index++
		if record.Previous != previousHash || record.Index != index {
			return loadedCatalogCache{}, domain.ErrHashMismatch
		}
		previousHash = hash
		if previous == nil {
			after = checkpoint
			cursors = make(map[string]bool)
			seen = make(map[string]int64)
		}
		if err := validateCatalogCachePage(repo, after, previous, *page, cursors, seen); err != nil {
			return loadedCatalogCache{}, err
		}
		applyCatalogCacheEntries(image, page.Entries)
		previous = page
		if page.Checkpoint != nil {
			checkpoint = page.Checkpoint
			previous = nil
		}
	}
	if previous != nil || !sameCatalogCheckpoint(checkpoint, head.Checkpoint) {
		return loadedCatalogCache{}, domain.ErrHashMismatch
	}
	if head.Checkpoint != nil {
		value.Entries = sortedCatalogCacheEntries(image)
		if _, _, err := domain.CatalogManifest(repo, value.Entries); err != nil {
			return loadedCatalogCache{}, err
		}
	}
	previous = nil
	after = head.Checkpoint
	cursors = make(map[string]bool)
	seen = make(map[string]int64)
	pendingHashes, pending, err := s.readCatalogCachePending(ctx, head)
	if err != nil {
		return loadedCatalogCache{}, err
	}
	for i := range pending {
		page := &pending[i]
		if previous == nil && page.Mode == "baseline" {
			after = nil
		}
		if err := validateCatalogCachePage(repo, after, previous, *page, cursors, seen); err != nil {
			return loadedCatalogCache{}, err
		}
		previous = page
	}
	value.Pending = pending
	if err := ctx.Err(); err != nil {
		return loadedCatalogCache{}, err
	}
	return loadedCatalogCache{head: head, complete: complete, pending: pendingHashes, value: value}, nil
}

func (s *FileStore) ReadCatalogCache(ctx context.Context, repo, remote string) (outbound.CatalogCache, error) {
	loaded, err := s.loadCatalogCache(ctx, repo, remote)
	return loaded.value, err
}

func (s *FileStore) publishCatalogCacheHead(ctx context.Context, path string, head catalogCacheHead) (domain.ContentHash, error) {
	if head.Generation == ^uint64(0) {
		return "", domain.ErrHashMismatch
	}
	head.Generation++
	var err error
	head.Revision, err = catalogCacheRevision(head.catalogCacheHeadPayload)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(head)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := writeAtomic(path, raw); err != nil {
		return "", err
	}
	return head.Revision, nil
}

func (s *FileStore) AppendCatalogPage(ctx context.Context, expected domain.ContentHash, repo, remote string, page domain.CatalogPage) (domain.ContentHash, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path, err := s.catalogCachePath(repo, remote)
	if err != nil {
		return "", err
	}
	if err := domain.ValidateOptionalContentHash(expected); err != nil {
		return "", err
	}
	var revision domain.ContentHash
	err = s.withMutationLock(ctx, catalogCacheNamespace, filepath.Base(path), func() error {
		var head catalogCacheHead
		var current outbound.CatalogCache
		var complete catalogCacheComplete
		var pending []domain.ContentHash
		var err error
		if page.Checkpoint != nil {
			// Final acknowledgement validates every current referenced byte under the
			// CAS lock, including descriptor/checkpoint agreement and the entire run.
			var loaded loadedCatalogCache
			loaded, err = s.loadCatalogCache(ctx, repo, remote)
			head, current, complete, pending = loaded.head, loaded.value, loaded.complete, loaded.pending
		} else {
			// O(1) metadata and at most one received page, regardless of pending count
			// or prior completed image size. No descriptor is read on this path.
			head, err = s.readCatalogCacheHead(ctx, repo, remote)
			if err == nil && head.Revision == expected {
				current.Checkpoint = head.Checkpoint
				if head.PendingCount > 0 {
					var record catalogCacheRecord
					record, err = s.readCatalogCacheRecord(ctx, repo, remote, head.PendingTail)
					if err == nil {
						if record.Format != catalogCachePageFormat || record.Page.Checkpoint != nil || record.Index != head.PendingCount {
							return domain.ErrHashMismatch
						}
						current.Pending = []domain.CatalogPage{*record.Page}
					}
				}
			}
		}
		if err != nil {
			return err
		}
		if head.Revision != expected {
			return domain.ErrSyncConflict
		}
		if head.Generation == ^uint64(0) {
			return domain.ErrHashMismatch
		}
		after := current.Checkpoint
		var previous *domain.CatalogPage
		cursors := make(map[string]bool, len(current.Pending))
		seen := make(map[string]int64)
		if len(current.Pending) > 0 {
			previous = &current.Pending[len(current.Pending)-1]
			if current.Pending[0].Mode == "baseline" {
				after = nil
			}
			for _, p := range current.Pending {
				cursors[p.NextCursor] = true
				for _, entry := range p.Entries {
					seen[domain.CatalogEntryIdentity(entry)] = entry.Sequence
				}
			}
		} else if page.Mode == "baseline" {
			after = nil
		}
		if err := validateCatalogCachePage(repo, after, previous, page, cursors, seen); err != nil {
			return err
		}
		var entries []domain.CatalogEntry
		if page.Checkpoint != nil {
			image := make(map[string]domain.CatalogEntry)
			if page.Mode == "delta" {
				applyCatalogCacheEntries(image, current.Entries)
			}
			for _, p := range current.Pending {
				applyCatalogCacheEntries(image, p.Entries)
			}
			applyCatalogCacheEntries(image, page.Entries)
			entries = sortedCatalogCacheEntries(image)
			// A valid envelope may still omit protocol or delete required metadata.
			if _, _, err := domain.CatalogManifest(repo, entries); err != nil {
				return err
			}
		}
		hash, err := s.storeCatalogCacheRecord(ctx, catalogCacheRecord{
			Format: catalogCachePageFormat, Version: 1, RepoID: repo, Remote: remote,
			Previous: head.PendingTail, Index: head.PendingCount + 1, Page: &page,
		})
		if err != nil {
			return err
		}
		if page.Checkpoint == nil {
			head.PendingTail = hash
			head.PendingCount++
		} else {
			unchanged := head.PendingCount == 0 && page.Mode == "delta" && len(page.Entries) == 0 && sameCatalogCheckpoint(head.Checkpoint, page.Checkpoint)
			if !unchanged {
				if page.Mode == "baseline" {
					complete = catalogCacheComplete{Format: catalogCacheCompleteFormat, Version: 1, RepoID: repo, Remote: remote, CompactAfter: catalogCacheInitialThreshold}
				}
				complete.Pages = append(complete.Pages, pending...)
				complete.Pages = append(complete.Pages, hash)
				complete.Checkpoint = page.Checkpoint
				if len(complete.Image)+len(complete.Pages) >= complete.CompactAfter {
					complete.Image = nil
					for start := 0; start < len(entries); start += catalogCacheImageSize {
						hash, err := s.storeCatalogCacheRecord(ctx, catalogCacheRecord{
							Format: catalogCacheImageFormat, Version: 1, RepoID: repo, Remote: remote,
							Entries: entries[start:min(start+catalogCacheImageSize, len(entries))],
						})
						if err != nil {
							return err
						}
						complete.Image = append(complete.Image, hash)
					}
					complete.ImageCheckpoint = complete.Checkpoint
					complete.Pages = nil
					complete.CompactAfter = max(catalogCacheInitialThreshold, 2*len(complete.Image))
				}
				head.Completed, err = s.storeCatalogCacheComplete(ctx, complete)
				if err != nil {
					return err
				}
			}
			head.Checkpoint = page.Checkpoint
			head.PendingTail = ""
			head.PendingCount = 0
		}
		revision, err = s.publishCatalogCacheHead(ctx, path, head)
		return err
	})
	if err != nil {
		return "", err
	}
	return revision, nil
}

func (s *FileStore) ResetCatalogRun(ctx context.Context, expected domain.ContentHash, repo, remote string) (domain.ContentHash, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path, err := s.catalogCachePath(repo, remote)
	if err != nil {
		return "", err
	}
	if err := domain.ValidateOptionalContentHash(expected); err != nil {
		return "", err
	}
	var revision domain.ContentHash
	err = s.withMutationLock(ctx, catalogCacheNamespace, filepath.Base(path), func() error {
		loaded, err := s.loadCatalogCache(ctx, repo, remote)
		head := loaded.head
		if err != nil {
			return err
		}
		if head.Revision != expected {
			return domain.ErrSyncConflict
		}
		head.PendingTail = ""
		head.PendingCount = 0
		revision, err = s.publishCatalogCacheHead(ctx, path, head)
		return err
	})
	if err != nil {
		return "", err
	}
	return revision, nil
}

var _ outbound.CatalogCacheStore = (*FileStore)(nil)
