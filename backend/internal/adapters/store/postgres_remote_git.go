//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.RemoteGitStore = (*PostgresStore)(nil)

func decodeRemoteGitState(body []byte, repo domain.ContentHash) (domain.RemoteGitState, error) {
	var state domain.RemoteGitState
	if err := json.Unmarshal(body, &state); err != nil {
		return domain.RemoteGitState{}, fmt.Errorf("%w: invalid stored remote git state: %v", domain.ErrIntegrity, err)
	}
	if err := state.Validate(); err != nil || state.RepoID != repo {
		return domain.RemoteGitState{}, fmt.Errorf("%w: invalid stored remote git state", domain.ErrIntegrity)
	}
	return state, nil
}

func (s *PostgresStore) GetRemoteGitState(ctx context.Context, repo domain.ContentHash) (domain.RemoteGitState, error) {
	if err := validateHash(repo); err != nil {
		return domain.RemoteGitState{}, err
	}
	var body []byte
	if err := s.db(ctx).QueryRow(ctx, `SELECT body FROM remote_git_state WHERE repo_id=$1`, repo).Scan(&body); err != nil {
		return domain.RemoteGitState{}, mapNoRows(err)
	}
	return decodeRemoteGitState(body, repo)
}

// ClaimRemoteGitSync takes the claim with one conditional statement, so that
// of two workers asking at once one gets it. That statement applies the rule
// domain.ClaimRemoteGitPass states: no claim is still held, and the next pass
// is due or what is stored was read from another origin. What follows, inside
// the same transaction, belongs to the worker that got the claim.
func (s *PostgresStore) ClaimRemoteGitSync(ctx context.Context, repo domain.ContentHash, origin string, now time.Time, lease time.Duration) (domain.RemoteGitState, error) {
	if lease <= 0 {
		return domain.RemoteGitState{}, fmt.Errorf("%w: a claim needs a lease", domain.ErrValidation)
	}
	now = now.UTC()
	fresh := domain.NewRemoteGitState(repo, origin, now).Started(now)
	if err := fresh.Validate(); err != nil {
		return domain.RemoteGitState{}, err
	}
	body, err := json.Marshal(fresh)
	if err != nil {
		return domain.RemoteGitState{}, err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return domain.RemoteGitState{}, err
	}
	defer tx.Rollback(ctx)
	var stored []byte
	if err := tx.QueryRow(ctx, `INSERT INTO remote_git_state(repo_id,body,next_at,claimed_until) VALUES($1,$2,$3,$4)
 ON CONFLICT (repo_id) DO UPDATE SET claimed_until=EXCLUDED.claimed_until
 WHERE (remote_git_state.claimed_until IS NULL OR remote_git_state.claimed_until <= $5)
   AND (remote_git_state.next_at <= $5 OR remote_git_state.body->>'git_origin' <> $6)
 RETURNING body`, repo, body, fresh.NextAt, now.Add(lease), now, origin).Scan(&stored); err != nil {
		return domain.RemoteGitState{}, mapNoRows(err)
	}
	state, err := decodeRemoteGitState(stored, repo)
	if err != nil {
		return domain.RemoteGitState{}, err
	}
	if state.GitOrigin != origin {
		// What is stored was read from another origin: none of it is kept.
		for _, statement := range []string{
			`DELETE FROM remote_git_commits WHERE repo_id=$1`,
			`DELETE FROM remote_git_branches WHERE repo_id=$1`,
			`DELETE FROM remote_git_pulls WHERE repo_id=$1`,
		} {
			if _, err := tx.Exec(ctx, statement, repo); err != nil {
				return domain.RemoteGitState{}, err
			}
		}
		state = fresh
	} else {
		state = state.Started(now)
	}
	if body, err = json.Marshal(state); err != nil {
		return domain.RemoteGitState{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE remote_git_state SET body=$2, next_at=$3 WHERE repo_id=$1`, repo, body, state.NextAt); err != nil {
		return domain.RemoteGitState{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.RemoteGitState{}, err
	}
	return state, nil
}

func (s *PostgresStore) FinishRemoteGitSync(ctx context.Context, state domain.RemoteGitState) error {
	if err := state.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tag, err := s.db(ctx).Exec(ctx, `UPDATE remote_git_state SET body=$2, next_at=$3, claimed_until=NULL WHERE repo_id=$1`, state.RepoID, body, state.NextAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	return nil
}

func (s *PostgresStore) RequestRemoteGitSync(ctx context.Context, repo domain.ContentHash, now time.Time) error {
	if err := validateHash(repo); err != nil {
		return err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var body []byte
	if err := tx.QueryRow(ctx, `SELECT body FROM remote_git_state WHERE repo_id=$1 FOR UPDATE`, repo).Scan(&body); errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	state, err := decodeRemoteGitState(body, repo)
	if err != nil {
		return err
	}
	state = state.Request(now)
	if body, err = json.Marshal(state); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE remote_git_state SET body=$2, next_at=$3 WHERE repo_id=$1`, repo, body, state.NextAt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const remoteGitCommitColumns = `stored.sha,stored.parents,stored.author_name,stored.author_email,stored.authored_at,stored.committed_at,stored.subject`

func scanRemoteGitCommits(rows pgx.Rows, err error) ([]domain.RemoteCommit, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	commits := []domain.RemoteCommit{}
	for rows.Next() {
		var commit domain.RemoteCommit
		if err := rows.Scan(&commit.SHA, &commit.Parents, &commit.AuthorName, &commit.AuthorEmail, &commit.AuthoredAt, &commit.CommittedAt, &commit.Subject); err != nil {
			return nil, err
		}
		commit = remoteGitCommit(commit)
		if err := commit.Validate(); err != nil {
			return nil, fmt.Errorf("%w: invalid stored remote commit", domain.ErrIntegrity)
		}
		commits = append(commits, commit)
	}
	return commits, rows.Err()
}

// PutRemoteCommits adds the commits in one statement. A commit that is stored
// already is left as it is.
func (s *PostgresStore) PutRemoteCommits(ctx context.Context, repo domain.ContentHash, commits []domain.RemoteCommit) (int, error) {
	if err := validateHash(repo); err != nil {
		return 0, err
	}
	count := len(commits)
	shas, parents, names, emails, subjects := make([]string, 0, count), make([]string, 0, count), make([]string, 0, count), make([]string, 0, count), make([]string, 0, count)
	authored, committed := make([]time.Time, 0, count), make([]time.Time, 0, count)
	for _, commit := range commits {
		if err := commit.Validate(); err != nil {
			return 0, err
		}
		shas, parents = append(shas, commit.SHA), append(parents, strings.Join(commit.Parents, ","))
		names, emails, subjects = append(names, commit.AuthorName), append(emails, commit.AuthorEmail), append(subjects, commit.Subject)
		authored, committed = append(authored, commit.AuthoredAt.UTC()), append(committed, commit.CommittedAt.UTC())
	}
	if count == 0 {
		return 0, nil
	}
	tag, err := s.db(ctx).Exec(ctx, `INSERT INTO remote_git_commits(repo_id,sha,parents,author_name,author_email,authored_at,committed_at,subject)
 SELECT $1, listed.sha, string_to_array(listed.parents, ','), listed.author_name, listed.author_email, listed.authored_at, listed.committed_at, listed.subject
 FROM unnest($2::text[],$3::text[],$4::text[],$5::text[],$6::timestamptz[],$7::timestamptz[],$8::text[])
  AS listed(sha,parents,author_name,author_email,authored_at,committed_at,subject)
 ON CONFLICT (repo_id,sha) DO NOTHING`, repo, shas, parents, names, emails, authored, committed, subjects)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (s *PostgresStore) MissingRemoteCommits(ctx context.Context, repo domain.ContentHash, shas []string) ([]string, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	asked, err := remoteGitSHAs(shas)
	if err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT asked.sha FROM unnest($2::text[]) WITH ORDINALITY AS asked(sha,place)
 WHERE NOT EXISTS (SELECT 1 FROM remote_git_commits stored WHERE stored.repo_id=$1 AND stored.sha=asked.sha)
 ORDER BY asked.place`, repo, asked)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	missing := []string{}
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		missing = append(missing, sha)
	}
	return missing, rows.Err()
}

func (s *PostgresStore) GetRemoteCommits(ctx context.Context, repo domain.ContentHash, shas []string) ([]domain.RemoteCommit, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	asked, err := remoteGitSHAs(shas)
	if err != nil {
		return nil, err
	}
	return scanRemoteGitCommits(s.db(ctx).Query(ctx, `SELECT `+remoteGitCommitColumns+`
 FROM unnest($2::text[]) WITH ORDINALITY AS asked(sha,place)
 JOIN remote_git_commits stored ON stored.repo_id=$1 AND stored.sha=asked.sha
 ORDER BY asked.place`, repo, asked))
}

func (s *PostgresStore) ListRemoteCommits(ctx context.Context, repo domain.ContentHash, before domain.RemoteCommitCursor, limit int) ([]domain.RemoteCommit, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	if err := remoteGitLimit(limit); err != nil {
		return nil, err
	}
	if before.IsZero() {
		return scanRemoteGitCommits(s.db(ctx).Query(ctx, `SELECT `+remoteGitCommitColumns+` FROM remote_git_commits stored
 WHERE stored.repo_id=$1 ORDER BY stored.committed_at DESC, stored.sha DESC LIMIT $2`, repo, limit))
	}
	return scanRemoteGitCommits(s.db(ctx).Query(ctx, `SELECT `+remoteGitCommitColumns+` FROM remote_git_commits stored
 WHERE stored.repo_id=$1 AND (stored.committed_at, stored.sha) < ($2::timestamptz, $3::text)
 ORDER BY stored.committed_at DESC, stored.sha DESC LIMIT $4`, repo, before.CommittedAt.UTC(), before.SHA, limit))
}

func (s *PostgresStore) RemoteCommitFrontier(ctx context.Context, repo domain.ContentHash, limit int) ([]string, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	if err := remoteGitLimit(limit); err != nil {
		return nil, err
	}
	unreadable := []string{}
	if state, err := s.GetRemoteGitState(ctx, repo); err == nil {
		unreadable = append(unreadable, state.Unreadable...)
	} else if !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT DISTINCT named.parent FROM remote_git_commits child CROSS JOIN LATERAL unnest(child.parents) AS named(parent)
 WHERE child.repo_id=$1
   AND NOT EXISTS (SELECT 1 FROM remote_git_commits stored WHERE stored.repo_id=$1 AND stored.sha=named.parent)
   AND named.parent <> ALL($2::text[])
 ORDER BY named.parent LIMIT $3`, repo, unreadable, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	frontier := []string{}
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		frontier = append(frontier, sha)
	}
	return frontier, rows.Err()
}

func (s *PostgresStore) FindRemoteCommitCopies(ctx context.Context, repo domain.ContentHash, keys []domain.RemoteCommitKey) ([]domain.RemoteCommit, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	wanted, err := remoteGitKeys(keys)
	if err != nil {
		return nil, err
	}
	authored, emails, subjects := make([]time.Time, 0, len(wanted)), make([]string, 0, len(wanted)), make([]string, 0, len(wanted))
	for _, key := range wanted {
		authored, emails, subjects = append(authored, key.AuthoredAt), append(emails, key.AuthorEmail), append(subjects, key.Subject)
	}
	return scanRemoteGitCommits(s.db(ctx).Query(ctx, `SELECT `+remoteGitCommitColumns+`
 FROM unnest($2::timestamptz[],$3::text[],$4::text[]) AS wanted(authored_at,author_email,subject)
 JOIN remote_git_commits stored ON stored.repo_id=$1 AND stored.authored_at=wanted.authored_at
  AND stored.author_email=wanted.author_email AND stored.subject=wanted.subject
 ORDER BY stored.sha`, repo, authored, emails, subjects))
}

// ReplaceRemoteBranches removes the names that are gone and writes the ones
// that are new or moved, so that an unchanged list writes nothing.
func (s *PostgresStore) ReplaceRemoteBranches(ctx context.Context, repo domain.ContentHash, branches []domain.RemoteBranch) error {
	if err := validateHash(repo); err != nil {
		return err
	}
	listed, err := remoteGitBranches(branches)
	if err != nil {
		return err
	}
	names, shas := make([]string, 0, len(listed)), make([]string, 0, len(listed))
	for _, branch := range listed {
		names, shas = append(names, branch.Name), append(shas, branch.SHA)
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM remote_git_branches WHERE repo_id=$1 AND name <> ALL($2::text[])`, repo, names); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO remote_git_branches(repo_id,name,sha)
 SELECT $1, listed.name, listed.sha FROM unnest($2::text[],$3::text[]) AS listed(name,sha)
 ON CONFLICT (repo_id,name) DO UPDATE SET sha=EXCLUDED.sha WHERE remote_git_branches.sha <> EXCLUDED.sha`, repo, names, shas); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) ListRemoteBranches(ctx context.Context, repo domain.ContentHash) ([]domain.RemoteBranch, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT name,sha FROM remote_git_branches WHERE repo_id=$1`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	branches := []domain.RemoteBranch{}
	for rows.Next() {
		var branch domain.RemoteBranch
		if err := rows.Scan(&branch.Name, &branch.SHA); err != nil {
			return nil, err
		}
		branches = append(branches, branch)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The order of names is the same in both stores, whatever the database
	// collation is.
	listed, err := remoteGitBranches(branches)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid stored remote branch", domain.ErrIntegrity)
	}
	return listed, nil
}

// PutRemotePull replaces the pull request and its commit list as one change.
// The commit list lives in its own table, in the provider's order.
func (s *PostgresStore) PutRemotePull(ctx context.Context, repo domain.ContentHash, pull domain.RemotePull) error {
	if err := validateHash(repo); err != nil {
		return err
	}
	if err := pull.Validate(); err != nil {
		return err
	}
	pull = remoteGitPull(pull)
	commits := pull.Commits
	pull.Commits = nil
	body, err := json.Marshal(pull)
	if err != nil {
		return err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO remote_git_pulls(repo_id,number,body) VALUES($1,$2,$3)
 ON CONFLICT (repo_id,number) DO UPDATE SET body=EXCLUDED.body`, repo, pull.Number, body); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM remote_git_pull_commits WHERE repo_id=$1 AND number=$2`, repo, pull.Number); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO remote_git_pull_commits(repo_id,number,position,sha)
 SELECT $1, $2, listed.place-1, listed.sha FROM unnest($3::text[]) WITH ORDINALITY AS listed(sha,place)`, repo, pull.Number, commits); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) ListRemotePulls(ctx context.Context, repo domain.ContentHash) ([]domain.RemotePull, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	db := s.db(ctx)
	commits := map[int][]string{}
	listed, err := db.Query(ctx, `SELECT number,sha FROM remote_git_pull_commits WHERE repo_id=$1 ORDER BY number, position`, repo)
	if err != nil {
		return nil, err
	}
	defer listed.Close()
	for listed.Next() {
		var number int
		var sha string
		if err := listed.Scan(&number, &sha); err != nil {
			return nil, err
		}
		commits[number] = append(commits[number], sha)
	}
	if err := listed.Err(); err != nil {
		return nil, err
	}
	listed.Close()
	rows, err := db.Query(ctx, `SELECT number,body FROM remote_git_pulls WHERE repo_id=$1 ORDER BY number DESC`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pulls := []domain.RemotePull{}
	for rows.Next() {
		var number int
		var body []byte
		if err := rows.Scan(&number, &body); err != nil {
			return nil, err
		}
		var pull domain.RemotePull
		if err := json.Unmarshal(body, &pull); err != nil {
			return nil, fmt.Errorf("%w: invalid stored pull request: %v", domain.ErrIntegrity, err)
		}
		pull.Commits = commits[number]
		pull = remoteGitPull(pull)
		if err := pull.Validate(); err != nil || pull.Number != number {
			return nil, fmt.Errorf("%w: invalid stored pull request", domain.ErrIntegrity)
		}
		pulls = append(pulls, pull)
	}
	return pulls, rows.Err()
}

func (s *PostgresStore) RemotePullsListing(ctx context.Context, repo domain.ContentHash, shas []string) (map[string][]int, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	asked, err := remoteGitSHAs(shas)
	if err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT DISTINCT sha,number FROM remote_git_pull_commits WHERE repo_id=$1 AND sha=ANY($2::text[]) ORDER BY sha, number`, repo, asked)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	listing := map[string][]int{}
	for rows.Next() {
		var sha string
		var number int
		if err := rows.Scan(&sha, &number); err != nil {
			return nil, err
		}
		listing[sha] = append(listing[sha], number)
	}
	return listing, rows.Err()
}
