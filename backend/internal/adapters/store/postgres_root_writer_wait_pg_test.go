//go:build postgres

package store

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type rootWriterWaitFenceKeyPG struct{}

type rootWriterWaitTracePG struct {
	reached chan struct{}
	release chan struct{}
	once    sync.Once
	pid     uint32
	pending bool
}

func (trace *rootWriterWaitTracePG) TraceQueryStart(ctx context.Context, connection *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	statement := strings.ToLower(strings.Join(strings.Fields(data.SQL), " "))
	if strings.Contains(statement, "from doc_finalization_jobs") && strings.Contains(statement, "for update") {
		return context.WithValue(ctx, rootWriterWaitFenceKeyPG{}, true)
	}
	return ctx
}

func (trace *rootWriterWaitTracePG) TraceQueryEnd(ctx context.Context, connection *pgx.Conn, data pgx.TraceQueryEndData) {
	if data.Err != nil || ctx.Value(rootWriterWaitFenceKeyPG{}) != true {
		return
	}
	trace.once.Do(func() {
		trace.pid = connection.PgConn().PID()
		trace.pending = true
		defer func() { trace.pending = false }()
		close(trace.reached)
		select {
		case <-trace.release:
		case <-ctx.Done():
		}
	})
}

func rootWriterWaitFixturePG(test *testing.T, sample int) rootReaderFixture {
	test.Helper()
	fixture := rootFixture(test, "")
	fixture.cir.Envelope.SessionOriginID = fmt.Sprintf("rf13-lp1-root-writer-wait-8192-sample-%d", sample)
	fixture.cir.Events = make([]domain.CIREvent, 0, 8192)
	payload := strings.Repeat("x", 1024)
	for position := range 8192 {
		text := fmt.Sprintf("event-%04d ", position) + payload
		event := domain.CIREvent{Seq: position + 1, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}
		if position%4 != 0 {
			event.Kind, event.Role, event.Blocks = domain.EventToolResult, "", nil
			event.CallID, event.Output = fmt.Sprintf("tool-%d", position), text
		}
		fixture.cir.Events = append(fixture.cir.Events, event)
	}
	var err error
	fixture.manifest, fixture.bodies, err = domain.ConversationManifestForCIR(fixture.cir)
	if err != nil {
		test.Fatal(err)
	}
	fixture.hash, err = domain.ConversationManifestHash(fixture.manifest)
	if err != nil {
		test.Fatal(err)
	}
	fixture.manifestBytes, err = domain.CanonicalConversationManifest(fixture.manifest)
	if err != nil {
		test.Fatal(err)
	}
	fixture.canonical, err = domain.CanonicalBytes(fixture.cir)
	if err != nil {
		test.Fatal(err)
	}
	return fixture
}

func TestPGRootWriterWait(test *testing.T) {
	if os.Getenv("CXT_ROOT_WRITER_COST") != "1" {
		test.Skip("set CXT_ROOT_WRITER_COST=1 for owned local PostgreSQL writer-wait samples")
	}
	test.Log("RF13-LP1: copy this unchanged fixture to baseline/current; 8192 events x 3 distinct roots; deterministic corpus, fresh owned databases and preconnected independent backends")
	test.Log("raw per-job after-release observations only; setup, preparation and deliberate barrier/lock-observation delay excluded; not total lock-hold latency, mean, p95 or a hardware-capacity claim; no allocation measurement")
	roots := make(map[domain.ContentHash]bool)
	for sample := 1; sample <= 3; sample++ {
		test.Run(fmt.Sprintf("8192-events/sample-%d", sample), func(test *testing.T) {
			corpus := rootWriterWaitFixturePG(test, sample)
			if roots[corpus.hash] {
				test.Fatal("each sample must publish a distinct root")
			}
			roots[corpus.hash] = true
			fixture, storage := authorityScopePG(test)
			ctx := rootPublicationContext(fixture.ctx)
			if _, finite := ctx.Deadline(); !finite {
				test.Fatal("writer-wait fixture must retain its finite parent deadline")
			}
			_, repo := authorityScopeRepositoryPG(test, ctx, storage)
			job, _ := rootPublicationClaimPG(test, storage, ctx, corpus, repo)
			trace := &rootWriterWaitTracePG{reached: make(chan struct{}), release: make(chan struct{})}
			publisher := authorityScopePeerPG(test, storage, "", trace)
			observer := authorityScopePeerPG(test, storage, "", nil)
			rootPID := authorityScopePIDPG(test, ctx, publisher)
			otherPID := authorityScopePIDPG(test, ctx, observer)
			var observationPID uint32
			if err := fixture.observer.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&observationPID); err != nil {
				test.Fatal(err)
			}
			if rootPID == otherPID || observationPID == rootPID || observationPID == otherPID {
				test.Fatal("publisher, competing writer and lock observer must use independent PostgreSQL backends")
			}
			directory := rangeSpoolTempDir(test)
			publication := prepareRootStreamingPG(test, publisher, ctx, job, directory)
			assertRootStreamingUnpublishedPG(test, storage, ctx, job)
			test.Logf("fixture sample=%d events=%d root_hash=%s canonical_hash=%s canonical_bytes=%d manifest_bytes=%d manifest_chunks=%d unique_chunks=%d corpus=75pct_tool_results_1KiB_text",
				sample, len(corpus.cir.Events), corpus.hash, domain.HashContent(corpus.canonical), len(corpus.canonical), len(corpus.manifestBytes), len(corpus.manifest.Chunks), len(corpus.bodies))
			work, cancel := context.WithCancel(ctx)
			var resume sync.Once
			unpause := func() { resume.Do(func() { close(trace.release) }) }
			rootDone, otherDone := make(chan error, 1), make(chan error, 1)
			rootJoined := make(chan struct{})
			var otherJoined chan struct{}
			var rootCompleted, otherResumed, otherCompleted time.Time
			defer func() {
				cancel()
				unpause()
				<-rootJoined
				if otherJoined != nil {
					<-otherJoined
				}
				if trace.pending {
					test.Error("root job-fence callback remained pending after cancellation and join")
				}
			}()
			go func() {
				defer close(rootJoined)
				err := publication.Complete(work, job, time.Now())
				rootCompleted = time.Now()
				rootDone <- err
			}()
			authorityScopeReadyPG(test, work, trace.reached, rootDone)
			if trace.pid != rootPID {
				test.Fatal("job-fence QueryEnd came from a different publisher backend")
			}
			description := fmt.Sprintf("RF13-LP1 competing writer sample %d", sample)
			website := "https://example.test/rf13-lp1"
			topics := []string{"rf13-lp1", "same-repository"}
			otherJoined = make(chan struct{})
			go func() {
				defer close(otherJoined)
				err := observer.WithinRepository(work, repo, func(writeCtx context.Context) error {
					otherResumed = time.Now()
					return observer.UpdateRepoAbout(writeCtx, repo, description, website, topics)
				})
				otherCompleted = time.Now()
				otherDone <- err
			}()
			authorityScopeWaitPG(test, work, fixture.observer, rootPID, otherPID, "pg_advisory_xact_lock", otherDone)
			var advisoryWait bool
			if err := fixture.observer.QueryRow(work, `SELECT wait_event_type='Lock' AND wait_event='advisory'
 AND $1::integer=ANY(pg_blocking_pids(pid))
 AND EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND mode='ExclusiveLock' AND granted)
 AND EXISTS(SELECT 1 FROM pg_locks WHERE pid=$2 AND locktype='advisory' AND mode='ExclusiveLock' AND NOT granted)
 FROM pg_stat_activity WHERE pid=$2`, rootPID, otherPID).Scan(&advisoryWait); err != nil || !advisoryWait {
				test.Fatal("exact competing backend was not advisory-blocked by the paused root backend", advisoryWait, err)
			}
			released := time.Now()
			unpause()
			authorityScopeDonePG(test, work, rootDone)
			authorityScopeDonePG(test, work, otherDone)
			<-rootJoined
			<-otherJoined
			if otherResumed.IsZero() || otherResumed.Before(released) || rootCompleted.Before(released) || otherCompleted.Before(otherResumed) {
				test.Fatal("publication or competing write escaped the observed barrier")
			}
			test.Logf("root_writer_wait sample=%d events=8192 root_pid=%d other_pid=%d observation_pid=%d advisory_wait_verified=true root_complete_after_release_ns=%d competing_write_complete_after_release_ns=%d competing_write_resume_after_release_ns=%d",
				sample, rootPID, otherPID, observationPID, rootCompleted.Sub(released), otherCompleted.Sub(released), otherResumed.Sub(released))
			done, err := storage.GetDocJob(ctx, repo, job.ID)
			if err != nil || done.State != "completed" || done.DocumentRef() != job.DocumentRef() || done.Version != job.Version || done.RepositoryEpoch != job.RepositoryEpoch || !done.LeaseUntil.IsZero() || !bytes.Equal(done.RootManifest, job.RootManifest) {
				test.Fatal("committed root receipt lost its original identity, lease fence or manifest", err)
			}
			owned, err := storage.HasDocs(ctx, repo, []domain.ContentHash{job.DocHash})
			if err != nil || !slices.Equal(owned, []domain.ContentHash{job.DocHash}) {
				test.Fatal("root ownership did not commit", err)
			}
			document, err := storage.ReadVerifiedDoc(ctx, repo, job.DocHash)
			if err != nil {
				test.Fatal("committed original root could not be read", err)
			}
			assertRootProof(test, document, corpus)
			if err := storage.WithinReadSnapshot(ctx, func(readCtx context.Context) error {
				visible, err := storage.OpenDocVisibleGeneration(readCtx, repo, job.DocHash)
				if err != nil {
					return err
				}
				descriptor := visible.Descriptor()
				if descriptor.Hash != job.DocHash || descriptor.Events != len(corpus.cir.Events) {
					return fmt.Errorf("visible generation does not match the committed original root")
				}
				for _, mode := range []string{"all", "chat", "prompts"} {
					view, exists := descriptor.Views[mode]
					if !exists || view.Units == 0 {
						return fmt.Errorf("committed visible generation has no %s units", mode)
					}
					for _, position := range []int{0, view.Units - 1} {
						if _, _, err := visible.Unit(readCtx, mode, position, false); err != nil {
							return err
						}
					}
				}
				return nil
			}); err != nil {
				test.Fatal("visible publication did not commit", err)
			}
			metadata, err := storage.GetRepo(ctx, repo)
			if err != nil || metadata.RequiredDocIdentity != domain.DocumentIdentityRootV1 || metadata.Description != description || metadata.Website != website || !slices.Equal(metadata.Topics, topics) {
				test.Fatal("actual competing UpdateRepoAbout did not commit or changed root identity", err)
			}
		})
	}
}
