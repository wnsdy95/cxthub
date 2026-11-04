package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func legacySourceGeometryInput(raw []byte, partBytes int) (*legacySourceProofBuffer, LegacyByteSource) {
	input := &legacySourceProofBuffer{body: bytes.Clone(raw)}
	source := input.input()
	source.PartBytes = partBytes
	if partBytes == 64<<10 {
		source.Parts = nil
		for offset := 0; offset < len(raw); offset += 64 << 10 {
			part := input.body[offset:min(offset+64<<10, len(raw))]
			source.Parts = append(source.Parts, ConversationManifestChunk{Hash: HashContent(part), Bytes: int64(len(part))})
		}
	}
	return input, source
}

func legacySourceGeometryReads(test *testing.T, input *legacySourceProofBuffer, partBytes int) {
	test.Helper()
	if partBytes == 0 {
		partBytes = 512 << 10
	}
	seen := make([]bool, (len(input.body)+partBytes-1)/partBytes)
	var zeroEOF, oneEOF bool
	for _, read := range input.reads {
		if read[0] == len(input.body) && read[1] == 0 {
			zeroEOF = true
			continue
		}
		if read[0] == len(input.body) && read[1] == 1 {
			oneEOF = true
			continue
		}
		if read[0] < 0 || read[0] >= len(input.body) || read[0]%partBytes != 0 ||
			read[1] != min(partBytes, len(input.body)-read[0]) {
			test.Fatal("source read did not match the declared authenticated geometry", partBytes, read)
		}
		seen[read[0]/partBytes] = true
	}
	for ordinal, read := range seen {
		if !read {
			test.Fatal("normalization completed without authenticating a source part", partBytes, ordinal)
		}
	}
	if !zeroEOF || !oneEOF {
		test.Fatal("normalization omitted the original EOF probes", zeroEOF, oneEOF)
	}
}

func legacySourceGeometryReject(test *testing.T, input *legacySourceProofBuffer, source LegacyByteSource) {
	test.Helper()
	normalizer, err := NewLegacyNormalizationDecoder(context.Background(), source)
	if normalizer != nil || !errors.Is(err, ErrIntegrity) || len(input.reads) != 0 {
		test.Fatal("invalid geometry or manifest escaped constructor validation or caused source I/O", err, len(input.reads))
	}
}

func TestLegacySourceGeometryAcceptedDescriptorsAndReads(test *testing.T) {
	if LegacyNormalizationPartBytes != 64<<10 || ConversationManifestChunkBytes != 512<<10 {
		test.Fatal("normalization or original-proof part size changed", LegacyNormalizationPartBytes, ConversationManifestChunkBytes)
	}
	for _, partBytes := range []int{0, 512 << 10, 64 << 10} {
		for _, extent := range []int{0, 2, 64<<10 - 1, 64 << 10, 64<<10 + 1, 512<<10 - 1, 512 << 10, 512<<10 + 1, 2*(512<<10) + 37} {
			test.Run(fmt.Sprintf("part-%d/bytes-%d", partBytes, extent), func(test *testing.T) {
				raw := bytes.Repeat([]byte(" "), extent)
				copy(raw, "{}")
				input, source := legacySourceGeometryInput(raw, partBytes)
				normalizer, err := NewLegacyNormalizationDecoder(context.Background(), source)
				if err != nil || normalizer == nil || len(input.reads) != 0 {
					test.Fatal("valid geometry failed or eagerly read source bytes", err, len(input.reads))
				}
				if extent == 0 {
					return
				}
				result, err := legacyNormalizationFrameDrain(test, context.Background(), normalizer, input, false)
				want, wantErr := legacyNormalizationOracle(raw)
				if err != nil || wantErr != nil || !bytes.Equal(result.canonical, want) {
					test.Fatal("geometry changed the original typed normalization result", err, wantErr)
				}
				legacySourceGeometryReads(test, input, partBytes)
			})
		}
	}
}

func TestLegacySourceGeometryRejectsUnsupportedSizes(test *testing.T) {
	for _, extent := range []int{0, 2, 64<<10 + 1} {
		input, source := legacySourceGeometryInput(bytes.Repeat([]byte(" "), extent), 0)
		for _, partBytes := range []int{-1, -64 << 10, 1, 32 << 10, 64<<10 - 1, 64<<10 + 1, 128 << 10, 512<<10 - 1, 512<<10 + 1, 1 << 20, int(^uint(0) >> 1)} {
			test.Run(fmt.Sprintf("bytes-%d/part-%d", extent, partBytes), func(test *testing.T) {
				invalid := source
				invalid.PartBytes = partBytes
				legacySourceGeometryReject(test, input, invalid)
				var verifier CanonicalDocVerifier
				output := &legacySourceProofBuffer{}
				preparation, err := NewLegacyByteSourcePreparation(context.Background(), HashContent(input.body), invalid, output, &verifier)
				if preparation != nil || !errors.Is(err, ErrIntegrity) || len(input.reads) != 0 || len(output.body) != 0 {
					test.Fatal("canonical proof accepted unsupported geometry or touched source bytes", err)
				}
			})
		}
	}
}

func TestLegacySourceGeometryRejectsManifestMetadata(test *testing.T) {
	raw := []byte("{}" + strings.Repeat(" ", 2*(512<<10)+37))
	for _, partBytes := range []int{0, 512 << 10, 64 << 10} {
		test.Run(fmt.Sprintf("part-%d", partBytes), func(test *testing.T) {
			input, source := legacySourceGeometryInput(raw, partBytes)
			for _, fixture := range []struct {
				name   string
				mutate func(*LegacyByteSource)
			}{
				{name: "negative-extent", mutate: func(source *LegacyByteSource) { source.Bytes = -1 }},
				{name: "oversize-extent", mutate: func(source *LegacyByteSource) { source.Bytes = MaxFinalizedDocBytes + 1 }},
				{name: "overflow-extent", mutate: func(source *LegacyByteSource) { source.Bytes = int(^uint(0) >> 1) }},
				{name: "shorter-extent", mutate: func(source *LegacyByteSource) { source.Bytes-- }},
				{name: "longer-extent", mutate: func(source *LegacyByteSource) { source.Bytes++ }},
				{name: "missing-reader", mutate: func(source *LegacyByteSource) { source.Read = nil }},
				{name: "missing-manifest", mutate: func(source *LegacyByteSource) { source.Parts = nil }},
				{name: "missing-part", mutate: func(source *LegacyByteSource) { source.Parts = source.Parts[:len(source.Parts)-1] }},
				{name: "extra-part", mutate: func(source *LegacyByteSource) { source.Parts = append(source.Parts, source.Parts[len(source.Parts)-1]) }},
				{name: "opposite-geometry-manifest", mutate: func(source *LegacyByteSource) {
					other := 64 << 10
					if partBytes == 64<<10 {
						other = 512 << 10
					}
					_, replacement := legacySourceGeometryInput(raw, other)
					source.Parts = replacement.Parts
				}},
			} {
				test.Run(fixture.name, func(test *testing.T) {
					invalid := source
					invalid.Parts = append([]ConversationManifestChunk(nil), source.Parts...)
					fixture.mutate(&invalid)
					legacySourceGeometryReject(test, input, invalid)
				})
			}
			for ordinal, part := range source.Parts {
				for _, length := range []int64{-1, 0, part.Bytes - 1, part.Bytes + 1} {
					test.Run(fmt.Sprintf("ordinal-%d/length-%d", ordinal, length), func(test *testing.T) {
						invalid := source
						invalid.Parts = append([]ConversationManifestChunk(nil), source.Parts...)
						invalid.Parts[ordinal].Bytes = length
						legacySourceGeometryReject(test, input, invalid)
					})
				}
				for _, hash := range []ContentHash{"", "bad", ContentHash("sha256:" + strings.Repeat("g", 64)), ContentHash("sha256:" + strings.Repeat("A", 64))} {
					test.Run(fmt.Sprintf("ordinal-%d/hash-%s", ordinal, hash), func(test *testing.T) {
						invalid := source
						invalid.Parts = append([]ConversationManifestChunk(nil), source.Parts...)
						invalid.Parts[ordinal].Hash = hash
						legacySourceGeometryReject(test, input, invalid)
					})
				}
			}
		})
	}
}

func TestLegacySourceGeometryAuthenticatesWellFormedHashes(test *testing.T) {
	raw := []byte("{}" + strings.Repeat(" ", 2*(512<<10)+37))
	for _, partBytes := range []int{0, 512 << 10, 64 << 10} {
		_, source := legacySourceGeometryInput(raw, partBytes)
		for _, ordinal := range []int{0, len(source.Parts) / 2, len(source.Parts) - 1} {
			test.Run(fmt.Sprintf("part-%d/ordinal-%d", partBytes, ordinal), func(test *testing.T) {
				input, source := legacySourceGeometryInput(raw, partBytes)
				source.Parts[ordinal].Hash = HashContent([]byte("different authenticated bytes"))
				normalizer, err := NewLegacyNormalizationDecoder(context.Background(), source)
				if err != nil || normalizer == nil || len(input.reads) != 0 {
					test.Fatal("well-formed hash descriptor triggered eager authentication", err)
				}
				result, err := legacyNormalizationFrameDrain(test, context.Background(), normalizer, input, false)
				if !errors.Is(err, ErrPreparedDocRangeObject) || result.canonical != nil {
					test.Fatal("declared geometry bypassed actual source hash authentication", ordinal, err)
				}
			})
		}
	}
}

func TestLegacySourceGeometryDecoderOwnsMetadataAndUsesCurrentContext(test *testing.T) {
	raw := []byte(`{"envelope":{"cir_version":"2","source_model":"geometry-owned"},"ignored":"` + strings.Repeat("x", 512<<10+37) + `"}`)
	want, err := legacyNormalizationOracle(raw)
	if err != nil {
		test.Fatal(err)
	}
	for _, partBytes := range []int{0, 512 << 10, 64 << 10} {
		test.Run(fmt.Sprintf("part-%d", partBytes), func(test *testing.T) {
			input, source := legacySourceGeometryInput(raw, partBytes)
			borrowed := make([]byte, 512<<10)
			type contextKey struct{}
			key := contextKey{}
			current := context.WithValue(context.Background(), key, "current-step")
			source.Read = func(readCtx context.Context, offset, length int) ([]byte, error) {
				if readCtx.Value(key) != "current-step" || readCtx.Err() != nil {
					test.Fatal("decoder retained the constructor or a previous step context", readCtx.Err())
				}
				body, err := input.Read(readCtx, offset, length)
				if err != nil {
					return nil, err
				}
				copy(borrowed, body)
				return borrowed[:len(body)], nil
			}
			constructorCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			normalizer, err := NewLegacyNormalizationDecoder(constructorCtx, source)
			cancel()
			if err != nil || normalizer == nil || len(input.reads) != 0 {
				test.Fatal("constructor failed or eagerly read source bytes", err)
			}
			before, err := normalizer.RetainedBytes()
			if err != nil || before <= 0 {
				test.Fatal("constructor lost retained source metadata", before, err)
			}
			for ordinal := range source.Parts {
				source.Parts[ordinal].Hash = HashContent([]byte("caller replaced descriptor"))
				source.Parts[ordinal].Bytes = -1
			}
			source.Bytes, source.PartBytes, source.Parts, source.Read = -1, -1, nil, nil
			after, err := normalizer.RetainedBytes()
			if err != nil || after != before {
				test.Fatal("caller metadata changes altered decoder-owned accounting", before, after, err)
			}
			result, err := legacyNormalizationFrameDrain(test, current, normalizer, input, false)
			if err != nil || !bytes.Equal(result.canonical, want) {
				test.Fatal("caller metadata or expired contexts changed normalized bytes", err)
			}
			legacySourceGeometryReads(test, input, partBytes)
		})
	}
}

func TestLegacySourceGeometryPreCanceledConstructor(test *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, partBytes := range []int{0, 512 << 10, 64 << 10, -1} {
		test.Run(fmt.Sprintf("part-%d", partBytes), func(test *testing.T) {
			input, source := legacySourceGeometryInput([]byte("{}"), 0)
			source.PartBytes = partBytes
			normalizer, err := NewLegacyNormalizationDecoder(ctx, source)
			if normalizer != nil || !errors.Is(err, context.Canceled) || len(input.reads) != 0 {
				test.Fatal("constructor ignored current cancellation or read its source", err)
			}
		})
	}
}

func TestLegacySourceGeometryOriginalProofPreservesCanonicalParts(test *testing.T) {
	raw, original := legacySourceProofDocument(test, 2, 300<<10)
	for _, partBytes := range []int{0, 512 << 10} {
		test.Run(fmt.Sprintf("part-%d", partBytes), func(test *testing.T) {
			input, source := legacySourceGeometryInput(raw, partBytes)
			output := &legacySourceProofBuffer{}
			var verifier CanonicalDocVerifier
			preparation, err := NewLegacyByteSourcePreparation(context.Background(), original.Hash(), source, output, &verifier)
			if err != nil || preparation == nil || len(input.reads) != 0 {
				test.Fatal("original proof rejected canonical geometry or eagerly read bytes", err)
			}
			progress, err := legacySourceProofDrain(test, preparation)
			proof, proofErr := preparation.Result()
			if err != nil || progress != LegacyByteSourceVerified || proofErr != nil || !proof.Valid() {
				test.Fatal("canonical geometry could not earn the original proof", progress, err, proofErr)
			}
			plan, err := proof.Plan()
			if err != nil {
				test.Fatal(err)
			}
			spec, err := plan.Spec()
			if err != nil || spec.Reference.Hash != original.Hash() {
				test.Fatal("explicit geometry changed the original source identity", err)
			}
			legacySourceGeometryReads(test, input, partBytes)
		})
	}
}

func TestLegacySourceGeometryOriginalProofRejectsNormalizationParts(test *testing.T) {
	for _, payload := range []int{0, 600 << 10} {
		raw, original := legacySourceProofDocument(test, 1, payload)
		for _, nativeManifest := range []bool{false, true} {
			test.Run(fmt.Sprintf("payload-%d/native-manifest-%t", payload, nativeManifest), func(test *testing.T) {
				fixtureBytes := 0
				if nativeManifest {
					fixtureBytes = 64 << 10
				}
				input, source := legacySourceGeometryInput(raw, fixtureBytes)
				source.PartBytes = 64 << 10
				output := &legacySourceProofBuffer{}
				var verifier CanonicalDocVerifier
				preparation, err := NewLegacyByteSourcePreparation(context.Background(), original.Hash(), source, output, &verifier)
				if preparation != nil || !errors.Is(err, ErrIntegrity) || len(input.reads) != 0 || len(output.body) != 0 || len(verifier.events) != 0 {
					test.Fatal("normalization-only geometry acquired original proof authority or performed I/O", err)
				}
			})
		}
	}
}
