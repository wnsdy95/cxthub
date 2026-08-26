package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

const sessionAffinityTTL = 30 * 24 * time.Hour

type sessionAffinityFile struct {
	Provider  domain.ProviderKind `json:"provider"`
	SessionID string              `json:"session_id"`
	UpdatedAt time.Time           `json:"updated_at"`
}

func terminalIdentity() string {
	if id := os.Getenv("TERM_SESSION_ID"); id != "" {
		return id
	}
	return os.Getenv("ITERM_SESSION_ID")
}

func affinityPath(provider domain.ProviderKind) string {
	terminal := terminalIdentity()
	if terminal == "" {
		return ""
	}
	key := domain.HashContent([]byte(terminal + "\x00" + string(provider)))
	return filepath.Join(".cxt", "session-affinity", strings.TrimPrefix(string(key), "sha256:")+".json")
}

// RecordSessionAffinity binds only this local terminal to its provider session.
// The opaque terminal ID is hashed and never synchronized to the server.
func RecordSessionAffinity(cwd string, provider domain.ProviderKind, sessionID string) {
	if !providerfs.ValidSessionID(sessionID) {
		return
	}
	rel := affinityPath(provider)
	if rel == "" {
		return
	}
	b, err := json.Marshal(sessionAffinityFile{Provider: provider, SessionID: sessionID, UpdatedAt: time.Now().UTC()})
	if err != nil {
		return
	}
	_ = providerfs.WriteRepoFileAtomic(cwd, rel, b, 0o600)
}

// SessionAffinity returns the last capture from this exact terminal/provider.
func SessionAffinity(cwd string, provider domain.ProviderKind) string {
	rel := affinityPath(provider)
	if rel == "" {
		return ""
	}
	b, err := providerfs.ReadRepoFile(cwd, rel)
	if err != nil {
		return ""
	}
	var affinity sessionAffinityFile
	if json.Unmarshal(b, &affinity) != nil || affinity.Provider != provider ||
		!providerfs.ValidSessionID(affinity.SessionID) || time.Since(affinity.UpdatedAt) > sessionAffinityTTL {
		return ""
	}
	return affinity.SessionID
}
