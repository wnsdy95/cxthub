package storage

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Bump this when canonical/CIR validation semantics change. Receipts are local
// optimization hints, never replicated evidence or a replacement for fsck.
const docVerificationVersion = 1
const maxDocReceiptBytes = 4 << 20

type docVerifiedFile struct {
	Kind   string             `json:"kind"`
	ID     domain.ContentHash `json:"id"`
	Stored domain.ContentHash `json:"stored"`
}

type docVerificationProof struct {
	Version int                `json:"version"`
	Doc     domain.ContentHash `json:"doc"`
	Files   []docVerifiedFile  `json:"files"`
}

type docVerificationReceipt struct {
	Proof docVerificationProof `json:"proof"`
	MAC   string               `json:"mac"`
}

// EnableDocVerificationCache configures optional persistent validation progress.
// The caller supplies a private user-cache key path OUTSIDE the repository.
// Configuration itself does no IO; unavailable keys/receipts degrade to full
// verification. Set this before sharing the store between goroutines.
func (s *FileStore) EnableDocVerificationCache(keyPath string) {
	s.docProofKeyPath = keyPath
}

func (s *FileStore) docReceiptPath(id domain.ContentHash) string {
	return filepath.Join(s.storeDir(), "doc-verification", hexOf(id)+".json")
}

// VerifyStoredDoc avoids decoding historical transcripts on a warm pull. Every
// file named by an authenticated proof is hashed from disk on EVERY invocation;
// timestamps, sizes, existence, and untrusted .cxt flags never prove integrity.
func (s *FileStore) VerifyStoredDoc(ctx context.Context, id domain.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := domain.ValidateContentHash(id); err != nil {
		return err
	}
	key, _ := s.docVerificationKey() // caching is optional, correctness is not
	if len(key) != 0 && s.matchesDocReceipt(ctx, id, key) {
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	proof := docVerificationProof{Version: docVerificationVersion, Doc: id}
	var observe func(string, domain.ContentHash, []byte)
	if len(key) != 0 {
		observe = func(kind string, hash domain.ContentHash, raw []byte) {
			proof.Files = append(proof.Files, docVerifiedFile{Kind: kind, ID: hash, Stored: domain.HashContent(raw)})
		}
	}
	// The receipt describes these exact consumed bytes, not a second read after
	// validation. Repacking or a concurrent file replacement invalidates reuse.
	data, _, err := s.readStoredDoc(ctx, id, observe)
	if err != nil {
		return err
	}
	var cir domain.CIRDocument
	if err := json.Unmarshal(data, &cir); err != nil {
		return domain.ErrInvalidCIR
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := domain.ValidateSessionDocHash(domain.SessionDoc{Hash: id, CIR: cir}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(key) != 0 {
		receipt := docVerificationReceipt{Proof: proof, MAC: signDocProof(proof, key)}
		if raw, err := json.Marshal(receipt); err == nil && len(raw) <= maxDocReceiptBytes {
			// A failed cache write cannot turn a valid document into a failed pull.
			_ = writeAtomic(s.docReceiptPath(id), raw)
		}
	}
	return ctx.Err()
}

func signDocProof(proof docVerificationProof, key []byte) string {
	raw, _ := json.Marshal(proof)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *FileStore) matchesDocReceipt(ctx context.Context, id domain.ContentHash, key []byte) bool {
	f, err := openDocVerificationFile(s.docReceiptPath(id))
	if err != nil {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxDocReceiptBytes+1))
	_ = f.Close()
	if err != nil || len(raw) > maxDocReceiptBytes {
		return false
	}
	var receipt docVerificationReceipt
	if json.Unmarshal(raw, &receipt) != nil || receipt.Proof.Version != docVerificationVersion || receipt.Proof.Doc != id {
		return false
	}
	mac, err := hex.DecodeString(receipt.MAC)
	if err != nil {
		return false
	}
	expect, _ := hex.DecodeString(signDocProof(receipt.Proof, key))
	if !hmac.Equal(mac, expect) || len(receipt.Proof.Files) == 0 {
		return false
	}
	buf := make([]byte, 64<<10)
	for i, file := range receipt.Proof.Files {
		if (i == 0 && (file.Kind != "docs" || file.ID != id)) || (i > 0 && file.Kind != "chunks") || domain.ValidateContentHash(file.ID) != nil {
			return false
		}
		got, err := hashDocVerificationFile(ctx, s.objectPath(file.Kind, file.ID), buf)
		if err != nil || got != file.Stored {
			return false
		}
	}
	return ctx.Err() == nil
}

func openDocVerificationFile(path string) (*os.File, error) {
	if err := validateCxtWritePath(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	current, currentErr := os.Lstat(path)
	if err != nil || currentErr != nil || !info.Mode().IsRegular() || !os.SameFile(info, current) || current.Mode()&os.ModeSymlink != 0 {
		_ = f.Close()
		return nil, domain.ErrHashMismatch
	}
	return f, nil
}

func hashDocVerificationFile(ctx context.Context, path string, buf []byte) (domain.ContentHash, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f, err := openDocVerificationFile(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := f.Read(buf)
		_, _ = hash.Write(buf[:n])
		if err == io.EOF {
			return domain.ContentHash("sha256:" + hex.EncodeToString(hash.Sum(nil))), nil
		}
		if err != nil {
			return "", err
		}
	}
}

// Create the key with a temp file + exclusive link so simultaneous CLI processes
// can only observe a complete key, and a losing creator never overwrites it.
func (s *FileStore) docVerificationKey() ([]byte, error) {
	path := s.docProofKeyPath
	if path == "" {
		return nil, nil
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	root, err := filepath.Abs(s.repoRoot)
	if err != nil {
		return nil, err
	}
	if rel, err := filepath.Rel(root, path); err != nil || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
		return nil, fmt.Errorf("document verification key must be outside the repository")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("document verification key directory is not private")
	}
	// Also reject a cache parent symlink that resolves back into the replica.
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	if rel, err := filepath.Rel(resolvedRoot, resolvedDir); err != nil || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
		return nil, fmt.Errorf("document verification key resolves inside the repository")
	}
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		tmp, err := os.CreateTemp(dir, ".key-")
		if err != nil {
			return nil, err
		}
		defer os.Remove(tmp.Name())
		_, err = tmp.Write(key)
		if err == nil {
			err = tmp.Sync()
		}
		closeErr := tmp.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if err := os.Link(tmp.Name(), path); err != nil && !os.IsExist(err) {
			return nil, err
		}
	}
	info, err = os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() != 32 {
		return nil, fmt.Errorf("document verification key is not a private 32-byte file")
	}
	key, err := os.ReadFile(path)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("cannot read document verification key")
	}
	return key, nil
}
