package app

import (
	"crypto/sha256"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestBranchContextHistoricalDeliveryLegacyCompletion(t *testing.T) {
	fixture, ref, history := branchContextFixture(t)
	ctx := systemTestContext()
	snapshots, err := fixture.st.ListSnapshots(ctx, fixture.repo, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, code, state string }{
		{"included", effectiveOID(3), "included"},
		{"reset", effectiveOID(1), "not_selected"},
		{"unknown selected ancestry", effectiveOID(99), "review"},
	} {
		t.Run(test.name, func(t *testing.T) {
			evidence, err := fixture.svc.newCodeEvidence(ctx, fixture.repo)
			if err != nil {
				t.Fatal(err)
			}
			inclusion, err := fixture.svc.branchContext(ctx, ref, test.code, snapshots, history, evidence)
			if err != nil {
				t.Fatal(err)
			}
			if len(inclusion.Merges) != 2 {
				t.Fatalf("missing completion pairs: %+v", inclusion)
			}
			for _, merge := range inclusion.Merges {
				if !merge.DeliveryAcknowledged || merge.State != test.state || merge.DestinationBranchID != ref.BranchID || domain.ValidateGitOID(merge.MergeSHA) != nil {
					t.Fatalf("historical completion confused with current inclusion: %+v", merge)
				}
			}
			if test.state != "included" && (slices.Contains(inclusion.SnapshotIDs, fixture.a) || slices.Contains(inclusion.SnapshotIDs, fixture.b)) {
				t.Fatalf("acknowledgement restored excluded context: %+v", inclusion)
			}
		})
	}
	for _, rows := range [][]domain.HistoryEvent{history[:1], history[1:2], nil} {
		evidence, err := fixture.svc.newCodeEvidence(ctx, fixture.repo)
		if err != nil {
			t.Fatal(err)
		}
		inclusion, err := fixture.svc.branchContext(ctx, ref, effectiveOID(3), snapshots, rows, evidence)
		if err != nil {
			t.Fatal(err)
		}
		for _, merge := range inclusion.Merges {
			if merge.DeliveryAcknowledged {
				t.Fatalf("unpaired/unknown completion acknowledged: %+v", merge)
			}
		}
	}
}

func TestBranchContextHistoricalDeliveryKeepsInheritedDestination(t *testing.T) {
	fixture, main, history := branchContextFixture(t)
	ctx := systemTestContext()
	child := main
	child.Name, child.BranchID = "child", "child-identity"
	history = append(history,
		domain.HistoryEvent{ID: fmt.Sprintf("%032x", 901), RepoID: string(fixture.repo), Kind: "publish", Branch: main.Name, BranchID: main.BranchID, Source: main.Target, Target: main.Target, GitAfter: effectiveOID(3)},
		domain.HistoryEvent{ID: fmt.Sprintf("%032x", 902), RepoID: string(fixture.repo), Kind: "birth", Branch: child.Name, BranchID: child.BranchID, Source: main.Target, Target: main.Target, GitAfter: effectiveOID(3)},
	)
	snapshots, err := fixture.st.ListSnapshots(ctx, fixture.repo, "")
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := fixture.svc.newCodeEvidence(ctx, fixture.repo)
	if err != nil {
		t.Fatal(err)
	}
	inclusion, err := fixture.svc.branchContext(ctx, child, effectiveOID(1), snapshots, history, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if inclusion.BranchID != child.BranchID || len(inclusion.Merges) != 2 {
		t.Fatalf("missing inherited receipts: %+v", inclusion)
	}
	for _, merge := range inclusion.Merges {
		if !merge.DeliveryAcknowledged || merge.State != "not_selected" || merge.DestinationBranchID != main.BranchID || merge.DestinationBranchID == inclusion.BranchID {
			t.Fatalf("inherited receipt acknowledged wrong destination: %+v", merge)
		}
	}
	child.BranchID = "recreated-child"
	evidence, err = fixture.svc.newCodeEvidence(ctx, fixture.repo)
	if err != nil {
		t.Fatal(err)
	}
	inclusion, err = fixture.svc.branchContext(ctx, child, effectiveOID(3), snapshots, history, evidence)
	if err != nil || len(inclusion.Merges) != 0 {
		t.Fatalf("reused name inherited acknowledgements: %+v %v", inclusion, err)
	}
}

func TestBranchContextHistoricalDeliveryContributionResetAndConflict(t *testing.T) {
	for _, legacyPair := range []bool{false, true} {
		t.Run(fmt.Sprintf("coalesced-legacy=%t", legacyPair), func(t *testing.T) {
			worker, storage, remote, repo, ordinary, main := contributionFixture(t)
			ctx := systemTestContext()
			publishPRSource(t, worker.core, ordinary)
			for range 2 {
				if err := worker.reconcilePRHistory(ctx, remote, storage, historyWork(t, worker, storage, repo)); err != nil {
					t.Fatal(err)
				}
			}
			records, err := storage.ListPRContributions(ctx, repo.ID)
			if err != nil || len(records) != 1 {
				t.Fatalf("applied fixture: %+v %v", records, err)
			}
			if legacyPair {
				receipt := domain.HistoryEvent{ID: strings.Repeat("6", 32), RepoID: string(repo.ID), Kind: "pr-merge", Branch: main.Name, BranchID: main.BranchID, SourceBranchID: ordinary.BranchID, Source: ordinary.Target, Target: ordinary.Target, PR: &records[0].PR, CreatedAt: time.Unix(300, 0).UTC()}
				if err := storage.ApplyHistoryEvent(ctx, receipt); err != nil {
					t.Fatal(err)
				}
				completion := receipt
				key := sha256.Sum256([]byte(receipt.ID + ":completed"))
				completion.ID, completion.PRCompleted = fmt.Sprintf("%x", key[:16]), true
				if err := storage.ApplyHistoryEvent(ctx, completion); err != nil {
					t.Fatal(err)
				}
			}
			rewound := main
			rewound.Target = ordinary.Source
			if err := storage.CompareAndSwapRef(ctx, repo.ID, rewound, main.Target); err != nil {
				t.Fatal(err)
			}
			position := domain.HistoryEvent{ID: strings.Repeat("9", 32), RepoID: string(repo.ID), Kind: "position", Branch: main.Name, BranchID: main.BranchID, Source: rewound.Target, Target: rewound.Target, GitAfter: strings.Repeat("1", 40), CreatedAt: time.Unix(600, 0).UTC()}
			if err := worker.core.RecordHistory(ctx, position); err != nil {
				t.Fatal(err)
			}
			read := func(acknowledged bool, state string) {
				t.Helper()
				full, err := worker.core.GetRepositoryView(ctx, repo.ID)
				if err != nil {
					t.Fatal(err)
				}
				inclusion := full.Graph.BranchContexts["main"]
				if len(inclusion.Merges) != 1 {
					t.Fatalf("unexpected merges: %+v", inclusion)
				}
				merge := inclusion.Merges[0]
				if merge.DeliveryAcknowledged != acknowledged || merge.State != state || merge.Source != ordinary.Target || merge.MergeSHA != records[0].PR.MergeSHA || merge.DestinationBranchID != main.BranchID {
					t.Fatalf("invalid historical acknowledgement: %+v", merge)
				}
				if state == "review" && merge.Reason != "participation_evidence_conflict" {
					t.Fatalf("invalidated ledger not shown for review: %+v", merge)
				}
				if inclusion.SnapshotID != rewound.Target || slices.Contains(inclusion.SnapshotIDs, ordinary.Target) {
					t.Fatalf("acknowledgement changed reset inclusion: %+v", inclusion)
				}
				stream, err := worker.core.GetPendingView(ctx, repo.ID)
				if err != nil || !reflect.DeepEqual(stream.Graph.BranchContexts["main"], inclusion) {
					t.Fatalf("stream acknowledgement differs: %v", err)
				}
				query, err := worker.core.QueryContext(ctx, repo.ID, domain.ContextSelection{Scope: "current", Branch: "main", Position: "main"})
				if err != nil || !reflect.DeepEqual(query.Inclusion, &inclusion) {
					t.Fatalf("context/MCP projection differs: %+v %v", query.Inclusion, err)
				}
				current, err := storage.GetRef(ctx, repo.ID, domain.RefBranch, "main")
				if err != nil || current != rewound {
					t.Fatalf("read moved reset branch: %+v %v", current, err)
				}
			}
			read(true, "not_selected")
			conflict := ordinary
			conflict.ID, conflict.BranchID, conflict.Kind = strings.Repeat("7", 32), "ambiguous-original", "position"
			if err := worker.core.RecordHistory(ctx, conflict); err != nil {
				t.Fatal(err)
			}
			read(false, "review")
		})
	}
}

func TestBranchContextHistoricalDeliveryRequiresRevalidatedLedger(t *testing.T) {
	worker, storage, remote, repo, ordinary, main := contributionFixture(t)
	ctx := systemTestContext()
	publishPRSource(t, worker.core, ordinary)
	for range 2 {
		if err := worker.reconcilePRHistory(ctx, remote, storage, historyWork(t, worker, storage, repo)); err != nil {
			t.Fatal(err)
		}
	}
	records, err := storage.ListPRContributions(ctx, repo.ID)
	if err != nil || len(records) != 1 {
		t.Fatalf("applied fixture: %+v %v", records, err)
	}
	unknown := records[0]
	unknown.PR.Number++
	unknown.PullRequestID++
	unknown.PR.MergeSHA = strings.Repeat("9", 40)
	unknown = unknown.WithID()
	if err := storage.PutPRContribution(ctx, unknown); err != nil {
		t.Fatal(err)
	}
	if err := storage.AdvanceEvidenceRevision(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	view, err := worker.core.GetRepositoryView(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	merges := view.Graph.BranchContexts[main.Name].Merges
	if len(merges) != 2 {
		t.Fatalf("unknown ledger row hidden: %+v", merges)
	}
	for _, merge := range merges {
		if merge.MergeSHA == unknown.PR.MergeSHA {
			if merge.DeliveryAcknowledged || merge.State != "review" || merge.Reason != "participation_evidence_conflict" {
				t.Fatalf("unverified ledger row acknowledged: %+v", merge)
			}
		} else if !merge.DeliveryAcknowledged || merge.MergeSHA != records[0].PR.MergeSHA {
			t.Fatalf("valid receipt lost: %+v", merge)
		}
	}
	history, err := worker.core.ListHistory(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	child := main
	child.Name, child.BranchID = "child", "child-identity"
	history = append(history,
		domain.HistoryEvent{ID: fmt.Sprintf("%032x", 901), RepoID: string(repo.ID), Kind: "publish", Branch: main.Name, BranchID: main.BranchID, Source: main.Target, Target: main.Target, GitAfter: strings.Repeat("e", 40), CreatedAt: time.Unix(500, 0).UTC()},
		domain.HistoryEvent{ID: fmt.Sprintf("%032x", 902), RepoID: string(repo.ID), Kind: "birth", Branch: child.Name, BranchID: child.BranchID, Source: main.Target, Target: main.Target, GitAfter: strings.Repeat("e", 40), CreatedAt: time.Unix(501, 0).UTC()},
	)
	snapshots, err := storage.ListSnapshots(ctx, repo.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := worker.core.newCodeEvidence(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	inherited, err := worker.core.branchContext(ctx, child, strings.Repeat("1", 40), snapshots, history, evidence)
	if err != nil || len(inherited.Merges) != 2 {
		t.Fatalf("inherited ledger receipts: %+v %v", inherited, err)
	}
	for _, merge := range inherited.Merges {
		wantAcknowledged := merge.MergeSHA == records[0].PR.MergeSHA
		if merge.DestinationBranchID != main.BranchID || merge.DestinationBranchID == inherited.BranchID || merge.DeliveryAcknowledged != wantAcknowledged {
			t.Fatalf("inherited ledger acknowledged wrong destination or proof: %+v", merge)
		}
	}
}
