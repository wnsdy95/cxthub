//go:build postgres

package app

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/klauspost/compress/zstd"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestP9PGRootWorkerStoredDecoderBoundary(t *testing.T) {
	for _, kind := range []string{"malformed", "oversized", "hostile-window", "truncated-tail"} {
		t.Run(kind, func(t *testing.T) {
			svc, st, repo := collaborationPG(t)
			ctx, cancel := context.WithTimeout(systemTestContext(), 20*time.Second)
			defer cancel()
			pool, err := pgxpool.New(ctx, collaborationDSN(t))
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			rep, _ := rootWorkerFixture(t)
			m, err := rep.ConversationManifest()
			if err != nil {
				t.Fatal(err)
			}
			// Unique current dependencies in the persistent disposable database.
			var cir domain.CIRDocument
			cir.Envelope = domain.CIREnvelope{CIRVersion: "1", SessionOriginID: time.Now().String()}
			cir.Events = []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: time.Now().String()}}}}
			m, bodies, err := domain.ConversationManifestForCIR(cir)
			if err != nil {
				t.Fatal(err)
			}
			rep.Hash, err = domain.ConversationManifestHash(m)
			if err != nil {
				t.Fatal(err)
			}
			rep.RootManifest, err = domain.CanonicalConversationManifest(m)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = st.PutChunks(ctx, repo, bodies); err != nil {
				t.Fatal(err)
			}
			job, err := domain.NewDocFinalizationJobForRepresentation(repo, rep, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = st.EnqueueDocJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			if _, err = svc.verifyDocJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			chunk := m.Chunks[0]
			raw := []byte{0x28, 0xb5, 0x2f, 0xfd}
			switch kind {
			case "oversized":
				enc, _ := zstd.NewWriter(nil)
				raw = enc.EncodeAll(bytes.Repeat([]byte("x"), 4<<20), nil)
				enc.Close()
			case "hostile-window":
				body := bodies[chunk.Hash]
				header := len(body)<<3 | 1
				raw = append([]byte{0x28, 0xb5, 0x2f, 0xfd, 0, byte((29 - 10) << 3), byte(header), byte(header >> 8), byte(header >> 16)}, body...)
			case "truncated-tail":
				enc, _ := zstd.NewWriter(nil)
				raw = enc.EncodeAll(bodies[chunk.Hash], nil)
				enc.Close()
				raw = raw[:len(raw)-1]
			}
			if _, err = pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, string(chunk.Hash), raw); err != nil {
				t.Fatal(err)
			}
			loaded, err := st.ReadConversationChunk(ctx, repo, chunk.Hash, chunk.Bytes)
			if !errors.Is(err, domain.ErrIntegrity) || len(loaded) != 0 {
				t.Fatal("bounded decoder", len(loaded), err)
			}
			claimed, err := st.ClaimDocJob(ctx, repo, time.Now(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if err = svc.runDocJob(ctx, st, claimed); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatal(err)
			}
			got, err := st.GetDocJob(ctx, repo, job.ID)
			if err != nil || got.State != "rejected" || got.Reason != "invalid_document" {
				t.Fatal(got, err)
			}
			if have, err := st.HasDocs(ctx, repo, []domain.ContentHash{rep.Hash}); err != nil || len(have) != 0 {
				t.Fatal(have, err)
			}
			t.Logf("%s: decoded=%d state=%s reason=%s", kind, len(loaded), got.State, got.Reason)
		})
	}
}
