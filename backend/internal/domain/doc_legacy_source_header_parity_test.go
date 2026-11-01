package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"testing"
)

type legacySourceHeaderParityFixture struct {
	name    string
	header  []byte
	failure error
}

func legacySourceHeaderParityFixtures(test *testing.T) []legacySourceHeaderParityFixture {
	test.Helper()
	var fixtures []legacySourceHeaderParityFixture
	for _, fixture := range []struct {
		name     string
		envelope CIREnvelope
	}{
		{name: "implicit-version-defaults"},
		{name: "explicit-v1", envelope: CIREnvelope{CIRVersion: CIRVersionV1}},
		{name: "explicit-v2", envelope: CIREnvelope{CIRVersion: CIRVersionV2}},
		{name: "omitted-zero-counters", envelope: CIREnvelope{CIRVersion: CIRVersionV2, ContextTokens: 0, OutputTokens: 0, CompactionCount: 0}},
		{name: "nil-models", envelope: CIREnvelope{CIRVersion: CIRVersionV2, SourceModels: nil}},
		{name: "empty-models", envelope: CIREnvelope{CIRVersion: CIRVersionV2, SourceModels: []string{}}},
		{name: "ordered-repeated-models", envelope: CIREnvelope{CIRVersion: CIRVersionV2, SourceModels: []string{"", "same", "same", "last"}}},
		{name: "signed-counters", envelope: CIREnvelope{CIRVersion: CIRVersionV2, ContextTokens: -17, OutputTokens: 19, CompactionCount: -3}},
		{name: "integer-extremes", envelope: CIREnvelope{CIRVersion: CIRVersionV2, ContextTokens: int(^uint(0) >> 1), OutputTokens: -int(^uint(0)>>1) - 1}},
		{name: "canonical-escapes", envelope: CIREnvelope{CIRVersion: CIRVersionV2, Cwd: "한🙂/<&>\u2028\u2029\"\\\b\f\n\r\t\x00\x1f", SourceModels: []string{"한🙂", "\ufffd"}}},
		{name: "canonical-replacement-runes", envelope: CIREnvelope{CIRVersion: CIRVersionV2, SourceModel: "\xff\xc0\xaf\xed\xa0\x80", SourceModels: []string{"\xff", "\ufffd"}}},
	} {
		header, err := canonicalJSON(fixture.envelope)
		if err != nil {
			test.Fatal(err)
		}
		fixtures = append(fixtures, legacySourceHeaderParityFixture{name: fixture.name, header: header})
	}
	full, err := canonicalJSON(CIREnvelope{CIRVersion: CIRVersionV2, SourceProvider: ProviderCodex, SourceModel: "model",
		CapturedAt: "2026-10-26T00:00:00Z", Cwd: "한🙂/<&>\u2028\u2029\n", GitBranch: "main", SessionOriginID: "source-session", Fidelity: FidelityFull,
		ContextTokens: 17, OutputTokens: 19, SourceModels: []string{"first", "same", "same", "last"}, CompactionCount: 3})
	if err != nil {
		test.Fatal(err)
	}
	fixtures = append(fixtures, legacySourceHeaderParityFixture{name: "all-fields", header: full})
	for _, member := range []string{
		`"captured_at":"2026-10-26T00:00:00Z"`, `"cir_version":"2"`, `"cwd":"한🙂/\u003c\u0026\u003e\u2028\u2029\n"`,
		`"fidelity":"full"`, `"git_branch":"main"`, `"session_origin_id":"source-session"`, `"source_model":"model"`, `"source_provider":"codex"`,
	} {
		key, _, found := bytes.Cut([]byte(member), []byte(":"))
		if !found || bytes.Count(full, []byte(member)) != 1 {
			test.Fatal("required-member fixture no longer matches canonical bytes", member, string(full))
		}
		removed := bytes.Replace(full, []byte(member+","), nil, 1)
		if bytes.Equal(removed, full) {
			removed = bytes.Replace(full, []byte(","+member), nil, 1)
		}
		fixtures = append(fixtures,
			legacySourceHeaderParityFixture{name: "missing-required/" + string(key), header: removed, failure: ErrIntegrity},
			legacySourceHeaderParityFixture{name: "null-required/" + string(key), header: bytes.Replace(full, []byte(member), append(bytes.Clone(key), []byte(":null")...), 1), failure: ErrIntegrity})
	}
	for _, mutation := range []struct {
		name    string
		before  string
		after   string
		failure error
	}{
		{name: "explicit-zero-context", before: `"context_tokens":17`, after: `"context_tokens":0`},
		{name: "explicit-zero-output", before: `"output_tokens":19`, after: `"output_tokens":0`},
		{name: "explicit-zero-compaction", before: `"compaction_count":3`, after: `"compaction_count":0`},
		{name: "null-context", before: `"context_tokens":17`, after: `"context_tokens":null`},
		{name: "null-output", before: `"output_tokens":19`, after: `"output_tokens":null`},
		{name: "null-compaction", before: `"compaction_count":3`, after: `"compaction_count":null`},
		{name: "decimal-integer", before: `"context_tokens":17`, after: `"context_tokens":17.0`},
		{name: "exponent-integer", before: `"context_tokens":17`, after: `"context_tokens":17e0`},
		{name: "overflow-integer", before: `"context_tokens":17`, after: `"context_tokens":9223372036854775808`},
		{name: "string-integer", before: `"context_tokens":17`, after: `"context_tokens":"17"`},
		{name: "null-models", before: `"source_models":["first","same","same","last"]`, after: `"source_models":null`},
		{name: "empty-models-present", before: `"source_models":["first","same","same","last"]`, after: `"source_models":[]`},
		{name: "null-model-element", before: `"source_models":["first","same","same","last"]`, after: `"source_models":[null,"same","same","last"]`},
		{name: "number-model-element", before: `"source_models":["first","same","same","last"]`, after: `"source_models":[1,"same","same","last"]`},
		{name: "object-models", before: `"source_models":["first","same","same","last"]`, after: `"source_models":{}`},
		{name: "duplicate-same-string", before: `"source_model":"model"`, after: `"source_model":"model","source_model":"model"`},
		{name: "duplicate-last-string", before: `"source_model":"model"`, after: `"source_model":"old","source_model":"model"`},
		{name: "duplicate-null-string", before: `"source_model":"model"`, after: `"source_model":"model","source_model":null`},
		{name: "duplicate-last-models", before: `"source_models":["first","same","same","last"]`, after: `"source_models":["discarded"],"source_models":["first","same","same","last"]`},
		{name: "duplicate-null-models", before: `"source_models":["first","same","same","last"]`, after: `"source_models":["first","same","same","last"],"source_models":null`},
		{name: "duplicate-null-counter", before: `"context_tokens":17`, after: `"context_tokens":17,"CONTEXT_TOKENS":null`},
		{name: "folded-key", before: `"source_model":"model"`, after: `"SOURCE_MODEL":"model"`},
		{name: "unicode-folded-key", before: `"source_models":`, after: `"ſource_modelſ":`},
		{name: "unknown-member", before: `"source_model":"model"`, after: `"source_model":"model","unknown":true`},
		{name: "unknown-nested-member", before: `"source_model":"model"`, after: `"source_model":"model","unknown":{"cir_version":"999","source_models":[null]}`},
		{name: "member-order", before: `"captured_at":"2026-10-26T00:00:00Z","cir_version":"2"`, after: `"cir_version":"2","captured_at":"2026-10-26T00:00:00Z"`},
		{name: "space-after-colon", before: `"source_model":"model"`, after: `"source_model": "model"`},
		{name: "newline-between-members", before: `"source_model":"model",`, after: "\"source_model\":\"model\",\n\t"},
		{name: "escaped-key", before: `"source_model":"model"`, after: `"\u0073ource_model":"model"`},
		{name: "escaped-ascii", before: `"source_model":"model"`, after: `"source_model":"\u006dodel"`},
		{name: "escaped-slash", before: "한🙂/", after: `한🙂\/`},
		{name: "escaped-korean", before: "한🙂", after: `\ud55c🙂`},
		{name: "surrogate-pair", before: "한🙂", after: `한\ud83d\ude42`},
		{name: "unescaped-html", before: `\u003c\u0026\u003e`, after: "<&>"},
		{name: "uppercase-escape", before: `\u003c`, after: `\u003C`},
		{name: "literal-line-separators", before: `\u2028\u2029`, after: "\u2028\u2029"},
		{name: "long-control-escape", before: `\n`, after: `\u000a`},
		{name: "escaped-replacement-rune", before: `"source_model":"model"`, after: `"source_model":"\ufffd"`},
		{name: "lone-surrogate", before: `"source_model":"model"`, after: `"source_model":"\ud800"`},
		{name: "invalid-utf8", before: `"source_model":"model"`, after: "\"source_model\":\"\xff\xc0\xaf\xed\xa0\x80\""},
		{name: "wrong-string-type", before: `"source_model":"model"`, after: `"source_model":[]`},
		{name: "wrong-version-type", before: `"cir_version":"2"`, after: `"cir_version":2`},
		{name: "invalid-escape", before: `"source_model":"model"`, after: `"source_model":"\q"`},
		{name: "unsupported-version", before: `"cir_version":"2"`, after: `"cir_version":"999"`, failure: ErrUnsupportedCIRVersion},
		{name: "unsupported-escaped-version", before: `"cir_version":"2"`, after: `"cir_version":"\u0039\u0039\u0039"`, failure: ErrUnsupportedCIRVersion},
		{name: "unsupported-before-canonicality", before: `"cir_version":"2"`, after: `"cir_version": "999","unknown":null`, failure: ErrUnsupportedCIRVersion},
		{name: "duplicate-last-unsupported", before: `"cir_version":"2"`, after: `"cir_version":"2","cir_version":"999"`, failure: ErrUnsupportedCIRVersion},
		{name: "duplicate-last-supported", before: `"cir_version":"2"`, after: `"cir_version":"999","cir_version":"2"`},
		{name: "type-before-unsupported", before: `"cir_version":"2"`, after: `"cir_version":"999","source_model":[]`},
		{name: "syntax-before-unsupported", before: `"cir_version":"2"`, after: `"cir_version":"999","unknown":"\q"`},
	} {
		if bytes.Count(full, []byte(mutation.before)) != 1 {
			test.Fatal("mutation no longer selects exactly one canonical member", mutation.name, mutation.before)
		}
		failure := mutation.failure
		if failure == nil {
			failure = ErrIntegrity
		}
		fixtures = append(fixtures, legacySourceHeaderParityFixture{name: mutation.name,
			header: bytes.Replace(full, []byte(mutation.before), []byte(mutation.after), 1), failure: failure})
	}
	for _, header := range []string{`{}`, `null`, `[]`} {
		fixtures = append(fixtures, legacySourceHeaderParityFixture{name: "incomplete-envelope/" + header, header: []byte(header), failure: ErrIntegrity})
	}
	return fixtures
}

func legacySourceHeaderParityPlanAndSeed(test *testing.T, proof VerifiedLegacyByteSource, output *legacyMetadataWorkStream, raw, header, stream []byte) {
	test.Helper()
	var document CIRDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		test.Fatal(err)
	}
	original, canonical := legacyDocByteBackfillDocumentFixture(test, document)
	if !bytes.Equal(raw, canonical) || HashContent(raw) != original.Hash() {
		test.Fatal("accepted source changed the original exact canonical bytes/hash")
	}
	plan, err := proof.Plan()
	if err != nil {
		test.Fatal(err)
	}
	wantPlan, wantSpec, wantKey := legacyDocByteBackfillRequirePlan(test, original, stream)
	spec, err := plan.Spec()
	key, keyErr := plan.Key()
	if err != nil || keyErr != nil || spec != wantSpec || key != wantKey || spec.Reference.Hash != HashContent(raw) {
		test.Fatal("header proof changed the original plan identity", spec, wantSpec, key, wantKey, err, keyErr)
	}
	var actualStream []byte
	for ordinal := range spec.Blocks {
		block, err := plan.Block(context.Background(), ordinal)
		want, wantErr := wantPlan.Block(context.Background(), ordinal)
		if err != nil || wantErr != nil || !block.Valid() || block.Reference() != original.DocumentRef() || block.PlanKey() != wantKey ||
			block.Ordinal() != ordinal || block.Descriptor() != want.Descriptor() || !bytes.Equal(block.Bytes(), want.Bytes()) || HashContent(block.Bytes()) != block.Descriptor().Hash {
			test.Fatal("header proof changed original block bytes/hash", ordinal, err, wantErr)
		}
		actualStream = append(actualStream, block.Bytes()...)
	}
	if !bytes.Equal(actualStream, stream) || !bytes.Equal(output.body, stream) || !bytes.Equal(AssembleCanonicalDoc(header, [][]byte{actualStream}), raw) {
		test.Fatal("accepted header no longer reconstructs the exact original source")
	}
	seed, err := proof.Seed()
	if err != nil || seed.Reference() != original.DocumentRef() || seed.PlanKey() != wantKey {
		test.Fatal("header proof changed seed authority", err)
	}
	state, err := seed.NewPreparation(context.Background(), plan)
	if err != nil {
		test.Fatal(err)
	}
	got, saved := legacyMetadataWorkFinish(test, state, plan, output, false, false)
	var expected []DocRangeObject
	want, err := PrepareLegacyDerivedRangeMetadata(context.Background(), original, func(_ context.Context, object DocRangeObject) error {
		expected = append(expected, DocRangeObject{Hash: object.Hash, Bytes: bytes.Clone(object.Bytes)})
		return nil
	})
	if err != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(saved, expected) {
		test.Fatal("source-backed seed changed the original exact metadata bytes/hash", err)
	}
	descriptor, err := got.Descriptor()
	if err != nil || descriptor.Hash != HashContent(raw) || !bytes.Equal(descriptor.Envelope, header) || descriptor.StreamBytes != int64(len(stream)) {
		test.Fatal("seed lost the exact accepted canonical header or original document hash", err)
	}
}

func TestLegacySourceHeaderStrictCanonicalityMatchesOriginalExactOracle(test *testing.T) {
	base, _ := legacySourceProofDocument(test, 2, 32)
	_, stream := chunkVerifierParts(test, base)
	for _, fixture := range legacySourceHeaderParityFixtures(test) {
		test.Run(fixture.name, func(test *testing.T) {
			_, oracleErr := decodeCanonicalCIREnvelope(fixture.header)
			if !errors.Is(oracleErr, fixture.failure) || (oracleErr == nil) != (fixture.failure == nil) {
				test.Fatal("fixture changed the original exact-byte oracle classification", oracleErr, fixture.failure)
			}
			raw := AssembleCanonicalDoc(fixture.header, [][]byte{stream})
			for _, warm := range []bool{false, true} {
				test.Run(fmt.Sprintf("cached-events=%t", warm), func(test *testing.T) {
					input := &legacySourceProofBuffer{body: bytes.Clone(raw)}
					output := &legacyMetadataWorkStream{legacyTestStream: &legacyTestStream{}, test: test}
					var verifier CanonicalDocVerifier
					if warm {
						if _, err := verifier.Verify(context.Background(), HashContent(base), base); err != nil {
							test.Fatal(err)
						}
					}
					cached := maps.Clone(verifier.events)
					preparation, err := NewLegacyByteSourcePreparation(context.Background(), HashContent(raw), input.input(), output, &verifier)
					if err != nil || len(input.reads) != 0 {
						test.Fatal("source proof constructor rejected fixture or eagerly read it", err, len(input.reads))
					}
					var progress LegacyByteSourceProgress
					for steps := 0; ; steps++ {
						if steps == 6000 {
							test.Fatal("source header proof did not terminate")
						}
						if result, resultErr := preparation.Result(); result.Valid() || !errors.Is(resultErr, ErrDocRangeIndexPending) {
							test.Fatal("unfinished header proof exposed publication authority", resultErr)
						}
						progress, err = preparation.Step(context.Background())
						if (err != nil || progress != LegacyByteSourceVerified) && !maps.Equal(verifier.events, cached) {
							test.Fatal("unfinished or rejected header primed the semantic cache", progress, err)
						}
						if err != nil || progress != LegacyByteSourcePending {
							break
						}
					}
					if preparation.NormalizationEligible() {
						test.Fatal("equal raw document hash was confused with normalization eligibility")
					}
					proof, resultErr := preparation.Result()
					if oracleErr != nil {
						if proof.Valid() || !errors.Is(resultErr, ErrDocRangeIndexPending) || len(output.body) != 0 {
							test.Fatal("invalid header published bytes, a plan, or a seed", resultErr, len(output.body))
						}
						if errors.Is(oracleErr, ErrUnsupportedCIRVersion) {
							if err != nil || progress != LegacyByteSourceOriginalRequired {
								test.Fatal("unsupported version lost original-required precedence", progress, err)
							}
						} else if !errors.Is(err, ErrIntegrity) || errors.Is(err, ErrPreparedDocRangeObject) || progress == LegacyByteSourceOriginalRequired {
							test.Fatal("equal raw hash bypassed strict canonical header rejection", progress, err, oracleErr)
						}
						return
					}
					if err != nil || progress != LegacyByteSourceVerified || resultErr != nil || !proof.Valid() || proof.Source() == nil || len(verifier.events) == 0 {
						test.Fatal("exact canonical header failed original source proof", progress, err, resultErr)
					}
					legacySourceHeaderParityPlanAndSeed(test, proof, output, raw, fixture.header, stream)
				})
			}
		})
	}
}

func TestLegacySourceHeaderVersionCannotBorrowCachedV2EventSemantics(test *testing.T) {
	document := CIRDocument{Envelope: CIREnvelope{CIRVersion: CIRVersionV2}, Events: []CIREvent{
		{Kind: EventMessage, Seq: 17, Role: RoleAssistant, AgentMessage: true, Blocks: []ContentBlock{{Type: "text", Text: "cached v2 event"}}},
	}}
	_, base := legacyDocByteBackfillDocumentFixture(test, document)
	_, stream := chunkVerifierParts(test, base)
	for _, version := range []string{"", CIRVersionV1} {
		test.Run(fmt.Sprintf("version=%q", version), func(test *testing.T) {
			header, err := canonicalJSON(CIREnvelope{CIRVersion: version})
			if err != nil {
				test.Fatal(err)
			}
			if _, err := decodeCanonicalCIREnvelope(header); err != nil {
				test.Fatal("supported header fixture changed its exact-byte classification", err)
			}
			raw := AssembleCanonicalDoc(header, [][]byte{stream})
			var oracle, verifier CanonicalDocVerifier
			if proof, err := oracle.Verify(context.Background(), HashContent(raw), raw); !errors.Is(err, ErrUnsupportedCIRVersion) || proof.Valid() {
				test.Fatal("original verifier admitted v2-only event semantics under an older header", err)
			}
			if _, err := verifier.Verify(context.Background(), HashContent(base), base); err != nil || len(verifier.events) == 0 {
				test.Fatal("v2 semantic-cache fixture failed", err)
			}
			cached := maps.Clone(verifier.events)
			input, output := &legacySourceProofBuffer{body: raw}, &legacySourceProofBuffer{}
			preparation, err := NewLegacyByteSourcePreparation(context.Background(), HashContent(raw), input.input(), output, &verifier)
			if err != nil {
				test.Fatal(err)
			}
			progress, err := legacySourceProofDrain(test, preparation)
			proof, resultErr := preparation.Result()
			if err != nil || progress != LegacyByteSourceOriginalRequired || preparation.NormalizationEligible() || proof.Valid() ||
				!errors.Is(resultErr, ErrDocRangeIndexPending) || !maps.Equal(verifier.events, cached) || len(output.body) != 0 {
				test.Fatal("header version borrowed v2 cache authority for an original plan/seed", progress, err, resultErr)
			}
		})
	}
}

func TestLegacySourceHeaderRawHashMismatchKeepsOriginalClassifierBeforeHeaderOracle(test *testing.T) {
	base, original := legacySourceProofDocument(test, 2, 32)
	header, stream := chunkVerifierParts(test, base)
	for _, mutation := range []struct {
		name  string
		value string
	}{
		{name: "duplicate-version", value: `"cir_version":"2","cir_version":"2"`},
		{name: "escaped-version", value: `"cir_version":"\u0032"`},
		{name: "unsupported-version", value: `"cir_version":"999"`},
		{name: "unsupported-and-wrong-type", value: `"cir_version":"999","source_models":{}`},
	} {
		test.Run(mutation.name, func(test *testing.T) {
			changed := bytes.Replace(header, []byte(`"cir_version":"2"`), []byte(mutation.value), 1)
			if _, err := decodeCanonicalCIREnvelope(changed); err == nil {
				test.Fatal("classifier fixture unexpectedly became an exact canonical header")
			}
			raw := AssembleCanonicalDoc(changed, [][]byte{stream})
			if HashContent(raw) == original.Hash() {
				test.Fatal("classifier fixture no longer has a mismatched raw document hash")
			}
			input, output := &legacySourceProofBuffer{body: raw}, &legacySourceProofBuffer{}
			var verifier CanonicalDocVerifier
			preparation, err := NewLegacyByteSourcePreparation(context.Background(), original.Hash(), input.input(), output, &verifier)
			if err != nil {
				test.Fatal(err)
			}
			progress, err := legacySourceProofDrain(test, preparation)
			proof, resultErr := preparation.Result()
			if err != nil || progress != LegacyByteSourceOriginalRequired || !preparation.NormalizationEligible() || proof.Valid() ||
				!errors.Is(resultErr, ErrDocRangeIndexPending) || len(output.body) != 0 || len(verifier.events) != 0 {
				test.Fatal("header oracle displaced the original raw-hash classifier", progress, err, resultErr)
			}
		})
	}
}
