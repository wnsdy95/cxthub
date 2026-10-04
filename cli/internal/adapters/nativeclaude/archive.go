package nativeclaude

import (
	"context"
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
	if err := ctx.Err(); err != nil {
		return ReferenceReceipt{}, err
	}
	select {
	case <-s.closed:
	default:
		return ReferenceReceipt{}, ErrState
	}
	s.mu.Lock()
	receipt := s.receipt
	err := s.closeErr
	s.mu.Unlock()
	if err != nil || !receipt.NoTurnAcknowledged {
		return ReferenceReceipt{}, ErrState
	}
	rel := filepath.Join(providerfs.EncodeCwd(s.cwd), s.id+".jsonl")
	if !filepath.IsAbs(path) || filepath.Clean(path) != filepath.Join(s.archiveRoot, rel) {
		return ReferenceReceipt{}, ErrState
	}
	root, err := os.OpenRoot(s.archiveRoot)
	if err != nil {
		return ReferenceReceipt{}, ErrState
	}
	defer root.Close()
	f, err := openArchive(root, rel)
	if err != nil {
		return ReferenceReceipt{}, ErrState
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<20 {
		return ReferenceReceipt{}, ErrLimit
	}
	matches := 0
	err = readFrames(io.LimitReader(f, (64<<20)+1), func(raw []byte) error {
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
	if err != nil {
		return ReferenceReceipt{}, err
	}
	after, err := f.Stat()
	if err != nil || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		return ReferenceReceipt{}, ErrState
	}
	if err := ctx.Err(); err != nil {
		return ReferenceReceipt{}, err
	}
	if matches != 1 {
		return ReferenceReceipt{}, ErrProtocol
	}
	receipt.Persisted = true
	return receipt, nil
}
