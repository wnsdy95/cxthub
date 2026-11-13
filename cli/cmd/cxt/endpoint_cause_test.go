package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

type endpointCauseTransport func(*http.Request) (*http.Response, error)

func (f endpointCauseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func endpointCauseClient(t *testing.T, connectionFailures int, failure func(*http.Request) (*http.Response, error)) (*backendclient.BackendClient, remotecfg.EndpointBinding, *int) {
	t.Helper()
	repoID := remotecfg.RepoIDFor(endpointTestIdentity)
	binding := remotecfg.EndpointBinding{RemoteURL: endpointTestIdentity, RepoID: repoID, RepositoryID: "repository-fixture", Endpoint: endpointTestDestination}
	calls := 0
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = endpointCauseTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/api/v1/repository-connections" {
			calls++
			if calls <= connectionFailures {
				return failure(request)
			}
			return capturePushJSON(t, request, 200, domain.RepositoryConnection{RepoID: repoID, RemoteURL: endpointTestIdentity, RepositoryID: "repository-fixture"}), nil
		}
		return capturePushJSON(t, request, 200, map[string]any{"id": repoID, "remote_url": endpointTestIdentity, "repository_id": "repository-fixture", "default_branch": "main", "context_protocol": 1}), nil
	})
	client := backendclient.NewBackendClient(func() string { return endpointTestDestination }, func() string { return "synthetic-token" }, domain.TeamIdentity{})
	return client, binding, &calls
}

func overloaded(request *http.Request) (*http.Response, error) {
	body := `{"error":{"code":"resource_overloaded","message":"resource capacity unavailable; retry later"}}`
	return &http.Response{StatusCode: 503, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
}

func TestEndpointVerificationRetriesTransientUnderUploadPolicy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, binding, calls := endpointCauseClient(t, 2, overloaded)
		ctx := outbound.WithUploadPolicy(context.Background(), outbound.UploadPolicy{StallBudget: time.Minute})
		if _, err := verifyEndpointRepository(ctx, client, binding); err != nil || *calls != 3 {
			t.Fatalf("transient verification failure burned the attempt: err=%v calls=%d", err, *calls)
		}
	})
}

func TestEndpointVerificationKeepsClassifiedCause(t *testing.T) {
	client, binding, _ := endpointCauseClient(t, 1, overloaded)
	_, err := verifyEndpointRepository(context.Background(), client, binding)
	var rejection *backendclient.HTTPError
	if !errors.As(err, &rejection) || rejection.Status != 503 || rejection.Code != "resource_overloaded" || !strings.Contains(err.Error(), "connection verification failed") {
		t.Fatalf("verification hid its cause: %v", err)
	}
}

func TestEndpointVerificationDropsRequestURL(t *testing.T) {
	refused := errors.New("synthetic connection refused")
	client, binding, _ := endpointCauseClient(t, 1, func(*http.Request) (*http.Response, error) { return nil, refused })
	_, err := verifyEndpointRepository(context.Background(), client, binding)
	if !errors.Is(err, refused) || strings.Contains(err.Error(), "remote_url") || strings.Contains(err.Error(), "localhost:5173") {
		t.Fatalf("verification error leaked the request URL or lost its cause: %v", err)
	}
}
