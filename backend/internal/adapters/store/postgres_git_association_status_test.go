//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func readGitStatusTestPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash, origin, kind, state, cursor string, limit int) []outbound.GitAssociationStatusEvidence {
	t.Helper()
	var result []outbound.GitAssociationStatusEvidence
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		var err error
		result, err = storage.ReadGitAssociationStatus(read, repo, origin, domain.GitAssociationStatusRequest{Kind: kind, State: state, Cursor: cursor, Limit: limit})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("nil status rows")
	}
	return result
}

func gitStatusIDsPG(kind string, rows []outbound.GitAssociationStatusEvidence) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		switch kind {
		case "publication":
			ids = append(ids, row.Publication.ID)
		case "integration":
			ids = append(ids, row.Work.AdmissionID)
		case "memory":
			ids = append(ids, row.Supplement.ID)
		}
	}
	return ids
}

func writeGitStatusSupplementPG(t *testing.T, storage *PostgresStore, job domain.GitContributionSupplement) {
	t.Helper()
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.pool.Exec(context.Background(), `UPDATE git_contribution_supplements SET version=$3,state=$4,next_attempt=$5,lease_until=$6,payload=$7 WHERE repo_id=$1 AND id=$2`, job.RepoID, job.ID, job.Version, job.State, job.NextAttempt, job.LeaseUntil, raw); err != nil {
		t.Fatal(err)
	}
}

func TestPGGitAssociationStatusPublicationScopeAndAdmission(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, discovery, publications := gitDiscoveryPGFixture(t, storage)
	repo, origin := discovery.RepoID, discovery.GitOrigin
	if err := saveGitDiscoveryPG(ctx, storage, gitDiscoveryPGPage(discovery), 0, publications); err != nil {
		t.Fatal(err)
	}
	rows := readGitStatusTestPG(t, ctx, peer, repo, origin, "publication", "pending", "", 1)
	if len(rows) != 2 || rows[0].Publication.ID != publications[0].ID || rows[1].Publication.ID != publications[1].ID || rows[0].Admission != nil {
		t.Fatal("publication lookahead changed", rows)
	}
	admission := gitAdmissionPGBaseline(t, ctx, storage, repo, origin, time.Now().UTC())
	admission.PublicationID, admission.Capture = publications[0].ID, publications[0].Target
	admission.OrdinaryObservationIDs, admission.CarryObservationIDs = []string{strings.Repeat("d", 32)}, []string{strings.Repeat("e", 32)}
	admission = admission.WithID()
	putGitContributionWorkAdmissionPG(t, storage, ctx, admission)
	other := admission
	other.Destination.DestinationBranchID = "other-destination-identity"
	other.Destination = other.Destination.WithID()
	other = other.WithID()
	putGitContributionWorkAdmissionPG(t, storage, ctx, other)
	rows = readGitStatusTestPG(t, ctx, peer, repo, origin, "publication", "all", "", 100)
	if len(rows) != 3 || rows[0].Admission == nil || rows[0].Admission.ID != min(admission.ID, other.ID) || !reflect.DeepEqual(*rows[0].Publication, publications[0]) {
		t.Fatal("first exact admission or original publication lost", rows)
	}
	rows = readGitStatusTestPG(t, ctx, peer, repo, origin, "publication", "pending", "", 100)
	if got := gitStatusIDsPG("publication", rows); !reflect.DeepEqual(got, []string{publications[1].ID, publications[2].ID}) {
		t.Fatal("pending admission filter", got)
	}
	rows = readGitStatusTestPG(t, ctx, peer, repo, origin, "publication", "pending", publications[1].ID, 1)
	if len(rows) != 1 || rows[0].Publication.ID != publications[2].ID {
		t.Fatal("publication cursor was not exclusive", rows)
	}
	foreign := domain.HashContent([]byte(t.Name() + " foreign"))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: foreign, GitRemoteURL: origin}); err != nil {
		t.Fatal(err)
	}
	if rows := readGitStatusTestPG(t, ctx, peer, foreign, origin, "publication", "all", "", 100); len(rows) != 0 {
		t.Fatal("repository scope leaked", rows)
	}
	newOrigin := "https://github.com/acme/new-origin.git"
	if _, err := storage.pool.Exec(ctx, `UPDATE repos SET git_remote_url=$2 WHERE id=$1`, repo, newOrigin); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.ReadGitAssociationStatus(ctx, repo, origin, domain.GitAssociationStatusRequest{Kind: "publication", State: "all", Limit: 100}); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("historical origin accepted as current", err)
	}
	if rows := readGitStatusTestPG(t, ctx, peer, repo, newOrigin, "publication", "all", "", 100); len(rows) != 0 {
		t.Fatal("old origin publication leaked", rows)
	}
	discovery.GitOrigin = newOrigin
	if err := saveGitDiscoveryPG(ctx, storage, gitDiscoveryPGPage(discovery), 0, publications); err != nil {
		t.Fatal(err)
	}
	if rows := readGitStatusTestPG(t, ctx, peer, repo, newOrigin, "publication", "pending", "", 100); len(rows) != 3 {
		t.Fatal("old origin admission suppressed current publication", rows)
	}
}

func TestPGGitAssociationStatusEffectsOverrideSchedulerState(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	fixture := newGitSupplementPGFixture(t, storage)
	ctx, repo, origin := fixture.ctx, fixture.receipt.RepoID, fixture.receipt.GitOrigin
	rows := readGitStatusTestPG(t, ctx, peer, repo, origin, "integration", "pending", "", 100)
	if len(rows) != 2 {
		t.Fatal("initial admissions missing", rows)
	}
	for _, row := range rows {
		if row.Work == nil || row.Admission == nil || row.Work.Version != 0 || row.Work.Attempts != 0 || row.Work.ValidateRecorded() != nil || row.Proof != nil {
			t.Fatal("initial version-zero work not preserved", row)
		}
	}
	if err := putGitProofPG(ctx, storage, fixture.proof); err != nil {
		t.Fatal(err)
	}
	rows = readGitStatusTestPG(t, ctx, peer, repo, origin, "integration", "pending", "", 100)
	if len(rows) != 1 || rows[0].Work.AdmissionID == fixture.proof.AdmissionID {
		t.Fatal("waiting scheduler row with proof remained pending", rows)
	}
	missingEffect := *rows[0].Work
	missingEffect.State = "completed"
	writeGitContributionWorkTestPG(t, storage, missingEffect)
	rows = readGitStatusTestPG(t, ctx, peer, repo, origin, "integration", "pending", "", 100)
	if len(rows) != 1 || rows[0].Work.State != "completed" || rows[0].Proof != nil {
		t.Fatal("completed work without effect hidden from validation", rows)
	}
	rows = readGitStatusTestPG(t, ctx, peer, repo, origin, "integration", "all", "", 100)
	proofCount := 0
	for _, row := range rows {
		if row.Proof != nil {
			proofCount++
			if *row.Proof != fixture.proof || row.Admission.ID != fixture.proof.AdmissionID || row.Work.State != "waiting" {
				t.Fatal("actual proof evidence lost", row)
			}
		}
	}
	if proofCount != 1 {
		t.Fatal("proof count", proofCount)
	}
	if err := fixture.discover(ctx, storage, fixture.publication); err != nil {
		t.Fatal(err)
	}
	rows = readGitStatusTestPG(t, ctx, peer, repo, origin, "memory", "pending", "", 100)
	if len(rows) != 1 || rows[0].Memory != nil || rows[0].Admission == nil || rows[0].Admission.ID != fixture.proof.AdmissionID || rows[0].Proof == nil || *rows[0].Proof != fixture.proof || !reflect.DeepEqual(*rows[0].Publication, fixture.publication) {
		t.Fatal("pending memory missing exact linked evidence", rows)
	}
	initial := *rows[0].Supplement
	malformedCompletion := initial
	malformedCompletion.Version, malformedCompletion.Attempts, malformedCompletion.State = 1, 1, "completed"
	writeGitStatusSupplementPG(t, storage, malformedCompletion)
	rows = readGitStatusTestPG(t, ctx, peer, repo, origin, "memory", "pending", "", 100)
	if len(rows) != 1 || rows[0].Supplement.State != "completed" || rows[0].Memory != nil {
		t.Fatal("completed supplement without pin hidden from validation", rows)
	}
	writeGitStatusSupplementPG(t, storage, initial)
	pin := supplementPinPG(fixture, fixture.publication)
	if err := storage.PutGitContributionMemory(ctx, pin); err != nil {
		t.Fatal(err)
	}
	if rows := readGitStatusTestPG(t, ctx, peer, repo, origin, "memory", "pending", "", 100); len(rows) != 0 {
		t.Fatal("waiting scheduler row with pin remained pending", rows)
	}
	rows = readGitStatusTestPG(t, ctx, peer, repo, origin, "memory", "all", "", 100)
	if len(rows) != 1 || rows[0].Memory == nil || *rows[0].Memory != pin || rows[0].Supplement.State != "waiting" || rows[0].Admission == nil {
		t.Fatal("actual pin or mandatory admission lost", rows)
	}
}

func TestPGGitAssociationStatusPaginationReadOnlyAndIndexes(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	fixture := newGitSupplementPGFixture(t, storage)
	ctx, repo, origin := fixture.ctx, fixture.receipt.RepoID, fixture.receipt.GitOrigin
	if err := putGitProofPG(ctx, storage, fixture.proof); err != nil {
		t.Fatal(err)
	}
	var batch []domain.HistoryEvent
	for number := 1; number <= 103; number++ {
		publication := fixture.publication
		publication.ID = fmt.Sprintf("%032x", number)
		if err := storage.ApplyHistoryEvent(ctx, publication); err != nil {
			t.Fatal(err)
		}
		admission := fixture.admission
		admission.PublicationID, admission.AfterReadVersion = publication.ID, fixture.receipt.Version
		admission = admission.WithID()
		putGitContributionWorkAdmissionPG(t, storage, ctx, admission)
		batch = append(batch, publication)
		if len(batch) == 100 || number == 103 {
			if err := fixture.discover(ctx, storage, batch...); err != nil {
				t.Fatal(err)
			}
			batch = nil
		}
	}
	before, err := storage.RepositoryRevision(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"publication", "integration", "memory"} {
		t.Run(kind, func(t *testing.T) {
			rows := readGitStatusTestPG(t, ctx, peer, repo, origin, kind, "all", "", 100)
			if len(rows) != 101 {
				t.Fatal("raw limit plus one not respected", len(rows))
			}
			prefix := gitStatusIDsPG(kind, rows)
			if !slices.IsSorted(prefix) {
				t.Fatal("rows not ordered", prefix)
			}
			tail := gitStatusIDsPG(kind, readGitStatusTestPG(t, ctx, peer, repo, origin, kind, "all", prefix[99], 100))
			if len(tail) == 0 || tail[0] != prefix[100] {
				t.Fatal("lookahead skipped on next page", tail, prefix[100])
			}
			want := 103
			if kind == "integration" {
				want = 105
			}
			if 100+len(tail) != want {
				t.Fatal("lost or duplicated status rows", 100+len(tail), want)
			}
			if rows := readGitStatusTestPG(t, ctx, peer, repo, origin, kind, "all", tail[len(tail)-1], 100); len(rows) != 0 {
				t.Fatal("cursor did not drain", rows)
			}
		})
	}
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		if _, err := storage.db(read).Exec(read, `SET LOCAL enable_seqscan=off`); err != nil {
			return err
		}
		for _, kind := range []string{"publication", "integration", "memory"} {
			request := domain.GitAssociationStatusRequest{Kind: kind, State: "pending", Limit: 100}
			var plan []byte
			if err := storage.db(read).QueryRow(read, `EXPLAIN (FORMAT JSON) `+gitAssociationStatusQueryPG(request), repo, origin, "", 101).Scan(&plan); err != nil {
				return err
			}
			if strings.Contains(string(plan), `"Node Type": "Seq Scan"`) || !strings.Contains(string(plan), `"Index Name"`) {
				t.Fatal("status query lacks indexed path", kind, string(plan))
			}
			if _, err := storage.ReadGitAssociationStatus(read, repo, origin, request); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal("status not read-only or nested reads left connection busy", err)
	}
	if after, err := peer.RepositoryRevision(ctx, repo); err != nil || after != before {
		t.Fatal("diagnostics mutated revisions", after, before, err)
	}
	var claimed int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM git_contribution_supplements WHERE repo_id=$1 AND (version<>0 OR state<>'waiting')`, repo).Scan(&claimed); err != nil || claimed != 0 {
		t.Fatal("diagnostics mutated scheduler", claimed, err)
	}
}

func TestPGGitAssociationStatusCorruptLinksFailClosed(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	fixture := newGitSupplementPGFixture(t, storage)
	ctx, repo, origin := fixture.ctx, fixture.receipt.RepoID, fixture.receipt.GitOrigin
	if err := putGitProofPG(ctx, storage, fixture.proof); err != nil {
		t.Fatal(err)
	}
	if err := fixture.discover(ctx, storage, fixture.publication); err != nil {
		t.Fatal(err)
	}
	pin := supplementPinPG(fixture, fixture.publication)
	if err := storage.PutGitContributionMemory(ctx, pin); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name, kind, sql string
		args            []any
	}{
		{"unaccepted-publication", "publication", `DELETE FROM context_history WHERE repo_id=$1 AND id=$2`, []any{repo, fixture.publication.ID}},
		{"changed-publication", "publication", `UPDATE git_association_publications SET payload=jsonb_set(payload,'{branch_id}',to_jsonb('other-source'::text)) WHERE repo_id=$1 AND publication_id=$2`, []any{repo, fixture.publication.ID}},
		{"work-column", "integration", `UPDATE git_contribution_work SET version=1 WHERE repo_id=$1 AND admission_id=$2`, []any{repo, fixture.admission.ID}},
		{"admission-column", "integration", `UPDATE git_contribution_admissions SET admission_key=$3 WHERE repo_id=$1 AND id=$2`, []any{repo, fixture.admission.ID, strings.Repeat("f", 64)}},
		{"baseline-receipt", "integration", `UPDATE git_head_receipts SET payload=jsonb_set(payload,'{more}',to_jsonb(true)) WHERE repo_id=$1 AND id=$2`, []any{repo, fixture.admission.Destination.ReadReceiptID}},
		{"proof", "integration", `UPDATE git_contribution_proofs SET payload=jsonb_set(payload,'{context_head_id}',to_jsonb($3::text)) WHERE repo_id=$1 AND id=$2`, []any{repo, fixture.proof.ID, strings.Repeat("f", 64)}},
		{"supplement-column", "memory", `UPDATE git_contribution_supplements SET version=1 WHERE repo_id=$1`, []any{repo}},
		{"memory-column", "memory", `UPDATE git_contribution_memories SET memory_source=$3 WHERE repo_id=$1 AND id=$2`, []any{repo, pin.ID, domain.HashContent([]byte("tampered pin source"))}},
		{"memory-publication", "memory", `UPDATE context_history SET event=jsonb_set(event,'{memory_pinned}',to_jsonb(false)) WHERE repo_id=$1 AND id=$2`, []any{repo, fixture.publication.ID}},
		{"memory-admission", "memory", `UPDATE git_contribution_admissions SET admission_key=$3 WHERE repo_id=$1 AND id=$2`, []any{repo, fixture.admission.ID, strings.Repeat("f", 64)}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			abort := errors.New("rollback corrupted status fixture")
			if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
				if _, err := storage.db(bound).Exec(bound, scenario.sql, scenario.args...); err != nil {
					return err
				}
				if _, err := storage.ReadGitAssociationStatus(bound, repo, origin, domain.GitAssociationStatusRequest{Kind: scenario.kind, State: "all", Limit: 100}); !errors.Is(err, domain.ErrIntegrity) {
					t.Fatal("corrupt linked row exposed", err)
				}
				return abort
			}); !errors.Is(err, abort) {
				t.Fatal(err)
			}
		})
	}
	conflict := pin
	conflict.MemoryHash, conflict.MemorySource = domain.HashContent([]byte("conflicting pin")), fixture.admission.Capture
	conflict = conflict.WithID()
	if err := storage.PutGitContributionMemory(ctx, conflict); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ReadGitAssociationStatus(ctx, repo, origin, domain.GitAssociationStatusRequest{Kind: "memory", State: "all", Limit: 100}); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("conflicting logical pins silently selected", err)
	}
}

func TestPGGitAssociationStatusCancellationValidationAndOriginIsolation(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	fixture := newGitSupplementPGFixture(t, storage)
	ctx, repo, origin := fixture.ctx, fixture.receipt.RepoID, fixture.receipt.GitOrigin
	if err := putGitProofPG(ctx, storage, fixture.proof); err != nil {
		t.Fatal(err)
	}
	if err := fixture.discover(ctx, storage, fixture.publication); err != nil {
		t.Fatal(err)
	}
	request := domain.GitAssociationStatusRequest{Kind: "memory", State: "all", Limit: 100}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := storage.ReadGitAssociationStatus(canceled, repo, origin, request); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, mutate := range []func(*domain.GitAssociationStatusRequest){
		func(request *domain.GitAssociationStatusRequest) { request.Kind = "unknown" },
		func(request *domain.GitAssociationStatusRequest) { request.State = "completed" },
		func(request *domain.GitAssociationStatusRequest) { request.Limit = 0 },
		func(request *domain.GitAssociationStatusRequest) { request.Limit = 101 },
		func(request *domain.GitAssociationStatusRequest) { request.Cursor = strings.Repeat("A", 64) },
		func(request *domain.GitAssociationStatusRequest) { request.Cursor = strings.Repeat("a", 32) },
	} {
		changed := request
		mutate(&changed)
		if _, err := storage.ReadGitAssociationStatus(ctx, repo, origin, changed); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid status request accepted", changed, err)
		}
	}
	if err := peer.WithinReadSnapshot(ctx, func(read context.Context) error {
		if _, err := storage.ReadGitAssociationStatus(read, repo, origin, request); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("read escaped foreign transaction", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	foreign := domain.HashContent([]byte(t.Name() + " foreign"))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: foreign, GitRemoteURL: origin}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"publication", "integration", "memory"} {
		if rows := readGitStatusTestPG(t, ctx, peer, foreign, origin, kind, "all", "", 100); len(rows) != 0 {
			t.Fatal("foreign repository leaked evidence", kind, rows)
		}
	}
	newOrigin := "https://github.com/acme/other.git"
	if _, err := storage.pool.Exec(ctx, `UPDATE repos SET git_remote_url=$2 WHERE id=$1`, repo, newOrigin); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"publication", "integration", "memory"} {
		if rows := readGitStatusTestPG(t, ctx, peer, repo, newOrigin, kind, "all", "", 100); len(rows) != 0 {
			t.Fatal("historical origin leaked evidence", kind, rows)
		}
	}
}
