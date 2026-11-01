//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type legacyNormalizationDecoderTestSource struct {
	body  []byte
	parts []domain.ConversationManifestChunk
}

func newLegacyNormalizationDecoderTestSource(raw []byte) *legacyNormalizationDecoderTestSource {
	source := &legacyNormalizationDecoderTestSource{body: bytes.Clone(raw)}
	for offset := 0; offset < len(source.body); offset += domain.ConversationManifestChunkBytes {
		part := source.body[offset:min(offset+domain.ConversationManifestChunkBytes, len(source.body))]
		source.parts = append(source.parts, domain.ConversationManifestChunk{Hash: domain.HashContent(part), Bytes: int64(len(part))})
	}
	return source
}

func (source *legacyNormalizationDecoderTestSource) Read(ctx context.Context, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if offset < 0 || length < 0 || offset > len(source.body) {
		return nil, domain.ErrIntegrity
	}
	if length > len(source.body)-offset {
		return nil, io.EOF
	}
	return source.body[offset : offset+length], nil
}

func (source *legacyNormalizationDecoderTestSource) input() domain.LegacyByteSource {
	return domain.LegacyByteSource{
		Bytes: len(source.body),
		Parts: append([]domain.ConversationManifestChunk(nil), source.parts...),
		Read:  source.Read,
	}
}

func legacyNormalizationDecoderTestBytes(ctx context.Context, event domain.LegacyNormalizedEvent) ([]byte, error) {
	if event.Encoding == nil {
		return nil, fmt.Errorf("normalization event has no canonical encoding")
	}
	var body []byte
	for chunkIndex := 0; chunkIndex < 1<<20; chunkIndex++ {
		chunk, done, err := event.Encoding.Next(ctx)
		if err != nil {
			return nil, err
		}
		if len(chunk) > 64<<10 {
			return nil, fmt.Errorf("normalization encoding chunk exceeds 64 KiB: %d", len(chunk))
		}
		body = append(body, chunk...)
		if done {
			return body, nil
		}
	}
	return nil, fmt.Errorf("normalization encoding made no bounded progress")
}

func legacyNormalizationDecoderTestProof(test testing.TB, ctx context.Context, claimed domain.ContentHash, raw []byte) (domain.VerifiedSessionDoc, error) {
	test.Helper()
	source := newLegacyNormalizationDecoderTestSource(raw)
	defer func() {
		if !bytes.Equal(source.body, raw) {
			test.Fatal("normalization decoder modified borrowed source bytes")
		}
	}()
	decoder, err := domain.NewLegacyNormalizationDecoder(ctx, source.input())
	if err != nil {
		return domain.VerifiedSessionDoc{}, err
	}
	type materializedEvent struct {
		Sequence int
		Bytes    []byte
	}
	var selected []materializedEvent
	for stepIndex := 0; stepIndex < len(raw)+len(source.parts)+16; stepIndex++ {
		step, err := decoder.Step(ctx)
		if err != nil {
			return domain.VerifiedSessionDoc{}, err
		}
		if step.Reset {
			selected = nil
		}
		if len(step.Events) > 64 {
			test.Fatalf("normalization decoder returned %d events in one Step", len(step.Events))
		}
		for _, event := range step.Events {
			body, err := legacyNormalizationDecoderTestBytes(ctx, event)
			if err != nil {
				return domain.VerifiedSessionDoc{}, err
			}
			selected = append(selected, materializedEvent{Sequence: event.Sequence, Bytes: body})
		}
		if !step.Complete {
			continue
		}
		if step.Count != len(selected) || len(step.Envelope) == 0 {
			test.Fatalf("completed normalization has count=%d selected=%d envelope=%q", step.Count, len(selected), step.Envelope)
		}
		if err := ctx.Err(); err != nil {
			return domain.VerifiedSessionDoc{}, err
		}
		sort.SliceStable(selected, func(left, right int) bool {
			return selected[left].Sequence < selected[right].Sequence
		})
		var composed bytes.Buffer
		composed.WriteString(`{"envelope":`)
		composed.Write(step.Envelope)
		composed.WriteString(`,"events":[`)
		for ordinal, event := range selected {
			var decoded domain.CIREvent
			if err := json.Unmarshal(event.Bytes, &decoded); err != nil {
				return domain.VerifiedSessionDoc{}, domain.ErrIntegrity
			}
			if decoded.Seq != event.Sequence {
				test.Fatalf("event descriptor sequence=%d differs from original typed fragment sequence=%d", event.Sequence, decoded.Seq)
			}
			if ordinal != 0 {
				composed.WriteByte(',')
			}
			composed.Write(event.Bytes)
		}
		composed.WriteString(`]}`)
		var document domain.CIRDocument
		if err := json.Unmarshal(composed.Bytes(), &document); err != nil {
			return domain.VerifiedSessionDoc{}, domain.ErrIntegrity
		}
		proof, err := domain.VerifySessionDoc(domain.SessionDoc{Hash: claimed, CIR: document})
		if err != nil {
			return domain.VerifiedSessionDoc{}, err
		}
		if !proof.Valid() || proof.Hash() != claimed || domain.HashContent(composed.Bytes()) != claimed || !bytes.Equal(proof.Bytes(), composed.Bytes()) {
			test.Fatal("provisional normalization fragments differ from original verified canonical bytes")
		}
		if err := ctx.Err(); err != nil {
			return domain.VerifiedSessionDoc{}, err
		}
		return proof, nil
	}
	test.Fatal("normalization decoder did not make bounded progress")
	return domain.VerifiedSessionDoc{}, domain.ErrIntegrity
}

func checkLegacyNormalizationDecoderParity(test testing.TB, ctx context.Context, claimed domain.ContentHash, raw []byte, reference domain.VerifiedSessionDoc, referenceErr error) {
	test.Helper()
	proof, err := legacyNormalizationDecoderTestProof(test, ctx, claimed, raw)
	if (err == nil) != (referenceErr == nil) {
		test.Fatalf("normalization decoder acceptance differs from original monolithic producer: decoder=%v original=%v", err, referenceErr)
	}
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded, domain.ErrIntegrity, domain.ErrUnsupportedCIRVersion, domain.ErrDocumentResourceLimit} {
		if errors.Is(err, sentinel) != errors.Is(referenceErr, sentinel) {
			test.Fatalf("normalization decoder error differs from original monolithic producer for %v: decoder=%v original=%v", sentinel, err, referenceErr)
		}
	}
	if err != nil {
		if proof.Valid() || proof.Reference().Valid() {
			test.Fatal("rejected normalization returned proof authority")
		}
		return
	}
	if !proof.Valid() || !reference.Valid() || proof.Hash() != reference.Hash() || proof.DocumentRef() != reference.DocumentRef() ||
		proof.Reference() != reference.Reference() || !bytes.Equal(proof.Bytes(), reference.Bytes()) {
		test.Fatal("completed normalization differs from original monolithic proof identity or canonical bytes")
	}
}

func TestPostgresLegacyNormalizationDecoderAdditionalAcceptedContracts(test *testing.T) {
	versionOneTurn := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1"},
		Events:   []domain.CIREvent{{Kind: domain.EventTurn, Seq: 1, Role: domain.RoleUser}},
	}
	fixtures := []struct {
		name     string
		raw      string
		expected domain.CIRDocument
	}{
		{
			name:     "root-null",
			raw:      " \nnull\t ",
			expected: domain.CIRDocument{},
		},
		{
			name:     "missing-root-fields",
			raw:      `{}`,
			expected: domain.CIRDocument{},
		},
		{
			name:     "unknown-overflow-number-is-syntax-only",
			raw:      `{"ignored":1e9999,"envelope":{"cir_version":"1","ignored":1e9999},"events":[{"kind":"turn","seq":1,"role":"user","ignored":1e9999}]}`,
			expected: versionOneTurn,
		},
		{
			name:     "escaped-case-folded-root-envelope-and-event-fields",
			raw:      `{"\u0045NVELOPE":{"CIR_VER\u0053ION":"1"},"EVENT\u0053":[{"KIND":"turn","SEQ":1,"ROLE":"user"}]}`,
			expected: versionOneTurn,
		},
		{
			name:     "unicode-folded-root-events",
			raw:      `{"envelope":{"cir_version":"1"},"event\u017f":[{"kind":"turn","seq":1,"role":"user"}]}`,
			expected: versionOneTurn,
		},
		{
			name: "folded-optional-fields-do-not-acquire-exact-presence",
			raw:  `{"ENVELOPE":{"CIR_VERSION":"1"},"EVENTS":[{"KIND":"message","SEQ":1,"ROLE":"user","AGENT_MESSAGE":false,"AGENT_AUTHOR":""}]}`,
			expected: domain.CIRDocument{
				Envelope: domain.CIREnvelope{CIRVersion: "1"},
				Events:   []domain.CIREvent{{Kind: domain.EventMessage, Seq: 1, Role: domain.RoleUser, Blocks: []domain.ContentBlock{}}},
			},
		},
		{
			name:     "overwritten-null-event-does-not-fail-final-union",
			raw:      `{"envelope":{"cir_version":"1"},"events":[null],"events":[{"kind":"turn","seq":1,"role":"user"}]}`,
			expected: versionOneTurn,
		},
		{
			name: "null-array-discards-earlier-union-error",
			raw:  `{"envelope":{"cir_version":"1"},"events":[{"kind":"unknown","seq":9}],"events":null}`,
			expected: domain.CIRDocument{
				Envelope: domain.CIREnvelope{CIRVersion: "1"},
			},
		},
		{
			name: "final-envelope-controls-retained-v2-events",
			raw:  `{"envelope":{"cir_version":"1"},"events":[{"kind":"compaction","seq":1,"replacement":[],"replacement_complete":true}],"envelope":{"cir_version":"2"}}`,
			expected: domain.CIRDocument{
				Envelope: domain.CIREnvelope{CIRVersion: "2"},
				Events:   []domain.CIREvent{{Kind: domain.EventCompaction, Seq: 1, Replacement: []domain.CIREvent{}, ReplacementComplete: true}},
			},
		},
		{
			name:     "overwritten-unsupported-envelope-version-is-not-a-type-error",
			raw:      `{"envelope":{"cir_version":"99"},"events":[{"kind":"turn","seq":1,"role":"user"}],"envelope":{"cir_version":"1"}}`,
			expected: versionOneTurn,
		},
		{
			name: "invalid-utf8-normalizes-through-original-decoder",
			raw:  "{\"envelope\":{\"cir_version\":\"1\"},\"events\":[{\"kind\":\"turn\",\"seq\":1,\"role\":\"user\",\"id\":\"\xff\"}]}",
			expected: domain.CIRDocument{
				Envelope: domain.CIREnvelope{CIRVersion: "1"},
				Events:   []domain.CIREvent{{Kind: domain.EventTurn, Seq: 1, Role: domain.RoleUser, ID: "\ufffd"}},
			},
		},
	}
	var storage PostgresStore
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			proof := legacyMonolithicVerifierProof(test, fixture.expected)
			raw := []byte(fixture.raw)
			if domain.HashContent(raw) == proof.Hash() {
				test.Fatal("literal fixture must require typed normalization")
			}
			for _, testCase := range []legacyMonolithicVerifierCase{
				{name: "normalized-hash", raw: raw, hash: proof.Hash(), expected: proof},
				{name: "raw-hash", raw: raw, hash: domain.HashContent(raw), wantErr: domain.ErrIntegrity},
			} {
				test.Run(testCase.name, func(test *testing.T) {
					checkLegacyMonolithicVerifierCase(test, &storage, testCase)
				})
			}
		})
	}
}

func TestPostgresLegacyNormalizationDecoderAdditionalRejectedContracts(test *testing.T) {
	versionOneTurn := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1"},
		Events:   []domain.CIREvent{{Kind: domain.EventTurn, Seq: 1, Role: domain.RoleUser}},
	}
	fixtures := []struct {
		name    string
		raw     string
		claimed domain.CIRDocument
	}{
		{name: "empty-source", raw: ""},
		{name: "non-null-scalar-root", raw: "0"},
		{name: "array-root", raw: "[]"},
		{name: "root-null-with-trailing-value", raw: "null true"},
		{
			name:    "missing-root-close-after-valid-events",
			raw:     `{"envelope":{"cir_version":"1"},"events":[{"kind":"turn","seq":1,"role":"user"}]`,
			claimed: versionOneTurn,
		},
		{
			name:    "missing-events-and-root-close-after-valid-event",
			raw:     `{"envelope":{"cir_version":"1"},"events":[{"kind":"turn","seq":1,"role":"user"}`,
			claimed: versionOneTurn,
		},
		{
			name:    "trailing-root-value",
			raw:     `{"envelope":{"cir_version":"1"},"events":[{"kind":"turn","seq":1,"role":"user"}]} {}`,
			claimed: versionOneTurn,
		},
		{
			name:    "root-trailing-comma",
			raw:     `{"envelope":{"cir_version":"1"},"events":[{"kind":"turn","seq":1,"role":"user"}],}`,
			claimed: versionOneTurn,
		},
		{
			name:    "events-trailing-comma",
			raw:     `{"envelope":{"cir_version":"1"},"events":[{"kind":"turn","seq":1,"role":"user"},]}`,
			claimed: versionOneTurn,
		},
		{
			name:    "late-invalid-ignored-literal",
			raw:     `{"envelope":{"cir_version":"1"},"events":[{"kind":"turn","seq":1,"role":"user"}],"ignored":tru}`,
			claimed: versionOneTurn,
		},
		{
			name:    "overwritten-events-container-type-error",
			raw:     `{"envelope":{"cir_version":"1"},"events":"invalid","events":[{"kind":"turn","seq":1,"role":"user"}]}`,
			claimed: versionOneTurn,
		},
		{
			name:    "overwritten-envelope-container-type-error",
			raw:     `{"envelope":[],"envelope":{"cir_version":"1"},"events":[{"kind":"turn","seq":1,"role":"user"}]}`,
			claimed: versionOneTurn,
		},
		{
			name:    "escaped-exact-presence-is-not-folded-presence",
			raw:     `{"envelope":{"cir_version":"1"},"events":[{"kind":"turn","seq":1,"role":"user","\u0061gent_message":false}]}`,
			claimed: versionOneTurn,
		},
		{
			name:    "null-array-does-not-clear-earlier-typed-error",
			raw:     `{"envelope":{"cir_version":"1"},"events":[{"kind":"turn","seq":"invalid","role":"user"}],"events":null}`,
			claimed: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1"}},
		},
	}
	var storage PostgresStore
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			proof := legacyMonolithicVerifierProof(test, fixture.claimed)
			raw := []byte(fixture.raw)
			for _, testCase := range []legacyMonolithicVerifierCase{
				{name: "normalized-hash", raw: raw, hash: proof.Hash(), wantErr: domain.ErrIntegrity},
				{name: "raw-hash", raw: raw, hash: domain.HashContent(raw), wantErr: domain.ErrIntegrity},
			} {
				test.Run(testCase.name, func(test *testing.T) {
					checkLegacyMonolithicVerifierCase(test, &storage, testCase)
				})
			}
		})
	}
}

func TestPostgresLegacyNormalizationDecoderFullWrapperDepth(test *testing.T) {
	proof := legacyMonolithicVerifierProof(test, domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1"},
		Events:   []domain.CIREvent{{Kind: domain.EventTurn, Seq: 1, Role: domain.RoleUser}},
	})
	fixtures := []struct {
		name     string
		nesting  int
		inEvent  bool
		accepted bool
	}{
		{name: "root-value-at-limit", nesting: 9999, accepted: true},
		{name: "root-value-over-limit", nesting: 10000},
		{name: "event-value-at-limit", nesting: 9997, inEvent: true, accepted: true},
		{name: "event-value-over-limit", nesting: 9998, inEvent: true},
	}
	var storage PostgresStore
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			nested := strings.Repeat("[", fixture.nesting) + "0" + strings.Repeat("]", fixture.nesting)
			raw := []byte(`{"envelope":{"cir_version":"1"},"events":[{"kind":"turn","seq":1,"role":"user"}],"ignored":` + nested + `}`)
			if fixture.inEvent {
				raw = []byte(`{"envelope":{"cir_version":"1"},"events":[{"kind":"turn","seq":1,"role":"user","ignored":` + nested + `}]}`)
			}
			normalized := legacyMonolithicVerifierCase{name: "normalized-hash", raw: raw, hash: proof.Hash(), wantErr: domain.ErrIntegrity}
			if fixture.accepted {
				normalized.expected, normalized.wantErr = proof, nil
			}
			for _, testCase := range []legacyMonolithicVerifierCase{
				normalized,
				{name: "raw-hash", raw: raw, hash: domain.HashContent(raw), wantErr: domain.ErrIntegrity},
			} {
				test.Run(testCase.name, func(test *testing.T) {
					checkLegacyMonolithicVerifierCase(test, &storage, testCase)
				})
			}
		})
	}
}

func TestPostgresLegacyNormalizationDecoderSelectedArrayAcrossSteps(test *testing.T) {
	var discarded, retained []string
	for range 129 {
		discarded = append(discarded, `{"kind":"unknown","seq":0}`)
	}
	document := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1"}}
	for ordinal := range 193 {
		sequence := ordinal*37%23 - 11
		identifier := fmt.Sprintf("selected-%d", ordinal)
		document.Events = append(document.Events, domain.CIREvent{Kind: domain.EventTurn, ID: identifier, Seq: sequence, Role: domain.RoleUser})
		retained = append(retained, fmt.Sprintf(`{"kind":"turn","id":%q,"seq":%d,"role":"user"}`, identifier, sequence))
	}
	raw := []byte(`{"envelope":{"cir_version":"1"},"events":[` + strings.Join(discarded, ",") + `],"events":[` + strings.Join(retained, ",") + `]}`)
	proof := legacyMonolithicVerifierProof(test, document)
	var storage PostgresStore
	for _, testCase := range []legacyMonolithicVerifierCase{
		{name: "normalized-hash", raw: raw, hash: proof.Hash(), expected: proof},
		{name: "raw-hash", raw: raw, hash: domain.HashContent(raw), wantErr: domain.ErrIntegrity},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			checkLegacyMonolithicVerifierCase(test, &storage, testCase)
		})
	}
}
