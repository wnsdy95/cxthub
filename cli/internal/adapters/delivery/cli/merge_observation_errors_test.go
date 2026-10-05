package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

// Missing observed nodes may not borrow edges from the local catalog.
func TestMergeUnknownGraphAndFailure(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	r, a, b, x := briefingHash("review-r"), briefingHash("review-a"), briefingHash("review-b"), briefingHash("review-x")
	for _, mode := range []string{"missing_root", "missing_intermediate", "missing_candidate", "missing_candidate_intermediate", "failed_covering_candidate", "direct_observed_edge_without_child_metadata", "exact_tip_without_metadata", "catalog_order_is_not_commit_order", "observed_older_and_failed_newer"} {
		t.Run(mode, func(t *testing.T) {
			local := []domain.Snapshot{{ID: b, GraftParents: []domain.ContentHash{a}, Message: "B [git bbbb]"}, {ID: a, Message: "A [git aaaa]"}, {ID: r, GraftParents: []domain.ContentHash{a}}, {ID: x, GraftParents: []domain.ContentHash{a}}}
			ref := domain.Ref{Kind: domain.RefBranch, Name: "main", Target: r}
			observed := []domain.Snapshot{{ID: r}, {ID: a}}
			shas := []string{"aaaa1111"}
			want := []domain.ContentHash{a}
			wantReflected := false
			var appendErr error = fmt.Errorf("synthetic denied append")
			switch mode {
			case "missing_root":
				observed = nil
			case "missing_intermediate":
				observed[0].Parents = []domain.ContentHash{x}
			case "missing_candidate":
				shas, want = []string{"aaaa1111", "bbbb2222"}, []domain.ContentHash{a, b}
			case "missing_candidate_intermediate":
				observed = append(observed, domain.Snapshot{ID: b, GraftParents: []domain.ContentHash{x}})
				shas, want = []string{"aaaa1111", "bbbb2222"}, []domain.ContentHash{a, b}
			case "failed_covering_candidate":
				observed = append(observed, domain.Snapshot{ID: b, GraftParents: []domain.ContentHash{a}})
				shas, want = []string{"aaaa1111", "bbbb2222"}, []domain.ContentHash{b}
			case "direct_observed_edge_without_child_metadata":
				observed = []domain.Snapshot{{ID: r, Parents: []domain.ContentHash{a}}}
				want, wantReflected = nil, true
			case "exact_tip_without_metadata":
				ref.Target, observed, want, wantReflected = a, nil, nil, true
			case "catalog_order_is_not_commit_order":
				observed = []domain.Snapshot{{ID: b}, {ID: r}, {ID: a}}
				shas, want = []string{"aaaa1111", "bbbb2222"}, []domain.ContentHash{a, b}
				appendErr, wantReflected = nil, true
			case "observed_older_and_failed_newer":
				observed = []domain.Snapshot{{ID: r, Parents: []domain.ContentHash{a}}, {ID: a}, {ID: b}}
				shas, want, wantReflected = []string{"aaaa1111", "bbbb2222"}, []domain.ContentHash{b}, true
			}
			base := &mergeObservationRefOnly{ref: ref, appendErr: appendErr}
			syncer := &mergeObservationBoundarySync{mergeObservationRefOnly: base, observed: inbound.RemoteBranchObservation{Ref: ref, Snapshots: observed}}
			got := appendMergedContexts(context.Background(), &Container{List: fixedBriefingList{out: inbound.ListOutput{Snapshots: local}}, Sync: syncer}, t.TempDir(), "main", shas)
			if got != wantReflected || !reflect.DeepEqual(base.appends, want) || syncer.calls != 1 || base.refCalls != 0 {
				t.Fatalf("reflected=%t want=%t; appends=%v want=%v; observation calls=%d ref-only calls=%d", got, wantReflected, base.appends, want, syncer.calls, base.refCalls)
			}
		})
	}
}

type mergeCommandSync struct {
	*app.SyncRepoService
	observed inbound.RemoteBranchObservation
}

func (s *mergeCommandSync) ResolveRemoteBranchObservation(context.Context, inbound.SyncInput, string) (inbound.RemoteBranchObservation, error) {
	return s.observed, nil
}

type mergeErrorTransport func(*http.Request) (*http.Response, error)

func (f mergeErrorTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Real FileStore -> SyncRepoService.AppendBranch -> BackendClient -> canned
// in-process RoundTripper -> real HTTPError parsing. No sockets/server/provider.
// The observation is synthetic. This is not a server identity-enforcement test;
// it proves the caller preserves the local command identity and classifies an
// actual transport error representation, including its branch-bearing path.
func TestMergeRejectedAppendIsNotReflected(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, tc := range []struct {
		name, branch, code string
		status             int
		want               bool
	}{
		{"forbidden_control", "main", "forbidden", 403, false},
		{"forbidden_name_contains_error_code", "fix/non_fast_forward", "forbidden", 403, false},
		{"identity_conflict_control", "main", "ref_conflict", 409, false},
		{"identity_conflict_name_contains_error_code", "fix/non_fast_forward", "ref_conflict", 409, false},
		{"genuine_behind_control", "main", "non_fast_forward", 409, true},
		{"genuine_behind_named_control", "fix/non_fast_forward", "non_fast_forward", 409, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, root := context.Background(), t.TempDir()
			repo := string(briefingHash("review-command-repo"))
			r, a := briefingHash("review-command-r"), briefingHash("review-command-a")
			if err := domain.ValidateBranchName(tc.branch); err != nil {
				t.Fatal(err)
			}
			store := storage.NewFileStore(root)
			local := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: tc.branch, Target: r, BranchID: "review-local-identity"}
			if err := store.PutRef(ctx, local); err != nil {
				t.Fatal(err)
			}
			observed := local
			if tc.code == "ref_conflict" {
				observed.BranchID = "review-reused-remote-identity"
			}
			requests := 0
			originalTransport := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = originalTransport })
			http.DefaultTransport = mergeErrorTransport(func(req *http.Request) (*http.Response, error) {
				requests++
				if req.Method != http.MethodPut || !strings.HasSuffix(req.URL.Path, "/refs/branch/"+tc.branch) {
					t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
				}
				var payload struct {
					BranchID string             `json:"branch_id"`
					Target   domain.ContentHash `json:"target"`
					Append   bool               `json:"append"`
				}
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				if payload.BranchID != local.BranchID || payload.Target != a || !payload.Append {
					t.Fatalf("wrong command identity or target: %+v", payload)
				}
				body := fmt.Sprintf(`{"error":{"code":%q,"message":"synthetic rejection"}}`, tc.code)
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			})
			remote := backendclient.NewBackendClient(func() string { return "https://merge-review.invalid/api/v1" }, func() string { return "" }, domain.TeamIdentity{})
			syncer := &mergeCommandSync{SyncRepoService: app.NewSyncRepoService(store, remote, mergeObservationGit{domain.Repo{ID: repo, LocalPath: root}}, storage.NewSyncOutbox()), observed: inbound.RemoteBranchObservation{Ref: observed, Snapshots: []domain.Snapshot{{ID: r}, {ID: a}}}}
			got := appendMergedContexts(ctx, &Container{List: fixedBriefingList{out: inbound.ListOutput{Snapshots: []domain.Snapshot{{ID: a, Message: "A [git aaaa]"}, {ID: r}}, Refs: []domain.Ref{local}}}, Sync: syncer}, root, tc.branch, []string{"aaaa1111"})
			after, err := store.GetRef(ctx, repo, domain.RefBranch, tc.branch)
			if err != nil || after != local || requests != 1 {
				t.Fatalf("local identity/ref changed or wrong request count: after=%+v err=%v requests=%d", after, err, requests)
			}
			if got != tc.want {
				t.Fatalf("reflected=%t want=%t after real HTTP %d code=%s, branch=%q; candidate absent from observed remote graph", got, tc.want, tc.status, tc.code, tc.branch)
			}
		})
	}
}
