package preparation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const (
	preparationTimeout     = 5 * time.Second
	preparationGlobalLimit = 600
	preparationScopeLimit  = 30
	preparationRateWindow  = time.Minute
)

type ContextBackend interface {
	GetRepo(context.Context, domain.ContentHash) (domain.Repo, error)
}

type IdentityBackend interface {
	inbound.SnapshotReadIdentity
	ResolveMCPUser(context.Context, string, ...string) (domain.User, error)
}

type preparationRequest struct {
	repoID             domain.ContentHash
	documentIdentities []domain.DocumentIdentity
	prepare            func(context.Context) error
}

type preparationEndpoint struct {
	path        string
	maxBytes    int64
	rateKey     string
	unavailable error
	decode      func(*json.Decoder) (preparationRequest, error)
}

type preparationHandler struct {
	context   ContextBackend
	identity  IdentityBackend
	runtime   outbound.RuntimeStore
	publicURL string
	resource  string
	endpoint  preparationEndpoint
}

func NewMemoryPreparationHandler(contextBackend ContextBackend, identity IdentityBackend, preparer inbound.PreparedMemoryDemand, runtime outbound.RuntimeStore, publicURL, resourceOrigin string) (http.Handler, error) {
	if missingBackend(preparer) {
		return nil, fmt.Errorf("memory preparation requires context, snapshot identity, preparation and shared runtime backends")
	}
	endpoint := preparationEndpoint{
		path: domain.MemoryReadPreparationPath, maxBytes: domain.MaxMemoryReadPreparationBytes,
		rateKey: "mcp:memory-preparation", unavailable: domain.ErrMemoryPreparationUnavailable,
		decode: func(decoder *json.Decoder) (preparationRequest, error) {
			var body domain.MemoryReadPreparationRequest
			if err := decoder.Decode(&body); err != nil {
				return preparationRequest{}, err
			}
			if err := body.Validate(); err != nil {
				return preparationRequest{}, err
			}
			return preparationRequest{
				repoID: body.RepoID, documentIdentities: body.DocumentIdentities,
				prepare: func(ctx context.Context) error {
					return preparer.RequestPreparedMemory(ctx, body.RepoID, domain.EffectiveMemoryRequest{Selection: body.Selection, Content: body.Content, Cursor: body.Cursor})
				},
			}, nil
		},
	}
	return newPreparationHandler(contextBackend, identity, runtime, publicURL, resourceOrigin, endpoint)
}

func newPreparationHandler(contextBackend ContextBackend, identity IdentityBackend, runtime outbound.RuntimeStore, publicURL, resourceOrigin string, endpoint preparationEndpoint) (http.Handler, error) {
	if missingBackend(contextBackend) || missingBackend(identity) || missingBackend(runtime) {
		return nil, fmt.Errorf("preparation requires context, snapshot identity and shared runtime backends")
	}
	publicURL, err := canonicalOrigin(publicURL)
	if err != nil {
		return nil, err
	}
	resourceOrigin, err = canonicalOrigin(resourceOrigin)
	if err != nil {
		return nil, err
	}
	return &preparationHandler{context: contextBackend, identity: identity, runtime: runtime, publicURL: publicURL, resource: resourceOrigin + "/mcp", endpoint: endpoint}, nil
}

func missingBackend(backend any) bool {
	if backend == nil {
		return true
	}
	value := reflect.ValueOf(backend)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func canonicalOrigin(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", fmt.Errorf("memory preparation origin must be an absolute origin without credentials, path, query or fragment")
	}
	loopback := strings.EqualFold(parsed.Hostname(), "localhost")
	if address := net.ParseIP(parsed.Hostname()); address != nil {
		loopback = loopback || address.IsLoopback()
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopback) {
		return "", fmt.Errorf("memory preparation origin requires HTTPS except for loopback development")
	}
	parsed.Host = strings.ToLower(parsed.Host)
	return parsed.String(), nil
}

func (handler *preparationHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	started := time.Now()
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	ctx, cancel := context.WithTimeout(request.Context(), preparationTimeout)
	defer cancel()
	controller := http.NewResponseController(writer)
	deadline, _ := ctx.Deadline()
	server, _ := request.Context().Value(http.ServerContextKey).(*http.Server)
	if server != nil && server.ReadTimeout > 0 {
		if serverDeadline := started.Add(server.ReadTimeout); serverDeadline.Before(deadline) {
			deadline = serverDeadline
		}
	}
	if err := controller.SetReadDeadline(deadline); err != nil {
		writeFailure(writer, handler.endpoint.unavailable)
		return
	}
	if request.URL.Path != handler.endpoint.path {
		writeFailure(writer, domain.ErrNotFound)
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeJSON(writer, http.StatusMethodNotAllowed, map[string]any{"error": map[string]string{"code": "method_not_allowed"}})
		return
	}
	if raw := strings.TrimSpace(request.Header.Get("Origin")); raw != "" {
		origin, err := canonicalOrigin(raw)
		if err != nil || origin != handler.publicURL {
			writeFailure(writer, domain.ErrForbidden)
			return
		}
	}
	if !handler.allow(writer, ctx, handler.endpoint.rateKey+":global", preparationGlobalLimit) {
		return
	}
	authorization := request.Header.Get("Authorization")
	if len(authorization) > 8192+len("Bearer ") {
		writeFailure(writer, domain.ErrUnauthorized)
		return
	}
	credentials := strings.Fields(authorization)
	if len(request.Header.Values("Authorization")) != 1 || len(credentials) != 2 || !strings.EqualFold(credentials[0], "Bearer") {
		writeFailure(writer, domain.ErrUnauthorized)
		return
	}
	token := credentials[1]
	user, err := handler.identity.ResolveMCPUser(ctx, token, handler.resource)
	if err != nil {
		writeFailure(writer, err)
		return
	}
	if user.ID == "" {
		writeFailure(writer, domain.ErrUnauthorized)
		return
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		writeFailure(writer, domain.ErrValidation)
		return
	}
	if err := ctx.Err(); err != nil {
		writeFailure(writer, err)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, handler.endpoint.maxBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	body, err := handler.endpoint.decode(decoder)
	if err != nil {
		writeFailure(writer, bodyReadFailure(ctx, err))
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeFailure(writer, bodyReadFailure(ctx, err))
		return
	}
	if (server == nil || server.ReadTimeout == 0) && controller.SetReadDeadline(time.Time{}) != nil {
		writeFailure(writer, handler.endpoint.unavailable)
		return
	}
	key := handler.endpoint.rateKey + ":scope:" + string(domain.HashContent([]byte(user.ID+"\x00"+string(body.repoID))))
	if !handler.allow(writer, ctx, key, preparationScopeLimit) {
		return
	}
	ctx = inbound.WithDocumentIdentities(ctx, body.documentIdentities)
	ctx = inbound.WithDocumentReadAuthorizer(ctx, handler.readAuthorizer(user, token))
	if err := ctx.Err(); err != nil {
		writeFailure(writer, err)
		return
	}
	err = body.prepare(ctx)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		writeFailure(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]bool{"accepted": true})
}

func bodyReadFailure(ctx context.Context, err error) error {
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return context.DeadlineExceeded
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return domain.ErrValidation
}

func (handler *preparationHandler) readAuthorizer(user domain.User, token string) inbound.DocumentReadAuthorization {
	return func(ctx context.Context, repoID domain.ContentHash) (context.Context, error) {
		if err := ctx.Err(); err != nil {
			return ctx, err
		}
		current, err := handler.identity.ResolveMCPUser(ctx, token, handler.resource)
		if err != nil {
			return ctx, err
		}
		if user.ID == "" || current.ID != user.ID {
			return ctx, domain.ErrUnauthorized
		}
		bound := inbound.WithRepositoryActor(ctx, current.ID)
		lookup := inbound.WithDocumentReadAuthorizer(bound, nil)
		repo, err := handler.context.GetRepo(lookup, repoID)
		if err != nil {
			return ctx, err
		}
		if repo.ID != repoID || repo.RepositoryID == "" {
			return ctx, domain.ErrForbidden
		}
		permit, err := handler.identity.AdmitRepositoryRead(lookup, repo.RepositoryID, current.ID)
		if err != nil {
			return ctx, err
		}
		if permit == nil {
			return ctx, domain.ErrForbidden
		}
		snapshot := func(snapshotContext context.Context, requested domain.ContentHash) (context.Context, error) {
			snapshotContext = inbound.WithRepositoryActor(snapshotContext, "")
			if requested != repo.ID {
				return snapshotContext, domain.ErrForbidden
			}
			if err := snapshotContext.Err(); err != nil {
				return snapshotContext, err
			}
			fresh, err := handler.identity.ResolveMCPUserForSnapshot(snapshotContext, token, handler.resource)
			if err != nil {
				return snapshotContext, err
			}
			if fresh.ID == "" || fresh.ID != user.ID {
				return snapshotContext, domain.ErrUnauthorized
			}
			candidate, err := handler.context.GetRepo(inbound.WithDocumentReadAuthorizer(snapshotContext, nil), requested)
			if err != nil {
				return snapshotContext, err
			}
			if candidate.ID != requested || candidate.RepositoryID != repo.RepositoryID {
				return snapshotContext, domain.ErrForbidden
			}
			if err := permit.AuthorizeSnapshot(snapshotContext, candidate.RepositoryID, fresh.ID); err != nil {
				return snapshotContext, err
			}
			return inbound.WithRepositoryActor(snapshotContext, fresh.ID), nil
		}
		return inbound.WithDocumentSnapshotReadAuthorizer(bound, snapshot), nil
	}
}

func (handler *preparationHandler) allow(writer http.ResponseWriter, ctx context.Context, key string, limit int) bool {
	limited, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	allowed, err := handler.runtime.AllowRequest(limited, key, limit, preparationRateWindow, time.Time{})
	if err != nil {
		writeFailure(writer, handler.endpoint.unavailable)
		return false
	}
	if !allowed {
		writer.Header().Set("Retry-After", "60")
		writeFailure(writer, domain.ErrResourceOverloaded)
		return false
	}
	return true
}

func writeFailure(writer http.ResponseWriter, err error) {
	code, status, retryable := "unavailable", http.StatusServiceUnavailable, false
	for _, mapping := range []struct {
		cause     error
		code      string
		status    int
		retryable bool
	}{
		{domain.ErrUnauthorized, "unauthenticated", 401, false},
		{domain.ErrForbidden, "forbidden", 403, false},
		{domain.ErrBreakGlassAuditUnavailable, "audit_unavailable", 503, false},
		{domain.ErrIntegrity, "integrity_violation", 503, false},
		{domain.ErrMemoryReadNeedsAttention, "memory_read_needs_attention", 503, false},
		{domain.ErrEffectiveMemoryCursorStale, "memory_cursor_stale", 409, false},
		{domain.ErrMemoryCaptureExpired, "memory_capture_expired", 409, false},
		{domain.ErrCatalogResetRequired, "catalog_reset_required", 409, false},
		{domain.ErrCatalogUnsupported, "prepared_memory_unavailable", 503, false},
		{domain.ErrUnsupportedDocumentIdentity, "unsupported_document_identity", 409, false},
		{domain.ErrDocumentIdentityUpgradeRequired, "document_identity_upgrade_required", 409, false},
		{domain.ErrDocVisibleGenerationStale, "doc_visible_generation_stale", 409, false},
		{domain.ErrDocFindFieldsUnavailable, "doc_find_fields_unavailable", 503, false},
		{domain.ErrDocFindDisplayUnavailable, "doc_find_display_unavailable", 503, false},
		{domain.ErrMemoryPreparationUnavailable, "memory_preparation_unavailable", 503, true},
		{domain.ErrConversationFindPreparationUnavailable, "conversation_find_preparation_unavailable", 503, true},
		{domain.ErrConflict, "conflict", 409, false},
		{context.Canceled, "canceled", 503, false},
		{domain.ErrValidation, "validation", 422, false},
		{domain.ErrNotFound, "not_found", 404, false},
		{domain.ErrMemoryProjectionLimit, "memory_projection_limit", 422, false},
		{domain.ErrResourceOverloaded, "resource_overloaded", 429, true},
		{context.DeadlineExceeded, "deadline_exceeded", 503, true},
		{domain.ErrMemoryReadNotReady, "memory_read_not_ready", 503, true},
		{domain.ErrDocRangeIndexUnavailable, "doc_range_index_unavailable", 503, true},
		{domain.ErrDocRangeIndexPending, "doc_range_index_pending", 503, true},
		{domain.ErrDocFindFieldsPending, "doc_find_fields_pending", 503, true},
	} {
		if errors.Is(err, mapping.cause) {
			code, status, retryable = mapping.code, mapping.status, mapping.retryable
			break
		}
	}
	if code == "memory_read_not_ready" && !domain.IsOnlyMemoryReadPending(err) {
		code, status, retryable = "unavailable", http.StatusServiceUnavailable, false
	}
	writeJSON(writer, status, map[string]any{"error": map[string]any{"code": code, "retryable": retryable}})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
