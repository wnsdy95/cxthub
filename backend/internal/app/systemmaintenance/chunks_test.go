package systemmaintenance_test

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/systemmaintenance"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type chunkStore struct {
	sweep func(context.Context) (domain.DocChunkSweep, error)
}

func (store *chunkStore) SweepNextDocChunks(ctx context.Context) (domain.DocChunkSweep, error) {
	return store.sweep(ctx)
}

type nilChunkFunction func(context.Context) (domain.DocChunkSweep, error)

func (store nilChunkFunction) SweepNextDocChunks(ctx context.Context) (domain.DocChunkSweep, error) {
	return store(ctx)
}

func requireChunks(test *testing.T, store systemmaintenance.ChunkStore) *systemmaintenance.Chunks {
	test.Helper()
	worker, err := systemmaintenance.NewChunks(systemmaintenance.ChunksConfig{Profile: systemmaintenance.Complete, Store: store})
	if err != nil {
		test.Fatal(err)
	}
	return worker
}

func chunkRepo(index int) domain.ContentHash {
	return domain.HashContent([]byte{byte(index)})
}

func TestChunksConstructorValidatesProfilesAndStores(test *testing.T) {
	var absent *chunkStore
	var absentFunction nilChunkFunction
	for _, fixture := range []struct {
		name    string
		profile systemmaintenance.Profile
		store   systemmaintenance.ChunkStore
		invalid bool
	}{
		{name: "compatibility-unsupported", profile: systemmaintenance.Compatibility},
		{name: "compatibility-supported", profile: systemmaintenance.Compatibility, store: &chunkStore{}},
		{name: "complete-supported", profile: systemmaintenance.Complete, store: &chunkStore{}},
		{name: "complete-missing", profile: systemmaintenance.Complete, invalid: true},
		{name: "complete-typed-nil", profile: systemmaintenance.Complete, store: absent, invalid: true},
		{name: "complete-nil-function", profile: systemmaintenance.Complete, store: absentFunction, invalid: true},
		{name: "compatibility-typed-nil", profile: systemmaintenance.Compatibility, store: absent, invalid: true},
		{name: "empty-profile", store: &chunkStore{}, invalid: true},
		{name: "unknown-profile", profile: "unknown", store: &chunkStore{}, invalid: true},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			worker, err := systemmaintenance.NewChunks(systemmaintenance.ChunksConfig{Profile: fixture.profile, Store: fixture.store})
			if fixture.invalid {
				if worker != nil || !errors.Is(err, domain.ErrValidation) {
					test.Fatal("invalid configuration was accepted", worker, err)
				}
				return
			}
			if worker == nil || err != nil {
				test.Fatal("supported configuration was rejected", worker, err)
			}
		})
	}
}

func TestChunksCompatibilityWithoutStoreIsANoOp(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		worker, err := systemmaintenance.NewChunks(systemmaintenance.ChunksConfig{Profile: systemmaintenance.Compatibility})
		if err != nil {
			test.Fatal(err)
		}
		started := time.Now()
		if visited, err := worker.Process(context.Background()); visited != 0 || err != nil {
			test.Fatal("unsupported chunk cleanup performed work", visited, err)
		}
		worker.Run(context.Background())
		if time.Since(started) != 0 {
			test.Fatal("unsupported chunk cleanup waited", time.Since(started))
		}
	})
}

func TestChunksProcessVisitsUntilIdleWithinItsBounds(test *testing.T) {
	type callerKey struct{}
	for _, fixture := range []struct {
		name      string
		available int
		want      int
	}{
		{name: "idle", available: 0, want: 0},
		{name: "partial", available: 3, want: 3},
		{name: "exact-limit", available: 16, want: 16},
		{name: "more-than-one-tick", available: 40, want: 16},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			calls := 0
			worker := requireChunks(test, &chunkStore{sweep: func(ctx context.Context) (domain.DocChunkSweep, error) {
				calls++
				if ctx.Value(callerKey{}) != "caller" {
					test.Error("chunk cleanup replaced the caller's context")
				}
				if deadline, bounded := ctx.Deadline(); !bounded || time.Until(deadline) > 45*time.Second {
					test.Error("chunk cleanup visit is not bounded by the tick budget")
				}
				if calls > fixture.available {
					return domain.DocChunkSweep{}, nil
				}
				return domain.DocChunkSweep{Repo: chunkRepo(calls)}, nil
			}})
			visited, err := worker.Process(context.WithValue(context.Background(), callerKey{}, "caller"))
			if err != nil || visited != fixture.want {
				test.Fatal("visited repositories", visited, err)
			}
			if wantCalls := min(fixture.available+1, 16); calls != wantCalls {
				test.Fatal("store calls", calls, wantCalls)
			}
		})
	}
}

func TestChunksProcessContinuesPastAFailedRepository(test *testing.T) {
	broken := errors.New("repository cannot be classified")
	calls := 0
	worker := requireChunks(test, &chunkStore{sweep: func(context.Context) (domain.DocChunkSweep, error) {
		calls++
		switch calls {
		case 2, 4:
			return domain.DocChunkSweep{Repo: chunkRepo(calls)}, broken
		case 6:
			return domain.DocChunkSweep{}, nil
		}
		return domain.DocChunkSweep{Repo: chunkRepo(calls)}, nil
	}})
	visited, err := worker.Process(context.Background())
	if calls != 6 || !errors.Is(err, broken) || strings.Count(err.Error(), broken.Error()) != 2 {
		test.Fatal("failed repositories stopped or were dropped from the tick", calls, err)
	}
	if visited != 5 {
		test.Fatal("visit count", visited)
	}
}

func TestChunksProcessStopsWhenItsBudgetIsSpent(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		calls := 0
		worker := requireChunks(test, &chunkStore{sweep: func(ctx context.Context) (domain.DocChunkSweep, error) {
			calls++
			if calls < 3 {
				time.Sleep(20 * time.Second)
				return domain.DocChunkSweep{Repo: chunkRepo(calls)}, nil
			}
			<-ctx.Done()
			return domain.DocChunkSweep{}, ctx.Err()
		}})
		started := time.Now()
		visited, err := worker.Process(context.Background())
		if !errors.Is(err, context.DeadlineExceeded) || visited != 2 || calls != 3 || time.Since(started) != 45*time.Second {
			test.Fatal("tick budget", visited, calls, time.Since(started), err)
		}
	})
	synctest.Test(test, func(test *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		worker := requireChunks(test, &chunkStore{sweep: func(ctx context.Context) (domain.DocChunkSweep, error) {
			calls++
			cancel()
			return domain.DocChunkSweep{}, ctx.Err()
		}})
		if visited, err := worker.Process(ctx); !errors.Is(err, context.Canceled) || visited != 0 || calls != 1 {
			test.Fatal("canceled tick kept visiting", visited, calls, err)
		}
	})
}

func TestChunksRunTicksEveryMinuteLogsAndStopsOnCancellation(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		var logs bytes.Buffer
		previous, flags := log.Writer(), log.Flags()
		log.SetOutput(&logs)
		log.SetFlags(0)
		defer func() { log.SetOutput(previous); log.SetFlags(flags) }()
		repo := chunkRepo(1)
		broken := errors.New("repository cannot be classified")
		ticks := 0
		var at []time.Duration
		started := time.Now()
		worker := requireChunks(test, &chunkStore{sweep: func(context.Context) (domain.DocChunkSweep, error) {
			ticks++
			at = append(at, time.Since(started))
			switch ticks {
			case 1:
				return domain.DocChunkSweep{Repo: repo, Orphans: 9, Removed: 4, Bodies: 3, Bytes: 4096, Deferred: 2, Waiting: 3}, nil
			case 3:
				return domain.DocChunkSweep{Repo: repo, Orphans: 5, Waiting: 5}, nil
			case 5:
				return domain.DocChunkSweep{Repo: repo}, broken
			}
			return domain.DocChunkSweep{}, nil
		}})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { worker.Run(ctx); close(done) }()
		synctest.Wait()
		if ticks != 0 {
			test.Fatal("chunk cleanup ran before its first tick")
		}
		time.Sleep(3*time.Minute + time.Second)
		synctest.Wait()
		cancel()
		<-done
		// Tick one visits a repository and then finds the pass idle; tick two
		// visits a repository with nothing due; tick three fails once.
		if ticks != 6 || at[0] != time.Minute || at[2] != 2*time.Minute || at[4] != 3*time.Minute {
			test.Fatal("tick schedule", ticks, at)
		}
		output := logs.String()
		want := "chunk cleanup: repository " + string(repo) + ": removed 4 orphan chunk(s), freed 4096 stored byte(s) in 3 chunk bodies; 2 left for a later visit, 3 waiting for the grace period\n"
		if !strings.Contains(output, want) {
			test.Fatal("removal was not reported", output)
		}
		if !strings.Contains(output, "chunk cleanup failed: "+broken.Error()) {
			test.Fatal("failure was not reported", output)
		}
		if strings.Count(output, "\n") != 2 {
			test.Fatal("visits that changed nothing must stay quiet", output)
		}
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if ticks != 6 {
			test.Fatal("chunk cleanup kept running after cancellation", ticks)
		}
	})
}
