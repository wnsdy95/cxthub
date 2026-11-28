package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"sort"
	"strings"
)

const (
	canonicalDocPrefix  = `{"envelope":`
	canonicalDocMiddle  = `,"events":[`
	canonicalDocSuffix  = `]}`
	canonicalFrameBytes = len(canonicalDocPrefix) + len(canonicalDocMiddle) + len(canonicalDocSuffix)
	canonicalCheckBytes = 64 << 10
)

// All strings and slices here are private and immutable after verification.
// V2 parts are normalized to ChunkTarget; a legacy value borrows its own string.
type canonicalSegments struct {
	parts   []string
	offsets []int
	size    int
}

type canonicalSpan struct{ offset, length int }

// Verification retains only source coordinates and the checked identity/order.
// Search text, role and document-relative index belong to the read projection.
type verifiedCanonicalEvent struct {
	span canonicalSpan
	hash ContentHash
	seq  int
}

type verifiedDocChunks struct {
	envelope string
	stream   canonicalSegments
	hashes   []ContentHash
	events   []verifiedCanonicalEvent
}

func (s *canonicalSegments) add(part string) {
	if len(part) == 0 {
		return
	}
	s.offsets = append(s.offsets, s.size)
	s.parts = append(s.parts, part)
	s.size += len(part)
}

func (s canonicalSegments) ranges(span canonicalSpan, visit func(string)) {
	if span.length == 0 {
		return
	}
	i := sort.Search(len(s.offsets), func(i int) bool { return s.offsets[i] > span.offset }) - 1
	for span.length > 0 {
		start := span.offset - s.offsets[i]
		n := min(span.length, len(s.parts[i])-start)
		visit(s.parts[i][start : start+n])
		span.offset += n
		span.length -= n
		i++
	}
}

func (s canonicalSegments) appendRange(dst []byte, span canonicalSpan) []byte {
	if cap(dst)-len(dst) < span.length {
		next := make([]byte, len(dst), len(dst)+span.length)
		copy(next, dst)
		dst = next
	}
	s.ranges(span, func(part string) { dst = append(dst, part...) })
	return dst
}

// A reusable small buffer avoids a []byte conversion of a whole event/string
// when hashing through hash.Hash. Even a cached huge event stays segmented.
type canonicalSpanHasher struct {
	hash hash.Hash
	work []byte
}

func newCanonicalSpanHasher() canonicalSpanHasher {
	return canonicalSpanHasher{sha256.New(), make([]byte, canonicalCheckBytes)}
}

func (h canonicalSpanHasher) sum(ctx context.Context, s canonicalSegments, span canonicalSpan) (ContentHash, error) {
	h.hash.Reset()
	var err error
	s.ranges(span, func(part string) {
		for len(part) > 0 && err == nil {
			if err = ctx.Err(); err != nil {
				return
			}
			n := copy(h.work, part)
			h.hash.Write(h.work[:n])
			part = part[n:]
		}
	})
	if err != nil {
		return "", err
	}
	return ContentHash("sha256:" + hex.EncodeToString(h.hash.Sum(nil))), nil
}

func (d VerifiedSessionDoc) segmented() (string, canonicalSegments, error) {
	if d.chunks != nil {
		return d.chunks.envelope, d.chunks.stream, nil
	}
	// The string is already verified. Decode only the envelope to find the
	// events-array interior; do not copy the document into a RawMessage array.
	if !strings.HasPrefix(d.canonical, canonicalDocPrefix) || !strings.HasSuffix(d.canonical, canonicalDocSuffix) {
		return "", canonicalSegments{}, ErrIntegrity
	}
	decoder := json.NewDecoder(strings.NewReader(d.canonical[len(canonicalDocPrefix):]))
	var env json.RawMessage
	if err := decoder.Decode(&env); err != nil {
		return "", canonicalSegments{}, err
	}
	end := len(canonicalDocPrefix) + int(decoder.InputOffset())
	if !strings.HasPrefix(d.canonical[end:], canonicalDocMiddle) {
		return "", canonicalSegments{}, ErrIntegrity
	}
	var stream canonicalSegments
	stream.add(d.canonical[end+len(canonicalDocMiddle) : len(d.canonical)-len(canonicalDocSuffix)])
	return string(env), stream, nil
}

// visit scans compact array framing across arbitrary byte boundaries. Each
// completed object still needs exact typed canonical verification (or an exact
// cached proof). That check rejects malformed JSON, duplicate keys, unknown
// fields, invalid numbers and escapes inside an object. Framing must separately
// reject missing/extra commas and whitespace, even when all events are cached.
func (s canonicalSegments) visit(ctx context.Context, visit func(canonicalSpan) error) error {
	return visitCanonicalParts(ctx, s.parts, visit)
}

// Both raw and chunk-backed verification reserve the root object AND the
// events array in encoding/json's 10000-level budget. Validating the array in
// isolation previously admitted depth-10001 documents that GetDoc cannot read.
func visitCanonicalParts[T ~string | ~[]byte](ctx context.Context, parts []T, visit func(canonicalSpan) error) error {
	var stack [9998]byte
	depth, start, offset := 0, 0, 0
	quoted, escaped, needComma, afterComma := false, false, false, false
	for _, part := range parts {
		for i := 0; i < len(part); i++ {
			if offset%canonicalCheckBytes == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			ch := part[i]
			if quoted {
				if escaped {
					escaped = false
				} else if ch == '\\' {
					escaped = true
				} else if ch == '"' {
					quoted = false
				}
			} else if depth == 0 {
				if needComma {
					if ch != ',' {
						return ErrIntegrity
					}
					needComma, afterComma = false, true
				} else {
					if ch != '{' {
						return ErrIntegrity
					}
					start, depth, stack[0], afterComma = offset, 1, '{', false
				}
			} else {
				switch ch {
				case '"':
					quoted = true
				case '{', '[':
					if depth == len(stack) {
						return ErrIntegrity
					}
					stack[depth] = ch
					depth++
				case '}', ']':
					if (ch == '}' && stack[depth-1] != '{') || (ch == ']' && stack[depth-1] != '[') {
						return ErrIntegrity
					}
					depth--
					if depth == 0 {
						if err := visit(canonicalSpan{start, offset - start + 1}); err != nil {
							return err
						}
						needComma = true
					}
				case ' ', '\n', '\r', '\t':
					return ErrIntegrity
				}
			}
			offset++
		}
	}
	if depth != 0 || quoted || afterComma {
		return ErrIntegrity
	}
	return ctx.Err()
}
