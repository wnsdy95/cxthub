package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// AppendCaptureDoc retains closed canonical event chunks. Old bytes are hashed
// for integrity, but never decoded, masked or canonicalized again. Identity is
// exactly the existing whole-document hash, including the updated envelope.
func (s *FileStore) AppendCaptureDoc(ctx context.Context, base domain.ContentHash, delta domain.CIRDocument) (domain.ContentHash, error) {
	var hash domain.ContentHash
	err := s.WithObjectsRetained(ctx, func() error {
		var err error
		hash, err = s.appendCaptureDoc(ctx, base, delta)
		return err
	})
	return hash, err
}

func (s *FileStore) appendCaptureDoc(ctx context.Context, base domain.ContentHash, delta domain.CIRDocument) (domain.ContentHash, error) {
	if base == "" {
		return s.putDoc(domain.SessionDoc{CIR: delta})
	}
	if err := domain.ValidateContentHash(base); err != nil {
		return "", err
	}
	raw, err := readCxtFile(s.objectPath("docs", base))
	if os.IsNotExist(err) {
		return "", domain.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	raw, err = docDecompress(raw)
	if err != nil {
		return "", err
	}
	man, ok := chunkcas.ParseManifest(raw)
	if !ok || man.Format != chunkcas.FormatV2 {
		prior, err := s.GetDoc(ctx, base)
		if err != nil {
			return "", err
		}
		delta.Events = append(prior.CIR.Events, delta.Events...)
		return s.putDoc(domain.SessionDoc{CIR: delta})
	}
	cb, err := domain.CanonicalBytes(delta)
	if err != nil {
		return "", err
	}
	var parts struct {
		Envelope json.RawMessage   `json:"envelope"`
		Events   []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(cb, &parts); err != nil {
		return "", err
	}
	oldHash, newHash := sha256.New(), sha256.New()
	oldHash.Write([]byte(`{"envelope":`))
	oldHash.Write(man.Envelope)
	oldHash.Write([]byte(`,"events":[`))
	newHash.Write([]byte(`{"envelope":`))
	newHash.Write(parts.Envelope)
	newHash.Write([]byte(`,"events":[`))
	next := chunkcas.Manifest{Format: chunkcas.FormatV2, Envelope: parts.Envelope}
	var tail []byte
	for i, hash := range man.Chunks {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		chunk, err := s.GetChunk(hash)
		if err != nil {
			return "", err
		}
		if domain.HashContent(chunk) != hash {
			return "", domain.ErrHashMismatch
		}
		oldHash.Write(chunk)
		newHash.Write(chunk)
		if i == len(man.Chunks)-1 {
			tail = chunk
		} else {
			next.Chunks = append(next.Chunks, hash)
		}
	}
	oldHash.Write([]byte(`]}`))
	if domain.ContentHash("sha256:"+hex.EncodeToString(oldHash.Sum(nil))) != base {
		return "", domain.ErrHashMismatch
	}
	var added bytes.Buffer
	for _, event := range parts.Events {
		added.WriteByte(',')
		added.Write(event)
	}
	newHash.Write(added.Bytes())
	newHash.Write([]byte(`]}`))
	target := domain.ContentHash("sha256:" + hex.EncodeToString(newHash.Sum(nil)))
	tail = append(tail, added.Bytes()...)
	for len(tail) > 0 {
		n := min(len(tail), chunkcas.ChunkTarget)
		chunk := tail[:n]
		tail = tail[n:]
		hash := domain.HashContent(chunk)
		next.Chunks = append(next.Chunks, hash)
		existing, err := s.GetChunk(hash)
		if errors.Is(err, domain.ErrNotFound) {
			if err = writeAtomic(s.objectPath("chunks", hash), docCompress(chunk)); err != nil {
				return "", err
			}
		} else if err != nil {
			return "", err
		} else if !bytes.Equal(existing, chunk) {
			return "", domain.ErrHashMismatch
		}
	}
	mb, err := json.Marshal(next)
	if err != nil {
		return "", fmt.Errorf("capture manifest: %w", err)
	}
	if err := writeAtomic(s.objectPath("docs", target), docCompress(mb)); err != nil {
		return "", err
	}
	return target, nil
}
