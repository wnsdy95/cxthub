package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func checkPRReadyJobs(t *testing.T, st interface {
	outbound.PRJobStore
	outbound.MetadataStore
}) {
	for _, state := range []string{"waiting", "retrying"} {
		for _, targeted := range []bool{false, true} {
			t.Run(state+map[bool]string{false: "/worker", true: "/targeted"}[targeted], func(t *testing.T) {
				ctx := context.Background()
				repo := domain.HashContent([]byte(t.Name()))
				if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
					t.Fatal(err)
				}
				now := time.Now().UTC()
				old := sourceJob(repo, 1, now)
				old.State, old.NextAttempt = state, now.Add(5*time.Minute)
				ready := sourceJob(repo, 2, now.Add(time.Millisecond))
				ready.State, ready.NextAttempt = "waiting", now
				for _, j := range []domain.PRPromotionJob{old, ready} {
					if _, err := st.EnqueuePRJob(ctx, j); err != nil {
						t.Fatal(err)
					}
				}
				id := ""
				if targeted {
					id = ready.ID
				}
				claims := make(chan domain.PRPromotionJob, 8)
				var wg sync.WaitGroup
				for i := 0; i < 8; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						j, err := st.ClaimPRJob(ctx, repo, id, now, time.Minute)
						if err == nil {
							claims <- j
						} else if !errors.Is(err, domain.ErrNotFound) {
							t.Errorf("claim: %v", err)
						}
					}()
				}
				wg.Wait()
				close(claims)
				if len(claims) != 1 {
					t.Fatalf("ready successor claims=%d, want exactly one", len(claims))
				}
				got := <-claims
				if got.ID != ready.ID {
					t.Fatalf("claimed %s, want ready successor %s", got.ID, ready.ID)
				}
				// A predecessor becoming due cannot overlap the successor's active
				// lease. Even an expired lease must be reclaimed and fenced first.
				if err := st.RetryPRJob(ctx, repo, old.ID, now); err != nil {
					t.Fatal(err)
				}
				if _, err := st.ClaimPRJob(ctx, repo, old.ID, now, time.Minute); !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("overlapped running successor: %v", err)
				}
				recovered, err := st.ClaimPRJob(ctx, repo, "", now.Add(2*time.Minute), time.Minute)
				if err != nil || recovered.ID != ready.ID {
					t.Fatalf("lease recovery = %+v, %v", recovered, err)
				}
				got.State = "completed"
				if err := st.FinishPRJob(ctx, got); !errors.Is(err, domain.ErrConflict) {
					t.Fatalf("stale worker finished: %v", err)
				}
				recovered.State = "completed"
				if err := st.FinishPRJob(ctx, recovered); err != nil {
					t.Fatal(err)
				}
				if got, err = st.ClaimPRJob(ctx, repo, "", now.Add(2*time.Minute), time.Minute); err != nil || got.ID != old.ID {
					t.Fatalf("due predecessor did not resume: %+v, %v", got, err)
				}
			})
		}
	}
}

func TestFSPRReadyJobs(t *testing.T) { checkPRReadyJobs(t, NewFSStore(t.TempDir())) }
