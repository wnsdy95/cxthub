package cli

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// The real FileStore, SyncRepoService, ListSessionsService and generic-merge
// entrypoint run here. Only transport, Git identity and append effects are fake.
// No HTTP server, native Git repository or provider is needed.
type mergeObservationGit struct{ repo domain.Repo }

func (g mergeObservationGit) CurrentRepo(context.Context, string) (domain.Repo, error) {
	return g.repo, nil
}
func (mergeObservationGit) CurrentBranch(context.Context, string) (string, error) {
	return "main", nil
}

type mergeObservationRemote struct {
	outbound.RemoteSync
	ref      domain.Ref
	snaps    []domain.Snapshot
	returned []int
}

func (r *mergeObservationRemote) Pull(_ context.Context, repo string, states map[domain.ContentHash]domain.ContentHash, haves []domain.ContentHash) ([]domain.Snapshot, []domain.SessionDoc, []domain.Ref, error) {
	if repo != r.ref.RepoID || len(haves) != len(r.snaps) {
		return nil, nil, nil, fmt.Errorf("fixture repository/document inventory mismatch")
	}
	var changed []domain.Snapshot
	for _, snap := range r.snaps {
		state, err := domain.SnapshotStateHash(snap)
		if err != nil {
			return nil, nil, nil, err
		}
		if states[snap.ID] != state {
			changed = append(changed, snap)
		}
	}
	r.returned = append(r.returned, len(changed))
	return changed, nil, []domain.Ref{r.ref}, nil
}

// Record the command boundary; do not simulate server graph updates or use
// them to calculate expected coverage. Resolution is the real promoted method.
type mergeObservationSync struct {
	*app.SyncRepoService
	appends []domain.ContentHash
}

func (s *mergeObservationSync) AppendBranch(_ context.Context, _ inbound.SyncInput, branch string, target domain.ContentHash) error {
	if branch != "main" {
		return fmt.Errorf("unexpected fixture branch")
	}
	s.appends = append(s.appends, target)
	return nil
}

func TestMergeObservedCoverage(t *testing.T) {
	// loadRewrites probes ContextRoot. Prevent it from spawning native Git;
	// cwd falls back to our test directory. No other tests are selected.
	t.Setenv("PATH", t.TempDir())
	for _, mode := range []string{
		"local_only_remote_graft",
		"observed_remote_graft_control",
		"local_only_candidate_graft",
		"observed_candidate_graft_control",
	} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			repo := string(domain.HashContent([]byte("merge observation synthetic repository")))
			store := storage.NewFileStore(root)
			when := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
			var remoteSnaps []domain.Snapshot
			for i, label := range []string{"R", "A", "B"} {
				doc := domain.SessionDoc{CIR: domain.CIRDocument{
					Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, Fidelity: domain.FidelityFull},
					Events:   []domain.Event{{Kind: domain.EventMessage, Seq: 0, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "synthetic merge " + label}}}},
				}}
				id, err := store.PutDoc(ctx, doc)
				if err != nil {
					t.Fatal(err)
				}
				message := label
				if label == "A" {
					message += " [git aaaa]"
				} else if label == "B" {
					message += " [git bbbb]"
				}
				remoteSnaps = append(remoteSnaps, domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: "main", Message: message, CreatedAt: when.Add(time.Duration(i) * time.Second)})
			}
			r, a, b := remoteSnaps[0].ID, remoteSnaps[1].ID, remoteSnaps[2].ID
			localSnaps := append([]domain.Snapshot(nil), remoteSnaps...)
			edgeFrom := 0 // local R includes A, while the remote may not
			shas := []string{"aaaa1111"}
			want := []domain.ContentHash{a}
			switch mode {
			case "observed_remote_graft_control":
				want = nil
			case "local_only_candidate_graft":
				edgeFrom, shas, want = 2, []string{"aaaa1111", "bbbb2222"}, []domain.ContentHash{a, b}
			case "observed_candidate_graft_control":
				edgeFrom, shas, want = 2, []string{"aaaa1111", "bbbb2222"}, []domain.ContentHash{b}
			}
			localSnaps[edgeFrom].Grafted = true
			localSnaps[edgeFrom].GraftParents = []domain.ContentHash{a}
			localSnaps[edgeFrom].GraftSeq = 1
			if mode == "observed_remote_graft_control" || mode == "observed_candidate_graft_control" {
				remoteSnaps[edgeFrom] = localSnaps[edgeFrom]
			}
			for _, snap := range localSnaps {
				if err := store.PutSnapshot(ctx, snap); err != nil {
					t.Fatal(err)
				}
			}
			ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: r, BranchID: domain.LegacyContextBranchID(repo, "main")}
			if err := store.PutRef(ctx, ref); err != nil {
				t.Fatal(err)
			}
			remote := &mergeObservationRemote{ref: ref, snaps: remoteSnaps}
			syncer := &mergeObservationSync{SyncRepoService: app.NewSyncRepoService(store, remote, mergeObservationGit{domain.Repo{ID: repo, LocalPath: root}}, storage.NewSyncOutbox())}
			listing := app.NewListSessionsService(store)
			before, err := listing.List(ctx, inbound.ListInput{RepoID: repo})
			if err != nil {
				t.Fatal(err)
			}
			// Match the actual handleIncomingContexts ordering: first FetchOnly,
			// then a local list and the generic resolver's second fetch.
			out, err := syncer.Pull(ctx, inbound.SyncInput{RepoID: repo, Cwd: root, FetchOnly: true})
			if err != nil || out.Pulled != 3 || len(out.Conflicts) != 0 {
				t.Fatalf("initial real fetch: pulled=%d conflicts=%d error=%v", out.Pulled, len(out.Conflicts), err)
			}
			observed, err := store.ReadRemoteObservation(ctx, repo, "configured")
			if err != nil || len(observed.Snapshots) != 3 {
				t.Fatalf("missing verified observation: %v", err)
			}
			for _, snap := range observed.Snapshots {
				for _, expected := range remoteSnaps {
					if snap.ID == expected.ID && !reflect.DeepEqual(snap, expected) {
						t.Fatal("observation is not the remote graph")
					}
				}
			}
			reflected := appendMergedContexts(ctx, &Container{List: listing, Sync: syncer}, root, "main", shas)
			after, err := listing.List(ctx, inbound.ListInput{RepoID: repo})
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("fetch/observation adopted local metadata or moved refs: %v", err)
			}
			if !reflect.DeepEqual(remote.returned, []int{3, 0}) {
				t.Fatalf("expected actual cold fetch then warm resolver fetch; returned=%v", remote.returned)
			}
			names := func(ids []domain.ContentHash) []string {
				var labels []string
				for _, id := range ids {
					labels = append(labels, map[domain.ContentHash]string{r: "R", a: "A", b: "B"}[id])
				}
				return labels
			}
			if !reflect.DeepEqual(syncer.appends, want) {
				t.Fatalf("append targets=%v want=%v; reflected=%t; cold/warm metadata=[3 0], local graft retained, remote observation verified", names(syncer.appends), names(want), reflected)
			}
			if !reflected {
				t.Fatal("successful append or observed coverage was not reflected")
			}
		})
	}
}
