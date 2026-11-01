//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docDisplayPGFixture struct {
	storage *PostgresStore
	ctx     context.Context
	repo    domain.ContentHash
	doc     rootReaderFixture
	targets []domain.DocDisplayTarget
	stream  []byte
}

func docDisplayPG(t *testing.T, events ...domain.CIREvent) docDisplayPGFixture {
	t.Helper()
	if dsn := os.Getenv("CXT_TEST_DSN"); dsn != "" {
		config, err := pgx.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		if _, owned := docDisplayOwnedFixturesPG.Load(config.Database); !owned {
			isolated, _ := rootProjectionIsolatedPG(t)
			t.Setenv("CXT_TEST_DSN", isolated.pool.Config().ConnString())
		}
	}
	storage, ctx := chunkReusePG(t)
	repo, _ := rootPGRepo(t, ctx, storage)
	fixture := rootFixture(t, "")
	unique := t.Name() + time.Now().String()
	fixture.cir.Envelope.SessionOriginID = unique
	fixture.cir.Events = []domain.CIREvent{
		{Kind: domain.EventToolResult, Seq: 1, CallID: "display", Output: unique + strings.Repeat("x", domain.DocDisplayPageTextBytes+100)},
		{Kind: domain.EventToolResult, Seq: 2, CallID: "second", Output: unique},
		{Kind: domain.EventMessage, Seq: 3, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "not a tool result"}}},
	}
	if len(events) > 0 {
		fixture.cir.Events = events
	}
	var err error
	fixture.manifest, fixture.bodies, err = domain.ConversationManifestForCIR(fixture.cir)
	if err != nil {
		t.Fatal(err)
	}
	fixture.hash, err = domain.ConversationManifestHash(fixture.manifest)
	if err != nil {
		t.Fatal(err)
	}
	fixture.manifestBytes, err = domain.CanonicalConversationManifest(fixture.manifest)
	if err != nil {
		t.Fatal(err)
	}
	fixture.canonical, err = domain.CanonicalBytes(fixture.cir)
	if err != nil {
		t.Fatal(err)
	}
	seedRootPG(t, ctx, storage, repo, fixture)
	if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: fixture.hash, RepoID: repo, DocHash: fixture.hash,
		DocIdentity: domain.DocumentIdentityRootV1, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		t.Fatal(err)
	}
	if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
		t.Fatal(err)
	}
	result := docDisplayPGFixture{storage: storage, ctx: ctx, repo: repo, doc: fixture}
	for _, chunk := range fixture.manifest.Chunks {
		result.stream = append(result.stream, fixture.bodies[chunk.Hash]...)
	}
	result.refreshTargets(t)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := storage.pool.Exec(cleanup, `DELETE FROM doc_display_jobs_v1 WHERE repo_id=$1`, repo); err != nil {
			t.Error(err)
		}
	})
	return result
}

func (fixture *docDisplayPGFixture) refreshTargets(t *testing.T) {
	t.Helper()
	if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		generation, err := fixture.storage.OpenDocRangeGeneration(read, fixture.repo, fixture.doc.hash)
		if err != nil {
			return err
		}
		entries, err := generation.ReadPage(read, 0)
		if err != nil {
			return err
		}
		fixture.targets = nil
		for _, entry := range entries {
			fixture.targets = append(fixture.targets, domain.DocDisplayTarget{RepoID: fixture.repo, DocHash: fixture.doc.hash,
				Binding: generation.Binding(), Event: entry.DocEventIndex})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func (fixture docDisplayPGFixture) enqueue(t *testing.T, index int) domain.DocDisplayJob {
	t.Helper()
	job, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, fixture.targets[index], time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func (fixture docDisplayPGFixture) claim(t *testing.T) domain.DocDisplayJob {
	t.Helper()
	job, err := fixture.storage.ClaimDocDisplay(fixture.ctx, time.Now().UTC(), time.Minute)
	if err != nil || job.RepoID != fixture.repo {
		t.Fatalf("claim: %+v %v", job, err)
	}
	return job
}

func (fixture docDisplayPGFixture) source(t *testing.T, index int) domain.DocDisplayStringSource {
	t.Helper()
	location := fixture.targets[index].Event
	raw := fixture.stream[location.Offset : location.Offset+location.Length]
	source, err := domain.BuildDocDisplayStringSourceReader(fixture.ctx, location.Hash, location.Length, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func (fixture docDisplayPGFixture) initialize(t *testing.T, job domain.DocDisplayJob) domain.DocDisplayPreparation {
	t.Helper()
	prepared, err := fixture.storage.InitializeDocDisplay(fixture.ctx, job, fixture.source(t, job.Target.Event.Index))
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func (fixture docDisplayPGFixture) step(t *testing.T, prepared domain.DocDisplayPreparation) domain.DocDisplayStringStep {
	t.Helper()
	var location domain.DocEventIndex
	for _, target := range fixture.targets {
		if target.Event.Hash == prepared.Source.EventHash {
			location = target.Event
		}
	}
	step, err := domain.StepDocDisplayString(fixture.ctx, *prepared.Source, prepared.State, func(_ context.Context, offset, length int) ([]byte, error) {
		return fixture.stream[location.Offset+offset : location.Offset+offset+length], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return step
}

func (fixture docDisplayPGFixture) done(t *testing.T, job domain.DocDisplayJob) domain.DocDisplayPreparation {
	t.Helper()
	prepared := fixture.initialize(t, job)
	for !prepared.State.Done {
		step := fixture.step(t, prepared)
		var err error
		prepared, err = fixture.storage.SaveDocDisplayStep(fixture.ctx, job, prepared.Checkpoint, step)
		if err != nil {
			t.Fatal(err)
		}
	}
	return prepared
}

func (fixture docDisplayPGFixture) image(t *testing.T) string {
	t.Helper()
	var image string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT jsonb_build_object(
 'jobs',(SELECT jsonb_agg(to_jsonb(job) ORDER BY event_index) FROM doc_display_jobs_v1 job WHERE repo_id=$1),
 'sources',(SELECT jsonb_agg(to_jsonb(source) ORDER BY event_hash) FROM doc_display_sources_v1 source WHERE event_hash IN ($2,$3)),
 'objects',(SELECT jsonb_agg(to_jsonb(object) ORDER BY hash) FROM doc_display_objects_v1 object WHERE source_hash IN
 (SELECT source_hash FROM doc_display_sources_v1 WHERE event_hash IN ($2,$3))),
 'generations',(SELECT jsonb_agg(to_jsonb(generation) ORDER BY event_hash) FROM doc_display_generations_v1 generation WHERE event_hash IN ($2,$3)))::text`,
		fixture.repo, fixture.targets[0].Event.Hash, fixture.targets[1].Event.Hash).Scan(&image); err != nil {
		t.Fatal(err)
	}
	return image
}

func (fixture docDisplayPGFixture) read(t *testing.T, target domain.DocDisplayTarget, want error) {
	t.Helper()
	before := fixture.image(t)
	if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		generation, err := fixture.storage.ReadDocDisplayGeneration(read, target)
		if !errors.Is(err, want) {
			t.Fatalf("read generation: %+v %v, want %v", generation, err, want)
		}
		if err == nil {
			var text strings.Builder
			for index := range generation.Pages.Count {
				page, err := fixture.storage.ReadDocDisplayPage(read, target, index)
				if err != nil {
					return err
				}
				text.WriteString(page.Text)
			}
			if text.String() != fixture.doc.cir.Events[target.Event.Index].Output {
				t.Fatal("display text differs from source")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if after := fixture.image(t); after != before {
		t.Fatal("reader modified durable state")
	}
}

func TestDocDisplayPGCheckpointCASRestart(t *testing.T) {
	fixture := docDisplayPG(t)
	queued := fixture.enqueue(t, 0)
	if duplicate := fixture.enqueue(t, 0); !reflect.DeepEqual(queued, duplicate) {
		t.Fatal("enqueue is not idempotent")
	}
	for _, claim := range []func(context.Context, time.Time, time.Duration) (domain.DocRangeIndexJob, error){fixture.storage.ClaimDocRangeJob, fixture.storage.ClaimDocFindFields} {
		if _, err := claim(fixture.ctx, time.Now().UTC(), time.Minute); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("old worker claimed new display work", err)
		}
	}
	job := fixture.claim(t)
	empty, err := fixture.storage.LoadDocDisplayJob(fixture.ctx, job)
	if err != nil || empty.Source != nil || empty.Checkpoint != "" {
		t.Fatalf("cold preparation: %+v %v", empty, err)
	}
	prepared := fixture.initialize(t, job)
	if duplicate := fixture.initialize(t, job); !reflect.DeepEqual(prepared, duplicate) {
		t.Fatal("initialize is not idempotent")
	}
	step := fixture.step(t, prepared)
	next, err := fixture.storage.SaveDocDisplayStep(fixture.ctx, job, prepared.Checkpoint, step)
	if err != nil || next.Checkpoint == prepared.Checkpoint {
		t.Fatal("save step", err)
	}
	fixture.read(t, job.Target, domain.ErrNotFound)
	before := fixture.image(t)
	if _, err := fixture.storage.SaveDocDisplayStep(fixture.ctx, job, prepared.Checkpoint, step); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("stale checkpoint committed", err)
	}
	if err := fixture.storage.CompleteDocDisplay(fixture.ctx, job, next.Checkpoint, time.Now()); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("incomplete checkpoint published", err)
	}
	if after := fixture.image(t); after != before {
		t.Fatal("failed CAS/completion had effects")
	}
	restarted, err := NewPostgresStore(fixture.ctx, os.Getenv("CXT_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	fixture.storage = restarted
	loaded, err := restarted.LoadDocDisplayJob(fixture.ctx, job)
	if err != nil || !reflect.DeepEqual(next, loaded) {
		t.Fatalf("restart lost checkpoint: %+v %v", loaded, err)
	}
	for !loaded.State.Done {
		loaded, err = restarted.SaveDocDisplayStep(fixture.ctx, job, loaded.Checkpoint, fixture.step(t, loaded))
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := restarted.CompleteDocDisplay(fixture.ctx, job, loaded.Checkpoint, time.Now()); err != nil {
		t.Fatal(err)
	}
	fixture.read(t, job.Target, nil)
	if err := restarted.CompleteDocDisplay(fixture.ctx, job, loaded.Checkpoint, time.Now()); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("completed lease reused", err)
	}
}

func TestDocDisplayPGCompletedDescriptorLoss(t *testing.T) {
	fixture := docDisplayPG(t)
	fixture.enqueue(t, 0)
	job := fixture.claim(t)
	prepared := fixture.done(t, job)
	if err := fixture.storage.CompleteDocDisplay(fixture.ctx, job, prepared.Checkpoint, time.Now()); err != nil {
		t.Fatal(err)
	}
	completed := fixture.enqueue(t, 0)
	fixture.enqueue(t, 1)
	other := fixture.claim(t)
	otherPrepared := fixture.done(t, other)
	if err := fixture.storage.CompleteDocDisplay(fixture.ctx, other, otherPrepared.Checkpoint, time.Now()); err != nil {
		t.Fatal(err)
	}
	var sourceHash, generationHash domain.ContentHash
	var descriptor []byte
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT source_hash,generation_hash,bytes
 FROM doc_display_generations_v1 WHERE event_hash=$1 AND profile=$2`, job.Target.Event.Hash, domain.DocDisplayStringProfile).
		Scan(&sourceHash, &generationHash, &descriptor); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM doc_display_generations_v1 WHERE event_hash=$1 AND profile=$2`,
		job.Target.Event.Hash, domain.DocDisplayStringProfile); err != nil {
		t.Fatal(err)
	}
	fixture.read(t, job.Target, domain.ErrNotFound)
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_display_jobs_v1
 SET reason='descriptor_evicted',attempts=7,next_attempt=clock_timestamp()+interval '1 hour'
 WHERE repo_id=$1 AND event_index=$2`, fixture.repo, job.Target.Event.Index); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	reopened, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, job.Target, now)
	want := completed
	want.State, want.Reason, want.Attempts, want.Version, want.NextAttempt = "waiting", "", 0, completed.Version+1, now.In(completed.NextAttempt.Location())
	if err != nil || !reflect.DeepEqual(reopened, want) {
		t.Fatalf("completed receipt did not reopen: %+v, want %+v: %v", reopened, want, err)
	}
	before := fixture.image(t)
	if duplicate := fixture.enqueue(t, 0); !reflect.DeepEqual(duplicate, reopened) || fixture.image(t) != before {
		t.Fatal("duplicate repair demand changed pending receipt or checkpoint")
	}
	staleWrites := map[string]func() error{
		"load": func() error {
			_, err := fixture.storage.LoadDocDisplayJob(fixture.ctx, job)
			return err
		},
		"initialize": func() error {
			_, err := fixture.storage.InitializeDocDisplay(fixture.ctx, job, *prepared.Source)
			return err
		},
		"save": func() error {
			_, err := fixture.storage.SaveDocDisplayStep(fixture.ctx, job, prepared.Checkpoint, domain.DocDisplayStringStep{})
			return err
		},
		"complete": func() error {
			return fixture.storage.CompleteDocDisplay(fixture.ctx, job, prepared.Checkpoint, time.Now())
		},
		"renew": func() error {
			return fixture.storage.RenewDocDisplay(fixture.ctx, job, time.Now(), time.Minute)
		},
		"finish": func() error {
			stale := job
			stale.State = "waiting"
			return fixture.storage.FinishDocDisplay(fixture.ctx, stale, time.Now())
		},
	}
	for _, phase := range []string{"waiting", "running"} {
		if phase == "running" {
			reopened = fixture.claim(t)
			if reopened.Version != want.Version+1 || reopened.Attempts != 1 {
				t.Fatalf("repair claim did not advance fence: %+v", reopened)
			}
		}
		before = fixture.image(t)
		for name, write := range staleWrites {
			if err := write(); !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("old completed worker %s during %s: %v", name, phase, err)
			}
		}
		if fixture.image(t) != before {
			t.Fatal("old completed worker changed repair state")
		}
	}
	loaded, err := fixture.storage.LoadDocDisplayJob(fixture.ctx, reopened)
	if err != nil || !reflect.DeepEqual(loaded, prepared) || !loaded.State.Done {
		t.Fatalf("repair lost trusted Done checkpoint: %+v %v", loaded, err)
	}
	if err := fixture.storage.CompleteDocDisplay(fixture.ctx, reopened, loaded.Checkpoint, time.Now()); err != nil {
		t.Fatal(err)
	}
	var restoredSource, restoredHash domain.ContentHash
	var restored []byte
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT source_hash,generation_hash,bytes
 FROM doc_display_generations_v1 WHERE event_hash=$1 AND profile=$2`, job.Target.Event.Hash, domain.DocDisplayStringProfile).
		Scan(&restoredSource, &restoredHash, &restored); err != nil {
		t.Fatal(err)
	}
	if restoredSource != sourceHash || restoredHash != generationHash || !bytes.Equal(restored, descriptor) {
		t.Fatal("repair did not restore exact descriptor and source association")
	}
	fixture.read(t, job.Target, nil)
}

func TestDocDisplayPGCompletedDescriptorReceiptStable(t *testing.T) {
	for _, corruption := range []string{"intact", "generation_hash", "generation_bytes", "source_bytes", "source_association"} {
		t.Run(corruption, func(t *testing.T) {
			fixture := docDisplayPG(t)
			fixture.enqueue(t, 0)
			job := fixture.claim(t)
			prepared := fixture.done(t, job)
			if err := fixture.storage.CompleteDocDisplay(fixture.ctx, job, prepared.Checkpoint, time.Now()); err != nil {
				t.Fatal(err)
			}
			completed := fixture.enqueue(t, 0)
			var err error
			switch corruption {
			case "generation_hash":
				_, err = fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_display_generations_v1 SET generation_hash=$2
 WHERE event_hash=$1`, job.Target.Event.Hash, domain.HashContent([]byte("corrupt")))
			case "generation_bytes":
				_, err = fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_display_generations_v1 SET bytes=$2
 WHERE event_hash=$1`, job.Target.Event.Hash, []byte(`{}`))
			case "source_bytes":
				_, err = fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_display_sources_v1 SET bytes=$2
 WHERE source_hash=$1`, prepared.State.Source, []byte(`{}`))
			case "source_association":
				source := fixture.source(t, 1)
				raw, encodeErr := source.Encode()
				if encodeErr != nil {
					t.Fatal(encodeErr)
				}
				if _, err := fixture.storage.pool.Exec(fixture.ctx, `INSERT INTO doc_display_sources_v1(event_hash,profile,source_hash,bytes)
 VALUES($1,$2,$3,$4)`, source.EventHash, source.Profile, domain.HashContent(raw), raw); err != nil {
					t.Fatal(err)
				}
				_, err = fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_display_generations_v1 SET source_hash=$2
 WHERE event_hash=$1`, job.Target.Event.Hash, domain.HashContent(raw))
			}
			if err != nil {
				t.Fatal(err)
			}
			before := fixture.image(t)
			if receipt := fixture.enqueue(t, 0); !reflect.DeepEqual(receipt, completed) || fixture.image(t) != before {
				t.Fatal("present descriptor reopened or changed completed receipt")
			}
			if corruption == "intact" {
				fixture.read(t, job.Target, nil)
			} else {
				fixture.read(t, job.Target, domain.ErrIntegrity)
			}
		})
	}
}

func TestDocDisplayPGCompletedDescriptorRepairCapacity(t *testing.T) {
	fixture := docDisplayPG(t,
		domain.CIREvent{Kind: domain.EventToolResult, Seq: 1, CallID: "repair", Output: t.Name()},
		domain.CIREvent{Kind: domain.EventToolResult, Seq: 2, CallID: "active", Output: t.Name()},
		domain.CIREvent{Kind: domain.EventToolResult, Seq: 3, CallID: "new", Output: t.Name()})
	fixture.enqueue(t, 0)
	job := fixture.claim(t)
	prepared := fixture.done(t, job)
	if err := fixture.storage.CompleteDocDisplay(fixture.ctx, job, prepared.Checkpoint, time.Now()); err != nil {
		t.Fatal(err)
	}
	completed := fixture.enqueue(t, 0)
	fixture.enqueue(t, 1)
	active := fixture.claim(t)
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `INSERT INTO doc_display_jobs_v1
 (repo_id,doc_hash,profile,event_index,event_hash,event_offset,event_length,event_seq,event_role,source_id,source_version,range_generation,state,created_at,next_attempt)
 SELECT repo_id,doc_hash,profile,pending.index,event_hash,event_offset,event_length,event_seq,event_role,source_id,source_version,range_generation,
 'waiting',created_at,next_attempt FROM doc_display_jobs_v1 CROSS JOIN generate_series(3,$2::integer) pending(index)
 WHERE repo_id=$1 AND event_index=1`, fixture.repo, domain.MaxPendingDocRangeJobs+1); err != nil {
		t.Fatal(err)
	}
	before := fixture.image(t)
	if receipt := fixture.enqueue(t, 0); !reflect.DeepEqual(receipt, completed) || fixture.image(t) != before {
		t.Fatal("full queue changed intact completed receipt")
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM doc_display_generations_v1 WHERE event_hash=$1`, job.Target.Event.Hash); err != nil {
		t.Fatal(err)
	}
	before = fixture.image(t)
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, job.Target, time.Now()); !errors.Is(err, domain.ErrResourceOverloaded) {
		t.Fatal("repair bypassed active capacity", err)
	}
	if fixture.image(t) != before {
		t.Fatal("overloaded repair changed completed receipt or checkpoint")
	}
	if receipt := fixture.enqueue(t, 1); !reflect.DeepEqual(receipt, active) || fixture.image(t) != before {
		t.Fatal("full queue changed active receipt")
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM doc_display_jobs_v1 WHERE repo_id=$1 AND event_index=3`, fixture.repo); err != nil {
		t.Fatal(err)
	}
	reopened := fixture.enqueue(t, 0)
	if reopened.State != "waiting" || reopened.Version != completed.Version+1 {
		t.Fatalf("available repair slot not admitted: %+v", reopened)
	}
	var pending int
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_jobs_v1
 WHERE repo_id=$1 AND state IN ('waiting','running','retrying')`, fixture.repo).Scan(&pending); err != nil || pending != domain.MaxPendingDocRangeJobs {
		t.Fatal("reopened receipt not counted as pending", pending, err)
	}
	before = fixture.image(t)
	if receipt := fixture.enqueue(t, 0); !reflect.DeepEqual(receipt, reopened) || fixture.image(t) != before {
		t.Fatal("full queue changed duplicate repair receipt")
	}
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, fixture.targets[2], time.Now()); !errors.Is(err, domain.ErrResourceOverloaded) {
		t.Fatal("new demand ignored reopened job capacity", err)
	}
	if fixture.image(t) != before {
		t.Fatal("new overloaded demand changed repair state")
	}
}

func TestDocDisplayPGCompletedDescriptorRepairCorruptProgress(t *testing.T) {
	for _, corruption := range []string{"missing_preparation", "missing_source", "source", "checkpoint_hash", "checkpoint_bytes", "checkpoint_object"} {
		t.Run(corruption, func(t *testing.T) {
			fixture := docDisplayPG(t)
			fixture.enqueue(t, 0)
			job := fixture.claim(t)
			prepared := fixture.done(t, job)
			if err := fixture.storage.CompleteDocDisplay(fixture.ctx, job, prepared.Checkpoint, time.Now()); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM doc_display_generations_v1 WHERE event_hash=$1`, job.Target.Event.Hash); err != nil {
				t.Fatal(err)
			}
			var err error
			switch corruption {
			case "missing_preparation":
				if _, err := docDisplayJobCorruptionPG(fixture.ctx, fixture.storage, `UPDATE doc_display_jobs_v1
 SET source_hash=NULL,checkpoint_hash=NULL,checkpoint=NULL WHERE repo_id=$1`, fixture.repo); err != nil {
					t.Fatal(err)
				}
				_, err = fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM doc_display_sources_v1 WHERE source_hash=$1`, prepared.State.Source)
			case "missing_source":
				_, err = fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_display_sources_v1 SET event_hash=$2
 WHERE source_hash=$1`, prepared.State.Source, fixture.targets[1].Event.Hash)
			case "source":
				_, err = fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_display_sources_v1 SET bytes=$2
 WHERE source_hash=$1`, prepared.State.Source, []byte(`{}`))
			case "checkpoint_hash":
				_, err = docDisplayJobCorruptionPG(fixture.ctx, fixture.storage, `UPDATE doc_display_jobs_v1 SET checkpoint_hash=$2
 WHERE repo_id=$1`, fixture.repo, domain.HashContent([]byte(`{}`)))
			case "checkpoint_bytes":
				_, err = docDisplayJobCorruptionPG(fixture.ctx, fixture.storage, `UPDATE doc_display_jobs_v1 SET checkpoint_hash=$2,checkpoint=$3
 WHERE repo_id=$1`, fixture.repo, domain.HashContent([]byte(`{}`)), []byte(`{}`))
			case "checkpoint_object":
				_, err = fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM doc_display_objects_v1 WHERE source_hash=$1`, prepared.State.Source)
			}
			if err != nil {
				t.Fatal(err)
			}
			if corruption == "missing_preparation" {
				before := fixture.image(t)
				if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, job.Target, time.Now()); !errors.Is(err, domain.ErrIntegrity) {
					t.Fatal("completed receipt without trusted preparation fell back to cold work", err)
				}
				if fixture.image(t) != before {
					t.Fatal("missing preparation changed completed receipt")
				}
				return
			}
			reopened := fixture.enqueue(t, 0)
			if reopened.State != "waiting" || reopened.Version != job.Version+1 {
				t.Fatalf("missing descriptor did not reopen: %+v", reopened)
			}
			reopened = fixture.claim(t)
			before := fixture.image(t)
			if _, err := fixture.storage.LoadDocDisplayJob(fixture.ctx, reopened); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatal("repair fell back from corrupt trusted preparation", err)
			}
			if _, err := fixture.storage.InitializeDocDisplay(fixture.ctx, reopened, *prepared.Source); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatal("repair initialization healed corrupt preparation", err)
			}
			if err := fixture.storage.CompleteDocDisplay(fixture.ctx, reopened, prepared.Checkpoint, time.Now()); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatal("repair completed corrupt preparation", err)
			}
			if fixture.image(t) != before {
				t.Fatal("corrupt repair changed trusted preparation or published descriptor")
			}
		})
	}
}

func TestDocDisplayPGLeaseRetryAndClockFences(t *testing.T) {
	fixture := docDisplayPG(t)
	fixture.enqueue(t, 0)
	job := fixture.claim(t)
	prepared := fixture.initialize(t, job)
	prepared, err := fixture.storage.SaveDocDisplayStep(fixture.ctx, job, prepared.Checkpoint, fixture.step(t, prepared))
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.storage.RenewDocDisplay(fixture.ctx, job, time.Now(), 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"waiting", "retrying", "rejected"} {
		job.State, job.NextAttempt = state, time.Now().Add(-time.Second)
		if err := fixture.storage.FinishDocDisplay(fixture.ctx, job, time.Now()); err != nil {
			t.Fatal(err)
		}
		if state == "rejected" {
			break
		}
		job = fixture.claim(t)
		loaded, err := fixture.storage.LoadDocDisplayJob(fixture.ctx, job)
		if err != nil || !reflect.DeepEqual(prepared, loaded) {
			t.Fatal("retry lost progress", err)
		}
	}
	fixture.enqueue(t, 1)
	job = fixture.claim(t)
	prepared = fixture.initialize(t, job)
	if _, err := fixture.storage.ClaimDocDisplay(fixture.ctx, time.Now().Add(time.Hour), time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("caller clock expired a live DB lease", err)
	}
	if _, err := docDisplayLeaseMutationPG(fixture.ctx, fixture.storage, fixture.repo, `UPDATE doc_display_jobs_v1 SET lease_until=clock_timestamp()-interval '1 second' WHERE repo_id=$1 AND state='running'`, fixture.repo); err != nil {
		t.Fatal(err)
	}
	before := fixture.image(t)
	if _, err := fixture.storage.SaveDocDisplayStep(fixture.ctx, job, prepared.Checkpoint, fixture.step(t, prepared)); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("expired save", err)
	}
	if err := fixture.storage.RenewDocDisplay(fixture.ctx, job, time.Now().Add(-time.Hour), time.Minute); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("caller clock resurrected expired lease", err)
	}
	if after := fixture.image(t); before != after {
		t.Fatal("expired worker changed progress")
	}
	reclaimed := fixture.claim(t)
	if reclaimed.Version != job.Version+1 || reclaimed.Attempts != job.Attempts+1 {
		t.Fatal("reclaim did not advance fence")
	}
	loaded, err := fixture.storage.LoadDocDisplayJob(fixture.ctx, reclaimed)
	if err != nil || !reflect.DeepEqual(loaded, prepared) {
		t.Fatal("lease expiry lost progress", err)
	}
	if _, err := fixture.storage.LoadDocDisplayJob(fixture.ctx, job); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("old version loaded progress", err)
	}
	job.State = "completed"
	if err := fixture.storage.FinishDocDisplay(fixture.ctx, job, time.Now()); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("Finish bypassed Complete", err)
	}
}

func TestDocDisplayPGTypedTargetAndSourceBinding(t *testing.T) {
	fixture := docDisplayPG(t)
	for _, field := range []string{"hash", "offset", "length", "uuid", "version", "range_hash", "kind"} {
		t.Run(field, func(t *testing.T) {
			target := fixture.targets[0]
			switch field {
			case "hash":
				target.Event.Hash = fixture.targets[1].Event.Hash
			case "offset":
				target.Event.Offset++
			case "length":
				target.Event.Length--
			case "uuid":
				target.Binding.SourceID = "11111111-1111-4111-8111-111111111111"
			case "version":
				target.Binding.SourceVersion++
			case "range_hash":
				target.Binding.RangeGeneration = fixture.targets[1].Event.Hash
			case "kind":
				target = fixture.targets[2]
			}
			if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now()); !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("forged target admitted: %v", err)
			}
		})
	}
	fixture.enqueue(t, 0)
	job := fixture.claim(t)
	prepared := fixture.initialize(t, job)
	for _, mutation := range []string{"uuid", "version"} {
		old := fixture.targets[0]
		if mutation == "uuid" {
			if _, err := fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM doc_range_generations_v2 WHERE doc_hash=$1`, fixture.doc.hash); err != nil {
				t.Fatal(err)
			}
			if err := fixture.storage.WithinRepository(fixture.ctx, fixture.repo, func(bound context.Context) error {
				return fixture.storage.ensureRangeGeneration(bound, fixture.repo, fixture.doc.hash)
			}); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err := fixture.storage.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.doc.hash); err != nil {
				t.Fatal(err)
			}
			if err := fixture.storage.WithinRepository(fixture.ctx, fixture.repo, func(bound context.Context) error {
				return fixture.storage.ensureRangeGeneration(bound, fixture.repo, fixture.doc.hash)
			}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := fixture.storage.SaveDocDisplayStep(fixture.ctx, job, prepared.Checkpoint, fixture.step(t, prepared)); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("old binding wrote", err)
		}
		fixture.read(t, old, domain.ErrConflict)
		fixture.refreshTargets(t)
		queued := fixture.enqueue(t, 0)
		if queued.Version != job.Version+1 || queued.State != "waiting" || queued.Attempts != 0 {
			t.Fatal("binding change did not reset queue")
		}
		job = fixture.claim(t)
		loaded, err := fixture.storage.LoadDocDisplayJob(fixture.ctx, job)
		if err != nil || loaded.Source == nil || loaded.Checkpoint != "" || !reflect.DeepEqual(loaded.State, domain.DocDisplayStringState{}) {
			t.Fatalf("fresh binding must reuse source, not progress: %+v %v", loaded, err)
		}
		prepared = fixture.initialize(t, job)
	}
}

func TestDocDisplayPGConcurrentClaimsPerEventAndCapacity(t *testing.T) {
	fixture := docDisplayPG(t)
	fixture.enqueue(t, 0)
	fixture.enqueue(t, 1)
	results := make(chan error, 8)
	jobs := make(chan domain.DocDisplayJob, 8)
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			job, err := fixture.storage.ClaimDocDisplay(fixture.ctx, time.Now(), time.Minute)
			if err == nil {
				jobs <- job
			} else {
				results <- err
			}
		})
	}
	workers.Wait()
	close(jobs)
	close(results)
	for err := range results {
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatal(err)
		}
	}
	if len(jobs) != 1 {
		t.Fatalf("claimed %d jobs in one repository", len(jobs))
	}
	first := <-jobs
	first.State = "rejected"
	if err := fixture.storage.FinishDocDisplay(fixture.ctx, first, time.Now()); err != nil {
		t.Fatal(err)
	}
	second := fixture.claim(t)
	if second.Target.Event.Index == first.Target.Event.Index {
		t.Fatal("parallel event task overwritten")
	}
	for index := 3; index < domain.MaxPendingDocRangeJobs+2; index++ {
		if _, err := fixture.storage.pool.Exec(fixture.ctx, `INSERT INTO doc_display_jobs_v1
 (repo_id,doc_hash,profile,event_index,event_hash,event_offset,event_length,event_seq,event_role,source_id,source_version,range_generation,state,created_at,next_attempt)
 SELECT repo_id,doc_hash,profile,$2,event_hash,event_offset,event_length,event_seq,event_role,source_id,source_version,range_generation,
 'waiting',created_at,next_attempt FROM doc_display_jobs_v1 WHERE repo_id=$1 AND event_index=$3`, fixture.repo, index, second.Target.Event.Index); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM doc_display_jobs_v1 WHERE repo_id=$1 AND event_index=$2`, fixture.repo, first.Target.Event.Index); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, first.Target, time.Now()); !errors.Is(err, domain.ErrResourceOverloaded) {
		t.Fatal("queue exceeded capacity", err)
	}
	if receipt := fixture.enqueue(t, second.Target.Event.Index); receipt.Version != second.Version {
		t.Fatal("full queue lost idempotent receipt")
	}
}

func TestDocDisplayPGReadOnlyAndPinnedSnapshot(t *testing.T) {
	fixture := docDisplayPG(t)
	fixture.enqueue(t, 0)
	job := fixture.claim(t)
	source := fixture.source(t, 0)
	readOnly := outbound.WithDocReadOnly(fixture.ctx)
	for name, action := range map[string]func() error{
		"enqueue": func() error {
			_, err := fixture.storage.EnqueueDocDisplay(readOnly, job.Target, time.Now())
			return err
		},
		"claim":      func() error { _, err := fixture.storage.ClaimDocDisplay(readOnly, time.Now(), time.Minute); return err },
		"renew":      func() error { return fixture.storage.RenewDocDisplay(readOnly, job, time.Now(), time.Minute) },
		"load":       func() error { _, err := fixture.storage.LoadDocDisplayJob(readOnly, job); return err },
		"initialize": func() error { _, err := fixture.storage.InitializeDocDisplay(readOnly, job, source); return err },
		"save": func() error {
			_, err := fixture.storage.SaveDocDisplayStep(readOnly, job, domain.HashContent(nil), domain.DocDisplayStringStep{})
			return err
		},
		"complete": func() error {
			return fixture.storage.CompleteDocDisplay(readOnly, job, domain.HashContent(nil), time.Now())
		},
		"finish": func() error {
			rejected := job
			rejected.State = "rejected"
			return fixture.storage.FinishDocDisplay(readOnly, rejected, time.Now())
		},
	} {
		if err := action(); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("read-only %s: %v", name, err)
		}
	}
	if _, err := fixture.storage.ReadDocDisplayGeneration(fixture.ctx, job.Target); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("unpinned read accepted", err)
	}
	transaction, err := fixture.storage.pool.BeginTx(fixture.ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackPG(transaction)
	bound := context.WithValue(fixture.ctx, repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: fixture.storage, readOnly: true})
	if _, err := fixture.storage.ReadDocDisplayPage(bound, job.Target, 0); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("read-committed snapshot accepted", err)
	}
	prepared := fixture.done(t, job)
	if err := fixture.storage.CompleteDocDisplay(fixture.ctx, job, prepared.Checkpoint, time.Now()); err != nil {
		t.Fatal(err)
	}
	fixture.read(t, job.Target, nil)
}

func TestDocDisplayPGCrossRepoReuseAndOwnershipRevocation(t *testing.T) {
	fixture := docDisplayPG(t)
	fixture.enqueue(t, 0)
	job := fixture.claim(t)
	prepared := fixture.done(t, job)
	if err := fixture.storage.CompleteDocDisplay(fixture.ctx, job, prepared.Checkpoint, time.Now()); err != nil {
		t.Fatal(err)
	}
	foreign, _ := rootPGRepo(t, fixture.ctx, fixture.storage)
	other := job.Target
	other.RepoID = foreign
	fixture.read(t, other, domain.ErrNotFound)
	seedRootPG(t, fixture.ctx, fixture.storage, foreign, fixture.doc)
	if err := fixture.storage.PutSnapshot(fixture.ctx, domain.Snapshot{ID: fixture.doc.hash, RepoID: foreign, DocHash: fixture.doc.hash,
		DocIdentity: domain.DocumentIdentityRootV1, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := fixture.storage.pool.Exec(context.Background(), `DELETE FROM doc_display_jobs_v1 WHERE repo_id=$1`, foreign); err != nil {
			t.Error(err)
		}
	})
	fixture.read(t, other, nil)
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, other, time.Now()); err != nil {
		t.Fatal(err)
	}
	second, err := fixture.storage.ClaimDocDisplay(fixture.ctx, time.Now(), time.Minute)
	if err != nil || second.RepoID != foreign {
		t.Fatal("cross-repository claim", err)
	}
	loaded, err := fixture.storage.LoadDocDisplayJob(fixture.ctx, second)
	if err != nil || !reflect.DeepEqual(loaded, prepared) || !loaded.State.Done {
		t.Fatalf("completed source/checkpoint not reused exactly: %+v %v", loaded, err)
	}
	var state string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT state FROM doc_display_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND event_index=$3 AND profile=$4`,
		foreign, second.DocHash, second.Target.Event.Index, second.Target.DisplayProfile()).Scan(&state); err != nil || state != "running" {
		t.Fatal("reused preparation fabricated a completed receipt", state, err)
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, foreign, fixture.doc.hash); err != nil {
		t.Fatal(err)
	}
	fixture.read(t, other, domain.ErrNotFound)
	if _, err := fixture.storage.InitializeDocDisplay(fixture.ctx, second, *loaded.Source); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("revoked owner initialized progress", err)
	}
	before := fixture.image(t)
	if err := fixture.storage.CompleteDocDisplay(fixture.ctx, second, loaded.Checkpoint, time.Now()); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("revoked owner completed borrowed progress", err)
	}
	if after := fixture.image(t); after != before {
		t.Fatal("revoked completion changed publication or receipt")
	}
	fixture.read(t, job.Target, nil)
}

func TestDocDisplayPGInvalidStepAndImmutableCollisionAtomicity(t *testing.T) {
	fixture := docDisplayPG(t)
	fixture.enqueue(t, 0)
	job := fixture.claim(t)
	prepared := fixture.initialize(t, job)
	step := fixture.step(t, prepared)
	before := fixture.image(t)
	invalid := step
	invalid.Objects = nil
	if _, err := fixture.storage.SaveDocDisplayStep(fixture.ctx, job, prepared.Checkpoint, invalid); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("omitted objects accepted", err)
	}
	if after := fixture.image(t); after != before {
		t.Fatal("invalid step had effects")
	}
	object := step.Objects[len(step.Objects)-1]
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `INSERT INTO doc_display_objects_v1(source_hash,hash,bytes) VALUES($1,$2,$3)`,
		prepared.State.Source, object.Hash, []byte("collision")); err != nil {
		t.Fatal(err)
	}
	before = fixture.image(t)
	if _, err := fixture.storage.SaveDocDisplayStep(fixture.ctx, job, prepared.Checkpoint, step); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("collision overwritten", err)
	}
	if after := fixture.image(t); after != before {
		t.Fatal("collision failure had effects")
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM doc_display_objects_v1 WHERE source_hash=$1`, prepared.State.Source); err != nil {
		t.Fatal(err)
	}
	prepared = fixture.done(t, job)
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `INSERT INTO doc_display_generations_v1(event_hash,profile,source_hash,generation_hash,bytes)
 VALUES($1,$2,$3,$4,$5)`, prepared.Source.EventHash, domain.DocDisplayStringProfile, prepared.State.Source, domain.HashContent([]byte("collision")), []byte("collision")); err != nil {
		t.Fatal(err)
	}
	before = fixture.image(t)
	if err := fixture.storage.CompleteDocDisplay(fixture.ctx, job, prepared.Checkpoint, time.Now()); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("generation collision overwritten", err)
	}
	if after := fixture.image(t); after != before {
		t.Fatal("failed Complete leaked tail directories")
	}
}

func TestDocDisplayPGTrustedCheckpointAndCorruption(t *testing.T) {
	for _, corruption := range []string{"checkpoint_hash", "checkpoint_bytes", "source", "generation", "directory", "page", "range_directory"} {
		t.Run(corruption, func(t *testing.T) {
			fixture := docDisplayPG(t)
			fixture.enqueue(t, 0)
			job := fixture.claim(t)
			prepared := fixture.done(t, job)
			if corruption == "checkpoint_hash" || corruption == "checkpoint_bytes" {
				raw := []byte(`{}`)
				hash := domain.HashContent(raw)
				if corruption == "checkpoint_hash" {
					if _, err := docDisplayJobCorruptionPG(fixture.ctx, fixture.storage, `UPDATE doc_display_jobs_v1 SET checkpoint_hash=$2 WHERE repo_id=$1`, fixture.repo, hash); err != nil {
						t.Fatal(err)
					}
				} else if _, err := docDisplayJobCorruptionPG(fixture.ctx, fixture.storage, `UPDATE doc_display_jobs_v1 SET checkpoint_hash=$2,checkpoint=$3 WHERE repo_id=$1`, fixture.repo, hash, raw); err != nil {
					t.Fatal(err)
				}
				before := fixture.image(t)
				if _, err := fixture.storage.LoadDocDisplayJob(fixture.ctx, job); !errors.Is(err, domain.ErrIntegrity) {
					t.Fatal("corrupt checkpoint loaded", err)
				}
				if err := fixture.storage.CompleteDocDisplay(fixture.ctx, job, prepared.Checkpoint, time.Now()); !errors.Is(err, domain.ErrIntegrity) {
					t.Fatal("caller hash healed trusted checkpoint", err)
				}
				if fixture.image(t) != before {
					t.Fatal("corruption failure had effects")
				}
				return
			}
			if err := fixture.storage.CompleteDocDisplay(fixture.ctx, job, prepared.Checkpoint, time.Now()); err != nil {
				t.Fatal(err)
			}
			var err error
			switch corruption {
			case "source":
				_, err = fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_display_sources_v1 SET bytes=$2 WHERE source_hash=$1`, prepared.State.Source, []byte(`{}`))
			case "generation":
				_, err = fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_display_generations_v1 SET bytes=$2 WHERE source_hash=$1`, prepared.State.Source, []byte(`{}`))
			case "directory", "page":
				generation, _, finishErr := domain.FinishDocDisplayString(fixture.ctx, *prepared.Source, prepared.State)
				if finishErr != nil {
					t.Fatal(finishErr)
				}
				hash := generation.Pages.Hash
				if corruption == "page" {
					hash, err = domain.ResolveDocRangeLeaf(fixture.ctx, generation.Pages, 0, func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
						var raw []byte
						err := fixture.storage.pool.QueryRow(ctx, `SELECT bytes FROM doc_display_objects_v1 WHERE source_hash=$1 AND hash=$2`, prepared.State.Source, hash).Scan(&raw)
						return raw, err
					})
					if err != nil {
						t.Fatal(err)
					}
				}
				_, err = fixture.storage.pool.Exec(fixture.ctx, `WITH replaced AS (DELETE FROM doc_display_objects_v1 WHERE source_hash=$1 AND hash=$2 RETURNING source_hash,hash)
 INSERT INTO doc_display_objects_v1(source_hash,hash,bytes) SELECT source_hash,hash,$3 FROM replaced`, prepared.State.Source, hash, []byte(`{}`))
			case "range_directory":
				_, err = fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_range_objects_v2 SET bytes=$2 WHERE doc_hash=$1`, fixture.doc.hash, []byte(`{}`))
			}
			if err != nil {
				t.Fatal(err)
			}
			if corruption == "directory" || corruption == "page" {
				if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
					_, err := fixture.storage.ReadDocDisplayPage(read, job.Target, 0)
					if !errors.Is(err, domain.ErrIntegrity) {
						t.Fatal("corrupt selected path served", err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				fixture.read(t, job.Target, domain.ErrIntegrity)
			}
		})
	}
}

func TestDocDisplayPGRehashedSkippedCheckpoint(t *testing.T) {
	for _, mutation := range []string{"next", "done"} {
		t.Run(mutation, func(t *testing.T) {
			fixture := docDisplayPG(t)
			fixture.enqueue(t, 0)
			job := fixture.claim(t)
			prepared := fixture.initialize(t, job)
			prepared, err := fixture.storage.SaveDocDisplayStep(fixture.ctx, job, prepared.Checkpoint, fixture.step(t, prepared))
			if err != nil || prepared.State.Directory.Count != 1 || prepared.State.Done {
				t.Fatalf("one-page progress: %+v %v", prepared, err)
			}
			loaded, err := fixture.storage.LoadDocDisplayJob(fixture.ctx, job)
			if err != nil || !reflect.DeepEqual(loaded, prepared) {
				t.Fatalf("valid checkpoint rejected: %+v %v", loaded, err)
			}
			changed := prepared.State
			changed.Next++
			if mutation == "done" {
				changed.Next, changed.Done = prepared.Source.Token.To, true
			}
			raw, err := changed.Encode(*prepared.Source)
			if err != nil {
				t.Fatal("test requires syntactically plausible skipped progress", err)
			}
			checkpoint := domain.HashContent(raw)
			if _, err := domain.DecodeDocDisplayStringState(*prepared.Source, checkpoint, raw); err != nil {
				t.Fatal("test checkpoint does not pass intrinsic hash/state validation", err)
			}
			if _, err := docDisplayJobCorruptionPG(fixture.ctx, fixture.storage, `UPDATE doc_display_jobs_v1 SET checkpoint=$4,checkpoint_hash=$5
 WHERE repo_id=$1 AND doc_hash=$2 AND event_index=$3`, job.RepoID, job.DocHash, job.Target.Event.Index, raw, checkpoint); err != nil {
				t.Fatal(err)
			}
			before := fixture.image(t)
			if _, err := fixture.storage.LoadDocDisplayJob(fixture.ctx, job); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatal("rehashed skipped checkpoint loaded", err)
			}
			if err := fixture.storage.CompleteDocDisplay(fixture.ctx, job, checkpoint, time.Now()); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatal("rehashed skipped checkpoint completed", err)
			}
			if fixture.image(t) != before {
				t.Fatal("checkpoint rejection changed persisted state")
			}
			var completed bool
			if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT EXISTS(SELECT 1 FROM doc_display_generations_v1
 WHERE event_hash=$1 AND profile=$2)`, job.Target.Event.Hash, domain.DocDisplayStringProfile).Scan(&completed); err != nil || completed {
				t.Fatal("skipped checkpoint published a completed descriptor", completed, err)
			}
		})
	}
}

func TestDocDisplayPGFullWidthEventSequence(t *testing.T) {
	maximum := int(^uint(0) >> 1)
	sequences := []int{-maximum - 1, 0, maximum}
	var events []domain.CIREvent
	for _, sequence := range sequences {
		events = append(events, domain.CIREvent{Kind: domain.EventToolResult, Seq: sequence, Output: t.Name()})
	}
	fixture := docDisplayPG(t, events...)
	for index, sequence := range sequences {
		queued := fixture.enqueue(t, index)
		if queued.Target.Event.Seq != sequence {
			t.Fatalf("enqueue narrowed event metadata: %+v", queued.Target.Event)
		}
		job := fixture.claim(t)
		if job.Target.Event.Seq != sequence {
			t.Fatalf("claim narrowed event metadata: %+v", job.Target.Event)
		}
		prepared := fixture.done(t, job)
		if err := fixture.storage.CompleteDocDisplay(fixture.ctx, job, prepared.Checkpoint, time.Now()); err != nil {
			t.Fatal(err)
		}
		fixture.read(t, job.Target, nil)
	}
}

func TestDocDisplayPGBoundedEnqueueWait(t *testing.T) {
	fixture := docDisplayPG(t)
	blocker, err := fixture.storage.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackPG(blocker)
	if _, err := blocker.Exec(fixture.ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "cxthub-doc-display-enqueue:"+string(fixture.repo)); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, fixture.targets[0], time.Now()); !errors.Is(err, domain.ErrResourceOverloaded) {
		t.Fatal("unbounded queue wait", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("enqueue exceeded bounded wait")
	}
}

func docDisplayPGDelay(t *testing.T, fixture docDisplayPGFixture, table, field string, hash domain.ContentHash) {
	t.Helper()
	name := fmt.Sprintf("display_delay_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `CREATE FUNCTION `+identifier+`() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.`+field+`='`+string(hash)+`' THEN PERFORM pg_sleep(0.15); END IF; RETURN NEW; END $$;
 CREATE TRIGGER `+identifier+` BEFORE INSERT ON `+table+` FOR EACH ROW EXECUTE FUNCTION `+identifier+`()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := fixture.storage.pool.Exec(context.Background(), `DROP TRIGGER `+identifier+` ON `+table+`; DROP FUNCTION `+identifier+`()`); err != nil {
			t.Error(err)
		}
	})
}

func TestDocDisplayPGLeaseExpiryAtomicity(t *testing.T) {
	for _, operation := range []string{"initialize", "save", "complete"} {
		t.Run(operation, func(t *testing.T) {
			fixture := docDisplayPG(t)
			fixture.enqueue(t, 0)
			job := fixture.claim(t)
			var prepared domain.DocDisplayPreparation
			var step domain.DocDisplayStringStep
			if operation != "initialize" {
				prepared = fixture.initialize(t, job)
			}
			switch operation {
			case "initialize":
				docDisplayPGDelay(t, fixture, "doc_display_sources_v1", "event_hash", job.Target.Event.Hash)
			case "save":
				step = fixture.step(t, prepared)
				docDisplayPGDelay(t, fixture, "doc_display_objects_v1", "source_hash", prepared.State.Source)
			case "complete":
				prepared = fixture.done(t, job)
				docDisplayPGDelay(t, fixture, "doc_display_generations_v1", "source_hash", prepared.State.Source)
			}
			if _, err := docDisplayLeaseMutationPG(fixture.ctx, fixture.storage, fixture.repo, `UPDATE doc_display_jobs_v1 SET lease_until=clock_timestamp()+interval '80 milliseconds' WHERE repo_id=$1`, fixture.repo); err != nil {
				t.Fatal(err)
			}
			before := fixture.image(t)
			var err error
			switch operation {
			case "initialize":
				_, err = fixture.storage.InitializeDocDisplay(fixture.ctx, job, fixture.source(t, 0))
			case "save":
				_, err = fixture.storage.SaveDocDisplayStep(fixture.ctx, job, prepared.Checkpoint, step)
			case "complete":
				err = fixture.storage.CompleteDocDisplay(fixture.ctx, job, prepared.Checkpoint, time.Now())
			}
			if !errors.Is(err, domain.ErrConflict) {
				t.Fatal("expired transaction committed", err)
			}
			if fixture.image(t) != before {
				t.Fatal("lease expiry leaked partial source, objects, checkpoint, or descriptor")
			}
		})
	}
}

func TestDocDisplayPGConcurrentCheckpointCAS(t *testing.T) {
	fixture := docDisplayPG(t)
	fixture.enqueue(t, 0)
	job := fixture.claim(t)
	before := fixture.image(t)
	wrong := fixture.source(t, 1)
	if _, err := fixture.storage.InitializeDocDisplay(fixture.ctx, job, wrong); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("wrong event source initialized", err)
	}
	if fixture.image(t) != before {
		t.Fatal("wrong source initialization had effects")
	}
	prepared := fixture.initialize(t, job)
	step := fixture.step(t, prepared)
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			_, err := fixture.storage.SaveDocDisplayStep(fixture.ctx, job, prepared.Checkpoint, step)
			results <- err
		})
	}
	workers.Wait()
	close(results)
	committed := 0
	for err := range results {
		if err == nil {
			committed++
		} else if !errors.Is(err, domain.ErrConflict) {
			t.Fatal(err)
		}
	}
	if committed != 1 {
		t.Fatalf("checkpoint committed %d times", committed)
	}
}

func TestDocDisplayPGWriteLocksCurrentSource(t *testing.T) {
	fixture := docDisplayPG(t)
	fixture.enqueue(t, 0)
	job := fixture.claim(t)
	locked, release := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- fixture.storage.docDisplayWrite(fixture.ctx, job, time.Time{}, func(_ context.Context, _ pgx.Tx) error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	for _, statement := range []string{
		`UPDATE blobs SET bytes=bytes WHERE hash=$2 AND $1::text<>''`,
		`DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`,
		`DELETE FROM doc_range_generations_v2 WHERE doc_hash=$2 AND $1::text<>''`,
	} {
		transaction, err := fixture.storage.pool.Begin(fixture.ctx)
		if err != nil {
			close(release)
			t.Fatal(err)
		}
		if _, err := transaction.Exec(fixture.ctx, `SET LOCAL lock_timeout='30ms'`); err != nil {
			close(release)
			rollbackPG(transaction)
			t.Fatal(err)
		}
		_, err = transaction.Exec(fixture.ctx, statement, fixture.repo, fixture.doc.hash)
		rollbackPG(transaction)
		var postgresError interface{ SQLState() string }
		if !errors.As(err, &postgresError) || postgresError.SQLState() != "55P03" {
			close(release)
			t.Fatal("current source mutation not fenced", err)
		}
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestDocDisplayPGDatabaseAndReadBounds(t *testing.T) {
	fixture := docDisplayPG(t)
	fixture.enqueue(t, 0)
	job := fixture.claim(t)
	prepared := fixture.initialize(t, job)
	for _, mutation := range []string{"source", "checkpoint", "object", "generation"} {
		transaction, err := fixture.storage.pool.Begin(fixture.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer rollbackPG(transaction)
		switch mutation {
		case "source":
			_, err = transaction.Exec(fixture.ctx, `UPDATE doc_display_sources_v1 SET bytes=$2 WHERE source_hash=$1`, prepared.State.Source, bytes.Repeat([]byte("x"), domain.MaxDocDisplaySourceBytes+1))
		case "checkpoint":
			if _, err := transaction.Exec(fixture.ctx, `SELECT cxthub_bind_doc_catalog_control($1,$2,$3,$4,$5)`, job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile(), job.Version); err != nil {
				t.Fatal(err)
			}
			_, err = transaction.Exec(fixture.ctx, `UPDATE doc_display_jobs_v1 SET checkpoint=$2 WHERE repo_id=$1`, fixture.repo, bytes.Repeat([]byte("x"), domain.MaxDocDisplayStateBytes+1))
		case "object":
			_, err = transaction.Exec(fixture.ctx, `INSERT INTO doc_display_objects_v1(source_hash,hash,bytes) VALUES($1,$2,$3)`, prepared.State.Source, domain.HashContent(nil), bytes.Repeat([]byte("x"), domain.MaxDocDisplayPageBytes+1))
		case "generation":
			_, err = transaction.Exec(fixture.ctx, `INSERT INTO doc_display_generations_v1(event_hash,profile,source_hash,generation_hash,bytes) VALUES($1,$2,$3,$4,$5)`,
				job.Target.Event.Hash, domain.DocDisplayStringProfile, prepared.State.Source, domain.HashContent(nil), bytes.Repeat([]byte("x"), domain.MaxDocDisplaySourceBytes+1))
		}
		rollbackPG(transaction)
		var postgresError interface{ SQLState() string }
		if !errors.As(err, &postgresError) || postgresError.SQLState() != "23514" {
			t.Fatalf("unbounded %s persisted: %v", mutation, err)
		}
	}
	for _, index := range []int{-1, domain.MaxDocRangePages} {
		if _, err := fixture.storage.ReadDocDisplayPage(fixture.ctx, job.Target, index); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid page index accepted", err)
		}
	}
}
