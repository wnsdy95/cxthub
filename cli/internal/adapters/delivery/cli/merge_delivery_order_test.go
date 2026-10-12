package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func TestMergeDeliveryAutomaticVerifiedAppendOrder(t *testing.T) {
	for _, scenario := range []string{"same", "ancestor", "newer server", "missing code", "unknown commit", "unrelated", "shallow"} {
		t.Run(scenario, func(t *testing.T) {
			root, container, syncer, key := freezeRawCoverageDelivery(t)
			candidate := syncer.snapshots[0].ID
			job, err := readMergeDelivery(root, key)
			if err != nil {
				t.Fatal(err)
			}
			incomingAfter := job.Evidence.After
			selected := incomingAfter
			switch scenario {
			case "ancestor":
				selected = job.Evidence.Before
			case "newer server":
				runGitForTest(t, root, "commit", "--allow-empty", "-m", "newer server code")
				selected = gitOut(root, "rev-parse", "HEAD")
				syncer.ref.Target = briefingHash("newer main from another client")
				syncer.snapshots = append(syncer.snapshots, domain.Snapshot{ID: syncer.ref.Target, RepoID: syncer.ref.RepoID})
			case "missing code":
				selected = ""
			case "unknown commit":
				selected = strings.Repeat("f", 40)
			case "unrelated":
				selected = gitOut(root, "commit-tree", gitOut(root, "rev-parse", "HEAD^{tree}"), "-m", "unrelated code")
			case "shallow":
				selected = job.Evidence.Before
				if err := os.WriteFile(filepath.Join(root, ".git", "shallow"), []byte(incomingAfter+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before := syncer.ref.Target
			semantic := &semanticMergeSync{mergeDeliverySync: syncer, view: semanticMergeView(syncer)}
			semantic.view.Inclusion.CodeCommit = selected
			semantic.view.Revision = domain.RepositoryRevision{Graph: 7, Evidence: 9}
			queries := 0
			semantic.onQuery = func() { queries++ }
			semantic.afterAppend = func() {
				semantic.view = semanticMergeView(syncer)
				semantic.view.Inclusion.CodeCommit = incomingAfter
				semantic.view.Revision = domain.RepositoryRevision{Graph: 8, Evidence: 9}
			}
			container.Sync = semantic
			attempts := len(syncer.appends)
			_, reflected, attempted := replayMergeDeliveriesAt(context.Background(), container, root, true)
			job, err = readMergeDelivery(root, key)
			if err != nil || !attempted {
				t.Fatalf("saved delivery was not attempted: %+v attempted=%v err=%v", job, attempted, err)
			}
			permitted := scenario == "same" || scenario == "ancestor"
			if permitted {
				if !job.Completed || !reflected || syncer.ref.Target != candidate || len(syncer.appends) != attempts+1 || semantic.conditionalAttempts != 1 || queries != 3 {
					t.Fatalf("verified order did not append with one query per phase: job=%+v ref=%+v appends=%v queries=%d", job, syncer.ref, syncer.appends, queries)
				}
			} else if job.Completed || reflected || syncer.ref.Target != before || len(syncer.appends) != attempts || semantic.conditionalAttempts != 0 || job.Reason == "" || queries != 2 {
				t.Fatalf("unverified order moved current main: job=%+v ref=%+v appends=%v queries=%d", job, syncer.ref, syncer.appends, queries)
			}
		})
	}
}

func TestMergeAppendOrderRejectsUnverifiedLocalEvidence(t *testing.T) {
	for _, scenario := range []string{"abbreviated code", "noncommit code", "missing incoming", "missing same commit", "replacement ancestry", "grafted ancestry"} {
		t.Run(scenario, func(t *testing.T) {
			root := newPostMergeBriefingRepo(t)
			incomingAfter := gitOut(root, "rev-parse", "HEAD")
			selected := gitOut(root, "rev-parse", "HEAD^")
			switch scenario {
			case "abbreviated code":
				selected = selected[:12]
			case "noncommit code":
				selected = gitOut(root, "rev-parse", "HEAD^{tree}")
			case "missing incoming":
				incomingAfter = strings.Repeat("f", 40)
			case "missing same commit":
				selected, incomingAfter = strings.Repeat("f", 40), strings.Repeat("f", 40)
			case "replacement ancestry", "grafted ancestry":
				selected = gitOut(root, "commit-tree", gitOut(root, "rev-parse", "HEAD^{tree}"), "-m", "unrelated code")
				if scenario == "replacement ancestry" {
					runGitForTest(t, root, "replace", "--graft", incomingAfter, selected)
				} else if err := os.WriteFile(filepath.Join(root, ".git", "info", "grafts"), []byte(incomingAfter+" "+selected+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := verifyMergeAppendOrder(context.Background(), root, selected, incomingAfter); !errors.Is(err, domain.ErrSelectionChanged) {
				t.Fatalf("unverified local evidence accepted: selected=%s incoming=%s err=%v", selected, incomingAfter, err)
			}
		})
	}
}
