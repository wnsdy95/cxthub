//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type normalizationPreparingPGAcknowledgement struct {
	Job       domain.DocRangeIndexJob
	Epoch     string
	Root      domain.ContentHash
	Parser    domain.ContentHash
	Preserved string
}

func normalizationPreparingPGDocument(repo domain.ContentHash) domain.CIRDocument {
	return domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: domain.CIRVersionV2, SourceProvider: domain.ProviderCodex,
			SessionOriginID: string(repo), Fidelity: domain.FidelityFull, Cwd: strings.Repeat("parser-progress🙂", 40000)},
		Events: []domain.CIREvent{
			{Kind: domain.EventTurn, Seq: 7},
			{Kind: domain.EventToolCall, Seq: 7, CallID: "preparing-restart", ToolName: "preparing-restart",
				Input: map[string]any{"items": make([]any, 5001)}},
			{Kind: domain.EventTurn, Seq: 8},
		},
	}
}

func TestNormalizationCheckpointPGPreparingColdRestart(test *testing.T) {
	if os.Getenv("CXT_NORMALIZATION_PREPARING_PRODUCER") == "1" {
		normalizationPreparingPGProducer(test)
		return
	}
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		test.Fatal("preparing restart requires an explicitly supplied existing owned local PostgreSQL server")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil || !processAdmissionLocalHost(config.Host) {
		test.Fatal("preparing restart requires a valid local PostgreSQL configuration")
	}
	for _, fallback := range config.Fallbacks {
		if !processAdmissionLocalHost(fallback.Host) {
			test.Fatal("preparing restart refuses a nonlocal PostgreSQL fallback")
		}
	}
	storage, ctx := rootProjectionIsolatedPG(test)
	if _, owned := docDisplayOwnedFixturesPG.Load(storage.pool.Config().ConnConfig.Database); !owned {
		test.Fatal("preparing restart requires its own isolated local database")
	}
	storage.ConfigureLegacyByteRangePublication(true)
	storage.ConfigureLegacyDerivedByteRangePublication(true)
	directory := rangeSpoolTempDir(test)
	producerDirectory := test.TempDir()
	repo, _ := docRangeJobPGRepo(test, storage, ctx)
	fixture := legacyByteStagePGDocument(test, storage, ctx, repo, normalizationPreparingPGDocument(repo))
	legacyByteRecoveryPGPreservationFixture(test, storage, ctx, fixture)
	executable, err := os.Executable()
	if err != nil {
		test.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		test.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := reader.SetReadDeadline(deadline); err != nil {
			test.Fatal(err)
		}
	}
	command := exec.CommandContext(ctx, executable, "-test.run=^TestNormalizationCheckpointPGPreparingColdRestart$", "-test.count=1", "-test.timeout=30s", "-test.v")
	command.Env = append(os.Environ(), "CXT_NORMALIZATION_PREPARING_PRODUCER=1", "CXT_NORMALIZATION_PREPARING_REPO="+string(repo),
		"CXT_NORMALIZATION_PREPARING_HASH="+string(fixture.hash), "TMPDIR="+producerDirectory, "TMP="+producerDirectory, "TEMP="+producerDirectory)
	command.ExtraFiles = []*os.File{writer}
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		test.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			command.Process.Kill()
			command.Wait()
		}
	}()
	writer.Close()
	var acknowledged normalizationPreparingPGAcknowledgement
	decodeErr := json.NewDecoder(reader).Decode(&acknowledged)
	killErr := command.Process.Kill()
	waitErr := command.Wait()
	waited = true
	if decodeErr != nil || killErr != nil {
		test.Fatalf("producer did not reach its durable ACK before interruption: decode=%v kill=%v wait=%v\n%s", decodeErr, killErr, waitErr, output.String())
	}
	status, ok := command.ProcessState.Sys().(syscall.WaitStatus)
	if waitErr == nil || !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		test.Fatal("producer was not killed without Close or deferred cleanup", waitErr, status)
	}
	if acknowledged.Job.Validate() != nil || acknowledged.Job.RepoID != repo || acknowledged.Job.DocHash != fixture.hash || acknowledged.Preserved == "" {
		test.Fatal("producer ACK identified a different fixture or omitted preservation evidence")
	}
	acknowledged.Job.RepositoryEpoch = acknowledged.Epoch
	state, found, err := storage.loadLegacyNormalizationCheckpoint(ctx, acknowledged.Job)
	if err != nil || !found || state.hash != acknowledged.Root || state.stage.plan != "" || state.stage.complete ||
		generationCountsPG(test, storage, ctx, fixture.hash) != [2]int{} {
		test.Fatal("SIGKILL lost the acknowledged processing root or published early", state, found, err)
	}
	if entries, err := os.ReadDir(producerDirectory); err != nil || len(entries) == 0 {
		test.Fatal("interrupted producer left no process-local spools to discard", entries, err)
	}
	if err := os.RemoveAll(producerDirectory); err != nil {
		test.Fatal(err)
	}
	if err := ctx.Err(); err != nil {
		test.Fatal("producer exceeded the unchanged setup deadline", err)
	}
	restart, cancel := context.WithTimeout(test.Context(), 30*time.Second)
	defer cancel()
	changed, err := storage.pool.Exec(restart, `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 minute'
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash='' AND state='running' AND version=$3`,
		acknowledged.Job.RepoID, acknowledged.Job.DocHash, acknowledged.Job.Version)
	if err != nil || changed.RowsAffected() != 1 {
		test.Fatal("failed to expire the killed producer's claim", changed.RowsAffected(), err)
	}
	encoded, err := json.Marshal(acknowledged.Job)
	if err != nil {
		test.Fatal(err)
	}
	consumer := exec.CommandContext(restart, executable, "-test.run=^TestNormalizationCheckpointPGColdChild$", "-test.count=1", "-test.timeout=30s", "-test.v")
	consumer.Env = append(os.Environ(), "CXT_NORMALIZATION_CHECKPOINT_CHILD=1", "CXT_NORMALIZATION_CHECKPOINT_PRIOR="+string(encoded),
		"CXT_NORMALIZATION_CHECKPOINT_EXPECTED_ROOT="+string(acknowledged.Root),
		"CXT_NORMALIZATION_CHECKPOINT_EXPECTED_PARSER="+string(acknowledged.Parser))
	if output, err := consumer.CombinedOutput(); err != nil {
		test.Fatalf("fresh claim could not resume the killed producer's preparing event: %v\n%s", err, output)
	}
	legacyByteRecoveryPGAssertPublished(test, storage, restart, fixture)
	if counts := generationCountsPG(test, storage, restart, fixture.hash); counts[0] != 1 || counts[1] == 0 ||
		legacyByteRecoveryPGPreservedImage(test, storage, restart, fixture) != acknowledged.Preserved {
		test.Fatal("preparing restart duplicated publication or changed original context/ref/memory", counts)
	}
	assertRangeSpoolsRemoved(test, directory)
}

func normalizationPreparingPGProducer(test *testing.T) {
	test.Helper()
	repo := domain.ContentHash(os.Getenv("CXT_NORMALIZATION_PREPARING_REPO"))
	hash := domain.ContentHash(os.Getenv("CXT_NORMALIZATION_PREPARING_HASH"))
	if domain.ValidateContentHash(repo) != nil || domain.ValidateContentHash(hash) != nil {
		test.Fatal("invalid preparing producer fixture")
	}
	storage, ctx := chunkReusePG(test)
	document := normalizationPreparingPGDocument(repo)
	stored, err := json.Marshal(document)
	if err != nil {
		test.Fatal(err)
	}
	stored = append(stored, '\n', ' ')
	raw, err := domain.CanonicalBytes(document)
	if err != nil || domain.HashContent(raw) != hash {
		test.Fatal("producer fixture differs from the independent canonical oracle", err)
	}
	var canonical struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(raw, &canonical); err != nil || len(canonical.Events) != 3 {
		test.Fatal("invalid preparing fixture event corpus", err)
	}
	fixture := legacyByteStagePGFixture{repo: repo, hash: hash}
	work, job, _ := docNormalizationPGStartSource(test, storage, ctx, fixture, stored)
	defer work.Close()
	preserved := legacyByteRecoveryPGPreservedImage(test, storage, ctx, fixture)
	normalizationParserRestartPGAwaitRoot(test, work, ctx, job, 0)
	input := work.checkpoint.root.Parser.Input
	trace := normalizationParserRepeatPGWatch(test, work, ctx, input, stored)
	type cursor struct {
		Count, Next, Pending, Written, Position int
		Phase                                   uint8
		Encoding, Preparation                   uintptr
		Inspected                               uint64
		Native                                  bool
	}
	observe := func() cursor {
		if work.normalization == nil || work.normalization.preparation == nil || work.normalization.prepared ||
			work.normalization.canonical != nil || work.canonicalProof.Valid() || work.work != nil || work.decoding == nil {
			test.Fatal("preparing cut escaped live provisional normalization")
		}
		preparation := reflect.ValueOf(work.normalization.preparation).Elem()
		decoder := preparation.FieldByName("decoder")
		if decoder.IsNil() {
			test.Fatal("preparing cut consumed the parser")
		}
		pending := preparation.FieldByName("pending")
		state := cursor{Count: int(preparation.FieldByName("count").Int()), Next: int(preparation.FieldByName("next").Int()),
			Pending: pending.Len(), Written: int(preparation.FieldByName("written").Int()),
			Position: int(decoder.Elem().FieldByName("position").Int()), Phase: uint8(decoder.Elem().FieldByName("phase").Uint())}
		if state.Next >= 0 && state.Next < pending.Len() {
			encoding := pending.Index(state.Next).FieldByName("Encoding")
			if !encoding.IsNil() {
				state.Encoding = encoding.Pointer()
				state.Preparation = encoding.Elem().FieldByName("preparation").Pointer()
				state.Inspected = encoding.Elem().FieldByName("inspectWork").Uint()
				state.Native = !encoding.Elem().FieldByName("nativeOwner").IsNil()
			}
		}
		return state
	}
	var before cursor
	normalizationCheckpointPGCut(test, work, ctx, job, func(checkpoint *legacyNormalizationCheckpointWork) bool {
		if checkpoint.busy() {
			return false
		}
		before = observe()
		if before.Count > 1 || before.Count == 1 && before.Written != 0 {
			test.Fatal("missed the current event's progressed zero-output preparation", before)
		}
		return before.Count == 1 && before.Preparation != 0 && before.Native && before.Inspected > 0
	})
	prior := work.checkpoint.state
	if before.Position >= len(stored) || work.checkpoint.root.Parser == nil || work.checkpoint.root.Parser.Input != input {
		test.Fatal("preparing cut lost the acknowledged input or its unconsumed suffix", before)
	}
	if err := work.checkpoint.begin(ctx, work.normalization.preparation); err != nil {
		test.Fatal("progressed source-backed preparation cannot be checkpointed before its first byte", err)
	}
	control, err := work.checkpoint.capture.Control(ctx)
	if err != nil || !control.Staging() || control.Written != 0 || len(control.HashState) != 0 || control.Count != 1 ||
		control.StagePending != before.Pending-before.Next || control.Active.Sequence != 7 || control.Active.Ordinal != 1 ||
		control.Active.Offset != uint64(len(canonical.Events[0])) || control.Files[0].Size != len(canonical.Events[0]) || control.Files[1].Size != 64 {
		test.Fatal("preparing capture lost its completed occurrence or unpublished current event", control, err)
	}
	completed, err := work.checkpoint.capture.ReadBuffer(ctx, 0)
	if err != nil || !bytes.Equal(completed, canonical.Events[0]) {
		test.Fatal("completed event bytes changed before capturing the next preparation", err)
	}
	if _, err := work.normalization.preparation.Step(ctx); !errors.Is(err, domain.ErrLegacyNormalizationBusy) {
		test.Fatal("preparing capture did not freeze semantic processing", err)
	}
	normalizationCheckpointPGCut(test, work, ctx, job, func(checkpoint *legacyNormalizationCheckpointWork) bool {
		resources, err := work.RetainedResources()
		if err != nil || resources.HeapBytes < 0 || resources.HeapBytes > 16<<20 || resources.SpoolBytes < 0 || observe() != before {
			test.Fatal("capture drained preparation, moved the parser, or exceeded the existing heap bound", resources, err)
		}
		return !checkpoint.busy() && checkpoint.state.hash != prior.hash
	})
	state, found, err := storage.loadLegacyNormalizationCheckpoint(ctx, job)
	if err != nil || !found || state != work.checkpoint.state || state.sequence != prior.sequence+1 || state.stage.plan != "" || state.stage.complete {
		test.Fatal("preparing Stage root was not independently acknowledged", state, found, err)
	}
	body, err := storage.readLegacyNormalizationCheckpointObject(ctx, job, state, state.hash, state.bytes)
	if err != nil {
		test.Fatal(err)
	}
	root, err := decodeLegacyNormalizationCheckpoint(body, state)
	if err != nil || !reflect.DeepEqual(root, work.checkpoint.root) || !reflect.DeepEqual(root.State, control) ||
		root.Version != 1 || root.Encoder == nil || root.Parser == nil || root.Parser.Input != input {
		test.Fatal("durable preparing root changed saved control or existing Stage/parser wire", err)
	}
	position, phase := normalizationParserRepeatPGPosition(test, storage, ctx, job, state, root.Parser.Ref)
	consumed, consumedErr := work.normalization.preparation.InputConsumed()
	if position != before.Position || phase != before.Phase || consumedErr != nil || consumed || observe() != before ||
		trace.objects.Load() == 0 || trace.bodyReads.Load() != 0 || trace.inputUploads.Load() != 0 || trace.inputReads.Load() != 0 {
		test.Fatal("capture advanced/drained the source or reread/recopied acknowledged input", position, phase, consumedErr,
			trace.objects.Load(), trace.bodyReads.Load(), trace.inputUploads.Load(), trace.inputReads.Load())
	}
	if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		test.Fatal("preparing checkpoint acquired premature publication authority", err)
	}
	ready := os.NewFile(3, "preparing-checkpoint-ack")
	if ready == nil {
		test.Fatal("missing parent ACK pipe")
	}
	if err := json.NewEncoder(ready).Encode(normalizationPreparingPGAcknowledgement{Job: job, Epoch: job.RepositoryEpoch, Root: state.hash, Parser: root.Parser.Ref.Hash, Preserved: preserved}); err != nil {
		test.Fatal(err)
	}
	ready.Close()
	<-ctx.Done()
	test.Fatal("producer was not SIGKILLed after ACK", ctx.Err())
}
