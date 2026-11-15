package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// The control lives outside historical-backfill on purpose: any file in the
// queue directory counts as tracking intent for first-tracking and capture
// bootstrap checks, while a pause or a hold is only a schedule.
func (s *FileStore) historicalUploadControlPath(repo string) (string, error) {
	if err := validateHashes(domain.ContentHash(repo)); err != nil {
		return "", err
	}
	return filepath.Join(s.storeDir(), "historical-uploads", hexOf(domain.ContentHash(repo))+".json"), nil
}

func (s *FileStore) HistoricalUploadControl(ctx context.Context, repo string) (domain.HistoricalUploadControl, error) {
	if err := ctx.Err(); err != nil {
		return domain.HistoricalUploadControl{}, err
	}
	path, err := s.historicalUploadControlPath(repo)
	if err != nil {
		return domain.HistoricalUploadControl{}, err
	}
	raw, err := readCxtFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return domain.HistoricalUploadControl{Version: 1}, nil
	}
	if err != nil {
		return domain.HistoricalUploadControl{}, err
	}
	var control domain.HistoricalUploadControl
	if err := json.Unmarshal(raw, &control); err != nil {
		return domain.HistoricalUploadControl{}, domain.ErrHashMismatch
	}
	if err := control.Validate(); err != nil {
		return domain.HistoricalUploadControl{}, err
	}
	return control, nil
}

// UpdateHistoricalUploadControl applies change to the current control under an
// exclusive lock, so a pause set by the user is never lost to a worker that is
// recording a hold at the same moment.
func (s *FileStore) UpdateHistoricalUploadControl(ctx context.Context, repo string, change func(domain.HistoricalUploadControl) domain.HistoricalUploadControl) (domain.HistoricalUploadControl, error) {
	var next domain.HistoricalUploadControl
	_, err := s.withOSLock(ctx, "historical-backfill", "control", syscall.LOCK_EX, true, func() error {
		current, err := s.HistoricalUploadControl(ctx, repo)
		if err != nil {
			return err
		}
		next = change(current)
		next.Version = 1
		if err := next.Validate(); err != nil {
			return err
		}
		path, err := s.historicalUploadControlPath(repo)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return err
		}
		return writeAtomic(path, raw)
	})
	return next, err
}
