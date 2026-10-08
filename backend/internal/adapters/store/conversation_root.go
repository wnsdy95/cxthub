package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Only top-level field names select the root family. In particular, a marker
// inside a transcript, tool input, or envelope is ordinary legacy content.
// Scan without retaining values: even a late declaration after a giant value
// must not enter the permissive legacy decoder or allocate a giant manifest.
type rootDeclarationScanner struct {
	depth, keySize                       int
	quoted, escape, key, expectKey, root bool
	name                                 []byte
}

func (s *rootDeclarationScanner) scan(b []byte) {
	for _, c := range b {
		if s.quoted {
			if s.key {
				s.keySize++
				if s.keySize <= 128 {
					s.name = append(s.name, c)
				}
			}
			if s.escape {
				s.escape = false
				continue
			}
			if c == '\\' {
				s.escape = true
				continue
			}
			if c != '"' {
				continue
			}
			s.quoted = false
			if s.key && s.keySize <= 128 {
				var name string
				if json.Unmarshal(s.name, &name) == nil {
					for _, field := range []string{"identity", "root_manifest", "chunk_format", "stream_bytes", "event_count"} {
						if strings.EqualFold(name, field) {
							s.root = true
							break
						}
					}
				}
			}
			s.key = false
			continue
		}
		switch c {
		case '"':
			s.quoted = true
			s.key = s.depth == 1 && s.expectKey
			s.expectKey = false
			s.keySize = 1
			s.name = append(s.name[:0], '"')
		case '{', '[':
			s.depth++
			if s.depth == 1 && c == '{' {
				s.expectKey = true
			}
		case '}', ']':
			s.depth--
		case ',':
			if s.depth == 1 {
				s.expectKey = true
			}
		case ':':
			s.expectKey = false
		}
	}
}

func docObjectStream(raw []byte) (io.Reader, func(), error) {
	if len(raw) < 4 || !bytes.Equal(raw[:4], []byte{0x28, 0xb5, 0x2f, 0xfd}) {
		return bytes.NewReader(raw), func() {}, nil
	}
	// Streaming avoids DecodeAll's unbounded output allocation. Disable its
	// small-compressed-input eager DecodeAll optimization as well.
	// Match the normal at-rest encoder's 8 MiB window. Output limits alone do
	// not constrain a hostile frame's advertised history allocation. Large
	// legacy whole bodies still stream through this window without a size cap.
	dec, err := zstd.NewReader(bytes.NewReader(raw), zstd.WithDecoderConcurrency(1), zstd.WithDecodeBuffersBelow(0),
		zstd.WithDecoderMaxWindow(8<<20), zstd.WithDecoderMaxMemory(8<<20))
	if err != nil {
		return nil, nil, err
	}
	return dec, dec.Close, nil
}

// A recognized root declaration is terminal even if strict decoding fails.
// Only the bounded prefix is retained while classifying a legacy large body.
func storedConversationManifest(ctx context.Context, raw []byte) (domain.ConversationManifest, bool, error) {
	var zero domain.ConversationManifest
	if err := ctx.Err(); err != nil {
		return zero, false, err
	}
	r, close, err := docObjectStream(raw)
	if err != nil {
		return zero, false, fmt.Errorf("%w: %v", domain.ErrIntegrity, err)
	}
	defer close()
	var scan rootDeclarationScanner
	var data []byte
	var total int64
	var buf [32 << 10]byte
	for {
		if err := ctx.Err(); err != nil {
			return zero, scan.root, err
		}
		n, err := r.Read(buf[:])
		if n > 0 {
			total += int64(n)
			scan.scan(buf[:n])
			if len(data) < domain.MaxConversationManifestBytes {
				data = append(data, buf[:min(n, domain.MaxConversationManifestBytes-len(data))]...)
			}
			if scan.root && total > domain.MaxConversationManifestBytes {
				return zero, true, fmt.Errorf("%w: root manifest exceeds byte limit", domain.ErrIntegrity)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return zero, scan.root, fmt.Errorf("%w: %v", domain.ErrIntegrity, err)
		}
	}
	if !scan.root {
		return zero, false, ctx.Err()
	}
	manifest, err := domain.DecodeConversationManifest(data)
	if err != nil {
		return zero, true, fmt.Errorf("%w: %w", domain.ErrIntegrity, err)
	}
	return manifest, true, ctx.Err()
}

func rejectStoredRoot(ctx context.Context, raw []byte) error {
	_, root, err := storedConversationManifest(ctx, raw)
	if err != nil {
		return err
	}
	if root {
		return domain.ErrUnsupportedDocumentIdentity
	}
	return nil
}

// Allocate at most the declared decoded size plus one byte, so an oversized
// compressed body fails before it can be retained as an owned stream part.
func rootChunkBytes(ctx context.Context, raw []byte, length int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if length <= 0 || length > domain.ConversationManifestChunkBytes {
		return nil, domain.ErrIntegrity
	}
	r, close, err := docObjectStream(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrIntegrity, err)
	}
	defer close()
	body := make([]byte, int(length)+1)
	n := 0
	for n < len(body) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		read, err := r.Read(body[n:min(n+(32<<10), len(body))])
		n += read
		if err == io.EOF {
			break
		}
		// A decoder's unexpected EOF is corrupt framing even when it emitted
		// exactly the claimed length. Only clean EOF terminates a valid frame.
		if err != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrIntegrity, err)
		}
		if read == 0 {
			return nil, fmt.Errorf("%w: stalled chunk decoder", domain.ErrIntegrity)
		}
	}
	if int64(n) != length {
		return nil, domain.ErrIntegrity
	}
	return body[:n:n], ctx.Err()
}

func verifyStoredConversation(ctx context.Context, hash domain.ContentHash, manifest domain.ConversationManifest, read storedChunkReader) (domain.VerifiedSessionDoc, error) {
	lengths := make(map[domain.ContentHash]int64, len(manifest.Chunks))
	for _, chunk := range manifest.Chunks {
		if old, seen := lengths[chunk.Hash]; seen && old != chunk.Bytes {
			return domain.VerifiedSessionDoc{}, domain.ErrIntegrity
		}
		lengths[chunk.Hash] = chunk.Bytes
	}
	var verifier domain.CanonicalDocVerifier
	return verifier.VerifyConversationManifestDoc(ctx, hash, manifest, func(ctx context.Context, h domain.ContentHash) ([]byte, error) {
		raw, err := read(ctx, h)
		if err != nil {
			return nil, err
		}
		return rootChunkBytes(ctx, raw, lengths[h])
	})
}

func verifyLegacyStoredDoc(ctx context.Context, hash domain.ContentHash, raw []byte, read storedChunkReader) (domain.VerifiedSessionDoc, error) {
	data, err := docDecompress(raw)
	if err != nil {
		return domain.VerifiedSessionDoc{}, domain.ErrIntegrity
	}
	if manifest, chunked := domain.ParseDocChunkManifest(data); chunked {
		var verifier domain.CanonicalDocVerifier
		return verifier.VerifyChunks(ctx, hash, manifest, func(ctx context.Context, h domain.ContentHash) ([]byte, error) {
			raw, err := read(ctx, h)
			if err != nil {
				return nil, err
			}
			body, err := docDecompress(raw)
			if err != nil {
				return nil, domain.ErrIntegrity
			}
			return body, ctx.Err()
		})
	}
	var cir domain.CIRDocument
	if err := json.Unmarshal(data, &cir); err != nil {
		return domain.VerifiedSessionDoc{}, domain.ErrIntegrity
	}
	doc, err := domain.VerifySessionDoc(domain.SessionDoc{Hash: hash, CIR: cir})
	if err != nil {
		return domain.VerifiedSessionDoc{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.VerifiedSessionDoc{}, err
	}
	return doc, nil
}
