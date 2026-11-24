package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Every error the server returns to a client has one code and one status,
// whether it arrives bare or wrapped in what the caller was doing.
func TestEveryErrorHasOneCodeAndOneStatus(t *testing.T) {
	answers := []struct {
		err    error
		code   string
		status int
	}{
		{domain.ErrResourceOverloaded, "resource_overloaded", http.StatusServiceUnavailable},
		{domain.ErrDocRangeIndexUnavailable, "doc_range_index_unavailable", http.StatusServiceUnavailable},
		{domain.ErrFederationUnavailable, "identity_connection_unavailable", http.StatusServiceUnavailable},
		{domain.ErrVerifiedDomainRequired, "verified_domain_required", http.StatusConflict},
		{domain.ErrRecentIdentityLogin, "recent_identity_login_required", http.StatusUnauthorized},
		{domain.ErrDomainVerificationUnavailable, "domain_verification_unavailable", http.StatusServiceUnavailable},
		{domain.ErrSecretsRevisionRequired, "revision_required", http.StatusPreconditionRequired},
		{domain.ErrSecretsConflict, "secrets_conflict", http.StatusConflict},
		{domain.ErrSecretsFingerprintRequired, "fingerprint_required", http.StatusBadRequest},
		{domain.ErrSecretsRotateConflict, "rotate_conflict", http.StatusConflict},
		{domain.ErrSecretsPassphraseMismatch, "passphrase_mismatch", http.StatusConflict},
		{domain.ErrSecretsConsistency, "consistency_check_failed", http.StatusServiceUnavailable},
		{domain.ErrSecretsMalformedEnvelope, "bad_request", http.StatusBadRequest},
		{domain.ErrStorageLimit, "storage_limit", http.StatusConflict},
		{domain.ErrUsageUnavailable, "usage_unavailable", http.StatusServiceUnavailable},
		{domain.ErrNotFound, "not_found", http.StatusNotFound},
		{domain.ErrIntegrity, "integrity_violation", http.StatusUnprocessableEntity},
		{domain.ErrRefConflict, "ref_conflict", http.StatusConflict},
		{domain.ErrUnauthorized, "unauthenticated", http.StatusUnauthorized},
		{domain.ErrBreakGlassAuditUnavailable, "audit_unavailable", http.StatusServiceUnavailable},
		{domain.ErrForbidden, "forbidden", http.StatusForbidden},
		{domain.ErrGitOriginMismatch, "git_origin_mismatch", http.StatusConflict},
		{domain.ErrUnsupportedCIRVersion, "unsupported_cir_version", http.StatusConflict},
		{domain.ErrDocumentIdentityUpgradeRequired, "document_identity_upgrade_required", http.StatusConflict},
		{domain.ErrRootPublicationDisabled, "root_publication_disabled", http.StatusConflict},
		{domain.ErrUnsupportedDocumentIdentity, "unsupported_document_identity", http.StatusConflict},
		{domain.ErrConflict, "conflict", http.StatusConflict},
		{domain.ErrValidation, "validation", http.StatusUnprocessableEntity},
	}
	codes := map[string]error{}
	for _, answer := range answers {
		for name, err := range map[string]error{"bare": answer.err, "wrapped": fmt.Errorf("while reading a session: %w", answer.err)} {
			if code, status := mapError(err); code != answer.code || status != answer.status {
				t.Errorf("%v (%s): %s %d, want %s %d", answer.err, name, code, status, answer.code, answer.status)
			}
		}
		// No two errors share a code: a client tells them apart by it.
		if earlier, taken := codes[answer.code]; taken {
			t.Errorf("%v and %v are both answered as %s", earlier, answer.err, answer.code)
		}
		codes[answer.code] = answer.err
	}

	// An error the server does not know is its own failure, and only that.
	for _, err := range []error{errors.New("synthetic: a driver failed"), fmt.Errorf("wrapped: %w", errors.New("synthetic")), http.ErrBodyNotAllowed} {
		if code, status := mapError(err); code != "internal" || status != http.StatusInternalServerError {
			t.Errorf("an unknown error %v: %s %d", err, code, status)
		}
	}

	// A refusal that already carries its own answer keeps it, whatever caused it.
	refusal := &httpAccessError{status: http.StatusForbidden, code: "repo_unbound", message: "synthetic", cause: domain.ErrNotFound}
	for name, err := range map[string]error{"bare": refusal, "wrapped": fmt.Errorf("while reading: %w", refusal)} {
		if code, status := mapError(err); code != "repo_unbound" || status != http.StatusForbidden {
			t.Errorf("a refusal with its own answer (%s): %s %d", name, code, status)
		}
	}

	// Secrets that could not be verified stay that answer whatever the store said underneath.
	for _, cause := range []error{domain.ErrNotFound, domain.ErrIntegrity, domain.ErrConflict, domain.ErrValidation, domain.ErrForbidden} {
		if code, status := mapError(fmt.Errorf("%w: %w", domain.ErrSecretsConsistency, cause)); code != "consistency_check_failed" || status != http.StatusServiceUnavailable {
			t.Errorf("unverifiable secrets over %v: %s %d", cause, code, status)
		}
	}
}

type errorEnvelope struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func respondedError(t *testing.T, prepare func(http.ResponseWriter), err error) (*httptest.ResponseRecorder, errorEnvelope) {
	t.Helper()
	response := httptest.NewRecorder()
	if prepare != nil {
		prepare(response)
	}
	(&Server{}).respond(response, nil, err)
	var envelope errorEnvelope
	decoder := json.NewDecoder(strings.NewReader(response.Body.String()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatalf("the error body is not the envelope: %v: %s", err, response.Body)
	}
	if got := response.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("an error was answered as %q", got)
	}
	if envelope.Error.Details == nil || len(envelope.Error.Details) != 0 {
		t.Errorf("error details: %s", response.Body)
	}
	return response, envelope
}

// What a client reads in an error answer: when to come back, and a message
// that says nothing about the inside of the server.
func TestErrorAnswerSaysWhenToComeBackAndHidesTheInside(t *testing.T) {
	const private = "synthetic-private-detail"
	// A busy server and an index that is not there ask the client to come back in a second.
	for _, err := range []error{domain.ErrResourceOverloaded, domain.ErrDocRangeIndexUnavailable} {
		response, envelope := respondedError(t, nil, fmt.Errorf("%s: %w", private, err))
		if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" {
			t.Errorf("%v: status %d, Retry-After %q", err, response.Code, response.Header().Get("Retry-After"))
		}
		if envelope.Error.Message == "" {
			t.Errorf("%v: no message", err)
		}
		// A handler that already asked for a longer wait keeps it.
		response, _ = respondedError(t, func(writer http.ResponseWriter) { writer.Header().Set("Retry-After", "7") }, err)
		if response.Header().Get("Retry-After") != "7" {
			t.Errorf("%v: a longer wait was replaced by %q", err, response.Header().Get("Retry-After"))
		}
	}
	// No other answer tells a client to come back.
	for _, err := range []error{domain.ErrNotFound, domain.ErrConflict, domain.ErrValidation, domain.ErrForbidden, domain.ErrUnauthorized,
		domain.ErrIntegrity, domain.ErrUsageUnavailable, domain.ErrSecretsConsistency, errors.New("synthetic")} {
		if response, _ := respondedError(t, nil, err); response.Header().Get("Retry-After") != "" {
			t.Errorf("%v: Retry-After %q", err, response.Header().Get("Retry-After"))
		}
	}
	// The server's own failure, unverifiable secrets and a failed audit are
	// answered with a fixed sentence: what went wrong inside is not shown.
	for name, s := range map[string]struct {
		err     error
		code    string
		status  int
		message string
	}{
		"an unknown failure":   {fmt.Errorf("%s at a private address", private), "internal", http.StatusInternalServerError, "internal server error"},
		"unverifiable secrets": {fmt.Errorf("%w: %s", domain.ErrSecretsConsistency, private), "consistency_check_failed", http.StatusServiceUnavailable, domain.ErrSecretsConsistency.Error()},
		"a failed audit":       {errors.Join(domain.ErrBreakGlassAuditUnavailable, errors.New(private)), "audit_unavailable", http.StatusServiceUnavailable, domain.ErrBreakGlassAuditUnavailable.Error()},
	} {
		response, envelope := respondedError(t, nil, s.err)
		if response.Code != s.status || envelope.Error.Code != s.code || envelope.Error.Message != s.message || strings.Contains(response.Body.String(), private) {
			t.Errorf("%s: %d %s", name, response.Code, response.Body)
		}
	}
	// Any other error says what it is, in its own words.
	response, envelope := respondedError(t, nil, fmt.Errorf("session 42: %w", domain.ErrNotFound))
	if response.Code != http.StatusNotFound || envelope.Error.Code != "not_found" || envelope.Error.Message != "session 42: not found" {
		t.Errorf("a missing session: %d %s", response.Code, response.Body)
	}
}

// An answer without an error is JSON with status 200, and a list that is
// empty is an empty list, never null.
func TestAnswerWithoutAnErrorIsJSON(t *testing.T) {
	for name, s := range map[string]struct {
		value any
		body  string
	}{
		"a list nobody filled": {[]string(nil), "[]\n"},
		"an empty list":        {[]int{}, "[]\n"},
		"a list":               {[]string{"a", "b"}, "[\"a\",\"b\"]\n"},
		"an object":            {map[string]int{"n": 1}, "{\"n\":1}\n"},
	} {
		response := httptest.NewRecorder()
		(&Server{}).respond(response, s.value, nil)
		if response.Code != http.StatusOK || response.Body.String() != s.body || response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
			t.Errorf("%s: %d %q %q", name, response.Code, response.Body.String(), response.Header().Get("Content-Type"))
		}
	}
}
