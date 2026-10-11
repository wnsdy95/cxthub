//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type gitSupplementPGFixture struct {
	gitContextHeadPGFixture
	publication domain.HistoryEvent
	discovery   domain.GitAssociationDiscovery
}

func newGitSupplementPGFixture(t *testing.T, storage *PostgresStore) gitSupplementPGFixture {
	t.Helper()
	fixture := newGitContextHeadPGFixture(t, storage, false)
	ctx, repo := fixture.ctx, fixture.receipt.RepoID
	if err := fixture.finish(ctx, storage); err != nil {
		t.Fatal(err)
	}
	capture := initializationPGSnapshot(t, storage, repo, "supplement capture")
	fixture.admission.Capture, fixture.admission.AfterReadVersion = capture.ID, fixture.receipt.Version
	fixture.admission = fixture.admission.WithID()
	putGitContributionWorkAdmissionPG(t, storage, ctx, fixture.admission)
	fixture.proof.AdmissionID = fixture.admission.ID
	fixture.proof.Contribution.Capture = capture.ID
	fixture.proof.Contribution = fixture.proof.Contribution.WithID()
	fixture = advanceGitContributionHeadTestPG(t, storage, fixture, "main", fixture.admission.Destination.DestinationBranchID)
	if err := storage.PutGitContribution(ctx, fixture.proof.Contribution); err != nil {
		t.Fatal(err)
	}
	publication := domain.HistoryEvent{ID: strings.Repeat("9", 32), RepoID: string(repo), BranchID: fixture.admission.SourceBranchID,
		Branch: fixture.admission.SourceBranch, Kind: "publish", Source: capture.ID, Target: capture.ID,
		GitAfter: fixture.admission.OriginalHead, WorktreeID: strings.Repeat("7", 32), MemoryPinned: true, CreatedAt: fixture.now}
	if err := storage.ApplyHistoryEvent(ctx, publication); err != nil {
		t.Fatal(err)
	}
	return gitSupplementPGFixture{fixture, publication, domain.GitAssociationDiscovery{RepoID: repo, GitOrigin: fixture.receipt.GitOrigin}}
}

func (fixture *gitSupplementPGFixture) discover(ctx context.Context, storage *PostgresStore, publications ...domain.HistoryEvent) error {
	next := gitDiscoveryPGPage(fixture.discovery)
	if err := saveGitDiscoveryPG(ctx, storage, next, fixture.discovery.Version, publications); err != nil {
		return err
	}
	fixture.discovery = next
	return nil
}

func readGitSupplementTestPG(t *testing.T, ctx context.Context, storage *PostgresStore, fixture gitSupplementPGFixture, publicationID string) domain.GitContributionSupplement {
	t.Helper()
	job, err := scanGitSupplementPG(storage.db(ctx).QueryRow(ctx, `SELECT `+gitSupplementColumnsPG+` FROM git_contribution_supplements WHERE repo_id=$1 AND contribution_id=$2 AND publication_id=$3`, fixture.receipt.RepoID, fixture.proof.Contribution.ID, publicationID), fixture.receipt.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func assertGitSupplementCountPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash, want int) {
	t.Helper()
	var count int
	if err := storage.db(ctx).QueryRow(ctx, `SELECT count(*) FROM git_contribution_supplements WHERE repo_id=$1`, repo).Scan(&count); err != nil || count != want {
		t.Fatal("unexpected supplement count", count, want, err)
	}
}

func finishGitSupplementTestPG(ctx context.Context, storage *PostgresStore, job domain.GitContributionSupplement, state string, next time.Time) error {
	job.State, job.LeaseUntil, job.NextAttempt = state, time.Time{}, next
	return storage.WithinRepository(ctx, job.RepoID, func(bound context.Context) error {
		return storage.FinishGitContributionSupplement(bound, job)
	})
}

func supplementPinPG(fixture gitSupplementPGFixture, publication domain.HistoryEvent) domain.GitContributionMemory {
	source := publication.MemorySource
	if source == "" && publication.MemoryHash != "" {
		source = fixture.proof.Contribution.Capture
	}
	return (domain.GitContributionMemory{RepoID: fixture.receipt.RepoID, ContributionID: fixture.proof.Contribution.ID,
		PublicationID: publication.ID, MemoryHash: publication.MemoryHash, MemorySource: source, MemoryPinned: true}).WithID()
}

func TestPGGitContributionSupplementHandoffReplayAndFilters(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	for _, proofFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(proofFirst), func(t *testing.T) {
			fixture := newGitSupplementPGFixture(t, storage)
			ctx, repo := fixture.ctx, fixture.receipt.RepoID
			before, err := storage.RepositoryRevision(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			if proofFirst {
				err = putGitProofPG(ctx, storage, fixture.proof)
			} else {
				err = fixture.discover(ctx, storage, fixture.publication)
			}
			if err != nil {
				t.Fatal(err)
			}
			assertGitSupplementCountPG(t, ctx, peer, repo, 0)
			if proofFirst {
				err = fixture.discover(ctx, storage, fixture.publication)
			} else {
				err = putGitProofPG(ctx, storage, fixture.proof)
			}
			if err != nil {
				t.Fatal(err)
			}
			initial := readGitSupplementTestPG(t, ctx, peer, fixture, fixture.publication.ID)
			if initial.Version != 0 || initial.Attempts != 0 || initial.State != "waiting" || initial.ProofID != fixture.proof.ID || initial.Validate() != nil {
				t.Fatal("bad initial obligation", initial)
			}
			now := time.Now().UTC().Add(time.Second)
			claim, err := storage.ClaimGitContributionSupplement(ctx, repo, now, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if err := putGitProofPG(ctx, storage, fixture.proof); err != nil {
				t.Fatal(err)
			}
			if err := fixture.discover(ctx, storage, fixture.publication); err != nil {
				t.Fatal(err)
			}
			if got := readGitSupplementTestPG(t, ctx, peer, fixture, fixture.publication.ID); !reflect.DeepEqual(got, claim) {
				t.Fatal("replayed handoff reset claimed job", got, claim)
			}
			if after, err := storage.RepositoryRevision(ctx, repo); err != nil || after != before {
				t.Fatal("handoff/claim advanced public revisions", before, after, err)
			}
			otherCapture := initializationPGSnapshot(t, storage, repo, "foreign capture")
			for index, mutate := range []func(*domain.HistoryEvent){
				func(publication *domain.HistoryEvent) { publication.MemoryPinned = false },
				func(publication *domain.HistoryEvent) { publication.BranchID = "other-source" },
				func(publication *domain.HistoryEvent) {
					publication.Source, publication.Target = otherCapture.ID, otherCapture.ID
				},
				func(publication *domain.HistoryEvent) { publication.GitAfter = strings.Repeat("c", 40) },
			} {
				publication := fixture.publication
				publication.ID = fmt.Sprintf("%032x", index+1)
				mutate(&publication)
				if err := storage.ApplyHistoryEvent(ctx, publication); err != nil {
					t.Fatal(err)
				}
				if err := fixture.discover(ctx, storage, publication); err != nil {
					t.Fatal(err)
				}
			}
			pinned := fixture.publication
			pinned.ID = strings.Repeat("8", 32)
			if err := storage.ApplyHistoryEvent(ctx, pinned); err != nil {
				t.Fatal(err)
			}
			if err := storage.PutGitContributionMemory(ctx, supplementPinPG(fixture, pinned)); err != nil {
				t.Fatal(err)
			}
			if err := fixture.discover(ctx, storage, pinned); err != nil {
				t.Fatal(err)
			}
			assertGitSupplementCountPG(t, ctx, peer, repo, 1)
			other := fixture.admission
			other.PublicationID, other.AfterReadVersion = strings.Repeat("6", 32), fixture.receipt.Version
			other = other.WithID()
			putGitContributionWorkAdmissionPG(t, storage, ctx, other)
			later := advanceGitContributionHeadTestPG(t, storage, fixture.gitContextHeadPGFixture, "main", fixture.admission.Destination.DestinationBranchID)
			secondProof := later.proof
			secondProof.AdmissionID = other.ID
			secondProof = secondProof.WithID()
			if err := putGitProofPG(ctx, storage, secondProof); err != nil {
				t.Fatal(err)
			}
			late := fixture.publication
			late.ID, late.MemoryHash = strings.Repeat("5", 32), domain.HashContent([]byte("late memory"))
			if err := storage.ApplyHistoryEvent(ctx, late); err != nil {
				t.Fatal(err)
			}
			if err := fixture.discover(ctx, storage, late); err != nil {
				t.Fatal(err)
			}
			if got := readGitSupplementTestPG(t, ctx, peer, fixture, late.ID); got.ProofID != fixture.proof.ID {
				t.Fatal("later proof replaced first accepted anchor", got)
			}
			assertGitSupplementCountPG(t, ctx, peer, repo, 2)
		})
	}
}

func TestPGGitContributionSupplementAtomicHandoff(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	for _, proofFirst := range []bool{false, true} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprint(proofFirst, canceled), func(t *testing.T) {
				fixture := newGitSupplementPGFixture(t, storage)
				ctx, repo := fixture.ctx, fixture.receipt.RepoID
				if proofFirst {
					if err := putGitProofPG(ctx, storage, fixture.proof); err != nil {
						t.Fatal(err)
					}
				} else if err := fixture.discover(ctx, storage, fixture.publication); err != nil {
					t.Fatal(err)
				}
				work, cancel := context.WithCancel(ctx)
				defer cancel()
				abort := errors.New("abort supplement handoff")
				err := storage.WithinRepository(work, repo, func(bound context.Context) error {
					if proofFirst {
						if err := storage.SaveGitAssociationDiscovery(bound, gitDiscoveryPGPage(fixture.discovery), fixture.discovery.Version, []domain.HistoryEvent{fixture.publication}); err != nil {
							return err
						}
					} else if err := storage.PutGitContributionProof(bound, fixture.proof); err != nil {
						return err
					}
					assertGitSupplementCountPG(t, bound, storage, repo, 1)
					assertGitSupplementCountPG(t, ctx, peer, repo, 0)
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
					t.Fatal(err)
				}
				assertGitSupplementCountPG(t, ctx, peer, repo, 0)
				if proofFirst {
					if _, err := peer.GetGitAssociationDiscovery(ctx, repo, fixture.receipt.GitOrigin); !errors.Is(err, domain.ErrNotFound) {
						t.Fatal("aborted checkpoint persisted", err)
					}
				} else if _, err := peer.GetGitContributionSupplementProof(ctx, repo, fixture.proof.ID); !errors.Is(err, domain.ErrNotFound) {
					t.Fatal("aborted proof persisted", err)
				}
			})
		}
	}
	t.Run("invalid-second-publication", func(t *testing.T) {
		fixture := newGitSupplementPGFixture(t, storage)
		ctx, repo := fixture.ctx, fixture.receipt.RepoID
		if err := putGitProofPG(ctx, storage, fixture.proof); err != nil {
			t.Fatal(err)
		}
		missing := fixture.publication
		missing.ID = strings.Repeat("1", 32)
		if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
			if err := storage.SaveGitAssociationDiscovery(bound, gitDiscoveryPGPage(fixture.discovery), 0, []domain.HistoryEvent{fixture.publication, missing}); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("invented original accepted", err)
			}
			assertGitSupplementCountPG(t, bound, storage, repo, 0)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		assertGitSupplementCountPG(t, ctx, peer, repo, 0)
		if _, err := peer.GetGitAssociationDiscovery(ctx, repo, fixture.receipt.GitOrigin); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("savepoint did not roll back checkpoint", err)
		}
	})
}

func TestPGGitContributionSupplementLeaseCompletionAndFences(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	fixture := newGitSupplementPGFixture(t, storage)
	ctx, repo := fixture.ctx, fixture.receipt.RepoID
	if err := putGitProofPG(ctx, storage, fixture.proof); err != nil {
		t.Fatal(err)
	}
	if err := fixture.discover(ctx, storage, fixture.publication); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	type result struct {
		job domain.GitContributionSupplement
		err error
	}
	ready, start, results := make(chan struct{}, 2), make(chan struct{}), make(chan result, 2)
	for _, writer := range []*PostgresStore{storage, peer} {
		go func() {
			ready <- struct{}{}
			<-start
			job, err := writer.ClaimGitContributionSupplement(bounded, repo, now, time.Minute)
			results <- result{job, err}
		}()
	}
	<-ready
	<-ready
	close(start)
	var claim domain.GitContributionSupplement
	winners := 0
	for range 2 {
		got := <-results
		if got.err == nil {
			claim = got.job
			winners++
		} else if !errors.Is(got.err, domain.ErrNotFound) {
			t.Fatal(got.err)
		}
	}
	if winners != 1 || claim.Version != 1 || claim.Attempts != 1 {
		t.Fatal("claim was not exclusive", winners, claim)
	}
	if err := finishGitSupplementTestPG(ctx, storage, claim, "completed", now); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("completed without corresponding pin", err)
	}
	for _, mutate := range []func(*domain.GitContributionSupplement){
		func(job *domain.GitContributionSupplement) { job.ProofID = strings.Repeat("a", 64) },
		func(job *domain.GitContributionSupplement) { job.GitOrigin += "/different" },
		func(job *domain.GitContributionSupplement) { job.Attempts++ },
		func(job *domain.GitContributionSupplement) { job.Version++ },
	} {
		changed := claim
		mutate(&changed)
		if err := finishGitSupplementTestPG(ctx, storage, changed, "waiting", now); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("immutable claim identity changed", err)
		}
	}
	reclaimed, err := peer.ClaimGitContributionSupplement(ctx, repo, now.Add(time.Minute), time.Minute)
	if err != nil || reclaimed.Version != 2 || reclaimed.Attempts != 2 || reclaimed.ID != claim.ID {
		t.Fatal("expired lease not reclaimed", reclaimed, err)
	}
	if err := finishGitSupplementTestPG(ctx, storage, claim, "waiting", now); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("stale worker finished replacement claim", err)
	}
	claim = reclaimed
	pin := supplementPinPG(fixture, fixture.publication)
	abort := errors.New("abort memory and completion")
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		if err := storage.PutGitContributionMemory(bound, pin); err != nil {
			return err
		}
		if err := finishGitSupplementTestPG(bound, storage, claim, "completed", now); err != nil {
			return err
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatal(err)
	}
	if _, err := peer.FindGitContributionSupplementMemory(ctx, repo, pin.ContributionID, pin.PublicationID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("aborted pin persisted", err)
	}
	if got := readGitSupplementTestPG(t, ctx, peer, fixture, fixture.publication.ID); !reflect.DeepEqual(got, claim) {
		t.Fatal("aborted completion persisted", got, claim)
	}
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		if err := storage.PutGitContributionMemory(bound, pin); err != nil {
			return err
		}
		return finishGitSupplementTestPG(bound, storage, claim, "completed", now)
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := peer.FindGitContributionSupplementMemory(ctx, repo, pin.ContributionID, pin.PublicationID); err != nil || got != pin || got.MemoryHash != "" || !got.MemoryPinned {
		t.Fatal("explicit empty pin changed", got, err)
	}
	if err := finishGitSupplementTestPG(ctx, storage, claim, "completed", now); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("completed claim replay bypassed CAS", err)
	}
	if _, err := storage.ClaimGitContributionSupplement(ctx, repo, now.Add(time.Hour), time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("completed obligation reclaimed", err)
	}
}

func TestPGGitContributionSupplementTransactionPolicyAndOverflow(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	fixture := newGitSupplementPGFixture(t, storage)
	ctx, repo := fixture.ctx, fixture.receipt.RepoID
	if err := putGitProofPG(ctx, storage, fixture.proof); err != nil {
		t.Fatal(err)
	}
	if err := fixture.discover(ctx, storage, fixture.publication); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Second)
	claim, err := storage.ClaimGitContributionSupplement(ctx, repo, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	next := claim
	next.State, next.LeaseUntil = "waiting", time.Time{}
	if err := storage.FinishGitContributionSupplement(ctx, next); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("outside-transaction finish", err)
	}
	check := func(bound context.Context, writer *PostgresStore, want error) {
		t.Helper()
		if _, err := writer.ClaimGitContributionSupplement(bound, repo, now, time.Minute); !errors.Is(err, want) {
			t.Fatal("claim bypassed transaction/policy", err, want)
		}
		if err := writer.FinishGitContributionSupplement(bound, next); !errors.Is(err, want) {
			t.Fatal("finish bypassed transaction/policy", err, want)
		}
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error { check(bound, storage, domain.ErrConflict); return nil }); err != nil {
		t.Fatal(err)
	}
	other := domain.HashContent([]byte("supplement foreign repo"))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: other}); err != nil {
		t.Fatal(err)
	}
	if err := storage.WithinRepository(ctx, other, func(bound context.Context) error { check(bound, storage, domain.ErrConflict); return nil }); err != nil {
		t.Fatal(err)
	}
	var escaped context.Context
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		escaped = bound
		check(bound, peer, domain.ErrConflict)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	check(escaped, storage, domain.ErrConflict)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	check(canceled, storage, context.Canceled)
	if err := storage.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		check(bound, storage, domain.ErrDocumentIdentityUpgradeRequired)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	supported := []domain.DocumentIdentity{domain.DocumentIdentityRootV1}
	compatible := outbound.WithDocumentIdentityCompatibility(ctx, supported, supported)
	if err := finishGitSupplementTestPG(compatible, storage, claim, "waiting", now); err != nil {
		t.Fatal(err)
	}
	initial := readGitSupplementTestPG(t, ctx, storage, fixture, fixture.publication.ID)
	abort := errors.New("rollback claimed supplement")
	if err := storage.WithinRepository(compatible, repo, func(bound context.Context) error {
		if _, err := storage.ClaimGitContributionSupplement(bound, repo, now, time.Minute); err != nil {
			return err
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatal(err)
	}
	if got := readGitSupplementTestPG(t, ctx, storage, fixture, fixture.publication.ID); !reflect.DeepEqual(initial, got) {
		t.Fatal("aborted claim persisted", got)
	}
	for _, counters := range [][2]int64{{math.MaxInt64, 1}, {1, int64(math.MaxInt)}} {
		changed := initial
		changed.Version, changed.Attempts = counters[0], int(counters[1])
		raw, err := json.Marshal(changed)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.pool.Exec(ctx, `UPDATE git_contribution_supplements SET version=$3,payload=$4 WHERE repo_id=$1 AND id=$2`, repo, changed.ID, changed.Version, raw); err != nil {
			t.Fatal(err)
		}
		if _, err := storage.ClaimGitContributionSupplement(compatible, repo, now, time.Minute); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("counter overflow accepted", err)
		}
		if got := readGitSupplementTestPG(t, ctx, storage, fixture, fixture.publication.ID); !reflect.DeepEqual(got, changed) {
			t.Fatal("counter changed on rejected increment", got)
		}
	}
}

func TestPGGitContributionSupplementLinkedReadsAndConflictingPins(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	fixture := newGitSupplementPGFixture(t, storage)
	ctx, repo := fixture.ctx, fixture.receipt.RepoID
	if err := putGitProofPG(ctx, storage, fixture.proof); err != nil {
		t.Fatal(err)
	}
	if got, err := storage.GetGitContributionSupplementProof(ctx, repo, fixture.proof.ID); err != nil || got != fixture.proof {
		t.Fatal("targeted proof differs", got, err)
	}
	foreign := domain.HashContent([]byte("foreign proof scope"))
	if _, err := storage.GetGitContributionSupplementProof(ctx, foreign, fixture.proof.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("foreign proof returned", err)
	}
	publication := fixture.publication
	publication.ID, publication.MemoryHash = strings.Repeat("1", 32), domain.HashContent([]byte("pinned bytes"))
	if err := storage.ApplyHistoryEvent(ctx, publication); err != nil {
		t.Fatal(err)
	}
	if err := fixture.discover(ctx, storage, publication); err != nil {
		t.Fatal(err)
	}
	claim, err := storage.ClaimGitContributionSupplement(ctx, repo, time.Now().UTC().Add(time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	pin := supplementPinPG(fixture, publication)
	if err := storage.PutGitContributionMemory(ctx, pin); err != nil {
		t.Fatal(err)
	}
	if got, err := storage.FindGitContributionSupplementMemory(ctx, repo, pin.ContributionID, pin.PublicationID); err != nil || got != pin || got.MemorySource != fixture.admission.Capture {
		t.Fatal("targeted original pin changed", got, err)
	}
	if _, err := storage.FindGitContributionSupplementMemory(ctx, foreign, pin.ContributionID, pin.PublicationID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("foreign memory returned", err)
	}
	conflict := pin
	conflict.MemoryHash = domain.HashContent([]byte("conflicting bytes"))
	conflict = conflict.WithID()
	if err := storage.PutGitContributionMemory(ctx, conflict); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.FindGitContributionSupplementMemory(ctx, repo, pin.ContributionID, pin.PublicationID); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("multiple logical pins silently selected", err)
	}
	if err := finishGitSupplementTestPG(ctx, storage, claim, "completed", time.Now()); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("conflicting pins completed", err)
	}
	if _, err := storage.pool.Exec(ctx, `DELETE FROM git_contribution_memories WHERE repo_id=$1 AND id=$2`, repo, conflict.ID); err != nil {
		t.Fatal(err)
	}
	if err := finishGitSupplementTestPG(ctx, storage, claim, "completed", time.Now()); err != nil {
		t.Fatal("nonempty original pin rejected", err)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE git_contribution_memories SET memory_source=$3 WHERE repo_id=$1 AND id=$2`, repo, pin.ID, domain.HashContent([]byte("tampered source"))); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.FindGitContributionSupplementMemory(ctx, repo, pin.ContributionID, pin.PublicationID); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("pin columns not validated", err)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE git_contributions SET payload=jsonb_set(payload,'{original_head}',to_jsonb($3::text)) WHERE repo_id=$1 AND id=$2`, repo, pin.ContributionID, strings.Repeat("d", 40)); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.GetGitContributionSupplementProof(ctx, repo, fixture.proof.ID); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("linked contribution not validated", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := storage.GetGitContributionSupplementProof(canceled, repo, fixture.proof.ID); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := storage.FindGitContributionSupplementMemory(canceled, repo, pin.ContributionID, pin.PublicationID); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := storage.GetGitContributionSupplementProof(ctx, repo, "bad"); !errors.Is(err, domain.ErrValidation) {
		t.Fatal(err)
	}
	if _, err := storage.FindGitContributionSupplementMemory(ctx, repo, pin.ContributionID, "bad"); !errors.Is(err, domain.ErrValidation) {
		t.Fatal(err)
	}
}

func TestPGGitContributionSupplementAttentionDoesNotStarveLaterJobs(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	fixture := newGitSupplementPGFixture(t, storage)
	ctx, repo := fixture.ctx, fixture.receipt.RepoID
	if err := putGitProofPG(ctx, storage, fixture.proof); err != nil {
		t.Fatal(err)
	}
	later := fixture.publication
	later.ID = strings.Repeat("1", 32)
	if err := storage.ApplyHistoryEvent(ctx, later); err != nil {
		t.Fatal(err)
	}
	if err := fixture.discover(ctx, storage, fixture.publication, later); err != nil {
		t.Fatal(err)
	}
	initial := readGitSupplementTestPG(t, ctx, storage, fixture, fixture.publication.ID)
	second := readGitSupplementTestPG(t, ctx, storage, fixture, later.ID)
	if second.ID < initial.ID {
		initial, second = second, initial
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE git_association_publications SET payload=jsonb_set(payload,'{branch_id}',to_jsonb('invalid-source'::text)) WHERE repo_id=$1 AND publication_id=$2`, repo, initial.PublicationID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Second)
	claim, err := storage.ClaimGitContributionSupplement(ctx, repo, now, time.Minute)
	if err != nil || claim.ID != initial.ID {
		t.Fatal("linked-input integrity prevented leased attention settlement", claim, err)
	}
	if _, _, err := readGitSupplementInputsPG(ctx, storage.pool, claim); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("changed original input accepted", err)
	}
	if err := finishGitSupplementTestPG(ctx, storage, claim, "completed", now); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("corrupt linked input completed", err)
	}
	claim.Reason = "integrity_check_failed"
	if err := finishGitSupplementTestPG(ctx, storage, claim, "attention", now); err != nil {
		t.Fatal(err)
	}
	attention := readGitSupplementTestPG(t, ctx, peer, fixture, initial.PublicationID)
	if attention.State != "attention" || attention.Version != claim.Version || attention.Attempts != claim.Attempts || attention.ProofID != claim.ProofID {
		t.Fatal("attention did not retain claim identity", attention)
	}
	next, err := peer.ClaimGitContributionSupplement(ctx, repo, now, time.Minute)
	if err != nil || next.ID != second.ID {
		t.Fatal("attention blocked later valid job", next, err)
	}
	if err := finishGitSupplementTestPG(ctx, storage, next, "retrying", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ClaimGitContributionSupplement(ctx, repo, now, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("attention or future retry was immediately reclaimed", err)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE git_contribution_supplements SET payload=jsonb_set(payload,'{proof_id}',to_jsonb('bad'::text)) WHERE repo_id=$1 AND id=$2`, repo, next.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ClaimGitContributionSupplement(ctx, repo, now.Add(time.Hour), time.Minute); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("corrupt job payload silently claimed", err)
	}
}

func TestPGGitContributionSupplementHandoffSavepointFailure(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	for _, proofFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(proofFirst), func(t *testing.T) {
			fixture := newGitSupplementPGFixture(t, storage)
			ctx, repo := fixture.ctx, fixture.receipt.RepoID
			if proofFirst {
				if err := putGitProofPG(ctx, storage, fixture.proof); err != nil {
					t.Fatal(err)
				}
			} else if err := fixture.discover(ctx, storage, fixture.publication); err != nil {
				t.Fatal(err)
			}
			if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
				if _, err := storage.db(bound).Exec(bound, `ALTER TABLE git_contribution_supplements ADD CONSTRAINT inject_handoff_failure CHECK (false) NOT VALID`); err != nil {
					return err
				}
				var err error
				if proofFirst {
					err = storage.SaveGitAssociationDiscovery(bound, gitDiscoveryPGPage(fixture.discovery), 0, []domain.HistoryEvent{fixture.publication})
				} else {
					err = storage.PutGitContributionProof(bound, fixture.proof)
				}
				if err == nil {
					t.Fatal("injected enqueue failure did not fail handoff")
				}
				assertGitSupplementCountPG(t, bound, storage, repo, 0)
				if proofFirst {
					if _, err := storage.GetGitAssociationDiscovery(bound, repo, fixture.receipt.GitOrigin); !errors.Is(err, domain.ErrNotFound) {
						t.Fatal("checkpoint escaped failed enqueue", err)
					}
				} else {
					if _, err := storage.GetGitContributionSupplementProof(bound, repo, fixture.proof.ID); !errors.Is(err, domain.ErrNotFound) {
						t.Fatal("proof escaped failed enqueue", err)
					}
					var anchors int
					if err := storage.db(bound).QueryRow(bound, `SELECT count(*) FROM git_contribution_supplement_anchors WHERE repo_id=$1`, repo).Scan(&anchors); err != nil || anchors != 0 {
						t.Fatal("anchor escaped failed enqueue", anchors, err)
					}
				}
				_, err = storage.db(bound).Exec(bound, `ALTER TABLE git_contribution_supplements DROP CONSTRAINT inject_handoff_failure`)
				return err
			}); err != nil {
				t.Fatal("failed handoff poisoned outer transaction", err)
			}
			assertGitSupplementCountPG(t, ctx, peer, repo, 0)
		})
	}
}

func TestPGGitContributionSupplementIndexedFanout(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	fixture := newGitSupplementPGFixture(t, storage)
	ctx, repo := fixture.ctx, fixture.receipt.RepoID
	var batch []domain.HistoryEvent
	for number := 1; number <= 205; number++ {
		publication := fixture.publication
		publication.ID = fmt.Sprintf("%032x", number)
		if err := storage.ApplyHistoryEvent(ctx, publication); err != nil {
			t.Fatal(err)
		}
		batch = append(batch, publication)
		if len(batch) == 100 || number == 205 {
			if err := fixture.discover(ctx, storage, batch...); err != nil {
				t.Fatal(err)
			}
			batch = nil
		}
	}
	assertGitSupplementCountPG(t, ctx, peer, repo, 0)
	if err := putGitProofPG(ctx, storage, fixture.proof); err != nil {
		t.Fatal(err)
	}
	assertGitSupplementCountPG(t, ctx, peer, repo, 205)
	if err := putGitProofPG(ctx, storage, fixture.proof); err != nil {
		t.Fatal(err)
	}
	assertGitSupplementCountPG(t, ctx, peer, repo, 205)
	abort := errors.New("rollback origin mismatch")
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		if _, err := storage.db(bound).Exec(bound, `UPDATE repos SET git_remote_url='https://github.com/acme/other.git' WHERE id=$1`, repo); err != nil {
			return err
		}
		var count int
		if err := storage.db(bound).QueryRow(bound, `SELECT count(*) FROM git_contribution_supplement_candidates WHERE repo_id=$1`, repo).Scan(&count); err != nil || count != 0 {
			t.Fatal("foreign current origin produced candidates", count, err)
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatal(err)
	}
}

func TestPGGitContributionSupplementMigrationBackfillIDParity(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	fixture := newGitSupplementPGFixture(t, storage)
	ctx, repo := fixture.ctx, fixture.receipt.RepoID
	if err := putGitProofPG(ctx, storage, fixture.proof); err != nil {
		t.Fatal(err)
	}
	if err := fixture.discover(ctx, storage, fixture.publication); err != nil {
		t.Fatal(err)
	}
	alreadyPinned := fixture.publication
	alreadyPinned.ID = strings.Repeat("1", 32)
	if err := storage.ApplyHistoryEvent(ctx, alreadyPinned); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutGitContributionMemory(ctx, supplementPinPG(fixture, alreadyPinned)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.discover(ctx, storage, alreadyPinned); err != nil {
		t.Fatal(err)
	}
	other := fixture.admission
	other.PublicationID, other.AfterReadVersion = strings.Repeat("6", 32), fixture.receipt.Version
	other = other.WithID()
	putGitContributionWorkAdmissionPG(t, storage, ctx, other)
	later := advanceGitContributionHeadTestPG(t, storage, fixture.gitContextHeadPGFixture, "main", fixture.admission.Destination.DestinationBranchID)
	secondProof := later.proof
	secondProof.AdmissionID = other.ID
	secondProof = secondProof.WithID()
	if err := putGitProofPG(ctx, storage, secondProof); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../../../schemas/db/migrations/0089_git_contribution_supplements.sql")
	if err != nil {
		t.Fatal(err)
	}
	before, err := storage.RepositoryRevision(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	abort := errors.New("rollback migration fixture")
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		if _, err := storage.db(bound).Exec(bound, `DROP VIEW git_contribution_supplement_candidates; DROP TABLE git_contribution_supplements; DROP TABLE git_contribution_supplement_anchors; DROP FUNCTION git_contribution_supplement_id(TEXT,TEXT,TEXT); DROP INDEX git_contributions_supplement_match; DROP INDEX git_association_publications_supplement_match; DROP INDEX git_contribution_memories_publication`); err != nil {
			return err
		}
		if _, err := storage.db(bound).Exec(bound, string(migration)); err != nil {
			return err
		}
		assertGitSupplementCountPG(t, bound, storage, repo, 1)
		job := readGitSupplementTestPG(t, bound, storage, fixture, fixture.publication.ID)
		if job.ID != job.WithID().ID || job.Version != 0 || job.Attempts != 0 || job.State != "waiting" || job.ProofID != min(fixture.proof.ID, secondProof.ID) {
			t.Fatal("backfill differs from Go ID or frozen proof selection", job)
		}
		for _, publicationID := range []string{strings.Repeat("0", 32), strings.Repeat("f", 32), fixture.publication.ID} {
			var storedID string
			if err := storage.db(bound).QueryRow(bound, `SELECT git_contribution_supplement_id($1,$2,$3)`, repo, fixture.proof.Contribution.ID, publicationID).Scan(&storedID); err != nil {
				return err
			}
			want := (domain.GitContributionSupplement{RepoID: repo, ContributionID: fixture.proof.Contribution.ID, PublicationID: publicationID}).WithID().ID
			if storedID != want {
				t.Fatal("SQL/Go versioned identity mismatch", storedID, want)
			}
		}
		claim, err := storage.ClaimGitContributionSupplement(bound, repo, time.Now().UTC().Add(time.Second), time.Minute)
		if err != nil || claim.ID != job.ID || claim.Version != 1 || claim.Attempts != 1 {
			t.Fatal("backfilled obligation not claimable", claim, err)
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatal(err)
	}
	if after, err := storage.RepositoryRevision(ctx, repo); err != nil || after != before {
		t.Fatal("backfill advanced revisions", before, after, err)
	}
}
