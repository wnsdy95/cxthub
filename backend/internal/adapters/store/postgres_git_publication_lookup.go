//go:build postgres

package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.GitPublicationLookupStore = (*PostgresStore)(nil)

const gitBranchBindingsQueryPG = `SELECT repo_id,id,event FROM context_history WHERE repo_id=$1 AND event->>'kind' IN ('birth','orphan','rename','archive') ORDER BY id`

const gitAdmissionRefsQueryPG = `SELECT kind,name,repo_id,COALESCE(target,''),symbolic,branch_id FROM refs WHERE repo_id=$1 AND kind='branch' ORDER BY name`

const gitPublicationWitnessesQueryPG = `SELECT repo_id,id,event FROM context_history
 WHERE repo_id=$1 AND event->>'kind' IN ('birth','attach','position','advance')
 AND COALESCE(event->>'branch_id','')=$2 AND COALESCE(event->>'branch','')=$3
 AND COALESCE(event->>'local_branch','')=$4 AND COALESCE(event->>'worktree_id','')=$5
 AND digest(COALESCE(event->>'branch',''),'sha256')=digest($3::text,'sha256')
 AND digest(COALESCE(event->>'local_branch',''),'sha256')=digest($4::text,'sha256')
 AND COALESCE(event->>'target','')=$6 AND COALESCE(event->>'git_after','')=$7
 ORDER BY id LIMIT $8`

const gitForeignHeadOwnerBeforeQueryPG = `SELECT repo_id,id,event FROM context_history
 WHERE repo_id=$1 AND event->>'kind' IN ('birth','attach','position','advance')
 AND COALESCE(event->>'target','')<>'' AND COALESCE(event->>'git_after','')=$2
 AND COALESCE(event->>'branch_id','')<$3 ORDER BY COALESCE(event->>'branch_id',''),id LIMIT 1`

const gitForeignHeadOwnerAfterQueryPG = `SELECT repo_id,id,event FROM context_history
 WHERE repo_id=$1 AND event->>'kind' IN ('birth','attach','position','advance')
 AND COALESCE(event->>'target','')<>'' AND COALESCE(event->>'git_after','')=$2
 AND COALESCE(event->>'branch_id','')>$3 ORDER BY COALESCE(event->>'branch_id',''),id LIMIT 1`

func (s *PostgresStore) withinGitPublicationLookup(ctx context.Context, repo domain.ContentHash, fn func(context.Context, pgDatabase) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if domain.ValidateContentHash(repo) != nil {
		return domain.ErrValidation
	}
	return s.WithinReadSnapshot(ctx, func(read context.Context) error {
		transaction, owned := read.Value(repositoryTxKey{}).(*repositoryTx)
		if !owned || transaction.owner != s || (!transaction.readOnly && transaction.repo != repo) {
			return domain.ErrConflict
		}
		if err := s.checkRepositoryDocumentIdentity(read, transaction, repo, !transaction.readOnly); err != nil {
			return err
		}
		return fn(read, transaction)
	})
}

func validGitHistoryEventIDPG(id string) bool {
	if len(id) != 32 || strings.ToLower(id) != id {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func scanGitHistoryEventPG(row interface{ Scan(...any) error }, repo domain.ContentHash) (domain.HistoryEvent, error) {
	var storedRepo domain.ContentHash
	var id string
	var raw []byte
	if err := row.Scan(&storedRepo, &id, &raw); err != nil {
		return domain.HistoryEvent{}, mapNoRows(err)
	}
	var event domain.HistoryEvent
	if json.Unmarshal(raw, &event) != nil || domain.ValidateHistoryEvent(event) != nil || storedRepo != repo || event.RepoID != string(repo) || event.ID != id {
		return domain.HistoryEvent{}, domain.ErrIntegrity
	}
	return event, nil
}

func readGitHistoryEventPG(ctx context.Context, database pgDatabase, repo domain.ContentHash, id string) (domain.HistoryEvent, error) {
	event, err := scanGitHistoryEventPG(database.QueryRow(ctx, `SELECT repo_id,id,event FROM context_history WHERE repo_id=$1 AND id=$2`, repo, id), repo)
	if err != nil {
		return domain.HistoryEvent{}, err
	}
	if event.ID != id {
		return domain.HistoryEvent{}, domain.ErrIntegrity
	}
	return event, nil
}

func (s *PostgresStore) GetGitHistoryEvent(ctx context.Context, repo domain.ContentHash, id string) (domain.HistoryEvent, error) {
	if err := ctx.Err(); err != nil {
		return domain.HistoryEvent{}, err
	}
	if !validGitHistoryEventIDPG(id) {
		return domain.HistoryEvent{}, domain.ErrValidation
	}
	var event domain.HistoryEvent
	err := s.withinGitPublicationLookup(ctx, repo, func(read context.Context, database pgDatabase) error {
		var err error
		event, err = readGitHistoryEventPG(read, database, repo, id)
		return err
	})
	if err != nil {
		return domain.HistoryEvent{}, err
	}
	return event, nil
}

func (s *PostgresStore) ListGitAdmissionRefs(ctx context.Context, repo domain.ContentHash) ([]domain.Ref, error) {
	result := []domain.Ref{}
	err := s.withinGitPublicationLookup(ctx, repo, func(read context.Context, database pgDatabase) error {
		metadata, err := s.GetRepo(read, repo)
		if err != nil {
			return err
		}
		rows, err := database.Query(read, gitAdmissionRefsQueryPG, repo)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err := read.Err(); err != nil {
				return err
			}
			var ref domain.Ref
			if err := rows.Scan(&ref.Kind, &ref.Name, &ref.RepoID, &ref.Target, &ref.Symbolic, &ref.BranchID); err != nil {
				return err
			}
			if ref.RepoID != repo || ref.Kind != domain.RefBranch || domain.ValidateRef(ref) != nil {
				return domain.ErrIntegrity
			}
			result = append(result, ref)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		if metadata.ContextProtocol == 1 {
			return nil
		}
		visible := result[:0]
		for _, ref := range result {
			lifecycle, err := listBranchLifecycleRefs(read, database, repo, ref.Name)
			if err != nil {
				return err
			}
			latest, exists, err := domain.LatestBranchLifecycle(lifecycle, ref.Name)
			if err != nil {
				return err
			}
			if !exists || latest.State != domain.BranchArchived || latest.Target != ref.Target {
				visible = append(visible, ref)
			}
		}
		result = visible
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *PostgresStore) ListGitBranchBindings(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	result := []domain.HistoryEvent{}
	err := s.withinGitPublicationLookup(ctx, repo, func(read context.Context, database pgDatabase) error {
		rows, err := database.Query(read, gitBranchBindingsQueryPG, repo)
		if err != nil {
			return err
		}
		defer rows.Close()
		previous := ""
		for rows.Next() {
			if err := read.Err(); err != nil {
				return err
			}
			event, err := scanGitHistoryEventPG(rows, repo)
			if err != nil {
				return err
			}
			if !domain.IsBranchBindingEvent(event) || event.ID <= previous {
				return domain.ErrIntegrity
			}
			previous = event.ID
			result = append(result, event)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *PostgresStore) ListGitPublicationWitnesses(ctx context.Context, repo domain.ContentHash, query domain.GitPublicationWitnessQuery) ([]domain.HistoryEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if query.Validate() != nil || query.Publication.RepoID != string(repo) {
		return nil, domain.ErrValidation
	}
	result := []domain.HistoryEvent{}
	err := s.withinGitPublicationLookup(ctx, repo, func(read context.Context, database pgDatabase) error {
		publication, err := readGitHistoryEventPG(read, database, repo, query.Publication.ID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(publication, query.Publication) {
			return domain.ErrConflict
		}
		statement := gitPublicationWitnessesQueryPG
		args := []any{repo, publication.BranchID, publication.Branch, publication.LocalBranch, publication.WorktreeID, publication.Target, query.GitHead, domain.MaxGitAdmissionWitnesses}
		if len(query.IDs) != 0 {
			statement = `SELECT repo_id,id,event FROM context_history WHERE repo_id=$1 AND id=ANY($2::text[]) ORDER BY id`
			args = []any{repo, query.IDs}
		}
		rows, err := database.Query(read, statement, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		previous := ""
		for rows.Next() {
			if err := read.Err(); err != nil {
				return err
			}
			event, err := scanGitHistoryEventPG(rows, repo)
			if err != nil {
				return err
			}
			if event.ID <= previous || len(result) == domain.MaxGitAdmissionWitnesses {
				return domain.ErrIntegrity
			}
			if !query.Matches(event) {
				if len(query.IDs) != 0 {
					return domain.ErrConflict
				}
				return domain.ErrIntegrity
			}
			previous = event.ID
			result = append(result, event)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(query.IDs) != 0 {
			if len(result) != len(query.IDs) {
				return domain.ErrConflict
			}
			for index, event := range result {
				if event.ID != query.IDs[index] {
					return domain.ErrConflict
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *PostgresStore) FindGitForeignHeadOwner(ctx context.Context, repo domain.ContentHash, head, branchID string) (domain.HistoryEvent, error) {
	if err := ctx.Err(); err != nil {
		return domain.HistoryEvent{}, err
	}
	if domain.ValidateGitOID(head) != nil || branchID == "" || len(branchID) > 128 {
		return domain.HistoryEvent{}, domain.ErrValidation
	}
	var event domain.HistoryEvent
	err := s.withinGitPublicationLookup(ctx, repo, func(read context.Context, database pgDatabase) error {
		var err error
		event, err = scanGitHistoryEventPG(database.QueryRow(read, gitForeignHeadOwnerBeforeQueryPG, repo, head, branchID), repo)
		if errors.Is(err, domain.ErrNotFound) {
			event, err = scanGitHistoryEventPG(database.QueryRow(read, gitForeignHeadOwnerAfterQueryPG, repo, head, branchID), repo)
		}
		if err != nil {
			return err
		}
		switch event.Kind {
		case "birth", "attach", "position", "advance":
		default:
			return domain.ErrIntegrity
		}
		if event.Target == "" || event.GitAfter != head || event.BranchID == branchID {
			return domain.ErrIntegrity
		}
		return nil
	})
	if err != nil {
		return domain.HistoryEvent{}, err
	}
	return event, nil
}
