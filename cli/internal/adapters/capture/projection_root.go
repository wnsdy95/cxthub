package capture

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
	"sort"
	"syscall"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// Project selects identity before reading or installing anything. Root captures
// rebuild the full scrubbed source; legacy append hints never attest a root.
func (s *SessionCaptureAdapter) Project(ctx context.Context, root, path string, capt outbound.CaptureSource, cdc outbound.ProviderCodec, allowPartial bool, identity domain.DocumentIdentity) (domain.Envelope, domain.DocumentRef, int64, *time.Time, error) {
	if err := ctx.Err(); err != nil {
		return domain.Envelope{}, domain.DocumentRef{}, 0, nil, err
	}
	if err := identity.Validate(); err != nil {
		return domain.Envelope{}, domain.DocumentRef{}, 0, nil, err
	}
	if identity == domain.DocumentIdentityLegacy {
		env, hash, n, at, err := s.projectLegacy(ctx, root, path, capt, cdc, allowPartial)
		return env, domain.DocumentRef{Hash: hash}, n, at, err
	}
	return s.projectRoot(ctx, root, path, capt, cdc, allowPartial)
}

type rootCaptureStore interface {
	outbound.RootDocumentStore
	outbound.ObjectRetention
	PutChunk(context.Context, domain.ContentHash, []byte) error
}

func (s *SessionCaptureAdapter) projectRoot(ctx context.Context, root, path string, capt outbound.CaptureSource, cdc outbound.ProviderCodec, allowPartial bool) (domain.Envelope, domain.DocumentRef, int64, *time.Time, error) {
	store, ok := s.store.(rootCaptureStore)
	if !ok {
		return domain.Envelope{}, domain.DocumentRef{}, 0, nil, domain.ErrUnsupportedDocumentIdentity
	}
	key := sha256.Sum256([]byte(string(capt.Provider()) + "\x00" + path))
	cache, err := providerfs.PrepareRepoFile(root, filepath.Join(".cxt", "capture", "projections", fmt.Sprintf("%x.json", key)), 0700)
	if err != nil {
		return domain.Envelope{}, domain.DocumentRef{}, 0, nil, err
	}
	lock, err := os.OpenFile(cache+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return domain.Envelope{}, domain.DocumentRef{}, 0, nil, err
	}
	defer lock.Close()
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return domain.Envelope{}, domain.DocumentRef{}, 0, nil, err
		}
		select {
		case <-ctx.Done():
			return domain.Envelope{}, domain.DocumentRef{}, 0, nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	policy, err := ScrubPolicyFingerprint(root)
	if err != nil {
		return domain.Envelope{}, domain.DocumentRef{}, 0, nil, err
	}
	var raw []byte
	var at *time.Time
	// Match legacy JSONL capture's stable size and complete-record boundary.
	native, nativeOK := capt.(interface{ IncrementalCapture() bool })
	if nativeOK && native.IncrementalCapture() {
		f, e := providerfs.OpenRegularFile(path)
		if e != nil {
			return domain.Envelope{}, domain.DocumentRef{}, 0, nil, e
		}
		defer f.Close()
		stat, e := f.Stat()
		if e != nil {
			return domain.Envelope{}, domain.DocumentRef{}, 0, nil, e
		}
		stamp := stat.ModTime().UTC()
		at = &stamp
		raw, err = io.ReadAll(io.LimitReader(contextReader{ctx, f}, stat.Size()))
		if err == nil && int64(len(raw)) != stat.Size() {
			err = fmt.Errorf("native transcript changed during capture; retry capture")
		}
		if err == nil {
			last := bytes.LastIndexByte(raw, '\n') + 1
			if len(bytes.TrimSpace(raw[last:])) > 0 && !json.Valid(bytes.TrimSpace(raw[last:])) {
				if !allowPartial {
					err = fmt.Errorf("native transcript ends in an incomplete record; retry capture")
				} else {
					raw = raw[:last]
				}
			}
		}
	} else {
		if stat, e := os.Stat(path); e == nil {
			stamp := stat.ModTime().UTC()
			at = &stamp
		}
		raw, err = capt.ReadSession(ctx, path)
	}
	if err != nil {
		return domain.Envelope{}, domain.DocumentRef{}, 0, at, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return domain.Envelope{}, domain.DocumentRef{}, 0, at, domain.ErrNoActiveSession
	}
	n := int64(len(raw))
	prefix := sha256.Sum256(raw)
	raw, _ = ScrubSecrets(raw, root)
	doc, err := cdc.Decode(ctx, raw)
	if err != nil {
		return domain.Envelope{}, domain.DocumentRef{}, 0, at, err
	}
	doc = ScrubDoc(doc, root)
	manifest, bodies, err := domain.ConversationManifestForCIR(doc)
	if err != nil {
		return domain.Envelope{}, domain.DocumentRef{}, 0, at, err
	}
	hash, err := domain.ConversationManifestHash(manifest)
	if err != nil {
		return domain.Envelope{}, domain.DocumentRef{}, 0, at, err
	}
	canonical, err := domain.CanonicalConversationManifest(manifest)
	if err != nil {
		return domain.Envelope{}, domain.DocumentRef{}, 0, at, err
	}
	ref := domain.DocumentRef{Hash: hash, Identity: domain.DocumentIdentityRootV1}
	if current, e := ScrubPolicyFingerprint(root); e != nil || current != policy {
		return domain.Envelope{}, domain.DocumentRef{}, 0, at, fmt.Errorf("capture masking policy changed; retry capture")
	}
	err = store.WithObjectsRetained(ctx, func() error {
		hashes := make([]domain.ContentHash, 0, len(bodies))
		for h := range bodies {
			hashes = append(hashes, h)
		}
		sort.Slice(hashes, func(i, j int) bool { return hashes[i] < hashes[j] })
		for _, h := range hashes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := store.PutChunk(ctx, h, bodies[h]); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return store.PutConversationManifest(ctx, domain.DocumentRepresentation{Hash: hash, Identity: ref.Identity, RootManifest: canonical})
	})
	if err != nil {
		return domain.Envelope{}, domain.DocumentRef{}, 0, at, err
	}
	if err = ctx.Err(); err != nil {
		return domain.Envelope{}, domain.DocumentRef{}, 0, at, err
	}
	next := captureProjection{Version: 2, DocIdentity: ref.Identity, Offset: n, Prefix: hex.EncodeToString(prefix[:]), Policy: policy, Doc: hash, Envelope: doc.Envelope, Events: len(doc.Events)}
	next.Checksum = next.sum()
	b, err := json.Marshal(next)
	if err == nil {
		err = providerfs.WriteRegularFileAtomic(cache, b, 0600)
	}
	return doc.Envelope, ref, n, at, err
}
