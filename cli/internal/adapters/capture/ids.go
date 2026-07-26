package capture

import "github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"

// newSessionID generates a UUIDv4-like identifier for session filenames (uses crypto/rand without external dependencies).
// Format: 8-4-4-4-12 hex (RFC4122 v4 variant bit settings).
func newSessionID() string {
	return providerfs.NewSessionID()
}

// pickLatest selects the path with the latest mtime from the (path, mtimeUnixNano) candidates.
// Returns an empty string if no candidates are found.
func pickLatest(candidates map[string]int64) string {
	best := ""
	var bestMtime int64 = -1
	for path, mtime := range candidates {
		if mtime > bestMtime {
			bestMtime = mtime
			best = path
		}
	}
	return best
}
