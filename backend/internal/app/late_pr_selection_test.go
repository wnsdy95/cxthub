package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestResolvedBranchCodeRequiresCausalAndGitEvidence(t *testing.T) {
	for _, scenario := range []string{"reversed history", "multiple older code observations", "missing Git evidence", "conflicting ordinary selection", "newer ordinary selection", "newer ordinary publication", "ordinary reset", "ordinary reset to merged code", "conflicting ordinary merged codes", "different branch identity", "cyclic receipts"} {
		t.Run(scenario, func(t *testing.T) {
			f, ref, history := branchContextFixture(t)
			ref.Target = f.a
			for i := range history {
				if history[i].PRCompleted && history[i].PR.Number == 10 {
					history[i].SharedTarget = f.b
				}
			}
			want := effectiveOID(3)
			switch scenario {
			case "reversed history":
				for i, j := 0, len(history)-1; i < j; i, j = i+1, j-1 {
					history[i], history[j] = history[j], history[i]
				}
			case "missing Git evidence":
				for i := range history {
					if history[i].PR.Number == 11 {
						copy := *history[i].PR
						copy.MergeSHA = effectiveOID(99)
						history[i].PR = &copy
					}
				}
				want = ""
			case "conflicting ordinary selection":
				history = append(history, domain.HistoryEvent{RepoID: string(f.repo), BranchID: ref.BranchID, Target: ref.Target, Kind: "position", GitAfter: effectiveOID(98)})
				want = ""
			case "newer ordinary selection", "newer ordinary publication":
				kind := "position"
				if scenario == "newer ordinary publication" {
					kind = "publish"
				}
				history = append(history, domain.HistoryEvent{RepoID: string(f.repo), BranchID: ref.BranchID, Source: ref.Target, Target: ref.Target, Kind: kind, GitAfter: effectiveOID(4)})
				want = effectiveOID(4)
			case "ordinary reset":
				history = append(history, domain.HistoryEvent{RepoID: string(f.repo), BranchID: ref.BranchID, Target: ref.Target, Kind: "position", GitAfter: effectiveOID(1)})
				want = ""
			case "ordinary reset to merged code":
				history = append(history, domain.HistoryEvent{RepoID: string(f.repo), BranchID: ref.BranchID, Target: ref.Target, Kind: "position", GitAfter: effectiveOID(2)})
				want = ""
			case "conflicting ordinary merged codes":
				for _, code := range []int{2, 3} {
					history = append(history, domain.HistoryEvent{RepoID: string(f.repo), BranchID: ref.BranchID, Target: ref.Target, Kind: "position", GitAfter: effectiveOID(code)})
				}
				want = ""
			case "multiple older code observations":
				for _, code := range []int{1, 2} {
					history = append(history, domain.HistoryEvent{RepoID: string(f.repo), BranchID: ref.BranchID, Target: hh("pre-merge-main"), Kind: "position", GitAfter: effectiveOID(code)})
				}
			case "different branch identity":
				for i := range history {
					if history[i].PR.Number == 11 {
						history[i].BranchID = "another-identity"
					}
				}
				want = effectiveOID(2)
			case "cyclic receipts":
				for i := range history {
					if history[i].PRCompleted && history[i].PR.Number == 11 {
						history[i].SharedTarget = f.a
					}
				}
				want = ""
			}
			evidence, err := f.svc.newCodeEvidence(systemTestContext(), f.repo)
			if err != nil {
				t.Fatal(err)
			}
			got, err := resolvedBranchCode(systemTestContext(), ref, history, evidence)
			if err != nil || got != want {
				t.Fatalf("got %q, want %q: %v", got, want, err)
			}
		})
	}
}

func TestLatePRPromotionKeepsNewestVerifiedCodeAcrossQueries(t *testing.T) {
	for _, alreadyContained := range []bool{false, true} {
		t.Run(fmt.Sprint(alreadyContained), func(t *testing.T) {
			ctx := systemTestContext()
			f := newEffectiveFixture(t)
			base := prSnapshot(t, f.st, f.repo, "BASE CONTEXT")
			older := prSnapshot(t, f.st, f.repo, "OLDER CONTEXT", base)
			parent := base
			if alreadyContained {
				parent = older
			}
			newer := prSnapshot(t, f.st, f.repo, "NEWER CONTEXT", parent)
			ref := domain.Ref{RepoID: f.repo, Kind: domain.RefBranch, Name: "main", Target: base}
			if err := f.st.CompareAndSwapRef(ctx, f.repo, ref, ""); err != nil {
				t.Fatal(err)
			}
			for id, text := range map[domain.ContentHash]string{older: "OLDER MEMORY", newer: "NEWER MEMORY"} {
				if _, err := f.svc.PutMemoryDigest(ctx, f.repo, domain.MemoryDigest{SnapshotID: id, Summary: text}); err != nil {
					t.Fatal(err)
				}
			}
			f.publish(t, older, 8, "older")
			f.publish(t, newer, 9, "newer")
			newerPR := domain.PullRequestMerge{Number: 102, BaseBranch: "main", HeadBranch: "newer", HeadSHA: effectiveOID(9), MergeSHA: effectiveOID(3)}
			olderPR := domain.PullRequestMerge{Number: 101, BaseBranch: "main", HeadBranch: "older", HeadSHA: effectiveOID(8), MergeSHA: effectiveOID(2)}
			if _, err := f.svc.PromoteRepositoryPR(ctx, f.repo, newerPR); err != nil {
				t.Fatal(err)
			}
			beforeSnapshots, err := f.st.ListSnapshots(ctx, f.repo, "")
			if err != nil {
				t.Fatal(err)
			}
			beforeLog, err := f.st.ReadReflog(ctx, f.repo)
			if err != nil {
				t.Fatal(err)
			}
			beforeHistory, err := f.svc.ListHistory(ctx, f.repo)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				out, err := f.svc.PromoteRepositoryPR(ctx, f.repo, olderPR)
				if err != nil {
					t.Fatal(err)
				}
				if out.Result != inbound.RefUpToDate || out.Ref.Target != newer || out.ServerTarget != newer || out.RequestedTarget != older {
					t.Fatalf("late promotion did not retain current ref: %+v", out)
				}
			}
			ref, err = f.st.GetRef(ctx, f.repo, domain.RefBranch, "main")
			if err != nil || ref.Target != newer {
				t.Fatalf("late PR rewound main: got=%s want=%s err=%v", ref.Target, newer, err)
			}
			afterSnapshots, err := f.st.ListSnapshots(ctx, f.repo, "")
			if err != nil || !reflect.DeepEqual(beforeSnapshots, afterSnapshots) {
				t.Fatalf("late PR changed snapshot ancestry: %v", err)
			}
			afterLog, err := f.st.ReadReflog(ctx, f.repo)
			if err != nil || !reflect.DeepEqual(beforeLog, afterLog) {
				t.Fatalf("late PR appended a ref movement: %v", err)
			}
			afterHistory, err := f.svc.ListHistory(ctx, f.repo)
			if err != nil {
				t.Fatal(err)
			}
			if len(afterHistory) != len(beforeHistory)+2 {
				t.Fatalf("expected one binding/completion pair, got %d new events", len(afterHistory)-len(beforeHistory))
			}
			byID := map[string]domain.HistoryEvent{}
			for _, event := range afterHistory {
				byID[event.ID] = event
				if event.PRCompleted && event.PR.Number == olderPR.Number && (event.Source != older || event.Target != newer || event.SharedTarget != "") {
					t.Fatalf("incorrect late completion: %+v", event)
				}
			}
			for _, event := range beforeHistory {
				if !reflect.DeepEqual(event, byID[event.ID]) {
					t.Fatalf("late PR changed immutable receipt %s", event.ID)
				}
			}
			full, err := f.svc.GetRepositoryView(ctx, f.repo)
			if err != nil {
				t.Fatal(err)
			}
			got := full.Graph.BranchContexts["main"]
			if got.CodeCommit != effectiveOID(3) || got.SnapshotID != newer || len(got.Merges) != 2 {
				t.Fatalf("late PR rewound shared code: %+v", got)
			}
			for _, m := range got.Merges {
				if m.State != "included" {
					t.Fatalf("lost merge: %+v", m)
				}
			}
			merges := map[int]domain.GraphMerge{}
			for _, merge := range full.Graph.Operations.Merges {
				merges[merge.PRNumber] = merge
			}
			olderMerge, newerMerge := merges[olderPR.Number], merges[newerPR.Number]
			if olderMerge.Source != older || olderMerge.Before != "" || olderMerge.After != newer || newerMerge.Source != newer {
				t.Fatalf("graph lost a verified merge: %+v", merges)
			}
			if len(full.Graph.Integrations) != 1 {
				t.Fatalf("missing graph integration: %+v", full.Graph.Integrations)
			}
			integration := full.Graph.Integrations[0]
			if integration.Head != newerMerge.ID || integration.Target != newer ||
				!reflect.DeepEqual(integration.Parents[olderMerge.ID], []string{string(older)}) ||
				!reflect.DeepEqual(integration.Parents[newerMerge.ID], []string{olderMerge.ID, string(newer)}) {
				t.Fatalf("graph merge connections lost verified Git order: %+v", integration)
			}
			live, err := f.svc.GetPendingView(ctx, f.repo)
			if err != nil || !reflect.DeepEqual(full.Graph, live.Graph) {
				t.Fatalf("live mismatch: %v", err)
			}
			query, err := f.svc.QueryContext(ctx, f.repo, domain.ContextSelection{Scope: "current", Branch: "main", Position: "main"})
			if err != nil || !reflect.DeepEqual(query.Inclusion, &got) {
				t.Fatalf("context mismatch: %+v %v", query.Inclusion, err)
			}
			memory, err := f.svc.QueryBranchMemory(ctx, f.repo, "main", got.SnapshotID, "")
			if err != nil || !reflect.DeepEqual(memory.Inclusion, &got) {
				t.Fatalf("memory mismatch: %+v %v", memory.Inclusion, err)
			}
			for _, text := range []string{"OLDER MEMORY", "NEWER MEMORY"} {
				if !strings.Contains(memory.Digest.Summary, text) {
					t.Fatalf("memory missing %q", text)
				}
			}
			past, err := f.svc.QueryBranchMemory(ctx, f.repo, "main", got.SnapshotID, effectiveOID(2))
			if err != nil {
				t.Fatal(err)
			}
			if past.Inclusion.CodeCommit != effectiveOID(2) {
				t.Fatal("explicit historical selection changed")
			}
			for _, m := range past.Inclusion.Merges {
				if m.PRNumber == 102 && m.State != "not_selected" {
					t.Fatalf("future merge included: %+v", m)
				}
			}
			pastSource, err := f.svc.QueryBranchMemory(ctx, f.repo, "main", older, effectiveOID(2))
			if err != nil || strings.Contains(pastSource.Digest.Summary, "NEWER MEMORY") {
				t.Fatalf("late receipt borrowed future context as its pre-merge position: %+v %v", pastSource, err)
			}
		})
	}
}

func TestLatePRPromotionRequiresCurrentVerifiedOrder(t *testing.T) {
	for _, scenario := range []struct {
		name                string
		currentMerge, merge int
		observation         string
		observedCode        int
		preserve            bool
		wantCode            int
		wantErr             error
		contained           bool
	}{
		{"completed merge", 3, 2, "", 0, true, 3, nil, false},
		{"ordinary position", 3, 2, "position", 4, true, 4, nil, false},
		{"ordinary publication", 3, 2, "publish", 4, true, 4, nil, false},
		{"ordinary continuation", 3, 2, "advance", 4, true, 4, nil, false},
		{"ordinary without completion", 0, 2, "position", 3, true, 3, nil, false},
		{"reset excludes prior completion", 3, 2, "reset", 1, false, 2, nil, false},
		{"newer merge", 3, 4, "", 0, false, 4, nil, false},
		{"equal merge", 3, 3, "", 0, false, 3, nil, false},
		{"missing incoming evidence", 3, 99, "", 0, true, 3, domain.ErrPROrderingPending, false},
		{"missing selected evidence", 99, 2, "", 0, true, 99, domain.ErrPROrderingPending, false},
		{"unrelated merges", 6, 7, "", 0, true, 6, domain.ErrConflict, false},
		{"ambiguous ordinary selection", 3, 2, "position", 98, true, 0, domain.ErrPROrderingPending, false},
		{"ambiguous ordinary without completion", 0, 2, "ambiguous", 3, true, 0, domain.ErrPROrderingPending, false},
		{"unknown continuation after completion", 3, 2, "advance", 0, true, 0, domain.ErrPROrderingPending, false},
		{"unknown reset after completion", 3, 2, "reset", 0, true, 0, domain.ErrPROrderingPending, false},
		{"first legacy bootstrap", 0, 99, "", 0, false, 99, nil, false},
		{"contained source with missing incoming evidence", 3, 99, "", 0, true, 3, domain.ErrPROrderingPending, true},
		{"contained source with missing selected evidence", 99, 2, "", 0, true, 99, domain.ErrPROrderingPending, true},
		{"contained source with unrelated merges", 6, 7, "", 0, true, 6, domain.ErrConflict, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := systemTestContext()
			fixture := newEffectiveFixture(t)
			base := prSnapshot(t, fixture.st, fixture.repo, "base")
			source := prSnapshot(t, fixture.st, fixture.repo, "delivered", base)
			parent := base
			if scenario.contained {
				parent = source
			}
			current := prSnapshot(t, fixture.st, fixture.repo, "current", parent)
			identity := domain.LegacyContextBranchID(string(fixture.repo), "main")
			ref := domain.Ref{RepoID: fixture.repo, Kind: domain.RefBranch, Name: "main", BranchID: identity, Target: base}
			if err := fixture.st.CompareAndSwapRef(ctx, fixture.repo, ref, ""); err != nil {
				t.Fatal(err)
			}
			fixture.publish(t, current, 9, "current")
			fixture.publish(t, source, 8, "delivered")
			if scenario.currentMerge != 0 {
				pr := domain.PullRequestMerge{Number: 200, BaseBranch: "main", HeadBranch: "current", HeadSHA: effectiveOID(9), MergeSHA: effectiveOID(scenario.currentMerge)}
				if _, err := fixture.svc.PromoteRepositoryPR(ctx, fixture.repo, pr); err != nil {
					t.Fatal(err)
				}
				ref.Target = current
			}
			if scenario.observation != "" {
				observation := domain.HistoryEvent{ID: fmt.Sprintf("%032x", 500), RepoID: string(fixture.repo), BranchID: identity, Branch: "main", Kind: "position", Source: ref.Target, Target: ref.Target, GitAfter: effectiveOID(scenario.observedCode), CreatedAt: time.Unix(1, 0).UTC()}
				if scenario.observedCode == 0 {
					observation.GitAfter = ""
				}
				if scenario.observation == "advance" || scenario.observation == "reset" {
					observation.Kind = "advance"
					observation.Target = base
					if scenario.observation == "advance" {
						observation.Target = prSnapshot(t, fixture.st, fixture.repo, "continuation", current)
					}
					ref.Target = observation.Target
				}
				if err := fixture.svc.RecordHistory(ctx, observation); err != nil {
					t.Fatal(err)
				}
				if scenario.observation == "ambiguous" {
					observation.ID, observation.GitAfter = fmt.Sprintf("%032x", 501), effectiveOID(4)
					if err := fixture.svc.RecordHistory(ctx, observation); err != nil {
						t.Fatal(err)
					}
				}
				if scenario.observation == "publish" {
					publishPRSource(t, fixture.svc, observation)
				}
			}
			pr := domain.PullRequestMerge{Number: 201, BaseBranch: "main", HeadBranch: "delivered", HeadSHA: effectiveOID(8), MergeSHA: effectiveOID(scenario.merge)}
			before, err := fixture.st.ListSnapshots(ctx, fixture.repo, "")
			if err != nil {
				t.Fatal(err)
			}
			beforeLog, err := fixture.st.ReadReflog(ctx, fixture.repo)
			if err != nil {
				t.Fatal(err)
			}
			out, err := fixture.svc.PromoteRepositoryPR(ctx, fixture.repo, pr)
			if !errors.Is(err, scenario.wantErr) {
				t.Fatalf("promotion error=%v want=%v", err, scenario.wantErr)
			}
			if scenario.wantErr != nil {
				rows, err := fixture.svc.ListHistory(ctx, fixture.repo)
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range rows {
					if event.PRCompleted && event.PR.Number == pr.Number {
						t.Fatalf("unverified order created completion: %+v", event)
					}
				}
			}
			want := source
			if scenario.preserve {
				want = ref.Target
				if scenario.wantErr == nil && out.Result != inbound.RefUpToDate {
					t.Fatalf("historical insertion moved ref: %+v", out)
				}
				after, err := fixture.st.ListSnapshots(ctx, fixture.repo, "")
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("historical insertion changed ancestry: %v", err)
				}
				afterLog, err := fixture.st.ReadReflog(ctx, fixture.repo)
				if err != nil || !reflect.DeepEqual(beforeLog, afterLog) {
					t.Fatalf("preserved promotion moved a ref: %v", err)
				}
			}
			stored, err := fixture.st.GetRef(ctx, fixture.repo, domain.RefBranch, "main")
			if err != nil || stored.Target != want || (scenario.wantErr == nil && out.Ref.Target != want) {
				t.Fatalf("target=%s output=%s want=%s err=%v", stored.Target, out.Ref.Target, want, err)
			}
			view, err := fixture.svc.GetRepositoryView(ctx, fixture.repo)
			if err != nil {
				t.Fatal(err)
			}
			wantCode := ""
			if scenario.wantCode != 0 {
				wantCode = effectiveOID(scenario.wantCode)
			}
			inclusion := view.Graph.BranchContexts["main"]
			if inclusion.CodeCommit != wantCode || inclusion.SnapshotID != want {
				t.Fatalf("code=%q want=%q inclusion=%+v", inclusion.CodeCommit, wantCode, inclusion)
			}
		})
	}
}

func TestLatePRCompletionRetryAndReplayPreserveCurrentRef(t *testing.T) {
	ctx := systemTestContext()
	fixture := newEffectiveFixture(t)
	base := prSnapshot(t, fixture.st, fixture.repo, "base")
	older := prSnapshot(t, fixture.st, fixture.repo, "older", base)
	newer := prSnapshot(t, fixture.st, fixture.repo, "newer", base)
	identity := domain.LegacyContextBranchID(string(fixture.repo), "main")
	ref := domain.Ref{RepoID: fixture.repo, Kind: domain.RefBranch, Name: "main", BranchID: identity, Target: base}
	if err := fixture.st.CompareAndSwapRef(ctx, fixture.repo, ref, ""); err != nil {
		t.Fatal(err)
	}
	fixture.publish(t, older, 8, "older")
	fixture.publish(t, newer, 9, "newer")
	newerPR := domain.PullRequestMerge{Number: 202, BaseBranch: "main", HeadBranch: "newer", HeadSHA: effectiveOID(9), MergeSHA: effectiveOID(3)}
	olderPR := domain.PullRequestMerge{Number: 201, BaseBranch: "main", HeadBranch: "older", HeadSHA: effectiveOID(8), MergeSHA: effectiveOID(2)}
	if _, err := fixture.svc.PromoteRepositoryPR(ctx, fixture.repo, newerPR); err != nil {
		t.Fatal(err)
	}
	failing := &failingCompletionStore{FSStore: fixture.st, fail: true}
	fixture.svc.meta = failing
	if _, err := fixture.svc.PromoteRepositoryPR(ctx, fixture.repo, olderPR); err == nil {
		t.Fatal("completion failure was acknowledged")
	}
	current, err := fixture.st.GetRef(ctx, fixture.repo, domain.RefBranch, "main")
	if err != nil || current.Target != newer {
		t.Fatalf("failed completion changed current ref: %+v %v", current, err)
	}
	failing.fail = false
	if _, err := fixture.svc.PromoteRepositoryPR(ctx, fixture.repo, olderPR); err != nil {
		t.Fatal(err)
	}
	reset := domain.HistoryEvent{ID: fmt.Sprintf("%032x", 500), RepoID: string(fixture.repo), BranchID: identity, Branch: "main", Kind: "advance", Source: newer, Target: base, GitAfter: effectiveOID(1), CreatedAt: time.Now().UTC()}
	if err := fixture.svc.RecordHistory(ctx, reset); err != nil {
		t.Fatal(err)
	}
	before, err := fixture.svc.ListHistory(ctx, fixture.repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, pr := range []domain.PullRequestMerge{olderPR, newerPR} {
		out, err := fixture.svc.PromoteRepositoryPR(ctx, fixture.repo, pr)
		if err != nil || out.Ref.Target != base || out.Result != inbound.RefUpToDate {
			t.Fatalf("completed replay overrode reset: %+v %v", out, err)
		}
	}
	after, err := fixture.svc.ListHistory(ctx, fixture.repo)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("completed replay changed history: %v", err)
	}
	view, err := fixture.svc.GetRepositoryView(ctx, fixture.repo)
	if err != nil || view.Graph.BranchContexts["main"].CodeCommit != effectiveOID(1) {
		t.Fatalf("completed replay changed selected code: %+v %v", view.Graph.BranchContexts["main"], err)
	}
}

type latePRJobClock struct {
	*store.FSStore
	now time.Time
}

func (clock *latePRJobClock) ClaimPRJob(ctx context.Context, repo domain.ContentHash, id string, now time.Time, lease time.Duration) (domain.PRPromotionJob, error) {
	if !clock.now.IsZero() {
		now = clock.now
	}
	return clock.FSStore.ClaimPRJob(ctx, repo, id, now, lease)
}

func TestLatePRPromotionWaitsForOrderingEvidenceBeforeCompleting(t *testing.T) {
	for _, missing := range []string{"incoming", "selected", "position"} {
		t.Run(missing, func(t *testing.T) {
			ctx := systemTestContext()
			fixture := newEffectiveFixture(t)
			base := prSnapshot(t, fixture.st, fixture.repo, "base")
			source := prSnapshot(t, fixture.st, fixture.repo, "source", base)
			current := prSnapshot(t, fixture.st, fixture.repo, "current", base)
			ref := domain.Ref{RepoID: fixture.repo, Kind: domain.RefBranch, Name: "main", Target: base}
			if err := fixture.st.CompareAndSwapRef(ctx, fixture.repo, ref, ""); err != nil {
				t.Fatal(err)
			}
			fixture.publish(t, source, 8, "source")
			fixture.publish(t, current, 9, "current")
			currentMerge, incomingMerge := 3, 99
			if missing == "selected" {
				currentMerge, incomingMerge = 99, 2
			}
			if missing == "position" {
				incomingMerge = 2
			}
			currentPR := domain.PullRequestMerge{Number: 300, BaseBranch: "main", HeadBranch: "current", HeadSHA: effectiveOID(9), MergeSHA: effectiveOID(currentMerge)}
			if _, err := fixture.svc.PromoteRepositoryPR(ctx, fixture.repo, currentPR); err != nil {
				t.Fatal(err)
			}
			if missing == "position" {
				reset := domain.HistoryEvent{ID: fmt.Sprintf("%032x", 500), RepoID: string(fixture.repo), BranchID: domain.LegacyContextBranchID(string(fixture.repo), "main"), Branch: "main", Kind: "advance", Source: current, Target: base, CreatedAt: time.Now().UTC()}
				if err := fixture.svc.RecordHistory(ctx, reset); err != nil {
					t.Fatal(err)
				}
				current = base
			}
			pr := domain.PullRequestMerge{Number: 301, BaseBranch: "main", HeadBranch: "source", HeadSHA: effectiveOID(8), MergeSHA: effectiveOID(incomingMerge)}
			clock := &latePRJobClock{FSStore: fixture.st}
			fixture.svc.meta = clock
			before, err := fixture.st.ListSnapshots(ctx, fixture.repo, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.svc.DeliverPRPromotion(ctx, fixture.repo, pr); !errors.Is(err, domain.ErrPROrderingPending) || errors.Is(err, domain.ErrPRSourcePending) {
				t.Fatalf("ordering evidence was not distinguished from source: %v", err)
			}
			var job domain.PRPromotionJob
			for attempt := 1; attempt <= 22; attempt++ {
				job, err = fixture.st.GetPRJob(ctx, fixture.repo, domain.PRPromotionID(fixture.repo, pr.Number))
				if err != nil || job.State != "waiting" || job.Reason != "ordering_evidence_pending" || job.Attempts != attempt || job.Version != int64(attempt) {
					t.Fatalf("missing evidence did not stay queued: %+v %v", job, err)
				}
				if job.NextAttempt.Sub(job.UpdatedAt) != time.Second<<min(attempt, 8) || !job.LeaseUntil.IsZero() {
					t.Fatalf("ordering wait lost bounded backoff or lease release: %+v", job)
				}
				if job.Diagnostics == nil || job.Diagnostics.TotalClaims != uint64(attempt) || len(job.Diagnostics.Events) > domain.MaxPRJobDiagnosticEvents {
					t.Fatalf("ordering wait lost bounded diagnostic history: %+v", job.Diagnostics)
				}
				clock.now = job.NextAttempt.Add(-time.Nanosecond)
				if err := fixture.svc.ProcessPRPromotions(ctx, 1); err != nil {
					t.Fatal(err)
				}
				replayed, err := fixture.svc.SubmitPRPromotion(ctx, fixture.repo, pr)
				if err != nil || !reflect.DeepEqual(job, replayed) {
					t.Fatalf("publication or delivery reset pending ordering: %+v %v", replayed, err)
				}
				ref, err = fixture.st.GetRef(ctx, fixture.repo, domain.RefBranch, "main")
				if err != nil || ref.Target != current {
					t.Fatalf("missing evidence moved current ref: %+v %v", ref, err)
				}
				after, err := fixture.st.ListSnapshots(ctx, fixture.repo, "")
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("missing evidence changed ancestry: %v", err)
				}
				rows, err := fixture.svc.ListHistory(ctx, fixture.repo)
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range rows {
					if event.PRCompleted && event.PR.Number == pr.Number {
						t.Fatalf("missing evidence completed promotion: %+v", event)
					}
				}
				if attempt < 22 {
					clock.now = job.NextAttempt
					if err := fixture.svc.ProcessPRPromotions(ctx, 1); err != nil {
						t.Fatal(err)
					}
				}
			}
			if missing == "position" {
				position := domain.HistoryEvent{ID: fmt.Sprintf("%032x", 501), RepoID: string(fixture.repo), BranchID: domain.LegacyContextBranchID(string(fixture.repo), "main"), Branch: "main", Kind: "position", Source: current, Target: current, GitAfter: effectiveOID(3), CreatedAt: time.Unix(1, 0).UTC()}
				if err := fixture.svc.RecordHistory(ctx, position); err != nil {
					t.Fatal(err)
				}
			} else {
				fixture.reader.deltas[effectiveOID(99)] = domain.GitCommitDelta{Commit: effectiveOID(99), Parent: effectiveOID(3), Parents: []string{effectiveOID(3)}, Changes: []domain.GitPathChange{}, Complete: true}
				scans, err := NewGitScans(fixture.svc, fixture.reader)
				if err != nil {
					t.Fatal(err)
				}
				jobs, err := scans.ListScans(ctx, fixture.repo, "", 100)
				if err != nil {
					t.Fatal(err)
				}
				queued := false
				for _, scan := range jobs.Items {
					if scan.Commit == effectiveOID(99) && scan.State == "waiting" {
						queued = true
					}
				}
				if !queued {
					t.Fatal("missing Git evidence was not durably queued")
				}
				if err := scans.Process(ctx, 100); err != nil {
					t.Fatal(err)
				}
			}
			clock.now = job.NextAttempt
			if err := fixture.svc.ProcessPRPromotions(ctx, 1); err != nil {
				t.Fatal(err)
			}
			job, err = fixture.st.GetPRJob(ctx, fixture.repo, domain.PRPromotionID(fixture.repo, pr.Number))
			if err != nil || job.State != "completed" || job.Attempts != 23 || job.Reason != "" {
				t.Fatalf("verified evidence did not complete queued promotion: %+v %v", job, err)
			}
			want := source
			if missing != "incoming" {
				want = current
			}
			ref, err = fixture.st.GetRef(ctx, fixture.repo, domain.RefBranch, "main")
			if err != nil || ref.Target != want {
				t.Fatalf("verified promotion target=%s want=%s err=%v", ref.Target, want, err)
			}
			view, err := fixture.svc.GetRepositoryView(ctx, fixture.repo)
			code := effectiveOID(99)
			if missing == "position" {
				code = effectiveOID(3)
			}
			if err != nil || view.Graph.BranchContexts["main"].CodeCommit != code {
				t.Fatalf("verified evidence lost selected code: %+v %v", view.Graph.BranchContexts["main"], err)
			}
		})
	}
}
