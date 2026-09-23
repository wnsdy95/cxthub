package resend

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestResendDelivery(t *testing.T) {
	for _, tt := range []struct {
		status       int
		body, reason string
		retry        bool
	}{
		{200, `{"id":"provider-id"}`, "", false},
		{200, `{}`, "invalid_response", true},
		{401, `{"message":"secret must not leak"}`, "invalid_api_key", false},
		{403, `{}`, "sender_not_authorized", false},
		{409, `{"name":"concurrent_idempotent_requests"}`, "request_in_progress", true},
		{409, `{"name":"invalid_idempotent_request"}`, "idempotency_conflict", false},
		{429, `{}`, "provider_temporary", true}, {503, `{}`, "provider_temporary", true}, {302, `{}`, "provider_rejected", false},
	} {
		t.Run(tt.reason+tt.body, func(t *testing.T) {
			m := New("synthetic-key")
			m.client.Transport = transport(func(req *http.Request) (*http.Response, error) {
				if req.URL.String() != "https://api.resend.com/emails" || req.Header.Get("Authorization") != "Bearer synthetic-key" || req.Header.Get("Idempotency-Key") != "invite/test" {
					t.Fatal("request contract", req.URL)
				}
				body, _ := io.ReadAll(req.Body)
				if !strings.Contains(string(body), `"to":["test@example.test"]`) {
					t.Fatal(string(body))
				}
				return &http.Response{StatusCode: tt.status, Body: io.NopCloser(strings.NewReader(tt.body)), Header: http.Header{"Retry-After": []string{"120"}}}, nil
			})
			id, err := m.Send(context.Background(), "invite/test", domain.EmailMessage{From: "from@example.test", To: []string{"test@example.test"}, Text: "hello"})
			if tt.reason == "" {
				if id != "provider-id" || err != nil {
					t.Fatal(id, err)
				}
				return
			}
			var failure *outbound.EmailDeliveryError
			if !errors.As(err, &failure) || failure.Reason != tt.reason || failure.Retryable != tt.retry {
				t.Fatalf("%+v", err)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "synthetic-key") {
				t.Fatal("secret leaked")
			}
			if tt.status == 429 && failure.RetryAfter != 2*time.Minute {
				t.Fatal(failure.RetryAfter)
			}
		})
	}
}
func TestResendTransportFailureRedactsErrors(t *testing.T) {
	m := New("synthetic-key")
	m.client.Transport = transport(func(*http.Request) (*http.Response, error) { return nil, errors.New("private network or credentials") })
	_, err := m.Send(context.Background(), "test", domain.EmailMessage{})
	if err.Error() != "transport_failed" {
		t.Fatal(err)
	}
	if m.client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("redirects must not forward credentials")
	}
}
