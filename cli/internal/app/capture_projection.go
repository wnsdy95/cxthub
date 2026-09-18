package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type appendCodec interface {
	DecodeAppend(context.Context, []byte, domain.Envelope, int) (domain.CIRDocument, error)
}
type appendCaptureStore interface {
	AppendCaptureDoc(context.Context, domain.ContentHash, domain.CIRDocument) (domain.ContentHash, error)
}

// A disposable projection hint, not an accepted capture/commit receipt. The
// content-addressed document is durable before this checkpoint is published.
type captureProjection struct {
	Version  int                `json:"version"`
	Offset   int64              `json:"offset"`
	Prefix   string             `json:"prefix"`
	Policy   string             `json:"policy"`
	Doc      domain.ContentHash `json:"doc"`
	Envelope domain.Envelope    `json:"envelope"`
	Events   int                `json:"events"`
	Checksum string             `json:"checksum"`
}

func (p captureProjection) sum() string {
	p.Checksum = ""
	b, _ := json.Marshal(p)
	return string(domain.HashContent(b))
}

// projectCapture reuses normalized chunks while checking the complete native
// prefix with a bounded-memory hash. Merely observing growth cannot prove that
// older native bytes were not edited; a prefix mismatch rebuilds the projection.
func (s *SaveSessionService) projectCapture(ctx context.Context, root, path string, capt outbound.CaptureSource, cdc outbound.ProviderCodec, allowPartial ...bool) (domain.Envelope, domain.ContentHash, int64, *time.Time, error) {
	incremental, codecOK := cdc.(appendCodec)
	store, storeOK := s.store.(appendCaptureStore)
	native, nativeOK := capt.(interface{ IncrementalCapture() bool })
	if !codecOK || !storeOK || !nativeOK || !native.IncrementalCapture() {
		var at *time.Time
		if st, e := os.Stat(path); e == nil {
			v := st.ModTime().UTC()
			at = &v
		}
		raw, err := capt.ReadSession(ctx, path)
		if err != nil {
			return domain.Envelope{}, "", 0, at, err
		}
		n := int64(len(raw))
		raw, _ = capture.ScrubSecrets(raw, root)
		doc, err := cdc.Decode(ctx, raw)
		if err != nil {
			return domain.Envelope{}, "", 0, at, err
		}
		doc = capture.ScrubDoc(doc, root)
		hash, err := s.store.PutDoc(ctx, domain.SessionDoc{CIR: doc})
		return doc.Envelope, hash, n, at, err
	}
	key := sha256.Sum256([]byte(string(capt.Provider()) + "\x00" + path))
	cache, err := providerfs.PrepareRepoFile(root, filepath.Join(".cxt", "capture", "projections", fmt.Sprintf("%x.json", key)), 0700)
	if err != nil {
		return domain.Envelope{}, "", 0, nil, err
	}
	lock, err := os.OpenFile(cache+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return domain.Envelope{}, "", 0, nil, err
	}
	defer lock.Close()
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return domain.Envelope{}, "", 0, nil, err
		}
		select {
		case <-ctx.Done():
			return domain.Envelope{}, "", 0, nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	policy, err := capture.ScrubPolicyFingerprint(root)
	if err != nil {
		return domain.Envelope{}, "", 0, nil, err
	}
	f, err := providerfs.OpenRegularFile(path)
	if err != nil {
		return domain.Envelope{}, "", 0, nil, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return domain.Envelope{}, "", 0, nil, err
	}
	at := stat.ModTime().UTC()
	var prior captureProjection
	if b, e := providerfs.ReadRegularFile(cache); e == nil {
		_ = json.Unmarshal(b, &prior)
	}
	h := sha256.New()
	valid := prior.Version == 1 && prior.Offset > 0 && prior.Offset <= stat.Size() && prior.Events >= 0 && prior.Policy == policy && prior.Checksum == prior.sum() && domain.ValidateContentHash(prior.Doc) == nil
	if valid {
		_, err = io.CopyN(h, contextReader{ctx, f}, prior.Offset)
		valid = err == nil && hex.EncodeToString(h.Sum(nil)) == prior.Prefix
	}
	if !valid {
		prior = captureProjection{}
		h.Reset()
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			return domain.Envelope{}, "", 0, &at, err
		}
	}
	raw, err := io.ReadAll(io.LimitReader(contextReader{ctx, f}, stat.Size()-prior.Offset))
	if err != nil {
		return domain.Envelope{}, "", 0, &at, err
	}
	// A final complete JSON object is valid even without a trailing newline.
	// A partially written record is left at the cursor for the next observation.
	end := len(raw)
	last := bytes.LastIndexByte(raw, '\n') + 1
	if len(bytes.TrimSpace(raw[last:])) > 0 && !json.Valid(bytes.TrimSpace(raw[last:])) {
		if len(allowPartial) == 0 || !allowPartial[0] {
			return domain.Envelope{}, "", 0, &at, fmt.Errorf("native transcript ends in an incomplete record; retry capture")
		}
		end = last
	}
	raw = raw[:end]
	if prior.Offset == 0 && len(bytes.TrimSpace(raw)) == 0 {
		return domain.Envelope{}, "", 0, &at, domain.ErrNoActiveSession
	}
	h.Write(raw)
	offset := prior.Offset + int64(len(raw))
	raw, _ = capture.ScrubSecrets(raw, root)
	delta, err := incremental.DecodeAppend(ctx, raw, prior.Envelope, prior.Events)
	if err != nil {
		return domain.Envelope{}, "", 0, &at, err
	}
	delta = capture.ScrubDoc(delta, root)
	if current, e := capture.ScrubPolicyFingerprint(root); e != nil || current != policy {
		return domain.Envelope{}, "", 0, &at, fmt.Errorf("capture masking policy changed; retry capture")
	}
	hash, err := store.AppendCaptureDoc(ctx, prior.Doc, delta)
	if errors.Is(err, domain.ErrNotFound) && prior.Doc != "" {
		// GC may have removed a replaced pending document. Invalidate only this
		// disposable hint; the next capture reconstructs from the native file.
		_ = providerfs.WriteRegularFileAtomic(cache, []byte("{}"), 0600)
	}
	if err != nil {
		return domain.Envelope{}, "", 0, &at, err
	}
	next := captureProjection{Version: 1, Offset: offset, Prefix: hex.EncodeToString(h.Sum(nil)), Policy: policy, Doc: hash, Envelope: delta.Envelope, Events: prior.Events + len(delta.Events)}
	next.Checksum = next.sum()
	b, err := json.Marshal(next)
	if err == nil {
		err = providerfs.WriteRegularFileAtomic(cache, b, 0600)
	}
	return delta.Envelope, hash, offset, &at, err
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
