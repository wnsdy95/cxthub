package backendclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// BackendClient implements RemoteSync for synchronous REST sync with the central server (sync protocol).
//
// Uses net/http (stdlib). baseURL is the central server REST base (e.g., http://127.0.0.1:8080/api/v1).
// It attaches the Authorization: Bearer header to requests.
type BackendClient struct {
	// baseURL/token is resolved at request time — supports lazy evaluation for processes starting after remote add or cxt login.
	baseURL          func() string
	token            func() string
	httpc            *http.Client
	resolveTransport func() (string, string, error)
}

// NewBackendClient creates a BackendClient.
// Timeout 30s: agent tool hooks read branch summaries through this client, so an
// infinite wait would block the agent session itself.
func NewBackendClient(baseURL, token func() string) *BackendClient {
	return &BackendClient{baseURL: baseURL, token: token, httpc: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func NewResolvedBackendClient(resolve func() (string, string, error)) *BackendClient {
	client := NewBackendClient(func() string { endpoint, _, _ := resolve(); return endpoint }, func() string { _, token, _ := resolve(); return token })
	client.resolveTransport = resolve
	return client
}

func (c *BackendClient) transport() (string, string, error) {
	if c.resolveTransport != nil {
		return c.resolveTransport()
	}
	return c.baseURL(), c.token(), nil
}

// Freeze the destination and credentials for HTTP requests, so every request
// of one operation goes to the same server with the same credential even when
// the configured transport changes meanwhile.
func (c *BackendClient) frozenRequestClient(endpoint, token string) *BackendClient {
	return &BackendClient{
		baseURL: func() string { return endpoint },
		token:   func() string { return token },
		httpc:   c.httpc,
	}
}

// --- wire DTO (same snake_case as backend) ---

type negotiateReq struct {
	DocHaves []domain.ContentHash `json:"doc_haves"`
	// ChunkHaves is the hash of chunks to send — chunk server responds with only missing chunks.
	ChunkHaves []domain.ContentHash `json:"chunk_haves,omitempty"`
}
type negotiateResp struct {
	DocIdentitiesSupported []domain.DocumentIdentity `json:"doc_identities_supported,omitempty"`
	RootPublicationEnabled bool                      `json:"root_publication_enabled,omitempty"`
	AsyncDocsSupported     bool                      `json:"async_docs_supported,omitempty"`
	// SnapshotWants must stay empty: this client never offers snapshots.
	SnapshotWants []domain.ContentHash `json:"snapshot_wants"`
	DocWants      []domain.ContentHash `json:"doc_wants"`
	// ChunksSupported true = chunk wire support server (old servers lack field → false — blanket fallback).
	ChunksSupported        bool                 `json:"chunks_supported,omitempty"`
	BoundedChunksSupported bool                 `json:"bounded_chunks_supported,omitempty"`
	ChunkFormatsSupported  []string             `json:"chunk_formats_supported,omitempty"`
	CIRVersionsSupported   []string             `json:"cir_versions_supported,omitempty"`
	ChunkWants             []domain.ContentHash `json:"chunk_wants,omitempty"`
}

// chunkedDocWire is the wire form sending doc as manifest (envelope+chunk hash).
type chunkedDocWire = domain.DocumentRepresentation

// chunkObjWire is the chunk body (Data is the uncompressed chunk bytes — JSON base64).
type chunkObjWire struct {
	Hash domain.ContentHash `json:"hash"`
	Data []byte             `json:"data"`
}
type objectsReq struct {
	// Chunk wire (used only when server negotiate has chunks_supported):
	// doc is the manifest, the body contains only server missing chunks (delta upload).
	ChunkedDocs  []chunkedDocWire `json:"chunked_docs,omitempty"`
	ChunkObjects []chunkObjWire   `json:"chunk_objects,omitempty"`
}
type chunksReq struct {
	Chunks []chunkObjWire `json:"chunks"`
}

const (
	// Server inbound.MaxChunkWire* and similar wire contract. Splits general chunks (512KiB) into ~2MiB raw units for JSON base64 encoding while keeping proxy requests/responses small.
	maxChunkWireRawBytes = chunkcas.MaxPortableChunkBytes
	maxChunkWireObjects  = 32
	maxACKDrainBytes     = 32 << 10
	ackDrainTimeout      = 100 * time.Millisecond
)

var errBoundedResponse = errors.New("response exceeds bounded transport limit")

// do sends JSON request and decodes response to out (error on non-2xx).
func (c *BackendClient) do(ctx context.Context, method, path string, body, out any) error {
	return c.doLimited(ctx, method, path, body, out, 0)
}

// doLimited decodes bounded endpoint responses only below an explicit upper limit. max=0 is
// the existing general JSON path.
func (c *BackendClient) doLimited(ctx context.Context, method, path string, body, out any, max int64) error {
	endpoint, token, err := c.transport()
	if err != nil {
		return err
	}
	var rdr io.Reader
	if body != nil {
		var b []byte
		var err error
		if frozen, ok := body.(uploadJSON); ok {
			b = frozen
		} else {
			b, err = json.Marshal(body)
		}
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	var cancelACK context.CancelFunc
	if out == nil {
		ctx, cancelACK = context.WithCancel(ctx)
		defer cancelACK()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// A protocol declaration only; the server still checks repository policy,
	// current authorization and every referenced document's actual bytes.
	req.Header.Set("X-Cxt-Doc-Identities", string(domain.DocumentIdentityRootV1))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		rejection := newHTTPError(resp.StatusCode, method, path, b)
		// Only the bounded uploadBatch retry reads the hint, and only for
		// application backpressure.
		if readErr == nil && rejection.Status == http.StatusServiceUnavailable && rejection.Code == "resource_overloaded" {
			rejection.retryAfter, rejection.retryAfterValid = uploadRetryAfter(resp.Header.Values("Retry-After"), time.Now())
		}
		return rejection
	}
	if resp.StatusCode == http.StatusNoContent {
		// Optional singleton assets (team settings and the sealed secrets
		// envelope) use 204 for "not configured". Preserve the CLI's existing
		// domain-level absence behavior while keeping browser probes out of the
		// failed-request path.
		if out != nil {
			return domain.ErrNotFound
		}
		return nil
	}
	if out != nil {
		if max > 0 {
			b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
			if err != nil {
				return err
			}
			if int64(len(b)) > max {
				return fmt.Errorf("%s %s: %w", method, path, errBoundedResponse)
			}
			return json.Unmarshal(b, out)
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}
	// An unexpected 101 hands the upgraded connection to the response body;
	// net/http no longer interrupts its reads on request cancellation. There is
	// no reusable HTTP connection to recover, so close it without reading.
	if resp.StatusCode < 200 {
		return nil
	}
	// Successful headers already acknowledge the write. Draining its unused ACK
	// is only best-effort connection cleanup, never a commit check or retry signal.
	// The 100ms cleanup bound starts only after successful headers. It never
	// extends the caller deadline or the client's existing 30s request timeout.
	// Cancelling the request interrupts a stalled net/http body read without a
	// separate reader goroutine; the byte limit includes every drain read.
	timer := time.AfterFunc(ackDrainTimeout, cancelACK)
	defer timer.Stop()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxACKDrainBytes))
	return nil
}

// HTTPError is a server-provided non-2xx response. It parses the server's error envelope
// ({ "error": { "code", "message" } }) to preserve the Code, allowing the caller to distinguish
// and handle specific rejections (e.g., git_origin_mismatch).
type HTTPError struct {
	Status          int
	Code            string
	Message         string
	retryAfter      time.Duration
	retryAfterValid bool
	// Operation contains only the method and escaped path, never query values,
	// credentials, headers or request bodies.
	Operation string
	// raw is the original text (for debugging) when envelope parsing fails.
	raw string
}

func (e *HTTPError) Error() string {
	// code and status are included as strings — existing callers identify "401", "403"
	// as error strings (for compatibility with the old do() which directly stored the original envelope).
	// New code instead uses *HTTPError.Code for type discrimination.
	var message string
	switch {
	case e.Code != "":
		message = fmt.Sprintf("%s (%d): %s", e.Code, e.Status, e.Message)
	case e.Message != "":
		message = fmt.Sprintf("(%d): %s", e.Status, e.Message)
	default:
		message = fmt.Sprintf("Server error (%d): %s", e.Status, e.raw)
	}
	if e.Operation != "" {
		return e.Operation + ": " + message
	}
	return message
}

// StatusCode lets use-case code classify terminal queue conflicts without
// depending on this HTTP adapter's concrete error type.
func (e *HTTPError) StatusCode() int { return e.Status }

func newHTTPError(status int, method, path string, body []byte) *HTTPError {
	he := &HTTPError{Status: status, raw: responseBodySummary(body)}
	he.Operation = method
	if u, err := url.Parse(path); err == nil {
		he.Operation += " " + u.EscapedPath()
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && env.Error.Code != "" {
		he.Code = env.Error.Code
		he.Message = domain.SanitizeErrorText(env.Error.Message, maxErrorMessageBytes)
	} else {
		he.Message = he.raw
	}
	return he
}

const maxErrorMessageBytes = 300

// responseBodySummary keeps a short, sanitized text body for diagnosis. A
// non-JSON page from a proxy (HTML, challenge pages) can embed opaque tokens
// and is never copied into errors that queues persist or status prints.
func responseBodySummary(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return ""
	}
	if strings.HasPrefix(text, "<") {
		return fmt.Sprintf("non-JSON response body omitted (%d bytes)", len(body))
	}
	return domain.SanitizeErrorText(text, maxErrorMessageBytes)
}

func (c *BackendClient) reposPath(repoID string) string {
	return "/repos/" + url.PathEscape(repoID)
}

func setOf(hs []domain.ContentHash) map[domain.ContentHash]bool {
	m := make(map[domain.ContentHash]bool, len(hs))
	for _, h := range hs {
		m[h] = true
	}
	return m
}

func chunkUploadBatches(chunks []chunkObjWire) ([][]chunkObjWire, error) {
	var batches [][]chunkObjWire
	var current []chunkObjWire
	total := 0
	flush := func() {
		if len(current) == 0 {
			return
		}
		batches = append(batches, current)
		current = nil
		total = 0
	}
	for _, chunk := range chunks {
		if len(chunk.Data) == 0 || len(chunk.Data) > maxChunkWireRawBytes {
			return nil, fmt.Errorf("chunk %s exceeds bounded transport size (%d bytes)", chunk.Hash, len(chunk.Data))
		}
		if len(current) > 0 && (len(current) >= maxChunkWireObjects || total+len(chunk.Data) > maxChunkWireRawBytes) {
			flush()
		}
		current = append(current, chunk)
		total += len(chunk.Data)
	}
	flush()
	return batches, nil
}

func (c *BackendClient) pushChunkBatches(ctx context.Context, repoID string, chunks []chunkObjWire) error {
	batches, err := chunkUploadBatches(chunks)
	if err != nil {
		return err
	}
	for _, batch := range batches {
		if err := c.uploadBatch(ctx, c.reposPath(repoID)+"/push/chunks", chunksReq{Chunks: batch}); err != nil {
			return err
		}
	}
	return nil
}

// RegisterRepo registers repo metadata to the server via POST /repos and returns a confirmed record (idempotent).
func (c *BackendClient) RegisterRepo(ctx context.Context, repo domain.Repo) (domain.Repo, error) {
	var out domain.Repo
	if err := c.do(ctx, http.MethodPost, "/repos", repo, &out); err != nil {
		return domain.Repo{}, err
	}
	return out, nil
}

// PullSettings fetches team default setting bundles using GET /repos/{repoID}/settings/{kind}.
func (c *BackendClient) PullSettings(ctx context.Context, repoID, kind string) (domain.SettingsBundle, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return domain.SettingsBundle{}, err
	}
	if !domain.ValidSettingsKind(kind) {
		return domain.SettingsBundle{}, domain.ErrHashMismatch
	}
	var b domain.SettingsBundle
	if err := c.do(ctx, http.MethodGet, c.reposPath(repoID)+"/settings/"+url.PathEscape(kind), nil, &b); err != nil {
		return domain.SettingsBundle{}, err
	}
	if err := domain.ValidateSettingsBundle(kind, "", b); err != nil {
		return domain.SettingsBundle{}, err
	}
	return b, nil
}

// SecretsOrigin scopes local edit baselines to the backend that issued them.
func (c *BackendClient) SecretsOrigin() string { return strings.TrimRight(c.baseURL(), "/") }

// PushSecrets requires the generation read before editing, even for same-key writes.
func (c *BackendClient) PushSecrets(ctx context.Context, repoID string, raw []byte, rotate bool, expect, revision string) (string, error) {
	path := c.reposPath(repoID) + "/secrets?expected_revision=" + url.QueryEscape(revision)
	if rotate {
		path += "&rotate=true&expect=" + url.QueryEscape(expect)
	}
	var result struct {
		Revision string `json:"revision"`
	}
	err := c.do(ctx, http.MethodPut, path, json.RawMessage(raw), &result)
	if err == nil && result.Revision == "" {
		return "", fmt.Errorf("server did not acknowledge a secrets revision; upgrade the server before editing")
	}
	return result.Revision, err
}

// PullSecrets retrieves the sealed envelope of secret ciphertexts as raw bytes (decryption is performed by the caller).
func (c *BackendClient) PullSecrets(ctx context.Context, repoID string) ([]byte, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, c.reposPath(repoID)+"/secrets", nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func validateNegotiatedSubset(haves, wants []domain.ContentHash) error {
	offered := make(map[domain.ContentHash]bool, len(haves))
	for _, hash := range haves {
		offered[hash] = true
	}
	seen := make(map[domain.ContentHash]bool, len(wants))
	for _, hash := range wants {
		if err := domain.ValidateContentHash(hash); err != nil {
			return err
		}
		if !offered[hash] || seen[hash] {
			return domain.ErrHashMismatch
		}
		seen[hash] = true
	}
	return nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
