package capture

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
)

func validHookSessionID(sessionID string) bool {
	if sessionID == "" || len(sessionID) > 256 || strings.TrimSpace(sessionID) != sessionID {
		return false
	}
	for _, r := range sessionID {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

var ErrSessionIdentityMismatch = errors.New("provider session identity mismatch")

type codexSessionHeader struct {
	Type    string `json:"type"`
	Payload struct {
		ID        string `json:"id"`
		Cwd       string `json:"cwd"`
		Timestamp string `json:"timestamp"`
	} `json:"payload"`
}

// Read only the native header, even for a long-lived, very large rollout.
func readCodexSessionHeader(path string) (codexSessionHeader, error) {
	var header codexSessionHeader
	f, err := providerfs.OpenRegularFile(path)
	if err != nil {
		return header, err
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 1<<20))
	sc.Buffer(make([]byte, 4096), 1<<20)
	if !sc.Scan() {
		return header, fmt.Errorf("missing Codex session header")
	}
	if err := json.Unmarshal(sc.Bytes(), &header); err != nil {
		return header, err
	}
	if header.Type != "session_meta" || !validHookSessionID(header.Payload.ID) || header.Payload.Cwd == "" {
		return header, fmt.Errorf("incomplete Codex session metadata")
	}
	return header, nil
}

func sameNativeSessionID(a, b string) bool {
	if providerfs.ValidSessionID(a) && providerfs.ValidSessionID(b) {
		return strings.EqualFold(a, b)
	}
	return a == b
}
