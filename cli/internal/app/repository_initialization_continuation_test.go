package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestInitialLegacyContinuationStartsAtItsRecordedPrerequisite(t *testing.T) {
	for _, response := range []string{"complete", "lost-initialization", "lost-advance", "competing-tip"} {
		t.Run(response, func(t *testing.T) {
			svc, st, remote, in, competitor := initialPublicationFixture(t)
			ctx := context.Background()
			base, err := st.GetRef(ctx, in.RepoID, domain.RefBranch, "main")
			if err != nil {
				t.Fatal(err)
			}
			add := func(text string) domain.ContentHash {
				doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex}, Events: []domain.Event{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}}}}
				id, err := st.PutDoc(ctx, doc)
				if err != nil {
					t.Fatal(err)
				}
				if err := st.PutSnapshot(ctx, domain.Snapshot{RepoID: in.RepoID, ID: id, DocHash: id, Branch: "main", Parents: []domain.ContentHash{base.Target}}); err != nil {
					t.Fatal(err)
				}
				return id
			}
			old, continued := add("retained forward work"), add("new work from earlier code")
			current := base
			current.Target = continued
			if err := st.PutRef(ctx, current); err != nil {
				t.Fatal(err)
			}
			advance := domain.HistoryEvent{ID: strings.Repeat("d", 32), RepoID: in.RepoID, Branch: "main", BranchID: base.BranchID, Kind: "advance", Source: old, Target: continued, CreatedAt: time.Unix(3, 0).UTC()}
			if err := st.PutHistoryEvent(ctx, advance); err != nil {
				t.Fatal(err)
			}
			remote.lostFinalize = response == "lost-initialization" || response == "competing-tip"
			if response == "lost-advance" {
				remote.lostID = advance.ID
			}
			_, err = svc.Push(ctx, in)
			if response == "complete" && err != nil {
				t.Fatal(err)
			}
			if response != "complete" && err == nil {
				t.Fatal("lost response was reported as success")
			}
			if remote.receipt == nil || remote.receipt.Anchor == nil || remote.receipt.Anchor.Ref.Target != old {
				t.Fatal("initial observation did not preserve the recorded continuation prerequisite")
			}
			if response == "competing-tip" {
				competing := base
				competing.Target = competitor
				remote.setRef(competing)
				if _, err := svc.Push(ctx, in); !errors.Is(err, domain.ErrSyncConflict) {
					t.Fatalf("competing tip: %v", err)
				}
				if remote.refs[0].Target != competitor {
					t.Fatal("initialization retry rewound another writer")
				}
				return
			}
			if _, err := svc.Push(ctx, in); err != nil {
				t.Fatal(err)
			}
			if len(remote.refs) != 1 || remote.refs[0] != current || remote.finalizes != 1 {
				t.Fatal("continuation did not converge exactly once")
			}
			count := 0
			for _, e := range remote.sent {
				if e.ID == advance.ID {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("advance accepted %d times", count)
			}
		})
	}
}

func TestInitialObservationRejectsIncompatibleContinuationBeforeEffects(t *testing.T) {
	for _, failure := range []string{"disconnected-next-source", "unrelated-final-tip"} {
		t.Run(failure, func(t *testing.T) {
			svc, st, remote, in, unrelated := initialPublicationFixture(t)
			ctx := context.Background()
			ref, err := st.GetRef(ctx, in.RepoID, domain.RefBranch, "main")
			if err != nil {
				t.Fatal(err)
			}
			if failure == "unrelated-final-tip" {
				ref.Target = unrelated
				if err := st.PutRef(ctx, ref); err != nil {
					t.Fatal(err)
				}
			}
			// The first recorded continuation moves to main's original snapshot.
			base, err := st.ListHistoryEvents(ctx, in.RepoID)
			if err != nil {
				t.Fatal(err)
			}
			first := domain.HistoryEvent{ID: strings.Repeat("d", 32), RepoID: in.RepoID, Branch: "main", BranchID: ref.BranchID, Kind: "advance", Source: unrelated, Target: base[0].Target, CreatedAt: time.Unix(3, 0).UTC()}
			if err := st.PutHistoryEvent(ctx, first); err != nil {
				t.Fatal(err)
			}
			if failure == "disconnected-next-source" {
				second := first
				second.ID, second.CreatedAt = strings.Repeat("e", 32), time.Unix(4, 0).UTC()
				if err := st.PutHistoryEvent(ctx, second); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := svc.Push(ctx, in); !errors.Is(err, domain.ErrSyncConflict) {
				t.Fatalf("incompatible continuation: %v", err)
			}
			if remote.finalizes != 0 || len(remote.snaps)+len(remote.sent)+len(remote.writes) != 0 {
				t.Fatal("invalid initial sequence had publication effects")
			}
		})
	}
}
