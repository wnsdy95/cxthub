package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type legacySourceProofBuffer struct {
	body     []byte
	reads    [][2]int
	onAppend func()
}

func (buffer *legacySourceProofBuffer) Append(ctx context.Context, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	buffer.body = append(buffer.body, body...)
	if buffer.onAppend != nil {
		buffer.onAppend()
	}
	return ctx.Err()
}

func (buffer *legacySourceProofBuffer) Read(ctx context.Context, offset, length int) ([]byte, error) {
	buffer.reads = append(buffer.reads, [2]int{offset, length})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if offset < 0 || length < 0 || offset > len(buffer.body) || length > len(buffer.body)-offset {
		return nil, ErrIntegrity
	}
	return buffer.body[offset : offset+length], nil
}

func (buffer *legacySourceProofBuffer) input() LegacyByteSource {
	source := LegacyByteSource{Bytes: len(buffer.body), Read: buffer.Read}
	authority, err := newLegacyNormalizationSourceAuthority()
	if err != nil {
		panic(err)
	}
	source.authority = authority
	for offset := 0; offset < len(buffer.body); offset += ConversationManifestChunkBytes {
		body := buffer.body[offset:min(offset+ConversationManifestChunkBytes, len(buffer.body))]
		source.Parts = append(source.Parts, ConversationManifestChunk{Hash: HashContent(body), Bytes: int64(len(body))})
	}
	return source
}

func legacySourceProofDocument(test *testing.T, count, payload int) ([]byte, VerifiedSessionDoc) {
	test.Helper()
	document := CIRDocument{Envelope: CIREnvelope{CIRVersion: "2", SourceProvider: ProviderCodex, Fidelity: FidelityFull}}
	for ordinal := range count {
		document.Events = append(document.Events, CIREvent{Kind: EventMessage, Seq: ordinal, Role: RoleUser,
			Blocks: []ContentBlock{{Type: "text", Text: fmt.Sprintf("%d:", ordinal) + strings.Repeat("x", payload) + "한🙂\"\\\n"}}})
	}
	raw, err := CanonicalBytes(document)
	if err != nil {
		test.Fatal(err)
	}
	proof, err := VerifySessionDoc(SessionDoc{Hash: HashContent(raw), CIR: document})
	if err != nil {
		test.Fatal(err)
	}
	return raw, proof
}

func legacySourceProofDrain(test *testing.T, preparation *LegacyByteSourcePreparation) (LegacyByteSourceProgress, error) {
	test.Helper()
	for range 6000 {
		progress, err := preparation.Step(context.Background())
		if err != nil || progress != LegacyByteSourcePending {
			return progress, err
		}
	}
	test.Fatal("source proof did not terminate")
	return 0, ErrIntegrity
}

func TestLegacySourceProofBoundedReadsOriginalPlanAndSeedParity(test *testing.T) {
	for _, count := range []int{0, 1, 128} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			raw, original := legacySourceProofDocument(test, count, 20<<10)
			input, output := &legacySourceProofBuffer{body: bytes.Clone(raw)}, &legacySourceProofBuffer{}
			var verifier CanonicalDocVerifier
			preparation, err := NewLegacyByteSourcePreparation(context.Background(), original.Hash(), input.input(), output, &verifier)
			if err != nil {
				test.Fatal(err)
			}
			steps := 0
			for ; steps < 6000; steps++ {
				if proof, err := preparation.Result(); proof.Valid() || !errors.Is(err, ErrDocRangeIndexPending) {
					test.Fatal("partial input produced publication authority", steps, proof, err)
				}
				before := len(input.reads)
				progress, err := preparation.Step(context.Background())
				if err != nil {
					test.Fatal(steps, err)
				}
				if len(input.reads)-before > 2 {
					test.Fatal("source quantum read more than two adjacent authenticated blocks", steps, before, len(input.reads))
				}
				retained, err := preparation.RetainedBytes()
				if err != nil || retained <= 0 || retained >= 2<<20 {
					test.Fatal("ordinary source proof retained cumulative canonical bytes", retained, err)
				}
				if progress == LegacyByteSourceVerified {
					break
				}
				if progress != LegacyByteSourcePending || len(verifier.events) != 0 {
					test.Fatal("incomplete source proof escaped or primed the semantic cache", progress, len(verifier.events))
				}
			}
			if steps >= 6000 || count == 128 && steps < 10 {
				test.Fatal("source proof did not retain genuine incremental phases", steps)
			}
			for _, read := range append(input.reads, output.reads...) {
				if read[1] > ConversationManifestChunkBytes {
					test.Fatal("source proof materialized an unbounded read", read)
				}
			}
			proof, err := preparation.Result()
			if err != nil || !proof.Valid() || proof.Source() != output {
				test.Fatal("complete proof lost its bound output", proof, err)
			}
			plan, err := proof.Plan()
			if err != nil {
				test.Fatal(err)
			}
			wantPlan, err := NewLegacyByteBackfill(context.Background(), original)
			if err != nil {
				test.Fatal(err)
			}
			gotSpec, _ := plan.Spec()
			wantSpec, _ := wantPlan.Spec()
			gotKey, _ := plan.Key()
			wantKey, _ := wantPlan.Key()
			if gotSpec != wantSpec || gotKey != wantKey {
				test.Fatal("source proof changed original byte plan identity", gotSpec, wantSpec, gotKey, wantKey)
			}
			for ordinal := range gotSpec.Blocks {
				got, err := plan.Block(context.Background(), ordinal)
				want, baselineErr := wantPlan.Block(context.Background(), ordinal)
				if err != nil || baselineErr != nil || !bytes.Equal(got.Bytes(), want.Bytes()) || got.Descriptor() != want.Descriptor() {
					test.Fatal("source-backed block changed canonical bytes", ordinal, err, baselineErr)
				}
			}
			seed, _ := proof.Seed()
			wantSeed, err := NewLegacyRangeMetadataSeed(context.Background(), original, wantPlan)
			if err != nil || !reflect.DeepEqual(seed, wantSeed) {
				test.Fatal("source proof changed original header authority", err)
			}
			encoded, _ := json.Marshal(proof)
			var decoded VerifiedLegacyByteSource
			if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.Valid() {
				test.Fatal("serialized input minted source proof authority", string(encoded), err)
			}
		})
	}
}

func TestLegacySourceProofRetainsOriginalClassifierAndUnsupportedPrecedence(test *testing.T) {
	raw, original := legacySourceProofDocument(test, 1, 32)
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		test.Fatal(err)
	}
	manifest := []byte(`{"format":"cxt-doc-chunks-v2","envelope":null,"chunks":["bad"]}`)
	future := bytes.Replace(raw, []byte(`"cir_version":"2"`), []byte(`"cir_version":"999"`), 1)
	for _, fixture := range []struct {
		name      string
		raw       []byte
		want      ContentHash
		normalize bool
	}{
		{name: "normalization", raw: pretty.Bytes(), want: original.Hash(), normalize: true},
		{name: "manifest-with-equal-raw-hash", raw: manifest, want: HashContent(manifest)},
		{name: "manifest-with-unequal-raw-hash", raw: manifest, want: original.Hash()},
		{name: "unsupported", raw: future, want: HashContent(future)},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			input, output := &legacySourceProofBuffer{body: bytes.Clone(fixture.raw)}, &legacySourceProofBuffer{}
			var verifier CanonicalDocVerifier
			preparation, err := NewLegacyByteSourcePreparation(context.Background(), fixture.want, input.input(), output, &verifier)
			if err != nil {
				test.Fatal(err)
			}
			if preparation.NormalizationEligible() {
				test.Fatal("unfinished source classified as normalization eligible")
			}
			progress, err := legacySourceProofDrain(test, preparation)
			proof, resultErr := preparation.Result()
			if err != nil || progress != LegacyByteSourceOriginalRequired || proof.Valid() || !errors.Is(resultErr, ErrDocRangeIndexPending) || len(verifier.events) != 0 {
				test.Fatal("original classification was guessed or bypassed", progress, err, resultErr)
			}
			if preparation.NormalizationEligible() != fixture.normalize {
				test.Fatal("normalization routing changed original classification", preparation.NormalizationEligible(), fixture.normalize)
			}
		})
	}
}

func TestLegacySourceProofPrivateMutationCannotUseCachedEventOrSealEarly(test *testing.T) {
	for _, mutation := range []string{"input", "late-output"} {
		test.Run(mutation, func(test *testing.T) {
			raw, original := legacySourceProofDocument(test, 128, 20<<10)
			input, output := &legacySourceProofBuffer{body: bytes.Clone(raw)}, &legacySourceProofBuffer{}
			var verifier CanonicalDocVerifier
			if _, err := verifier.Verify(context.Background(), original.Hash(), raw); err != nil {
				test.Fatal(err)
			}
			preparation, err := NewLegacyByteSourcePreparation(context.Background(), original.Hash(), input.input(), output, &verifier)
			if err != nil {
				test.Fatal(err)
			}
			if mutation == "input" {
				for preparation.phase < 2 {
					if _, err := preparation.Step(context.Background()); err != nil {
						test.Fatal(err)
					}
				}
				input.body[len(input.body)-1] ^= 1
			} else {
				output.onAppend = func() {
					if int64(len(output.body)) == preparation.spec.StreamBytes {
						output.body[0] ^= 1
					}
				}
			}
			_, err = legacySourceProofDrain(test, preparation)
			if !errors.Is(err, ErrPreparedDocRangeObject) {
				test.Fatal("private mutation escaped original hash receipts", mutation, err)
			}
			if proof, _ := preparation.Result(); proof.Valid() {
				test.Fatal("private damage produced a sealed plan/header")
			}
		})
	}
}

func TestLegacySourceProofCancellationDoesNotInvalidateCommittedCacheAdmission(test *testing.T) {
	raw, original := legacySourceProofDocument(test, 1, 32)
	input, output := &legacySourceProofBuffer{body: bytes.Clone(raw)}, &legacySourceProofBuffer{}
	var verifier CanonicalDocVerifier
	preparation, err := NewLegacyByteSourcePreparation(context.Background(), original.Hash(), input.input(), output, &verifier)
	if err != nil {
		test.Fatal(err)
	}
	for preparation.phase != 4 {
		if _, err := preparation.Step(context.Background()); err != nil {
			test.Fatal(err)
		}
	}
	canceled, cancelBefore := context.WithCancel(context.Background())
	cancelBefore()
	if _, err := preparation.Step(canceled); !errors.Is(err, context.Canceled) || len(verifier.events) != 0 {
		test.Fatal("cancellation before admission changed cache", err, len(verifier.events))
	}
	base, cancelAfter := context.WithCancel(context.Background())
	defer cancelAfter()
	ctx := canonicalAdmissionContext{Context: base, check: func() {
		if len(verifier.events) != 0 {
			cancelAfter()
		}
	}}
	progress, err := preparation.Step(ctx)
	proof, resultErr := preparation.Result()
	if err != nil || progress != LegacyByteSourceVerified || resultErr != nil || !proof.Valid() || len(verifier.events) == 0 {
		test.Fatal("post-admission cancellation contradicted the committed proof/cache", progress, err, resultErr, len(verifier.events))
	}
}

func TestLegacySourceProofEOFDoesNotSwallowPrivateIntegritySubtype(test *testing.T) {
	for _, private := range []bool{false, true} {
		read := func(_ context.Context, _ int, length int) ([]byte, error) {
			if length == 0 {
				return nil, nil
			}
			if private {
				return nil, ErrPreparedDocRangeObject
			}
			return nil, ErrIntegrity
		}
		err := legacySourceEOF(context.Background(), read, 0)
		if private && !errors.Is(err, ErrPreparedDocRangeObject) || !private && err != nil {
			test.Fatal("EOF confused private corruption with expected range bounds", private, err)
		}
	}
}

func TestLegacySourceProofOwnsMetadataAndReusableLoaderBytes(test *testing.T) {
	raw, original := legacySourceProofDocument(test, 128, 20<<10)
	input, output := &legacySourceProofBuffer{body: bytes.Clone(raw)}, &legacySourceProofBuffer{}
	source := input.input()
	scratch := make([]byte, ConversationManifestChunkBytes)
	source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
		body, err := input.Read(ctx, offset, length)
		if err != nil {
			return nil, err
		}
		copy(scratch, body)
		return scratch[:len(body)], nil
	}
	var verifier CanonicalDocVerifier
	preparation, err := NewLegacyByteSourcePreparation(context.Background(), original.Hash(), source, output, &verifier)
	if err != nil {
		test.Fatal(err)
	}
	source.Parts[0].Hash = HashContent([]byte("caller changed metadata"))
	progress, err := legacySourceProofDrain(test, preparation)
	if err != nil || progress != LegacyByteSourceVerified {
		test.Fatal("source proof borrowed caller metadata or reusable bytes", progress, err)
	}
	proof, _ := preparation.Result()
	plan, _ := proof.Plan()
	spec, _ := plan.Spec()
	_, stream, err := canonicalVerificationFrame(context.Background(), raw)
	if err != nil || !bytes.Equal(output.body, stream) || spec.Reference.Hash != original.Hash() {
		test.Fatal("reused loader changed canonical stream", err)
	}
}

func TestLegacySourceProofMalformedFramingAndOrderParity(test *testing.T) {
	raw, _ := legacySourceProofDocument(test, 2, 32)
	var parts struct {
		Envelope json.RawMessage   `json:"envelope"`
		Events   []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		test.Fatal(err)
	}
	backwards := AssembleCanonicalDoc(parts.Envelope, [][]byte{bytes.Join([][]byte{parts.Events[1], parts.Events[0]}, []byte(","))})
	if !json.Valid(backwards) {
		test.Fatal("order fixture must remain valid JSON")
	}
	for _, body := range [][]byte{
		backwards,
		[]byte(`{"envelope":{},"events":[,]}`),
		append(bytes.Clone(raw), '\n'),
		bytes.Replace(raw, []byte(`"events":[`), []byte(`"events": [`), 1),
		bytes.Replace(raw, []byte(`"kind":"message"`), []byte(`"kind":"bogus"`), 1),
	} {
		var before, after CanonicalDocVerifier
		want := HashContent(body)
		_, baselineErr := before.Verify(context.Background(), want, body)
		input, output := &legacySourceProofBuffer{body: bytes.Clone(body)}, &legacySourceProofBuffer{}
		preparation, err := NewLegacyByteSourcePreparation(context.Background(), want, input.input(), output, &after)
		if err != nil {
			test.Fatal(err)
		}
		_, err = legacySourceProofDrain(test, preparation)
		if !errors.Is(baselineErr, ErrIntegrity) || !errors.Is(err, ErrIntegrity) || len(after.events) != 0 {
			test.Fatal("source proof changed malformed input semantics", baselineErr, err, len(after.events))
		}
	}
}

func TestLegacySourceProofCancellationDuringReadNeverCreatesAuthority(test *testing.T) {
	raw, original := legacySourceProofDocument(test, 128, 20<<10)
	input, output := &legacySourceProofBuffer{body: bytes.Clone(raw)}, &legacySourceProofBuffer{}
	source := input.input()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
		body, err := input.Read(ctx, offset, length)
		cancel()
		return body, err
	}
	var verifier CanonicalDocVerifier
	preparation, err := NewLegacyByteSourcePreparation(context.Background(), original.Hash(), source, output, &verifier)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := preparation.Step(ctx); !errors.Is(err, context.Canceled) || len(output.body) != 0 || len(verifier.events) != 0 {
		test.Fatal("canceled source read wrote output or admitted event cache", err)
	}
	if proof, _ := preparation.Result(); proof.Valid() {
		test.Fatal("canceled source read produced a sealed proof")
	}
	if _, err := preparation.Step(context.Background()); !errors.Is(err, context.Canceled) {
		test.Fatal("failed source preparation resumed partial state", err)
	}
}

func TestLegacySourceProofActiveSealCancellationLeavesCacheUnchanged(test *testing.T) {
	raw, original := legacySourceProofDocument(test, 1, 32)
	input, output := &legacySourceProofBuffer{body: bytes.Clone(raw)}, &legacySourceProofBuffer{}
	var verifier CanonicalDocVerifier
	preparation, err := NewLegacyByteSourcePreparation(context.Background(), original.Hash(), input.input(), output, &verifier)
	if err != nil {
		test.Fatal(err)
	}
	for preparation.phase != 4 {
		if _, err := preparation.Step(context.Background()); err != nil {
			test.Fatal(err)
		}
	}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	checks := 0
	ctx := canonicalAdmissionContext{Context: base, check: func() {
		checks++
		if checks == 6 {
			cancel()
		}
	}}
	progress, err := preparation.Step(ctx)
	proof, resultErr := preparation.Result()
	if !errors.Is(err, context.Canceled) || progress == LegacyByteSourceVerified || proof.Valid() || !errors.Is(resultErr, ErrDocRangeIndexPending) || len(verifier.events) != 0 || checks != 6 {
		test.Fatal("active pre-admission cancellation changed proof/cache", progress, err, resultErr, len(verifier.events), checks)
	}
}
