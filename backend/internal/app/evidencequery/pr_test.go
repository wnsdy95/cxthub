package evidencequery

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type verifiedPRReadLog struct {
	repo    domain.ContentHash
	calls   []string
	failAt  string
	failure error
}

func (reads *verifiedPRReadLog) record(repo domain.ContentHash, operation string) error {
	reads.calls = append(reads.calls, operation)
	if repo != reads.repo {
		return domain.ErrIntegrity
	}
	if operation == reads.failAt {
		return reads.failure
	}
	return nil
}

type verifiedPRMetadataReader struct {
	reads    *verifiedPRReadLog
	metadata domain.Repo
}

func (reader *verifiedPRMetadataReader) GetRepo(_ context.Context, repo domain.ContentHash) (domain.Repo, error) {
	return reader.metadata, reader.reads.record(repo, "metadata")
}

type verifiedPRLedgerReader struct {
	reads   *verifiedPRReadLog
	records []domain.PRContribution
}

func (reader *verifiedPRLedgerReader) ListPRContributions(_ context.Context, repo domain.ContentHash) ([]domain.PRContribution, error) {
	return reader.records, reader.reads.record(repo, "ledger")
}

type verifiedPRHistoryReader struct {
	reads        *verifiedPRReadLog
	checkpoints  []domain.PRHistoryCheckpoint
	proofs       []domain.PRParticipation
	observations []domain.GitRefObservation
}

func (reader *verifiedPRHistoryReader) GetPRHistoryCheckpoint(_ context.Context, repo domain.ContentHash, origin string, repository int64, number int) (domain.PRHistoryCheckpoint, error) {
	if err := reader.reads.record(repo, "checkpoint"); err != nil {
		return domain.PRHistoryCheckpoint{}, err
	}
	for _, checkpoint := range reader.checkpoints {
		if checkpoint.RepoID == repo && checkpoint.GitOrigin == origin && checkpoint.ProviderRepositoryID == repository && checkpoint.PR.Number == number {
			return checkpoint, nil
		}
	}
	return domain.PRHistoryCheckpoint{}, domain.ErrNotFound
}

func (reader *verifiedPRHistoryReader) GetPRRefObservation(_ context.Context, repo domain.ContentHash, origin, identity string) (domain.GitRefObservation, error) {
	if err := reader.reads.record(repo, "observation"); err != nil {
		return domain.GitRefObservation{}, err
	}
	for _, observation := range reader.observations {
		if observation.RepoID == repo && observation.GitOrigin == origin && observation.ID == identity {
			return observation, nil
		}
	}
	return domain.GitRefObservation{}, domain.ErrNotFound
}

func (reader *verifiedPRHistoryReader) ListPRParticipations(_ context.Context, repo domain.ContentHash, origin string, number int) ([]domain.PRParticipation, error) {
	if err := reader.reads.record(repo, "participations"); err != nil {
		return nil, err
	}
	var proofs []domain.PRParticipation
	for _, proof := range reader.proofs {
		if proof.RepoID == repo && proof.GitOrigin == origin && proof.Number == number {
			proofs = append(proofs, proof)
		}
	}
	return proofs, nil
}

type verifiedPRFixture struct {
	repo     domain.ContentHash
	history  []domain.HistoryEvent
	reads    *verifiedPRReadLog
	metadata *verifiedPRMetadataReader
	ledger   *verifiedPRLedgerReader
	evidence *verifiedPRHistoryReader
}

func newVerifiedPRFixture(test *testing.T) *verifiedPRFixture {
	test.Helper()
	repo := domain.ContentHash("sha256:" + strings.Repeat("1", 64))
	checkpoint := domain.PRHistoryCheckpoint{
		RepoID: repo, GitOrigin: "https://github.com/acme/api.git", InstallationID: 789, ProviderRepositoryID: 91,
		PR:      domain.PullRequestMerge{Number: 7, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("c", 40), MergeSHA: strings.Repeat("d", 40)},
		Version: 1, PullRequestID: 701, MergedAt: time.Unix(300, 0).UTC(), NextAttempt: time.Unix(400, 0).UTC(),
	}
	observation := (domain.GitRefObservation{
		RepoID: repo, GitOrigin: checkpoint.GitOrigin, Source: "github-pr-reconciliation", Ref: "refs/heads/feature",
		Before: strings.Repeat("a", 40), After: strings.Repeat("b", 40), Forced: true, Delivery: "rewrite",
		PullRequest: &domain.GitPullRequestObservation{
			InstallationID: 789, RepositoryID: 91, ID: 701, Number: 7, Action: "force-pushed", HeadRepositoryID: 91,
			BaseRepositoryID: 91, BaseRef: "main", ProviderEventID: "rewrite-event", OccurredAt: time.Unix(200, 0).UTC(),
		},
	}).WithID()
	capture := domain.ContentHash("sha256:" + strings.Repeat("2", 64))
	ordinary := domain.HistoryEvent{
		ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: "feature-identity", Branch: "feature", Kind: "birth",
		Source: capture, Target: capture, GitAfter: observation.Before, CreatedAt: time.Unix(100, 0).UTC(),
	}
	publication := ordinary
	publication.ID, publication.Kind, publication.CreatedAt = strings.Repeat("2", 32), "publish", time.Unix(500, 0).UTC()
	history := []domain.HistoryEvent{ordinary, publication}
	proof, err := domain.VerifyPRParticipation(checkpoint, observation, publication, history)
	if err != nil {
		test.Fatal(err)
	}
	record := (domain.PRContribution{
		RepoID: repo, GitOrigin: proof.GitOrigin, ProviderRepositoryID: proof.ProviderRepositoryID, PullRequestID: proof.PullRequestID,
		PR: checkpoint.PR, DestinationBranchID: "main-identity", SourceBranchID: proof.BranchID, Capture: proof.Capture, OriginalHead: proof.OriginalHead,
	}).WithID()
	if err := record.Validate(); err != nil {
		test.Fatal(err)
	}
	reads := &verifiedPRReadLog{repo: repo}
	return &verifiedPRFixture{
		repo: repo, history: history, reads: reads,
		metadata: &verifiedPRMetadataReader{reads: reads, metadata: domain.Repo{ID: repo, GitRemoteURL: checkpoint.GitOrigin}},
		ledger:   &verifiedPRLedgerReader{reads: reads, records: []domain.PRContribution{record}},
		evidence: &verifiedPRHistoryReader{reads: reads, checkpoints: []domain.PRHistoryCheckpoint{checkpoint}, proofs: []domain.PRParticipation{proof}, observations: []domain.GitRefObservation{observation}},
	}
}

func (fixture *verifiedPRFixture) queries() *Queries {
	return &Queries{metadata: fixture.metadata, prLedger: fixture.ledger, prHistory: fixture.evidence}
}

func (fixture *verifiedPRFixture) assertReads(test *testing.T, expected ...string) {
	test.Helper()
	if !slices.Equal(fixture.reads.calls, expected) {
		test.Fatalf("reads = %v, want %v", fixture.reads.calls, expected)
	}
}

func TestVerifiedPRProof(test *testing.T) {
	fixture := newVerifiedPRFixture(test)
	fixture.metadata.metadata.GitRemoteURL = "git@GitHub.com:acme/api.git"
	verified, err := fixture.queries().VerifiedPR(context.Background(), fixture.repo, fixture.history)
	if err != nil || len(verified) != 1 {
		test.Fatalf("verified = %+v, error = %v", verified, err)
	}
	contribution := verified[0]
	publication := fixture.history[1]
	if contribution.Record != fixture.ledger.records[0] || !contribution.Valid || len(contribution.Publications) != 1 || contribution.Publications[publication.ID] != publication || contribution.MergedAt != fixture.evidence.checkpoints[0].MergedAt {
		test.Fatalf("incomplete verified proof: %+v", contribution)
	}
	fixture.assertReads(test, "ledger", "metadata", "checkpoint", "participations", "observation")
}

func TestVerifiedPRCompatibilityAndOriginMismatch(test *testing.T) {
	for _, scenario := range []string{"missing-ledger", "empty-ledger", "missing-history", "origin-mismatch"} {
		fixture := newVerifiedPRFixture(test)
		queries := fixture.queries()
		var expectedError error
		var expectedReads []string
		switch scenario {
		case "missing-ledger":
			queries.prLedger = nil
			queries.metadata, queries.prHistory = nil, nil
		case "empty-ledger":
			fixture.ledger.records = nil
			queries.metadata, queries.prHistory = nil, nil
			expectedReads = []string{"ledger"}
		case "missing-history":
			queries.prHistory, queries.metadata = nil, nil
			expectedError = domain.ErrIntegrity
			expectedReads = []string{"ledger"}
		case "origin-mismatch":
			fixture.metadata.metadata.GitRemoteURL = "https://github.com/acme/other.git"
			expectedReads = []string{"ledger", "metadata"}
		}
		verified, err := queries.VerifiedPR(context.Background(), fixture.repo, fixture.history)
		if !errors.Is(err, expectedError) {
			test.Fatalf("%s: error = %v, want %v", scenario, err, expectedError)
		}
		if scenario == "origin-mismatch" {
			if len(verified) != 1 || verified[0].Record != fixture.ledger.records[0] || verified[0].Valid || len(verified[0].Publications) != 0 || !verified[0].MergedAt.IsZero() {
				test.Fatalf("%s: origin mismatch changed the valid record: %+v", scenario, verified)
			}
		} else if verified != nil {
			test.Fatalf("%s: expected nil result, got %+v", scenario, verified)
		}
		fixture.assertReads(test, expectedReads...)
	}
}

func TestVerifiedPRReadErrorsAndOrdering(test *testing.T) {
	operations := []string{"ledger", "metadata", "checkpoint", "participations", "observation"}
	for position, operation := range operations {
		fixture := newVerifiedPRFixture(test)
		fixture.reads.failAt, fixture.reads.failure = operation, errors.New("read failed")
		if position < 2 {
			fixture.ledger.records[0].ID = "invalid"
		}
		verified, err := fixture.queries().VerifiedPR(context.Background(), fixture.repo, fixture.history)
		if !errors.Is(err, fixture.reads.failure) || verified != nil {
			test.Fatalf("%s: verified = %+v, error = %v", operation, verified, err)
		}
		fixture.assertReads(test, operations[:position+1]...)
	}
}

func TestVerifiedPRHistoryAndEvidenceValidation(test *testing.T) {
	for _, scenario := range []string{"invalid-history", "checkpoint-mismatch", "proof-mismatch", "invalid-observation", "missing-publication", "pending-source", "missing-checkpoint"} {
		fixture := newVerifiedPRFixture(test)
		expectedError := domain.ErrIntegrity
		expectedReads := []string{"ledger", "metadata", "checkpoint", "participations", "observation"}
		switch scenario {
		case "invalid-history":
			fixture.history[0].ID = "invalid"
			expectedReads = expectedReads[:3]
		case "checkpoint-mismatch":
			fixture.evidence.checkpoints[0].PullRequestID++
		case "proof-mismatch":
			fixture.evidence.proofs[0].BranchID = "forged-identity"
			fixture.evidence.proofs[0] = fixture.evidence.proofs[0].WithID()
		case "invalid-observation":
			fixture.evidence.observations[0].After = "invalid"
		case "missing-publication":
			fixture.history = fixture.history[:1]
			expectedError = nil
			expectedReads = expectedReads[:4]
		case "pending-source":
			fixture.evidence.observations[0].PullRequest.ID++
			fixture.evidence.observations[0] = fixture.evidence.observations[0].WithID()
			expectedError = nil
		case "missing-checkpoint":
			fixture.evidence.checkpoints = nil
			expectedError = nil
			expectedReads = expectedReads[:3]
		}
		verified, err := fixture.queries().VerifiedPR(context.Background(), fixture.repo, fixture.history)
		if !errors.Is(err, expectedError) {
			test.Fatalf("%s: error = %v, want %v", scenario, err, expectedError)
		}
		if expectedError != nil {
			if verified != nil {
				test.Fatalf("%s: returned partial proof: %+v", scenario, verified)
			}
		} else if len(verified) != 1 || verified[0].Record != fixture.ledger.records[0] || verified[0].Valid || len(verified[0].Publications) != 0 {
			test.Fatalf("%s: unverified evidence was accepted: %+v", scenario, verified)
		}
		fixture.assertReads(test, expectedReads...)
	}
}

func TestVerifiedPRCancellation(test *testing.T) {
	fixture := newVerifiedPRFixture(test)
	fixture.ledger.records[0].ID = "invalid"
	fixture.metadata.metadata.ID = domain.ContentHash("sha256:" + strings.Repeat("9", 64))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	verified, err := fixture.queries().VerifiedPR(ctx, fixture.repo, fixture.history)
	if !errors.Is(err, context.Canceled) || verified != nil {
		test.Fatalf("cancellation lost precedence: verified = %+v, error = %v", verified, err)
	}
	fixture.assertReads(test, "ledger", "metadata")
}

func TestVerifiedPRSameCaptureConflictingScope(test *testing.T) {
	fixture := newVerifiedPRFixture(test)
	branchConflict, headConflict := fixture.ledger.records[0], fixture.ledger.records[0]
	branchConflict.SourceBranchID = "other-source-identity"
	headConflict.OriginalHead = strings.Repeat("e", 40)
	fixture.ledger.records = append(fixture.ledger.records, branchConflict.WithID(), headConflict.WithID())
	ambiguous := fixture.history[0]
	ambiguous.ID, ambiguous.BranchID = strings.Repeat("3", 32), "reused-source-identity"
	fixture.history = append(fixture.history, ambiguous)
	verified, err := fixture.queries().VerifiedPR(context.Background(), fixture.repo, fixture.history[:2])
	if err != nil || len(verified) != 3 {
		test.Fatalf("conflicting scopes: verified = %+v, error = %v", verified, err)
	}
	if !verified[0].Valid || verified[1].Valid || verified[2].Valid || len(verified[1].Publications) != 0 || len(verified[2].Publications) != 0 {
		test.Fatalf("same capture borrowed another scope's proof: %+v", verified)
	}
	fixture.assertReads(test, "ledger", "metadata", "checkpoint", "participations", "observation")
	fixture.reads.calls = nil
	verified, err = fixture.queries().VerifiedPR(context.Background(), fixture.repo, fixture.history)
	if err != nil || len(verified) != 3 {
		test.Fatalf("ambiguous history: verified = %+v, error = %v", verified, err)
	}
	for _, contribution := range verified {
		if contribution.Valid || len(contribution.Publications) != 0 {
			test.Fatalf("ambiguous branch participation was certified: %+v", contribution)
		}
	}
	fixture.assertReads(test, "ledger", "metadata", "checkpoint", "participations", "observation")
}

func TestVerifiedPRGroupedLookupReuse(test *testing.T) {
	fixture := newVerifiedPRFixture(test)
	publication := fixture.history[1]
	publication.ID = strings.Repeat("3", 32)
	fixture.history = append(fixture.history, publication)
	proof := fixture.evidence.proofs[0]
	proof.PublicationID = publication.ID
	fixture.evidence.proofs = append(fixture.evidence.proofs, proof.WithID())
	record := fixture.ledger.records[0]
	record.DestinationBranchID = "other-destination-identity"
	fixture.ledger.records = append(fixture.ledger.records, record.WithID())
	verified, err := fixture.queries().VerifiedPR(context.Background(), fixture.repo, fixture.history)
	if err != nil || len(verified) != 2 {
		test.Fatalf("grouped proof: verified = %+v, error = %v", verified, err)
	}
	for position, contribution := range verified {
		if contribution.Record != fixture.ledger.records[position] || !contribution.Valid || len(contribution.Publications) != 2 {
			test.Fatalf("grouped publications lost: %+v", contribution)
		}
	}
	fixture.assertReads(test, "ledger", "metadata", "checkpoint", "participations", "observation")
}

func TestVerifiedPRRejectsInvalidAndForeignFacts(test *testing.T) {
	for _, scenario := range []string{"invalid-record", "foreign-record", "foreign-metadata", "invalid-record-other-origin", "foreign-record-other-origin"} {
		fixture := newVerifiedPRFixture(test)
		foreign := domain.ContentHash("sha256:" + strings.Repeat("9", 64))
		switch scenario {
		case "invalid-record", "invalid-record-other-origin":
			fixture.ledger.records[0].ID = "invalid"
		case "foreign-record", "foreign-record-other-origin":
			fixture.ledger.records[0].RepoID = foreign
			fixture.ledger.records[0] = fixture.ledger.records[0].WithID()
			if err := fixture.ledger.records[0].Validate(); err != nil {
				test.Fatal(err)
			}
		case "foreign-metadata":
			fixture.metadata.metadata.ID = foreign
		}
		if strings.HasSuffix(scenario, "other-origin") {
			fixture.metadata.metadata.GitRemoteURL = "https://github.com/acme/other.git"
		}
		verified, err := fixture.queries().VerifiedPR(context.Background(), fixture.repo, fixture.history)
		if !errors.Is(err, domain.ErrIntegrity) || verified != nil {
			test.Fatalf("%s: foreign or malformed fact escaped: verified = %+v, error = %v", scenario, verified, err)
		}
		fixture.assertReads(test, "ledger", "metadata")
	}
}
