package store

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (s *FSStore) rangeIndexPath(repo, hash domain.ContentHash) string {
	return filepath.Join(s.repoDir(repo), "doc-range-index-v1", hexOf(hash), "header.json")
}

func (s *FSStore) rangePagePath(repo, hash, page domain.ContentHash) string {
	return filepath.Join(s.repoDir(repo), "doc-range-index-v1", hexOf(hash), hexOf(page)+".json")
}

func readRangeFile(ctx context.Context, path string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, domain.ErrIntegrity
	}
	return raw, ctx.Err()
}

func (s *FSStore) rangeRoot(ctx context.Context, repo, hash domain.ContentHash) (domain.ConversationManifest, error) {
	if err := validateHashes(repo, hash); err != nil {
		return domain.ConversationManifest{}, err
	}
	raw, err := readRangeFile(ctx, s.docPath(repo, hash), maxStoredRangeRootBytes)
	if err != nil {
		return domain.ConversationManifest{}, err
	}
	return rangeRootManifest(ctx, hash, raw)
}

func (s *FSStore) DocRangeIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocRangeIndex, error) {
	if _, err := s.rangeRoot(ctx, repo, hash); err != nil {
		return domain.DocRangeIndex{}, err
	}
	raw, err := readRangeFile(ctx, s.rangeIndexPath(repo, hash), domain.MaxDocRangeIndexBytes)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.DocRangeIndex{}, domain.ErrDocRangeIndexUnavailable
	}
	if err != nil {
		return domain.DocRangeIndex{}, err
	}
	header, err := decodeRangeHeader(hash, raw)
	if err != nil {
		return domain.DocRangeIndex{}, err
	}
	return header, ctx.Err()
}

func (s *FSStore) DocRangePage(ctx context.Context, repo domain.ContentHash, header domain.DocRangeIndex, position int) ([]domain.DocRangeEntry, error) {
	current, err := s.DocRangeIndex(ctx, repo, header.Hash)
	if err != nil {
		return nil, err
	}
	if err := matchRangeHeader(current, header); err != nil {
		return nil, err
	}
	if position < 0 || position >= len(current.Pages) {
		return nil, domain.ErrIntegrity
	}
	raw, err := readRangeFile(ctx, s.rangePagePath(repo, current.Hash, current.Pages[position]), domain.MaxDocRangePageBytes)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, domain.ErrIntegrity
	}
	if err != nil {
		return nil, err
	}
	entries, err := current.DecodePage(position, raw)
	if err != nil {
		return nil, err
	}
	return entries, ctx.Err()
}

func (s *FSStore) putRangeIndex(ctx context.Context, repo domain.ContentHash, index domain.VerifiedDocRangeIndex, publish func() error) error {
	header, raw, err := verifiedRangeHeader(index)
	if err != nil {
		return err
	}
	for position, hash := range header.Pages {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := index.Page(ctx, position)
		if err != nil {
			return err
		}
		if err := writeAtomic(s.rangePagePath(repo, header.Hash, hash), page); err != nil {
			return err
		}
	}
	if err := publish(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeAtomic(s.rangeIndexPath(repo, header.Hash), raw)
}

func (s *FSStore) EnsureDocRangeIndex(ctx context.Context, repo, hash domain.ContentHash) error {
	if _, err := s.DocRangeIndex(ctx, repo, hash); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
		return err
	}
	if outbound.DocReadOnly(ctx) {
		return domain.ErrDocRangeIndexUnavailable
	}
	retention := s.oauthLock()
	retention.Lock()
	defer retention.Unlock()
	if _, err := s.DocRangeIndex(ctx, repo, hash); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
		return err
	}
	manifest, err := s.rangeRoot(ctx, repo, hash)
	if err != nil {
		return err
	}
	spool, err := newDocRangeSpool()
	if err != nil {
		return err
	}
	defer spool.Close()
	index, err := verifyStoredRangeIndex(ctx, &s.docProofs.rootVerifier, hash, manifest, func(ctx context.Context, chunk domain.ContentHash) ([]byte, error) {
		return readRangeFile(ctx, s.chunkPath(repo, chunk), maxStoredRangeChunkBytes)
	}, spool)
	if err != nil {
		return err
	}
	return s.putRangeIndex(ctx, repo, index, func() error {
		_, err := s.rangeRoot(ctx, repo, hash)
		return err
	})
}

var _ outbound.DocRangeStore = (*FSStore)(nil)
