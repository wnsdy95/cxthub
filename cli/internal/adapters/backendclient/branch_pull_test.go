package backendclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestSelectedBranchTransportUsesOnlyPlanInventory(t *testing.T) {
	for _, mode := range []string{"cold", "local-doc", "warm", "changed-state", "forbidden", "not-found", "invalid-plan"} {
		t.Run(mode, func(t *testing.T) {
			repo := string(domain.HashContent([]byte("selected transport")))
			snap, doc := makePullClientDoc(t, repo)
			state, err := domain.SnapshotStateHash(snap)
			if err != nil {
				t.Fatal(err)
			}
			ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "feature", Target: snap.ID}
			plan := domain.BranchPullPlan{Version: 1, RepoID: repo, Branch: "feature", SelectedRef: ref, Refs: []domain.Ref{ref}, SnapshotIndex: []domain.ContentHash{snap.ID}, SnapshotStates: map[domain.ContentHash]domain.ContentHash{snap.ID: state}}
			if mode == "invalid-plan" {
				plan.SnapshotIndex = append(plan.SnapshotIndex, snap.ID)
			}
			plans, metadata, bodies := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/pull/branch-plan") {
					plans++
					var req domain.BranchPullRequest
					if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&req) != nil || req.Version != 1 || req.Branch != "feature" {
						t.Error("invalid branch plan request")
						w.WriteHeader(400)
						return
					}
					if mode == "forbidden" {
						w.WriteHeader(403)
						return
					}
					if mode == "not-found" {
						w.WriteHeader(404)
						return
					}
					_ = json.NewEncoder(w).Encode(plan)
					return
				}
				if !strings.HasSuffix(r.URL.Path, "/pull/objects") {
					t.Errorf("unexpected broad query %s", r.URL.Path)
					w.WriteHeader(500)
					return
				}
				var req pullReq
				if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&req) != nil {
					t.Error("invalid object request")
					w.WriteHeader(400)
					return
				}
				if len(req.SnapshotWants) > 0 {
					metadata++
					if !reflect.DeepEqual(req.SnapshotWants, []domain.ContentHash{snap.ID}) {
						t.Errorf("out-of-plan snapshots: %v", req.SnapshotWants)
					}
					returned := snap
					if mode == "changed-state" {
						returned.GraftSeq++
					}
					_ = json.NewEncoder(w).Encode(pullResp{Snapshots: []domain.Snapshot{returned}})
					return
				}
				if !reflect.DeepEqual(req.DocManifestWants, []domain.ContentHash{doc.Hash}) {
					t.Errorf("out-of-plan bodies: %+v", req)
					w.WriteHeader(400)
					return
				}
				bodies++
				_ = json.NewEncoder(w).Encode(pullResp{Docs: []domain.SessionDoc{doc}})
			}))
			defer server.Close()
			client := NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
			states := map[domain.ContentHash]domain.ContentHash{}
			var haves []domain.ContentHash
			if mode == "local-doc" || mode == "warm" {
				haves = []domain.ContentHash{doc.Hash}
			}
			if mode == "warm" {
				states[snap.ID] = state
			}
			receiver := stagedPullDocs{}
			got, snaps, err := client.PullSelectedBranchTo(context.Background(), repo, domain.BranchPullRequest{Version: 1, Branch: "feature"}, states, haves, receiver)
			if mode == "forbidden" || mode == "not-found" || mode == "invalid-plan" || mode == "changed-state" {
				if err == nil || got.Version != 0 || len(snaps) != 0 || len(receiver) != 0 {
					t.Fatalf("invalid plan/objects succeeded: %+v %v", got, err)
				}
				wantMetadata := 0
				if mode == "changed-state" {
					wantMetadata = 1
				}
				if metadata != wantMetadata || bodies != 0 {
					t.Fatalf("failure performed extra I/O: metadata=%d body=%d", metadata, bodies)
				}
			} else {
				if err != nil || got.SelectedRef != ref {
					t.Fatalf("selected transport: %+v %v", got, err)
				}
				wantMetadata, wantBodies := 1, 1
				if mode == "warm" {
					wantMetadata = 0
				}
				if mode != "cold" {
					wantBodies = 0
				}
				if metadata != wantMetadata || bodies != wantBodies || len(snaps) != wantMetadata {
					t.Fatalf("metadata=%d body=%d snaps=%d", metadata, bodies, len(snaps))
				}
			}
			if plans != 1 {
				t.Fatalf("plans=%d", plans)
			}
		})
	}
}
