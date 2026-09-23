// Package resend implements only outbound email delivery; invitation authority
// and retry policy belong to the application layer.
package resend

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type Mailer struct {
	key    string
	client *http.Client
}

func New(key string) *Mailer {
	return &Mailer{key: strings.TrimSpace(key), client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (m *Mailer) Send(ctx context.Context, key string, message domain.EmailMessage) (string, error) {
	raw, err := json.Marshal(message)
	if err != nil {
		return "", &outbound.EmailDeliveryError{Reason: "invalid_message"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.resend.com/emails", bytes.NewReader(raw))
	if err != nil {
		return "", &outbound.EmailDeliveryError{Reason: "invalid_message"}
	}
	req.Header.Set("Authorization", "Bearer "+m.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	resp, err := m.client.Do(req)
	if err != nil {
		return "", &outbound.EmailDeliveryError{Reason: "transport_failed", Retryable: true}
	}
	defer resp.Body.Close()
	var result struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&result)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if decodeErr != nil || strings.TrimSpace(result.ID) == "" {
			return "", &outbound.EmailDeliveryError{Reason: "invalid_response", Retryable: true}
		}
		return result.ID, nil
	}
	e := &outbound.EmailDeliveryError{Reason: "provider_rejected"}
	switch {
	case resp.StatusCode == 401:
		e.Reason = "invalid_api_key"
	case resp.StatusCode == 403:
		e.Reason = "sender_not_authorized"
	case resp.StatusCode == 409 && result.Name == "concurrent_idempotent_requests":
		e.Reason = "request_in_progress"
		e.Retryable = true
	case resp.StatusCode == 409:
		e.Reason = "idempotency_conflict"
	case resp.StatusCode == 408 || resp.StatusCode == 425 || resp.StatusCode == 429 || resp.StatusCode >= 500:
		e.Reason = "provider_temporary"
		e.Retryable = true
	}
	if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
		e.RetryAfter = time.Duration(min(seconds, 86400)) * time.Second
	} else if at, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil {
		e.RetryAfter = time.Until(at)
	}
	return "", e
}
