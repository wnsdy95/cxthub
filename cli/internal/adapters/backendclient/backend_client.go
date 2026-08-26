package backendclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// BackendClient implements RemoteSync for synchronous REST sync with the central server (sync protocol).
//
// Uses net/http (stdlib). baseURL is the central server REST base (e.g., http://127.0.0.1:8080/api/v1).
// It attaches Authorization: Bearer and X-Cxt-Identity headers to requests.
type BackendClient struct {
	// baseURL/token is resolved at request time — supports lazy evaluation for processes starting after remote add or cxt login.
	baseURL  func() string
	token    func() string
	identity domain.TeamIdentity
	httpc    *http.Client
	// chunks accesses local chunk store (optional — for pull delta, inject with SetChunkLocal).
	chunks ChunkLocal
}

// NewBackendClient creates a BackendClient.
// Timeout 30s: git hooks (pre-push/post-checkout, etc.) use this client for sync network, so infinite wait blocks git command itself (review #2 — fail-open contract hole).
func NewBackendClient(baseURL, token func() string, identity domain.TeamIdentity) *BackendClient {
	return &BackendClient{baseURL: baseURL, token: token, identity: identity, httpc: &http.Client{Timeout: 30 * time.Second}}
}

var _ outbound.RemoteSync = (*BackendClient)(nil)
var _ outbound.PushObjectNegotiator = (*BackendClient)(nil)

// --- wire DTO (same snake_case as backend) ---

type negotiateReq struct {
	SnapshotHaves []domain.ContentHash `json:"snapshot_haves"`
	DocHaves      []domain.ContentHash `json:"doc_haves"`
	// ChunkHaves is the hash of chunks to send — chunk server responds with only missing chunks.
	ChunkHaves []domain.ContentHash `json:"chunk_haves,omitempty"`
}
type negotiateResp struct {
	SnapshotWants []domain.ContentHash `json:"snapshot_wants"`
	DocWants      []domain.ContentHash `json:"doc_wants"`
	// ChunksSupported true = chunk wire support server (old servers lack field → false — blanket fallback).
	ChunksSupported        bool                 `json:"chunks_supported,omitempty"`
	BoundedChunksSupported bool                 `json:"bounded_chunks_supported,omitempty"`
	ChunkFormatsSupported  []string             `json:"chunk_formats_supported,omitempty"`
	ChunkWants             []domain.ContentHash `json:"chunk_wants,omitempty"`
}

// chunkedDocWire is the wire form sending doc as manifest (envelope+chunk hash).
type chunkedDocWire struct {
	Hash     domain.ContentHash   `json:"hash"`
	Format   string               `json:"format,omitempty"`
	Envelope json.RawMessage      `json:"envelope"`
	Chunks   []domain.ContentHash `json:"chunks"`
}

// chunkObjWire is the chunk body (Data is the uncompressed chunk bytes — JSON base64).
type chunkObjWire struct {
	Hash domain.ContentHash `json:"hash"`
	Data []byte             `json:"data"`
}
type objectsReq struct {
	Snapshots []domain.Snapshot   `json:"snapshots"`
	Docs      []domain.SessionDoc `json:"docs"`
	// Chunk wire (new type — used only when server negotiate has chunks_supported):
	// doc is the manifest, the body contains only server missing chunks (delta upload).
	ChunkedDocs  []chunkedDocWire `json:"chunked_docs,omitempty"`
	ChunkObjects []chunkObjWire   `json:"chunk_objects,omitempty"`
}
type chunksReq struct {
	Chunks []chunkObjWire `json:"chunks"`
}
type pullReq struct {
	SnapshotWants []domain.ContentHash `json:"snapshot_wants"`
	DocWants      []domain.ContentHash `json:"doc_wants"`
	// Chunk wire (new type): receive doc as manifest and only missing chunk bodies (delta download).
	DocManifestWants      []domain.ContentHash `json:"doc_manifest_wants,omitempty"`
	ChunkWants            []domain.ContentHash `json:"chunk_wants,omitempty"`
	ChunkFormatsSupported []string             `json:"chunk_formats_supported,omitempty"`
}
type pullResp struct {
	Snapshots              []domain.Snapshot   `json:"snapshots"`
	Docs                   []domain.SessionDoc `json:"docs"`
	DocManifests           []chunkedDocWire    `json:"doc_manifests,omitempty"`
	ChunkObjects           []chunkObjWire      `json:"chunk_objects,omitempty"`
	BoundedChunksSupported bool                `json:"bounded_chunks_supported,omitempty"`
}

const (
	// Server inbound.MaxChunkWire* and similar wire contract. Splits general chunks (512KiB) into ~2MiB raw units for JSON base64 encoding while keeping proxy requests/responses small.
	maxChunkWireRawBytes = chunkcas.MaxPortableChunkBytes
	maxChunkWireObjects  = 32
	maxChunkWireJSONBody = 4 << 20
)

type putRefReq struct {
	Target         domain.ContentHash `json:"target"`
	ExpectedTarget domain.ContentHash `json:"expected_target"`
	Symbolic       string             `json:"symbolic"`
	Force          bool               `json:"force,omitempty"`
	// Append grafts diverged push to server head (--append).
	Append bool `json:"append,omitempty"`
}

// ChunkLocal is local chunk store access (pull delta — does not receive existing chunks).
// nil means receive all chunks (still maintains doc unit delta).
type ChunkLocal interface {
	HasChunk(hash domain.ContentHash) bool
	GetChunk(hash domain.ContentHash) ([]byte, error)
}

// SetChunkLocal injects local chunk access (composition root — DI).
func (c *BackendClient) SetChunkLocal(cl ChunkLocal) { c.chunks = cl }

// do sends JSON request and decodes response to out (error on non-2xx).
func (c *BackendClient) do(ctx context.Context, method, path string, body, out any) error {
	return c.doLimited(ctx, method, path, body, out, 0)
}

// doLimited decodes bounded endpoint responses only below an explicit upper limit. max=0 is
// the existing general JSON path.
func (c *BackendClient) doLimited(ctx context.Context, method, path string, body, out any, max int64) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL()+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if t := c.token(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	if c.identity.Email != "" {
		req.Header.Set("X-Cxt-Identity", c.identity.Email)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return newHTTPError(resp.StatusCode, method, path, b)
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
				return fmt.Errorf("%s %s response exceeds bounded transport limit", method, path)
			}
			return json.Unmarshal(b, out)
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// HTTPError is a server-provided non-2xx response. It parses the server's error envelope
// ({ "error": { "code", "message" } }) to preserve the Code, allowing the caller to distinguish
// and handle specific rejections (e.g., git_origin_mismatch).
type HTTPError struct {
	Status  int
	Code    string
	Message string
	// raw is the original text (for debugging) when envelope parsing fails.
	raw string
}

func (e *HTTPError) Error() string {
	// code and status are included as strings — existing callers identify "non_fast_forward", "401", "403"
	// as error strings (for compatibility with the old do() which directly stored the original envelope).
	// New code instead uses *HTTPError.Code for type discrimination.
	switch {
	case e.Code != "":
		return fmt.Sprintf("%s (%d): %s", e.Code, e.Status, e.Message)
	case e.Message != "":
		return fmt.Sprintf("(%d): %s", e.Status, e.Message)
	default:
		return fmt.Sprintf("Server error (%d): %s", e.Status, e.raw)
	}
}

// StatusCode lets use-case code classify terminal queue conflicts without
// depending on this HTTP adapter's concrete error type.
func (e *HTTPError) StatusCode() int { return e.Status }

func newHTTPError(status int, method, path string, body []byte) *HTTPError {
	he := &HTTPError{Status: status, raw: string(body)}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && env.Error.Code != "" {
		he.Code = env.Error.Code
		he.Message = env.Error.Message
	} else {
		he.Message = fmt.Sprintf("%s %s → %d: %s", method, path, status, strings.TrimSpace(string(body)))
	}
	return he
}

func (c *BackendClient) reposPath(repoID string) string {
	return "/repos/" + url.PathEscape(repoID)
}

func escapePathName(name string) string {
	parts := strings.Split(name, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func validateSnapshotObject(snap domain.Snapshot) error {
	if err := domain.ValidateContentHash(snap.ID); err != nil {
		return err
	}
	if err := domain.ValidateContentHash(snap.DocHash); err != nil {
		return err
	}
	if snap.ID != snap.DocHash {
		return domain.ErrHashMismatch
	}
	for _, h := range []domain.ContentHash{snap.MemoryHash, snap.ClaudeSettings, snap.AgentsSettings, snap.CodexSettings} {
		if err := domain.ValidateOptionalContentHash(h); err != nil {
			return err
		}
	}
	for _, p := range snap.Parents {
		if err := domain.ValidateContentHash(p); err != nil {
			return err
		}
	}
	for _, p := range snap.GraftParents {
		if err := domain.ValidateContentHash(p); err != nil {
			return err
		}
	}
	return nil
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
		if err := c.do(ctx, http.MethodPost, c.reposPath(repoID)+"/push/chunks", chunksReq{Chunks: batch}, nil); err != nil {
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

// PushMemory advances a causal attachment through the CAS-only endpoint. A
// distinct path makes rolling upgrades fail before mutation on old servers
// that do not understand PreviousMemoryHash.
func (c *BackendClient) PushMemory(ctx context.Context, repoID string, digest domain.MemoryDigest) error {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return err
	}
	if err := domain.ValidateContentHash(digest.SnapshotID); err != nil {
		return err
	}
	want, err := domain.MemoryDigestHash(digest)
	if err != nil {
		return err
	}
	var out struct {
		MemoryHash domain.ContentHash `json:"memory_hash"`
	}
	path := c.reposPath(repoID) + "/memory-attachments/" + url.PathEscape(string(digest.SnapshotID))
	if err := c.do(ctx, http.MethodPut, path, digest, &out); err != nil {
		return err
	}
	if out.MemoryHash != want {
		return domain.ErrHashMismatch
	}
	return nil
}

// PullMemory downloads a digest from the server via GET /repos/{repoID}/memories/{snapshotID}.
func (c *BackendClient) PullMemory(ctx context.Context, repoID string, snapshotID domain.ContentHash) (domain.MemoryDigest, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return domain.MemoryDigest{}, err
	}
	if err := domain.ValidateContentHash(snapshotID); err != nil {
		return domain.MemoryDigest{}, err
	}
	var d domain.MemoryDigest
	if err := c.do(ctx, http.MethodGet, c.reposPath(repoID)+"/memories/"+url.PathEscape(string(snapshotID)), nil, &d); err != nil {
		return domain.MemoryDigest{}, err
	}
	return d, nil
}

func (c *BackendClient) PullMemoryObject(ctx context.Context, repoID string, hash domain.ContentHash) (domain.MemoryDigest, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return domain.MemoryDigest{}, err
	}
	if err := domain.ValidateContentHash(hash); err != nil {
		return domain.MemoryDigest{}, err
	}
	var d domain.MemoryDigest
	if err := c.do(ctx, http.MethodGet, c.reposPath(repoID)+"/memory-objects/"+url.PathEscape(string(hash)), nil, &d); err != nil {
		return domain.MemoryDigest{}, err
	}
	return d, nil
}

// PromoteSnapshotMessage promotes a hook label to a commit message using POST /repos/{repoID}/snapshots/{id}/promote (server performs unidirectional rule validation — idempotence).
func (c *BackendClient) PromoteSnapshotMessage(ctx context.Context, repoID string, id domain.ContentHash, message string) error {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return err
	}
	if err := domain.ValidateContentHash(id); err != nil {
		return err
	}
	body := struct {
		Message string `json:"message"`
	}{Message: message}
	return c.do(ctx, http.MethodPost, c.reposPath(repoID)+"/snapshots/"+url.PathEscape(string(id))+"/promote", body, nil)
}

// GraftSnapshotParents adds an availability overlay edge to the expectedSeq CAS using POST /repos/{repoID}/snapshots/{id}/graft. The server validates existence/cycles, and only retries idempotent success for responses to previously confirmed events.
func (c *BackendClient) GraftSnapshotParents(ctx context.Context, repoID string, id domain.ContentHash, parents []domain.ContentHash, expectedSeq uint64) error {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return err
	}
	if err := domain.ValidateContentHash(id); err != nil {
		return err
	}
	body := struct {
		Parents     []domain.ContentHash `json:"parents"`
		ExpectedSeq uint64               `json:"expected_seq"`
	}{Parents: parents, ExpectedSeq: expectedSeq}
	return c.do(ctx, http.MethodPost, c.reposPath(repoID)+"/snapshots/"+url.PathEscape(string(id))+"/graft", body, nil)
}

// GetSnapshotRemote reads authoritative snapshot metadata, including the
// server-owned graft register.
func (c *BackendClient) GetSnapshotRemote(ctx context.Context, repoID string, id domain.ContentHash) (domain.Snapshot, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return domain.Snapshot{}, err
	}
	if err := domain.ValidateContentHash(id); err != nil {
		return domain.Snapshot{}, err
	}
	var snap domain.Snapshot
	if err := c.do(ctx, http.MethodGet, c.reposPath(repoID)+"/snapshots/"+url.PathEscape(string(id)), nil, &snap); err != nil {
		return domain.Snapshot{}, err
	}
	if snap.ID != id || snap.RepoID != repoID {
		return domain.Snapshot{}, domain.ErrHashMismatch
	}
	if err := validateSnapshotObject(snap); err != nil {
		return domain.Snapshot{}, err
	}
	return snap, nil
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

// PushSecrets uploads a sealed envelope of secret ciphertexts (server performs transparent storage — E2E). If rotate=true, it performs an explicit replacement — instead of server's thumbprint verification, it uses CAS: if the thumbprint of the envelope read based on the replacement differs from the server's current thumbprint, it returns 409 (to prevent loss during the update).
func (c *BackendClient) PushSecrets(ctx context.Context, repoID string, raw []byte, rotate bool, expect string) error {
	path := c.reposPath(repoID) + "/secrets"
	if rotate {
		path += "?rotate=true&expect=" + url.QueryEscape(expect)
	}
	return c.do(ctx, http.MethodPut, path, json.RawMessage(raw), nil)
}

// PullSecrets retrieves the sealed envelope of secret ciphertexts as raw bytes (decryption is performed by the caller).
func (c *BackendClient) PullSecrets(ctx context.Context, repoID string) ([]byte, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, c.reposPath(repoID)+"/secrets", nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// FsckReport is the server's reference reachability audit result (read-only — equivalent to git fsck).
type FsckReport struct {
	Total           int      `json:"total"`
	Reachable       int      `json:"reachable"`
	Roots           []string `json:"roots"`
	Unreachable     []string `json:"unreachable"`
	DanglingParents []struct {
		Snapshot string `json:"snapshot"`
		Missing  string `json:"missing"`
	} `json:"dangling_parents"`
}

// Fsck performs reference reachability auditing on the repo server-side and receives the result (does not make any changes).
func (c *BackendClient) Fsck(ctx context.Context, repoID string) (FsckReport, error) {
	var r FsckReport
	if err := c.do(ctx, http.MethodGet, c.reposPath(repoID)+"/fsck", nil, &r); err != nil {
		return FsckReport{}, err
	}
	return r, nil
}

// RefLogEntry is a record of a ref movement (corresponds to git reflog).
type RefLogEntry struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Old       string `json:"old"`
	New       string `json:"new"`
	CreatedAt string `json:"created_at"`
}

// Reflog receives the ref movement records of a repo (latest first) (read-only — basis for recovering hidden tips).
func (c *BackendClient) Reflog(ctx context.Context, repoID string) ([]RefLogEntry, error) {
	var r []RefLogEntry
	if err := c.do(ctx, http.MethodGet, c.reposPath(repoID)+"/reflog", nil, &r); err != nil {
		return nil, err
	}
	return r, nil
}

// PushSettingsObject uploads a commit attachment settings object (content-addressed, idempotent).
func (c *BackendClient) PushSettingsObject(ctx context.Context, repoID string, hash domain.ContentHash, bundle domain.SettingsBundle) error {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return err
	}
	if err := domain.ValidateContentHash(hash); err != nil {
		return err
	}
	if err := domain.ValidateSettingsBundle(bundle.Kind, hash, bundle); err != nil {
		return err
	}
	return c.do(ctx, http.MethodPut, c.reposPath(repoID)+"/settings-objects/"+url.PathEscape(string(hash)), bundle, nil)
}

// PullSettingsObject downloads a settings object.
func (c *BackendClient) PullSettingsObject(ctx context.Context, repoID string, hash domain.ContentHash) (domain.SettingsBundle, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return domain.SettingsBundle{}, err
	}
	if err := domain.ValidateContentHash(hash); err != nil {
		return domain.SettingsBundle{}, err
	}
	var b domain.SettingsBundle
	if err := c.do(ctx, http.MethodGet, c.reposPath(repoID)+"/settings-objects/"+url.PathEscape(string(hash)), nil, &b); err != nil {
		return domain.SettingsBundle{}, err
	}
	if err := domain.ValidateSettingsBundle(b.Kind, hash, b); err != nil {
		return domain.SettingsBundle{}, err
	}
	return b, nil
}

// PushPending upserts a session's ongoing context pointer (PUT — idempotent overwrite).
func (c *BackendClient) PushPending(ctx context.Context, repoID string, p domain.Pending) error {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return err
	}
	if err := domain.ValidateContentHash(p.Target); err != nil {
		return err
	}
	return c.do(ctx, http.MethodPut, c.reposPath(repoID)+"/pending/"+url.PathEscape(p.SessionID), p, nil)
}

// DeletePendingRemote removes a session's pending from the server (commit resolution — idempotent).
func (c *BackendClient) DeletePendingRemote(ctx context.Context, repoID string, sessionID string) error {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, c.reposPath(repoID)+"/pending/"+url.PathEscape(sessionID), nil, nil)
}

// PushUnsync upserts a push pending pointer by branch (PUT — authenticated user key, idempotent).
// Branch names are sent in their original path as with ref PUT (server {branch...} rest-matching).
func (c *BackendClient) PushUnsync(ctx context.Context, repoID string, u domain.Unsync) error {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return err
	}
	if err := domain.ValidateBranchName(u.Branch); err != nil {
		return err
	}
	if err := domain.ValidateContentHash(u.Target); err != nil {
		return err
	}
	if u.Author.Name == "" && u.Author.Email == "" {
		u.Author = c.identity // enrich display attribution; the server's authenticated user remains the key
	}
	return c.do(ctx, http.MethodPut, c.reposPath(repoID)+"/unsync/"+escapePathName(u.Branch), u, nil)
}

// DeleteUnsyncRemote removes the push pointer of a branch (git push resolution — idempotent).
func (c *BackendClient) DeleteUnsyncRemote(ctx context.Context, repoID string, branch string) error {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return err
	}
	if err := domain.ValidateBranchName(branch); err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, c.reposPath(repoID)+"/unsync/"+escapePathName(branch), nil, nil)
}

// RemoteManifest fetches the server catalog using GET /repos/{repoID}/manifest.
func (c *BackendClient) RemoteManifest(ctx context.Context, repoID string) (domain.Manifest, error) {
	var m domain.Manifest
	if err := c.do(ctx, http.MethodGet, c.reposPath(repoID)+"/manifest", nil, &m); err != nil {
		return domain.Manifest{}, err
	}
	if m.RepoID != repoID {
		return domain.Manifest{}, domain.ErrHashMismatch
	}
	index := make(map[domain.ContentHash]bool, len(m.SnapshotIndex))
	for _, id := range m.SnapshotIndex {
		if err := domain.ValidateContentHash(id); err != nil || index[id] {
			return domain.Manifest{}, domain.ErrHashMismatch
		}
		index[id] = true
	}
	for snapshotID, memoryHash := range m.MemoryAttachments {
		if err := domain.ValidateContentHash(snapshotID); err != nil {
			return domain.Manifest{}, err
		}
		if err := domain.ValidateContentHash(memoryHash); err != nil {
			return domain.Manifest{}, err
		}
		if !index[snapshotID] {
			return domain.Manifest{}, domain.ErrHashMismatch
		}
	}
	return m, nil
}

// NegotiatePushObjects performs the hash-only first phase before the
// application opens any cumulative SessionDoc bodies. It uses the existing
// sync endpoint, so old servers that support Push already support this
// optimization. A server response must be a unique subset of what the client
// advertised; arbitrary wants are rejected before they can drive local reads.
// Missing docs are opened afterward, and Push's second negotiation handles
// their chunk manifests and resumes partially staged chunk uploads.
func (c *BackendClient) NegotiatePushObjects(ctx context.Context, repoID string, snapshotHaves, docHaves []domain.ContentHash) (outbound.PushObjectWants, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return outbound.PushObjectWants{}, err
	}
	for _, hash := range append(append([]domain.ContentHash(nil), snapshotHaves...), docHaves...) {
		if err := domain.ValidateContentHash(hash); err != nil {
			return outbound.PushObjectWants{}, err
		}
	}
	var neg negotiateResp
	if err := c.do(ctx, http.MethodPost, c.reposPath(repoID)+"/push/negotiate", negotiateReq{
		SnapshotHaves: snapshotHaves,
		DocHaves:      docHaves,
	}, &neg); err != nil {
		return outbound.PushObjectWants{}, err
	}
	if err := validateNegotiatedSubset(snapshotHaves, neg.SnapshotWants); err != nil {
		return outbound.PushObjectWants{}, err
	}
	if err := validateNegotiatedSubset(docHaves, neg.DocWants); err != nil {
		return outbound.PushObjectWants{}, err
	}
	return outbound.PushObjectWants{Snapshots: neg.SnapshotWants, Docs: neg.DocWants}, nil
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

// Push performs three steps: negotiate(A) → objects(B) → refs PUT(C) for uploading only missing parts (sync protocol).
func (c *BackendClient) Push(ctx context.Context, repoID string, snapshots []domain.Snapshot, docs []domain.SessionDoc, refs []domain.Ref, force, appendDiverged bool) error {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return err
	}
	snapHaves := make([]domain.ContentHash, 0, len(snapshots))
	for _, s := range snapshots {
		if err := validateSnapshotObject(s); err != nil {
			return err
		}
		if s.RepoID != repoID {
			return domain.ErrHashMismatch
		}
		snapHaves = append(snapHaves, s.ID)
	}
	docHaves := make([]domain.ContentHash, 0, len(docs))
	// Chunk plan: canonical calculation and hash validation (equivalent to ValidateSessionDocHash — duplicate serialization removal).
	// Successful doc plans send only manifest and missing chunks to the server (delta upload).
	plans := make(map[domain.ContentHash]chunkcas.Plan, len(docs))
	var chunkHaves []domain.ContentHash
	seenChunk := map[domain.ContentHash]bool{}
	for _, d := range docs {
		cb, cerr := domain.CanonicalBytes(d.CIR)
		if cerr != nil {
			return cerr
		}
		if domain.HashContent(cb) != d.Hash {
			return domain.ErrHashMismatch
		}
		docHaves = append(docHaves, d.Hash)
		// Storage and transport use the same v2 chunk identities. Sending v1 to a
		// v2 server would leave a second, unreferenced chunk representation in its
		// CAS. A pre-v2 server gets the complete document below; correctness is
		// preserved during rolling upgrades at the cost of temporary delta upload.
		plan, ok := chunkcas.PlanDoc(cb)
		if ok {
			plans[d.Hash] = plan
			for _, ch := range plan.Order {
				if !seenChunk[ch] {
					seenChunk[ch] = true
					chunkHaves = append(chunkHaves, ch)
				}
			}
		}
	}
	for _, ref := range refs {
		if err := domain.ValidateRef(ref); err != nil {
			return err
		}
		if ref.RepoID != "" && ref.RepoID != repoID {
			return domain.ErrHashMismatch
		}
	}

	var neg negotiateResp
	if len(snapHaves) > 0 || len(docHaves) > 0 {
		if err := c.do(ctx, http.MethodPost, c.reposPath(repoID)+"/push/negotiate", negotiateReq{snapHaves, docHaves, chunkHaves}, &neg); err != nil {
			return err
		}
	}
	wantSnap, wantDoc, wantChunk := setOf(neg.SnapshotWants), setOf(neg.DocWants), setOf(neg.ChunkWants)

	var sendSnaps []domain.Snapshot
	for _, s := range snapshots {
		if wantSnap[s.ID] {
			// Graft metadata is a server-owned reachability overlay. A local save may
			// already carry the same edge so its ref never drops an older session
			// branch, but a snapshot the server does not have yet must be created
			// without that overlay. The durable graft queue is flushed through the
			// dedicated endpoint after object upload.
			sendSnaps = append(sendSnaps, snapshotForCreate(s))
		}
	}
	var sendDocs []domain.SessionDoc
	var chunkedDocs []chunkedDocWire
	var chunkObjs []chunkObjWire
	sentChunk := map[domain.ContentHash]bool{}
	for _, d := range docs {
		if !wantDoc[d.Hash] {
			continue
		}
		plan, planned := plans[d.Hash]
		if planned && plan.Manifest.Format == chunkcas.FormatV2 && !containsString(neg.ChunkFormatsSupported, chunkcas.FormatV2) {
			planned = false
		}
		if !neg.ChunksSupported || !planned {
			sendDocs = append(sendDocs, d) // fallback to old server/plan — full (original path)
			continue
		}
		wireFormat := plan.Manifest.Format
		if wireFormat == chunkcas.FormatV1 {
			wireFormat = "" // pre-v2 servers interpret the omitted field as v1
		}
		chunkedDocs = append(chunkedDocs, chunkedDocWire{Hash: d.Hash, Format: wireFormat, Envelope: plan.Manifest.Envelope, Chunks: plan.Manifest.Chunks})
		for _, ch := range plan.Order {
			if wantChunk[ch] && !sentChunk[ch] {
				sentChunk[ch] = true
				chunkObjs = append(chunkObjs, chunkObjWire{Hash: ch, Data: plan.Bodies[ch]})
			}
		}
	}
	if neg.BoundedChunksSupported && len(chunkObjs) > 0 {
		if err := c.pushChunkBatches(ctx, repoID, chunkObjs); err != nil {
			return err
		}
		chunkObjs = nil
	}
	if len(sendSnaps) > 0 || len(sendDocs) > 0 || len(chunkedDocs) > 0 {
		if err := c.do(ctx, http.MethodPost, c.reposPath(repoID)+"/push/objects", objectsReq{sendSnaps, sendDocs, chunkedDocs, chunkObjs}, nil); err != nil {
			return err
		}
	}

	// ref movement (branch/session/tag; HEAD is a local concept). Server determines policy by kind.
	// Non-fast-forward rejections are collected and reported in git style at the end (other refs continue).
	var rejected []string
	for _, ref := range refs {
		if ref.Kind == domain.RefHEAD || ref.Target == "" {
			continue
		}
		path := c.reposPath(repoID) + "/refs/" + string(ref.Kind) + "/" + escapePathName(ref.Name)
		if err := c.do(ctx, http.MethodPut, path, putRefReq{Target: ref.Target, ExpectedTarget: "", Symbolic: ref.Symbolic, Force: force, Append: appendDiverged}, nil); err != nil {
			if strings.Contains(err.Error(), "non_fast_forward") {
				rejected = append(rejected, string(ref.Kind)+"/"+ref.Name)
				continue
			}
			return err
		}
	}
	if len(rejected) > 0 {
		return fmt.Errorf("%w: %s", domain.ErrSyncConflict, strings.Join(rejected, ", "))
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

func snapshotForCreate(s domain.Snapshot) domain.Snapshot {
	s.GraftParents = nil
	s.GraftSeq = 0
	s.Grafted = false
	return s
}

// Search calls server search (commit messages, conversation bodies) — for MCP context_search.
func (c *BackendClient) Search(ctx context.Context, repoID, query string) ([]outbound.SearchHit, bool, error) {
	var out struct {
		Hits      []outbound.SearchHit `json:"hits"`
		Truncated bool                 `json:"truncated"`
	}
	path := c.reposPath(repoID) + "/search?q=" + url.QueryEscape(query)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, false, err
	}
	return out.Hits, out.Truncated, nil
}

// UpdateRefRemote requests a single ref movement to the server (putRef — Append is lossless graft re-rebase).
func (c *BackendClient) UpdateRefRemote(ctx context.Context, repoID string, ref domain.Ref, appendDiverged bool) error {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return err
	}
	if err := domain.ValidateRef(ref); err != nil {
		return err
	}
	if ref.RepoID != "" && ref.RepoID != repoID {
		return domain.ErrHashMismatch
	}
	path := c.reposPath(repoID) + "/refs/" + string(ref.Kind) + "/" + escapePathName(ref.Name)
	return c.do(ctx, http.MethodPut, path, putRefReq{Target: ref.Target, Symbolic: ref.Symbolic, Append: appendDiverged}, nil)
}

// Pulls manifest → pull/objects(snapshots) → pull/objects(docs) to fetch server objects.
// Local storage/ref merge is performed by the caller (SyncRepo).
func (c *BackendClient) Pull(ctx context.Context, repoID string, docHaves []domain.ContentHash) ([]domain.Snapshot, []domain.SessionDoc, []domain.Ref, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return nil, nil, nil, err
	}
	man, err := c.RemoteManifest(ctx, repoID)
	if err != nil {
		return nil, nil, nil, err
	}
	if man.RepoID != repoID {
		return nil, nil, nil, domain.ErrHashMismatch
	}
	wantedSnapshots := make(map[domain.ContentHash]bool, len(man.SnapshotIndex))
	for _, id := range man.SnapshotIndex {
		if err := domain.ValidateContentHash(id); err != nil {
			return nil, nil, nil, err
		}
		if wantedSnapshots[id] {
			return nil, nil, nil, domain.ErrHashMismatch
		}
		wantedSnapshots[id] = true
	}
	for _, ref := range man.Refs {
		if err := domain.ValidateRef(ref); err != nil {
			return nil, nil, nil, err
		}
		if ref.RepoID != repoID {
			return nil, nil, nil, domain.ErrHashMismatch
		}
	}
	if len(man.SnapshotIndex) == 0 {
		return nil, nil, man.Refs, nil
	}
	var snapResp pullResp
	if err := c.do(ctx, http.MethodPost, c.reposPath(repoID)+"/pull/objects", pullReq{SnapshotWants: man.SnapshotIndex}, &snapResp); err != nil {
		return nil, nil, nil, err
	}
	// Delta reception: Does not request doc(docHaves) already in local storage — eliminates waste by re-receiving entire body (snapshot meta is received fully — graft/memory sync).
	haveDoc := make(map[domain.ContentHash]bool, len(docHaves))
	for _, h := range docHaves {
		haveDoc[h] = true
	}
	seenSnapshots := make(map[domain.ContentHash]bool, len(snapResp.Snapshots))
	docWantSet := make(map[domain.ContentHash]bool, len(snapResp.Snapshots))
	var docWants []domain.ContentHash
	for _, s := range snapResp.Snapshots {
		if err := validateSnapshotObject(s); err != nil {
			return nil, nil, nil, err
		}
		if s.RepoID != repoID || !wantedSnapshots[s.ID] || seenSnapshots[s.ID] {
			return nil, nil, nil, domain.ErrHashMismatch
		}
		seenSnapshots[s.ID] = true
		if !haveDoc[s.DocHash] && !docWantSet[s.DocHash] {
			docWantSet[s.DocHash] = true
			docWants = append(docWants, s.DocHash)
		}
	}
	if len(seenSnapshots) != len(wantedSnapshots) {
		return nil, nil, nil, domain.ErrHashMismatch
	}
	pulledDocs, err := c.pullDocs(ctx, repoID, docWants, docWantSet)
	if err != nil {
		return nil, nil, nil, err
	}
	return snapResp.Snapshots, pulledDocs, man.Refs, nil
}

// pullDocs receives missing doc bodies — for chunked server, only manifest+missing chunks (delta), for old server, entire body (original path). Recalculates integrity hashes for all paths.
func (c *BackendClient) pullDocs(ctx context.Context, repoID string, docWants []domain.ContentHash, docWantSet map[domain.ContentHash]bool) ([]domain.SessionDoc, error) {
	if len(docWants) == 0 {
		return nil, nil
	}
	// 1) Manifest request (new) — old server ignores fields, returns empty response → fallback to entire body.
	// New server can return entire body for unplanable docs (mixed response).
	var manResp pullResp
	if err := c.do(ctx, http.MethodPost, c.reposPath(repoID)+"/pull/objects", pullReq{DocManifestWants: docWants, ChunkFormatsSupported: []string{chunkcas.FormatV1, chunkcas.FormatV2}}, &manResp); err != nil {
		return nil, err
	}
	if len(manResp.DocManifests) == 0 && len(manResp.Docs) == 0 {
		var docResp pullResp
		if err := c.do(ctx, http.MethodPost, c.reposPath(repoID)+"/pull/objects", pullReq{DocWants: docWants}, &docResp); err != nil {
			return nil, err
		}
		seenDocs := make(map[domain.ContentHash]bool, len(docResp.Docs))
		for _, d := range docResp.Docs {
			if err := domain.ValidateSessionDocHash(d); err != nil {
				return nil, err
			}
			if !docWantSet[d.Hash] || seenDocs[d.Hash] {
				return nil, domain.ErrHashMismatch
			}
			seenDocs[d.Hash] = true
		}
		if len(seenDocs) != len(docWantSet) {
			return nil, domain.ErrHashMismatch
		}
		return docResp.Docs, nil
	}
	// 2) Chunk paths: Manifest ∪ fallback doc must cover entire request.
	seenMan := make(map[domain.ContentHash]bool, len(manResp.DocManifests))
	var docs []domain.SessionDoc
	for _, d := range manResp.Docs {
		if err := domain.ValidateSessionDocHash(d); err != nil {
			return nil, err
		}
		if !docWantSet[d.Hash] || seenMan[d.Hash] {
			return nil, domain.ErrHashMismatch
		}
		seenMan[d.Hash] = true
		docs = append(docs, d)
	}
	needSet := map[domain.ContentHash]bool{}
	var need []domain.ContentHash
	bodies := map[domain.ContentHash][]byte{}
	for _, m := range manResp.DocManifests {
		if !docWantSet[m.Hash] || seenMan[m.Hash] || len(m.Chunks) == 0 || !chunkcas.SupportedFormat(m.Format) {
			return nil, domain.ErrHashMismatch
		}
		seenMan[m.Hash] = true
		for _, ch := range m.Chunks {
			if err := domain.ValidateContentHash(ch); err != nil {
				return nil, err
			}
			if _, ok := bodies[ch]; ok || needSet[ch] {
				continue
			}
			if c.chunks != nil && c.chunks.HasChunk(ch) {
				if b, gerr := c.chunks.GetChunk(ch); gerr == nil && domain.HashContent(b) == ch {
					bodies[ch] = b // Local held chunk — reception skipped (delta)
					continue
				}
			}
			needSet[ch] = true
			need = append(need, ch)
		}
	}
	if len(seenMan) != len(docWantSet) {
		return nil, domain.ErrHashMismatch
	}
	if len(need) > 0 && manResp.BoundedChunksSupported {
		pending := need
		for len(pending) > 0 {
			count := len(pending)
			if count > maxChunkWireObjects {
				count = maxChunkWireObjects
			}
			wants := pending[:count]
			var chunkResp pullResp
			if err := c.doLimited(ctx, http.MethodPost, c.reposPath(repoID)+"/pull/chunks", pullReq{ChunkWants: wants}, &chunkResp, maxChunkWireJSONBody); err != nil {
				return nil, err
			}
			if len(chunkResp.ChunkObjects) == 0 || len(chunkResp.ChunkObjects) > len(wants) {
				return nil, domain.ErrHashMismatch
			}
			rawTotal := 0
			for i, co := range chunkResp.ChunkObjects {
				rawTotal += len(co.Data)
				if len(co.Data) == 0 || len(co.Data) > maxChunkWireRawBytes || rawTotal > maxChunkWireRawBytes {
					return nil, domain.ErrHashMismatch
				}
				if co.Hash != wants[i] || !needSet[co.Hash] || domain.HashContent(co.Data) != co.Hash {
					return nil, domain.ErrHashMismatch
				}
				bodies[co.Hash] = co.Data
			}
			pending = pending[len(chunkResp.ChunkObjects):]
		}
	} else if len(need) > 0 {
		var chunkResp pullResp
		if err := c.do(ctx, http.MethodPost, c.reposPath(repoID)+"/pull/objects", pullReq{ChunkWants: need}, &chunkResp); err != nil {
			return nil, err
		}
		for _, co := range chunkResp.ChunkObjects {
			if !needSet[co.Hash] || domain.HashContent(co.Data) != co.Hash {
				return nil, domain.ErrHashMismatch
			}
			bodies[co.Hash] = co.Data
		}
	}
	for _, m := range manResp.DocManifests {
		chunks := make([][]byte, 0, len(m.Chunks))
		for _, ch := range m.Chunks {
			b, ok := bodies[ch]
			if !ok {
				return nil, domain.ErrHashMismatch // Server did not send missing chunk
			}
			chunks = append(chunks, b)
		}
		// AssembleChunks validates integrity hashes (equivalent to ValidateSessionDocHash for entire path).
		cb, aerr := chunkcas.AssembleChunks(chunkcas.Manifest{Format: m.Format, Envelope: m.Envelope, Chunks: m.Chunks}, chunks, m.Hash)
		if aerr != nil {
			return nil, aerr
		}
		var cir domain.CIRDocument
		if err := json.Unmarshal(cb, &cir); err != nil {
			return nil, domain.ErrInvalidCIR
		}
		docs = append(docs, domain.SessionDoc{Hash: m.Hash, CIR: cir})
	}
	return docs, nil
}
