package evidencequery

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/contextprojection"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type gitQueryFixture struct {
	test      *testing.T
	repo      domain.Repo
	history   []domain.HistoryEvent
	records   []domain.GitContribution
	proofs    []domain.GitContributionProof
	admission domain.GitContributionAdmission
	head      domain.GitContextHead
	receipt   domain.GitHeadReceipt
	baseline  domain.GitHeadReceipt
	reads     []string
	failRead  string
	readErr   error
}

func gitQueryReceipt(test *testing.T, destination domain.GitDestinationCandidate, version int64, page int, commit string, extra ...domain.GitRefObservation) (domain.GitDestinationCandidate, domain.GitHeadReceipt) {
	test.Helper()
	provider := domain.GitProviderObservation{InstallationID: destination.InstallationID, RepositoryID: destination.ProviderRepositoryID}
	observation := (domain.GitRefObservation{RepoID: destination.RepoID, GitOrigin: destination.GitOrigin, Source: "reconciliation",
		Ref: "refs/heads/" + destination.DestinationBranch, After: commit, Provider: &provider}).WithID()
	claim := domain.GitHeadScan{RepoID: destination.RepoID, GitOrigin: destination.GitOrigin, Version: version, Page: page,
		State: "running", LeaseUntil: time.Unix(500, 0).UTC()}
	receipt, err := domain.NewGitHeadReceipt(claim, append([]domain.GitRefObservation{observation}, extra...), false)
	if err != nil || receipt == nil {
		test.Fatal("invalid receipt fixture", receipt, err)
	}
	destination.ObservationID, destination.DestinationHead = observation.ID, commit
	destination.ReadReceiptID, destination.ReadVersion = receipt.ID, receipt.Version
	return destination.WithID(), *receipt
}

func newGitQueryFixture(test *testing.T) *gitQueryFixture {
	test.Helper()
	repo := domain.Repo{ID: domain.HashContent([]byte("git query repo")), GitRemoteURL: "https://github.com/acme/api.git"}
	admission := domain.GitContributionAdmission{
		Destination: domain.GitDestinationCandidate{RepoID: repo.ID, GitOrigin: repo.GitRemoteURL, InstallationID: 17, ProviderRepositoryID: 29,
			DestinationBranchID: "main-incarnation", DestinationBranch: "main"},
		SourceBranchID: "feature-incarnation", SourceBranch: "feature", SourceAnchorID: strings.Repeat("1", 32), DestinationAnchorID: strings.Repeat("2", 32),
		PublicationID: strings.Repeat("3", 32), Capture: domain.HashContent([]byte("original capture")), OriginalHead: strings.Repeat("a", 40), SourceHead: strings.Repeat("c", 40),
		OrdinaryObservationIDs: []string{strings.Repeat("1", 32)}, CarryObservationIDs: []string{strings.Repeat("4", 32)},
		GraphRevision: 7, EvidenceRevision: 11, AfterReadVersion: 6,
	}
	provider := domain.GitProviderObservation{InstallationID: admission.Destination.InstallationID, RepositoryID: admission.Destination.ProviderRepositoryID}
	source := (domain.GitRefObservation{RepoID: repo.ID, GitOrigin: repo.GitRemoteURL, Source: "reconciliation",
		Ref: "refs/heads/" + admission.SourceBranch, After: admission.SourceHead, Provider: &provider}).WithID()
	destination, baseline := gitQueryReceipt(test, admission.Destination, 4, 1, strings.Repeat("b", 40), source)
	admission.Destination, admission.SourceObservationID = destination, source.ID
	admission = admission.WithID()
	destination, receipt := gitQueryReceipt(test, admission.Destination, 7, 2, strings.Repeat("e", 40))
	head := (domain.GitContextHead{Destination: destination, AnchorID: admission.DestinationAnchorID,
		ContextTarget: domain.HashContent([]byte("destination capture at read")), GraphRevision: 15, EvidenceRevision: 21}).WithID()
	record := (domain.GitContribution{RepoID: repo.ID, GitOrigin: repo.GitRemoteURL, ProviderRepositoryID: 29,
		DestinationBranchID: admission.Destination.DestinationBranchID, SourceBranchID: admission.SourceBranchID,
		Capture: admission.Capture, OriginalHead: admission.OriginalHead, IntegrationSHA: strings.Repeat("d", 40)}).WithID()
	proof := (domain.GitContributionProof{AdmissionID: admission.ID, ContextHeadID: head.ID, Contribution: record}).WithID()
	if !admission.MatchesBaseline(baseline) || !proof.MatchesInputs(admission, head, receipt) {
		test.Fatal("invalid frozen proof fixture")
	}
	ordinary := domain.HistoryEvent{ID: admission.SourceAnchorID, RepoID: string(repo.ID), Kind: "birth",
		BranchID: admission.SourceBranchID, Branch: admission.SourceBranch, Source: admission.Capture, Target: admission.Capture,
		GitAfter: admission.OriginalHead, CreatedAt: time.Unix(100, 0).UTC()}
	destinationBirth := domain.HistoryEvent{ID: admission.DestinationAnchorID, RepoID: string(repo.ID), Kind: "birth",
		BranchID: destination.DestinationBranchID, Branch: destination.DestinationBranch, Target: head.ContextTarget, CreatedAt: time.Unix(101, 0).UTC()}
	publication := ordinary
	publication.ID, publication.Kind = admission.PublicationID, "publish"
	carry := ordinary
	carry.ID, carry.Kind, carry.GitAfter = admission.CarryObservationIDs[0], "position", admission.SourceHead
	supplement := publication
	supplement.ID, supplement.MemoryPinned = strings.Repeat("5", 32), true
	supplement.MemorySource, supplement.MemoryHash = admission.Capture, domain.HashContent([]byte("pinned supplemental memory"))
	return &gitQueryFixture{test: test, repo: repo, admission: admission, head: head, receipt: receipt, baseline: baseline,
		records: []domain.GitContribution{record}, proofs: []domain.GitContributionProof{proof},
		history: []domain.HistoryEvent{ordinary, destinationBirth, publication, carry, supplement}}
}

func (fixture *gitQueryFixture) queries() *Queries {
	fixture.test.Helper()
	queries, err := New(Config{Profile: Compatibility, Metadata: fixture, GitLedger: fixture,
		GitHeads: fixture, GitAdmission: fixture, GitReceipts: fixture})
	if err != nil {
		fixture.test.Fatal(err)
	}
	return queries
}

func (fixture *gitQueryFixture) read(ctx context.Context, repo domain.ContentHash, operation string) error {
	fixture.test.Helper()
	if repo != fixture.admission.Destination.RepoID {
		fixture.test.Fatal("read escaped repository", operation, repo)
	}
	err := ctx.Err()
	fixture.reads = append(fixture.reads, operation)
	if err != nil {
		return err
	}
	if operation == fixture.failRead {
		return fixture.readErr
	}
	return nil
}

func (fixture *gitQueryFixture) assertReads(expected ...string) {
	fixture.test.Helper()
	if !slices.Equal(fixture.reads, expected) {
		fixture.test.Fatalf("reads = %v, want %v", fixture.reads, expected)
	}
}

func (fixture *gitQueryFixture) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	return fixture.repo, fixture.read(ctx, repo, "metadata")
}

func (fixture *gitQueryFixture) ListGitContributions(ctx context.Context, repo domain.ContentHash) ([]domain.GitContribution, error) {
	return fixture.records, fixture.read(ctx, repo, "ledger")
}

func (fixture *gitQueryFixture) ListGitContributionProofs(ctx context.Context, repo domain.ContentHash) ([]domain.GitContributionProof, error) {
	return fixture.proofs, fixture.read(ctx, repo, "proofs")
}

func (fixture *gitQueryFixture) GetGitContributionAdmission(ctx context.Context, repo domain.ContentHash, identity string) (domain.GitContributionAdmission, error) {
	if identity != fixture.proofs[0].AdmissionID {
		fixture.test.Fatal("wrong admission identity", identity)
	}
	return fixture.admission, fixture.read(ctx, repo, "admission")
}

func (fixture *gitQueryFixture) GetGitContextHead(ctx context.Context, repo domain.ContentHash, identity string) (domain.GitContextHead, error) {
	if identity != fixture.proofs[0].ContextHeadID {
		fixture.test.Fatal("wrong head identity", identity)
	}
	return fixture.head, fixture.read(ctx, repo, "head")
}

func (fixture *gitQueryFixture) GetGitHeadReceipt(ctx context.Context, repo domain.ContentHash, origin string, version int64) (domain.GitHeadReceipt, error) {
	if origin != fixture.admission.Destination.GitOrigin {
		fixture.test.Fatal("wrong receipt origin", origin)
	}
	switch version {
	case fixture.head.Destination.ReadVersion:
		return fixture.receipt, fixture.read(ctx, repo, "receipt")
	case fixture.admission.Destination.ReadVersion:
		return fixture.baseline, fixture.read(ctx, repo, "baseline")
	default:
		fixture.test.Fatal("wrong receipt version", version)
		return domain.GitHeadReceipt{}, domain.ErrNotFound
	}
}

func TestVerifiedGitProofAndPinnedSupplement(test *testing.T) {
	fixture := newGitQueryFixture(test)
	unpinned, unproven := fixture.history[4], fixture.history[4]
	unpinned.ID, unpinned.MemoryPinned = strings.Repeat("6", 32), false
	unproven.ID, unproven.LocalBranch = strings.Repeat("7", 32), "other-feature"
	fixture.history = append(fixture.history, unpinned, unproven)
	rows, err := fixture.queries().VerifiedGit(context.Background(), fixture.repo.ID, fixture.history)
	if err != nil || len(rows) != 1 {
		test.Fatal("proof query failed", rows, err)
	}
	row := rows[0]
	if row.Record != fixture.records[0] || !row.Valid || row.SourceBranch != "feature" || row.DestinationBranch != "main" {
		test.Fatal("frozen contribution changed", row)
	}
	if len(row.Publications) != 2 || row.Publications[fixture.history[2].ID] != fixture.history[2] || row.Publications[fixture.history[4].ID] != fixture.history[4] {
		test.Fatal("publication proof or pinned supplement changed", row.Publications)
	}
	fixture.assertReads("ledger", "proofs", "metadata", "admission", "head", "receipt", "baseline")
}

func TestVerifiedGitMissingReaders(test *testing.T) {
	for _, scenario := range []string{"ledger", "empty ledger", "heads", "admission", "receipts"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGitQueryFixture(test)
			queries := fixture.queries()
			switch scenario {
			case "ledger":
				queries.gitLedger = nil
			case "empty ledger":
				fixture.records = nil
			case "heads":
				queries.gitHeads = nil
			case "admission":
				queries.gitAdmission = nil
			case "receipts":
				queries.gitReceipts = nil
			}
			rows, err := queries.VerifiedGit(context.Background(), fixture.repo.ID, fixture.history)
			if err != nil {
				test.Fatal(err)
			}
			if scenario == "ledger" || scenario == "empty ledger" {
				if rows != nil {
					test.Fatal("unsupported or empty ledger returned rows", rows)
				}
			} else if len(rows) != 1 || rows[0].Record != fixture.records[0] || rows[0].Valid || rows[0].Publications == nil || len(rows[0].Publications) != 0 || rows[0].SourceBranch != "" || rows[0].DestinationBranch != "" {
				test.Fatal("missing proof reader changed compatibility rows", rows)
			}
			if scenario == "ledger" {
				fixture.assertReads()
			} else {
				fixture.assertReads("ledger")
			}
		})
	}
}

func TestVerifiedGitCurrentOriginAndDeferredPublication(test *testing.T) {
	for _, scenario := range []string{"current origin", "missing publication", "missing carry", "missing anchor"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGitQueryFixture(test)
			switch scenario {
			case "current origin":
				fixture.repo.GitRemoteURL = "https://github.com/acme/other.git"
			case "missing publication":
				fixture.history[2].ID = strings.Repeat("6", 32)
			case "missing carry":
				fixture.history[3].GitAfter = strings.Repeat("f", 40)
			case "missing anchor":
				fixture.history[0].Kind = "attach"
			}
			rows, err := fixture.queries().VerifiedGit(context.Background(), fixture.repo.ID, fixture.history)
			if err != nil || len(rows) != 1 || rows[0].Record != fixture.records[0] || rows[0].Valid || len(rows[0].Publications) != 0 || rows[0].SourceBranch != "feature" || rows[0].DestinationBranch != "main" {
				test.Fatal("deferred proof was admitted or lost its historical names", rows, err)
			}
			fixture.assertReads("ledger", "proofs", "metadata", "admission", "head", "receipt")
		})
	}
}

func TestVerifiedGitRejectsRecordAndProofMismatch(test *testing.T) {
	for _, scenario := range []string{"record hash", "foreign record", "unknown proof record", "record proof mismatch", "proof hash", "publication mismatch", "receipt page", "baseline page"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGitQueryFixture(test)
			reads := []string{"ledger", "proofs", "metadata", "admission", "head", "receipt"}
			switch scenario {
			case "record hash":
				fixture.records[0].Capture = domain.HashContent([]byte("tampered capture"))
				reads = reads[:1]
			case "foreign record":
				fixture.records[0].RepoID = domain.HashContent([]byte("foreign repo"))
				fixture.records[0] = fixture.records[0].WithID()
				reads = reads[:1]
			case "unknown proof record":
				fixture.proofs[0].Contribution.Capture = domain.HashContent([]byte("other capture"))
				fixture.proofs[0].Contribution = fixture.proofs[0].Contribution.WithID()
				fixture.proofs[0] = fixture.proofs[0].WithID()
				reads = reads[:3]
			case "record proof mismatch":
				fixture.proofs[0].Contribution.IntegrationSHA = strings.Repeat("f", 40)
				reads = reads[:3]
			case "proof hash":
				fixture.proofs[0].ID = strings.Repeat("f", 64)
			case "publication mismatch":
				fixture.history[0].LocalBranch, fixture.history[2].LocalBranch = "other-feature", "other-feature"
			case "receipt page":
				fixture.receipt.Page++
				fixture.receipt = fixture.receipt.WithID()
			case "baseline page":
				fixture.baseline.Page++
				fixture.baseline = fixture.baseline.WithID()
				reads = append(reads, "baseline")
			}
			rows, err := fixture.queries().VerifiedGit(context.Background(), fixture.repo.ID, fixture.history)
			if !errors.Is(err, domain.ErrIntegrity) || rows != nil {
				test.Fatal("invalid proof returned partial rows", rows, err)
			}
			fixture.assertReads(reads...)
		})
	}
}

func TestVerifiedGitSnapshotValidation(test *testing.T) {
	for _, scenario := range []string{"foreign metadata", "invalid history", "competing bindings"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGitQueryFixture(test)
			repo := fixture.repo.ID
			switch scenario {
			case "foreign metadata":
				fixture.repo.ID = domain.HashContent([]byte("foreign metadata repo"))
			case "invalid history":
				fixture.history[4].MemoryHash = "invalid"
			case "competing bindings":
				fixture.history[1].Branch = fixture.history[0].Branch
			}
			fixture.proofs = nil
			rows, err := fixture.queries().VerifiedGit(context.Background(), repo, fixture.history)
			if err == nil || rows != nil || scenario != "competing bindings" && !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("snapshot validation bypassed without proofs", rows, err)
			}
			fixture.assertReads("ledger", "proofs", "metadata")
		})
	}
}

func TestVerifiedGitLimits(test *testing.T) {
	for _, scenario := range []string{"records", "proofs"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGitQueryFixture(test)
			if scenario == "records" {
				fixture.records = make([]domain.GitContribution, contextprojection.MaxProjectionSnapshots+1)
			} else {
				fixture.proofs = make([]domain.GitContributionProof, contextprojection.MaxProjectionSnapshots+1)
			}
			rows, err := fixture.queries().VerifiedGit(context.Background(), fixture.repo.ID, fixture.history)
			if !errors.Is(err, domain.ErrMemoryProjectionLimit) || rows != nil {
				test.Fatal("projection limit returned partial rows", rows, err)
			}
			if scenario == "records" {
				fixture.assertReads("ledger")
			} else {
				fixture.assertReads("ledger", "proofs")
			}
		})
	}
}

func TestVerifiedGitPropagatesReadErrors(test *testing.T) {
	reads := []string{"ledger", "proofs", "metadata", "admission", "head", "receipt", "baseline"}
	for position, operation := range reads {
		test.Run(operation, func(test *testing.T) {
			fixture := newGitQueryFixture(test)
			cause := domain.ErrIntegrity
			if operation == "admission" || operation == "baseline" {
				cause = domain.ErrNotFound
			} else if operation == "head" {
				cause = domain.ErrConflict
			}
			fixture.failRead, fixture.readErr = operation, fmt.Errorf("read %s: %w", operation, cause)
			rows, err := fixture.queries().VerifiedGit(context.Background(), fixture.repo.ID, fixture.history)
			if err != fixture.readErr || rows != nil {
				test.Fatal("reader error was swallowed or returned partial rows", rows, err)
			}
			fixture.assertReads(reads[:position+1]...)
		})
	}
}

type gitQueryReadCancellation struct {
	context.Context
	reads     *[]string
	afterRead string
	done      chan struct{}
}

func (ctx *gitQueryReadCancellation) Done() <-chan struct{} {
	return ctx.done
}

func (ctx *gitQueryReadCancellation) Err() error {
	reads := *ctx.reads
	if len(reads) > 0 && reads[len(reads)-1] == ctx.afterRead {
		select {
		case <-ctx.done:
		default:
			close(ctx.done)
		}
	}
	select {
	case <-ctx.done:
		return context.Canceled
	default:
		return ctx.Context.Err()
	}
}

func TestVerifiedGitCancellation(test *testing.T) {
	for _, scenario := range []string{"before ledger", "snapshot", "next proof", "supplement"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGitQueryFixture(test)
			ctx := &gitQueryReadCancellation{Context: context.Background(), reads: &fixture.reads, afterRead: "baseline", done: make(chan struct{})}
			switch scenario {
			case "before ledger":
				close(ctx.done)
			case "snapshot":
				ctx.afterRead = "metadata"
			case "next proof":
				fixture.proofs = append(fixture.proofs, domain.GitContributionProof{})
			}
			rows, err := fixture.queries().VerifiedGit(ctx, fixture.repo.ID, fixture.history)
			if !errors.Is(err, context.Canceled) || rows != nil {
				test.Fatal("cancellation returned partial contributions", rows, err)
			}
			switch scenario {
			case "before ledger":
				fixture.assertReads("ledger")
			case "snapshot":
				fixture.assertReads("ledger", "proofs", "metadata")
			default:
				fixture.assertReads("ledger", "proofs", "metadata", "admission", "head", "receipt", "baseline")
			}
		})
	}
}
