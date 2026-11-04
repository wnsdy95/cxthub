//go:build postgres

package store

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestConversationChunkStoredAndDecodedBounds(test *testing.T) {
	body := bytes.Repeat([]byte{'a'}, 97)
	for _, mode := range []string{"raw", "compressed", "oversized stored", "expanded compressed", "inconsistent stored length"} {
		test.Run(mode, func(test *testing.T) {
			storage, ctx, transaction, repo := legacyRangeTestContext(test)
			limit := 2*len(body) + 64
			stored := body
			if mode == "compressed" {
				stored = docCompress(body)
			}
			if mode == "expanded compressed" {
				stored = docCompress(bytes.Repeat([]byte{'a'}, 1<<20))
				if len(stored) > limit {
					test.Fatal("fixture does not reach bounded decompression", len(stored), limit)
				}
			}
			queries := 0
			transaction.row = func(statement string, arguments []any) pgx.Row {
				queries++
				if !strings.Contains(statement, "CASE WHEN octet_length(b.bytes)<=$4 THEN substring(b.bytes FROM 1 FOR $4) END") || !strings.Contains(statement, "WHERE rb.repo_id=$1 AND rb.kind=$2 AND rb.hash=$3") || arguments[0] != string(repo) || arguments[1] != "chunk" || arguments[3] != limit {
					test.Fatal("selected chunk lost scoped pre-transfer bounds", statement, arguments)
				}
				if mode == "oversized stored" {
					return legacyRangeTestRow{values: []any{limit + 1, []byte(nil)}}
				}
				if mode == "inconsistent stored length" {
					return legacyRangeTestRow{values: []any{len(stored) + 1, stored}}
				}
				return legacyRangeTestRow{values: []any{len(stored), stored}}
			}
			current, err := storage.ReadConversationChunk(ctx, repo, domain.HashContent(body), int64(len(body)))
			if mode == "raw" || mode == "compressed" {
				if err != nil || !bytes.Equal(current, body) {
					test.Fatal("bounded reader changed valid selected bytes", current, err)
				}
			} else {
				want := domain.ErrIntegrity
				if mode == "oversized stored" {
					want = domain.ErrDocumentResourceLimit
				}
				if !errors.Is(err, want) || current != nil {
					test.Fatal("oversized selected chunk escaped its bound", len(current), err)
				}
			}
			if queries != 1 {
				test.Fatal("selected reader used an unbounded fallback", queries)
			}
		})
	}
	storage, ctx, _, repo := legacyRangeTestContext(test)
	for _, length := range []int64{0, -1, domain.ConversationManifestChunkBytes + 1} {
		if raw, err := storage.ReadConversationChunk(ctx, repo, domain.HashContent(body), length); !errors.Is(err, domain.ErrIntegrity) || raw != nil {
			test.Fatal("invalid selected length reached storage", length, err)
		}
	}
}

func TestConversationChunkStoredAndDecodedBoundsCoverEncoder(test *testing.T) {
	for length := 1; length <= domain.ConversationManifestChunkBytes; length++ {
		if zstdEnc.MaxEncodedSize(length) > 2*length+64 {
			test.Fatal("stored transfer bound rejects a supported encoder output", length, zstdEnc.MaxEncodedSize(length))
		}
	}
}
