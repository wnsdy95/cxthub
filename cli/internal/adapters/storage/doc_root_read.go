package storage

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// GetDocReference is the explicit dual reader. The hash-only GetDoc remains a
// legacy boundary until its consumers also carry an expected identity.
func (s *FileStore) GetDocReference(ctx context.Context, ref domain.DocumentRef) (domain.SessionDoc, error) {
	if err := ctx.Err(); err != nil {
		return domain.SessionDoc{}, err
	}
	if err := ref.Validate(); err != nil {
		return domain.SessionDoc{}, err
	}
	var doc domain.SessionDoc
	err := s.WithObjectsRetained(ctx, func() error {
		var err error
		if ref.Identity == domain.DocumentIdentityLegacy {
			doc, err = s.GetDoc(ctx, ref.Hash)
		} else {
			doc, err = s.readRootDocument(ctx, ref, true)
		}
		if err == nil && doc.DocumentRef() != ref {
			return domain.ErrHashMismatch
		}
		return err
	})
	if err != nil {
		return domain.SessionDoc{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.SessionDoc{}, err
	}
	return doc, nil
}

// VerifyStoredDocReference never uses or creates a receipt for a root. Every
// current chunk occurrence is read and checked by the shared domain verifier.
func (s *FileStore) VerifyStoredDocReference(ctx context.Context, ref domain.DocumentRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	return s.WithObjectsRetained(ctx, func() error {
		if ref.Identity == domain.DocumentIdentityLegacy {
			return s.VerifyStoredDoc(ctx, ref.Hash)
		}
		_, err := s.readRootDocument(ctx, ref, false)
		return err
	})
}

// Inspection creates no store lock files or persistent cache writes and makes
// no atomic-scan claim. Root reads may use the in-memory semantic verifier and
// its mutex; they still join the snapshot's explicit scheme to current bytes.
func (s *FileStore) inspectDocReference(ctx context.Context, ref domain.DocumentRef, reuse *inspectionEventReuse) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if ref.Identity == domain.DocumentIdentityLegacy {
		return s.verifyDoc(ctx, ref.Hash, reuse)
	}
	_, err := s.readRootDocument(ctx, ref, false)
	return err
}

func (s *FileStore) readRootDocument(ctx context.Context, ref domain.DocumentRef, materialize bool) (domain.SessionDoc, error) {
	raw, err := readRootObject(ctx, s.objectPath("docs", ref.Hash), domain.MaxConversationManifestBytes)
	if err != nil {
		return domain.SessionDoc{}, err
	}
	manifest, err := domain.DecodeConversationManifest(raw)
	if err != nil {
		return domain.SessionDoc{}, err
	}
	return s.verifyRootDocument(ctx, ref, manifest, materialize)
}

func (s *FileStore) verifyRootDocument(ctx context.Context, ref domain.DocumentRef, manifest domain.ConversationManifest, materialize bool) (domain.SessionDoc, error) {
	var canonical bytes.Buffer
	if materialize {
		canonical.WriteString(`{"envelope":`)
		canonical.Write(manifest.Envelope)
		canonical.WriteString(`,"events":[`)
	}
	next := 0
	err := s.docVerifier.VerifyConversation(ctx, ref.Hash, manifest, func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
		// The domain verifier owns the ordered metadata and invokes this loader
		// once per occurrence. Do not deduplicate away a current-byte check.
		chunk := manifest.Chunks[next]
		next++
		body, err := readRootObject(ctx, s.objectPath("chunks", hash), int(chunk.Bytes))
		if err == nil && materialize {
			canonical.Write(body)
		}
		return body, err
	})
	if err != nil {
		return domain.SessionDoc{}, err
	}
	doc := domain.SessionDoc{Hash: ref.Hash, Identity: domain.DocumentIdentityRootV1}
	if materialize {
		canonical.WriteString(`]}`)
		if err := json.Unmarshal(canonical.Bytes(), &doc.CIR); err != nil {
			return domain.SessionDoc{}, err
		}
	}
	return doc, ctx.Err()
}

func readRootObject(ctx context.Context, path string, limit int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := openDocVerificationFile(path)
	if os.IsNotExist(err) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return boundedRootData(ctx, f, limit)
}

// Limit decoded output before allocation; a frame's advertised size must not
// drive DecodeAll. The window bound also covers hostile streaming frames.
func boundedRootData(ctx context.Context, input io.Reader, limit int) ([]byte, error) {
	reader, close, err := documentStream(ctx, input, true)
	if err != nil {
		return nil, err
	}
	defer close()
	body, err := io.ReadAll(io.LimitReader(contextDocReader{ctx, reader}, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > limit {
		return nil, fmt.Errorf("%w: decoded root object exceeds limit", domain.ErrConversationManifest)
	}
	return body, ctx.Err()
}

type contextDocReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextDocReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.r.Read(p)
	if canceled := r.ctx.Err(); canceled != nil {
		return n, canceled
	}
	return n, err
}

func documentStream(ctx context.Context, input io.Reader, root bool) (io.Reader, func(), error) {
	r := bufio.NewReaderSize(contextDocReader{ctx, input}, 64<<10)
	header, err := r.Peek(4)
	if err != nil && err != io.EOF {
		return nil, nil, err
	}
	if !bytes.Equal(header, []byte{0x28, 0xb5, 0x2f, 0xfd}) {
		return r, func() {}, nil
	}
	options := []zstd.DOption{zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true)}
	if root {
		options = append(options, zstd.WithDecoderMaxMemory(8<<20), zstd.WithDecoderMaxWindow(8<<20))
	}
	decoder, err := zstd.NewReader(r, options...)
	if err != nil {
		return nil, nil, err
	}
	return decoder, decoder.Close, nil
}

// Recognize root declarations only at the top level, including escaped and
// case-aliased keys. This is a discriminator, not a JSON validator: recognized
// input must still pass the strict canonical root codec. Scanning does not
// retain arbitrary legacy values or confuse prose containing root words.
func rootDocumentDeclaration(ctx context.Context, input io.Reader) (bool, error) {
	r := bufio.NewReaderSize(contextDocReader{ctx, input}, 64<<10)
	depth := 0
	quoted, escaped := false, false
	var key []byte
	var pending string
	for {
		ch, err := r.ReadByte()
		if err == io.EOF {
			return false, ctx.Err()
		}
		if err != nil {
			return false, err
		}
		if quoted {
			if depth == 1 && len(key) < 256 {
				key = append(key, ch)
			}
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				quoted = false
				if depth == 1 {
					_ = json.Unmarshal(key, &pending)
				}
			}
			continue
		}
		if ch == ' ' || ch == '\n' || ch == '\r' || ch == '\t' {
			continue
		}
		if ch == ':' && depth == 1 {
			for _, name := range []string{"identity", "root_manifest", "chunk_format", "stream_bytes", "event_count"} {
				if strings.EqualFold(pending, name) {
					return true, nil
				}
			}
		}
		pending = ""
		switch ch {
		case '"':
			quoted = true
			key = append(key[:0], ch)
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
	}
}

func storedRootDeclaration(ctx context.Context, input io.Reader) (bool, error) {
	r, close, err := documentStream(ctx, input, false)
	if err != nil {
		return false, err
	}
	defer close()
	return rootDocumentDeclaration(ctx, r)
}

func legacyDocumentData(ctx context.Context, raw []byte) ([]byte, error) {
	root, err := storedRootDeclaration(ctx, bytes.NewReader(raw))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, domain.ErrInvalidCIR
	}
	if root {
		return nil, domain.ErrUnsupportedDocumentIdentity
	}
	data, err := docDecompress(raw)
	if err != nil {
		return nil, domain.ErrInvalidCIR
	}
	return data, ctx.Err()
}

// The receipt's descriptor hash and identity fence observe the same open file.
// A separate probe before/after the authenticated read could admit an ABA swap.
func hashLegacyDocReceiptFile(ctx context.Context, path string, out io.Writer) (domain.ContentHash, error) {
	f, err := openDocVerificationFile(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	digest := sha256.New()
	var sink io.Writer = digest
	if out != nil {
		sink = io.MultiWriter(digest, out)
	}
	input := io.TeeReader(contextDocReader{ctx, f}, sink)
	root, err := storedRootDeclaration(ctx, input)
	if err != nil {
		return "", err
	}
	if root {
		return "", domain.ErrUnsupportedDocumentIdentity
	}
	if _, err := io.Copy(io.Discard, input); err != nil {
		return "", err
	}
	return domain.ContentHash("sha256:" + hex.EncodeToString(digest.Sum(nil))), ctx.Err()
}
