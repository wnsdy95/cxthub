package backendclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestRefPushPreservesConflictDetails(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			refs := conflictTestRefs(2)
			if !legacy {
				refs = conflictTestRefs(2*maxRefBatchUpdates + 1)
			}
			calls := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet || (legacy && r.Method == http.MethodPost) {
					http.NotFound(w, r)
					return
				}
				calls++
				if calls == 2 && !legacy {
					_ = json.NewEncoder(w).Encode(map[string]int{"applied": maxRefBatchUpdates})
					return
				}
				writeRefTestError(w, http.StatusConflict, "non_fast_forward", fmt.Sprintf("non-fast-forward: branch/conflict-%d", calls))
			}))
			defer ts.Close()
			c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
			err := c.Push(context.Background(), refs[0].RepoID, nil, nil, refs, false, false)
			if !errors.Is(err, domain.ErrSyncConflict) {
				t.Fatalf("error=%v, want conflict", err)
			}
			wantCalls := 3
			if legacy {
				wantCalls = 2
			}
			if calls != wantCalls {
				t.Fatalf("requests=%d, want %d", calls, wantCalls)
			}
			for _, n := range []int{1, wantCalls} {
				if !strings.Contains(err.Error(), fmt.Sprintf("branch/conflict-%d", n)) {
					t.Fatalf("rejection details lost: %v", err)
				}
			}
			var httpErr *HTTPError
			if !errors.As(err, &httpErr) || httpErr.Code != "non_fast_forward" || httpErr.Status != http.StatusConflict {
				t.Fatalf("typed server cause lost: %v", err)
			}
		})
	}
}

func TestRefPushTerminalErrorKeepsPriorRejectionsWithoutBecomingRetryable(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			refs := conflictTestRefs(2)
			if !legacy {
				refs = conflictTestRefs(maxRefBatchUpdates + 1)
			}
			calls := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet || (legacy && r.Method == http.MethodPost) {
					http.NotFound(w, r)
					return
				}
				calls++
				if calls == 1 {
					writeRefTestError(w, http.StatusConflict, "non_fast_forward", "branch/earlier-conflict")
					return
				}
				// The ref name can contain this text without classifying a 403 as
				// a retryable synchronization conflict.
				writeRefTestError(w, http.StatusForbidden, "forbidden", "access revoked: branch/non_fast_forward")
			}))
			defer ts.Close()
			c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
			err := c.Push(context.Background(), refs[0].RepoID, nil, nil, refs, false, false)
			var httpErr *HTTPError
			if errors.Is(err, domain.ErrSyncConflict) || !errors.As(err, &httpErr) || httpErr.Status != http.StatusForbidden {
				t.Fatalf("terminal authority failure misclassified: %v", err)
			}
			if calls != 2 || !strings.Contains(err.Error(), "branch/earlier-conflict") || !strings.Contains(err.Error(), "access revoked") {
				t.Fatalf("requests=%d details=%v", calls, err)
			}
		})
	}
}

func TestRefPushErrorClassificationUsesCode(t *testing.T) {
	for _, code := range []string{"", "forbidden", "validation_failed", "branch_archived", "non_fast_forward"} {
		cause := &HTTPError{Status: 409, Code: code, Message: "branch/non_fast_forward"}
		err := classifyRefPushError(cause)
		if errors.Is(err, domain.ErrSyncConflict) != (code == "non_fast_forward") || errors.Is(err, domain.ErrBranchArchived) != (code == "branch_archived") {
			t.Fatalf("code %q misclassified: %v", code, err)
		}
		var httpErr *HTTPError
		if !errors.As(err, &httpErr) || httpErr != cause {
			t.Fatalf("code %q lost server cause: %v", code, err)
		}
	}
}

func conflictTestRefs(n int) []domain.Ref {
	repoID := string(domain.HashContent([]byte("synthetic ref conflicts")))
	target := domain.HashContent([]byte("synthetic target"))
	refs := make([]domain.Ref, n)
	for i := range refs {
		refs[i] = domain.Ref{RepoID: repoID, Kind: domain.RefBranch, Name: fmt.Sprintf("feature/%05d", i), Target: target}
	}
	return refs
}

func writeRefTestError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}
