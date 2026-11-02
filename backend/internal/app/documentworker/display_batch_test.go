package documentworker

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type displayBatchTestChunks struct {
	fixture *wave38DisplayFixture
	calls   int
	fail    domain.ContentHash
	failure error
}

func (chunks *displayBatchTestChunks) ReadConversationChunk(ctx context.Context, repo, hash domain.ContentHash, length int64) ([]byte, error) {
	chunks.calls++
	if hash == chunks.fail {
		return nil, chunks.failure
	}
	return chunks.fixture.ReadConversationChunk(ctx, repo, hash, length)
}

type displayBatchTestStore struct {
	*wave38DisplayStore
	fixture    *wave38DisplayFixture
	sources    *wave38DisplaySources
	beforeSave func(int) error
	saved      []domain.DocDisplayStringStep
}

func (storage *displayBatchTestStore) SaveDocDisplayStep(ctx context.Context, job domain.DocDisplayJob, checkpoint domain.ContentHash, step domain.DocDisplayStringStep) (domain.DocDisplayPreparation, error) {
	storage.scope.requireWrite(ctx)
	if _, err := storage.sources.last.Read(0, 1); !errors.Is(err, context.Canceled) {
		storage.scope.test.Fatal("source remained live during save", err)
	}
	if storage.beforeSave != nil {
		if err := storage.beforeSave(len(storage.saved)); err != nil {
			return domain.DocDisplayPreparation{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return domain.DocDisplayPreparation{}, err
	}
	if job.Target.Binding != storage.fixture.binding {
		return domain.DocDisplayPreparation{}, domain.ErrConflict
	}
	if err := domain.ValidateDocDisplayStringStep(ctx, *storage.progress.Source, storage.progress.State, step); err != nil {
		return domain.DocDisplayPreparation{}, err
	}
	progress, err := storage.wave38DisplayStore.SaveDocDisplayStep(ctx, job, checkpoint, step)
	if err == nil {
		storage.saved = append(storage.saved, step)
	}
	return progress, err
}

type displayBatchTestScope struct {
	*wave38DisplayScope
	afterRead func()
}

func (scope *displayBatchTestScope) WithinRepositoryRead(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	err := scope.wave38DisplayScope.WithinRepositoryRead(ctx, repo, read)
	if scope.afterRead != nil {
		scope.afterRead()
	}
	return err
}

func newDisplayBatchTest(test *testing.T, output string) (*DisplayWorker, *displayBatchTestStore, *displayBatchTestChunks, []byte) {
	test.Helper()
	fixture, _ := newWave38DisplayFixture(test, []domain.CIREvent{{Kind: domain.EventToolResult, Output: output}})
	chunks := &displayBatchTestChunks{fixture: fixture}
	queries, err := documentquery.New(documentquery.Config{Profile: documentquery.LegacyCompatibility, Scope: fixture.scope, Snapshots: fixture, Legacy: fixture,
		View: &documentquery.ViewDependencies{Range: fixture, RangeGenerations: fixture, Chunks: chunks, Resources: fixture.scope}})
	if err != nil {
		test.Fatal(err)
	}
	sources := &wave38DisplaySources{queries: queries, scope: fixture.scope}
	target := fixture.target(0, "")
	var stream []byte
	for _, chunk := range fixture.manifest.Chunks {
		stream = append(stream, fixture.bodies[chunk.Hash]...)
	}
	raw := stream[target.Event.Offset : target.Event.Offset+target.Event.Length]
	source, err := domain.BuildDocDisplayStringSourceReader(context.Background(), target.Event.Hash, len(raw), bytes.NewReader(raw))
	if err != nil {
		test.Fatal(err)
	}
	storage := &displayBatchTestStore{wave38DisplayStore: &wave38DisplayStore{scope: fixture.scope, job: domain.DocDisplayJob{
		DocRangeIndexJob: domain.DocRangeIndexJob{RepoID: fixture.repo, DocHash: fixture.hash}, Target: target}}, fixture: fixture, sources: sources}
	if _, err := storage.InitializeDocDisplay(context.Background(), storage.job, source); err != nil {
		test.Fatal(err)
	}
	return NewDisplayWorker(storage, sources, fixture.scope), storage, chunks, raw
}

func displayBatchTestSteps(test *testing.T, source domain.DocDisplayStringSource, state domain.DocDisplayStringState, count int, load func(context.Context, int, int) ([]byte, error)) ([]domain.DocDisplayStringStep, error) {
	test.Helper()
	var steps []domain.DocDisplayStringStep
	for len(steps) < count && !state.Done {
		step, err := domain.StepDocDisplayString(context.Background(), source, state, load)
		if err != nil {
			return steps, err
		}
		steps = append(steps, step)
		state = step.State
	}
	return steps, nil
}

func TestDisplayBatchReadScopeExactStepsAndResume(test *testing.T) {
	for _, output := range []string{strings.Repeat("a", 10*domain.DocDisplayPageTextBytes), strings.Repeat("a\U0001f642<&>\n", 2_000), "last page"} {
		test.Run(string(domain.HashContent([]byte(output))), func(test *testing.T) {
			worker, storage, chunks, raw := newDisplayBatchTest(test, output)
			for batch := 0; batch < 10; batch++ {
				prior := storage.progress.State
				expected, err := displayBatchTestSteps(test, *storage.progress.Source, prior, DisplayJobSteps, func(_ context.Context, offset, length int) ([]byte, error) { return raw[offset : offset+length], nil })
				if err != nil {
					test.Fatal(err)
				}
				beforeReads, beforeChunks, beforeSaved := storage.scope.reads, chunks.calls, len(storage.saved)
				done, err := NewDisplayWorker(storage, worker.sources, worker.scope).Build(context.Background(), storage.job)
				if err != nil || storage.scope.reads-beforeReads != 1 || chunks.calls-beforeChunks != 1 || !reflect.DeepEqual(storage.saved[beforeSaved:], expected) {
					test.Fatal("batched read changed single-step oracle, source opens or chunk reuse", err, storage.scope.reads-beforeReads, chunks.calls-beforeChunks)
				}
				if len(expected) > DisplayJobSteps || done != storage.progress.State.Done {
					test.Fatal("batch changed step/completion bounds")
				}
				if done {
					if storage.completions != 1 {
						test.Fatal("completion repeated", storage.completions)
					}
					return
				}
			}
			test.Fatal("bounded restarts did not complete")
		})
	}
}

func TestDisplayBatchWriteFailurePreservesOnlyCommittedPrefix(test *testing.T) {
	for _, failureAt := range []int{0, 1, DisplayJobSteps - 1} {
		for _, changeSource := range []bool{false, true} {
			worker, storage, _, raw := newDisplayBatchTest(test, strings.Repeat("a", 10*domain.DocDisplayPageTextBytes))
			failure := errors.New("save failed")
			if changeSource {
				failure = domain.ErrConflict
			}
			storage.beforeSave = func(index int) error {
				if index != failureAt {
					return nil
				}
				if changeSource {
					storage.fixture.binding.SourceVersion++
					return nil
				}
				return failure
			}
			done, err := worker.Build(context.Background(), storage.job)
			if done || !errors.Is(err, failure) || len(storage.saved) != failureAt || storage.progress.State.Directory.Count != failureAt || storage.completions != 0 {
				test.Fatal("failed write lost its durable prefix or saved a suffix", done, err, len(storage.saved))
			}
			if changeSource {
				continue
			}
			storage.beforeSave = nil
			prior := storage.progress.State
			expected, err := displayBatchTestSteps(test, *storage.progress.Source, prior, DisplayJobSteps, func(_ context.Context, offset, length int) ([]byte, error) { return raw[offset : offset+length], nil })
			if err != nil {
				test.Fatal(err)
			}
			if _, err := NewDisplayWorker(storage, worker.sources, worker.scope).Build(context.Background(), storage.job); err != nil || !reflect.DeepEqual(storage.saved[failureAt:], expected) {
				test.Fatal("restart used speculative rather than committed progress", err)
			}
		}
	}
}

func TestDisplayBatchReadFailureAndCloseFences(test *testing.T) {
	worker, storage, chunks, raw := newDisplayBatchTest(test, strings.Repeat("a", 1<<20))
	load := func(_ context.Context, offset, length int) ([]byte, error) { return raw[offset : offset+length], nil }
	prefix, err := displayBatchTestSteps(test, *storage.progress.Source, storage.progress.State, 13, load)
	if err != nil {
		test.Fatal(err)
	}
	storage.progress.State = prefix[len(prefix)-1].State
	encoded, err := storage.progress.State.Encode(*storage.progress.Source)
	if err != nil {
		test.Fatal(err)
	}
	storage.progress.Checkpoint = domain.HashContent(encoded)
	failure := errors.New("second source chunk unavailable")
	chunks.fail, chunks.failure = storage.fixture.manifest.Chunks[1].Hash, failure
	expected, err := displayBatchTestSteps(test, *storage.progress.Source, storage.progress.State, DisplayJobSteps, func(ctx context.Context, offset, length int) ([]byte, error) {
		if storage.job.Target.Event.Offset+offset+length > domain.ConversationManifestChunkBytes {
			return nil, failure
		}
		return load(ctx, offset, length)
	})
	if !errors.Is(err, failure) || len(expected) == 0 || len(expected) >= DisplayJobSteps {
		test.Fatal("fixture did not fail after a nonempty bounded prefix", len(expected), err)
	}
	if done, err := worker.Build(context.Background(), storage.job); done || !errors.Is(err, failure) || !reflect.DeepEqual(storage.saved, expected) {
		test.Fatal("read failure lost verified prefix or original error", done, err, len(storage.saved))
	}
	for _, cancelAfterRead := range []bool{false, true} {
		worker, storage, _, _ := newDisplayBatchTest(test, strings.Repeat("a", 10*domain.DocDisplayPageTextBytes))
		ctx, cancel := context.WithCancel(context.Background())
		failure := errors.New("read close failed")
		if cancelAfterRead {
			worker.scope = &displayBatchTestScope{wave38DisplayScope: storage.scope, afterRead: cancel}
			failure = context.Canceled
		} else {
			storage.scope.afterRead = failure
		}
		done, err := worker.Build(ctx, storage.job)
		cancel()
		if done || !errors.Is(err, failure) || len(storage.saved) != 0 || storage.progress.State.Directory.Count != 0 || storage.completions != 0 {
			test.Fatal("failed read scope leaked staged writes", done, err, len(storage.saved))
		}
	}
}

func TestDisplayBatchRetainedStepCapacityBound(test *testing.T) {
	for _, count := range []int{0, 127, 16383, 1_000_000} {
		source := domain.DocDisplayStringSource{Version: 1, Profile: domain.DocDisplayStringProfile, EventHash: domain.HashContent([]byte("bounded virtual source")), EventBytes: domain.MaxConversationDocumentBytes, Token: domain.DocFindSpan{From: 10, To: domain.MaxConversationDocumentBytes - 1}}
		state, err := domain.NewDocDisplayStringState(source)
		if err != nil {
			test.Fatal(err)
		}
		if count > 0 {
			state.Directory.Count, state.TextBytes, state.HasContent = count, count*domain.DocDisplayPageRuns, true
			state.Next = source.Token.From + 1 + state.TextBytes
			for remaining := count; remaining > 0; remaining /= domain.DocRangeDirectoryFanout {
				level := make([]domain.ContentHash, remaining%domain.DocRangeDirectoryFanout)
				for index := range level {
					level[index] = domain.HashContent([]byte("bounded frontier"))
				}
				state.Directory.Levels = append(state.Directory.Levels, level)
			}
		}
		steps, err := displayBatchTestSteps(test, source, state, DisplayJobSteps, func(_ context.Context, offset, length int) ([]byte, error) {
			raw := bytes.Repeat([]byte("a"), length)
			if offset == source.Token.From {
				raw[0] = '"'
			}
			return raw, nil
		})
		if err != nil || len(steps) != DisplayJobSteps {
			test.Fatal("bounded source did not produce eight steps", err, len(steps))
		}
		retained, objectBytes, sourceBytes := 0, 0, 0
		for _, step := range steps {
			sourceBytes += step.ReadBytes
			retained += 256 + cap(step.Objects)*40 + cap(step.State.Directory.Levels)*24
			for _, level := range step.State.Directory.Levels {
				retained += cap(level) * 16
				for _, hash := range level {
					retained += len(hash)
				}
			}
			for _, object := range step.Objects {
				objectBytes += len(object.Bytes)
				retained += cap(object.Bytes) + len(object.Hash)
			}
			if step.Page != nil {
				retained += 2*domain.DocDisplayPageTextBytes + cap(step.Page.Runs)*32 + 128
			}
		}
		if objectBytes > DisplayJobSteps*domain.MaxDocDisplayPageBytes+2*domain.MaxDocRangeDirectoryBytes || sourceBytes > DisplayJobSteps*domain.DocDisplayStepSourceBytes || retained >= 4<<20 {
			test.Fatal("buffer exceeded existing bounded data envelope", count, objectBytes, sourceBytes, retained)
		}
		test.Logf("frontier_pages=%d objects_bytes=%d source_bytes=%d conservative_retained_bytes=%d", count, objectBytes, sourceBytes, retained)
	}
}
