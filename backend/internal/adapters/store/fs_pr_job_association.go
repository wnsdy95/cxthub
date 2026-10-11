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

var _ outbound.PRJobAssociationStore = (*FSStore)(nil)

func readPRJobAssociationDirectory[Record interface{ Validate() error }](ctx context.Context, directory string, pathFor func(Record) string, selected func(Record) bool) ([]Record, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	dir, err := os.Open(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(outbound.PRJobAssociationReadLimit + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	if len(entries) > outbound.PRJobAssociationReadLimit {
		return nil, false, nil
	}
	var records []Record
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		if !entry.Type().IsRegular() {
			return nil, false, domain.ErrIntegrity
		}
		path := filepath.Join(directory, entry.Name())
		file, err := os.Open(path)
		if err != nil {
			return nil, false, err
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, (32<<10)+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			return nil, false, errors.Join(readErr, closeErr)
		}
		if len(raw) > 32<<10 {
			return nil, false, domain.ErrIntegrity
		}
		record, err := decodePRHistory[Record](raw)
		if err != nil || pathFor(record) != path {
			return nil, false, domain.ErrIntegrity
		}
		if selected(record) {
			records = append(records, record)
		}
	}
	return records, true, ctx.Err()
}

func (s *FSStore) ReadPRJobAssociationEvidence(ctx context.Context, repo domain.ContentHash, numbers []int) (outbound.PRJobAssociationEvidence, error) {
	var result outbound.PRJobAssociationEvidence
	selected, err := prJobAssociationNumbers(repo, numbers)
	if err != nil {
		return result, err
	}
	lock := s.oauthLock()
	lock.Lock()
	defer lock.Unlock()
	result.Checkpoints, result.CheckpointsComplete, err = readPRJobAssociationDirectory(ctx, filepath.Join(s.dataDir, "pr-history-checkpoints"), func(record domain.PRHistoryCheckpoint) string {
		return s.prHistoryCheckpointPath(record.RepoID, record.GitOrigin, record.ProviderRepositoryID, record.PR.Number)
	}, func(record domain.PRHistoryCheckpoint) bool {
		return record.RepoID == repo && selected[record.PR.Number]
	})
	if err != nil {
		return result, err
	}
	result.Participations, result.ParticipationsComplete, err = readPRJobAssociationDirectory(ctx, filepath.Join(s.dataDir, "pr-participations"), func(record domain.PRParticipation) string {
		return s.gitEvidencePath("pr-participations", record.RepoID, record.ID)
	}, func(record domain.PRParticipation) bool { return record.RepoID == repo && selected[record.Number] })
	if err != nil {
		return result, err
	}
	result.Contributions, result.ContributionsComplete, err = readPRJobAssociationDirectory(ctx, filepath.Join(s.dataDir, "pr-contributions"), func(record domain.PRContribution) string {
		return s.gitEvidencePath("pr-contributions", record.RepoID, record.ID)
	}, func(record domain.PRContribution) bool { return record.RepoID == repo && selected[record.PR.Number] })
	return result, err
}
