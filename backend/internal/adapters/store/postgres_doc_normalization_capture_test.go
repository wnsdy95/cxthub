//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocNormalizationWorkRQ87OwnerClose(test *testing.T) {
	for _, mode := range []string{"explicit", "verified"} {
		test.Run(mode, func(test *testing.T) {
			ctx := context.Background()
			document := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: domain.CIRVersionV2}, Events: []domain.CIREvent{{Seq: 1, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "retained"}}}}}
			canonical, err := domain.CanonicalBytes(document)
			if err != nil {
				test.Fatal(err)
			}
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, canonical, "", "  "); err != nil {
				test.Fatal(err)
			}
			stored := pretty.Bytes()
			source := domain.LegacyByteSource{Bytes: len(stored), Parts: []domain.ConversationManifestChunk{{Hash: domain.HashContent(stored), Bytes: int64(len(stored))}}, Read: func(ctx context.Context, offset, length int) ([]byte, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if offset < 0 || length < 0 || offset > len(stored) {
					return nil, domain.ErrIntegrity
				}
				if length > len(stored)-offset {
					return bytes.Clone(stored[offset:]), io.EOF
				}
				return bytes.Clone(stored[offset : offset+length]), nil
			}}
			var verifier domain.CanonicalDocVerifier
			work, err := newLegacyNormalizationWork(ctx, domain.HashContent(canonical), source, &verifier)
			if err != nil {
				test.Fatal(err)
			}
			defer work.Close()
			preparation := work.preparation
			if mode == "verified" {
				complete := false
				for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit && !complete; quantum++ {
					complete, err = work.Step(ctx)
					if err != nil {
						test.Fatal(err)
					}
				}
				if !complete || !work.complete || work.preparation != nil {
					test.Fatal("normalization never completed its actual proof handoff")
				}
			} else if err := work.Close(); err != nil {
				test.Fatal(err)
			}
			if ready, err := preparation.Step(ctx); ready || !errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrLegacyNormalizationBusy) {
				test.Fatal("closed normalization owner still permits mutation", ready, err)
			}
			if err := work.Close(); err != nil {
				test.Fatal("normalization owner cleanup is not idempotent", err)
			}
		})
	}
}
