package backendclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func TestEndpointTransportResolvesCredentialAndDestinationTogether(t *testing.T) {
	lookups, requests := 0, 0
	client := NewResolvedBackendClient(func() (string, string, error) {
		lookups++
		if lookups == 1 {
			return "https://first.test/api/v1", "first-synthetic-secret", nil
		}
		return "https://second.test/api/v1", "second-synthetic-secret", nil
	}, domain.TeamIdentity{})
	client.httpc.Transport = reviewTransferRoundTripper(func(request *http.Request) (*http.Response, error) {
		requests++
		host, token := "first.test", "first-synthetic-secret"
		if requests == 2 {
			host, token = "second.test", "second-synthetic-secret"
		}
		if lookups != requests || request.URL.Host != host || request.Header.Get("Authorization") != "Bearer "+token {
			t.Fatal("endpoint and credential came from different observations")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}, Request: request}, nil
	})
	for attempt := 0; attempt < 2; attempt++ {
		var result map[string]any
		if err := client.do(context.Background(), http.MethodGet, "/me", nil, &result); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEndpointResolutionErrorDoesNotReachTransport(t *testing.T) {
	expected := errors.New("invalid local binding")
	client := NewResolvedBackendClient(func() (string, string, error) {
		return "", "", expected
	}, domain.TeamIdentity{})
	client.httpc.Transport = reviewTransferRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("failed endpoint resolution reached network")
		return nil, nil
	})
	var result map[string]any
	if err := client.do(context.Background(), http.MethodGet, "/me", nil, &result); !errors.Is(err, expected) {
		t.Fatal(err)
	}
}
