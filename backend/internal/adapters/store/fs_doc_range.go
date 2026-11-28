package store

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
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
