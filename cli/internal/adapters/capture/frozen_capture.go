package capture

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

var _ outbound.FrozenSessionCapture = (*SessionCaptureAdapter)(nil)

// Freeze copies a fixed prefix in bounded memory and independently re-hashes
// that complete prefix through a fresh source open. Growth is allowed; rotation,
// truncation, prefix rewrites and masking-policy drift fail closed. Failed and
// successful raw chunks are retained as private evidence, never normal CAS.
func (s *SessionCaptureAdapter) Freeze(ctx context.Context, root, path string, provider domain.ProviderKind, identity domain.DocumentIdentity) (domain.FrozenCaptureInput, error) {
	var empty domain.FrozenCaptureInput
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if provider != domain.ProviderClaude && provider != domain.ProviderCodex {
		return empty, domain.ErrFrozenCaptureInput
	}
	if err := identity.Validate(); err != nil {
		return empty, err
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return empty, err
	}
	policy, err := ScrubPolicyFingerprint(root)
	if err != nil {
		return empty, err
	}
	f, err := openFrozenNative(path)
	if err != nil {
		return empty, err
	}
	defer f.Close()
	initial, err := f.Stat()
	if err != nil {
		return empty, err
	}
	if initial.Size() == 0 {
		return empty, domain.ErrNoActiveSession
	}
	if initial.Size() < 0 || initial.Size() > domain.FrozenCaptureMaxBytes {
		return empty, domain.ErrFrozenCaptureInput
	}
	dir, err := openFrozenDir(root, true)
	if err != nil {
		return empty, err
	}
	defer dir.Close()
	in := domain.FrozenCaptureInput{Version: domain.FrozenCaptureVersion, Provider: provider,
		SourcePath: path, Size: initial.Size(), CapturedAt: time.Now().UTC(),
		PolicyFingerprint: policy, DocIdentity: identity}
	h := sha256.New()
	buf := make([]byte, domain.FrozenCaptureChunkBytes)
	for remaining := in.Size; remaining > 0; {
		part := buf[:min(remaining, int64(len(buf)))]
		if _, err := io.ReadFull(contextReader{ctx, f}, part); err != nil {
			return empty, err
		}
		h.Write(part)
		chunk := domain.FrozenCaptureChunk{Hash: domain.HashContent(part), Size: int64(len(part))}
		if err := putFrozenChunk(ctx, dir, chunk, part); err != nil {
			return empty, err
		}
		in.Chunks = append(in.Chunks, chunk)
		remaining -= chunk.Size
	}
	in.Hash = domain.ContentHash("sha256:" + hex.EncodeToString(h.Sum(nil)))
	if err := verifyFrozenNativePrefix(ctx, path, initial, in.Size, in.Hash); err != nil {
		return empty, err
	}
	if err := checkFrozenPolicy(ctx, root, policy); err != nil {
		return empty, err
	}
	return in, in.Validate()
}

// ProjectFrozen deliberately lacks an incremental-source capability. Existing
// Project owns masking/normalization/storage; this boundary owns native-byte and
// policy validation. Its existing []byte codec requires O(input size) memory,
// bounded by the 2 GiB input limit, unlike Freeze's 1 MiB copy buffer.
func (s *SessionCaptureAdapter) ProjectFrozen(ctx context.Context, root string, in domain.FrozenCaptureInput, source outbound.CaptureSource, codec outbound.ProviderCodec) (domain.Envelope, domain.DocumentRef, int64, *time.Time, error) {
	fail := func(err error) (domain.Envelope, domain.DocumentRef, int64, *time.Time, error) {
		return domain.Envelope{}, domain.DocumentRef{}, 0, nil, err
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := in.Validate(); err != nil {
		return fail(err)
	}
	if source == nil || codec == nil || s.store == nil || source.Provider() != in.Provider || codec.Provider() != in.Provider {
		return fail(domain.ErrFrozenCaptureInput)
	}
	if err := checkFrozenPolicy(ctx, root, in.PolicyFingerprint); err != nil {
		return fail(err)
	}
	raw, err := readFrozenInput(ctx, root, in)
	if err != nil {
		return fail(err)
	}
	// The generic CaptureSource path has no native partial-record check. Never
	// let a decoder skip an incomplete captured tail and claim the entire input.
	complete := bytes.TrimSpace(raw)
	if len(complete) == 0 {
		return fail(domain.ErrNoActiveSession)
	}
	last := bytes.LastIndexByte(complete, '\n') + 1
	if !json.Valid(bytes.TrimSpace(complete[last:])) {
		return fail(fmt.Errorf("frozen native transcript ends in an incomplete record"))
	}
	guard := func(ctx context.Context) error { return checkFrozenPolicy(ctx, root, in.PolicyFingerprint) }
	if err := guard(ctx); err != nil {
		return fail(err)
	}
	// Embed only the declared port, never the native concrete source's optional
	// IncrementalCapture method. ReadSession cannot consult the mutable source.
	capt := frozenCaptureSource{CaptureSource: source, raw: raw, guard: guard}
	guarded := &frozenProjectionStore{SessionStore: s.store, guard: guard}
	var projectStore outbound.SessionStore = guarded
	if in.DocIdentity == domain.DocumentIdentityRootV1 {
		store, ok := s.store.(rootCaptureStore)
		if !ok {
			return fail(domain.ErrUnsupportedDocumentIdentity)
		}
		projectStore = &frozenRootProjectionStore{frozenProjectionStore: guarded, rootCaptureStore: store}
	}
	env, ref, size, _, err := NewSessionCapture(projectStore).Project(ctx, root, in.SourcePath, capt, codec, false, in.DocIdentity)
	if err != nil {
		return fail(err)
	}
	if err := guard(ctx); err != nil {
		return fail(err)
	}
	if size != in.Size || ref.Identity != in.DocIdentity || ref.Validate() != nil {
		return fail(domain.ErrFrozenCaptureInput)
	}
	at := in.CapturedAt
	return env, ref, size, &at, nil
}

func checkFrozenPolicy(ctx context.Context, root, expected string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	got, err := ScrubPolicyFingerprint(root)
	if err != nil {
		return err
	}
	if got != expected {
		return fmt.Errorf("capture masking policy changed")
	}
	return ctx.Err()
}

// Match providerfs's regular-file/parent policy, with O_NOFOLLOW and NONBLOCK
// before opening, so a raced symlink or special file cannot be read or block.
func openFrozenNative(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, domain.ErrFrozenCaptureInput
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 {
		return nil, domain.ErrFrozenCaptureInput
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		f.Close()
		return nil, domain.ErrFrozenCaptureInput
	}
	return f, nil
}

func verifyFrozenNativePrefix(ctx context.Context, path string, initial os.FileInfo, size int64, want domain.ContentHash) error {
	f, err := openFrozenNative(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(initial, info) || info.Size() < size {
		return domain.ErrFrozenCaptureInput
	}
	h := sha256.New()
	if _, err := io.CopyN(h, contextReader{ctx, f}, size); err != nil {
		return err
	}
	if domain.ContentHash("sha256:"+hex.EncodeToString(h.Sum(nil))) != want {
		return domain.ErrFrozenCaptureInput
	}
	info, err = os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !os.SameFile(initial, info) || info.Size() < size {
		return domain.ErrFrozenCaptureInput
	}
	return ctx.Err()
}

// Each opened child is bound to its pre-open inode; os.Root then anchors all
// subsequent access even if a path is renamed. No symlink component is accepted.
func openFrozenDir(root string, create bool) (*os.Root, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{".cxt", "capture", "inputs"} {
		if create {
			if err := r.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				r.Close()
				return nil, err
			}
		}
		info, err := r.Lstat(name)
		if err != nil {
			r.Close()
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (name == "inputs" && info.Mode().Perm() != 0700) {
			r.Close()
			return nil, domain.ErrFrozenCaptureInput
		}
		child, err := r.OpenRoot(name)
		if err != nil {
			r.Close()
			return nil, err
		}
		actual, err := child.Stat(".")
		if err != nil || !os.SameFile(info, actual) {
			child.Close()
			r.Close()
			return nil, domain.ErrFrozenCaptureInput
		}
		if create {
			err = syncFrozenDir(r)
		}
		r.Close()
		if err != nil {
			child.Close()
			return nil, err
		}
		r = child
	}
	return r, nil
}

func syncFrozenDir(r *os.Root) error {
	f, err := r.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func frozenChunkName(hash domain.ContentHash) string {
	return strings.TrimPrefix(string(hash), "sha256:")
}

func readFrozenChunk(ctx context.Context, dir *os.Root, chunk domain.FrozenCaptureChunk, dst []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := dir.OpenFile(frozenChunkName(chunk.Hash), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != chunk.Size || int64(len(dst)) != chunk.Size {
		return domain.ErrFrozenCaptureInput
	}
	if _, err := io.ReadFull(contextReader{ctx, f}, dst); err != nil {
		return err
	}
	var tail [1]byte
	if n, err := f.Read(tail[:]); n != 0 || err != io.EOF {
		return domain.ErrFrozenCaptureInput
	}
	if domain.HashContent(dst) != chunk.Hash {
		return domain.ErrFrozenCaptureInput
	}
	return ctx.Err()
}

func putFrozenChunk(ctx context.Context, dir *os.Root, chunk domain.FrozenCaptureChunk, data []byte) error {
	verify := func() error {
		got := make([]byte, len(data))
		if err := readFrozenChunk(ctx, dir, chunk, got); err != nil {
			return err
		}
		if !bytes.Equal(got, data) {
			return domain.ErrFrozenCaptureInput
		}
		return nil
	}
	if err := verify(); err == nil {
		return syncFrozenDir(dir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tmp := ".write-" + rand.Text()
	f, err := dir.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer dir.Remove(tmp)
	writeErr := f.Chmod(0600)
	if writeErr == nil {
		_, writeErr = f.Write(data)
	}
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Link installs without overwrite. Equivalent concurrent builders may win;
	// existing corrupt bytes are never repaired or trusted.
	if err := dir.Link(tmp, frozenChunkName(chunk.Hash)); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := syncFrozenDir(dir); err != nil {
		return err
	}
	return verify()
}

func readFrozenInput(ctx context.Context, root string, in domain.FrozenCaptureInput) ([]byte, error) {
	if in.Size > int64(int(^uint(0)>>1)) {
		return nil, domain.ErrFrozenCaptureInput
	}
	dir, err := openFrozenDir(root, false)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	raw := make([]byte, int(in.Size))
	offset := int64(0)
	h := sha256.New()
	for _, chunk := range in.Chunks {
		part := raw[offset : offset+chunk.Size]
		if err := readFrozenChunk(ctx, dir, chunk, part); err != nil {
			return nil, err
		}
		h.Write(part)
		offset += chunk.Size
	}
	if domain.ContentHash("sha256:"+hex.EncodeToString(h.Sum(nil))) != in.Hash {
		return nil, domain.ErrFrozenCaptureInput
	}
	return raw, ctx.Err()
}

type frozenCaptureSource struct {
	outbound.CaptureSource
	raw   []byte
	guard func(context.Context) error
}

func (s frozenCaptureSource) ReadSession(ctx context.Context, _ string) ([]byte, error) {
	if err := s.guard(ctx); err != nil {
		return nil, err
	}
	return s.raw, nil
}

// Gate writes as well as the final return: in particular the legacy full-decode
// Project path predates masking-policy checks around its PutDoc call.
type frozenProjectionStore struct {
	outbound.SessionStore
	guard func(context.Context) error
}

func (s *frozenProjectionStore) PutDoc(ctx context.Context, doc domain.SessionDoc) (domain.ContentHash, error) {
	if err := s.guard(ctx); err != nil {
		return "", err
	}
	return s.SessionStore.PutDoc(ctx, doc)
}

type frozenRootProjectionStore struct {
	*frozenProjectionStore
	rootCaptureStore
}

var _ rootCaptureStore = (*frozenRootProjectionStore)(nil)

func (s *frozenRootProjectionStore) WithObjectsRetained(ctx context.Context, fn func() error) error {
	if err := s.guard(ctx); err != nil {
		return err
	}
	return s.rootCaptureStore.WithObjectsRetained(ctx, fn)
}

func (s *frozenRootProjectionStore) PutChunk(ctx context.Context, hash domain.ContentHash, data []byte) error {
	if err := s.guard(ctx); err != nil {
		return err
	}
	return s.rootCaptureStore.PutChunk(ctx, hash, data)
}

func (s *frozenRootProjectionStore) PutConversationManifest(ctx context.Context, representation domain.DocumentRepresentation) error {
	if err := s.guard(ctx); err != nil {
		return err
	}
	return s.rootCaptureStore.PutConversationManifest(ctx, representation)
}
