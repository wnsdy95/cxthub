package backendclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func TestAppendRefConditionalWire(t *testing.T) {
	repo := string(domain.HashContent([]byte("conditional repo")))
	target := domain.HashContent([]byte("conditional target"))
	before := domain.HashContent([]byte("conditional before"))
	ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "feature/conditional", BranchID: "branch-identity", Target: target}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/repos/"+repo+"/conditional-appends/v1/feature/conditional" {
			t.Errorf("unexpected endpoint: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer conditional-token" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("X-Cxt-Doc-Identities") == "" {
			t.Error("request lost authentication or protocol headers")
		}
		var body struct {
			BranchID         string             `json:"branch_id"`
			Target           domain.ContentHash `json:"target"`
			ExpectedTarget   domain.ContentHash `json:"expected_target"`
			ExpectedRevision map[string]string  `json:"expected_revision"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.BranchID != ref.BranchID || body.Target != target || body.ExpectedTarget != before || body.ExpectedRevision["graph"] != "9007199254740993" || body.ExpectedRevision["evidence"] != "0" || body.ExpectedRevision["pending"] != "27" {
			t.Errorf("lost conditional binding: %+v", body)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := NewBackendClient(func() string { return server.URL + "/api/v1" }, func() string { return "conditional-token" }, domain.TeamIdentity{})
	if err := client.AppendRefConditional(context.Background(), repo, ref, before, domain.RepositoryRevision{Graph: 9007199254740993, Pending: 27}); err != nil || calls != 1 {
		t.Fatalf("append: %v requests=%d", err, calls)
	}
}

func TestAppendRefConditionalNeverFallsBack(t *testing.T) {
	for _, status := range []int{404, 405, 409, 403, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(status)
			}))
			defer server.Close()
			client := NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
			repo := string(domain.HashContent([]byte("repo")))
			ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", BranchID: "main-id", Target: domain.HashContent([]byte("target"))}
			err := client.AppendRefConditional(context.Background(), repo, ref, ref.Target, domain.RepositoryRevision{})
			var transport *HTTPError
			if !errors.As(err, &transport) || transport.Status != status || calls != 1 {
				t.Fatalf("error=%v requests=%d", err, calls)
			}
		})
	}
}

func TestAppendRefConditionalRejectsInvalidBinding(t *testing.T) {
	repo := string(domain.HashContent([]byte("repo")))
	ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", BranchID: "main-id", Target: domain.HashContent([]byte("target"))}
	for _, change := range []func(*domain.Ref, *domain.ContentHash){
		func(ref *domain.Ref, expected *domain.ContentHash) { ref.BranchID = "" },
		func(ref *domain.Ref, expected *domain.ContentHash) { ref.Kind = domain.RefTag },
		func(ref *domain.Ref, expected *domain.ContentHash) {
			ref.RepoID = string(domain.HashContent([]byte("other")))
		},
		func(ref *domain.Ref, expected *domain.ContentHash) { *expected = "" },
	} {
		candidate, expected := ref, ref.Target
		change(&candidate, &expected)
		client := NewBackendClient(func() string { t.Fatal("invalid request reached transport"); return "" }, func() string { return "" }, domain.TeamIdentity{})
		if err := client.AppendRefConditional(context.Background(), repo, candidate, expected, domain.RepositoryRevision{}); err == nil {
			t.Fatal("invalid binding accepted")
		}
	}
}
