//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type gitContextHeadPGFixture struct {
	ctx          context.Context
	admission    domain.GitContributionAdmission
	claim        domain.GitHeadScan
	observations []domain.GitRefObservation
	receipt      domain.GitHeadReceipt
	head         domain.GitContextHead
	proof        domain.GitContributionProof
	now          time.Time
}

func gitContextHeadForReceiptPG(receipt domain.GitHeadReceipt, branch, identity, anchor string) domain.GitContextHead {
	for _, observed := range receipt.Heads {
		if observed.Ref != "refs/heads/"+branch {
			continue
		}
		destination := domain.GitDestinationCandidate{RepoID: receipt.RepoID, GitOrigin: receipt.GitOrigin,
			InstallationID: receipt.Provider.InstallationID, ProviderRepositoryID: receipt.Provider.RepositoryID,
			ObservationID: observed.ObservationID, ReadReceiptID: receipt.ID, ReadVersion: receipt.Version,
			DestinationBranchID: identity, DestinationBranch: branch, DestinationHead: observed.Commit}.WithID()
		return domain.GitContextHead{Destination: destination, AnchorID: anchor, ContextTarget: domain.HashContent([]byte("frozen context " + branch)), GraphRevision: 3, EvidenceRevision: 7}.WithID()
	}
	return domain.GitContextHead{}
}

func newGitContextHeadPGFixture(t *testing.T, storage *PostgresStore, more bool) gitContextHeadPGFixture {
	t.Helper()
	ctx, admission, now := gitAdmissionPGFixture(t, storage)
	repo, origin := admission.Destination.RepoID, admission.Destination.GitOrigin
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		return storage.PutGitContributionAdmission(bound, admission)
	}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(6 * time.Minute)
	claim, err := storage.ClaimGitHeadScan(ctx, repo, origin, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	provider := &domain.GitProviderObservation{InstallationID: admission.Destination.InstallationID, RepositoryID: admission.Destination.ProviderRepositoryID}
	observations := []domain.GitRefObservation{
		{RepoID: repo, GitOrigin: origin, Source: "reconciliation", Ref: "refs/heads/main", After: strings.Repeat("e", 40), Provider: provider},
		{RepoID: repo, GitOrigin: origin, Source: "reconciliation", Ref: "refs/heads/feature", After: admission.SourceHead, Provider: provider},
	}
	for index := range observations {
		observations[index] = observations[index].WithID()
	}
	receipt, err := domain.NewGitHeadReceipt(claim, observations, more)
	if err != nil || receipt == nil {
		t.Fatal(receipt, err)
	}
	head := gitContextHeadForReceiptPG(*receipt, "main", admission.Destination.DestinationBranchID, admission.DestinationAnchorID)
	contribution := domain.GitContribution{RepoID: repo, GitOrigin: origin, ProviderRepositoryID: provider.RepositoryID,
		DestinationBranchID: admission.Destination.DestinationBranchID, SourceBranchID: admission.SourceBranchID,
		Capture: admission.Capture, OriginalHead: admission.OriginalHead, IntegrationSHA: strings.Repeat("e", 40)}.WithID()
	proof := domain.GitContributionProof{AdmissionID: admission.ID, ContextHeadID: head.ID, Contribution: contribution}.WithID()
	if head.Validate() != nil || proof.Validate() != nil || !proof.MatchesInputs(admission, head, *receipt) {
		t.Fatal("invalid context head/proof fixture")
	}
	return gitContextHeadPGFixture{ctx, admission, claim, observations, *receipt, head, proof, now}
}

func (fixture gitContextHeadPGFixture) finish(ctx context.Context, storage *PostgresStore) error {
	return storage.WithinRepository(ctx, fixture.receipt.RepoID, func(bound context.Context) error {
		if err := storage.FinishGitHeadScan(bound, fixture.claim, fixture.observations, fixture.receipt.More, fixture.now); err != nil {
			return err
		}
		return storage.PutGitContextHeads(bound, fixture.receipt, []domain.GitContextHead{fixture.head})
	})
}

func putGitContextHeadsPG(ctx context.Context, storage *PostgresStore, receipt domain.GitHeadReceipt, heads ...domain.GitContextHead) error {
	return storage.WithinRepository(ctx, receipt.RepoID, func(bound context.Context) error {
		return storage.PutGitContextHeads(bound, receipt, heads)
	})
}

func putGitProofPG(ctx context.Context, storage *PostgresStore, proof domain.GitContributionProof) error {
	return storage.WithinRepository(ctx, proof.Contribution.RepoID, func(bound context.Context) error {
		return storage.PutGitContributionProof(bound, proof)
	})
}

func TestPGGitContextHeadsImmutableAndSavepoint(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	for _, more := range []bool{false, true} {
		t.Run(fmt.Sprint(more), func(t *testing.T) {
			fixture := newGitContextHeadPGFixture(t, storage, more)
			ctx, repo := fixture.ctx, fixture.receipt.RepoID
			before, err := storage.RepositoryRevision(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.finish(ctx, storage); err != nil {
				t.Fatal(err)
			}
			if err := putGitContextHeadsPG(ctx, storage, fixture.receipt, fixture.head); err != nil {
				t.Fatal("identical replay rejected", err)
			}
			if got, err := peer.GetGitContextHead(ctx, repo, fixture.head.ID); err != nil || got != fixture.head {
				t.Fatal("head read changed immutable binding", got, err)
			}
			changed := fixture.head
			changed.ContextTarget = domain.HashContent([]byte("changed context"))
			changed = changed.WithID()
			feature := gitContextHeadForReceiptPG(fixture.receipt, "feature", "feature-identity", strings.Repeat("6", 32))
			if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
				if err := storage.PutGitContextHeads(bound, fixture.receipt, []domain.GitContextHead{feature, changed}); !errors.Is(err, domain.ErrConflict) {
					t.Fatal("receipt/branch collision silently ignored", err)
				}
				if _, err := storage.GetGitContextHead(bound, repo, feature.ID); !errors.Is(err, domain.ErrNotFound) {
					t.Fatal("first head escaped failed batch savepoint", err)
				}
				return storage.PutGitContextHeads(bound, fixture.receipt, []domain.GitContextHead{feature})
			}); err != nil {
				t.Fatal("failed batch poisoned outer transaction", err)
			}
			renamedIdentity := fixture.head
			renamedIdentity.Destination.DestinationBranchID = "other-identity"
			renamedIdentity.Destination = renamedIdentity.Destination.WithID()
			if err := putGitContextHeadsPG(ctx, storage, fixture.receipt, renamedIdentity.WithID()); !errors.Is(err, domain.ErrConflict) {
				t.Fatal("same ref accepted a second incarnation in one read", err)
			}
			if after, err := peer.RepositoryRevision(ctx, repo); err != nil || after != before {
				t.Fatal("context-head storage advanced public revisions", before, after, err)
			}
		})
	}
}

func TestPGGitContextHeadsRollbackIncludesProofAndPin(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			fixture := newGitContextHeadPGFixture(t, storage, false)
			ctx, repo := fixture.ctx, fixture.receipt.RepoID
			pin := domain.GitContributionMemory{RepoID: repo, ContributionID: fixture.proof.Contribution.ID,
				PublicationID: fixture.admission.PublicationID, MemoryHash: domain.HashContent([]byte("original pinned memory")),
				MemorySource: fixture.admission.Capture, MemoryPinned: true}.WithID()
			before, err := storage.RepositoryRevision(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			write := func(bound context.Context) error {
				if err := fixture.finish(bound, storage); err != nil {
					return err
				}
				if err := storage.PutGitContribution(bound, fixture.proof.Contribution); err != nil {
					return err
				}
				if err := storage.PutGitContributionProof(bound, fixture.proof); err != nil {
					return err
				}
				if proofs, err := storage.ListGitContributionProofs(bound, repo); err != nil || !reflect.DeepEqual(proofs, []domain.GitContributionProof{fixture.proof}) {
					t.Fatal("transaction cannot read its proof", proofs, err)
				}
				return storage.PutGitContributionMemory(bound, pin)
			}
			abort := errors.New("abort context head/proof publication")
			work, cancel := context.WithCancel(ctx)
			defer cancel()
			err = storage.WithinRepository(work, repo, func(bound context.Context) error {
				if err := write(bound); err != nil {
					return err
				}
				if _, err := peer.GetGitContextHead(ctx, repo, fixture.head.ID); !errors.Is(err, domain.ErrNotFound) {
					t.Fatal("uncommitted head visible", err)
				}
				if proofs, err := peer.ListGitContributionProofs(ctx, repo); err != nil || proofs == nil || len(proofs) != 0 {
					t.Fatal("uncommitted proof visible", proofs, err)
				}
				if canceled {
					cancel()
					return work.Err()
				}
				return abort
			})
			want := abort
			if canceled {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatal("outer failure lost", err)
			}
			for _, table := range []string{"git_context_heads", "git_contribution_proofs", "git_contributions", "git_contribution_memories"} {
				var count int
				if err := peer.pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE repo_id=$1", repo).Scan(&count); err != nil || count != 0 {
					t.Fatal("row escaped rollback", table, count, err)
				}
			}
			if _, err := peer.GetGitHeadReceipt(ctx, repo, fixture.receipt.GitOrigin, fixture.receipt.Version); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("receipt escaped rollback", err)
			}
			if claim, err := peer.GetGitHeadScan(ctx, repo, fixture.receipt.GitOrigin); err != nil || !reflect.DeepEqual(claim, fixture.claim) {
				t.Fatal("head finish escaped rollback", claim, err)
			}
			if err := storage.WithinRepository(ctx, repo, write); err != nil {
				t.Fatal("retry failed", err)
			}
			if pins, err := peer.ListGitContributionMemories(ctx, repo); err != nil || !reflect.DeepEqual(pins, []domain.GitContributionMemory{pin}) {
				t.Fatal("original publication pin changed", pins, err)
			}
			if proofs, err := peer.ListGitContributionProofs(ctx, repo); err != nil || len(proofs) != 1 || proofs[0].Contribution.OriginalHead != fixture.admission.OriginalHead || proofs[0].Contribution.OriginalHead == fixture.admission.SourceHead {
				t.Fatal("proof relabeled original capture as carried head", proofs, err)
			}
			if after, err := peer.RepositoryRevision(ctx, repo); err != nil || after != before {
				t.Fatal("storage advanced revisions", before, after, err)
			}
		})
	}
}

func TestPGGitContextHeadsStaleAndForgedReceipts(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	for _, scenario := range []string{"unfinished", "stale", "forged-provider", "wrong-repo", "wrong-origin", "wrong-version", "wrong-observation", "duplicate-identity", "duplicate-ref", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newGitContextHeadPGFixture(t, storage, false)
			ctx, receipt, head := fixture.ctx, fixture.receipt, fixture.head
			if scenario != "unfinished" {
				if err := fixture.finish(ctx, storage); err != nil {
					t.Fatal(err)
				}
			}
			want := domain.ErrIntegrity
			heads := []domain.GitContextHead{head}
			switch scenario {
			case "unfinished":
				want = domain.ErrConflict
			case "stale":
				if _, err := storage.ClaimGitHeadScan(ctx, receipt.RepoID, receipt.GitOrigin, fixture.now.Add(6*time.Minute), time.Minute); err != nil {
					t.Fatal(err)
				}
				want = domain.ErrConflict
			case "forged-provider":
				receipt.Provider.InstallationID++
				for index := range receipt.Heads {
					observed := &receipt.Heads[index]
					observed.ObservationID = (domain.GitRefObservation{RepoID: receipt.RepoID, GitOrigin: receipt.GitOrigin, Source: "reconciliation", Ref: observed.Ref, After: observed.Commit, Provider: &receipt.Provider}).WithID().ID
				}
				receipt = receipt.WithID()
				heads[0] = gitContextHeadForReceiptPG(receipt, "main", head.Destination.DestinationBranchID, head.AnchorID)
			case "wrong-repo", "wrong-origin", "wrong-version", "wrong-observation":
				switch scenario {
				case "wrong-repo":
					head.Destination.RepoID = domain.HashContent([]byte("foreign head"))
				case "wrong-origin":
					head.Destination.GitOrigin += "-other"
				case "wrong-version":
					head.Destination.ReadVersion++
				case "wrong-observation":
					head.Destination.ObservationID = strings.Repeat("f", 64)
				}
				head.Destination = head.Destination.WithID()
				heads[0] = head.WithID()
			case "duplicate-identity":
				heads = append(heads, gitContextHeadForReceiptPG(receipt, "feature", head.Destination.DestinationBranchID, head.AnchorID))
				want = domain.ErrValidation
			case "duplicate-ref":
				heads = append(heads, gitContextHeadForReceiptPG(receipt, "main", "other-main", head.AnchorID))
				want = domain.ErrValidation
			case "oversized":
				heads = make([]domain.GitContextHead, 101)
				want = domain.ErrValidation
			}
			if err := putGitContextHeadsPG(ctx, storage, receipt, heads...); !errors.Is(err, want) {
				t.Fatal("invalid or stale binding accepted", err, want)
			}
		})
	}
}

func TestPGGitContextHeadsPaginationAndOccurrences(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	fixture := newGitContextHeadPGFixture(t, storage, false)
	ctx, repo, origin := fixture.ctx, fixture.receipt.RepoID, fixture.receipt.GitOrigin
	if err := fixture.finish(ctx, storage); err != nil {
		t.Fatal(err)
	}
	want := []domain.GitContextHead{fixture.head}
	for index := 1; index <= 2; index++ {
		now := fixture.now.Add(time.Duration(index) * 6 * time.Minute)
		claim, err := storage.ClaimGitHeadScan(ctx, repo, origin, now, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		receipt, err := domain.NewGitHeadReceipt(claim, fixture.observations, false)
		if err != nil {
			t.Fatal(err)
		}
		head := gitContextHeadForReceiptPG(*receipt, "main", fixture.head.Destination.DestinationBranchID, fixture.head.AnchorID)
		if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
			if err := storage.FinishGitHeadScan(bound, claim, fixture.observations, false, now); err != nil {
				return err
			}
			return storage.PutGitContextHeads(bound, *receipt, []domain.GitContextHead{head})
		}); err != nil {
			t.Fatal(err)
		}
		if head.ID == fixture.head.ID || head.Destination.ObservationID != fixture.head.Destination.ObservationID {
			t.Fatal("same-head occurrences lost immutable/read identity")
		}
		want = append(want, head)
	}
	got := []domain.GitContextHead{}
	for after := int64(0); ; {
		page, err := peer.ListGitContextHeads(ctx, repo, origin, fixture.head.Destination.DestinationBranchID, after, 1)
		if err != nil || page == nil || len(page) > 1 {
			t.Fatal(page, err)
		}
		if len(page) == 0 {
			break
		}
		got = append(got, page[0])
		after = page[0].Destination.ReadVersion
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("version pagination lost or reordered heads", got, want)
	}
	for _, query := range []struct {
		repo           domain.ContentHash
		origin, branch string
	}{
		{domain.HashContent([]byte("foreign query")), origin, fixture.head.Destination.DestinationBranchID},
		{repo, origin + "-other", fixture.head.Destination.DestinationBranchID},
		{repo, origin, "other-branch"},
	} {
		if rows, err := peer.ListGitContextHeads(ctx, query.repo, query.origin, query.branch, 0, 101); err != nil || rows == nil || len(rows) != 0 {
			t.Fatal("head list leaked scope", rows, err)
		}
	}
	if _, err := peer.GetGitContextHead(ctx, domain.HashContent([]byte("foreign query")), fixture.head.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("head getter leaked repository", err)
	}
}

func TestPGGitContextHeadAndProofTransactionPolicy(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	fixture := newGitContextHeadPGFixture(t, storage, false)
	ctx, repo := fixture.ctx, fixture.receipt.RepoID
	if err := fixture.finish(ctx, storage); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutGitContribution(ctx, fixture.proof.Contribution); err != nil {
		t.Fatal(err)
	}
	assertRejected := func(bound context.Context, writer *PostgresStore, want error) {
		t.Helper()
		if err := writer.PutGitContextHeads(bound, fixture.receipt, []domain.GitContextHead{fixture.head}); !errors.Is(err, want) {
			t.Fatal("head transaction/policy fence bypassed", err, want)
		}
		if err := writer.PutGitContributionProof(bound, fixture.proof); !errors.Is(err, want) {
			t.Fatal("proof transaction/policy fence bypassed", err, want)
		}
	}
	assertRejected(ctx, storage, domain.ErrConflict)
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		assertRejected(bound, storage, domain.ErrConflict)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	foreign := domain.HashContent([]byte("foreign transaction"))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: foreign}); err != nil {
		t.Fatal(err)
	}
	if err := storage.WithinRepository(ctx, foreign, func(bound context.Context) error {
		assertRejected(bound, storage, domain.ErrConflict)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var escaped context.Context
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		escaped = bound
		assertRejected(bound, peer, domain.ErrConflict)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertRejected(escaped, storage, domain.ErrConflict)
	if _, err := storage.pool.Exec(ctx, `UPDATE repos SET git_remote_url=$2 WHERE id=$1`, repo, fixture.receipt.GitOrigin+"-other"); err != nil {
		t.Fatal(err)
	}
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		assertRejected(bound, storage, domain.ErrConflict)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE repos SET git_remote_url=$2 WHERE id=$1`, repo, fixture.receipt.GitOrigin); err != nil {
		t.Fatal(err)
	}
	if err := storage.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		assertRejected(bound, storage, domain.ErrDocumentIdentityUpgradeRequired)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	supported := []domain.DocumentIdentity{domain.DocumentIdentityRootV1}
	compatible := outbound.WithDocumentIdentityCompatibility(ctx, supported, supported)
	if err := putGitProofPG(compatible, storage, fixture.proof); err != nil {
		t.Fatal("qualified compatible proof rejected", err)
	}
}

func TestPGGitContributionProofRequiresExistingExactLinks(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	for _, scenario := range []string{"missing-admission", "missing-head", "missing-contribution", "wrong-repo", "wrong-source", "wrong-capture", "relabel-original", "watermark", "altered-id"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newGitContextHeadPGFixture(t, storage, false)
			ctx, proof := fixture.ctx, fixture.proof
			if err := fixture.finish(ctx, storage); err != nil {
				t.Fatal(err)
			}
			want := domain.ErrIntegrity
			switch scenario {
			case "missing-admission":
				proof.AdmissionID, want = strings.Repeat("f", 64), domain.ErrNotFound
			case "missing-head":
				proof.ContextHeadID, want = strings.Repeat("f", 64), domain.ErrNotFound
			case "missing-contribution":
				want = domain.ErrNotFound
			case "wrong-repo":
				proof.Contribution.RepoID = domain.HashContent([]byte("foreign proof"))
				if _, err := storage.PutRepo(ctx, domain.Repo{ID: proof.Contribution.RepoID, GitRemoteURL: proof.Contribution.GitOrigin}); err != nil {
					t.Fatal(err)
				}
				want = domain.ErrNotFound
			case "wrong-source":
				proof.Contribution.SourceBranchID = "other-source"
			case "wrong-capture":
				proof.Contribution.Capture = domain.HashContent([]byte("other capture"))
			case "relabel-original":
				proof.Contribution.OriginalHead = fixture.admission.SourceHead
			case "watermark":
				admission := fixture.admission
				admission.PublicationID, admission.AfterReadVersion = strings.Repeat("9", 32), fixture.receipt.Version
				admission = admission.WithID()
				if err := storage.WithinRepository(ctx, fixture.receipt.RepoID, func(bound context.Context) error {
					return storage.PutGitContributionAdmission(bound, admission)
				}); err != nil {
					t.Fatal(err)
				}
				proof.AdmissionID = admission.ID
			}
			proof.Contribution = proof.Contribution.WithID()
			proof = proof.WithID()
			if scenario == "altered-id" {
				proof.ContextHeadID = strings.Repeat("f", 64)
			}
			if scenario != "missing-contribution" {
				if err := storage.PutGitContribution(ctx, proof.Contribution); err != nil {
					t.Fatal(err)
				}
			}
			if err := putGitProofPG(ctx, storage, proof); !errors.Is(err, want) {
				t.Fatal("forged proof links accepted", err, want)
			}
			if proofs, err := storage.ListGitContributionProofs(ctx, proof.Contribution.RepoID); err != nil || proofs == nil || len(proofs) != 0 {
				t.Fatal("invalid proof persisted", proofs, err)
			}
		})
	}
}

func TestPGGitContextHeadAndProofConcurrentDuplicates(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	for _, collision := range []bool{false, true} {
		t.Run(fmt.Sprint(collision), func(t *testing.T) {
			fixture := newGitContextHeadPGFixture(t, storage, false)
			ctx, cancel := context.WithTimeout(fixture.ctx, 10*time.Second)
			defer cancel()
			if err := storage.FinishGitHeadScan(ctx, fixture.claim, fixture.observations, false, fixture.now); err != nil {
				t.Fatal(err)
			}
			type result struct {
				head domain.GitContextHead
				err  error
			}
			ready, release, results := make(chan struct{}, 2), make(chan struct{}), make(chan result, 2)
			for index, writer := range []*PostgresStore{storage, peer} {
				go func() {
					head := fixture.head
					if collision && index == 1 {
						head.GraphRevision++
						head = head.WithID()
					}
					ready <- struct{}{}
					<-release
					results <- result{head, putGitContextHeadsPG(ctx, writer, fixture.receipt, head)}
				}()
			}
			<-ready
			<-ready
			close(release)
			accepted := 0
			var winner domain.GitContextHead
			for range 2 {
				select {
				case got := <-results:
					if got.err == nil {
						accepted++
						winner = got.head
					} else if !collision || !errors.Is(got.err, domain.ErrConflict) {
						t.Fatal(got.err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			want := 2
			if collision {
				want = 1
			}
			if accepted != want {
				t.Fatal("wrong immutable collision outcome", accepted, want)
			}
			proof := fixture.proof
			proof.ContextHeadID = winner.ID
			proof = proof.WithID()
			if err := storage.PutGitContribution(ctx, proof.Contribution); err != nil {
				t.Fatal(err)
			}
			proofResults := make(chan error, 2)
			for _, writer := range []*PostgresStore{storage, peer} {
				go func() { proofResults <- putGitProofPG(ctx, writer, proof) }()
			}
			for range 2 {
				select {
				case err := <-proofResults:
					if err != nil {
						t.Fatal("duplicate proof rejected", err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if proofs, err := peer.ListGitContributionProofs(ctx, fixture.receipt.RepoID); err != nil || !reflect.DeepEqual(proofs, []domain.GitContributionProof{proof}) {
				t.Fatal("duplicate proof duplicated immutable rows", proofs, err)
			}
			if _, err := storage.ClaimGitHeadScan(ctx, fixture.receipt.RepoID, fixture.receipt.GitOrigin, fixture.now.Add(6*time.Minute), time.Minute); err != nil {
				t.Fatal(err)
			}
			if err := putGitProofPG(ctx, storage, proof); err != nil {
				t.Fatal("historical proof incorrectly required newest provider read", err)
			}
		})
	}
}

func TestPGGitContextHeadAndProofReadLinkTampering(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	for _, scenario := range []string{"head-column", "head-payload", "receipt-payload", "baseline-payload", "admission-payload", "contribution-payload", "proof-payload", "proof-column"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newGitContextHeadPGFixture(t, storage, false)
			ctx, repo := fixture.ctx, fixture.receipt.RepoID
			if err := fixture.finish(ctx, storage); err != nil {
				t.Fatal(err)
			}
			if err := storage.PutGitContribution(ctx, fixture.proof.Contribution); err != nil {
				t.Fatal(err)
			}
			if err := putGitProofPG(ctx, storage, fixture.proof); err != nil {
				t.Fatal(err)
			}
			var err error
			switch scenario {
			case "head-column":
				_, err = storage.pool.Exec(ctx, `UPDATE git_context_heads SET destination_branch='wrong' WHERE repo_id=$1`, repo)
			case "head-payload":
				_, err = storage.pool.Exec(ctx, `UPDATE git_context_heads SET payload=jsonb_set(payload,'{graph_revision}','"99"') WHERE repo_id=$1`, repo)
			case "receipt-payload", "baseline-payload":
				version := fixture.receipt.Version
				if scenario == "baseline-payload" {
					version = fixture.admission.Destination.ReadVersion
				}
				_, err = storage.pool.Exec(ctx, `UPDATE git_head_receipts SET payload=jsonb_set(payload,'{page}','99') WHERE repo_id=$1 AND version=$2`, repo, version)
			case "admission-payload":
				_, err = storage.pool.Exec(ctx, `UPDATE git_contribution_admissions SET payload=jsonb_set(payload,'{source_branch_id}','"wrong"') WHERE repo_id=$1`, repo)
			case "contribution-payload":
				_, err = storage.pool.Exec(ctx, `UPDATE git_contributions SET payload=jsonb_set(payload,'{source_branch_id}','"wrong"') WHERE repo_id=$1`, repo)
			case "proof-payload":
				_, err = storage.pool.Exec(ctx, `UPDATE git_contribution_proofs SET payload=jsonb_set(payload,'{admission_id}',to_jsonb($2::text)) WHERE repo_id=$1`, repo, strings.Repeat("f", 64))
			case "proof-column":
				other := fixture.proof.Contribution
				other.IntegrationSHA = strings.Repeat("d", 40)
				other = other.WithID()
				if err := storage.PutGitContribution(ctx, other); err != nil {
					t.Fatal(err)
				}
				_, err = storage.pool.Exec(ctx, `UPDATE git_contribution_proofs SET contribution_id=$2 WHERE repo_id=$1`, repo, other.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := storage.ListGitContributionProofs(ctx, repo); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatal("corrupt proof link accepted by list", err)
			}
			if _, err := storage.FindGitContributionProof(ctx, repo, fixture.admission.ID); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatal("corrupt proof link accepted by lookup", err)
			}
			if err := putGitProofPG(ctx, storage, fixture.proof); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatal("proof replay bypassed link validation", err)
			}
			if scenario == "head-column" || scenario == "head-payload" || scenario == "receipt-payload" {
				if _, err := storage.GetGitContextHead(ctx, repo, fixture.head.ID); !errors.Is(err, domain.ErrIntegrity) {
					t.Fatal("corrupt head returned", err)
				}
				if _, err := storage.ListGitContextHeads(ctx, repo, fixture.receipt.GitOrigin, fixture.head.Destination.DestinationBranchID, 0, 10); !errors.Is(err, domain.ErrIntegrity) {
					t.Fatal("corrupt head listed", err)
				}
			}
		})
	}
}

func TestPGGitContextHeadAndProofValidationCancellation(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	fixture := newGitContextHeadPGFixture(t, storage, false)
	ctx, repo, origin, branch := fixture.ctx, fixture.receipt.RepoID, fixture.receipt.GitOrigin, fixture.head.Destination.DestinationBranchID
	for _, query := range []struct {
		repo           domain.ContentHash
		origin, branch string
		after          int64
		limit          int
	}{
		{"invalid", origin, branch, 0, 1}, {repo, "", branch, 0, 1}, {repo, origin, "", 0, 1},
		{repo, origin, " bad", 0, 1}, {repo, origin, branch, -1, 1}, {repo, origin, branch, 0, 0}, {repo, origin, branch, 0, 102},
	} {
		if _, err := storage.ListGitContextHeads(ctx, query.repo, query.origin, query.branch, query.after, query.limit); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid head query accepted", query, err)
		}
	}
	for _, id := range []string{"", "short", strings.Repeat("A", 64)} {
		if _, err := storage.GetGitContextHead(ctx, repo, id); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid head ID accepted", err)
		}
		if _, err := storage.FindGitContributionProof(ctx, repo, id); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid admission proof ID accepted", err)
		}
	}
	if _, err := storage.ListGitContributionProofs(ctx, "invalid"); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("invalid proof scope accepted", err)
	}
	if proofs, err := storage.ListGitContributionProofs(ctx, domain.HashContent([]byte("foreign proofs"))); err != nil || proofs == nil || len(proofs) != 0 {
		t.Fatal("proof list leaked repository", proofs, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for _, operation := range []func() error{
		func() error {
			return storage.PutGitContextHeads(canceled, fixture.receipt, []domain.GitContextHead{fixture.head})
		},
		func() error { _, err := storage.GetGitContextHead(canceled, repo, fixture.head.ID); return err },
		func() error { _, err := storage.ListGitContextHeads(canceled, repo, origin, branch, 0, 1); return err },
		func() error { return storage.PutGitContributionProof(canceled, fixture.proof) },
		func() error { _, err := storage.ListGitContributionProofs(canceled, repo); return err },
		func() error {
			_, err := storage.FindGitContributionProof(canceled, repo, fixture.admission.ID)
			return err
		},
	} {
		if err := operation(); !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation ignored", err)
		}
	}
}

func TestPGGitContributionProofFindAndAdmissionUniqueness(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	fixture := newGitContextHeadPGFixture(t, storage, false)
	ctx, repo := fixture.ctx, fixture.receipt.RepoID
	if _, err := storage.FindGitContributionProof(ctx, repo, fixture.admission.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("missing proof invented", err)
	}
	if err := fixture.finish(ctx, storage); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutGitContribution(ctx, fixture.proof.Contribution); err != nil {
		t.Fatal(err)
	}
	other := fixture.admission
	other.PublicationID, other.AfterReadVersion = strings.Repeat("9", 32), fixture.receipt.Version
	other = other.WithID()
	putGitContributionWorkAdmissionPG(t, storage, ctx, other)
	later := advanceGitContributionHeadTestPG(t, storage, fixture, "main", fixture.admission.Destination.DestinationBranchID)
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	type result struct {
		proof domain.GitContributionProof
		err   error
	}
	ready, release, results := make(chan struct{}, 2), make(chan struct{}), make(chan result, 2)
	for index, writer := range []*PostgresStore{storage, peer} {
		go func() {
			proof := fixture.proof
			if index == 1 {
				proof = later.proof
			}
			ready <- struct{}{}
			<-release
			results <- result{proof, putGitProofPG(bounded, writer, proof)}
		}()
	}
	<-ready
	<-ready
	close(release)
	winners := 0
	var winner domain.GitContributionProof
	for range 2 {
		select {
		case got := <-results:
			if got.err == nil {
				winners++
				winner = got.proof
			} else if !errors.Is(got.err, domain.ErrConflict) {
				t.Fatal(got.err)
			}
		case <-bounded.Done():
			t.Fatal(bounded.Err())
		}
	}
	if winners != 1 {
		t.Fatal("admission accepted more than one proof", winners)
	}
	if got, err := peer.FindGitContributionProof(ctx, repo, fixture.admission.ID); err != nil || got != winner {
		t.Fatal("lookup lost frozen original proof", got, winner, err)
	}
	if err := putGitProofPG(ctx, storage, winner); err != nil {
		t.Fatal("identical proof replay rejected", err)
	}
	if _, err := peer.FindGitContributionProof(ctx, domain.HashContent([]byte("other proof repo")), fixture.admission.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("lookup leaked another repository", err)
	}
	independent := later.proof
	independent.AdmissionID = other.ID
	independent = independent.WithID()
	if err := putGitProofPG(ctx, storage, independent); err != nil {
		t.Fatal("independent publication admission could not share contribution", err)
	}
	if got, err := peer.FindGitContributionProof(ctx, repo, other.ID); err != nil || got != independent || got.Contribution != winner.Contribution {
		t.Fatal("independent proof changed semantic contribution", got, err)
	}
	if proofs, err := storage.ListGitContributionProofs(ctx, repo); err != nil || len(proofs) != 2 {
		t.Fatal("proof uniqueness lost independent admission", proofs, err)
	}
}
