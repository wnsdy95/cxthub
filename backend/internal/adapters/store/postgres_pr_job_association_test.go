//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type prAssociationRows struct {
	pgx.Rows
	payload []byte
	count   int
	read    int
	closed  bool
}

func (rows *prAssociationRows) Next() bool { rows.read++; return rows.read <= rows.count }
func (rows *prAssociationRows) Err() error { return nil }
func (rows *prAssociationRows) Close()     { rows.closed = true }
func (rows *prAssociationRows) Scan(targets ...any) error {
	*targets[0].(*[]byte) = rows.payload
	return nil
}

type prAssociationDatabase struct {
	pgDatabase
	t     *testing.T
	repo  domain.ContentHash
	rows  []*prAssociationRows
	calls int
}

func (database *prAssociationDatabase) Query(_ context.Context, query string, args ...any) (pgx.Rows, error) {
	database.t.Helper()
	if !strings.Contains(query, "WHERE repo_id=$1 AND") || !strings.HasSuffix(query, "LIMIT $2") || len(args) != 3 || args[0] != database.repo || args[1] != outbound.PRJobAssociationReadLimit+1 {
		database.t.Fatal("unbounded or unscoped diagnostic SQL", query)
	}
	if strings.Contains(query, "FROM pr_contributions ") {
		if !strings.Contains(query, "payload->'pr'->>'number'=ANY($3::text[]) ORDER BY") || len(args[2].([]string)) != 100 {
			database.t.Fatal("contribution filtering must precede the row cap", query)
		}
	} else if !strings.Contains(query, "number=ANY($3::bigint[]) ORDER BY") || len(args[2].([]int64)) != 100 {
		database.t.Fatal("numeric filtering must precede the row cap", query)
	}
	rows := database.rows[database.calls]
	database.calls++
	return rows, nil
}

func TestPGPRJobAssociationUsesThreeCappedReads(t *testing.T) {
	storage := NewFSStore(t.TempDir())
	_, checkpoint, proof := prHistoryFixture(t, storage)
	contribution := (domain.PRContribution{RepoID: proof.RepoID, GitOrigin: proof.GitOrigin,
		ProviderRepositoryID: proof.ProviderRepositoryID, PullRequestID: proof.PullRequestID, PR: checkpoint.PR,
		DestinationBranchID: "main-id", SourceBranchID: proof.BranchID, Capture: proof.Capture, OriginalHead: proof.OriginalHead}).WithID()
	for _, count := range []int{1, outbound.PRJobAssociationReadLimit, outbound.PRJobAssociationReadLimit + 1} {
		database := &prAssociationDatabase{t: t, repo: checkpoint.RepoID}
		for _, record := range []any{checkpoint, proof, contribution} {
			raw, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			database.rows = append(database.rows, &prAssociationRows{payload: raw, count: count})
		}
		numbers := make([]int, 100)
		for index := range numbers {
			numbers[index] = index + 1
		}
		result, err := readPRJobAssociationEvidencePG(context.Background(), database, checkpoint.RepoID, numbers)
		complete := count <= outbound.PRJobAssociationReadLimit
		if err != nil || database.calls != 3 || result.CheckpointsComplete != complete || result.ParticipationsComplete != complete || result.ContributionsComplete != complete {
			t.Fatal("incorrect batch completion", database.calls, result, err)
		}
		if !complete && len(result.Checkpoints)+len(result.Participations)+len(result.Contributions) != 0 {
			t.Fatal("overflow published a partial total")
		}
		for _, rows := range database.rows {
			if !rows.closed || rows.read > outbound.PRJobAssociationReadLimit+1 {
				t.Fatal("result set read past sentinel or leaked")
			}
		}
	}
}

func TestPGPRJobAssociationUnrelatedCrowdDoesNotHideSelectedPR(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	ctx, checkpoint, proof := prHistoryFixture(t, storage)
	contribution := (domain.PRContribution{RepoID: proof.RepoID, GitOrigin: proof.GitOrigin,
		ProviderRepositoryID: proof.ProviderRepositoryID, PullRequestID: proof.PullRequestID, PR: checkpoint.PR,
		DestinationBranchID: "main-id", SourceBranchID: proof.BranchID, Capture: proof.Capture, OriginalHead: proof.OriginalHead}).WithID()
	if err := storage.SavePRHistoryCheckpoint(ctx, checkpoint, 0); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutPRParticipation(ctx, proof); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutPRContribution(ctx, contribution); err != nil {
		t.Fatal(err)
	}
	var checkpoints, participations, contributions [][]any
	for index := range outbound.PRJobAssociationReadLimit + 1 {
		otherCheckpoint, otherProof, otherContribution := checkpoint, proof, contribution
		otherCheckpoint.PR.Number, otherProof.Number, otherContribution.PR.Number = 900, 900, 900
		otherCheckpoint.ProviderRepositoryID = int64(1000 + index)
		otherProof.PublicationID = fmt.Sprintf("%032x", index+1)
		otherProof = otherProof.WithID()
		otherContribution.SourceBranchID = fmt.Sprintf("unrelated-%d", index)
		otherContribution = otherContribution.WithID()
		checkpointRaw, _ := json.Marshal(otherCheckpoint)
		proofRaw, _ := json.Marshal(otherProof)
		contributionRaw, _ := json.Marshal(otherContribution)
		checkpoints = append(checkpoints, []any{checkpoint.RepoID, checkpoint.GitOrigin, otherCheckpoint.ProviderRepositoryID, 900, 1, checkpointRaw})
		participations = append(participations, []any{proof.RepoID, otherProof.ID, proof.GitOrigin, proof.ProviderRepositoryID, 900, proofRaw})
		contributions = append(contributions, []any{contribution.RepoID, otherContribution.ID, contributionRaw})
	}
	for _, batch := range []struct {
		table   string
		columns []string
		rows    [][]any
	}{
		{"pr_history_checkpoints", []string{"repo_id", "origin", "provider_repository_id", "number", "version", "payload"}, checkpoints},
		{"pr_participations", []string{"repo_id", "id", "origin", "provider_repository_id", "number", "payload"}, participations},
		{"pr_contributions", []string{"repo_id", "id", "payload"}, contributions},
	} {
		if _, err := storage.pool.CopyFrom(ctx, pgx.Identifier{batch.table}, batch.columns, pgx.CopyFromRows(batch.rows)); err != nil {
			t.Fatal(err)
		}
	}
	result, err := storage.ReadPRJobAssociationEvidence(ctx, checkpoint.RepoID, []int{checkpoint.PR.Number})
	if err != nil || !result.CheckpointsComplete || !result.ParticipationsComplete || !result.ContributionsComplete || len(result.Checkpoints) != 1 || len(result.Participations) != 1 || len(result.Contributions) != 1 {
		t.Fatal("unrelated PRs exhausted a selected PR budget", result, err)
	}
	result, err = storage.ReadPRJobAssociationEvidence(ctx, checkpoint.RepoID, []int{900})
	if err != nil || result.CheckpointsComplete || result.ParticipationsComplete || result.ContributionsComplete || len(result.Checkpoints)+len(result.Participations)+len(result.Contributions) != 0 {
		t.Fatal("selected overflow was reported as a total", result, err)
	}
}
