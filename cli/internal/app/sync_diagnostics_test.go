package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// These adapters observe the actual service boundaries. Delayed reads can still
// return a valid object, as a local read need not itself honor cancellation.
type diagnosticPushStore struct {
	*docReadCountingStore
	activity      *[]string
	afterSnapshot func(context.Context)
	afterDocument func(context.Context)
}

func (s *diagnosticPushStore) GetSnapshot(ctx context.Context, id domain.ContentHash) (domain.Snapshot, error) {
	snap, err := s.SessionStore.GetSnapshot(ctx, id)
	if err == nil && s.afterSnapshot != nil {
		s.afterSnapshot(ctx)
	}
	return snap, err
}

func (s *diagnosticPushStore) GetDoc(ctx context.Context, hash domain.ContentHash) (domain.SessionDoc, error) {
	doc, err := s.docReadCountingStore.GetDoc(ctx, hash)
	if s.activity != nil {
		*s.activity = append(*s.activity, "read:"+string(hash))
	}
	if err == nil && s.afterDocument != nil {
		s.afterDocument(ctx)
	}
	return doc, err
}

type diagnosticPushRemote struct {
	*lazyPushRemote
	activity       *[]string
	observe        func(context.Context, bool)
	failDocumentAt int
	documentCalls  int
	failure        error
}

func (r *diagnosticPushRemote) NegotiatePushObjects(ctx context.Context, repo string, snapshots, docs []domain.ContentHash) (outbound.PushObjectWants, error) {
	if r.activity != nil {
		*r.activity = append(*r.activity, "negotiate")
	}
	if r.observe != nil {
		r.observe(ctx, false)
	}
	return r.lazyPushRemote.NegotiatePushObjects(ctx, repo, snapshots, docs)
}

func (r *diagnosticPushRemote) Push(ctx context.Context, repo string, snapshots []domain.Snapshot, docs []domain.SessionDoc, refs []domain.Ref, force, appendMode bool) error {
	if r.activity != nil {
		for _, doc := range docs {
			*r.activity = append(*r.activity, "document:"+string(doc.Hash))
		}
		for _, snap := range snapshots {
			*r.activity = append(*r.activity, "snapshot:"+string(snap.ID))
		}
		for _, ref := range refs {
			*r.activity = append(*r.activity, "ref:"+ref.Name+":"+string(ref.Target))
		}
	}
	if r.observe != nil {
		r.observe(ctx, len(docs) > 0)
	}
	if len(docs) > 0 {
		r.documentCalls++
		if r.documentCalls == r.failDocumentAt {
			return r.failure
		}
	}
	return r.lazyPushRemote.Push(ctx, repo, snapshots, docs, refs, force, appendMode)
}

func TestPushDiagnosticsPreserveObservableBehavior(t *testing.T) {
	for _, scenario := range []string{"new-repository", "no-op", "document-repair", "snapshot-repair", "failed-second-document"} {
		t.Run(scenario, func(t *testing.T) {
			base, repo, ids := lazyPushFixture(t)
			wants := outbound.PushObjectWants{Snapshots: ids, Docs: ids}
			switch scenario {
			case "no-op":
				wants = outbound.PushObjectWants{}
			case "document-repair":
				wants = outbound.PushObjectWants{Docs: ids[1:]}
			case "snapshot-repair":
				wants = outbound.PushObjectWants{Snapshots: ids[1:]}
			}
			failure := errors.New("second document not acknowledged")
			type observation struct {
				output    inbound.SyncOutput
				progress  []inbound.SyncProgress
				activity  []string
				reads     []domain.ContentHash
				snapshots []domain.Snapshot
				documents []domain.SessionDoc
				refs      int
			}
			var baseline observation
			for _, enabled := range []bool{false, true} {
				var got observation
				store := &diagnosticPushStore{docReadCountingStore: &docReadCountingStore{SessionStore: base}, activity: &got.activity}
				remote := &diagnosticPushRemote{lazyPushRemote: &lazyPushRemote{wants: wants}, activity: &got.activity, failure: failure}
				if scenario == "failed-second-document" {
					remote.failDocumentAt = 2
				}
				collector := outbound.NewSyncDiagnostics(64)
				ctx := context.Background()
				if enabled {
					ctx = outbound.WithSyncDiagnostics(ctx, collector)
				}
				var err error
				got.output, err = newTestSyncService(store, remote, nil).Push(ctx, inbound.SyncInput{RepoID: repo, Progress: func(p inbound.SyncProgress) {
					got.progress = append(got.progress, p)
				}})
				if scenario == "failed-second-document" {
					if !errors.Is(err, failure) {
						t.Fatalf("enabled=%v: error=%v, want document failure", enabled, err)
					}
					assertDiagnosticPushIncomplete(t, got.progress)
					if len(store.reads) != 2 || len(remote.objectDocs) != 1 || len(remote.objectSnapshots) != 0 || remote.refCalls != 0 {
						t.Fatalf("failed upload advanced publication: reads=%v docs=%d snapshots=%d refs=%d", store.reads, len(remote.objectDocs), len(remote.objectSnapshots), remote.refCalls)
					}
				} else if err != nil {
					t.Fatalf("enabled=%v: %v", enabled, err)
				} else if got.output.Pushed != len(wants.Snapshots) || len(store.reads) != len(wants.Docs) || remote.refCalls != 1 || len(got.progress) == 0 || got.progress[len(got.progress)-1].Phase != "complete" {
					t.Fatalf("incorrect successful push: output=%+v reads=%v refs=%d progress=%v", got.output, store.reads, remote.refCalls, got.progress)
				}
				got.reads, got.snapshots, got.documents, got.refs = store.reads, remote.objectSnapshots, remote.objectDocs, remote.refCalls
				if !enabled {
					baseline = got
					if len(collector.Snapshot().Events) != 0 {
						t.Fatal("unattached collector received events")
					}
					continue
				}
				if !reflect.DeepEqual(got, baseline) {
					t.Fatalf("diagnostics changed push behavior:\nwith=%+v\nwithout=%+v", got, baseline)
				}
				report := collector.Snapshot()
				if len(report.Events) == 0 || report.HasFailures != (scenario == "failed-second-document") {
					t.Fatalf("missing or incorrect diagnostics: %+v", report)
				}
				for _, stage := range []outbound.SyncDiagnosticStage{outbound.SyncStagePush, outbound.SyncStageRetention, outbound.SyncStagePreparation} {
					if events := diagnosticEventsForStage(report, stage); len(events) != 1 {
						t.Fatalf("%s recorded %d times, want once", stage, len(events))
					}
				}
				reads := diagnosticEventsForStage(report, outbound.SyncStageDocumentRead)
				if len(reads) != len(store.reads) {
					t.Fatalf("diagnostic reads=%d, actual reads=%v", len(reads), store.reads)
				}
			}
		})
	}
}

func TestPushDiagnosticsPreserveGraftPublicationAndProgress(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%v", fail), func(t *testing.T) {
			var baselineOutput inbound.SyncOutput
			var baselineProgress []inbound.SyncProgress
			var baselineEvents []string
			var baselineReads []domain.ContentHash
			failure := errors.New("graft unavailable")
			for _, enabled := range []bool{false, true} {
				svc, remote, root, _ := setupPushOrder(t)
				store := &docReadCountingStore{SessionStore: svc.store}
				svc.store = store
				if fail {
					remote.failGraft = failure
				}
				ctx := context.Background()
				if enabled {
					ctx = outbound.WithSyncDiagnostics(ctx, outbound.NewSyncDiagnostics(64))
				}
				var progress []inbound.SyncProgress
				out, err := svc.Push(ctx, inbound.SyncInput{Cwd: root, Progress: func(p inbound.SyncProgress) { progress = append(progress, p) }})
				if fail && !errors.Is(err, failure) || !fail && err != nil {
					t.Fatalf("enabled=%v failure=%v: %v", enabled, fail, err)
				}
				if !enabled {
					baselineOutput, baselineProgress, baselineEvents, baselineReads = out, progress, remote.events, store.reads
					continue
				}
				if !reflect.DeepEqual(out, baselineOutput) || !reflect.DeepEqual(progress, baselineProgress) || !reflect.DeepEqual(remote.events, baselineEvents) || !reflect.DeepEqual(store.reads, baselineReads) {
					t.Fatalf("diagnostics changed graft push: output=%+v progress=%v publications=%v reads=%v", out, progress, remote.events, store.reads)
				}
				want := []string{"objects", "objects", "objects", "objects", "graft"}
				if !fail {
					want = append(want, "refs")
				} else {
					assertDiagnosticPushIncomplete(t, progress)
				}
				if !reflect.DeepEqual(remote.events, want) {
					t.Fatalf("publication order=%v, want %v", remote.events, want)
				}
			}
		})
	}
}

func TestPushDiagnosticsCallerDeadlineStopsPublication(t *testing.T) {
	for _, stage := range []string{"preparation", "first-document", "second-document"} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/diagnostics=%v", stage, enabled), func(t *testing.T) {
				base, repo, ids := lazyPushFixture(t)
				store := &diagnosticPushStore{docReadCountingStore: &docReadCountingStore{SessionStore: base}}
				var activity []string
				remote := &diagnosticPushRemote{lazyPushRemote: &lazyPushRemote{wants: outbound.PushObjectWants{Snapshots: ids, Docs: ids}}, activity: &activity}
				caller, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				deadline, _ := caller.Deadline()
				waitForDeadline := func(ctx context.Context) {
					if got, ok := ctx.Deadline(); !ok || !got.Equal(deadline) {
						t.Fatalf("local work received a fresh budget: %v / %v, want %v", got, ok, deadline)
					}
					<-ctx.Done()
				}
				wantReads, wantPublished := 0, 0
				if stage == "preparation" {
					store.afterSnapshot = waitForDeadline
				} else {
					wantReads = 1
					if stage == "second-document" {
						wantReads, wantPublished = 2, 1
					}
					store.afterDocument = func(ctx context.Context) {
						if len(store.reads) == wantReads {
							waitForDeadline(ctx)
						}
					}
				}
				collector := outbound.NewSyncDiagnostics(64)
				ctx := caller
				if enabled {
					ctx = outbound.WithSyncDiagnostics(ctx, collector)
				}
				var progress []inbound.SyncProgress
				_, err := newTestSyncService(store, remote, nil).Push(ctx, inbound.SyncInput{RepoID: repo, Progress: func(p inbound.SyncProgress) { progress = append(progress, p) }})
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("deadline during %s: %v", stage, err)
				}
				if len(store.reads) != wantReads || len(remote.objectDocs) != wantPublished || len(remote.objectSnapshots) != 0 || remote.refCalls != 0 {
					t.Fatalf("expired budget advanced work: reads=%v documents=%d snapshots=%d refs=%d", store.reads, len(remote.objectDocs), len(remote.objectSnapshots), remote.refCalls)
				}
				wantRemoteCalls := wantPublished + 1 // inventory negotiation precedes document reads
				if stage == "preparation" {
					wantRemoteCalls = 0
				}
				if remote.documentCalls != wantPublished || remote.objectCalls != wantPublished || len(activity) != wantRemoteCalls {
					t.Fatalf("remote called after local budget exhaustion: %v", activity)
				}
				assertDiagnosticPushIncomplete(t, progress)
				for _, p := range progress {
					if p.Phase == "upload-and-verify-documents" && p.Completed > wantPublished {
						t.Fatalf("expired read counted as acknowledged: %+v", p)
					}
				}
				if enabled {
					push := diagnosticEventsForStage(collector.Snapshot(), outbound.SyncStagePush)
					if len(push) != 1 || push[0].Outcome != outbound.SyncDiagnosticDeadline || push[0].EndCallerState != outbound.SyncContextDeadline || push[0].EndRemainingMillis == nil || *push[0].EndRemainingMillis > 0 {
						t.Fatalf("deadline lost from push diagnostic: %+v", push)
					}
					stageKind := outbound.SyncStageDocumentRead
					if stage == "preparation" {
						stageKind = outbound.SyncStagePreparation
					}
					local := diagnosticEventsForStage(collector.Snapshot(), stageKind)
					if len(local) == 0 || local[len(local)-1].EndCallerState != outbound.SyncContextDeadline {
						t.Fatalf("local work did not consume original deadline: %+v", local)
					}
				}
			})
		}
	}
}

func TestPushDiagnosticsDocumentAndRetryScopesKeepCallerBudget(t *testing.T) {
	base, repo, ids := lazyPushFixture(t)
	store := &docReadCountingStore{SessionStore: base}
	failure := errors.New("second document upload interrupted")
	remote := &diagnosticPushRemote{lazyPushRemote: &lazyPushRemote{wants: outbound.PushObjectWants{Snapshots: ids, Docs: ids}}, failDocumentAt: 2, failure: failure}
	caller, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	deadline, _ := caller.Deadline()
	collector := outbound.NewSyncDiagnostics(64)
	operation := outbound.WithSyncDiagnostics(caller, collector)
	remote.observe = func(ctx context.Context, document bool) {
		if got, ok := ctx.Deadline(); !ok || !got.Equal(deadline) {
			t.Fatalf("retry/document changed caller deadline: %v / %v, want %v", got, ok, deadline)
		}
		if outbound.SyncDiagnosticsFromContext(ctx) != collector {
			t.Fatal("retry/document detached the operation collector")
		}
		if document {
			// A remote may impose a shorter private timeout. Its passive event
			// must still describe the budget attached by the original caller.
			private, stop := context.WithTimeout(ctx, time.Second)
			defer stop()
			outbound.RecordSyncDiagnostic(outbound.WithSyncDiagnosticRole(private, outbound.SyncRoleDocumentChunks), outbound.SyncStageCanonical, outbound.SyncDiagnosticCounts{Documents: 1}, nil)
		}
	}
	svc := newTestSyncService(store, remote, nil)
	var firstProgress []inbound.SyncProgress
	_, err := svc.Push(outbound.WithSyncDiagnosticAttempt(operation, 1), inbound.SyncInput{RepoID: repo, Progress: func(p inbound.SyncProgress) { firstProgress = append(firstProgress, p) }})
	if !errors.Is(err, failure) || len(store.reads) != 2 || len(remote.objectDocs) != 1 || remote.refCalls != 0 {
		t.Fatalf("first attempt error=%v reads=%v documents=%d refs=%d", err, store.reads, len(remote.objectDocs), remote.refCalls)
	}
	assertDiagnosticPushIncomplete(t, firstProgress)
	remaining := store.reads[1]
	remote.wants.Docs = []domain.ContentHash{remaining}
	out, err := svc.Push(outbound.WithSyncDiagnosticAttempt(operation, 2), inbound.SyncInput{RepoID: repo})
	if err != nil || out.Pushed != 2 || len(store.reads) != 3 || store.reads[2] != remaining || len(remote.objectDocs) != 2 || remote.refCalls != 1 {
		t.Fatalf("retry error=%v output=%+v reads=%v documents=%d refs=%d", err, out, store.reads, len(remote.objectDocs), remote.refCalls)
	}
	report := collector.Snapshot()
	for _, stage := range []outbound.SyncDiagnosticStage{outbound.SyncStageDocumentRead, outbound.SyncStageCanonical} {
		events := diagnosticEventsForStage(report, stage)
		want := [][2]int{{1, 1}, {1, 2}, {2, 1}}
		if len(events) != len(want) {
			t.Fatalf("%s scopes=%+v, want %v", stage, events, want)
		}
		for i, event := range events {
			if event.Attempt != want[i][0] || event.Document != want[i][1] || stage == outbound.SyncStageCanonical && event.Role != outbound.SyncRoleDocumentChunks {
				t.Fatalf("%s lost document/attempt scope: %+v", stage, event)
			}
		}
	}
	pushes := diagnosticEventsForStage(report, outbound.SyncStagePush)
	if len(pushes) != 2 || pushes[0].Attempt != 1 || pushes[1].Attempt != 2 || pushes[0].Outcome != outbound.SyncDiagnosticError || pushes[1].Outcome != outbound.SyncDiagnosticSuccess {
		t.Fatalf("retry push scopes/outcomes=%+v", pushes)
	}
	if pushes[0].StartRemainingMillis == nil {
		t.Fatal("first attempt lost the original caller deadline")
	}
	// Elapsed + remaining refers to a single fixed deadline in the collector's
	// clock. This also catches accidentally rebinding private timeout contexts.
	budget := pushes[0].ElapsedMillis + *pushes[0].StartRemainingMillis
	for _, event := range report.Events {
		if event.OperationID != report.OperationID || event.StartRemainingMillis == nil || event.EndRemainingMillis == nil {
			t.Fatalf("scope lost operation or original deadline: %+v", event)
		}
		gotBudget := event.ElapsedMillis + *event.StartRemainingMillis
		if gotBudget < budget-2 || gotBudget > budget+2 {
			t.Fatalf("scope received a fresh/private budget: %+v, original=%dms", event, budget)
		}
	}
}

func diagnosticEventsForStage(report outbound.SyncDiagnosticsReport, stage outbound.SyncDiagnosticStage) []outbound.SyncDiagnosticEvent {
	var events []outbound.SyncDiagnosticEvent
	for _, event := range report.Events {
		if event.Stage == stage {
			events = append(events, event)
		}
	}
	return events
}

func assertDiagnosticPushIncomplete(t *testing.T, progress []inbound.SyncProgress) {
	t.Helper()
	for _, p := range progress {
		if p.Phase == "complete" || p.Phase == "publish-refs" {
			t.Fatalf("incomplete push claimed publication: %+v", p)
		}
	}
}
