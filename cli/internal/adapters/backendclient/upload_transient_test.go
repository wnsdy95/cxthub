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
		started := time.Now()
		if err := upload(patient(20 * time.Second)); err == nil || time.Since(started) > 20*time.Second || calls < 2 {
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
