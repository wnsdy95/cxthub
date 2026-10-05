package nativeclaude

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
)

type archiveVerification struct {
	path     string
	exchange *ExchangeArchiveReceipt
	info     os.FileInfo
	digest   [sha256.Size]byte
}

// readArchive owns bounded, scoped readback and byte/file identity. Semantic
// verification of the completed ordinary exchange is supplied by the caller.
func (s *Session) readArchive(ctx context.Context, path string, visit func(map[string]json.RawMessage) error) (archiveVerification, error) {
	rel := filepath.Join(providerfs.EncodeCwd(s.cwd), s.id+".jsonl")
	if !filepath.IsAbs(path) || filepath.Clean(path) != filepath.Join(s.archiveRoot, rel) {
		return archiveVerification{}, ErrState
	}
	root, err := os.OpenRoot(s.archiveRoot)
	if err != nil {
		return archiveVerification{}, ErrState
	}
	defer root.Close()
	f, err := openArchive(root, rel)
	if err != nil {
		return archiveVerification{}, ErrState
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<20 {
		return archiveVerification{}, ErrLimit
	}
	digest := sha256.New()
	limited := &io.LimitedReader{R: idleContextReader{ctx: ctx, r: f}, N: (64 << 20) + 1}
	err = readFrames(io.TeeReader(limited, digest), func(raw []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		m, err := object(raw)
		if err != nil {
			return err
		}
		if raw, ok := m["sessionId"]; ok {
			var id string
			if json.Unmarshal(raw, &id) != nil || id != s.id {
				return ErrProtocol
			}
		}
		if raw, ok := m["cwd"]; ok {
			var cwd string
			if json.Unmarshal(raw, &cwd) != nil || cwd != s.cwd {
				return ErrProtocol
			}
		}
		return visit(m)
	})
	if ctx.Err() != nil {
		return archiveVerification{}, ctx.Err()
	}
	if limited.N == 0 {
		return archiveVerification{}, ErrLimit
	}
	if err != nil {
		return archiveVerification{}, err
	}
	after, err := f.Stat()
	if err != nil || !sameIdleFile(info, after) {
		return archiveVerification{}, ErrState
	}
	if err := ctx.Err(); err != nil {
		return archiveVerification{}, err
	}
	verified := archiveVerification{path: path, info: after}
	copy(verified.digest[:], digest.Sum(nil))
	return verified, nil
}
