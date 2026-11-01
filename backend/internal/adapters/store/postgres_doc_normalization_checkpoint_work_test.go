//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const normalizationCheckpointPGStepLimit = docCaptureWorkPGPreparationStepLimit + 32*2301

func normalizationCheckpointPGCut(test *testing.T, work *preparedLegacyCaptureWork, ctx context.Context, job domain.DocRangeIndexJob, match func(*legacyNormalizationCheckpointWork) bool) {
	test.Helper()
	for step := 0; step < normalizationCheckpointPGStepLimit; step++ {
		if work.checkpoint != nil && match(work.checkpoint) {
			return
		}
		if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending || work.work != nil {
			test.Fatal("real normalization worker passed its requested checkpoint cut", step, progress, err)
		}
	}
	test.Fatal("real normalization worker did not reach its bounded checkpoint cut")
}

func normalizationCheckpointPGColdReclaim(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture, job domain.DocRangeIndexJob, work *preparedLegacyCaptureWork) {
	test.Helper()
	state, found, err := storage.loadLegacyNormalizationCheckpoint(ctx, job)
	if err != nil || !found || state.stage.plan != "" || state.stage.complete || generationCountsPG(test, storage, ctx, fixture.hash) != [2]int{} {
		test.Fatal("cold normalization must start before canonical staging/publication", state, found, err)
	}
	var expectedParser domain.ContentHash
	if work.checkpoint != nil && work.checkpoint.root.Parser != nil {
		expectedParser = work.checkpoint.root.Parser.Ref.Hash
	}
	if err := work.Close(); err != nil {
		test.Fatal(err)
	}
	if actual, found, err := storage.loadLegacyNormalizationCheckpoint(ctx, job); err != nil || !found || actual != state {
		test.Fatal("Close rewrote the committed processing checkpoint", actual, err)
	}
	changed, err := storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 minute'
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash='' AND state='running' AND version=$3`, job.RepoID, job.DocHash, job.Version)
	if err != nil || changed.RowsAffected() != 1 {
		test.Fatal("failed to expire the original worker claim", changed.RowsAffected(), err)
	}
	encoded, err := json.Marshal(job)
	if err != nil {
		test.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		test.Fatal(err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		test.Log("cold child inherited remaining parent window", time.Until(deadline))
	}
	command := exec.CommandContext(ctx, executable, "-test.run=^TestNormalizationCheckpointPGColdChild$", "-test.count=1", "-test.timeout=30s", "-test.v")
	command.Env = append(os.Environ(), "CXT_NORMALIZATION_CHECKPOINT_CHILD=1", "CXT_NORMALIZATION_CHECKPOINT_PRIOR="+string(encoded),
		"CXT_NORMALIZATION_CHECKPOINT_EXPECTED_ROOT="+string(state.hash))
	if expectedParser != "" {
		command.Env = append(command.Env, "CXT_NORMALIZATION_CHECKPOINT_EXPECTED_PARSER="+string(expectedParser))
	}
	if output, err := command.CombinedOutput(); err != nil {
		test.Fatalf("processing checkpoint cold child failed: %v\n%s", err, output)
	}
	legacyByteRecoveryPGAssertPublished(test, storage, ctx, fixture)
	public := legacyByteRecoveryPGPublicationImage(test, storage, ctx, fixture)
	if stale, err := work.store.PrepareDocRangeWork(ctx, job); stale != nil || !errors.Is(err, domain.ErrConflict) {
		test.Fatal("the replaced worker reclaimed publication authority", stale, err)
	}
	if legacyByteRecoveryPGPublicationImage(test, storage, ctx, fixture) != public {
		test.Fatal("rejected duplicate preparation changed publication or the completed job")
	}
}

func TestNormalizationCheckpointPGColdRestartPublishes(test *testing.T) {
	for _, cut := range []string{"merge", "compose", "verify"} {
		test.Run(cut, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			directory := rangeSpoolTempDir(test)
			repo, _ := docRangeJobPGRepo(test, storage, ctx)
			document := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex,
				SessionOriginID: string(repo), Fidelity: domain.FidelityFull, Cwd: strings.Repeat("header", 20000)}}
			for ordinal := 0; ordinal < 2301; ordinal++ {
				document.Events = append(document.Events, domain.CIREvent{Kind: domain.EventMessage, Seq: 2301 - ordinal, Role: domain.RoleAssistant,
					Blocks: []domain.ContentBlock{{Type: "text", Text: fmt.Sprintf("event-%d", ordinal)}}})
			}
			document.Events[0].Blocks[0].Text = strings.Repeat("large-event🙂", 100000)
			fixture := legacyByteStagePGDocument(test, storage, ctx, repo, document)
			legacyByteRecoveryPGPreservationFixture(test, storage, ctx, fixture)
			stored, err := json.MarshalIndent(document, "", " ")
			if err != nil {
				test.Fatal(err)
			}
			work, job, _ := docNormalizationPGStartSource(test, storage, ctx, fixture, stored)
			preserved := legacyByteRecoveryPGPreservedImage(test, storage, ctx, fixture)
			defer work.Close()
			normalizationCheckpointPGCut(test, work, ctx, job, func(checkpoint *legacyNormalizationCheckpointWork) bool {
				if checkpoint.busy() || checkpoint.state.hash == "" {
					return false
				}
				control := checkpoint.root.State
				switch cut {
				case "merge":
					return control.Width > 0 && control.Position > control.Base && control.Position < min(control.Base+2*control.Width, control.Count) &&
						(control.Left != 0 || control.Right != 0)
				case "compose":
					return control.Written > 0 && control.Files[3].BufferBytes > 0
				default:
					return control.Verified > 0 && control.Verified < control.Files[3].Parts
				}
			})
			if work.decoding != nil || work.source != nil || work.normalization.canonical != nil {
				test.Fatal("producer retained its original decoder or advanced to proof at the requested cut")
			}
			normalizationCheckpointPGColdReclaim(test, storage, ctx, fixture, job, work)
			if legacyByteRecoveryPGPreservedImage(test, storage, ctx, fixture) != preserved {
				test.Fatal("processing recovery changed original conversation/memory/ref/publication identity")
			}
			if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
				test.Fatal("producer or cold consumer leaked process-local spools", len(entries), err)
			}
		})
	}
}

func TestNormalizationCheckpointPGRootCommitFailureAndLostResponse(test *testing.T) {
	for _, fault := range []string{"final-fence", "lost-response"} {
		test.Run(fault, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			rangeSpoolTempDir(test)
			fixture := legacyByteStagePGSource(test, storage, ctx, 768<<10)
			work, job, trace := docNormalizationPGStart(test, storage, ctx, fixture)
			defer work.Close()
			normalizationCheckpointPGCut(test, work, ctx, job, func(checkpoint *legacyNormalizationCheckpointWork) bool {
				return checkpoint.state.hash != "" && !checkpoint.busy()
			})
			prior := work.checkpoint.state
			normalizationCheckpointPGCut(test, work, ctx, job, func(checkpoint *legacyNormalizationCheckpointWork) bool {
				return checkpoint.capture != nil && checkpoint.pending.hash != ""
			})
			if _, err := work.normalization.preparation.Step(ctx); !errors.Is(err, domain.ErrLegacyNormalizationBusy) || work.normalization.canonical != nil {
				test.Fatal("uncommitted checkpoint did not freeze semantic processing and proof", err)
			}
			before := legacyNormalizationGCPGImage(test, storage, ctx, job)
			if fault == "final-fence" {
				name := pgx.Identifier{domain.NewID("normalization_ack_fence_")}.Sanitize()
				statement := fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.doc_hash='%s' AND NEW.checkpoint_sequence>OLD.checkpoint_sequence THEN
 UPDATE doc_range_jobs_v1 SET state='waiting',version=version+1,lease_until=NULL WHERE repo_id='%s' AND doc_hash=NEW.doc_hash;
 END IF; RETURN NEW; END; $$;
 CREATE TRIGGER %s AFTER UPDATE ON doc_legacy_byte_stages_v1 FOR EACH ROW EXECUTE FUNCTION %s()`, name, fixture.hash, fixture.repo, name, name)
				if _, err := storage.pool.Exec(ctx, statement); err != nil {
					test.Fatal(err)
				}
				progress, failure := work.Step(ctx, job)
				if _, err := storage.pool.Exec(ctx, `DROP TRIGGER `+name+` ON doc_legacy_byte_stages_v1; DROP FUNCTION `+name+`() `); err != nil {
					test.Fatal(err)
				}
				if progress != 0 || !errors.Is(failure, domain.ErrConflict) || work.checkpoint.state != prior ||
					legacyNormalizationGCPGImage(test, storage, ctx, job) != before {
					test.Fatal("failed root transaction changed prior root, references, accounting, cursor, or job", progress, failure)
				}
			} else {
				selected, cancel := context.WithCancel(ctx)
				committed := false
				commits := 0
				trace.commit.Store(&docCaptureCommitHook{run: func() {
					commits++
					if commits == 2 {
						committed = true
						trace.commit.Store(nil)
						cancel()
					}
				}})
				progress, failure := work.Step(selected, job)
				trace.commit.Store(nil)
				cancel()
				actual, found, err := storage.loadLegacyNormalizationCheckpoint(ctx, job)
				if !committed || progress != 0 || !errors.Is(failure, context.Canceled) || err != nil || !found ||
					actual.sequence != prior.sequence+1 || work.normalization.canonical != nil {
					test.Fatal("application response loss did not leave one durable root without subsequent proof", committed, progress, failure, actual, err)
				}
				pending := work.checkpoint.pending
				if pending.hash == "" {
					body, err := storage.readLegacyNormalizationCheckpointObject(ctx, job, actual, actual.hash, actual.bytes)
					if err != nil {
						test.Fatal(err)
					}
					pending = legacyByteCheckpointObject{hash: actual.hash, data: body, structure: 2}
				}
				attempt := &legacyNormalizationCheckpointWork{store: storage, job: job, state: prior, uncertain: true}
				if acknowledged, err := attempt.commit(ctx, pending); err != nil || acknowledged != actual {
					test.Fatal("reconciling the committed attempt minted another root or lost its ACK", acknowledged, err)
				}
			}
			normalizationCheckpointPGColdReclaim(test, storage, ctx, fixture, job, work)
		})
	}
}

func TestNormalizationCheckpointPGColdChild(test *testing.T) {
	if os.Getenv("CXT_NORMALIZATION_CHECKPOINT_CHILD") != "1" {
		test.Skip("only the processing-checkpoint parent launches this child")
	}
	started := time.Now()
	var prior domain.DocRangeIndexJob
	if err := json.Unmarshal([]byte(os.Getenv("CXT_NORMALIZATION_CHECKPOINT_PRIOR")), &prior); err != nil || prior.Validate() != nil {
		test.Fatal("invalid prior claim", err)
	}
	storage, base := chunkReusePG(test)
	peer, trace := docCaptureWorkPGStore(test, storage, base)
	job := docRangeJobPGClaim(test, peer, base, prior.RepoID, prior.DocHash)
	if job.Version != prior.Version+1 || job.Attempts != prior.Attempts+1 || !job.CreatedAt.Equal(prior.CreatedAt) {
		test.Fatal("new process did not reclaim the same durable job", prior, job)
	}
	ctx := &legacyByteRecoveryPGVerificationContext{Context: base}
	prepared, err := peer.PrepareDocRangeWork(ctx, job)
	if err != nil || prepared == nil {
		test.Fatal("new process could not prepare its processing checkpoint", err)
	}
	defer prepared.Close()
	work, ok := prepared.(*preparedLegacyCaptureWork)
	if !ok || work.checkpoint == nil || !work.checkpoint.restoring || work.source != nil || work.decoding != nil || work.normalization == nil {
		test.Fatalf("new process repeated original capture/decode or selected completed-stage recovery: %T", prepared)
	}
	if expected := os.Getenv("CXT_NORMALIZATION_CHECKPOINT_EXPECTED_ROOT"); expected != "" && work.checkpoint.state.hash != domain.ContentHash(expected) {
		test.Fatal("new process did not select the latest acknowledged root", work.checkpoint.state.hash, expected)
	}
	if expected := os.Getenv("CXT_NORMALIZATION_CHECKPOINT_EXPECTED_PARSER"); expected != "" &&
		(work.checkpoint.root.Parser == nil || work.checkpoint.root.Parser.Ref.Hash != domain.ContentHash(expected)) {
		test.Fatal("new process selected an earlier parser checkpoint", expected)
	}
	progress := outbound.DocRangeWorkPending
	expectedControl := work.checkpoint.root.State
	restoredControlChecked := false
	for step := 0; step < normalizationCheckpointPGStepLimit && progress == outbound.DocRangeWorkPending; step++ {
		if step%1024 == 0 {
			test.Log("cold worker progress", step, "elapsed", time.Since(started), "queries", trace.queries.Load(), "restoring", work.checkpoint != nil && work.checkpoint.restoring, "canonical", work.work != nil)
			if checkpoint := work.checkpoint; checkpoint != nil {
				test.Log("cold worker checkpoint", checkpoint.state.sequence, "capturing", checkpoint.capture != nil, "part", checkpoint.position, "decode steps", work.parserSteps)
			}
		}
		if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
			test.Fatal("processing state or restored control minted publication authority", step, err)
		}
		resources, err := work.RetainedResources()
		if err != nil || resources.HeapBytes < 0 || resources.SpoolBytes < 0 || resources.HeapBytes > 16<<20 {
			test.Fatal("processing restore lost aggregate resource accounting", resources, err)
		}
		progress, err = work.Step(ctx, job)
		if err != nil || progress == outbound.DocRangeWorkCompatibilityRequired {
			if downstream := work.work; downstream != nil {
				test.Log("downstream stage", downstream.session.stage, "header bytes", downstream.header.EnvelopeBytes(),
					"preparation", downstream.preparation != nil, "recovery", downstream.recoveryReady, "metadata", downstream.metadata.Valid())
			}
			if checkpoint := work.checkpoint; checkpoint != nil {
				test.Log("checkpoint", checkpoint.root.State.Phase, checkpoint.next.State.Phase, checkpoint.position, "capture", checkpoint.capture != nil)
			}
			test.Fatal("processing restore failed its bounded pipeline", step, progress, err)
		}
		if !restoredControlChecked && work.checkpoint != nil && !work.checkpoint.restoring {
			capture, err := work.normalization.preparation.BeginCheckpoint(ctx)
			if err != nil {
				test.Fatal(err)
			}
			actual, err := capture.Control(ctx)
			capture.Close()
			if err != nil || !reflect.DeepEqual(actual, expectedControl) || work.normalization.canonical != nil {
				test.Fatal("new process replayed from an earlier point or altered saved sort/event/verify cursors", actual, expectedControl, err)
			}
			restoredControlChecked = true
			test.Log("cold worker restored exact control", step, "elapsed", time.Since(started), "queries", trace.queries.Load())
		}
	}
	if progress != outbound.DocRangeWorkPublishable || !restoredControlChecked || trace.bodyReads.Load() != 0 || !ctx.preparation.Load() || !ctx.step.Load() || !ctx.event.Load() {
		test.Fatal("new process skipped fresh verification or reread the original source", progress, trace.bodyReads.Load(), ctx.preparation.Load(), ctx.step.Load(), ctx.event.Load())
	}
	if err := work.Publish(ctx, job); err != nil {
		test.Fatal("new process could not atomically publish its reverified result", err)
	}
	fixture := legacyByteStagePGFixture{repo: job.RepoID, hash: job.DocHash}
	public := legacyByteRecoveryPGPublicationImage(test, peer, base, fixture)
	if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("completed claim published twice", err)
	}
	if trace.bodyReads.Load() != 0 || legacyByteRecoveryPGPublicationImage(test, peer, base, fixture) != public {
		test.Fatal("duplicate publication changed the generation or reread original bytes")
	}
	if err := work.Close(); err != nil {
		test.Fatal(err)
	}
}
