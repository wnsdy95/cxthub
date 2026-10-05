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

// VerifyArchive is read-only and available only after successful Close, which
// includes complete protocol EOF auditing. The supplied path must be this new
// native session's exact transcript under its frozen configuration directory.
// Opening is scoped against traversal and nonblocking against special files.
// No archive or configuration file is created, rewritten or removed here.
func (s *Session) VerifyArchive(ctx context.Context, path string) (ReferenceReceipt, error) {
	verified, err := s.verifyArchive(ctx, path)
	if err != nil {
		return ReferenceReceipt{}, err
	}
	s.mu.Lock()
	s.verifiedArchive = &verified
	s.mu.Unlock()
	return verified.receipt, nil
}

type archiveVerification struct {
	path    string
	receipt ReferenceReceipt
	info    os.FileInfo
	digest  [sha256.Size]byte
}

func (s *Session) verifyArchive(ctx context.Context, path string) (archiveVerification, error) {
	if err := ctx.Err(); err != nil {
		return archiveVerification{}, err
	}
	select {
	case <-s.closed:
	default:
		return archiveVerification{}, ErrState
	}
	s.mu.Lock()
	receipt := s.receipt
	err := s.closeErr
	queried := s.firstQuestion != nil
	s.mu.Unlock()
	if err != nil || queried || !receipt.NoTurnAcknowledged {
		return archiveVerification{}, ErrState
	}
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
	matches := 0
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
		var kind string
		_ = json.Unmarshal(m["type"], &kind)
		var subtype string
		_ = json.Unmarshal(m["subtype"], &subtype)
		var compact bool
		_ = json.Unmarshal(m["isCompactSummary"], &compact)
		if kind == "assistant" || kind == "result" || subtype == "compact_boundary" || compact {
			return ErrProtocol
		}
		if kind != "user" {
			return nil
		}
		id, err := stringField(m, "uuid")
		if err != nil || id != receipt.MessageID {
			return ErrProtocol
		}
		id, err = stringField(m, "sessionId")
		if err != nil || id != s.id {
			return ErrProtocol
		}
		text, err := referenceText(m["message"])
		if err != nil {
			return err
		}
		if len(text) != receipt.NativeUTF8Bytes || hashText(text) != receipt.NativeContentHash {
			return ErrProtocol
		}
		matches++
		if matches != 1 {
			return ErrProtocol
		}
		return nil
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
	if matches != 1 {
		return archiveVerification{}, ErrProtocol
	}
	receipt.Persisted = true
	verified := archiveVerification{path: path, receipt: receipt, info: after}
	copy(verified.digest[:], digest.Sum(nil))
	return verified, nil
}
