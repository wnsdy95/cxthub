package app

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// Embedding leaves the fake limited to the worker's preparation lifecycle. An
// accidental call to the legacy CompleteDocJob path fails instead of succeeding.
type preparationJobStore struct {
	outbound.DocJobStore
	prepare func(context.Context, domain.VerifiedSessionDoc) (outbound.PreparedDocPublication, error)
	renew   func(context.Context, domain.DocFinalizationJob, time.Time, time.Duration) error
	finish  func(context.Context, domain.DocFinalizationJob, time.Time) error
}

func (s *preparationJobStore) PrepareDocJob(ctx context.Context, doc domain.VerifiedSessionDoc) (outbound.PreparedDocPublication, error) {
	return s.prepare(ctx, doc)
}

func (s *preparationJobStore) RenewDocJob(ctx context.Context, j domain.DocFinalizationJob, now time.Time, lease time.Duration) error {
	return s.renew(ctx, j, now, lease)
}

func (s *preparationJobStore) FinishDocJob(ctx context.Context, j domain.DocFinalizationJob, now time.Time) error {
	return s.finish(ctx, j, now)
}

type preparationPublication struct {
	complete func(context.Context, domain.DocFinalizationJob, time.Time) error
}

func (p preparationPublication) Complete(ctx context.Context, j domain.DocFinalizationJob, now time.Time) error {
	return p.complete(ctx, j, now)
}

type preparationChunks struct {
	outbound.BlobStore
	repo   domain.ContentHash
	bodies map[domain.ContentHash][]byte
}

func (s preparationChunks) GetChunk(_ context.Context, repo, hash domain.ContentHash) ([]byte, error) {
	body, ok := s.bodies[hash]
	if repo != s.repo || !ok {
		return nil, domain.ErrNotFound
	}
	return append([]byte(nil), body...), nil
}

func preparationFixture(t *testing.T) (*Service, domain.DocFinalizationJob) {
	t.Helper()
	cir := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1"},
		Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser,
			Blocks: []domain.ContentBlock{{Type: "text", Text: "preparation lifecycle"}}}},
	}
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := domain.PlanDocChunks(raw)
	if !ok {
		t.Fatal("fixture did not produce chunks")
	}
	repo := domain.HashContent([]byte(t.Name()))
	j, err := domain.NewDocFinalizationJob(repo, domain.HashContent(raw), plan.Manifest, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return NewService(nil, preparationChunks{repo: repo, bodies: plan.Bodies}, nil, nil, nil), j.Claim(time.Now().UTC(), docJobLease)
}

func TestDocJobPreparationRenewsAndJoinsBeforeCompletion(t *testing.T) {
	// Virtual time exercises the production lease/ticker, with no wall-clock wait
	// or package-global timing override that could interfere with another test.
	synctest.Test(t, func(t *testing.T) {
		svc, job := preparationFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		preparing, releasePrepare := make(chan struct{}), make(chan struct{})
		renewing, releaseRenew := make(chan struct{}), make(chan struct{})
		completing, releaseComplete := make(chan struct{}), make(chan struct{})
		var renewCalls, completeCalls atomic.Int32
		publication := preparationPublication{complete: func(ctx context.Context, got domain.DocFinalizationJob, _ time.Time) error {
			completeCalls.Add(1)
			if got.ID != job.ID || got.Version != job.Version {
				t.Error("completion changed the claimed job")
			}
			close(completing)
			select {
			case <-releaseComplete:
				return ctx.Err()
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
		st := &preparationJobStore{
			prepare: func(ctx context.Context, doc domain.VerifiedSessionDoc) (outbound.PreparedDocPublication, error) {
				if !doc.Valid() || doc.Hash() != job.DocHash {
					t.Error("preparation did not receive the verified job document")
				}
				close(preparing)
				select {
				case <-releasePrepare:
					return publication, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			},
			renew: func(ctx context.Context, got domain.DocFinalizationJob, now time.Time, lease time.Duration) error {
				if got.ID != job.ID || got.Version != job.Version || lease != docJobLease || !now.Equal(time.Now().UTC()) {
					t.Error("renewal changed the claim, lease, or current time")
				}
				if renewCalls.Add(1) == 4 {
					close(renewing)
					select {
					case <-releaseRenew:
					case <-ctx.Done():
					}
				}
				return ctx.Err()
			},
			finish: func(context.Context, domain.DocFinalizationJob, time.Time) error {
				t.Error("successful preparation/completion was classified as a failure")
				return nil
			},
		}
		done := make(chan error, 1)
		go func() { done <- svc.runDocJob(ctx, st, job) }()
		<-preparing
		synctest.Wait() // Install the ticker before advancing virtual time.
		interval := docJobLease / 3
		time.Sleep(3*interval + time.Second)
		synctest.Wait()
		if got := renewCalls.Load(); got != 3 {
			t.Fatalf("renewals while preparation exceeds the original lease = %d, want 3", got)
		}
		if got := completeCalls.Load(); got != 0 {
			t.Fatalf("completion started during preparation: %d calls", got)
		}
		time.Sleep(interval - time.Second)
		<-renewing
		close(releasePrepare)
		synctest.Wait()
		if got := completeCalls.Load(); got != 0 {
			t.Fatalf("completion did not join in-flight renewal: %d calls", got)
		}
		close(releaseRenew)
		<-completing
		synctest.Wait()
		time.Sleep(2 * interval)
		synctest.Wait()
		if got := renewCalls.Load(); got != 4 {
			t.Errorf("renewal continued during completion: %d calls, want 4", got)
		}
		close(releaseComplete)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if got := completeCalls.Load(); got != 1 {
			t.Errorf("prepared completion calls = %d, want 1", got)
		}
	})
}

func TestDocJobPreparationCancellationPreventsCompletion(t *testing.T) {
	for _, lostLease := range []bool{false, true} {
		for _, readyOnCancel := range []bool{false, true} {
			name := fmt.Sprintf("lease_loss=%t/ready_plan=%t", lostLease, readyOnCancel)
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					svc, job := preparationFixture(t)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					preparing, releasePrepare := make(chan struct{}), make(chan struct{})
					renewing, releaseRenew := make(chan struct{}), make(chan struct{})
					var renewCalls, completeCalls, finishCalls atomic.Int32
					publication := preparationPublication{complete: func(ctx context.Context, _ domain.DocFinalizationJob, _ time.Time) error {
						completeCalls.Add(1)
						return ctx.Err()
					}}
					st := &preparationJobStore{
						prepare: func(ctx context.Context, _ domain.VerifiedSessionDoc) (outbound.PreparedDocPublication, error) {
							close(preparing)
							if readyOnCancel {
								select {
								case <-releasePrepare:
									return publication, nil
								case <-ctx.Done():
									return nil, ctx.Err()
								}
							}
							<-ctx.Done()
							return nil, ctx.Err()
						},
						renew: func(ctx context.Context, _ domain.DocFinalizationJob, _ time.Time, _ time.Duration) error {
							renewCalls.Add(1)
							if readyOnCancel {
								close(renewing)
								select {
								case <-releaseRenew:
								case <-ctx.Done():
								}
							}
							if lostLease {
								return domain.ErrConflict
							}
							return ctx.Err()
						},
						finish: func(ctx context.Context, got domain.DocFinalizationJob, now time.Time) error {
							finishCalls.Add(1)
							if err := ctx.Err(); err != nil {
								t.Errorf("failure receipt inherited canceled work context: %v", err)
							}
							assertPreparationFailure(t, job, got, now, "retrying", "temporary_failure")
							return domain.ErrConflict // The lost worker cannot overwrite its successor.
						},
					}
					done := make(chan error, 1)
					go func() { done <- svc.runDocJob(ctx, st, job) }()
					<-preparing
					synctest.Wait()
					if readyOnCancel {
						time.Sleep(docJobLease / 3)
						<-renewing
						close(releasePrepare)
						synctest.Wait()
						// Preparation successfully returned before cancellation. The
						// worker is now joining a renewal that has not returned yet.
						if got := completeCalls.Load(); got != 0 {
							t.Errorf("completion overtook in-flight renewal: %d calls", got)
						}
						if !lostLease {
							cancel()
						}
						close(releaseRenew)
					} else if lostLease {
						time.Sleep(docJobLease / 3)
					} else {
						cancel()
					}
					err := <-done
					if !errors.Is(err, context.Canceled) {
						t.Errorf("worker error = %v, want context.Canceled", err)
					}
					if got := completeCalls.Load(); got != 0 {
						t.Errorf("canceled preparation invoked Complete %d times", got)
					}
					var wantRenew, wantFinish int32
					if lostLease || readyOnCancel {
						wantRenew = 1
					}
					if lostLease {
						wantFinish = 1
						if !errors.Is(err, domain.ErrConflict) {
							t.Errorf("worker discarded the failure receipt's fence error: %v", err)
						}
					}
					if renewCalls.Load() != wantRenew || finishCalls.Load() != wantFinish {
						t.Errorf("renew/finish calls = %d/%d, want %d/%d (shutdown must leave the lease reclaimable)", renewCalls.Load(), finishCalls.Load(), wantRenew, wantFinish)
					}
				})
			})
		}
	}
}

func assertPreparationFailure(t *testing.T, claim, got domain.DocFinalizationJob, now time.Time, state, reason string) {
	t.Helper()
	if got.ID != claim.ID || got.RepoID != claim.RepoID || got.DocHash != claim.DocHash || got.Version != claim.Version || got.Attempts != claim.Attempts {
		t.Error("preparation failure changed the claimed job identity or fence")
	}
	if got.State != state || got.Reason != reason || !got.LeaseUntil.IsZero() || !got.UpdatedAt.Equal(now) {
		t.Errorf("failure receipt = state %q reason %q lease %v updated %v; want %q/%q, cleared lease and updated %v", got.State, got.Reason, got.LeaseUntil, got.UpdatedAt, state, reason, now)
	}
	if want := now.Add(time.Second << min(claim.Attempts, 8)); !got.NextAttempt.Equal(want) {
		t.Errorf("next attempt = %v, want %v", got.NextAttempt, want)
	}
}

func TestDocJobPreparationErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		state  string
		reason string
	}{
		{"temporary", errors.New("preparation unavailable"), "retrying", "temporary_failure"},
		{"quota", domain.ErrStorageLimit, "retrying", "temporary_failure"},
		{"conflict", domain.ErrConflict, "retrying", "temporary_failure"},
		{"integrity", domain.ErrIntegrity, "rejected", "invalid_document"},
		{"validation", domain.ErrValidation, "rejected", "invalid_document"},
		{"unsupported_version", domain.ErrUnsupportedCIRVersion, "rejected", "invalid_document"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, job := preparationFixture(t)
				prepareCalls, finishCalls := 0, 0
				st := &preparationJobStore{
					prepare: func(context.Context, domain.VerifiedSessionDoc) (outbound.PreparedDocPublication, error) {
						prepareCalls++
						return nil, fmt.Errorf("prepare document: %w", tc.err)
					},
					finish: func(ctx context.Context, got domain.DocFinalizationJob, now time.Time) error {
						finishCalls++
						if err := ctx.Err(); err != nil {
							t.Errorf("failure receipt context: %v", err)
						}
						assertPreparationFailure(t, job, got, now, tc.state, tc.reason)
						return nil
					},
				}
				if err := svc.runDocJob(context.Background(), st, job); !errors.Is(err, tc.err) {
					t.Errorf("worker error = %v, want wrapped %v", err, tc.err)
				}
				if prepareCalls != 1 || finishCalls != 1 {
					t.Errorf("prepare/finish calls = %d/%d, want 1/1", prepareCalls, finishCalls)
				}
			})
		})
	}
}
