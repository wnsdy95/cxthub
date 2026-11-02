package domain

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestLegacyNormalizationHeaderBoundedAssemblyAndOwnership(test *testing.T) {
	envelope := CIREnvelope{CIRVersion: CIRVersionV2, SourceModel: strings.Repeat("<🙂\xff", 100000), SourceModels: []string{"one", "two"}}
	want, err := canonicalJSON(envelope)
	if err != nil {
		test.Fatal(err)
	}
	header := &legacyNormalizationHeader{encoding: legacyEnvelopeEncodingFixture(test, envelope)}
	assemblySteps, outputSteps := 0, 0
	for steps := 0; steps < 1024; steps++ {
		position, copying := header.position, header.copying
		ctx, cancel := context.WithCancel(context.Background())
		output, complete, err := header.step(ctx)
		cancel()
		if err != nil {
			test.Fatal(err)
		}
		if header.position-position > ConversationManifestChunkBytes/2 {
			test.Fatal("header copy exceeded the shared source/destination quantum", position, header.position)
		}
		if copying {
			outputSteps++
		} else if header.position > position {
			assemblySteps++
		}
		if retained, err := header.retained(); err != nil || retained <= 0 {
			test.Fatal("header lost retained accounting", retained, err)
		}
		if !complete {
			if output != nil {
				test.Fatal("header published partial bytes")
			}
			continue
		}
		if !bytes.Equal(output, want) || !bytes.Equal(header.header, want) || assemblySteps < 2 || outputSteps < 2 {
			test.Fatal("header assembly changed bytes or did not yield both copies", len(output), len(want), assemblySteps, outputSteps)
		}
		if header.first != nil || header.last != nil || header.encoding != nil || header.heap.bytes != 0 {
			test.Fatal("completed header retains staging chunks")
		}
		output[0] ^= 1
		if !bytes.Equal(header.header, want) {
			test.Fatal("caller output aliases the replay header")
		}
		return
	}
	test.Fatal("header assembly exhausted its progress limit")
}

func TestLegacyNormalizationHeaderPreCanceledCallPreservesProgress(test *testing.T) {
	header := &legacyNormalizationHeader{encoding: legacyEnvelopeEncodingFixture(test, CIREnvelope{CIRVersion: CIRVersionV2})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	output, complete, err := header.step(ctx)
	if !errors.Is(err, context.Canceled) || output != nil || complete || header.length != 0 || header.first != nil || header.encoding.phase != legacyEnvelopeEncodingStart {
		test.Fatal("pre-canceled header advanced or published state", complete, err)
	}
	output, complete, err = header.step(context.Background())
	want, wantErr := canonicalJSON(CIREnvelope{CIRVersion: CIRVersionV2})
	if err != nil || wantErr != nil || !complete || !bytes.Equal(output, want) {
		test.Fatal("fresh header context could not resume", complete, err, wantErr)
	}
}
