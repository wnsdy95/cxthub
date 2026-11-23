//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocNormalizationWorkOriginalOracleCandidates(test *testing.T) {
	selected := 0
	for _, fixture := range legacyMonolithicVerifierCases(test) {
		if fixture.hash == domain.HashContent(fixture.raw) && !fixture.canceled {
			continue
		}
		selected++
		test.Run(fixture.name, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if fixture.canceled {
				cancel()
			}
			source := domain.LegacyByteSource{Bytes: len(fixture.raw), Read: func(ctx context.Context, offset, length int) ([]byte, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if offset < 0 || length < 0 || offset > len(fixture.raw) || length > len(fixture.raw)-offset {
					return nil, domain.ErrIntegrity
				}
				return fixture.raw[offset : offset+length], nil
			}}
			for offset := 0; offset < len(fixture.raw); offset += domain.ConversationManifestChunkBytes {
				part := fixture.raw[offset:min(offset+domain.ConversationManifestChunkBytes, len(fixture.raw))]
				source.Parts = append(source.Parts, domain.ConversationManifestChunk{Hash: domain.HashContent(part), Bytes: int64(len(part))})
			}
			var verifier domain.CanonicalDocVerifier
			work, err := newLegacyNormalizationWork(ctx, fixture.hash, source, &verifier)
			if work != nil {
				defer func() {
					if closeErr := work.Close(); closeErr != nil {
						test.Error(closeErr)
					}
				}()
			}
			complete := false
			for quantum := 0; err == nil && !complete && quantum < docCaptureWorkPGPreparationStepLimit; quantum++ {
				complete, err = work.Step(ctx)
			}
			if !errors.Is(err, fixture.wantErr) || (err == nil) != fixture.expected.Valid() || !complete && err == nil {
				test.Fatal("normalized producer changed original acceptance or error", complete, err, fixture.wantErr)
			}
			if err != nil {
				return
			}
			proof, err := work.canonical.Result()
			if err != nil || !proof.Valid() {
				test.Fatal("normalized producer did not earn original authority", err)
			}
			plan, err := proof.Plan()
			if err != nil {
				test.Fatal(err)
			}
			want, err := domain.NewLegacyByteBackfill(ctx, fixture.expected)
			if err != nil {
				test.Fatal(err)
			}
			gotSpec, _ := plan.Spec()
			wantSpec, _ := want.Spec()
			if gotSpec != wantSpec {
				test.Fatal("normalized producer changed original plan", gotSpec, wantSpec)
			}
			for ordinal := range gotSpec.Blocks {
				got, err := plan.Block(ctx, ordinal)
				expected, wantErr := want.Block(ctx, ordinal)
				if err != nil || wantErr != nil || got.Descriptor() != expected.Descriptor() || !bytes.Equal(got.Bytes(), expected.Bytes()) {
					test.Fatal("normalized producer changed original event bytes", ordinal, err, wantErr)
				}
			}
		})
	}
	if selected < 10 {
		test.Fatal("original normalization candidate corpus was not exercised", selected)
	}
}
