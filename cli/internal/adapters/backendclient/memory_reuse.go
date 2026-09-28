package backendclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sync"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

const memoryReuseEntries = 16

type memoryReuseRequest struct {
	Version            uint32              `json:"version"`
	BaseHash           domain.ContentHash  `json:"base_hash"`
	MemoryHash         domain.ContentHash  `json:"memory_hash"`
	PreviousMemoryHash domain.ContentHash  `json:"previous_memory_hash,omitempty"`
	Provider           domain.ProviderKind `json:"provider"`
}

// Hints hold only hashes of acknowledged objects, not memory bodies or tokens.
// Losing a hint is harmless; all authority and CAS checks remain on the server.
type memoryReuseCache struct {
	mu       sync.Mutex
	scope    domain.ContentHash
	disabled bool
	entries  []memoryReuseEntry
}

type memoryReuseEntry struct{ body, object domain.ContentHash }

func (m *memoryReuseCache) lookup(scope, body domain.ContentHash) domain.ContentHash {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.scope != scope {
		m.scope, m.disabled, m.entries = scope, false, nil
	}
	if !m.disabled {
		for _, e := range m.entries {
			if e.body == body {
				return e.object
			}
		}
	}
	return ""
}

func (m *memoryReuseCache) remember(scope, body, object domain.ContentHash) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// A concurrent change of repository/credential must not repopulate its cache.
	if m.scope != scope || m.disabled {
		return
	}
	for i := range m.entries {
		if m.entries[i].body == body {
			m.entries[i].object = object
			return
		}
	}
	if len(m.entries) == memoryReuseEntries {
		m.entries = m.entries[1:]
	}
	m.entries = append(m.entries, memoryReuseEntry{body: body, object: object})
}

func (m *memoryReuseCache) disable(scope domain.ContentHash) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.scope == scope {
		m.disabled, m.entries = true, nil
	}
}

func (c *BackendClient) pushMemoryWithReuse(ctx context.Context, repo string, d domain.MemoryDigest, want domain.ContentHash) error {
	// Pin lazy credential/URL resolution for this operation, including fallback.
	baseURL, token := c.baseURL(), c.token()
	call := &BackendClient{baseURL: func() string { return baseURL }, token: func() string { return token }, identity: c.identity, httpc: c.httpc}
	publicBody := d
	publicBody.SnapshotID, publicBody.PreviousMemoryHash, publicBody.Provider = "", "", ""
	raw, err := json.Marshal(publicBody)
	if err != nil {
		return err
	}
	// Small digests are cheaper to send directly than to negotiate reuse.
	if len(raw) < 4096 {
		return call.pushMemoryFull(ctx, repo, d, want)
	}
	body := domain.HashContent(raw)
	scopeBytes, _ := json.Marshal([3]string{baseURL, repo, token})
	scope := domain.HashContent(scopeBytes)
	base := c.memoryReuse.lookup(scope, body)
	if base != "" {
		in := memoryReuseRequest{Version: 1, BaseHash: base, MemoryHash: want, PreviousMemoryHash: d.PreviousMemoryHash, Provider: d.Provider}
		var out struct {
			MemoryHash domain.ContentHash `json:"memory_hash"`
		}
		path := call.reposPath(repo) + "/memory-reuses/" + url.PathEscape(string(d.SnapshotID))
		err = call.doLimited(ctx, http.MethodPut, path, in, &out, 4096)
		if err == nil {
			if out.MemoryHash != want {
				return domain.ErrHashMismatch
			}
			c.memoryReuse.remember(scope, body, want)
			return nil
		}
		var response *HTTPError
		if !errors.As(err, &response) || (response.Status != http.StatusNotFound && response.Status != http.StatusMethodNotAllowed) {
			return err // no fallback for authority, CAS, integrity or transport failure
		}
		// An old peer or a collected base is only a missed optimization. Avoid
		// repeating probes for this scope in this client process.
		c.memoryReuse.disable(scope)
	}
	if err := call.pushMemoryFull(ctx, repo, d, want); err != nil {
		return err
	}
	c.memoryReuse.remember(scope, body, want)
	return nil
}
