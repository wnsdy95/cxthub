package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"unicode/utf8"
)

const conversationCheckBytes = 64 << 10

// VerifyConversationManifest reads and checks EVERY current chunk occurrence,
// including repeats, then validates complete canonical events across seams.
// Success describes only these supplied bytes. No certificate, cache trust,
// store ownership, snapshot identity or publication authority is returned.
//
// The loader may reuse its slice on its next call. It must honor ctx and must
// not concurrently mutate a returned slice while this call reads it. Metadata
// is snapshotted before loader callbacks, and a bounded chunk copy is retained.
func VerifyConversationManifest(ctx context.Context, want ContentHash, manifest ConversationManifest, load func(context.Context, ContentHash) ([]byte, error)) error {
	return (*StoredDocumentVerifier)(nil).VerifyConversation(ctx, want, manifest, load)
}

// VerifyConversation preserves the stateless verifier's complete current-byte
// checks, reusing only exact canonical event semantics across successful roots
// and legacy documents. A nil receiver remains stateless. Proofs are bounded and
// admitted only after the entire root succeeds; root reads never alter the legacy
// first-event hint or establish ownership, authorization or durable receipts.
func (v *StoredDocumentVerifier) VerifyConversation(ctx context.Context, want ContentHash, manifest ConversationManifest, load func(context.Context, ContentHash) ([]byte, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(manifest.Envelope) > MaxConversationManifestBytes || len(manifest.Chunks) > MaxConversationManifestChunks {
		return conversationManifestError("metadata limit")
	}
	manifest.Envelope = bytes.Clone(manifest.Envelope)
	if manifest.Chunks != nil {
		manifest.Chunks = append([]ConversationManifestChunk{}, manifest.Chunks...)
	}
	hash, err := ConversationManifestHash(manifest)
	if err != nil {
		return err
	}
	if ValidateContentHash(want) != nil || hash != want || (load == nil && len(manifest.Chunks) != 0) {
		return conversationManifestError("expected root or loader")
	}
	version, _ := conversationManifestEnvelope(manifest.Envelope) // validated above
	scanner := conversationEventScanner{version: version, limit: manifest.EventCount, verifier: v}
	for _, chunk := range manifest.Chunks {
		if err := ctx.Err(); err != nil {
			return err
		}
		body, err := load(ctx, chunk.Hash)
		if err != nil {
			return fmt.Errorf("conversation chunk load: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if int64(len(body)) != chunk.Bytes {
			return conversationManifestError("actual chunk length")
		}
		body = bytes.Clone(body)
		h := sha256.New()
		for offset := 0; offset < len(body); offset += conversationCheckBytes {
			if err := ctx.Err(); err != nil {
				return err
			}
			h.Write(body[offset:min(offset+conversationCheckBytes, len(body))])
		}
		if ContentHash("sha256:"+hex.EncodeToString(h.Sum(nil))) != chunk.Hash {
			return conversationManifestError("actual chunk hash")
		}
		if err := scanner.add(ctx, body); err != nil {
			return err
		}
	}
	if len(scanner.stack) != 0 || scanner.quoted || scanner.afterComma || scanner.count != manifest.EventCount {
		return conversationManifestError("incomplete framing or actual event count")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if v != nil {
		return v.remember(ctx, scanner.pending, nil)
	}
	return nil
}

// This is the same compact event framing discipline as the strict canonical
// verifier, kept local so the independent CLI needs no backend dependency.
// Two outer JSON levels are reserved. A UTF-8 rune or escape can cross chunks.
type conversationEventScanner struct {
	version                    string
	stack                      []byte
	event                      []byte
	quoted, escaped, needComma bool
	afterComma, havePrevious   bool
	previous                   int
	count, limit               int64
	verifier                   *StoredDocumentVerifier
	pending                    []storedEventProof
}

func (s *conversationEventScanner) verifyEvent(ctx context.Context) (int, error) {
	if s.verifier == nil {
		return conversationManifestEvent(ctx, s.version, s.event)
	}
	key := storedEventKey{sha256.Sum256(s.event), s.version}
	if seq, found := s.verifier.lookup(key); found {
		return seq, nil
	}
	seq, err := conversationManifestEvent(ctx, s.version, s.event)
	if err == nil && len(s.pending) < storedEventProofLimit {
		s.pending = append(s.pending, storedEventProof{key, seq})
	}
	return seq, err
}

func (s *conversationEventScanner) add(ctx context.Context, body []byte) error {
	for i, ch := range body {
		if i%conversationCheckBytes == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if len(s.stack) == 0 {
			if s.needComma {
				if ch != ',' {
					return conversationManifestError("missing event comma")
				}
				s.needComma, s.afterComma = false, true
				continue
			}
			if ch != '{' || s.count >= s.limit {
				return conversationManifestError("event shape or excess count")
			}
			s.stack = append(s.stack, ch)
			s.event = append(s.event[:0], ch)
			s.afterComma = false
			continue
		}
		s.event = append(s.event, ch)
		if s.quoted {
			if s.escaped {
				s.escaped = false
			} else if ch == '\\' {
				s.escaped = true
			} else if ch == '"' {
				s.quoted = false
			}
			continue
		}
		switch ch {
		case '"':
			s.quoted = true
		case '{', '[':
			if len(s.stack) == 9998 {
				return conversationManifestError("document JSON depth")
			}
			s.stack = append(s.stack, ch)
		case '}', ']':
			open := s.stack[len(s.stack)-1]
			if (ch == '}' && open != '{') || (ch == ']' && open != '[') {
				return conversationManifestError("mismatched event framing")
			}
			s.stack = s.stack[:len(s.stack)-1]
			if len(s.stack) == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
				if !utf8.Valid(s.event) {
					return conversationManifestError("event UTF-8")
				}
				seq, err := s.verifyEvent(ctx)
				if err != nil {
					return fmt.Errorf("%w: event canonical validation: %w", ErrConversationManifest, err)
				}
				if s.havePrevious && seq < s.previous {
					return conversationManifestError("event sequence regression")
				}
				s.previous, s.havePrevious = seq, true
				s.count++
				s.needComma = true
			}
		}
	}
	return ctx.Err()
}
