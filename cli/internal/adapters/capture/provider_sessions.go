package capture

import (
	"bufio"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func (resolver *CommandSessionResolver) MatchesSession(cwd string, provider domain.ProviderKind, sessionID, path string) bool {
	if resolver == nil || resolver.claude == nil || resolver.codex == nil || providerfs.SessionIDFromPath(path) != sessionID {
		return false
	}
	switch provider {
	case domain.ProviderClaude:
		return true
	case domain.ProviderCodex:
		return validCodexRetirementTarget(cwd, sessionID, path)
	default:
		return false
	}
}

func validCodexRetirementTarget(cwd, sessionID, path string) bool {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return false
	}
	file, err := providerfs.OpenRegularFile(path)
	if err != nil {
		return false
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, 1<<20))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	if !scanner.Scan() {
		return false
	}
	var header struct {
		Type    string `json:"type"`
		Payload struct {
			ID  string `json:"id"`
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	return json.Unmarshal(scanner.Bytes(), &header) == nil && header.Type == "session_meta" &&
		strings.EqualFold(header.Payload.ID, sessionID) && header.Payload.Cwd == abs
}
