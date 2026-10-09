package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/capturejournal"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/memory"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type frozenDriver struct {
	cwd     string
	c       *Container
	store   *storage.FileStore
	repo    domain.Repo
	save    *app.SaveSessionService
	capture *capture.SessionCaptureAdapter
}

func newFrozenDriver(t *testing.T) frozenDriver {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, key := range []string{"CXT_REMOTE", "CXT_WRAPPED", "CXT_WRAPPED_AGENT", "CODEX_THREAD_ID", "CODEX_SESSION_ID"} {
		t.Setenv(key, "")
	}
	cwd, c, store, repo := captureGateFixture(t)
	ctx := context.Background()
	base, err := store.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: domain.CIRVersionV1, SourceProvider: domain.ProviderClaude, SessionOriginID: "synthetic-driver-baseline", Fidelity: domain.FidelityFull}, Events: []domain.Event{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "baseline"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	memory, err := store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: base, Provider: domain.ProviderClaude, Summary: "frozen selected memory"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutSnapshot(ctx, domain.Snapshot{ID: base, DocHash: base, RepoID: repo.ID, Branch: "main", MemoryHash: memory, Provider: domain.ProviderClaude}); err != nil {
		t.Fatal(err)
	}
	ref, err := store.CreateBranchRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo.ID, Target: base})
	if err != nil {
		t.Fatal(err)
	}
	position, err := store.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	position.Snapshot, position.SharedTarget, position.BranchID = base, base, ref.BranchID
	position.MemoryHash, position.MemorySource, position.MemoryPinned = memory, base, true
	position.Selection = nil
	if err := store.PutWorkingPosition(ctx, position); err != nil {
		t.Fatal(err)
	}
	captureAdapter := capture.NewSessionCapture(store)
	save := app.NewSaveSessionService(gitctx.NewGitContextAdapter(),
		map[domain.ProviderKind]outbound.CaptureSource{domain.ProviderClaude: capture.NewClaudeCapture(), domain.ProviderCodex: capture.NewCodexCapture()},
		map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderClaude: codec.NewClaudeCodec(), domain.ProviderCodex: codec.NewCodexCodec()}, store, captureAdapter, nil)
	c.CommitCapture = save
	c.List = app.NewListSessionsService(store)
	if !gitctx.InspectContextRoot(ctx, cwd).Initialized {
		t.Fatal("worker fixture was not initialized")
	}
	return frozenDriver{cwd: cwd, c: c, store: store, repo: repo, save: save, capture: captureAdapter}
}

func (f frozenDriver) attempt(t *testing.T, text string, identity domain.DocumentIdentity, sealed bool) *commitCapturePass {
	t.Helper()
	ctx := context.Background()
	p, err := beginCommitCapture(ctx, f.c, f.cwd, []string{domain.ProviderClaude})
	if err != nil || p == nil || p.Version != 2 {
		t.Fatal("begin frozen attempt", err)
	}
	raw, err := json.Marshal(map[string]any{"type": "user", "sessionId": "synthetic-" + text, "message": map[string]any{"role": "user", "content": text}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "synthetic.jsonl")
	if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	in, err := f.capture.Freeze(ctx, f.cwd, path, domain.ProviderClaude, identity)
	if err != nil {
		t.Fatal(err)
	}
	next := *p
	next.Outcomes = append([]domain.CaptureOutcome(nil), p.Outcomes...)
	next.Outcomes[0].Input = &in
	next.Outcomes[0].SessionPath = path
	next.InputsReady = sealed
	next.Message = "synthetic frozen commit"
	if err := p.replace(ctx, f.cwd, next); err != nil {
		t.Fatal(err)
	}
	baseline := p.Proof
	baseline.ID = domain.CaptureBaselineObservationID(p.Proof.ID)
	baseline.Source, baseline.Target = p.Initial, p.Initial
	if err := f.c.History.RecordHistory(ctx, baseline); err != nil {
		t.Fatal(err)
	}
	// Subsequent success must come from the durable input, never rediscovery.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	return p
}

func (f frozenDriver) readAttempt(t *testing.T, p *commitCapturePass) commitCapturePass {
	t.Helper()
	raw, err := providerfs.ReadRepoFile(f.cwd, p.relativePath())
	if err != nil {
		t.Fatal(err)
	}
	var got commitCapturePass
	if err := json.Unmarshal(raw, &got); err != nil || got.validate() != nil {
		t.Fatal("invalid durable attempt", err)
	}
	return got
}

func (f frozenDriver) history(t *testing.T) map[string]domain.HistoryEvent {
	t.Helper()
	events, err := publicationHistory(context.Background(), f.c, f.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func (f frozenDriver) completed(t *testing.T, p *commitCapturePass) commitCapturePass {
	t.Helper()
	stored := f.readAttempt(t, p)
	if !stored.Complete || !stored.MemoryFinalized || stored.Outcomes[0].State != "saved" || stored.Observation == nil {
		t.Fatal("attempt did not complete durably")
	}
	outcome := stored.Outcomes[0]
	// This fixture has no distiller: Save must retain the original selected
	// tuple, and recordOutcome must durably copy both fields from its result.
	if outcome.MemoryHash == "" || outcome.MemoryHash != stored.Proof.MemoryHash || outcome.MemorySource != stored.Proof.MemorySource {
		t.Fatal("saved outcome lost the selected memory tuple")
	}
	id := domain.CaptureProviderObservationID(p.Proof.ID, 0)
	observation := stored.Observation
	if observation.ID != id || observation.Kind != "position" || !stored.matchesObservation(outcome.Target, *observation) || observation.Source != outcome.Target || !observation.MemoryPinned || observation.MemoryHash != outcome.MemoryHash || observation.MemorySource != outcome.MemorySource || !observation.CreatedAt.Equal(stored.Proof.CreatedAt) {
		t.Fatal("completion lost the exact frozen observation")
	}
	events := f.history(t)
	accepted, ok := events[id]
	if !ok {
		t.Fatal("missing immutable provider observation")
	}
	got, gotErr := json.Marshal(accepted)
	want, wantErr := json.Marshal(observation)
	if gotErr != nil || wantErr != nil || !bytes.Equal(got, want) {
		t.Fatal("completed observation differs from immutable history", gotErr, wantErr)
	}
	return stored
}

func (f frozenDriver) published(t *testing.T, p *commitCapturePass) {
	t.Helper()
	stored := f.completed(t, p)
	events := f.history(t)
	publication := domain.CaptureAttempt(stored).Publication()
	if got, ok := events[publication.ID]; !ok || got.Target != stored.Outcomes[0].Target || got.Kind != "publish" {
		t.Fatal("missing exact publication")
	}
}

type frozenDriverCapture struct {
	inbound.CommitCapture
	beforeFreeze   func(context.Context, string) error
	freezePaths    []string
	before         func(context.Context, domain.CaptureAttempt, int) error
	after          func(context.Context, domain.CaptureAttempt, int, inbound.SaveOutput) error
	beforePrepare  func(context.Context, domain.CaptureAttempt, int) error
	afterFinish    func(context.Context, domain.CaptureAttempt, domain.HistoryEvent) error
	prepared       []int
	apply          func(context.Context, domain.CaptureAttempt) error
	saves, freezes int
}

func (s *frozenDriverCapture) PrepareFrozenMemory(ctx context.Context, cwd string, p domain.CaptureAttempt, index int) (*domain.FrozenCaptureMemory, error) {
	s.prepared = append(s.prepared, index)
	if s.beforePrepare != nil {
		if err := s.beforePrepare(ctx, p, index); err != nil {
			return nil, err
		}
	}
	return s.CommitCapture.PrepareFrozenMemory(ctx, cwd, p, index)
}
func (s *frozenDriverCapture) FinishFrozenMemory(ctx context.Context, cwd string, p domain.CaptureAttempt) (domain.HistoryEvent, error) {
	event, err := s.CommitCapture.FinishFrozenMemory(ctx, cwd, p)
	if err == nil && s.afterFinish != nil {
		err = s.afterFinish(ctx, p, event)
	}
	return event, err
}

func (s *frozenDriverCapture) FreezeInput(ctx context.Context, cwd string, provider domain.ProviderKind, path string) (domain.FrozenCaptureInput, error) {
	s.freezes++
	s.freezePaths = append(s.freezePaths, path)
	if s.beforeFreeze != nil {
		if err := s.beforeFreeze(ctx, path); err != nil {
			return domain.FrozenCaptureInput{}, err
		}
	}
	return s.CommitCapture.FreezeInput(ctx, cwd, provider, path)
}
func (s *frozenDriverCapture) ApplyFrozen(ctx context.Context, cwd string, p domain.CaptureAttempt) error {
	if s.apply != nil {
		return s.apply(ctx, p)
	}
	return s.CommitCapture.ApplyFrozen(ctx, cwd, p)
}
func (s *frozenDriverCapture) SaveFrozen(ctx context.Context, cwd string, p domain.CaptureAttempt, index int) (inbound.SaveOutput, error) {
	s.saves++
	if s.before != nil {
		if err := s.before(ctx, p, index); err != nil {
			return inbound.SaveOutput{}, err
		}
	}
	out, err := s.CommitCapture.SaveFrozen(ctx, cwd, p, index)
	if err == nil && s.after != nil {
		err = s.after(ctx, p, index, out)
	}
	return out, err
}

func TestFrozenCaptureDriverExactObservationBeatsLaterSameSHARepin(t *testing.T) {
	for _, identity := range []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1} {
		t.Run(string(identity), func(t *testing.T) {
			f := newFrozenDriver(t)
			p := f.attempt(t, "exact-observation", identity, true)
			var laterPosition domain.WorkingPosition
			wrapped := &frozenDriverCapture{CommitCapture: f.save, after: func(ctx context.Context, p domain.CaptureAttempt, _ int, out inbound.SaveOutput) error {
				snapshot, err := f.store.GetSnapshot(ctx, out.SnapshotID)
				if err != nil {
					return err
				}
				later, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: out.SnapshotID, Provider: domain.ProviderClaude, PreviousMemoryHash: snapshot.MemoryHash, Summary: "later same-code memory"})
				if err != nil {
					return err
				}
				if err := f.store.CompareAndSwapSnapshotMemory(ctx, out.SnapshotID, snapshot.MemoryHash, later); err != nil {
					return err
				}
				e := p.Proof
				e.ID = strings.Repeat("e", 32)
				e.Source, e.Target = out.SnapshotID, out.SnapshotID
				e.MemoryHash, e.MemorySource = later, out.SnapshotID
				e.CreatedAt = p.Proof.CreatedAt.Add(time.Hour)
				if _, err := f.c.History.ValidateHistorySource(ctx, e); err != nil {
					return err
				}
				if err := f.c.History.RecordHistory(ctx, e); err != nil {
					return err
				}
				current, err := f.store.GetWorkingPosition(ctx)
				if err != nil {
					return err
				}
				current.Snapshot, current.MemoryHash, current.MemorySource, current.Selection = out.SnapshotID, later, out.SnapshotID, &e
				if err := f.store.PutWorkingPosition(ctx, current); err != nil {
					return err
				}
				laterPosition, err = f.store.GetWorkingPosition(ctx)
				return err
			}}
			f.c.CommitCapture = wrapped
			if err := processFrozenCapture(context.Background(), f.c, f.cwd, f.cwd, p); err != nil {
				t.Fatal(err)
			}
			f.published(t, p)
			current, err := f.store.GetWorkingPosition(context.Background())
			if err != nil || !reflect.DeepEqual(current, laterPosition) {
				t.Fatal("completion rewound the later memory selection", err)
			}
			ref, err := f.store.GetRef(context.Background(), f.repo.ID, domain.RefBranch, "main")
			if err != nil || ref.Target != p.Initial {
				t.Fatal("completion advanced a concurrently changed selection", err)
			}
			if wrapped.saves != 1 {
				t.Fatal("unexpected Save count")
			}
		})
	}
}

func TestFrozenCaptureDriverReplayAfterDurableSaveBeforeOutcome(t *testing.T) {
	f := newFrozenDriver(t)
	p := f.attempt(t, "cancel-before-outcome", domain.DocumentIdentityRootV1, true)
	before := domain.CaptureAttempt(*p).Fingerprint()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wrapped := &frozenDriverCapture{CommitCapture: f.save, after: func(context.Context, domain.CaptureAttempt, int, inbound.SaveOutput) error { cancel(); return nil }}
	f.c.CommitCapture = wrapped
	if err := processFrozenCapture(ctx, f.c, f.cwd, f.cwd, p); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation boundary", err)
	}
	stored := f.readAttempt(t, p)
	if domain.CaptureAttempt(stored).Fingerprint() != before || stored.Outcomes[0].State != "pending" {
		t.Fatal("outcome advanced after cancellation")
	}
	events := f.history(t)
	id := domain.CaptureProviderObservationID(p.Proof.ID, 0)
	observation, ok := events[id]
	if !ok {
		t.Fatal("Save did not become durable before interruption")
	}
	for _, e := range events {
		if e.Kind == "publish" {
			t.Fatal("interrupted attempt published")
		}
	}
	snaps, err := f.store.ListSnapshots(context.Background(), f.repo.ID, "")
	if err != nil || len(snaps) != 2 {
		t.Fatal("missing durable snapshot", err)
	}
	wrapped.after = nil
	p = &stored
	if err := processFrozenCapture(context.Background(), f.c, f.cwd, f.cwd, p); err != nil {
		t.Fatal(err)
	}
	f.published(t, p)
	if got := f.history(t)[id]; !reflect.DeepEqual(got, observation) {
		t.Fatal("retry replaced immutable observation")
	}
	after, err := f.store.ListSnapshots(context.Background(), f.repo.ID, "")
	if err != nil || len(after) != len(snaps) {
		t.Fatal("retry duplicated snapshot", err)
	}
	if wrapped.saves != 2 {
		t.Fatal("retry failed to re-enter actual SaveFrozen")
	}
}

func TestFrozenCaptureDriverAggregateMemoryEarlierProviderCoversLater(t *testing.T) {
	f := newFrozenDriver(t)
	f.save.WithFrozenMemory(nil, memory.NewRuleDistiller())
	ctx, stop := context.WithTimeout(context.Background(), 20*time.Second)
	defer stop()
	p, err := beginCommitCapture(ctx, f.c, f.cwd, []string{domain.ProviderClaude, domain.ProviderCodex})
	if err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		provider domain.ProviderKind
		raw      string
		source   outbound.CaptureSource
		codec    outbound.ProviderCodec
	}{
		{domain.ProviderClaude, `{"type":"user","sessionId":"frozen-claude","message":{"role":"user","content":"fresh claude decision"}}` + "\n", capture.NewClaudeCapture(), codec.NewClaudeCodec()},
		{domain.ProviderCodex, `{"type":"session_meta","payload":{"id":"frozen-codex"}}` + "\n" + `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"fresh codex decision"}]}}` + "\n", capture.NewCodexCapture(), codec.NewCodexCodec()},
	}
	next := *p
	next.Outcomes = append([]domain.CaptureOutcome(nil), p.Outcomes...)
	next.Message, next.InputsReady = "synthetic two-provider commit", true
	snaps := make([]domain.Snapshot, len(rows))
	for i, row := range rows {
		path := filepath.Join(t.TempDir(), "native.jsonl")
		if err := os.WriteFile(path, []byte(row.raw), 0600); err != nil {
			t.Fatal(err)
		}
		input, err := f.capture.Freeze(ctx, f.cwd, path, row.provider, domain.DocumentIdentityRootV1)
		if err != nil {
			t.Fatal(err)
		}
		input.SessionID, err = f.capture.FrozenSessionID(ctx, f.cwd, input)
		if err != nil {
			t.Fatal(err)
		}
		input.NativeMemory = &domain.NativeMemory{Provider: row.provider, Source: "synthetic frozen native", Text: "frozen " + row.provider + " native import"}
		env, ref, _, _, err := f.capture.ProjectFrozen(ctx, f.cwd, input, row.source, row.codec)
		if err != nil {
			t.Fatal(err)
		}
		next.Outcomes[i].Input = &input
		next.Outcomes[i].SessionPath, next.Outcomes[i].SessionID = path, input.SessionID
		snaps[i] = domain.Snapshot{ID: ref.Hash, DocHash: ref.Hash, DocIdentity: ref.Identity, RepoID: f.repo.ID, Branch: p.Proof.Branch, Parents: []domain.ContentHash{p.Initial}, Provider: row.provider, Fidelity: env.Fidelity, SessionID: env.SessionOriginID, CreatedAt: p.Proof.CreatedAt, Message: domain.HookMessagePrefix + " fixture"}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	// Both providers dedup existing snapshots. Only the earlier Claude result
	// contains all captures: Codex must not acquire the reverse graft on replay.
	snaps[0].Grafted, snaps[0].GraftSeq = true, 1
	snaps[0].GraftParents = []domain.ContentHash{snaps[1].ID}
	for i := len(snaps) - 1; i >= 0; i-- {
		if err := f.store.PutSnapshot(ctx, snaps[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.replace(ctx, f.cwd, next); err != nil {
		t.Fatal(err)
	}
	baseline := p.Proof
	baseline.ID = domain.CaptureBaselineObservationID(p.Proof.ID)
	if err := f.c.History.RecordHistory(ctx, baseline); err != nil {
		t.Fatal(err)
	}
	assertNoPublication := func() {
		t.Helper()
		for _, event := range f.history(t) {
			if event.Kind == "publish" {
				t.Fatal("interrupted memory capture published")
			}
		}
	}
	first, cancelFirst := context.WithCancel(ctx)
	defer cancelFirst()
	var savedOrder []int
	wrapped := &frozenDriverCapture{CommitCapture: f.save}
	recoveryChecked := false
	wrapped.beforePrepare = func(work context.Context, _ domain.CaptureAttempt, index int) error {
		if index != -1 {
			return nil
		}
		// Broad push can observe both saved outputs while this worker is about
		// to derive the aggregate. Its actual recovery path must not complete.
		durable := f.readAttempt(t, p)
		if durable.MemoryFinalized || durable.FinalMemory != nil || durable.Outcomes[0].State != "saved" || durable.Outcomes[1].State != "saved" {
			t.Fatal("recovery seam missed the pre-aggregate boundary")
		}
		before := domain.CaptureAttempt(durable).Fingerprint()
		if err := prepareCommitProof(work, f.c, &durable); err != nil || durable.Proof.Target != snaps[0].ID || durable.Observation == nil {
			t.Fatal("fixture lacked otherwise valid covering proof", err)
		}
		if err := recoverCommitCapture(work, f.c, f.cwd, f.cwd, &durable, f.history(t)); !errors.Is(err, errCaptureCompletionUnproven) {
			t.Fatal("recovery completed before final memory", err)
		}
		if after := f.readAttempt(t, p); domain.CaptureAttempt(after).Fingerprint() != before {
			t.Fatal("pre-aggregate recovery changed durable attempt")
		}
		recoveryChecked = true
		return nil
	}
	wrapped.before = func(_ context.Context, attempt domain.CaptureAttempt, index int) error {
		savedOrder = append(savedOrder, index)
		durable := f.readAttempt(t, p)
		plan := attempt.Outcomes[index].MemoryPlan
		if plan == nil || durable.Outcomes[index].MemoryPlan == nil || *plan != *durable.Outcomes[index].MemoryPlan {
			t.Fatal("Save preceded durable memory plan")
		}
		return nil
	}
	wrapped.after = func(work context.Context, attempt domain.CaptureAttempt, index int, out inbound.SaveOutput) error {
		if index == 1 {
			snapshot, err := f.store.GetSnapshot(work, out.SnapshotID)
			if err != nil || snapshot.MemoryHash != attempt.Outcomes[index].MemoryPlan.Memory || out.MemoryHash != snapshot.MemoryHash || out.MemorySource != snapshot.ID {
				t.Fatal("cancellation did not follow exact memory attachment", err)
			}
			cancelFirst() // Save is durable; recordOutcome has not run.
		}
		return nil
	}
	f.c.CommitCapture = wrapped
	if err := processFrozenCapture(first, f.c, f.cwd, f.cwd, p); !errors.Is(err, context.Canceled) {
		t.Fatal("provider outcome interruption", err)
	}
	interrupted := f.readAttempt(t, p)
	if interrupted.Complete || interrupted.MemoryFinalized || interrupted.FinalMemory != nil || interrupted.Outcomes[0].State != "saved" || interrupted.Outcomes[1].State != "pending" {
		t.Fatal("interrupted provider outcome advanced")
	}
	plans := [2]domain.FrozenCaptureMemory{*interrupted.Outcomes[0].MemoryPlan, *interrupted.Outcomes[1].MemoryPlan}
	assertNoPublication()

	// A second interruption follows the real final attachment/history write,
	// before the orchestrator can persist completion. Neither plan may refresh.
	second, cancelSecond := context.WithCancel(ctx)
	defer cancelSecond()
	wrapped.after = nil
	wrapped.afterFinish = func(work context.Context, attempt domain.CaptureAttempt, event domain.HistoryEvent) error {
		durable := f.readAttempt(t, p)
		if durable.FinalMemory == nil || attempt.FinalMemory == nil || *durable.FinalMemory != *attempt.FinalMemory || durable.Complete {
			t.Fatal("final attachment preceded durable aggregate plan")
		}
		snapshot, err := f.store.GetSnapshot(work, event.Target)
		if err != nil || snapshot.ID != snaps[0].ID || snapshot.MemoryHash != attempt.FinalMemory.Memory || event.MemoryHash != snapshot.MemoryHash || event.MemorySource != snapshot.ID {
			t.Fatal("final aggregate was not attached to covering snapshot", err)
		}
		cancelSecond()
		return nil
	}
	p = &interrupted
	if err := processFrozenCapture(second, f.c, f.cwd, f.cwd, p); !errors.Is(err, context.Canceled) {
		t.Fatal("final completion interruption", err)
	}
	interrupted = f.readAttempt(t, p)
	if interrupted.Complete || interrupted.MemoryFinalized || interrupted.FinalMemory == nil || interrupted.Outcomes[1].State != "saved" {
		t.Fatal("final interruption lost durable plan or advanced completion")
	}
	finalPlan := *interrupted.FinalMemory
	finalID := domain.CaptureFinalObservationID(p.Proof.ID)
	finalEvent, ok := f.history(t)[finalID]
	if !ok {
		t.Fatal("final attachment/history was not durable before interruption")
	}
	missingFinal := f.history(t)
	delete(missingFinal, finalID)
	claimedFinal := interrupted
	claimedFinal.MemoryFinalized = true
	if err := recoverCommitCapture(ctx, f.c, f.cwd, f.cwd, &claimedFinal, missingFinal); !errors.Is(err, errCaptureCompletionUnproven) {
		t.Fatal("recovery accepted finalized flag without exact final observation", err)
	}
	if current := f.readAttempt(t, p); domain.CaptureAttempt(current).Fingerprint() != domain.CaptureAttempt(interrupted).Fingerprint() {
		t.Fatal("missing-observation recovery changed durable attempt")
	}
	assertNoPublication()
	wrapped.afterFinish = nil
	p = &interrupted
	if err := processFrozenCapture(ctx, f.c, f.cwd, f.cwd, p); err != nil {
		t.Fatal("aggregate replay", err)
	}
	done := f.readAttempt(t, p)
	if !done.Complete || !done.MemoryFinalized || done.FinalMemory == nil || *done.FinalMemory != finalPlan || done.Observation == nil || done.Observation.ID != finalID || done.Proof.Target != snaps[0].ID || done.Observation.MemoryHash != finalPlan.Memory || done.Observation.MemorySource != snaps[0].ID || !done.Observation.MemoryPinned {
		t.Fatal("completion did not retain the exact aggregate observation")
	}
	for i, outcome := range done.Outcomes {
		if outcome.MemoryPlan == nil || *outcome.MemoryPlan != plans[i] || outcome.MemoryHash != plans[i].Memory || outcome.MemorySource != snaps[i].ID {
			t.Fatal("replay changed provider memory plan/result", i)
		}
		snapshot, err := f.store.GetSnapshot(ctx, snaps[i].ID)
		if err != nil || !reflect.DeepEqual(snapshot.Parents, snaps[i].Parents) || !reflect.DeepEqual(snapshot.GraftParents, snaps[i].GraftParents) || snapshot.GraftSeq != snaps[i].GraftSeq {
			t.Fatal("dedup replay changed natural/graft ancestry", i, err)
		}
	}
	if !recoveryChecked || !reflect.DeepEqual(wrapped.prepared, []int{0, 1, -1}) || !reflect.DeepEqual(savedOrder, []int{0, 1, 1}) {
		t.Fatal("replay re-prepared memory or re-saved a completed provider", wrapped.prepared, savedOrder)
	}
	digest, err := f.store.GetMemory(ctx, finalPlan.Memory)
	if err != nil || digest.SnapshotID != snaps[0].ID || digest.PreviousMemoryHash != plans[0].Memory {
		t.Fatal("aggregate lost its owner or causal attachment parent", err)
	}
	for source, wants := range map[domain.ContentHash][]string{
		p.Initial:   {"frozen selected memory"},
		snaps[0].ID: {"fresh claude decision", "frozen claude native import"},
		snaps[1].ID: {"fresh codex decision", "frozen codex native import"},
	} {
		var summaries string
		for _, fragment := range digest.Fragments {
			if fragment.SourceSnapshot == source {
				summaries += fragment.Summary
			}
		}
		for _, want := range wants {
			if !strings.Contains(summaries, want) || !strings.Contains(digest.Summary, want) {
				t.Fatal("aggregate lost frozen contribution or provenance", want)
			}
		}
	}
	events := f.history(t)
	wantEvent, _ := json.Marshal(finalEvent)
	gotEvent, _ := json.Marshal(events[finalID])
	completedEvent, _ := json.Marshal(done.Observation)
	if !bytes.Equal(wantEvent, gotEvent) || !bytes.Equal(wantEvent, completedEvent) {
		t.Fatal("retry replaced final immutable observation")
	}
	publication := domain.CaptureAttempt(done).Publication()
	if event, ok := events[publication.ID]; !ok || event.Kind != "publish" || event.Target != snaps[0].ID {
		t.Fatal("missing exact covering publication")
	}
}

func TestFrozenCaptureWorkerDuplicateFlockDoesNotProject(t *testing.T) {
	f := newFrozenDriver(t)
	p := f.attempt(t, "single-worker", domain.DocumentIdentityLegacy, true)
	wrapped := &frozenDriverCapture{CommitCapture: f.save}
	f.c.CommitCapture = wrapped
	release, err := claimCaptureWorker(f.cwd)
	if err != nil || release == nil {
		t.Fatal("initial worker claim", err)
	}
	t.Cleanup(func() {
		if release != nil {
			release()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runCommitCaptureWorker(ctx, f.c, f.cwd); err != nil {
		t.Fatal(err)
	}
	if wrapped.saves != 0 || f.readAttempt(t, p).Complete {
		t.Fatal("duplicate worker projected under an owned flock")
	}
	release()
	release = nil
	if err := runCommitCaptureWorker(ctx, f.c, f.cwd); err != nil {
		t.Fatal(err)
	}
	if wrapped.saves != 1 {
		t.Fatal("released worker did not process exactly once")
	}
	f.published(t, p)
}

func TestFrozenCaptureWorkerWakeGenerationDrainsNewSeal(t *testing.T) {
	f := newFrozenDriver(t)
	a := f.attempt(t, "queued-A", domain.DocumentIdentityRootV1, true)
	b := f.attempt(t, "queued-B", domain.DocumentIdentityRootV1, false)
	b.Proof.CreatedAt = a.Proof.CreatedAt.Add(time.Second)
	if err := b.write(f.cwd); err != nil {
		t.Fatal(err)
	}
	wake := filepath.Join(".cxt", "capture", "replay-wake")
	if err := providerfs.WriteRepoFileDurable(f.cwd, wake, []byte("generation-A"), 0600); err != nil {
		t.Fatal(err)
	}
	processed := []string{}
	duplicateReturned := false
	wakes := 0
	f.c.WakeHistoricalSync = func(string) { wakes++ }
	f.c.WakeCommitCapture = func(string) { t.Fatal("test must drain without another hook/worker spawn") }
	wrapped := &frozenDriverCapture{CommitCapture: f.save, before: func(ctx context.Context, p domain.CaptureAttempt, _ int) error {
		processed = append(processed, p.Proof.ID)
		if p.Proof.ID != a.Proof.ID {
			return nil
		}
		// A's worker has enumerated the old unsealed B and still owns flock.
		next := *b
		next.InputsReady = true
		if err := b.replace(ctx, f.cwd, next); err != nil {
			return err
		}
		if err := providerfs.WriteRepoFileDurable(f.cwd, wake, []byte("generation-B"), 0600); err != nil {
			return err
		}
		if err := runCommitCaptureWorker(ctx, f.c, f.cwd); err != nil {
			return err
		}
		duplicateReturned = true
		if len(processed) != 1 {
			return errors.New("second worker bypassed flock")
		}
		return nil
	}}
	f.c.CommitCapture = wrapped
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runCommitCaptureWorker(ctx, f.c, f.cwd); err != nil {
		t.Fatal(err)
	}
	if !duplicateReturned || !reflect.DeepEqual(processed, []string{a.Proof.ID, b.Proof.ID}) || wakes != 2 {
		t.Fatal("wake generation lost newly sealed B", processed, wakes)
	}
	f.published(t, a)
	f.published(t, b)
	for _, p := range []*commitCapturePass{a, b} {
		var retry captureRetry
		raw, err := providerfs.ReadRepoFile(f.cwd, filepath.Join(".cxt", "worktrees", p.Proof.WorktreeID, "capture-retries", p.Proof.ID+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &retry); err != nil || retry.Tries != 1 || retry.Error != "" {
			t.Fatal("unnecessary retry/backoff", err)
		}
	}
}

func TestFrozenCaptureWorkerDoesNotUpgradeUnsealedOrLegacy(t *testing.T) {
	f := newFrozenDriver(t)
	unsealed := f.attempt(t, "unsealed-v2", domain.DocumentIdentityLegacy, false)
	old := f.attempt(t, "old-v1", domain.DocumentIdentityLegacy, false)
	old.Version, old.FrozenPosition, old.InputsReady, old.Message = 1, nil, false, ""
	old.Outcomes[0].Input = nil
	if err := old.validate(); err != nil {
		t.Fatal(err)
	}
	if err := old.write(f.cwd); err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	for _, p := range []*commitCapturePass{unsealed, old} {
		raw, err := providerfs.ReadRepoFile(f.cwd, p.relativePath())
		if err != nil {
			t.Fatal(err)
		}
		before[p.Proof.ID] = raw
	}
	wrapped := &frozenDriverCapture{CommitCapture: f.save}
	f.c.CommitCapture = wrapped
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runCommitCaptureWorker(ctx, f.c, f.cwd); err != nil {
		t.Fatal(err)
	}
	if wrapped.saves != 0 || wrapped.freezes != 0 {
		t.Fatal("worker upgraded an unsealed/legacy receipt")
	}
	for _, p := range []*commitCapturePass{unsealed, old} {
		raw, err := providerfs.ReadRepoFile(f.cwd, p.relativePath())
		if err != nil || !bytes.Equal(before[p.Proof.ID], raw) {
			t.Fatal("receipt was mutated", err)
		}
	}
	for _, e := range f.history(t) {
		if e.Kind == "publish" {
			t.Fatal("unsealed/legacy receipt published")
		}
	}
}

func TestFrozenCaptureDriverRejectsForeignAndMalformedCurrentInput(t *testing.T) {
	for _, mode := range []string{"foreign", "malformed", "wrong-whole-hash", "corrupt", "missing", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			f := newFrozenDriver(t)
			p := f.attempt(t, "rejected-input", domain.DocumentIdentityRootV1, true)
			in := p.Outcomes[0].Input
			// A previous valid projection receipt must not hide later spool damage.
			if _, _, _, _, err := f.capture.ProjectFrozen(context.Background(), f.cwd, *in, capture.NewClaudeCapture(), codec.NewClaudeCodec()); err != nil {
				t.Fatal(err)
			}
			chunk := filepath.Join(f.cwd, ".cxt", "capture", "inputs", strings.TrimPrefix(string(in.Chunks[0].Hash), "sha256:"))
			switch mode {
			case "foreign":
				p.Proof.RepoID = string(domain.HashContent([]byte("foreign-repository")))
				p.FrozenPosition.RepoID = p.Proof.RepoID
			case "malformed":
				in.Chunks[0].Hash = "../escape"
			case "wrong-whole-hash":
				in.Hash = domain.HashContent([]byte("different native bytes"))
			case "corrupt":
				raw, err := os.ReadFile(chunk)
				if err != nil {
					t.Fatal(err)
				}
				raw[0] ^= 1
				if err := os.WriteFile(chunk, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(chunk); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(chunk, chunk+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(chunk+".original", chunk); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.write(f.cwd); err != nil {
				t.Fatal(err)
			}
			before, err := providerfs.ReadRepoFile(f.cwd, p.relativePath())
			if err != nil {
				t.Fatal(err)
			}
			position, err := f.store.GetWorkingPosition(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := processFrozenCapture(context.Background(), f.c, f.cwd, f.cwd, p); err == nil {
				t.Fatal("invalid input published")
			}
			after, err := providerfs.ReadRepoFile(f.cwd, p.relativePath())
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("failed receipt advanced", err)
			}
			now, err := f.store.GetWorkingPosition(context.Background())
			if err != nil || !reflect.DeepEqual(now, position) {
				t.Fatal("failed input moved selection", err)
			}
			snaps, err := f.store.ListSnapshots(context.Background(), f.repo.ID, "")
			if err != nil || len(snaps) != 1 {
				t.Fatal("failed input installed a snapshot", err)
			}
			for _, e := range f.history(t) {
				if e.Kind == "publish" {
					t.Fatal("invalid input published")
				}
			}
		})
	}
}

func frozenDriverNative(t *testing.T, f frozenDriver, fileID, bodyID string) string {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".claude", "projects", providerfs.EncodeCwd(f.cwd))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"type": "user", "sessionId": bodyID, "message": map[string]any{"role": "user", "content": "synthetic frozen hook input"}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fileID+".jsonl")
	if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFrozenCaptureDriverSelectionDiscoveryAndOwnedFailures(t *testing.T) {
	const owned = "11111111-1111-4111-8111-111111111111"
	const other = "22222222-2222-4222-8222-222222222222"
	for _, mode := range []string{"discovered", "absent", "owned-disappeared", "owned-id-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			f := newFrozenDriver(t)
			t.Setenv("CXT_WRAPPED_AGENT", domain.ProviderClaude)
			path := ""
			if mode != "absent" {
				bodyID := owned
				if mode == "owned-id-mismatch" {
					bodyID = other
				}
				path = frozenDriverNative(t, f, owned, bodyID)
			}
			managed := strings.HasPrefix(mode, "owned-")
			if managed {
				t.Setenv("CXT_WRAPPED", "1")
				t.Setenv("CXT_WRAPPER_PID", strconv.Itoa(os.Getppid()))
				t.Setenv("CXT_WRAPPED_SESSION_ID", owned)
			}
			selected, err := commandCapture(context.Background(), f.cwd, domain.ProviderClaude)
			if err != nil {
				t.Fatal(err)
			}
			if managed && (selected.SessionPath != path || selected.SessionID != owned) {
				t.Fatal("owned selector fixture was not exact")
			}
			if !managed && selected.SessionPath != "" {
				t.Fatal("fixture did not exercise the intentionally empty selector")
			}
			wrapped := &frozenDriverCapture{CommitCapture: f.save}
			if mode == "owned-disappeared" {
				wrapped.beforeFreeze = func(_ context.Context, selected string) error {
					if selected != path {
						return errors.New("wrong owned source")
					}
					return os.Remove(path)
				}
			}
			f.c.CommitCapture = wrapped
			p, err := beginCommitCapture(context.Background(), f.c, f.cwd, []string{domain.ProviderClaude})
			if err != nil {
				t.Fatal(err)
			}
			err = freezeCommitInputs(context.Background(), f.c, f.cwd, f.cwd, "synthetic hook", p)
			stored := f.readAttempt(t, p)
			if mode == "owned-disappeared" {
				if err == nil || stored.InputsReady || stored.Outcomes[0].State != "failed" || stored.Outcomes[0].Input != nil {
					t.Fatal("owned disappearance became absence", err)
				}
				return
			}
			if err != nil || !stored.InputsReady {
				t.Fatal("input was not sealed", err)
			}
			if mode == "absent" {
				if stored.Outcomes[0].State != "absent" || stored.Outcomes[0].Input != nil {
					t.Fatal("unclaimed missing source was not absent")
				}
				if err := processFrozenCapture(context.Background(), f.c, f.cwd, f.cwd, p); err != nil {
					t.Fatal(err)
				}
				if wrapped.saves != 0 || !p.Complete || p.Observation == nil || p.Observation.ID != domain.CaptureBaselineObservationID(p.Proof.ID) {
					t.Fatal("absence did not preserve exact baseline")
				}
				if !p.Observation.MemoryPinned || p.Observation.MemoryHash != p.Proof.MemoryHash || p.Observation.MemorySource != p.Proof.MemorySource {
					t.Fatal("absence changed the exact baseline memory tuple")
				}
				return
			}
			if stored.Outcomes[0].Input == nil || stored.Outcomes[0].Input.SourcePath != path || stored.Outcomes[0].SessionPath != path {
				t.Fatal("discovered path was not durably retained")
			}
			if !managed && (len(wrapped.freezePaths) != 1 || wrapped.freezePaths[0] != "") {
				t.Fatal("FreezeInput discovery bypassed")
			}
			if managed && stored.Outcomes[0].SessionID != owned {
				t.Fatal("owned native identity was stripped")
			}
			err = processFrozenCapture(context.Background(), f.c, f.cwd, f.cwd, p)
			if mode == "owned-id-mismatch" {
				if !errors.Is(err, domain.ErrHashMismatch) || f.readAttempt(t, p).Complete {
					t.Fatal("decoded identity mismatch accepted", err)
				}
				for _, e := range f.history(t) {
					if e.Kind == "publish" {
						t.Fatal("mismatched session published")
					}
				}
				return
			}
			if err != nil {
				stored := f.readAttempt(t, p)
				t.Fatalf("process failed: %v (settings nil: memory=%v journal=%v; saved=%v)", err, p.Settings == nil, stored.Settings == nil, stored.Outcomes[0].State == "saved")
			}
			f.published(t, p)
		})
	}
}

func TestFrozenCaptureHookInlineDeadlineDurableCallbackAndDeferredWake(t *testing.T) {
	for _, failApply := range []bool{false, true} {
		t.Run(strconv.FormatBool(failApply), func(t *testing.T) {
			f := newFrozenDriver(t)
			t.Setenv("CXT_WRAPPED_AGENT", domain.ProviderClaude)
			frozenDriverNative(t, f, "11111111-1111-4111-8111-111111111111", "11111111-1111-4111-8111-111111111111")
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			parentDeadline, _ := ctx.Deadline()
			wantErr := errors.New("synthetic optional Apply failure")
			checkedDeadline := false
			wrapped := &frozenDriverCapture{CommitCapture: f.save, before: func(work context.Context, _ domain.CaptureAttempt, _ int) error {
				end, ok := work.Deadline()
				if !ok || !end.Equal(parentDeadline.Add(-5*time.Second)) {
					return errors.New("inline deadline did not preserve hook reserve")
				}
				checkedDeadline = true
				release, err := claimCaptureWorker(f.cwd)
				if release != nil {
					release()
					return errors.New("inline projection did not own worker flock")
				}
				return err
			}}
			if failApply {
				wrapped.apply = func(context.Context, domain.CaptureAttempt) error { return wantErr }
			}
			f.c.CommitCapture = wrapped
			wakeCount, callbackCount := 0, 0
			f.c.WakeCommitCapture = func(string) { wakeCount++ }
			callback := func(cwd string) {
				callbackCount++
				if cwd != f.cwd {
					t.Fatal("wrong callback directory")
				}
				attempts, err := capturejournal.New(f.cwd, f.cwd).ListCaptureAttempts(context.Background(), f.repo.ID)
				if err != nil || len(attempts) != 1 || !attempts[0].Complete {
					t.Fatal("callback preceded durable completion", err)
				}
				completed := commitCapturePass(attempts[0])
				f.completed(t, &completed)
			}
			saved, err := snapshotForCommitWithPublication(ctx, f.c, f.cwd, "synthetic hook", callback)
			if failApply {
				if !errors.Is(err, wantErr) || saved != 0 {
					t.Fatal("Apply failure was hidden", err)
				}
			} else if err != nil || saved != 1 {
				t.Fatal("inline capture", saved, err)
			}
			if !checkedDeadline || callbackCount != 1 || wakeCount != 1 {
				t.Fatal("lost/duplicate callback or deferred worker wake", callbackCount, wakeCount)
			}
			release, err := claimCaptureWorker(f.cwd)
			if err != nil || release == nil {
				t.Fatal("inline worker lock leaked", err)
			}
			release()
			attempts, err := capturejournal.New(f.cwd, f.cwd).ListCaptureAttempts(context.Background(), f.repo.ID)
			if err != nil || len(attempts) != 1 {
				t.Fatal(err)
			}
			p := commitCapturePass(attempts[0])
			if failApply {
				if !p.Complete {
					t.Fatal("optional Apply erased durable completion")
				}
				for _, e := range f.history(t) {
					if e.Kind == "publish" {
						t.Fatal("publication ran after failed Apply")
					}
				}
			} else {
				f.published(t, &p)
			}
		})
	}
}

func TestFrozenCaptureHookInlineQueuesOversizedSealedInput(t *testing.T) {
	f := newFrozenDriver(t)
	p := f.attempt(t, "queued-large", domain.DocumentIdentityRootV1, true)
	raw, err := json.Marshal(map[string]any{"type": "user", "sessionId": "synthetic-queued-large", "message": map[string]any{"role": "user", "content": strings.Repeat("x", int(domain.FrozenCaptureChunkBytes))}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "large.jsonl")
	if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	input, err := f.capture.Freeze(ctx, f.cwd, path, domain.ProviderClaude, domain.DocumentIdentityRootV1)
	if err != nil || input.Validate() != nil || input.Size <= domain.FrozenCaptureChunkBytes {
		t.Fatal("invalid oversized frozen fixture", err)
	}
	next := *p
	next.Outcomes = append([]domain.CaptureOutcome(nil), p.Outcomes...)
	next.Outcomes[0].Input, next.Outcomes[0].SessionPath = &input, path
	if err := p.replace(ctx, f.cwd, next); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	before, err := providerfs.ReadRepoFile(f.cwd, p.relativePath())
	if err != nil {
		t.Fatal(err)
	}
	beforeFingerprint := domain.CaptureAttempt(*p).Fingerprint()
	wrapped := &frozenDriverCapture{CommitCapture: f.save}
	f.c.CommitCapture = wrapped
	saved, err := finishFrozenCaptureInline(ctx, f.c, f.cwd, f.cwd, p, func(string) { t.Fatal("queued input notified completion") })
	if err != nil || saved != 0 || wrapped.saves != 0 || len(wrapped.prepared) != 0 {
		t.Fatal("oversized sealed input normalized inline", saved, err)
	}
	after, err := providerfs.ReadRepoFile(f.cwd, p.relativePath())
	if err != nil || !bytes.Equal(before, after) || domain.CaptureAttempt(*p).Fingerprint() != beforeFingerprint {
		t.Fatal("inline deferral changed durable or in-memory attempt", err)
	}
	snapshots, err := f.store.ListSnapshots(ctx, f.repo.ID, "")
	if err != nil || len(snapshots) != 1 || snapshots[0].ID != p.Initial {
		t.Fatal("inline deferral projected a snapshot", err)
	}
}

func TestFrozenCaptureWorkerDoesNotInitializeUnconnectedDirectory(t *testing.T) {
	cwd := t.TempDir()
	// Both non-nil ports are needed to reach the real initialization gate.
	c := &Container{History: captureGateLegacyHistory{}, CommitCapture: &frozenDriverCapture{}}
	if err := runCommitCaptureWorker(context.Background(), c, cwd); err != nil {
		t.Fatal(err)
	}
	SpawnCommitCapture(cwd)
	if _, err := os.Stat(filepath.Join(cwd, ".cxt")); !os.IsNotExist(err) {
		t.Fatal("worker/wake recreated an unconnected replica", err)
	}
}
