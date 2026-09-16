package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type prDelivery struct {
	Repo     string                  `json:"repo"`
	PR       domain.PullRequestMerge `json:"pr"`
	Accepted bool                    `json:"accepted"`
}

func (s *FileStore) prDeliveryPath(repo string, n int) string {
	return filepath.Join(s.storeDir(), "pr-deliveries", fmt.Sprintf("%s-%020d.json", hexOf(domain.ContentHash(repo)), n))
}
func (s *FileStore) mutatePRDelivery(ctx context.Context, repo string, pr domain.PullRequestMerge, accept bool) error {
	if err := domain.ValidateContentHash(domain.ContentHash(repo)); err != nil {
		return err
	}
	if err := pr.Validate(); err != nil {
		return err
	}
	return s.withMutationLock(ctx, "pr-deliveries", "repo", func() error {
		path := s.prDeliveryPath(repo, pr.Number)
		raw, err := readCxtFile(path)
		record := prDelivery{Repo: repo, PR: pr}
		if err == nil {
			if json.Unmarshal(raw, &record) != nil || record.Repo != repo || record.PR != pr {
				return domain.ErrHashMismatch
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if accept {
			record.Accepted = true
		}
		raw, err = json.Marshal(record)
		if err != nil {
			return err
		}
		return writeAtomic(path, raw)
	})
}
func (s *FileStore) QueuePRDelivery(ctx context.Context, repo string, pr domain.PullRequestMerge) error {
	return s.mutatePRDelivery(ctx, repo, pr, false)
}
func (s *FileStore) AcceptPRDelivery(ctx context.Context, repo string, pr domain.PullRequestMerge) error {
	return s.mutatePRDelivery(ctx, repo, pr, true)
}
func (s *FileStore) PendingPRDeliveries(ctx context.Context, repo string) ([]domain.PullRequestMerge, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repo)); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.storeDir(), "pr-deliveries")
	if err := validateCxtDir(dir); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := []domain.PullRequestMerge{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		raw, err := readCxtFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var record prDelivery
		if json.Unmarshal(raw, &record) != nil || record.PR.Validate() != nil {
			return nil, domain.ErrHashMismatch
		}
		if record.Repo == repo && !record.Accepted {
			out = append(out, record.PR)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}
