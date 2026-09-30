//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func stagePublicationPG(t *testing.T, s *PostgresStore, ctx context.Context, doc domain.VerifiedSessionDoc) pgDocPublication {
	t.Helper()
	publication, err := s.PrepareDocJob(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := publication.(pgDocPublication)
	if !ok {
		t.Fatalf("unexpected publication type %T", publication)
	}
	return p
}

func stageCountPG(t *testing.T, s *PostgresStore, ctx context.Context, want int, query string, args ...any) {
	t.Helper()
	var got int
	if err := s.db(ctx).QueryRow(ctx, query, args...).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("count=%d want=%d for %s", got, want, query)
	}
}

func stagePinsPG(t *testing.T, s *PostgresStore, ctx context.Context, j domain.DocFinalizationJob, p pgDocPublication, present bool) {
	t.Helper()
	want := 0
	if present {
		want = len(p.doc.read.blocks)
	}
	stageCountPG(t, s, ctx, want, `SELECT count(*) FROM doc_read_block_preparations_v3 WHERE repo_id=$1 AND job_id=$2`, j.RepoID, j.ID)
	for _, b := range p.doc.read.blocks {
		n := 0
		if present {
			n = 1
		}
		stageCountPG(t, s, ctx, n, `SELECT count(*) FROM doc_read_block_preparations_v3 WHERE repo_id=$1 AND job_id=$2 AND version=$3 AND block_hash=$4`, j.RepoID, j.ID, j.Version, b.Hash)
	}
}

func stageDerivativesPG(t *testing.T, s *PostgresStore, ctx context.Context, p pgDocPublication, present bool) {
	t.Helper()
	for _, b := range p.doc.read.blocks {
		blocks, events := 0, 0
		if present {
			blocks, events = 1, b.Count
		}
		stageCountPG(t, s, ctx, blocks, `SELECT count(*) FROM doc_read_blocks_v3 WHERE hash=$1`, b.Hash)
		stageCountPG(t, s, ctx, events, `SELECT count(*) FROM doc_read_block_events_v3 WHERE block_hash=$1`, b.Hash)
		stageCountPG(t, s, ctx, events, `SELECT count(*) FROM doc_search_events_v2 WHERE hash=ANY($1::text[])`, b.EventHashes())
	}
}

func stageInvisiblePG(t *testing.T, s *PostgresStore, ctx context.Context, j domain.DocFinalizationJob) {
	t.Helper()
	assertDocNotPublishedPG(t, s, ctx, j)
	stageCountPG(t, s, ctx, 0, `SELECT count(*) FROM doc_read_index_current WHERE hash=$1`, j.DocHash)
	stageCountPG(t, s, ctx, 0, `SELECT count(*) FROM doc_read_block_locations_v3 WHERE doc_hash=$1`, j.DocHash)
	stageCountPG(t, s, ctx, 0, `SELECT count(*) FROM doc_read_event_locations_current WHERE doc_hash=$1`, j.DocHash)
	for _, repo := range []domain.ContentHash{j.RepoID, domain.HashContent([]byte("foreign stage " + string(j.RepoID)))} {
		if _, err := s.GetDoc(ctx, repo, j.DocHash); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("staged document visible to %s: %v", repo, err)
		}
		if _, err := s.DocReadIndex(ctx, repo, j.DocHash); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("staged read index visible to %s: %v", repo, err)
		}
		if hits, err := s.SearchDocEvents(ctx, repo, j.DocHash, "", -1, 10); !errors.Is(err, domain.ErrNotFound) || len(hits) != 0 {
			t.Fatalf("staged search visible to %s: %+v %v", repo, hits, err)
		}
		if hits, err := s.MatchingDocHashes(ctx, repo, ""); err != nil || hits[j.DocHash] {
			t.Fatalf("staged search candidate visible to %s: %+v %v", repo, hits, err)
		}
	}
}

func stageCompletedPG(t *testing.T, s *PostgresStore, ctx context.Context, j domain.DocFinalizationJob, p pgDocPublication) {
	t.Helper()
	job, err := s.GetDocJob(ctx, j.RepoID, j.ID)
	if err != nil || job.State != "completed" || job.Version != j.Version {
		t.Fatalf("completion state: %+v %v", job, err)
	}
	if got, err := s.GetDoc(ctx, j.RepoID, j.DocHash); err != nil || got.Hash != j.DocHash {
		t.Fatalf("completed document: %+v %v", got, err)
	}
	if idx, err := s.DocReadIndex(ctx, j.RepoID, j.DocHash); err != nil || len(idx.Events) != p.doc.read.eventCount {
		t.Fatalf("completed index events=%d want=%d: %v", len(idx.Events), p.doc.read.eventCount, err)
	}
	if hits, err := s.SearchDocEvents(ctx, j.RepoID, j.DocHash, "", -1, 1); err != nil || len(hits) != 1 {
		t.Fatalf("completed search: %+v %v", hits, err)
	}
	stagePinsPG(t, s, ctx, j, p, false)
	stageDerivativesPG(t, s, ctx, p, true)
}

func TestPGDocStageCompletesPreparationWhileRepositoryWriterHeld(t *testing.T) {
	s, ctx := chunkReusePG(t) // Only the parent's disposable CXT_TEST_DSN.
	j, doc, _ := preparedJobFixturePG(t, s, ctx)
	p := stagePublicationPG(t, s, ctx, doc)
	var ownershipBefore int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM repo_blobs WHERE repo_id=$1`, j.RepoID).Scan(&ownershipBefore); err != nil {
		t.Fatal(err)
	}

	writerCtx, cancelWriter := context.WithTimeout(ctx, 8*time.Second)
	defer cancelWriter()
	locked, release, writerDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		writerDone <- s.WithinRepository(writerCtx, j.RepoID, func(context.Context) error {
			close(locked)
			select {
			case <-release:
				return nil
			case <-writerCtx.Done():
				return writerCtx.Err()
			}
		})
	}()
	writerJoined, released := false, false
	unblock := func() {
		if !released {
			close(release)
			released = true
		}
	}
	defer func() {
		unblock()
		cancelWriter()
		if !writerJoined {
			select {
			case err := <-writerDone:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("repository writer cleanup: %v", err)
				}
			case <-time.After(6 * time.Second):
				t.Error("repository writer did not exit after release")
			}
		}
	}()
	select {
	case <-locked:
	case err := <-writerDone:
		writerJoined = true
		t.Fatalf("repository writer exited before lock: %v", err)
	case <-writerCtx.Done():
		t.Fatal("repository writer did not acquire lock", writerCtx.Err())
	}

	completeCtx, cancelComplete := context.WithTimeout(ctx, 8*time.Second)
	defer cancelComplete()
	completeDone := make(chan error, 1)
	go func() { completeDone <- p.Complete(completeCtx, j, time.Now().UTC()) }()
	completeJoined := false
	defer func() {
		unblock()
		cancelComplete()
		if !completeJoined {
			select {
			case err := <-completeDone:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("publication cleanup: %v", err)
				}
			case <-time.After(6 * time.Second):
				t.Error("publication did not exit after cancellation")
			}
		}
	}()

	stageCtx, cancelStage := context.WithTimeout(ctx, 3*time.Second)
	defer cancelStage()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var pins int
		if err := s.pool.QueryRow(stageCtx, `SELECT count(*) FROM doc_read_block_preparations_v3 WHERE repo_id=$1 AND job_id=$2 AND version=$3`, j.RepoID, j.ID, j.Version).Scan(&pins); err != nil {
			t.Fatal("preparation did not commit while repository writer held", err)
		}
		if pins == len(p.doc.read.blocks) {
			break
		}
		select {
		case err := <-completeDone:
			completeJoined = true
			t.Fatalf("publication exited while repository writer held: %v", err)
		case <-stageCtx.Done():
			t.Fatal("preparation waited behind repository writer", stageCtx.Err())
		case <-ticker.C:
		}
	}
	stagePinsPG(t, s, stageCtx, j, p, true)
	stageDerivativesPG(t, s, stageCtx, p, true)
	stageInvisiblePG(t, s, stageCtx, j)
	stageCountPG(t, s, stageCtx, ownershipBefore, `SELECT count(*) FROM repo_blobs WHERE repo_id=$1`, j.RepoID)
	stageCountPG(t, s, stageCtx, 0, `SELECT count(*) FROM blobs WHERE hash=$1`, j.DocHash)
	select {
	case err := <-completeDone:
		completeJoined = true
		t.Fatalf("publication bypassed repository writer: %v", err)
	default:
	}
	unblock()
	select {
	case err := <-writerDone:
		writerJoined = true
		if err != nil {
			t.Fatal(err)
		}
	case <-writerCtx.Done():
		t.Fatal("released repository writer did not finish", writerCtx.Err())
	}
	select {
	case err := <-completeDone:
		completeJoined = true
		if err != nil {
			t.Fatal(err)
		}
	case <-completeCtx.Done():
		t.Fatal("publication did not finish after writer release", completeCtx.Err())
	}
	stageCompletedPG(t, s, ctx, j, p)
}

func sharedStageFixturePG(t *testing.T, s *PostgresStore, ctx context.Context) (domain.DocFinalizationJob, pgDocPublication, domain.VerifiedSessionDoc) {
	t.Helper()
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err := s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	owner := readBlockDoc(t, string(repo), "published owner tail", 257)
	doc := readBlockDoc(t, string(repo), "staged tail", 257)
	j, p := stageJobForDocPG(t, s, ctx, repo, doc)
	return j, p, owner
}

func stageJobForDocPG(t *testing.T, s *PostgresStore, ctx context.Context, repo domain.ContentHash, doc domain.VerifiedSessionDoc) (domain.DocFinalizationJob, pgDocPublication) {
	t.Helper()
	chunks, ok := domain.PlanDocChunks(doc.Bytes())
	if !ok {
		t.Fatal("stage fixture must use chunks")
	}
	if _, _, err := s.PutChunks(ctx, repo, chunks.Bodies); err != nil {
		t.Fatal(err)
	}
	j, err := domain.NewDocFinalizationJob(repo, doc.Hash(), chunks.Manifest, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueDocJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	j, err = s.ClaimDocJob(ctx, repo, time.Now().UTC(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return j, stagePublicationPG(t, s, ctx, doc)
}

func TestPGDocStagePinRetainsBlocksAfterLastPublishedOwnerDeletion(t *testing.T) {
	s, ctx := chunkReusePG(t)
	j, p, owner := sharedStageFixturePG(t, s, ctx)
	if _, err := s.PutVerifiedDoc(ctx, j.RepoID, owner); err != nil {
		t.Fatal(err)
	}
	ownerPlan := stagePublicationPG(t, s, ctx, owner)
	if len(p.doc.read.blocks) < 3 || ownerPlan.doc.read.blocks[0].Hash != p.doc.read.blocks[0].Hash || ownerPlan.doc.read.blocks[2].Hash == p.doc.read.blocks[2].Hash {
		t.Fatal("fixture must include shared and distinct blocks")
	}
	if err := p.stageReadBlocks(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDoc(ctx, j.RepoID, owner.Hash()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDoc(ctx, j.RepoID, owner.Hash()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("last published owner was not deleted", err)
	}
	stagePinsPG(t, s, ctx, j, p, true)
	stageDerivativesPG(t, s, ctx, p, true)
	stageInvisiblePG(t, s, ctx, j)
	stageCountPG(t, s, ctx, 0, `SELECT count(*) FROM doc_read_blocks_v3 WHERE hash=$1`, ownerPlan.doc.read.blocks[2].Hash)
	if err := p.Complete(ctx, j, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	stageCompletedPG(t, s, ctx, j, p)
	if err := s.DeleteDoc(ctx, j.RepoID, j.DocHash); err != nil {
		t.Fatal(err)
	}
	stageDerivativesPG(t, s, ctx, p, false)
}

func TestPGDocStageFinishRetiresPinsAndPreservesRealOwners(t *testing.T) {
	for _, state := range []string{"rejected", "retrying"} {
		for _, keepOwner := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/owner=%t", state, keepOwner), func(t *testing.T) {
				s, ctx := chunkReusePG(t)
				j, p, owner := sharedStageFixturePG(t, s, ctx)
				if keepOwner {
					if _, err := s.PutVerifiedDoc(ctx, j.RepoID, owner); err != nil {
						t.Fatal(err)
					}
				}
				if err := p.stageReadBlocks(ctx, j); err != nil {
					t.Fatal(err)
				}
				// A lease-only update must keep the current generation's pins.
				if err := s.RenewDocJob(ctx, j, time.Now().UTC(), time.Minute); err != nil {
					t.Fatal(err)
				}
				stagePinsPG(t, s, ctx, j, p, true)
				stageDerivativesPG(t, s, ctx, p, true)
				stageInvisiblePG(t, s, ctx, j)
				finished := j
				finished.State, finished.Reason = state, "simulated worker failure"
				finished.LeaseUntil = time.Time{}
				finished.UpdatedAt = time.Now().UTC()
				finished.NextAttempt = finished.UpdatedAt.Add(time.Minute)
				if err := s.FinishDocJob(ctx, finished, finished.UpdatedAt); err != nil {
					t.Fatal(err)
				}
				got, err := s.GetDocJob(ctx, j.RepoID, j.ID)
				if err != nil || got.State != state || got.Version != j.Version {
					t.Fatalf("finished state: %+v %v", got, err)
				}
				stagePinsPG(t, s, ctx, j, p, false)
				if keepOwner {
					if idx, err := s.DocReadIndex(ctx, j.RepoID, owner.Hash()); err != nil || len(idx.Events) != 258 {
						t.Fatalf("remaining owner index events=%d: %v", len(idx.Events), err)
					}
					if hits, err := s.SearchDocEvents(ctx, j.RepoID, owner.Hash(), "50%_\\", 127, 3); err != nil || len(hits) != 3 || hits[0].Index != 128 || hits[2].Index != 130 {
						t.Fatalf("remaining owner search: %+v %v", hits, err)
					}
					stageDerivativesPG(t, s, ctx, stagePublicationPG(t, s, ctx, owner), true)
					stageCountPG(t, s, ctx, 0, `SELECT count(*) FROM doc_read_blocks_v3 WHERE hash=$1`, p.doc.read.blocks[2].Hash)
					stageCountPG(t, s, ctx, 0, `SELECT count(*) FROM doc_search_events_v2 WHERE hash=$1`, p.doc.read.blocks[2].EventHashes()[1])
					if err := s.DeleteDoc(ctx, j.RepoID, owner.Hash()); err != nil {
						t.Fatal(err)
					}
				}
				stageDerivativesPG(t, s, ctx, p, false)
				if err := p.stageReadBlocks(ctx, j); !errors.Is(err, domain.ErrConflict) {
					t.Fatal("finished generation staged again", err)
				}
				stagePinsPG(t, s, ctx, j, p, false)
			})
		}
	}
}

func TestPGDocStageCrashReclaimRetiresOldGeneration(t *testing.T) {
	s, ctx := chunkReusePG(t)
	j, doc, _ := preparedJobFixturePG(t, s, ctx)
	p := stagePublicationPG(t, s, ctx, doc)
	if err := p.stageReadBlocks(ctx, j); err != nil {
		t.Fatal(err)
	}
	stagePinsPG(t, s, ctx, j, p, true)
	stageDerivativesPG(t, s, ctx, p, true)
	stageInvisiblePG(t, s, ctx, j)
	// Model process loss after the stage commit: no FinishDocJob or wall-clock
	// sleep. The normal claim API reclaims at a time beyond the saved lease.
	reclaimed, err := s.ClaimDocJob(ctx, j.RepoID, j.LeaseUntil.Add(time.Second), time.Minute)
	if err != nil || reclaimed.ID != j.ID || reclaimed.Version != j.Version+1 {
		t.Fatalf("reclaim: %+v %v", reclaimed, err)
	}
	stagePinsPG(t, s, ctx, j, p, false)
	stageDerivativesPG(t, s, ctx, p, false)
	for _, operation := range []struct {
		name string
		run  func() error
	}{
		{"stage", func() error { return p.stageReadBlocks(ctx, j) }},
		{"complete", func() error { return p.Complete(ctx, j, time.Now().UTC()) }},
	} {
		if err := operation.run(); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("stale %s: %v", operation.name, err)
		}
		stagePinsPG(t, s, ctx, j, p, false)
		stageDerivativesPG(t, s, ctx, p, false)
		stageInvisiblePG(t, s, ctx, reclaimed)
	}
	// A fresh worker can rebuild collected derivatives and publish normally.
	next := stagePublicationPG(t, s, ctx, doc)
	if err := next.Complete(ctx, reclaimed, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	stageCompletedPG(t, s, ctx, reclaimed, next)
}

func sharedRepositoryStagesPG(t *testing.T, s *PostgresStore, ctx context.Context) ([2]domain.DocFinalizationJob, [2]pgDocPublication) {
	t.Helper()
	marker := string(domain.HashContent([]byte(t.Name() + time.Now().String())))
	var jobs [2]domain.DocFinalizationJob
	var publications [2]pgDocPublication
	for i := range jobs {
		repo := domain.HashContent([]byte(fmt.Sprintf("%s repo %d", marker, i)))
		if _, err := s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
			t.Fatal(err)
		}
		doc := readBlockDoc(t, marker, fmt.Sprintf("owner %d tail", i), 257)
		jobs[i], publications[i] = stageJobForDocPG(t, s, ctx, repo, doc)
		if err := publications[i].stageReadBlocks(ctx, jobs[i]); err != nil {
			t.Fatal(err)
		}
		stagePinsPG(t, s, ctx, jobs[i], publications[i], true)
		stageInvisiblePG(t, s, ctx, jobs[i])
	}
	if jobs[0].RepoID == jobs[1].RepoID || jobs[0].DocHash == jobs[1].DocHash || publications[0].doc.read.blocks[0].Hash != publications[1].doc.read.blocks[0].Hash {
		t.Fatal("fixture must use distinct repositories/documents sharing blocks")
	}
	return jobs, publications
}

func TestPGDocStageConcurrentPinRetirementAfterLocationsPublished(t *testing.T) {
	s, ctx := chunkReusePG(t)
	jobs, publications := sharedRepositoryStagesPG(t, s, ctx)
	completeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	ready, release, done := make(chan struct{}, 2), make(chan struct{}), make(chan error, 2)
	released, joined := false, 0
	unblock := func() {
		if !released {
			close(release)
			released = true
		}
	}
	defer func() {
		unblock()
		cancel()
		deadline := time.NewTimer(6 * time.Second)
		defer deadline.Stop()
		for joined < 2 {
			select {
			case err := <-done:
				joined++
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("pin-retirement cleanup: %v", err)
				}
			case <-deadline.C:
				t.Error("pin-retirement transactions did not exit after release")
				return
			}
		}
	}()
	for i := range jobs {
		go func() {
			j, p := jobs[i], publications[i]
			done <- s.WithinRepository(completeCtx, j.RepoID, func(bound context.Context) error {
				// Follow Complete's final transaction, retaining its job-row fence.
				if _, err := s.db(bound).Exec(bound, `SELECT id FROM doc_finalization_jobs WHERE repo_id=$1 AND id=$2 FOR UPDATE`, j.RepoID, j.ID); err != nil {
					return err
				}
				old, err := s.GetDocJob(bound, j.RepoID, j.ID)
				if err != nil {
					return err
				}
				if !old.Fences(j, time.Now().UTC()) {
					return domain.ErrConflict
				}
				if _, err := s.putPreparedDoc(bound, j.RepoID, p.doc); err != nil {
					return err
				}
				// Both transactions now own KEY SHARE locks on common blocks and
				// have inserted their real document locations. Unconditional pin
				// pruning upgrades both locks to FOR UPDATE and deadlocks here.
				ready <- struct{}{}
				select {
				case <-release:
				case <-completeCtx.Done():
					return completeCtx.Err()
				}
				old.State, old.Reason = "completed", ""
				old.UpdatedAt, old.LeaseUntil = time.Now().UTC(), time.Time{}
				return s.writeDocJob(bound, old)
			})
		}()
	}
	barrier := time.NewTimer(3 * time.Second)
	defer barrier.Stop()
	for arrived := 0; arrived < 2; {
		select {
		case <-ready:
			arrived++
		case err := <-done:
			joined++
			t.Fatalf("publication exited before both locations were ready: %v", err)
		case <-barrier.C:
			t.Fatal("both publications did not reach the post-location barrier")
		}
	}
	unblock()
	for joined < 2 {
		select {
		case err := <-done:
			joined++
			if err != nil {
				t.Errorf("concurrent pin retirement with published locations: %v", err)
			}
		case <-completeCtx.Done():
			t.Fatal("pin retirement blocked after barrier release", completeCtx.Err())
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	for i := range jobs {
		stageCompletedPG(t, s, ctx, jobs[i], publications[i])
	}
	if err := s.DeleteDoc(ctx, jobs[0].RepoID, jobs[0].DocHash); err != nil {
		t.Fatal(err)
	}
	stageCompletedPG(t, s, ctx, jobs[1], publications[1])
	if err := s.DeleteDoc(ctx, jobs[1].RepoID, jobs[1].DocHash); err != nil {
		t.Fatal(err)
	}
	for _, p := range publications {
		stageDerivativesPG(t, s, ctx, p, false)
	}
}

func TestPGDocStageConcurrentCompletionsAcrossRepositories(t *testing.T) {
	// Both workers begin with committed pins, then Complete stages again before
	// publication. Repeat the shared-block lock-upgrade race with fresh identities.
	for round := 0; round < 8; round++ {
		t.Run(fmt.Sprint(round), func(t *testing.T) {
			s, ctx := chunkReusePG(t)
			jobs, publications := sharedRepositoryStagesPG(t, s, ctx)
			completeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			start, done := make(chan struct{}), make(chan error, 2)
			joined := 0
			defer func() {
				cancel()
				deadline := time.NewTimer(6 * time.Second)
				defer deadline.Stop()
				for joined < 2 {
					select {
					case err := <-done:
						joined++
						if err != nil && !errors.Is(err, context.Canceled) {
							t.Errorf("concurrent completion cleanup: %v", err)
						}
					case <-deadline.C:
						t.Error("concurrent completions did not exit after cancellation")
						return
					}
				}
			}()
			for i := range jobs {
				go func() {
					<-start
					done <- publications[i].Complete(completeCtx, jobs[i], time.Now().UTC())
				}()
			}
			close(start) // Post-stage barrier; neither repo serializes the other.
			for joined < 2 {
				select {
				case err := <-done:
					joined++
					if err != nil {
						t.Errorf("concurrent shared-block completion: %v", err)
					}
				case <-completeCtx.Done():
					t.Fatal("concurrent completions did not finish", completeCtx.Err())
				}
			}
			if t.Failed() {
				t.FailNow()
			}
			for i := range jobs {
				stageCompletedPG(t, s, ctx, jobs[i], publications[i])
			}
			if err := s.DeleteDoc(ctx, jobs[0].RepoID, jobs[0].DocHash); err != nil {
				t.Fatal(err)
			}
			stageCompletedPG(t, s, ctx, jobs[1], publications[1])
			if err := s.DeleteDoc(ctx, jobs[1].RepoID, jobs[1].DocHash); err != nil {
				t.Fatal(err)
			}
			for _, p := range publications {
				stageDerivativesPG(t, s, ctx, p, false)
			}
		})
	}
}

func TestPGDocStageRejectsIncompatibleBoundRepository(t *testing.T) {
	s, ctx := chunkReusePG(t)
	j, doc, _ := preparedJobFixturePG(t, s, ctx)
	p := stagePublicationPG(t, s, ctx, doc)
	other := domain.HashContent([]byte("other bound repo " + string(j.RepoID)))
	if _, err := s.PutRepo(ctx, domain.Repo{ID: other}); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"stage", func(bound context.Context) error { return p.stageReadBlocks(bound, j) }},
		{"complete", func(bound context.Context) error { return p.Complete(bound, j, time.Now().UTC()) }},
	} {
		// Commit the enclosing transaction after checking rejection, so accidental
		// staging cannot be hidden by the outer transaction's rollback.
		err := s.WithinRepository(ctx, other, func(bound context.Context) error {
			if err := operation.run(bound); !errors.Is(err, domain.ErrConflict) {
				return fmt.Errorf("incompatible bound repository %s: got %v, want conflict", operation.name, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		stageInvisiblePG(t, s, ctx, j)
		stagePinsPG(t, s, ctx, j, p, false)
		stageDerivativesPG(t, s, ctx, p, false)
		stageCountPG(t, s, ctx, 0, `SELECT count(*) FROM doc_read_block_preparations_v3 WHERE repo_id=$1`, other)
	}
}

func TestPGDocStageCompatibleBoundRepositorySkipsStageAndContainsRollback(t *testing.T) {
	s, ctx := chunkReusePG(t)
	j, doc, _ := preparedJobFixturePG(t, s, ctx)
	p := stagePublicationPG(t, s, ctx, doc)
	boundCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var ownershipBefore int
	if err := s.pool.QueryRow(boundCtx, `SELECT count(*) FROM repo_blobs WHERE repo_id=$1`, j.RepoID).Scan(&ownershipBefore); err != nil {
		t.Fatal(err)
	}
	wrongDoc := stagePublicationPG(t, s, boundCtx, readBlockDoc(t, string(j.RepoID), "different document", 1))
	if err := s.WithinRepository(boundCtx, j.RepoID, func(bound context.Context) error {
		// Compatible nesting must still validate the job and verified document
		// before skipping staging to preserve the normal doc -> blocks lock order.
		invalid := j
		invalid.ID += "-invalid"
		if err := p.stageReadBlocks(bound, invalid); !errors.Is(err, domain.ErrValidation) {
			return fmt.Errorf("nested stage job validation: got %v, want validation error", err)
		}
		if err := wrongDoc.stageReadBlocks(bound, j); !errors.Is(err, domain.ErrIntegrity) {
			return fmt.Errorf("nested stage document validation: got %v, want integrity error", err)
		}
		if err := p.stageReadBlocks(bound, j); err != nil {
			return err
		}
		// Query through the bound transaction: a pool read alone would miss
		// derivatives created inside an unreleased nested savepoint.
		stagePinsPG(t, s, bound, j, p, false)
		stageDerivativesPG(t, s, bound, p, false)
		stageCountPG(t, s, bound, ownershipBefore, `SELECT count(*) FROM repo_blobs WHERE repo_id=$1`, j.RepoID)
		stageCountPG(t, s, bound, 0, `SELECT count(*) FROM doc_read_index_current WHERE hash=$1`, j.DocHash)
		stageCountPG(t, s, bound, 0, `SELECT count(*) FROM doc_read_block_locations_v3 WHERE doc_hash=$1`, j.DocHash)
		return nil // Commit, so skipped staging cannot be masked by rollback.
	}); err != nil {
		t.Fatal(err)
	}
	stageInvisiblePG(t, s, boundCtx, j)
	stagePinsPG(t, s, boundCtx, j, p, false)
	stageDerivativesPG(t, s, boundCtx, p, false)

	stop := errors.New("roll back compatible nested publication")
	err := s.WithinRepository(boundCtx, j.RepoID, func(bound context.Context) error {
		if err := p.Complete(bound, j, time.Now().UTC()); err != nil {
			return err
		}
		// Prove publication succeeded locally before forcing the outer rollback.
		stageCompletedPG(t, s, bound, j, p)
		stageCountPG(t, s, bound, 1, `SELECT count(*) FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, j.RepoID, j.DocHash)
		// An independent reader still sees the original running receipt and no
		// document grant while the enclosing transaction is uncommitted.
		stageInvisiblePG(t, s, boundCtx, j)
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("outer publication rollback: %v", err)
	}
	stageInvisiblePG(t, s, boundCtx, j)
	stagePinsPG(t, s, boundCtx, j, p, false)
	stageDerivativesPG(t, s, boundCtx, p, false)
	stageCountPG(t, s, boundCtx, ownershipBefore, `SELECT count(*) FROM repo_blobs WHERE repo_id=$1`, j.RepoID)
	stageCountPG(t, s, boundCtx, 0, `SELECT count(*) FROM blobs WHERE hash=$1`, j.DocHash)
	got, err := s.GetDocJob(boundCtx, j.RepoID, j.ID)
	if err != nil || got.State != j.State || got.Version != j.Version || got.Attempts != j.Attempts || !got.LeaseUntil.Equal(j.LeaseUntil) || !got.UpdatedAt.Equal(j.UpdatedAt) {
		t.Fatalf("job receipt escaped outer rollback: %+v %v", got, err)
	}
}
