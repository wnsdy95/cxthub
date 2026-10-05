package backendclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type reviewTransferRoundTripper func(*http.Request) (*http.Response, error)

func (f reviewTransferRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func reviewWireResponse(r *http.Request, status int, value any) *http.Response {
	raw, _ := json.Marshal(value)
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw))), Request: r}
}

func TestBranchCapabilityWire(t *testing.T) {
	for _, mode := range []string{"old-absent", "explicit-zero", "supported-modern", "supported-legacy", "malformed", "wrong-repo", "denied"} {
		t.Run(mode, func(t *testing.T) {
			repo := string(domain.HashContent([]byte("capability fixture")))
			view := map[string]any{"id": repo, "default_branch": "main", "context_protocol": 1}
			want := 0
			status := 200
			switch mode {
			case "explicit-zero":
				view["branch_pull_version"] = 0
			case "supported-modern":
				view["branch_pull_version"] = 1
				want = 1
			case "supported-legacy":
				view["branch_pull_version"] = 1
				view["context_protocol"] = 0
				want = 1
			case "malformed":
				view["branch_pull_version"] = "1"
			case "wrong-repo":
				view["id"] = string(domain.HashContent([]byte("wrong")))
			case "denied":
				status = 403
			}
			client := NewBackendClient(func() string { return "https://synthetic.invalid" }, func() string { return "" }, domain.TeamIdentity{})
			calls := 0
			client.httpc.Transport = reviewTransferRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, "/"+repo) {
					t.Errorf("unexpected capability path %s %s", r.Method, r.URL.Path)
				}
				return reviewWireResponse(r, status, view), nil
			})
			got, err := client.PullCapabilities(context.Background(), repo)
			bad := mode == "malformed" || mode == "wrong-repo" || mode == "denied"
			if bad {
				if err == nil {
					t.Fatalf("invalid capability succeeded: %+v", got)
				}
			} else if err != nil || got.BranchPlanVersion != want || got.ContextProtocol != view["context_protocol"].(int) {
				t.Fatalf("capability changed: %+v %v", got, err)
			}
			if calls != 1 {
				t.Fatalf("capability calls=%d", calls)
			}
		})
	}
}

func TestBranchPlanStatusDoesNotUseBroadTransport(t *testing.T) {
	for _, status := range []int{401, 403, 404, 409, 422, 500, 501} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			repo := string(domain.HashContent([]byte("supported error fixture")))
			client := NewBackendClient(func() string { return "https://synthetic.invalid" }, func() string { return "" }, domain.TeamIdentity{})
			calls := 0
			client.httpc.Transport = reviewTransferRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/"+repo) {
					return reviewWireResponse(r, 200, map[string]any{"id": repo, "default_branch": "main", "branch_pull_version": 1}), nil
				}
				if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/pull/branch-plan") {
					t.Errorf("unexpected fallback %s %s", r.Method, r.URL.Path)
				}
				return reviewWireResponse(r, status, map[string]any{"code": "review_error", "error": "synthetic rejection"}), nil
			})
			caps, err := client.PullCapabilities(context.Background(), repo)
			if err != nil || caps.BranchPlanVersion != 1 {
				t.Fatal(caps, err)
			}
			p, snaps, err := client.PullSelectedBranchTo(context.Background(), repo, domain.BranchPullRequest{Version: 1, Branch: "feature"}, nil, nil, stagedPullDocs{})
			var httpErr *HTTPError
			if !errors.As(err, &httpErr) || httpErr.Status != status || p.Version != 0 || len(snaps) != 0 || calls != 2 {
				t.Fatalf("wrong typed failure/fallback: p=%+v err=%v calls=%d", p, err, calls)
			}
		})
	}
}
