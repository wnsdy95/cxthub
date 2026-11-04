//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (fixture docDisplayPGFixture) discoveryStep(test *testing.T, job domain.DocDisplayJob, prior *domain.DocFieldDiscoveryState, length int) domain.DocFieldDiscoveryStep {
	test.Helper()
	var state domain.DocFieldDiscoveryState
	if prior == nil {
		var err error
		state, err = domain.NewDocFieldDiscoveryState(job.Target.Event.Hash, job.Target.Event.Length)
		if err != nil {
			test.Fatal(err)
		}
	} else {
		state = *prior
	}
	length = min(length, state.EventBytes-state.Next)
	from := job.Target.Event.Offset + state.Next
	step, err := domain.AdvanceDocFieldDiscovery(fixture.ctx, state, fixture.stream[from:from+length])
	if err != nil {
		test.Fatal(err)
	}
	return step
}

func (fixture docDisplayPGFixture) discover(test *testing.T, job domain.DocDisplayJob, prepared domain.DocDisplayPreparation) domain.DocDisplayPreparation {
	test.Helper()
	for prepared.Checkpoint == "" {
		step := fixture.discoveryStep(test, job, prepared.Discovery, domain.DocFieldDiscoveryStepBytes)
		var err error
		prepared, err = fixture.storage.SaveDocDisplayDiscovery(fixture.ctx, job, step)
		if err != nil {
			test.Fatal(err)
		}
	}
	return prepared
}

func (fixture docDisplayPGFixture) cacheSource(test *testing.T, index int) domain.DocDisplayStringSource {
	test.Helper()
	source := fixture.source(test, index)
	raw, err := source.Encode()
	if err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `INSERT INTO doc_display_sources_v1(event_hash,profile,source_hash,bytes)
 VALUES($1,$2,$3,$4)`, source.EventHash, source.Profile, domain.HashContent(raw), raw); err != nil {
		test.Fatal(err)
	}
	return source
}

func TestDocDisplayPGDiscoveryCheckpointCASRestart(test *testing.T) {
	fixture := docDisplayPG(test)
	fixture.enqueue(test, 0)
	job := fixture.claim(test)
	prepared, err := fixture.storage.LoadDocDisplayJob(fixture.ctx, job)
	if err != nil || !reflect.DeepEqual(prepared, domain.DocDisplayPreparation{}) {
		test.Fatal("cold preparation is not empty", prepared, err)
	}
	step := fixture.discoveryStep(test, job, nil, 64)
	hash, raw := step.Checkpoint()
	prepared, err = fixture.storage.SaveDocDisplayDiscovery(fixture.ctx, job, step)
	if err != nil || prepared.Discovery == nil || prepared.Discovery.Next != 64 || prepared.DiscoveryCheckpoint != hash || prepared.Source != nil || prepared.Checkpoint != "" {
		test.Fatal("partial discovery not saved", prepared, err)
	}
	var storedHash domain.ContentHash
	var stored []byte
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT discovery_hash,discovery FROM doc_display_jobs_v1 WHERE repo_id=$1`, fixture.repo).Scan(&storedHash, &stored); err != nil || storedHash != hash || !bytes.Equal(stored, raw) {
		test.Fatal("checkpoint bytes changed", err)
	}
	before := fixture.image(test)
	if _, err := fixture.storage.SaveDocDisplayDiscovery(fixture.ctx, job, step); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("stale discovery checkpoint committed", err)
	}
	ahead := fixture.discoveryStep(test, job, prepared.Discovery, 64)
	aheadState, err := ahead.State()
	if err != nil {
		test.Fatal(err)
	}
	skipped := fixture.discoveryStep(test, job, &aheadState, 64)
	if _, err := fixture.storage.SaveDocDisplayDiscovery(fixture.ctx, job, skipped); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("checkpoint offset skipped trusted state", err)
	}
	forged := *prepared.Discovery
	forged.OutputNonSpace = !forged.OutputNonSpace
	foreignPrevious := fixture.discoveryStep(test, job, &forged, 64)
	if _, err := fixture.storage.SaveDocDisplayDiscovery(fixture.ctx, job, foreignPrevious); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("foreign previous checkpoint committed", err)
	}
	if fixture.image(test) != before {
		test.Fatal("failed discovery CAS changed durable state")
	}
	restarted, err := NewPostgresStore(fixture.ctx, os.Getenv("CXT_TEST_DSN"))
	if err != nil {
		test.Fatal(err)
	}
	defer restarted.Close()
	fixture.storage = restarted
	loaded, err := restarted.LoadDocDisplayJob(fixture.ctx, job)
	if err != nil || !reflect.DeepEqual(loaded, prepared) {
		test.Fatal("restart lost discovery checkpoint", loaded, err)
	}
	render := fixture.discover(test, job, loaded)
	if render.Discovery != nil || render.DiscoveryCheckpoint != "" || render.Source == nil || *render.Source != fixture.source(test, 0) {
		test.Fatal("final discovery did not initialize the existing source format", render)
	}
	for !render.State.Done {
		render, err = restarted.SaveDocDisplayStep(fixture.ctx, job, render.Checkpoint, fixture.step(test, render))
		if err != nil {
			test.Fatal(err)
		}
	}
	if err := restarted.CompleteDocDisplay(fixture.ctx, job, render.Checkpoint, time.Now()); err != nil {
		test.Fatal(err)
	}
	fixture.read(test, job.Target, nil)
}

func TestDocDisplayPGDiscoveryConcurrentCAS(test *testing.T) {
	fixture := docDisplayPG(test)
	fixture.enqueue(test, 0)
	job := fixture.claim(test)
	step := fixture.discoveryStep(test, job, nil, 64)
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			_, err := fixture.storage.SaveDocDisplayDiscovery(fixture.ctx, job, step)
			results <- err
		})
	}
	workers.Wait()
	close(results)
	succeeded, conflicted := 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, domain.ErrConflict):
			conflicted++
		default:
			test.Fatal(err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		test.Fatal("discovery CAS did not serialize", succeeded, conflicted)
	}
}

func TestDocDisplayPGDiscoveryRetryLeaseAndBindingFences(test *testing.T) {
	for _, mutation := range []string{"retry", "lease", "source_version", "source_id", "owner"} {
		test.Run(mutation, func(test *testing.T) {
			fixture := docDisplayPG(test)
			fixture.enqueue(test, 0)
			job := fixture.claim(test)
			prepared, err := fixture.storage.SaveDocDisplayDiscovery(fixture.ctx, job, fixture.discoveryStep(test, job, nil, 64))
			if err != nil {
				test.Fatal(err)
			}
			next := fixture.discoveryStep(test, job, prepared.Discovery, 64)
			stale := job
			switch mutation {
			case "retry":
				job.State, job.NextAttempt = "retrying", time.Now().Add(-time.Second)
				if err := fixture.storage.FinishDocDisplay(fixture.ctx, job, time.Now()); err != nil {
					test.Fatal(err)
				}
			case "lease":
				_, err = docDisplayLeaseMutationPG(fixture.ctx, fixture.storage, fixture.repo, `UPDATE doc_display_jobs_v1 SET lease_until=clock_timestamp()-interval '1 second' WHERE repo_id=$1`, fixture.repo)
			case "source_version":
				_, err = fixture.storage.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.doc.hash)
			case "source_id":
				_, err = fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM doc_range_generations_v2 WHERE doc_hash=$1`, fixture.doc.hash)
			case "owner":
				_, err = fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, fixture.repo, fixture.doc.hash)
			}
			if err != nil {
				test.Fatal(err)
			}
			before := fixture.image(test)
			if _, err := fixture.storage.SaveDocDisplayDiscovery(fixture.ctx, stale, next); !errors.Is(err, domain.ErrConflict) {
				test.Fatal("stale discovery worker committed", err)
			}
			if _, err := fixture.storage.LoadDocDisplayJob(fixture.ctx, stale); !errors.Is(err, domain.ErrConflict) {
				test.Fatal("stale discovery worker loaded", err)
			}
			if fixture.image(test) != before {
				test.Fatal("failed fenced write changed discovery")
			}
			if mutation == "owner" {
				return
			}
			if mutation == "source_version" || mutation == "source_id" {
				if err := fixture.storage.WithinRepository(fixture.ctx, fixture.repo, func(bound context.Context) error {
					return fixture.storage.ensureRangeGeneration(bound, fixture.repo, fixture.doc.hash)
				}); err != nil {
					test.Fatal(err)
				}
				fixture.refreshTargets(test)
				fixture.enqueue(test, 0)
			}
			job = fixture.claim(test)
			loaded, err := fixture.storage.LoadDocDisplayJob(fixture.ctx, job)
			if err != nil {
				test.Fatal(err)
			}
			if mutation == "retry" || mutation == "lease" {
				if !reflect.DeepEqual(loaded, prepared) {
					test.Fatal("retry or expired lease lost discovery", loaded)
				}
				if _, err := fixture.storage.SaveDocDisplayDiscovery(fixture.ctx, job, next); err != nil {
					test.Fatal("current lease could not resume discovery", err)
				}
			} else if !reflect.DeepEqual(loaded, domain.DocDisplayPreparation{}) {
				test.Fatal("new source binding retained discovery", loaded)
			}
		})
	}
}

func TestDocDisplayPGDiscoveryCorruptionFailsClosedWithCache(test *testing.T) {
	for _, mutation := range []string{"hash", "bytes", "event_hash", "event_length", "zero", "done", "cache_identity"} {
		test.Run(mutation, func(test *testing.T) {
			fixture := docDisplayPG(test)
			fixture.enqueue(test, 0)
			job := fixture.claim(test)
			prepared, err := fixture.storage.SaveDocDisplayDiscovery(fixture.ctx, job, fixture.discoveryStep(test, job, nil, 64))
			if err != nil {
				test.Fatal(err)
			}
			next := fixture.discoveryStep(test, job, prepared.Discovery, 64)
			source := fixture.cacheSource(test, 0)
			state := *prepared.Discovery
			switch mutation {
			case "hash":
				_, err = docDisplayJobCorruptionPG(fixture.ctx, fixture.storage, `UPDATE doc_display_jobs_v1 SET discovery_hash=$2 WHERE repo_id=$1`, fixture.repo, domain.HashContent([]byte("wrong")))
			case "bytes":
				_, err = docDisplayJobCorruptionPG(fixture.ctx, fixture.storage, `UPDATE doc_display_jobs_v1 SET discovery=$2 WHERE repo_id=$1`, fixture.repo, []byte(`{}`))
			case "cache_identity":
				source.EventBytes++
				raw, encodeErr := source.Encode()
				if encodeErr != nil {
					test.Fatal(encodeErr)
				}
				transaction, beginErr := fixture.storage.pool.Begin(fixture.ctx)
				if beginErr != nil {
					test.Fatal(beginErr)
				}
				defer rollbackPG(transaction)
				if _, err = transaction.Exec(fixture.ctx, `DELETE FROM doc_display_sources_v1 WHERE event_hash=$1 AND profile=$2`, source.EventHash, source.Profile); err == nil {
					_, err = transaction.Exec(fixture.ctx, `INSERT INTO doc_display_sources_v1(event_hash,profile,source_hash,bytes)
 VALUES($1,$2,$3,$4)`, source.EventHash, source.Profile, domain.HashContent(raw), raw)
				}
				if err == nil {
					err = transaction.Commit(fixture.ctx)
				}
			default:
				switch mutation {
				case "event_hash":
					state.EventHash = fixture.targets[1].Event.Hash
				case "event_length":
					state.EventBytes++
				case "zero":
					state, err = domain.NewDocFieldDiscoveryState(job.Target.Event.Hash, job.Target.Event.Length)
				case "done":
					final := fixture.discoveryStep(test, job, prepared.Discovery, domain.DocFieldDiscoveryStepBytes)
					state, err = final.State()
				}
				if err != nil {
					test.Fatal(err)
				}
				raw, encodeErr := state.Encode()
				if encodeErr != nil {
					test.Fatal(encodeErr)
				}
				_, err = docDisplayJobCorruptionPG(fixture.ctx, fixture.storage, `UPDATE doc_display_jobs_v1 SET discovery_hash=$2,discovery=$3 WHERE repo_id=$1`, fixture.repo, domain.HashContent(raw), raw)
			}
			if err != nil {
				test.Fatal(err)
			}
			before := fixture.image(test)
			if _, err := fixture.storage.LoadDocDisplayJob(fixture.ctx, job); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("cache bypassed corrupt discovery", err)
			}
			if _, err := fixture.storage.SaveDocDisplayDiscovery(fixture.ctx, job, next); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("write healed corrupt discovery", err)
			}
			if _, err := fixture.storage.InitializeDocDisplay(fixture.ctx, job, fixture.source(test, 0)); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("cached initialization healed corrupt discovery", err)
			}
			if fixture.image(test) != before {
				test.Fatal("corruption failure changed durable state")
			}
		})
	}
}

func TestDocDisplayPGDiscoveryFinalAtomicity(test *testing.T) {
	for _, failure := range []string{"lease_expiry", "render_failure", "source_collision"} {
		test.Run(failure, func(test *testing.T) {
			fixture := docDisplayPG(test)
			fixture.enqueue(test, 0)
			job := fixture.claim(test)
			prepared, err := fixture.storage.SaveDocDisplayDiscovery(fixture.ctx, job, fixture.discoveryStep(test, job, nil, 64))
			if err != nil {
				test.Fatal(err)
			}
			final := fixture.discoveryStep(test, job, prepared.Discovery, domain.DocFieldDiscoveryStepBytes)
			want := error(domain.ErrIntegrity)
			switch failure {
			case "lease_expiry":
				docDisplayPGDelay(test, fixture, "doc_display_sources_v1", "event_hash", job.Target.Event.Hash)
				if _, err := docDisplayLeaseMutationPG(fixture.ctx, fixture.storage, fixture.repo, `UPDATE doc_display_jobs_v1 SET lease_until=clock_timestamp()+interval '80 milliseconds' WHERE repo_id=$1`, fixture.repo); err != nil {
					test.Fatal(err)
				}
				want = domain.ErrConflict
			case "render_failure":
				transaction, err := fixture.storage.pool.Begin(fixture.ctx)
				if err != nil {
					test.Fatal(err)
				}
				defer rollbackPG(transaction)
				name := pgx.Identifier{domain.NewID("discovery_fail_")}.Sanitize()
				statement := `CREATE FUNCTION ` + name + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF NEW.repo_id='` + string(fixture.repo) + `' AND NEW.source_hash IS NOT NULL THEN
 RAISE EXCEPTION 'injected render initialization failure' USING ERRCODE='23514'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER ` + name + ` BEFORE UPDATE ON doc_display_jobs_v1 FOR EACH ROW EXECUTE FUNCTION ` + name + `()`
				if _, err := transaction.Exec(fixture.ctx, statement); err != nil {
					test.Fatal(err)
				}
				if err := transaction.Commit(fixture.ctx); err != nil {
					test.Fatal(err)
				}
				test.Cleanup(func() {
					if _, err := fixture.storage.pool.Exec(context.Background(), `DROP TRIGGER `+name+` ON doc_display_jobs_v1; DROP FUNCTION `+name+`() `); err != nil {
						test.Error(err)
					}
				})
				want = nil
			case "source_collision":
				source := fixture.source(test, 0)
				source.Empty = !source.Empty
				raw, err := source.Encode()
				if err != nil {
					test.Fatal(err)
				}
				if _, err := fixture.storage.pool.Exec(fixture.ctx, `INSERT INTO doc_display_sources_v1(event_hash,profile,source_hash,bytes) VALUES($1,$2,$3,$4)`, source.EventHash, source.Profile, domain.HashContent(raw), raw); err != nil {
					test.Fatal(err)
				}
			}
			before := fixture.image(test)
			_, err = fixture.storage.SaveDocDisplayDiscovery(fixture.ctx, job, final)
			if want == nil {
				var constraint *pgconn.PgError
				if !errors.As(err, &constraint) || constraint.Code != "23514" {
					test.Fatal("render initialization did not fail", err)
				}
			} else if !errors.Is(err, want) {
				test.Fatal("final discovery failure", err)
			}
			if fixture.image(test) != before {
				test.Fatal("final discovery leaked source insertion, checkpoint, or discovery removal")
			}
		})
	}
}

func TestDocDisplayPGDiscoveryZeroStepForeignProofAndBounds(test *testing.T) {
	fixture := docDisplayPG(test,
		domain.CIREvent{Kind: domain.EventToolResult, Seq: 1, CallID: test.Name(), Output: strings.Repeat("x", domain.DocFieldDiscoveryStepBytes*3)},
		domain.CIREvent{Kind: domain.EventToolResult, Seq: 2, CallID: test.Name(), Output: "short"})
	fixture.enqueue(test, 0)
	job := fixture.claim(test)
	before := fixture.image(test)
	if _, err := fixture.storage.SaveDocDisplayDiscovery(fixture.ctx, job, domain.DocFieldDiscoveryStep{}); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("unsealed zero step accepted", err)
	}
	other := job
	other.Target = fixture.targets[1]
	if _, err := fixture.storage.SaveDocDisplayDiscovery(fixture.ctx, job, fixture.discoveryStep(test, other, nil, 64)); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("foreign event proof accepted", err)
	}
	step := fixture.discoveryStep(test, job, nil, domain.DocFieldDiscoveryStepBytes)
	foreign, _ := rootPGRepo(test, fixture.ctx, fixture.storage)
	other = job
	other.RepoID, other.Target.RepoID = foreign, foreign
	if _, err := fixture.storage.SaveDocDisplayDiscovery(fixture.ctx, other, step); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("foreign ownership proof accepted", err)
	}
	if fixture.image(test) != before {
		test.Fatal("invalid proof changed durable state")
	}
	initial, err := domain.NewDocFieldDiscoveryState(job.Target.Event.Hash, job.Target.Event.Length)
	if err != nil {
		test.Fatal(err)
	}
	from := job.Target.Event.Offset
	for _, size := range []int{0, 63, domain.DocFieldDiscoveryStepBytes + 1} {
		if _, err := domain.AdvanceDocFieldDiscovery(fixture.ctx, initial, fixture.stream[from:from+size]); !errors.Is(err, domain.ErrIntegrity) {
			test.Fatal("invalid step bound accepted", size, err)
		}
	}
	prepared, err := fixture.storage.SaveDocDisplayDiscovery(fixture.ctx, job, step)
	if err != nil || prepared.Discovery == nil || prepared.Discovery.Next != domain.DocFieldDiscoveryStepBytes {
		test.Fatal("64 KiB step rejected", err)
	}
	for _, statement := range []string{
		`UPDATE doc_display_jobs_v1 SET discovery=decode(repeat('00',65537),'hex') WHERE repo_id=$1`,
		`UPDATE doc_display_jobs_v1 SET discovery_hash=NULL WHERE repo_id=$1`,
		`UPDATE doc_display_jobs_v1 SET discovery=NULL WHERE repo_id=$1`,
		`UPDATE doc_display_jobs_v1 SET worker_protocol=1 WHERE repo_id=$1`,
	} {
		_, err := docDisplayJobCorruptionPG(fixture.ctx, fixture.storage, statement, fixture.repo)
		var constraint *pgconn.PgError
		if !errors.As(err, &constraint) || constraint.Code != "23514" {
			test.Fatal("discovery schema accepted invalid storage", statement, err)
		}
	}
	fixture.discover(test, job, prepared)
}

func TestDocDisplayPGDiscoveryMixedPhaseFailsIntegrity(test *testing.T) {
	fixture := docDisplayPG(test)
	fixture.enqueue(test, 0)
	job := fixture.claim(test)
	step := fixture.discoveryStep(test, job, nil, 64)
	hash, raw := step.Checkpoint()
	prepared := fixture.initialize(test, job)
	_, err := docDisplayJobCorruptionPG(fixture.ctx, fixture.storage, `UPDATE doc_display_jobs_v1 SET discovery_hash=$2,discovery=$3 WHERE repo_id=$1`, fixture.repo, hash, raw)
	var constraint *pgconn.PgError
	if !errors.As(err, &constraint) || constraint.Code != "23514" {
		test.Fatal("mixed phase accepted by schema", err)
	}
	transaction, err := fixture.storage.pool.Begin(fixture.ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(fixture.ctx, `ALTER TABLE doc_display_jobs_v1 DROP CONSTRAINT doc_display_discovery_pair, DROP CONSTRAINT doc_display_discovery_phase;
 ALTER TABLE doc_display_jobs_v1 DISABLE TRIGGER guard_doc_display_zz_catalog_write`); err != nil {
		test.Fatal(err)
	}
	bound := context.WithValue(fixture.ctx, repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: fixture.storage, repo: fixture.repo})
	for _, values := range []struct {
		hash any
		raw  any
	}{{hash, raw}, {hash, nil}, {nil, raw}} {
		if _, err := transaction.Exec(bound, `UPDATE doc_display_jobs_v1 SET discovery_hash=$2,discovery=$3 WHERE repo_id=$1`, fixture.repo, values.hash, values.raw); err != nil {
			test.Fatal(err)
		}
		if _, err := fixture.storage.readDocDisplayPreparation(bound, job); !errors.Is(err, domain.ErrIntegrity) {
			test.Fatal("mixed or torn discovery trusted", prepared, err)
		}
	}
}

func TestDocDisplayPGDiscoveryKnownSourceReuse(test *testing.T) {
	fixture := docDisplayPG(test)
	fixture.enqueue(test, 0)
	job := fixture.claim(test)
	partial, err := fixture.storage.SaveDocDisplayDiscovery(fixture.ctx, job, fixture.discoveryStep(test, job, nil, 64))
	if err != nil {
		test.Fatal(err)
	}
	source := fixture.cacheSource(test, 0)
	loaded, err := fixture.storage.LoadDocDisplayJob(fixture.ctx, job)
	if err != nil || loaded.Source == nil || *loaded.Source != source || !reflect.DeepEqual(loaded.Discovery, partial.Discovery) {
		test.Fatal("valid cache and partial discovery not loadable", loaded, err)
	}
	prepared, err := fixture.storage.InitializeDocDisplay(fixture.ctx, job, *loaded.Source)
	if err != nil || prepared.Discovery != nil || prepared.DiscoveryCheckpoint != "" || prepared.Checkpoint == "" {
		test.Fatal("cache reuse did not replace discovery atomically", prepared, err)
	}
	foreign, _ := rootPGRepo(test, fixture.ctx, fixture.storage)
	seedRootPG(test, fixture.ctx, fixture.storage, foreign, fixture.doc)
	if err := fixture.storage.PutSnapshot(fixture.ctx, domain.Snapshot{ID: fixture.doc.hash, RepoID: foreign, DocHash: fixture.doc.hash,
		DocIdentity: domain.DocumentIdentityRootV1, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if _, err := fixture.storage.pool.Exec(context.Background(), `DELETE FROM doc_display_jobs_v1 WHERE repo_id=$1`, foreign); err != nil {
			test.Error(err)
		}
	})
	target := job.Target
	target.RepoID = foreign
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now()); err != nil {
		test.Fatal(err)
	}
	other, err := fixture.storage.ClaimDocDisplay(fixture.ctx, time.Now(), time.Minute)
	if err != nil || other.RepoID != foreign {
		test.Fatal("cross-repository claim", other, err)
	}
	known, err := fixture.storage.LoadDocDisplayJob(fixture.ctx, other)
	if err != nil || known.Source == nil || *known.Source != source || known.Discovery != nil || known.Checkpoint != "" {
		test.Fatal("current owner did not reuse immutable source", known, err)
	}
	if _, err := fixture.storage.InitializeDocDisplay(fixture.ctx, other, *known.Source); err != nil {
		test.Fatal(err)
	}
}

func TestDocDisplayPGDiscoveryOldWorkerClaimFence(test *testing.T) {
	fixture := docDisplayPG(test)
	fixture.enqueue(test, 0)
	oldClaim := func() {
		test.Helper()
		before := fixture.image(test)
		_, err := fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_display_jobs_v1 SET state='running',version=version+1,
 attempts=attempts+1,lease_until=clock_timestamp()+interval '1 minute' WHERE repo_id=$1`, fixture.repo)
		var fence *pgconn.PgError
		if !errors.As(err, &fence) || fence.Code != "0A000" {
			test.Fatal("unqualified old worker stole display job", err)
		}
		if fixture.image(test) != before {
			test.Fatal("old worker claim changed durable state")
		}
	}
	oldClaim()
	job := fixture.claim(test)
	prepared, err := fixture.storage.SaveDocDisplayDiscovery(fixture.ctx, job, fixture.discoveryStep(test, job, nil, 64))
	if err != nil {
		test.Fatal(err)
	}
	if err := fixture.storage.RenewDocDisplay(fixture.ctx, job, time.Now(), time.Minute); err != nil {
		test.Fatal("protocol fence blocked renewal", err)
	}
	oldClaim()
	if _, err := docDisplayLeaseMutationPG(fixture.ctx, fixture.storage, fixture.repo, `UPDATE doc_display_jobs_v1 SET lease_until=clock_timestamp()-interval '1 second' WHERE repo_id=$1`, fixture.repo); err != nil {
		test.Fatal(err)
	}
	oldClaim()
	reclaimed := fixture.claim(test)
	if reclaimed.Version != job.Version+1 || reclaimed.Attempts != job.Attempts+1 {
		test.Fatal("old worker advanced the claim fence", reclaimed)
	}
	loaded, err := fixture.storage.LoadDocDisplayJob(fixture.ctx, reclaimed)
	if err != nil || !reflect.DeepEqual(loaded, prepared) {
		test.Fatal("protocol claim lost discovery", loaded, err)
	}
	reclaimed.State, reclaimed.NextAttempt = "waiting", time.Now().Add(-time.Second)
	if err := fixture.storage.FinishDocDisplay(fixture.ctx, reclaimed, time.Now()); err != nil {
		test.Fatal(err)
	}
	oldClaim()
}

func TestDocDisplayPGDiscoveryMigrationInvalidatesRunningAtomically(test *testing.T) {
	fixture := docDisplayPG(test)
	fixture.enqueue(test, 0)
	completed := fixture.claim(test)
	completedPreparation := fixture.done(test, completed)
	if err := fixture.storage.CompleteDocDisplay(fixture.ctx, completed, completedPreparation.Checkpoint, time.Now()); err != nil {
		test.Fatal(err)
	}
	fixture.enqueue(test, 1)
	job := fixture.claim(test)
	prepared := fixture.initialize(test, job)
	before := fixture.image(test)
	transaction, err := fixture.storage.pool.Begin(fixture.ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(fixture.ctx, `DROP TRIGGER guard_doc_display_protocol ON doc_display_jobs_v1;
 DROP TRIGGER doc_display_jobs_v1_queue_roots ON doc_display_jobs_v1;
 DROP TRIGGER guard_doc_display_z_catalog_claim ON doc_display_jobs_v1;
 DROP TRIGGER guard_doc_display_z_catalog_renew ON doc_display_jobs_v1;
 DROP TRIGGER guard_doc_display_zz_catalog_write ON doc_display_jobs_v1;
 DROP FUNCTION cxthub_guard_doc_display_protocol();
 ALTER TABLE doc_display_jobs_v1 DROP COLUMN discovery_hash,DROP COLUMN discovery,DROP COLUMN worker_protocol`); err != nil {
		test.Fatal(err)
	}
	raw, err := os.ReadFile("../../../../schemas/db/migrations/0127_doc_display_discovery.sql")
	if err != nil {
		test.Fatal(err)
	}
	if _, err := transaction.Exec(fixture.ctx, string(raw)); err != nil {
		test.Fatal(err)
	}
	var version int64
	var state string
	var lease *time.Time
	var source, checkpoint domain.ContentHash
	var checkpointRaw []byte
	var protocol int
	if err := transaction.QueryRow(fixture.ctx, `SELECT state,version,lease_until,source_hash,checkpoint_hash,checkpoint,worker_protocol
 FROM doc_display_jobs_v1 WHERE repo_id=$1 AND event_index=1`, fixture.repo).Scan(&state, &version, &lease, &source, &checkpoint, &checkpointRaw, &protocol); err != nil {
		test.Fatal(err)
	}
	if state != "waiting" || version != job.Version+1 || lease != nil || checkpoint != prepared.Checkpoint || source != prepared.State.Source || domain.HashContent(checkpointRaw) != prepared.Checkpoint || protocol != 2 {
		test.Fatal("migration did not fence running lease or preserved checkpoint", state, version, lease, protocol)
	}
	if err := transaction.QueryRow(fixture.ctx, `SELECT state,version,checkpoint_hash FROM doc_display_jobs_v1 WHERE repo_id=$1 AND event_index=0`, fixture.repo).Scan(&state, &version, &checkpoint); err != nil || state != "completed" || version != completed.Version || checkpoint != completedPreparation.Checkpoint {
		test.Fatal("migration changed completed receipt", err)
	}
	if err := transaction.Rollback(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	if fixture.image(test) != before {
		test.Fatal("rolled-back migration invalidated a lease or changed completed cache")
	}
}
