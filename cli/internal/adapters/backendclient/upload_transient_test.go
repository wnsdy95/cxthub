package backendclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// proxyResponse models a gateway failure that never reached the app: an
// empty (or HTML) body and no application error code.
func proxyResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/html"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}
}

func TestPatientUploadRetriesCodelessProxyFailures(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				upload := patientChunkFixture(t, 1, func(request *http.Request, _ int) *http.Response {
					calls++
					if calls <= 3 {
						return proxyResponse(request, status, "")
					}
					return reviewWireResponse(request, http.StatusOK, nil)
				})
				if err := upload(patient(time.Minute)); err != nil || calls != 4 {
					t.Fatalf("proxy failure was not transient: err=%v calls=%d", err, calls)
				}
				// Without a policy the bounded contract does not retry it.
				calls = 0
				var rejection *HTTPError
				if err := upload(context.Background()); !errors.As(err, &rejection) || rejection.Status != status || calls != 1 {
					t.Fatalf("legacy contract changed: err=%v calls=%d", err, calls)
				}
			})
		})
	}
}

func TestPatientUploadKeepsApplicationFailuresTerminal(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		code   string
	}{
		{"app 503 other code", http.StatusServiceUnavailable, "maintenance"},
		{"app 502 with code", http.StatusBadGateway, "upstream_rejected"},
		{"internal error", http.StatusInternalServerError, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				upload := patientChunkFixture(t, 1, func(request *http.Request, _ int) *http.Response {
					calls++
					if test.code == "" {
						return proxyResponse(request, test.status, "")
					}
					return reviewWireResponse(request, test.status, map[string]any{"error": map[string]string{"code": test.code, "message": "no"}})
				})
				if err := upload(patient(time.Minute)); err == nil || calls != 1 {
					t.Fatalf("terminal failure retried: err=%v calls=%d", err, calls)
				}
			})
		})
	}
}

func TestPatientSettingsObjectAndNegotiationRetryProxyFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := string(domain.HashContent([]byte("settings retry repo")))
		bundle := domain.SettingsBundle{Kind: "claude"}
		hash, err := domain.SettingsObjectHash(bundle)
		if err != nil {
			t.Fatal(err)
		}
		calls := map[string]int{}
		client := NewBackendClient(func() string { return "https://synthetic.invalid" }, func() string { return "" }, domain.TeamIdentity{})
		client.httpc.Transport = reviewTransferRoundTripper(func(request *http.Request) (*http.Response, error) {
			calls[request.Method]++
			if calls[request.Method] <= 2 {
				return proxyResponse(request, http.StatusServiceUnavailable, ""), nil
			}
			if request.Method == http.MethodPost {
				return reviewWireResponse(request, http.StatusOK, negotiateResp{}), nil
			}
			return reviewWireResponse(request, http.StatusOK, nil), nil
		})
		if err := client.PushSettingsObject(patient(time.Minute), repo, hash, bundle); err != nil || calls[http.MethodPut] != 3 {
			t.Fatalf("settings PUT not retried: err=%v calls=%d", err, calls[http.MethodPut])
		}
		var neg negotiateResp
		if err := client.doIdempotent(patient(time.Minute), http.MethodPost, client.reposPath(repo)+"/push/negotiate", negotiateReq{}, &neg); err != nil || calls[http.MethodPost] != 3 {
			t.Fatalf("negotiation not retried: err=%v calls=%d", err, calls[http.MethodPost])
		}
		// Without a policy the first proxy failure is returned unchanged.
		calls = map[string]int{}
		if err := client.PushSettingsObject(context.Background(), repo, hash, bundle); err == nil || calls[http.MethodPut] != 1 || !strings.Contains(err.Error(), "Server error (503)") {
			t.Fatalf("unpatient settings PUT changed: err=%v calls=%d", err, calls[http.MethodPut])
		}
	})
}

func TestPatientRetryBoundedByStallBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		upload := patientChunkFixture(t, 1, func(request *http.Request, _ int) *http.Response {
			calls++
			return proxyResponse(request, http.StatusGatewayTimeout, "")
		})
		// The stall budget covers consecutive retries of one batch; the
		// jittered pre-batch yield (at most 2*uploadBatchYield) precedes it.
		started := time.Now()
		if err := upload(patient(20 * time.Second)); err == nil || time.Since(started) > 20*time.Second+2*uploadBatchYield || calls < 2 {
			t.Fatalf("stall budget not honored: err=%v elapsed=%s calls=%d", err, time.Since(started), calls)
		}
	})
}

// A small server can take longer than the ordinary 30s to commit an object
// batch. A patient request gets a longer per-request bound, and a request
// that still times out is retried while the caller's budget remains.
func TestPatientUploadOutlivesShortTimeoutAndRetriesTimedOutRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		var budgets []time.Duration
		upload := patientChunkFixture(t, 1, func(request *http.Request, _ int) *http.Response {
			calls++
			deadline, _ := request.Context().Deadline()
			budgets = append(budgets, time.Until(deadline))
			if calls == 1 {
				<-request.Context().Done()
				return nil
			}
			time.Sleep(45 * time.Second) // slower than the hook-safe 30s client timeout
			return reviewWireResponse(request, http.StatusOK, nil)
		})
		if err := upload(patient(10 * time.Minute)); err != nil || calls != 2 {
			t.Fatalf("timed-out request was not retried to success: err=%v calls=%d", err, calls)
		}
		if budgets[0] < 90*time.Second {
			t.Fatalf("patient request kept the short budget: %s", budgets[0])
		}
	})
}

func TestPatientUploadStopsWhenCallerDeadlineEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		upload := patientChunkFixture(t, 1, func(request *http.Request, _ int) *http.Response {
			calls++
			<-request.Context().Done()
			return nil
		})
		ctx, cancel := context.WithTimeout(patient(time.Hour), 3*time.Minute)
		defer cancel()
		if err := upload(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("caller deadline not honored: %v", err)
		}
		if calls > 2 {
			t.Fatalf("retried past the caller deadline: calls=%d", calls)
		}
	})
}

func TestPatientIdempotentRequestRetriesClientTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := string(domain.HashContent([]byte("timeout retry repo")))
		calls := 0
		client := NewBackendClient(func() string { return "https://synthetic.invalid" }, func() string { return "" }, domain.TeamIdentity{})
		client.httpc.Transport = reviewTransferRoundTripper(func(request *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				<-request.Context().Done()
				return nil, request.Context().Err()
			}
			return reviewWireResponse(request, http.StatusOK, negotiateResp{}), nil
		})
		var neg negotiateResp
		if err := client.doIdempotent(patient(time.Minute), http.MethodPost, client.reposPath(repo)+"/push/negotiate", negotiateReq{}, &neg); err != nil || calls != 2 {
			t.Fatalf("client timeout not retried: err=%v calls=%d", err, calls)
		}
		calls = 0
		if err := client.doIdempotent(context.Background(), http.MethodPost, client.reposPath(repo)+"/push/negotiate", negotiateReq{}, &neg); err == nil || calls != 1 {
			t.Fatalf("unpatient request retried: err=%v calls=%d", err, calls)
		}
	})
}

func TestHTTPErrorNeverCopiesProxyPagesOrTokens(t *testing.T) {
	token := strings.Repeat("Q7x_", 24) + ".Nt" // synthetic opaque run; never a real credential
	html := newHTTPError(502, http.MethodPost, "/repos/x/push/objects", []byte("<!DOCTYPE html><html><script>window.__cf={t:'"+token+"'}</script></html>"))
	if strings.Contains(html.Error(), token) || strings.Contains(html.Error(), "<html") || !strings.Contains(html.Error(), "non-JSON response body omitted") {
		t.Fatalf("proxy page copied into error: %s", html.Error())
	}
	text := newHTTPError(503, http.MethodGet, "/repos/x", []byte("upstream reset token="+token))
	app := newHTTPError(503, http.MethodGet, "/repos/x", []byte(`{"error":{"code":"resource_overloaded","message":"busy `+token+`"}}`))
	for _, err := range []*HTTPError{text, app} {
		if strings.Contains(err.Error(), token) {
			t.Fatalf("opaque token copied into error: %s", err.Error())
		}
	}
	if app.Code != "resource_overloaded" || !strings.Contains(app.Error(), "busy") {
		t.Fatalf("application classification lost: %s", app.Error())
	}
}

// A manual push's preparation reads (history, repository, catalogs) wait out
// the same transient failures as its uploads.
func TestPatientReadsRetryTransientFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		client := NewBackendClient(func() string { return "https://synthetic.invalid" }, func() string { return "" }, domain.TeamIdentity{})
		client.httpc.Transport = reviewTransferRoundTripper(func(request *http.Request) (*http.Response, error) {
			calls++
			switch calls {
			case 1:
				return uploadOverloadResponse(request), nil
			case 2:
				return proxyResponse(request, http.StatusBadGateway, ""), nil
			}
			return reviewWireResponse(request, http.StatusOK, map[string]any{"events": []any{}}), nil
		})
		var out map[string]any
		if err := client.do(patient(time.Minute), http.MethodGet, "/repos/x/history", nil, &out); err != nil || calls != 3 {
			t.Fatalf("preparation read failed fast: err=%v calls=%d", err, calls)
		}
		calls = 0
		if err := client.do(context.Background(), http.MethodGet, "/repos/x/history", nil, &out); err == nil || calls != 1 {
			t.Fatalf("read without a policy changed: err=%v calls=%d", err, calls)
		}
		// Mutations stay single-shot unless routed through doIdempotent.
		calls = 0
		if err := client.do(patient(time.Minute), http.MethodDelete, "/repos/x/pending/y", nil, nil); err == nil || calls != 1 {
			t.Fatalf("mutation retried implicitly: err=%v calls=%d", err, calls)
		}
	})
}

// Recording history is the last step of a long push. The server accepts an
// exact replay unchanged, so a slow or shedding server must not discard the
// whole publication at this point.
func TestPatientHistoryRecordRetriesTransientFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(domain.HashContent([]byte("repo"))), BranchID: "identity", Branch: "main", Kind: "position", CreatedAt: time.Unix(1, 0).UTC(), BindingParent: strings.Repeat("2", 32)}
		calls := 0
		client := NewBackendClient(func() string { return "https://synthetic.invalid" }, func() string { return "" }, domain.TeamIdentity{})
		client.httpc.Transport = reviewTransferRoundTripper(func(request *http.Request) (*http.Response, error) {
			calls++
			switch calls {
			case 1:
				<-request.Context().Done() // client timeout while awaiting headers
				return nil, request.Context().Err()
			case 2:
				return uploadOverloadResponse(request, "3"), nil
			}
			return reviewWireResponse(request, http.StatusOK, map[string]string{"id": e.ID}), nil
		})
		if err := client.PushHistoryEvent(patient(10*time.Minute), e); err != nil || calls != 3 {
			t.Fatalf("history record failed fast: err=%v calls=%d", err, calls)
		}
		calls = 0
		if err := client.PushHistoryEvent(context.Background(), e); err == nil || calls != 1 {
			t.Fatalf("history record without a policy changed: err=%v calls=%d", err, calls)
		}
	})
}
