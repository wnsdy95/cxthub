package memorypreparation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type Client struct {
	endpoint             string
	conversationEndpoint string
	http                 *http.Client
}

var _ outbound.MemoryPreparationRequester = (*Client)(nil)
var _ outbound.ConversationFindPreparationRequester = (*Client)(nil)

type preparationPayload interface {
	Validate() error
}

type preparationEndpoint struct {
	url         string
	maxBytes    int64
	unavailable error
	rejection   func(int, string) error
}

var errMemoryPreparationFailed = errors.New("memory preparation request failed")

func NewClient(origin string) (*Client, error) {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" ||
		parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery ||
		strings.Contains(origin, "#") || strings.TrimSpace(origin) != origin {
		return nil, domain.ErrValidation
	}
	hostname := parsed.Hostname()
	if hostname == "" || strings.ContainsAny(hostname, "%\\ \t\r\n") || strings.HasSuffix(parsed.Host, ":") {
		return nil, domain.ErrValidation
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return nil, domain.ErrValidation
		}
	}
	address := net.ParseIP(hostname)
	if (strings.Contains(hostname, ":") && !strings.HasPrefix(parsed.Host, "[")) ||
		(strings.HasPrefix(parsed.Host, "[") && address == nil) {
		return nil, domain.ErrValidation
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if !strings.EqualFold(hostname, "localhost") && (address == nil || !address.IsLoopback()) {
			return nil, domain.ErrValidation
		}
	default:
		return nil, domain.ErrValidation
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	return &Client{
		endpoint:             parsed.String() + domain.MemoryReadPreparationPath,
		conversationEndpoint: parsed.String() + domain.ConversationFindPreparationPath,
		http: &http.Client{
			Timeout: 5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				Protocols:              protocols,
				DisableKeepAlives:      true,
				DisableCompression:     true,
				MaxResponseHeaderBytes: domain.MaxMemoryReadPreparationBytes,
			},
		},
	}, nil
}

func (client *Client) RequestMemoryPreparation(ctx context.Context, bearer string, input domain.MemoryReadPreparationRequest) error {
	return client.requestPreparation(ctx, bearer, input, preparationEndpoint{
		url: client.endpoint, maxBytes: domain.MaxMemoryReadPreparationBytes,
		unavailable: domain.ErrMemoryPreparationUnavailable, rejection: preparationRejection,
	})
}

func (client *Client) requestPreparation(ctx context.Context, bearer string, input preparationPayload, endpoint preparationEndpoint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if bearer == "" || len(bearer) > 8192 || strings.TrimSpace(bearer) != bearer {
		return domain.ErrUnauthorized
	}
	for _, character := range bearer {
		if character < ' ' || character == '\x7f' {
			return domain.ErrUnauthorized
		}
	}
	if err := input.Validate(); err != nil {
		if errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
			return domain.ErrUnsupportedDocumentIdentity
		}
		return domain.ErrValidation
	}
	body, err := json.Marshal(input)
	if err != nil || int64(len(body)) > endpoint.maxBytes {
		return domain.ErrValidation
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.url, bytes.NewReader(body))
	if err != nil {
		return endpoint.unavailable
	}
	request.GetBody = nil
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		return preparationTransportError(ctx, err, endpoint.unavailable)
	}
	defer response.Body.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" || response.ContentLength > endpoint.maxBytes {
		return endpoint.unavailable
	}
	body, err = io.ReadAll(io.LimitReader(response.Body, endpoint.maxBytes+1))
	if err != nil {
		return preparationTransportError(ctx, err, endpoint.unavailable)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if int64(len(body)) > endpoint.maxBytes {
		return endpoint.unavailable
	}
	envelope, valid := preparationJSONObject(body)
	if !valid {
		return endpoint.unavailable
	}
	if response.StatusCode == http.StatusAccepted {
		if len(envelope) == 1 && bytes.Equal(bytes.TrimSpace(envelope["accepted"]), []byte("true")) {
			return nil
		}
		return endpoint.unavailable
	}
	failure, valid := preparationJSONObject(envelope["error"])
	if !valid {
		return endpoint.unavailable
	}
	var code string
	if json.Unmarshal(failure["code"], &code) != nil {
		return endpoint.unavailable
	}
	return endpoint.rejection(response.StatusCode, code)
}

func preparationTransportError(ctx context.Context, err error, unavailable error) error {
	if canceled := ctx.Err(); canceled != nil {
		return canceled
	}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() {
		return context.DeadlineExceeded
	}
	return unavailable
}

func preparationJSONObject(body []byte) (map[string]json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, false
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, false
		}
		name, valid := token.(string)
		if _, duplicate := fields[name]; !valid || duplicate {
			return nil, false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, false
		}
		fields[name] = value
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		return nil, false
	}
	var trailing json.RawMessage
	if decoder.Decode(&trailing) != io.EOF {
		return nil, false
	}
	return fields, true
}

func preparationRejection(status int, code string) error {
	for _, mapping := range []struct {
		status int
		code   string
		cause  error
	}{
		{http.StatusUnauthorized, "unauthenticated", domain.ErrUnauthorized},
		{http.StatusForbidden, "forbidden", domain.ErrForbidden},
		{http.StatusNotFound, "not_found", domain.ErrNotFound},
		{http.StatusUnprocessableEntity, "validation", domain.ErrValidation},
		{http.StatusServiceUnavailable, "integrity_violation", domain.ErrIntegrity},
		{http.StatusUnprocessableEntity, "memory_projection_limit", domain.ErrMemoryProjectionLimit},
		{http.StatusConflict, "conflict", domain.ErrConflict},
		{http.StatusConflict, "catalog_reset_required", domain.ErrCatalogResetRequired},
		{http.StatusConflict, "memory_cursor_stale", domain.ErrEffectiveMemoryCursorStale},
		{http.StatusConflict, "unsupported_document_identity", domain.ErrUnsupportedDocumentIdentity},
		{http.StatusConflict, "document_identity_upgrade_required", domain.ErrDocumentIdentityUpgradeRequired},
		{http.StatusConflict, "memory_capture_expired", domain.ErrMemoryCaptureExpired},
		{http.StatusServiceUnavailable, "audit_unavailable", domain.ErrBreakGlassAuditUnavailable},
		{http.StatusServiceUnavailable, "memory_read_needs_attention", domain.ErrMemoryReadNeedsAttention},
		{http.StatusServiceUnavailable, "prepared_memory_unavailable", domain.ErrCatalogUnsupported},
		{http.StatusServiceUnavailable, "memory_preparation_unavailable", domain.ErrMemoryPreparationUnavailable},
		{http.StatusServiceUnavailable, "unavailable", errMemoryPreparationFailed},
		{http.StatusTooManyRequests, "resource_overloaded", domain.ErrResourceOverloaded},
		{http.StatusServiceUnavailable, "memory_read_not_ready", domain.ErrMemoryReadNotReady},
		{http.StatusServiceUnavailable, "deadline_exceeded", context.DeadlineExceeded},
		{http.StatusServiceUnavailable, "canceled", context.Canceled},
	} {
		if status == mapping.status && code == mapping.code {
			return mapping.cause
		}
	}
	return domain.ErrMemoryPreparationUnavailable
}
