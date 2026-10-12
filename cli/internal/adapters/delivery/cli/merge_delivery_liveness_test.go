package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func makeMergeDeliveryDue(t *testing.T, root, key string) mergeDelivery {
	t.Helper()
	job, err := readMergeDelivery(root, key)
	if err != nil {
		t.Fatal(err)
	}
	job.LastAttempt = time.Now().Add(-2 * time.Minute).UTC()
	if err := writeMergeDelivery(root, key, job); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestMergeDeliveryAutomaticRetryDueBackoff(t *testing.T) {
	now := time.Now().UTC()
	for attempt, delay := range []time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute, time.Minute} {
		job := mergeDelivery{Attempts: attempt}
		if !retryDueMergeDelivery(job, now) {
			t.Fatal("unattempted legacy job was not due")
		}
		if attempt == 0 {
			continue
		}
		job.LastAttempt = now
		if retryDueMergeDelivery(job, now.Add(delay-time.Nanosecond)) || !retryDueMergeDelivery(job, now.Add(delay)) {
			t.Fatalf("attempt %d did not retain %s backoff", attempt, delay)
		}
		job.LeaseUntil = now.Add(2 * time.Minute)
		if retryDueMergeDelivery(job, now.Add(delay)) || !retryDueMergeDelivery(job, job.LeaseUntil) {
			t.Fatal("retry bypassed a live lease or lost lease recovery")
		}
		job.Completed = true
		if retryDueMergeDelivery(job, now.Add(time.Hour)) {
			t.Fatal("completed delivery was replayable")
		}
	}
	if !retryDueMergeDelivery(mergeDelivery{LastAttempt: now.Add(time.Hour), Attempts: 99}, now) {
		t.Fatal("future timestamp postponed recovery indefinitely")
	}
}

func TestMergeDeliveryAutomaticSelectionBarriersAndFairness(t *testing.T) {
	now := time.Now().UTC()
	scope := mergeEvidence{RepoID: "repo", BranchID: "main"}
	for _, scenario := range []struct {
		name string
		edit func(*mergeDelivery, *mergeDelivery)
		want []string
	}{
		{"ordinary cooling", func(older, newer *mergeDelivery) { older.LastAttempt = now }, nil},
		{"ordinary due", func(older, newer *mergeDelivery) {}, []string{"older"}},
		{"waiting cooling", func(older, newer *mergeDelivery) { older.WaitingSource, older.LastAttempt = true, now }, []string{"newer"}},
		{"review cooling", func(older, newer *mergeDelivery) { older.ReviewRequired, older.LastAttempt = true, now }, []string{"newer"}},
		{"frozen waiting cooling", func(older, newer *mergeDelivery) {
			older.WaitingSource, older.LastAttempt = true, now
			older.Candidates = []domain.ContentHash{briefingHash("frozen")}
		}, nil},
		{"older waiting due", func(older, newer *mergeDelivery) {
			older.WaitingSource = true
			newer.LastAttempt = now.Add(-time.Minute)
		}, []string{"older", "newer"}},
		{"younger waiting blocked", func(older, newer *mergeDelivery) {
			older.LastAttempt, newer.WaitingSource = now, true
		}, nil},
		{"completed", func(older, newer *mergeDelivery) { older.Completed = true }, []string{"newer"}},
		{"other identity", func(older, newer *mergeDelivery) { older.Evidence.BranchID = "other" }, []string{"newer"}},
		{"reset away", func(older, newer *mergeDelivery) { older.Evidence.After = "unrelated" }, []string{"newer"}},
		{"live lease", func(older, newer *mergeDelivery) { older.LeaseUntil = now.Add(time.Minute) }, nil},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			older := mergeDelivery{Evidence: scope, Attempts: 8, LastAttempt: now.Add(-2 * time.Minute)}
			newer := mergeDelivery{Evidence: scope}
			older.Evidence.After, newer.Evidence.After = "old", "new"
			scenario.edit(&older, &newer)
			got := automaticMergeDeliveryKeys(map[string]mergeDelivery{"older": older, "newer": newer}, scope, map[string]int{"old": 1, "new": 0}, now)
			if !reflect.DeepEqual(got, scenario.want) {
				t.Fatalf("automatic selection=%v want=%v", got, scenario.want)
			}
		})
	}
}

func TestMergeDeliveryAutomaticResumeWithoutFetchOrRange(t *testing.T) {
	for _, failure := range []string{"fetch", "append", "configured origin", "no Git origin"} {
		t.Run(failure, func(t *testing.T) {
			root, container, syncer := newMergeDeliveryFixture(t)
			if failure == "configured origin" {
				if err := configFixtureSave(root, remotecfg.Remotes{"origin": "https://context.example.test/team/repo"}); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "no Git origin" {
				runGitForTest(t, root, "remote", "remove", "origin")
			}
			if failure == "fetch" {
				syncer.pullErr = context.DeadlineExceeded
			} else {
				syncer.appendErr = errors.New("offline")
			}
			handleIncomingContexts(context.Background(), container, root)
			key, original := mergeDeliveryJob(t, root)
			if original.Completed || original.Attempts != 1 {
				t.Fatalf("failure did not leave one saved attempt: %+v", original)
			}
			if _, err := os.Stat(filepath.Join(root, fetchObservationDir)); !os.IsNotExist(err) {
				t.Fatalf("fixture has fetch records: %v", err)
			}
			runGitForTest(t, root, "update-ref", "-d", "ORIG_HEAD")
			head := gitOut(root, "rev-parse", "HEAD")
			syncer.pullErr, syncer.appendErr = nil, nil
			makeMergeDeliveryDue(t, root, key)
			starts := 0
			if err := spawnFetchDiscovery(context.Background(), root, time.Now(), func() error {
				starts++
				return runFetchDiscoveryWorkerWithWait(context.Background(), container, root, func(context.Context, time.Duration) error {
					t.Fatal("successful merge replay did not drain")
					return nil
				})
			}); err != nil {
				t.Fatal(err)
			}
			_, saved := mergeDeliveryJob(t, root)
			if starts != 1 || !saved.Completed || saved.Attempts != 2 || !reflect.DeepEqual(saved.Evidence, original.Evidence) || saved.EvidenceHash != original.EvidenceHash || syncer.ref.Target != briefingHash("merge delivery source") {
				t.Fatalf("saved range did not resume: starts=%d job=%+v", starts, saved)
			}
			if gitOut(root, "rev-parse", "HEAD") != head || gitOut(root, "rev-parse", "--verify", "ORIG_HEAD") != "" {
				t.Fatal("automatic replay depended on or moved Git selection")
			}
			if err := spawnFetchDiscovery(context.Background(), root, time.Now().Add(time.Hour), func() error {
				t.Fatal("completed merge relaunched worker")
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMergeDeliveryAutomaticPreservesBaselineAndReportsOnlyProgress(t *testing.T) {
	root, container, syncer := newMergeDeliveryFixture(t)
	if err := persistMergeDelivery(context.Background(), container, root, readIncomingGitRange(root)); err != nil {
		t.Fatal(err)
	}
	syncer.appendErr = errors.New("offline")
	syncer.onObserve = func() {
		if baseline, ok := capture.ReadPullBriefingCursor(root, "main"); !ok || baseline != briefingHash("merge delivery base") {
			t.Fatalf("baseline was not saved before remote work: %s %v", baseline, ok)
		}
	}
	pending, progressed, err := replayAutomaticMergeDeliveries(context.Background(), container, root)
	if err != nil || !pending || !progressed {
		t.Fatalf("new frozen plan was not progress: %v %v %v", pending, progressed, err)
	}
	key, original := mergeDeliveryJob(t, root)
	if original.PlanHash == "" {
		t.Fatal("progress preceded a durable plan")
	}
	makeMergeDeliveryDue(t, root, key)
	pending, progressed, err = replayAutomaticMergeDeliveries(context.Background(), container, root)
	if err != nil || !pending || progressed {
		t.Fatalf("failed attempt was counted as progress: %v %v %v", pending, progressed, err)
	}
	_, failed := mergeDeliveryJob(t, root)
	if failed.Attempts != 2 || failed.PlanHash != original.PlanHash {
		t.Fatalf("retry lost frozen plan: %+v", failed)
	}
	pending, progressed, err = replayAutomaticMergeDeliveries(context.Background(), container, root)
	_, paced := mergeDeliveryJob(t, root)
	if err != nil || !pending || progressed || !reflect.DeepEqual(paced, failed) {
		t.Fatalf("early replay changed a paced job: %v %v %v", pending, progressed, err)
	}
	syncer.appendErr = nil
	makeMergeDeliveryDue(t, root, key)
	pending, progressed, err = replayAutomaticMergeDeliveries(context.Background(), container, root)
	if err != nil || pending || !progressed {
		t.Fatalf("verified completion was not progress: %v %v %v", pending, progressed, err)
	}
}

func TestMergeDeliveryAutomaticScopeResetAndCorruptionDoNotWrite(t *testing.T) {
	for _, changed := range []string{"Git origin", "context origin", "repository", "branch identity", "inactive", "local branch", "detached", "reset", "corrupt"} {
		t.Run(changed, func(t *testing.T) {
			root, container, syncer := newMergeDeliveryFixture(t)
			if err := persistMergeDelivery(context.Background(), container, root, readIncomingGitRange(root)); err != nil {
				t.Fatal(err)
			}
			key, saved := mergeDeliveryJob(t, root)
			switch changed {
			case "Git origin":
				runGitForTest(t, root, "remote", "set-url", "origin", "https://example.test/elsewhere")
			case "context origin":
				t.Setenv("CXT_REMOTE", "https://context.example.test/elsewhere")
			case "repository":
				container.ResolveRepo = func(context.Context, string) (domain.Repo, error) {
					return domain.Repo{ID: string(briefingHash("another repository"))}, nil
				}
			case "branch identity":
				container.History.(*mergeDeliveryHistory).binding.BranchID = "replacement"
			case "inactive":
				container.History.(*mergeDeliveryHistory).binding.Inactive = true
			case "local branch":
				runGitForTest(t, root, "checkout", "-b", "another")
			case "detached":
				runGitForTest(t, root, "checkout", "--detach")
			case "reset":
				runGitForTest(t, root, "reset", "--hard", "ORIG_HEAD")
			case "corrupt":
				saved.EvidenceHash = briefingHash("tampered")
				if err := writeMergeDelivery(root, key, saved); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(filepath.Join(root, mergeDeliveryPath(key)))
			if err != nil {
				t.Fatal(err)
			}
			pending, progressed, _ := replayAutomaticMergeDeliveries(context.Background(), container, root)
			if pending || progressed || len(syncer.appends) != 0 {
				t.Fatalf("cross-scope evidence replayed: %v %v %v", pending, progressed, syncer.appends)
			}
			after, err := os.ReadFile(filepath.Join(root, mergeDeliveryPath(key)))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("cross-scope attempt mutated durable evidence: %v", err)
			}
			if _, ok := capture.ReadPullBriefingCursor(root, "main"); ok {
				t.Fatal("cross-scope job wrote a briefing baseline")
			}
			if changed == "repository" || changed == "branch identity" || changed == "inactive" {
				return
			}
			if pending, _ := pendingMergeDeliveryWake(context.Background(), root, root, time.Now()); pending {
				t.Fatal("cheap scope check woke invalid evidence")
			}
		})
	}
}

func TestMergeDeliveryAutomaticWorkerFailurePacing(t *testing.T) {
	for _, failed := range []string{"fetch", "append"} {
		t.Run(failed, func(t *testing.T) {
			root, container, syncer := newMergeDeliveryFixture(t)
			if err := persistMergeDelivery(context.Background(), container, root, readIncomingGitRange(root)); err != nil {
				t.Fatal(err)
			}
			if failed == "fetch" {
				syncer.pullErr = context.DeadlineExceeded
			} else {
				syncer.appendErr = errors.New("offline")
			}
			waits := 0
			var pauses []time.Duration
			err := runFetchDiscoveryWorkerWithWait(context.Background(), container, root, func(ctx context.Context, delay time.Duration) error {
				waits++
				pauses = append(pauses, delay)
				want := min(time.Second<<min(waits-1, 6), time.Minute)
				if waits > 8 || delay != want {
					t.Fatalf("failure reset or escaped bounded backoff: %v", pauses)
				}
				key, saved := mergeDeliveryJob(t, root)
				if saved.Attempts != waits {
					t.Fatalf("attempt was skipped or spun: %+v", saved)
				}
				if due, err := pendingMergeDeliveryWake(ctx, root, root, saved.LastAttempt.Add(want-time.Nanosecond)); err != nil || due {
					t.Fatalf("observer bypassed durable pacing: %v %v", due, err)
				}
				makeMergeDeliveryDue(t, root, key)
				if waits == 8 {
					syncer.pullErr, syncer.appendErr = nil, nil
				}
				return nil
			})
			_, saved := mergeDeliveryJob(t, root)
			if err != nil || waits != 8 || !saved.Completed || saved.Attempts != 9 {
				t.Fatalf("paced worker failed to recover: waits=%d saved=%+v err=%v", waits, saved, err)
			}
		})
	}
}

func TestMergeDeliveryAutomaticWaitingItemIsNotStarved(t *testing.T) {
	root, container, syncer := newMergeDeliveryFixture(t)
	syncer.snapshots = syncer.snapshots[1:]
	handleIncomingContexts(context.Background(), container, root)
	olderKey, older := mergeDeliveryJob(t, root)
	if !older.WaitingSource {
		t.Fatal("older delivery is not waiting")
	}
	code := queueMergeDeliveryCommit(t, root, container, "newer failing merge")
	target := briefingHash("newer failing context")
	syncer.snapshots = append(syncer.snapshots, domain.Snapshot{ID: target, Message: fmt.Sprintf("source [git %s]", code)})
	syncer.appendErr = errors.New("offline")
	replaySavedMergeDeliveries(context.Background(), container, root)
	jobs, err := listMergeDeliveries(root)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("missing successor: %v", err)
	}
	for key, saved := range jobs {
		saved.LastAttempt = time.Now().Add(-2 * time.Minute)
		if key == olderKey {
			saved.LastAttempt = saved.LastAttempt.Add(-time.Minute)
		}
		if err := writeMergeDelivery(root, key, saved); err != nil {
			t.Fatal(err)
		}
	}
	if pending, _, err := replayAutomaticMergeDeliveries(context.Background(), container, root); err != nil || !pending {
		t.Fatalf("pending queue was lost: %v %v", pending, err)
	}
	resumed, err := readMergeDelivery(root, olderKey)
	if err != nil || resumed.Attempts != older.Attempts+1 {
		t.Fatalf("failing successor starved older waiting delivery: %+v %v", resumed, err)
	}
}

func TestMergeDeliveryAutomaticBackoffPredecessorBlocksSuccessor(t *testing.T) {
	root, container, syncer := newMergeDeliveryFixture(t)
	syncer.appendErr = errors.New("predecessor offline")
	handleIncomingContexts(context.Background(), container, root)
	olderKey, older := mergeDeliveryJob(t, root)
	older.Attempts, older.LastAttempt = 8, time.Now().UTC()
	if err := writeMergeDelivery(root, olderKey, older); err != nil {
		t.Fatal(err)
	}
	code := queueMergeDeliveryCommit(t, root, container, "ready successor")
	target := briefingHash("ready successor")
	syncer.snapshots = append(syncer.snapshots, domain.Snapshot{ID: target, Message: fmt.Sprintf("source [git %s]", code)})
	syncer.appendErr = nil
	before, err := listMergeDeliveries(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, attempted := replayMergeDeliveriesAt(context.Background(), container, root, true); attempted {
		t.Fatal("automatic replay bypassed ordinary predecessor backoff")
	}
	pending, progressed, err := replayAutomaticMergeDeliveries(context.Background(), container, root)
	if err != nil || !pending || progressed {
		t.Fatalf("blocked successor changed progress: %v %v %v", pending, progressed, err)
	}
	after, err := listMergeDeliveries(root)
	if err != nil || !reflect.DeepEqual(before, after) || len(syncer.appends) != 1 {
		t.Fatalf("backoff allowed a successor write: %v %v", syncer.appends, err)
	}
	makeMergeDeliveryDue(t, root, olderKey)
	if pending, progressed, err := replayAutomaticMergeDeliveries(context.Background(), container, root); err != nil || !pending || !progressed {
		t.Fatalf("due predecessor did not complete first: %v %v %v", pending, progressed, err)
	}
	if len(syncer.appends) != 2 || syncer.appends[1] != briefingHash("merge delivery source") {
		t.Fatalf("ready successor overtook due predecessor: %v", syncer.appends)
	}
	if pending, progressed, err := replayAutomaticMergeDeliveries(context.Background(), container, root); err != nil || pending || !progressed {
		t.Fatalf("successor did not resume after predecessor: %v %v %v", pending, progressed, err)
	}
	if len(syncer.appends) != 3 || syncer.appends[2] != target {
		t.Fatalf("successor completion order changed: %v", syncer.appends)
	}
}

func TestMergeDeliveryAutomaticRecoversExpiredLease(t *testing.T) {
	root, container, _ := newMergeDeliveryFixture(t)
	if err := persistMergeDelivery(context.Background(), container, root, readIncomingGitRange(root)); err != nil {
		t.Fatal(err)
	}
	key, saved := mergeDeliveryJob(t, root)
	saved.Attempts, saved.LastAttempt = 5, time.Now().Add(-2*time.Minute)
	saved.Lease, saved.LeaseUntil = "stopped-worker", time.Now().Add(time.Minute)
	if err := writeMergeDelivery(root, key, saved); err != nil {
		t.Fatal(err)
	}
	if due, err := pendingMergeDeliveryWake(context.Background(), root, root, time.Now()); err != nil || due {
		t.Fatalf("live lease woke automatic replay: %v %v", due, err)
	}
	if pending, progressed, err := replayAutomaticMergeDeliveries(context.Background(), container, root); err != nil || !pending || progressed {
		t.Fatalf("live lease was not preserved: %v %v %v", pending, progressed, err)
	}
	saved.LeaseUntil = time.Now().Add(-time.Second)
	if err := writeMergeDelivery(root, key, saved); err != nil {
		t.Fatal(err)
	}
	if due, err := pendingMergeDeliveryWake(context.Background(), root, root, time.Now()); err != nil || !due {
		t.Fatalf("expired lease did not wake replay: %v %v", due, err)
	}
	if pending, progressed, err := replayAutomaticMergeDeliveries(context.Background(), container, root); err != nil || pending || !progressed {
		t.Fatalf("expired lease did not recover: %v %v %v", pending, progressed, err)
	}
	_, completed := mergeDeliveryJob(t, root)
	if !completed.Completed || completed.Attempts != 6 || completed.Lease != "" || !completed.LeaseUntil.IsZero() {
		t.Fatalf("recovered claim lost checkpoint: %+v", completed)
	}
	if err := updateLeasedMerge(context.Background(), root, key, "stopped-worker", func(job *mergeDelivery) { job.Completed = false }); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("stopped worker was not fenced: %v", err)
	}
}

func TestMergeDeliveryAutomaticUnchangedObserverRestartsStoppedWorker(t *testing.T) {
	root, container, syncer := newMergeDeliveryFixture(t)
	if err := persistMergeDelivery(context.Background(), container, root, readIncomingGitRange(root)); err != nil {
		t.Fatal(err)
	}
	runGitForTest(t, root, "update-ref", "-d", "ORIG_HEAD")
	syncer.pullErr = context.DeadlineExceeded
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")
	id := "11111111-1111-4111-8111-111111111111"
	transcript := writeCodexRollout(t, os.Getenv("HOME"), root, id, time.Now())
	if err := capture.TrackAppSession(root, domain.ProviderCodex, id, transcript); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	wakes, starts, polls := 0, 0, 0
	stopped := errors.New("worker stopped")
	err := capture.WatchSession(ctx, root, domain.ProviderCodex, id, capture.SessionWatch{
		Poll: func(context.Context) error { polls++; return nil },
		Wake: func(ctx context.Context) {
			wakes++
			if err := spawnFetchDiscovery(ctx, root, time.Now(), func() error {
				starts++
				workerErr := runFetchDiscoveryWorkerWithWait(ctx, container, root, func(ctx context.Context, delay time.Duration) error {
					if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 30*time.Minute {
						t.Fatal("worker lost lifetime bound")
					}
					return stopped
				})
				if starts == 1 {
					if !errors.Is(workerErr, stopped) {
						t.Fatalf("worker did not stop with durable work: %v", workerErr)
					}
					key, saved := mergeDeliveryJob(t, root)
					if saved.Completed || saved.Attempts != 1 {
						t.Fatalf("stopped worker lost checkpoint: %+v", saved)
					}
					makeMergeDeliveryDue(t, root, key)
					syncer.pullErr = nil
					return nil
				}
				cancel()
				return workerErr
			}); err != nil {
				t.Error(err)
				cancel()
			}
		},
	})
	_, saved := mergeDeliveryJob(t, root)
	if !errors.Is(err, context.Canceled) || wakes != 2 || starts != 2 || polls != 1 || !saved.Completed || saved.Attempts != 2 {
		t.Fatalf("unchanged observer lost merge recovery: wakes=%d starts=%d polls=%d saved=%+v err=%v", wakes, starts, polls, saved, err)
	}
}
