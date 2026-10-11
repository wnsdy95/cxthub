package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.GitHeadReceiptStore = (*FSStore)(nil)

func (s *FSStore) gitHeadReceiptDirectory(repo domain.ContentHash, origin string) string {
	return filepath.Join(s.dataDir, "git-head-receipts", opaqueName(string(repo)+":"+origin))
}

func (s *FSStore) gitHeadReceiptPath(repo domain.ContentHash, origin string, version int64) string {
	return filepath.Join(s.gitHeadReceiptDirectory(repo, origin), fmt.Sprintf("%020d.json", version))
}

func (s *FSStore) checkGitHeadReceiptFS(receipt domain.GitHeadReceipt) error {
	old, err := readPRHistoryFile[domain.GitHeadReceipt](s.gitHeadReceiptPath(receipt.RepoID, receipt.GitOrigin, receipt.Version))
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(old, receipt) {
		return domain.ErrConflict
	}
	return nil
}

func (s *FSStore) putGitHeadReceiptFS(ctx context.Context, receipt domain.GitHeadReceipt) error {
	if err := receipt.Validate(); err != nil {
		return err
	}
	if err := s.checkGitHeadReceiptFS(receipt); err != nil {
		return err
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeAtomic(s.gitHeadReceiptPath(receipt.RepoID, receipt.GitOrigin, receipt.Version), raw)
}

func (s *FSStore) GetGitHeadReceipt(ctx context.Context, repo domain.ContentHash, origin string, version int64) (domain.GitHeadReceipt, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitHeadReceipt{}, err
	}
	if err := validateGitHeadReceiptScope(repo, origin, version); err != nil || version == 0 {
		return domain.GitHeadReceipt{}, domain.ErrValidation
	}
	lock := s.oauthLock()
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return domain.GitHeadReceipt{}, err
	}
	receipt, err := readPRHistoryFile[domain.GitHeadReceipt](s.gitHeadReceiptPath(repo, origin, version))
	if err != nil {
		return domain.GitHeadReceipt{}, err
	}
	if err := validateGitHeadReceiptIdentity(receipt, repo, origin, version); err != nil {
		return domain.GitHeadReceipt{}, err
	}
	return receipt, nil
}

func (s *FSStore) ListGitHeadReceipts(ctx context.Context, repo domain.ContentHash, origin string, afterVersion int64, limit int) ([]domain.GitHeadReceipt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateGitHeadReceiptScope(repo, origin, afterVersion); err != nil || limit < 1 || limit > 101 {
		return nil, domain.ErrValidation
	}
	lock := s.oauthLock()
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.gitHeadReceiptDirectory(repo, origin))
	result := []domain.GitHeadReceipt{}
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		version, err := strconv.ParseInt(strings.TrimSuffix(entry.Name(), ".json"), 10, 64)
		if err != nil || version <= 0 || entry.Name() != fmt.Sprintf("%020d.json", version) {
			return nil, domain.ErrIntegrity
		}
		if version <= afterVersion {
			continue
		}
		receipt, err := readPRHistoryFile[domain.GitHeadReceipt](s.gitHeadReceiptPath(repo, origin, version))
		if err != nil {
			return nil, err
		}
		if err := validateGitHeadReceiptIdentity(receipt, repo, origin, version); err != nil {
			return nil, err
		}
		result = append(result, receipt)
		if len(result) == limit {
			break
		}
	}
	return result, nil
}
