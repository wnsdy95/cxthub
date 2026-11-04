package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func legacySourceHeaderFenceDocument(test *testing.T, large bool) ([]byte, []byte) {
	test.Helper()
	envelope := CIREnvelope{CIRVersion: "2", SourceProvider: ProviderCodex, Fidelity: FidelityFull}
	if large {
		envelope.Cwd = strings.Repeat("x", 2*ConversationManifestChunkBytes+73)
	}
	raw, err := CanonicalBytes(CIRDocument{Envelope: envelope, Events: []CIREvent{{Kind: EventMessage, Seq: 0, Role: RoleUser,
		Blocks: []ContentBlock{{Type: "text", Text: "source header fence"}}}}})
	if err != nil {
		test.Fatal(err)
	}
	header, _, err := canonicalVerificationFrame(context.Background(), raw)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := decodeCanonicalCIREnvelope(header); err != nil {
		test.Fatal("fence fixture failed the original exact-byte oracle", err)
	}
	return raw, header
}

func legacySourceHeaderFenceHelper(test *testing.T, raw []byte, expected ContentHash) (*legacySourceEnvelope, *legacySourceProofBuffer, LegacyByteSource) {
	test.Helper()
	header, _, err := canonicalVerificationFrame(context.Background(), raw)
	if err != nil {
		test.Fatal(err)
	}
	helper, err := newLegacySourceEnvelope(len(canonicalDocPrefix), len(canonicalDocPrefix)+len(header), expected)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(helper.release)
	input := &legacySourceProofBuffer{body: bytes.Clone(raw)}
	return helper, input, input.input()
}

func legacySourceHeaderFenceTerminal(test *testing.T, helper *legacySourceEnvelope, input *legacySourceProofBuffer, source LegacyByteSource, want error) {
	test.Helper()
	reads := len(input.reads)
	complete, err := helper.step(context.Background(), source)
	if complete || !errors.Is(err, want) || len(input.reads) != reads {
		test.Fatal("failed header helper resumed source I/O or lost its terminal failure", complete, err, want)
	}
}

func legacySourceHeaderFenceUnpublished(test *testing.T, preparation *LegacyByteSourcePreparation, output *legacySourceProofBuffer, verifier *CanonicalDocVerifier) {
	test.Helper()
	proof, err := preparation.Result()
	if proof.Valid() || !errors.Is(err, ErrDocRangeIndexPending) || len(output.body) != 0 || len(verifier.events) != 0 {
		test.Fatal("unfinished or rejected header wrote output, primed the cache, or published authority", err, len(output.body), len(verifier.events))
	}
}

func TestLegacySourceHeaderFencesHelperUsesCurrentContextAndFreshBorrowedPages(test *testing.T) {
	raw, header := legacySourceHeaderFenceDocument(test, true)
	helper, input, source := legacySourceHeaderFenceHelper(test, raw, HashContent(header))
	type stepContextKey struct{}
	activeStep := -1
	borrowed := make([]byte, ConversationManifestChunkBytes)
	seen := make(map[int]int)
	replayed, zeroEOF, oneEOF := false, false, false
	source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
		if ctx.Err() != nil || ctx.Value(stepContextKey{}) != activeStep {
			test.Fatal("header read retained an expired Step context", offset, length, activeStep, ctx.Err())
		}
		if offset == len(raw) {
			zeroEOF = zeroEOF || length == 0
			oneEOF = oneEOF || length == 1
		} else {
			if previous, exists := seen[offset]; exists && previous != activeStep {
				replayed = true
			}
			seen[offset] = activeStep
		}
		body, err := input.Read(ctx, offset, length)
		if err != nil {
			return nil, err
		}
		copy(borrowed, body)
		return borrowed[:len(body)], nil
	}
	for stepIndex := 0; stepIndex < 512; stepIndex++ {
		activeStep = stepIndex
		ctx, cancel := context.WithCancel(context.WithValue(context.Background(), stepContextKey{}, activeStep))
		before := len(input.reads)
		complete, err := helper.step(ctx, source)
		cancel()
		clear(borrowed)
		if err != nil {
			test.Fatal("current context or poisoned borrowed bytes changed header acceptance", stepIndex, err)
		}
		payload, probes := 0, 0
		for _, read := range input.reads[before:] {
			if read[0] == len(raw) {
				probes++
				continue
			}
			payload++
			if read[0]%ConversationManifestChunkBytes != 0 || read[1] != min(ConversationManifestChunkBytes, len(raw)-read[0]) {
				test.Fatal("header helper bypassed the original authenticated full-part read", read)
			}
		}
		if payload > 2 || probes > 2 || payload != 0 && probes != 0 {
			test.Fatal("header helper merged native payload loads with its exact EOF phase", payload, probes)
		}
		if !complete {
			continue
		}
		if !replayed || !zeroEOF || !oneEOF || helper.version != "2" {
			test.Fatal("header acceptance skipped fresh native rereads, exact EOF, or the decoded version", replayed, zeroEOF, oneEOF, helper.version)
		}
		reads := len(input.reads)
		complete, err = helper.step(context.Background(), source)
		if err != nil || !complete || len(input.reads) != reads {
			test.Fatal("completed header helper resumed source reads", complete, err)
		}
		return
	}
	test.Fatal("header helper exhausted its finite progress bound")
}

func TestLegacySourceHeaderFencesHelperNativeRereadFailureIsTerminal(test *testing.T) {
	raw, header := legacySourceHeaderFenceDocument(test, true)
	for _, fault := range []string{"hash", "short", "late-io", "private-eof", "cancel-hash", "cancel-private-eof"} {
		test.Run(fault, func(test *testing.T) {
			helper, input, source := legacySourceHeaderFenceHelper(test, raw, HashContent(header))
			seen := make(map[int]int)
			activeStep, failedAt := 0, -1
			triggered := false
			var cancelRead context.CancelFunc
			source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
				body, err := input.Read(ctx, offset, length)
				previous, exists := seen[offset]
				seen[offset] = activeStep
				if triggered || err != nil || length == 0 || offset == len(raw) || !exists || previous == activeStep {
					return body, err
				}
				triggered, failedAt = true, len(input.reads)
				if strings.HasPrefix(fault, "cancel") {
					cancelRead()
				}
				switch fault {
				case "hash", "cancel-hash":
					body[0] ^= 1
					return body, nil
				case "short":
					return body[:len(body)-1], nil
				case "late-io":
					return body, io.ErrClosedPipe
				default:
					return nil, errors.Join(ErrPreparedDocRangeObject, io.EOF)
				}
			}
			for activeStep = 0; activeStep < 512; activeStep++ {
				ctx, cancel := context.WithCancel(context.Background())
				cancelRead = cancel
				complete, err := helper.step(ctx, source)
				cancel()
				if err == nil {
					if complete {
						test.Fatal("header completed without testing its next-Step source authentication")
					}
					continue
				}
				want := error(ErrPreparedDocRangeObject)
				if fault == "late-io" {
					want = io.ErrClosedPipe
				} else if strings.HasPrefix(fault, "cancel") {
					want = context.Canceled
				}
				if complete || !triggered || len(input.reads) != failedAt || !errors.Is(err, want) || errors.Is(err, ErrUnsupportedCIRVersion) || want == context.Canceled && errors.Is(err, ErrPreparedDocRangeObject) {
					test.Fatal("native header reread reused a stale hash or lost current I/O/context precedence", complete, triggered, err, want)
				}
				legacySourceHeaderFenceTerminal(test, helper, input, source, want)
				return
			}
			test.Fatal("native header fault exhausted its finite progress bound")
		})
	}
}

func TestLegacySourceHeaderFencesEncoderCancellationIsTerminal(test *testing.T) {
	raw, header := legacySourceHeaderFenceDocument(test, true)
	for _, outer := range []bool{false, true} {
		test.Run(fmt.Sprintf("outer-%t", outer), func(test *testing.T) {
			helper, input, source := legacySourceHeaderFenceHelper(test, raw, HashContent(header))
			output := &legacySourceProofBuffer{}
			var verifier CanonicalDocVerifier
			var preparation *LegacyByteSourcePreparation
			if outer {
				var err error
				preparation, err = NewLegacyByteSourcePreparation(context.Background(), HashContent(raw), source, output, &verifier)
				if err != nil {
					test.Fatal(err)
				}
			}
			for range 512 {
				if outer {
					progress, err := preparation.Step(context.Background())
					legacySourceHeaderFenceUnpublished(test, preparation, output, &verifier)
					if err != nil || progress != LegacyByteSourcePending {
						test.Fatal("outer header did not reach its actual incremental encoder", progress, err)
					}
					helper = preparation.envelope
				} else if complete, err := helper.step(context.Background(), source); complete || err != nil {
					test.Fatal("header helper did not reach its actual incremental encoder", complete, err)
				}
				if helper == nil || helper.encoding == nil || len(helper.hashState) == 0 {
					continue
				}
				reads := len(input.reads)
				base, cancel := context.WithCancel(context.Background())
				checks := 0
				ctx := canonicalAdmissionContext{Context: base, check: func() {
					checks++
					if checks == 4 {
						cancel()
					}
				}}
				var failure error
				if outer {
					progress, err := preparation.Step(ctx)
					failure = err
					if progress == LegacyByteSourceVerified || progress == LegacyByteSourceOriginalRequired {
						test.Fatal("canceled outer encoder published a terminal success", progress)
					}
				} else if complete, err := helper.step(ctx, source); complete {
					test.Fatal("canceled header encoder completed", err)
				} else {
					failure = err
				}
				cancel()
				if checks < 4 || !errors.Is(failure, context.Canceled) || errors.Is(failure, ErrPreparedDocRangeObject) || errors.Is(failure, ErrUnsupportedCIRVersion) || len(input.reads) != reads {
					test.Fatal("active encoder/hash cancellation lost context priority or read source/EOF", checks, failure, len(input.reads)-reads)
				}
				if outer {
					if _, err := preparation.Step(context.Background()); !errors.Is(err, context.Canceled) || len(input.reads) != reads || preparation.NormalizationEligible() {
						test.Fatal("canceled outer header encoder resumed source I/O or enabled normalization", err)
					}
					legacySourceHeaderFenceUnpublished(test, preparation, output, &verifier)
				} else {
					legacySourceHeaderFenceTerminal(test, helper, input, source, context.Canceled)
				}
				return
			}
			test.Fatal("encoder cancellation exhausted its finite progress bound")
		})
	}
}

func TestLegacySourceHeaderFencesOuterRereadsAuthenticatedHeaderBeforeClassification(test *testing.T) {
	raw, header := legacySourceHeaderFenceDocument(test, false)
	_, stream, err := canonicalVerificationFrame(context.Background(), raw)
	if err != nil {
		test.Fatal(err)
	}
	for _, candidate := range []struct {
		name   string
		header []byte
	}{
		{name: "canonical", header: header},
		{name: "unsupported", header: bytes.Replace(header, []byte(`"cir_version":"2"`), []byte(`"cir_version":"999"`), 1)},
		{name: "wrong-type", header: bytes.Replace(header, []byte(`"captured_at":""`), []byte(`"captured_at":false`), 1)},
	} {
		for _, fault := range []string{"hash", "late-io", "private-eof", "cancel-private-eof"} {
			test.Run(candidate.name+"/"+fault, func(test *testing.T) {
				body := AssembleCanonicalDoc(candidate.header, [][]byte{stream})
				input, output := &legacySourceProofBuffer{body: bytes.Clone(body)}, &legacySourceProofBuffer{}
				source := input.input()
				priorZero, priorOne, triggered := false, false, false
				failedAt := -1
				var cancelRead context.CancelFunc
				source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
					part, readErr := input.Read(ctx, offset, length)
					priorZero = priorZero || offset == len(body) && length == 0
					priorOne = priorOne || offset == len(body) && length == 1
					if triggered || !priorOne || offset >= len(body) || length == 0 {
						return part, readErr
					}
					triggered, failedAt = true, len(input.reads)
					switch fault {
					case "hash":
						part[0] ^= 1
						return part, nil
					case "late-io":
						return part, io.ErrClosedPipe
					case "cancel-private-eof":
						cancelRead()
					}
					return nil, errors.Join(ErrPreparedDocRangeObject, io.EOF)
				}
				var verifier CanonicalDocVerifier
				preparation, err := NewLegacyByteSourcePreparation(context.Background(), HashContent(body), source, output, &verifier)
				if err != nil {
					test.Fatal(err)
				}
				for range 128 {
					ctx, cancel := context.WithCancel(context.Background())
					cancelRead = cancel
					progress, failure := preparation.Step(ctx)
					cancel()
					legacySourceHeaderFenceUnpublished(test, preparation, output, &verifier)
					if failure == nil {
						if progress != LegacyByteSourcePending {
							test.Fatal("outer classifier reused first-pass bytes without current source authentication", progress)
						}
						continue
					}
					want := error(ErrPreparedDocRangeObject)
					if fault == "late-io" {
						want = io.ErrClosedPipe
					} else if fault == "cancel-private-eof" {
						want = context.Canceled
					}
					if !triggered || !priorZero || !priorOne || len(input.reads) != failedAt || !errors.Is(failure, want) || errors.Is(failure, ErrUnsupportedCIRVersion) || want == context.Canceled && errors.Is(failure, ErrPreparedDocRangeObject) {
						test.Fatal("outer native header source fault lost priority to cached semantics/version", triggered, priorZero, priorOne, failure)
					}
					reads := len(input.reads)
					if _, repeated := preparation.Step(context.Background()); !errors.Is(repeated, want) || len(input.reads) != reads || preparation.NormalizationEligible() {
						test.Fatal("failed outer native header resumed I/O or enabled normalization", repeated)
					}
					legacySourceHeaderFenceUnpublished(test, preparation, output, &verifier)
					return
				}
				test.Fatal("outer native source fault exhausted its finite progress bound")
			})
		}
	}
}

func TestLegacySourceHeaderFencesSemanticFailureWaitsForCurrentEOF(test *testing.T) {
	raw, header := legacySourceHeaderFenceDocument(test, false)
	_, stream, err := canonicalVerificationFrame(context.Background(), raw)
	if err != nil {
		test.Fatal(err)
	}
	for _, fixture := range []struct {
		name     string
		header   []byte
		expected ContentHash
		want     error
	}{
		{name: "unsupported", header: bytes.Replace(header, []byte(`"cir_version":"2"`), []byte(`"cir_version":"999"`), 1), want: ErrUnsupportedCIRVersion},
		{name: "wrong-type", header: bytes.Replace(header, []byte(`"captured_at":""`), []byte(`"captured_at":false`), 1), want: ErrIntegrity},
		{name: "noncanonical", header: append([]byte("{ "), header[1:]...), want: ErrIntegrity},
		{name: "digest-mismatch", header: header, expected: HashContent([]byte("different first-pass envelope")), want: ErrIntegrity},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			_, oracleErr := decodeCanonicalCIREnvelope(fixture.header)
			if fixture.expected == "" && !errors.Is(oracleErr, fixture.want) || fixture.expected != "" && oracleErr != nil {
				test.Fatal("fixture lost its independent exact-byte classification", oracleErr)
			}
			body := AssembleCanonicalDoc(fixture.header, [][]byte{stream})
			expected := fixture.expected
			if expected == "" {
				expected = HashContent(fixture.header)
			}
			helper, input, source := legacySourceHeaderFenceHelper(test, body, expected)
			zeroEOF, oneEOF := false, false
			source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
				zeroEOF = zeroEOF || offset == len(body) && length == 0
				oneEOF = oneEOF || offset == len(body) && length == 1
				return input.Read(ctx, offset, length)
			}
			for range 128 {
				complete, err := helper.step(context.Background(), source)
				if err == nil {
					if complete {
						test.Fatal("invalid header completed successfully")
					}
					continue
				}
				if complete || !zeroEOF || !oneEOF || !errors.Is(err, fixture.want) {
					test.Fatal("semantic, version, or canonical digest failure preceded current exact EOF", complete, zeroEOF, oneEOF, err)
				}
				legacySourceHeaderFenceTerminal(test, helper, input, source, fixture.want)
				return
			}
			test.Fatal("semantic header failure exhausted its finite progress bound")
		})
	}
}

func TestLegacySourceHeaderFencesLateEOFOverridesSemanticFailure(test *testing.T) {
	raw, header := legacySourceHeaderFenceDocument(test, false)
	_, stream, err := canonicalVerificationFrame(context.Background(), raw)
	if err != nil {
		test.Fatal(err)
	}
	for _, candidate := range []struct {
		name   string
		header []byte
	}{
		{name: "canonical", header: header},
		{name: "unsupported", header: bytes.Replace(header, []byte(`"cir_version":"2"`), []byte(`"cir_version":"999"`), 1)},
		{name: "wrong-type", header: bytes.Replace(header, []byte(`"captured_at":""`), []byte(`"captured_at":false`), 1)},
		{name: "noncanonical", header: append([]byte("{ "), header[1:]...)},
	} {
		for _, outer := range []bool{false, true} {
			for _, fault := range []struct {
				name    string
				length  int
				extra   bool
				noEOF   bool
				cancel  bool
				ioError error
			}{
				{name: "zero-private-eof"},
				{name: "one-private-eof", length: 1},
				{name: "zero-private-io", ioError: io.ErrClosedPipe},
				{name: "one-private-io", length: 1, ioError: io.ErrClosedPipe},
				{name: "zero-extra", extra: true},
				{name: "one-extra", length: 1, extra: true},
				{name: "one-missing-eof", length: 1, noEOF: true},
				{name: "zero-cancel-private-eof", cancel: true},
				{name: "one-cancel-private-eof", length: 1, cancel: true},
			} {
				test.Run(fmt.Sprintf("%s/outer-%t/%s", candidate.name, outer, fault.name), func(test *testing.T) {
					body := AssembleCanonicalDoc(candidate.header, [][]byte{stream})
					helper, input, source := legacySourceHeaderFenceHelper(test, body, HashContent(candidate.header))
					priorZero, priorOne, nativeRead, triggered := false, false, false, false
					failedAt := -1
					var cancelRead context.CancelFunc
					source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
						part, readErr := input.Read(ctx, offset, length)
						if offset < len(body) && length > 0 && priorOne {
							nativeRead = true
						}
						eligible := !outer || nativeRead
						if !triggered && eligible && offset == len(body) && length == fault.length {
							triggered, failedAt = true, len(input.reads)
							if fault.cancel {
								cancelRead()
							}
							if fault.extra {
								return []byte("x"), nil
							}
							if fault.noEOF {
								return nil, nil
							}
							cause := fault.ioError
							if cause == nil {
								cause = io.EOF
							}
							return nil, errors.Join(ErrPreparedDocRangeObject, cause)
						}
						priorZero = priorZero || offset == len(body) && length == 0
						priorOne = priorOne || offset == len(body) && length == 1
						return part, readErr
					}
					output := &legacySourceProofBuffer{}
					var verifier CanonicalDocVerifier
					var preparation *LegacyByteSourcePreparation
					if outer {
						preparation, err = NewLegacyByteSourcePreparation(context.Background(), HashContent(body), source, output, &verifier)
						if err != nil {
							test.Fatal(err)
						}
					}
					for range 128 {
						ctx, cancel := context.WithCancel(context.Background())
						cancelRead = cancel
						complete := false
						var failure error
						if outer {
							var progress LegacyByteSourceProgress
							progress, failure = preparation.Step(ctx)
							complete = progress != LegacyByteSourcePending
							legacySourceHeaderFenceUnpublished(test, preparation, output, &verifier)
						} else {
							complete, failure = helper.step(ctx, source)
						}
						cancel()
						if failure == nil {
							if complete {
								test.Fatal("header classification escaped before its late current EOF fault", triggered)
							}
							continue
						}
						want := error(ErrPreparedDocRangeObject)
						if fault.ioError != nil {
							want = fault.ioError
						} else if fault.cancel {
							want = context.Canceled
						}
						if !triggered || !outer && complete || len(input.reads) != failedAt || !errors.Is(failure, want) || errors.Is(failure, ErrUnsupportedCIRVersion) || fault.cancel && errors.Is(failure, ErrPreparedDocRangeObject) || outer && (!priorZero || !priorOne || !nativeRead) {
							test.Fatal("pending header semantics hid late private EOF/context or skipped prior EOF rechecking", triggered, priorZero, priorOne, nativeRead, failure)
						}
						if outer {
							reads := len(input.reads)
							if _, repeated := preparation.Step(context.Background()); !errors.Is(repeated, want) || len(input.reads) != reads || preparation.NormalizationEligible() {
								test.Fatal("terminal outer header failure resumed I/O or enabled normalization", repeated)
							}
							legacySourceHeaderFenceUnpublished(test, preparation, output, &verifier)
						} else {
							legacySourceHeaderFenceTerminal(test, helper, input, source, want)
						}
						return
					}
					test.Fatal("late header EOF fault exhausted its finite progress bound")
				})
			}
		}
	}
}

func TestLegacySourceHeaderFencesOuterOwnsManifestAndUsesCurrentContext(test *testing.T) {
	raw, _ := legacySourceHeaderFenceDocument(test, true)
	input, output := &legacySourceProofBuffer{body: bytes.Clone(raw)}, &legacySourceProofBuffer{}
	source := input.input()
	type stepContextKey struct{}
	activeStep, zeroEOF, oneEOF := -1, 0, 0
	borrowed := make([]byte, ConversationManifestChunkBytes)
	source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
		if ctx.Err() != nil || ctx.Value(stepContextKey{}) != activeStep {
			test.Fatal("outer header read kept a constructor or prior Step context", offset, length, ctx.Err())
		}
		if offset == len(raw) {
			if length == 0 {
				zeroEOF++
			} else if length == 1 {
				oneEOF++
			}
		}
		body, err := input.Read(ctx, offset, length)
		if err != nil {
			return nil, err
		}
		copy(borrowed, body)
		return borrowed[:len(body)], nil
	}
	var verifier CanonicalDocVerifier
	constructor, cancelConstructor := context.WithCancel(context.Background())
	preparation, err := NewLegacyByteSourcePreparation(constructor, HashContent(raw), source, output, &verifier)
	cancelConstructor()
	if err != nil || len(input.reads) != 0 {
		test.Fatal("outer constructor read the header eagerly", err, len(input.reads))
	}
	for ordinal := range source.Parts {
		source.Parts[ordinal].Hash = HashContent([]byte("caller replaced the frozen manifest"))
		source.Parts[ordinal].Bytes = -1
	}
	for activeStep = 0; activeStep < 1024; activeStep++ {
		before, beforeErr := preparation.RetainedBytes()
		reads, outputBytes := len(input.reads), len(output.body)
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		progress, canceledErr := preparation.Step(canceled)
		after, afterErr := preparation.RetainedBytes()
		if progress == LegacyByteSourceVerified || !errors.Is(canceledErr, context.Canceled) || beforeErr != nil || afterErr != nil || before != after || len(input.reads) != reads || len(output.body) != outputBytes {
			test.Fatal("pre-canceled outer header quantum mutated retained state, source reads, or output", canceledErr, before, after)
		}
		ctx, stop := context.WithCancel(context.WithValue(context.Background(), stepContextKey{}, activeStep))
		progress, err = preparation.Step(ctx)
		stop()
		clear(borrowed)
		if err != nil {
			test.Fatal("header helper used caller-mutated metadata, a stale context, or a borrowed source alias", activeStep, err)
		}
		if oneEOF < 2 {
			legacySourceHeaderFenceUnpublished(test, preparation, output, &verifier)
		}
		if progress == LegacyByteSourcePending {
			proof, resultErr := preparation.Result()
			if proof.Valid() || !errors.Is(resultErr, ErrDocRangeIndexPending) || len(verifier.events) != 0 {
				test.Fatal("pending outer header quantum exposed proof/cache authority", resultErr)
			}
			continue
		}
		proof, resultErr := preparation.Result()
		if progress != LegacyByteSourceVerified || resultErr != nil || !proof.Valid() || zeroEOF < 2 || oneEOF < 2 {
			test.Fatal("frozen source manifest did not survive native/encoding/EOF handoff", progress, resultErr, zeroEOF, oneEOF)
		}
		_, stream, frameErr := canonicalVerificationFrame(context.Background(), raw)
		if frameErr != nil || !bytes.Equal(output.body, stream) {
			test.Fatal("borrowed source pages changed the verified target stream", frameErr)
		}
		return
	}
	test.Fatal("outer header context fixture exhausted its finite progress bound")
}
