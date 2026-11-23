package backendclient

import (
	"sync"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// Hints hold only hashes of acknowledged objects, not memory bodies or tokens.
// Losing a hint is harmless; all authority and CAS checks remain on the server.
type memoryReuseCache struct {
	mu       sync.Mutex
	scope    domain.ContentHash
	disabled bool
	entries  []memoryReuseEntry
}

type memoryReuseEntry struct{ body, object domain.ContentHash }
