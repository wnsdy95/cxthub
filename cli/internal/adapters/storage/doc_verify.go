package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func decodeStoredSessionDoc(ctx context.Context, hash domain.ContentHash, data []byte, chunked bool) (domain.SessionDoc, error) {
	if !chunked {
		if err := ctx.Err(); err != nil {
			return domain.SessionDoc{}, err
		}
	}
	var cir domain.CIRDocument
	if err := json.Unmarshal(data, &cir); err != nil {
		return domain.SessionDoc{}, domain.ErrInvalidCIR
	}
	if err := ctx.Err(); err != nil {
		return domain.SessionDoc{}, err
	}
	doc := domain.SessionDoc{Hash: hash, CIR: cir}
	if !chunked {
		if err := domain.ValidateSessionDocHash(doc); err != nil {
			return domain.SessionDoc{}, err
		}
	}
	return doc, nil
}

// VerifyDoc performs GetDoc's validation without retaining the complete v2 CIR.
// It re-reads current chunks and hashes the exact reconstructed bytes. No proof
// or payload is cached across calls. Legacy representations use the same decoder
// and canonical hash verification as GetDoc.
func (s *FileStore) VerifyDoc(ctx context.Context, hash domain.ContentHash) error {
	return s.verifyDoc(ctx, hash, nil)
}

// reuse belongs to one sequential inspection. It never substitutes for reading
// current bytes, their complete hash, the envelope, or global JSON validation.
func (s *FileStore) verifyDoc(ctx context.Context, hash domain.ContentHash, reuse *inspectionEventReuse) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := domain.ValidateContentHash(hash); err != nil {
		return err
	}
	raw, err := readCxtFile(s.objectPath("docs", hash))
	if err != nil {
		if os.IsNotExist(err) {
			return domain.ErrNotFound
		}
		return err
	}
	data, err := docDecompress(raw)
	if err != nil {
		return domain.ErrInvalidCIR
	}
	manifest, chunked := chunkcas.ParseManifest(data)
	if !chunked {
		_, err = decodeStoredSessionDoc(ctx, hash, data, false)
		return err
	}
	if manifest.Format != chunkcas.FormatV2 {
		body, _, err := s.getDocChunkedObserved(ctx, hash, data, nil)
		if err != nil {
			return err
		}
		_, err = decodeStoredSessionDoc(ctx, hash, body, true)
		return err
	}
	proofs := reuse.forDocument(manifest.Chunks)
	chunks := &verificationChunkReader{ctx: ctx, store: s, doc: hash, hashes: manifest.Chunks}
	reader := io.MultiReader(
		strings.NewReader(`{"envelope":`), bytes.NewReader(manifest.Envelope),
		strings.NewReader(`,"events":[`), chunks, strings.NewReader(`]}`),
	)
	// Per-event Decode starts a fresh JSON scanner. Track the enclosing JSON
	// depth too, so inspection keeps encoding/json's whole-document limit.
	depth := &cirDepthReader{input: reader}
	digest := sha256.New()
	checked := io.TeeReader(depth, digest)
	decodeErr := verifyCIRStreamWithProofs(ctx, checked, proofs)
	// GetDoc checks byte identity before typed decoding. Finish hashing even if
	// typed decoding fails, preserving corruption/missing-chunk precedence.
	_, drainErr := io.Copy(io.Discard, checked)
	if chunks.err != nil {
		return chunks.err
	}
	if drainErr != nil {
		return drainErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if domain.ContentHash("sha256:"+hex.EncodeToString(digest.Sum(nil))) != hash {
		return domain.ErrHashMismatch
	}
	if depth.exceeded {
		return domain.ErrInvalidCIR
	}
	if decodeErr == nil {
		reuse.observe(manifest.Chunks)
	}
	return decodeErr
}

// encoding/json rejects more than 10,000 enclosing objects/arrays. The typed
// decoder still validates syntax; this observer only preserves its global
// depth limit without retaining values or interrupting byte hash validation.
type cirDepthReader struct {
	input    io.Reader
	depth    int
	quoted   bool
	escaped  bool
	exceeded bool
}

func (r *cirDepthReader) Read(p []byte) (int, error) {
	n, err := r.input.Read(p)
	for _, b := range p[:n] {
		if r.quoted {
			if r.escaped {
				r.escaped = false
			} else if b == '\\' {
				r.escaped = true
			} else if b == '"' {
				r.quoted = false
			}
			continue
		}
		switch b {
		case '"':
			r.quoted = true
		case '{', '[':
			r.depth++
			if r.depth > 10000 {
				r.exceeded = true
			}
		case '}', ']':
			r.depth--
		}
	}
	return n, err
}

type verificationChunkReader struct {
	ctx     context.Context
	store   *FileStore
	doc     domain.ContentHash
	hashes  []domain.ContentHash
	next    int
	current *bytes.Reader
	err     error
}

func (r *verificationChunkReader) Read(p []byte) (int, error) {
	for {
		if r.err != nil {
			return 0, r.err
		}
		if err := r.ctx.Err(); err != nil {
			r.err = err
			return 0, err
		}
		if r.current != nil {
			n, err := r.current.Read(p)
			if err != io.EOF {
				return n, err
			}
			r.current = nil
		}
		if r.next == len(r.hashes) {
			return 0, io.EOF
		}
		hash := r.hashes[r.next]
		r.next++
		if domain.ValidateContentHash(hash) != nil {
			r.err = domain.ErrInvalidCIR
			return 0, r.err
		}
		raw, err := readCxtFile(r.store.objectPath("chunks", hash))
		if err != nil {
			r.err = fmt.Errorf("%w: doc %s missing chunk %s", domain.ErrNotFound, r.doc, hash)
			return 0, r.err
		}
		body, err := docDecompress(raw)
		if err != nil {
			r.err = domain.ErrInvalidCIR
			return 0, r.err
		}
		r.current = bytes.NewReader(body)
	}
}

func verifyCIRStream(ctx context.Context, input io.Reader) error {
	return verifyCIRStreamWithProofs(ctx, input, nil)
}

func verifyCIRStreamWithProofs(ctx context.Context, input io.Reader, proofs *eventProofDecoder) error {
	decoder := json.NewDecoder(input)
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return domain.ErrInvalidCIR
	}
	for decoder.More() {
		if err := ctx.Err(); err != nil {
			return err
		}
		key, err := decoder.Token()
		if err != nil {
			return domain.ErrInvalidCIR
		}
		name, ok := key.(string)
		if !ok {
			return domain.ErrInvalidCIR
		}
		switch {
		case strings.EqualFold(name, "envelope"):
			var envelope domain.Envelope
			if decoder.Decode(&envelope) != nil {
				return domain.ErrInvalidCIR
			}
		case strings.EqualFold(name, "events"):
			start, err := decoder.Token()
			if err != nil {
				return domain.ErrInvalidCIR
			}
			if start == nil {
				continue
			}
			if start != json.Delim('[') {
				return domain.ErrInvalidCIR
			}
			for decoder.More() {
				if err := ctx.Err(); err != nil {
					return err
				}
				if proofs.decode(decoder) != nil {
					return domain.ErrInvalidCIR
				}
			}
			if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
				return domain.ErrInvalidCIR
			}
		default:
			var ignored json.RawMessage
			if decoder.Decode(&ignored) != nil {
				return domain.ErrInvalidCIR
			}
		}
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return domain.ErrInvalidCIR
	}
	var extra json.RawMessage
	if decoder.Decode(&extra) != io.EOF {
		return domain.ErrInvalidCIR
	}
	return ctx.Err()
}
