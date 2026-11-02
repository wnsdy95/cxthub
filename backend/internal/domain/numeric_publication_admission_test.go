package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

type numericPublicationMarshaler string

func (value numericPublicationMarshaler) MarshalJSON() ([]byte, error) {
	return []byte(value), nil
}

type numericPublicationMutatingMarshaler struct {
	input map[string]any
}

func (value numericPublicationMutatingMarshaler) MarshalJSON() ([]byte, error) {
	clear(value.input)
	return []byte("1e400"), nil
}

type numericPublicationFixture struct {
	document SessionDoc
	raw      []byte
	event    []byte
}

func numericPublicationToolFixture(kind EventKind, replacement bool, payload any, payloadJSON, metadataToken string) numericPublicationFixture {
	event := CIREvent{Kind: kind, Seq: 1, CallID: "numeric"}
	metadataJSON := ""
	if metadataToken != "" {
		createTime := json.Number(metadataToken)
		event.ProviderMetadata = &ProviderMetadata{CreateTime: &createTime}
		metadataJSON = `,"provider_metadata":{"create_time":` + metadataToken + `}`
	}
	eventJSON := ""
	if kind == EventToolCall {
		event.ToolName = "generic"
		event.Input = map[string]any{"value": payload}
		eventJSON = `{"call_id":"numeric","input":{"value":` + payloadJSON + `},"kind":"tool_call"` + metadataJSON + `,"seq":1,"tool_name":"generic"}`
	} else {
		event.Output = payload
		eventJSON = `{"call_id":"numeric","kind":"tool_result","output":` + payloadJSON + metadataJSON + `,"seq":1}`
	}
	if replacement {
		event = CIREvent{Kind: EventCompaction, Seq: 2, Replacement: []CIREvent{event}, ReplacementComplete: true}
		eventJSON = `{"kind":"compaction","replacement":[` + eventJSON + `],"replacement_complete":true,"seq":2}`
	}
	const envelopeJSON = `{"captured_at":"","cir_version":"2","cwd":"","fidelity":"","git_branch":"","session_origin_id":"","source_model":"","source_provider":""}`
	raw := []byte(`{"envelope":` + envelopeJSON + `,"events":[` + eventJSON + `]}`)
	return numericPublicationFixture{
		document: SessionDoc{
			Hash: HashContent(raw),
			CIR:  CIRDocument{Envelope: CIREnvelope{CIRVersion: "2"}, Events: []CIREvent{event}},
		},
		raw:   raw,
		event: []byte(eventJSON),
	}
}

func numericPublicationWireRoundTrip(raw []byte) ([]byte, error) {
	var document CIRDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var generic any
	if err := decoder.Decode(&generic); err != nil {
		return nil, err
	}
	return json.Marshal(generic)
}

func assertNumericPublicationReadable(test *testing.T, proof VerifiedSessionDoc, fixture numericPublicationFixture) {
	test.Helper()
	if !proof.Valid() || proof.Hash() != fixture.document.Hash ||
		proof.DocumentRef() != (DocumentRef{Hash: fixture.document.Hash}) || !bytes.Equal(proof.Bytes(), fixture.raw) {
		test.Fatal("proof changed the exact legacy bytes or content identity")
	}
	index, err := proof.ReadIndex()
	if err != nil || index.Hash != fixture.document.Hash || !reflect.DeepEqual(index.Envelope, fixture.document.CIR.Envelope) || len(index.Events) != 1 {
		test.Fatalf("ReadIndex = %+v, %v; want one event under the original identity", index, err)
	}
	wantEntry := DocEventIndex{
		Index: 0, Offset: 0, Length: len(fixture.event), Hash: HashContent(fixture.event),
		Seq: fixture.document.CIR.Events[0].Seq,
	}
	if index.Events[0] != wantEntry {
		test.Fatalf("indexed event = %+v; want %+v", index.Events[0], wantEntry)
	}
	stream, err := proof.EventStreamRange(context.Background(), wantEntry.Offset, wantEntry.Length)
	if err != nil || !bytes.Equal(stream, fixture.event) || HashContent(stream) != wantEntry.Hash {
		test.Fatalf("EventStreamRange = %s, %v; want exact event %s", stream, err, fixture.event)
	}
	var decoded CIREvent
	if err := json.Unmarshal(stream, &decoded); err != nil {
		test.Fatal("published numeric event is not readable by encoding/json", err)
	}
	owned := proof.Bytes()
	owned[0] = '!'
	stream[0] = '!'
	if !bytes.Equal(proof.Bytes(), fixture.raw) {
		test.Fatal("returned bytes alias immutable proof content")
	}
	stream, err = proof.EventStreamRange(context.Background(), 0, len(fixture.event))
	if err != nil || !bytes.Equal(stream, fixture.event) {
		test.Fatal("returned event bytes alias immutable proof content", err)
	}
}

func assertNumericPublicationAdmission(test *testing.T, build func() numericPublicationFixture, accepted bool) {
	test.Helper()
	fixture, original := build(), build()
	defer func() {
		if !reflect.DeepEqual(fixture, original) {
			test.Error("numeric admission mutated caller-owned values, numeric types, pointers or bytes")
		}
		canonical, err := CanonicalBytes(fixture.document.CIR)
		if err != nil || !bytes.Equal(canonical, original.raw) || HashContent(canonical) != original.document.Hash {
			test.Error("numeric admission changed legacy canonical bytes or hash", err)
		}
	}()
	canonical, err := CanonicalBytes(fixture.document.CIR)
	if err != nil || !bytes.Equal(canonical, fixture.raw) || HashContent(canonical) != fixture.document.Hash {
		test.Fatalf("CanonicalBytes = %s, %v; legacy bytes must remain %s", canonical, err, fixture.raw)
	}
	roundTrip, roundTripErr := numericPublicationWireRoundTrip(fixture.raw)
	if exact := roundTripErr == nil && bytes.Equal(roundTrip, fixture.raw); exact != accepted {
		test.Fatalf("fixture acceptance %v disagrees with encoding/json round trip: %s, %v", accepted, roundTrip, roundTripErr)
	}
	typed, typedErr := VerifySessionDoc(fixture.document)
	validated, validatedErr := ValidatedSessionDocBytes(fixture.document)
	hashErr := ValidateSessionDocHash(fixture.document)
	var verifier CanonicalDocVerifier
	wireProof, wireErr := verifier.Verify(context.Background(), fixture.document.Hash, fixture.raw)
	stored, storedErr := VerifyStoredDocBytes(fixture.document.Hash, fixture.raw)
	for _, result := range []struct {
		name string
		err  error
	}{
		{"VerifySessionDoc", typedErr},
		{"ValidatedSessionDocBytes", validatedErr},
		{"ValidateSessionDocHash", hashErr},
		{"CanonicalDocVerifier.Verify", wireErr},
		{"VerifyStoredDocBytes", storedErr},
	} {
		if accepted && result.err != nil {
			test.Errorf("%s rejected exact readable numeric bytes: %v", result.name, result.err)
		}
		if !accepted && !errors.Is(result.err, ErrIntegrity) {
			test.Errorf("%s = %v; want ErrIntegrity without normalizing the content identity", result.name, result.err)
		}
	}
	if !accepted {
		if typed.Valid() || typed.Hash() != "" || len(typed.Bytes()) != 0 ||
			wireProof.Valid() || wireProof.Hash() != "" || len(wireProof.Bytes()) != 0 ||
			stored.Valid() || stored.Hash() != "" || len(validated) != 0 {
			test.Error("noncanonical generic numbers minted a proof or publishable bytes")
		}
		return
	}
	if !bytes.Equal(validated, fixture.raw) || HashContent(validated) != fixture.document.Hash {
		test.Error("validated numeric bytes were normalized or rehashed")
	}
	if !stored.Valid() || stored.Hash() != fixture.document.Hash || stored.DocumentRef() != (DocumentRef{Hash: fixture.document.Hash}) {
		test.Error("stored-byte verification changed the numeric content identity")
	}
	test.Run("typed_reads", func(test *testing.T) { assertNumericPublicationReadable(test, typed, fixture) })
	test.Run("wire_reads", func(test *testing.T) { assertNumericPublicationReadable(test, wireProof, fixture) })
}

func assertNumericPublicationLocations(test *testing.T, payload func() any, payloadJSON, metadataToken string, accepted bool) {
	test.Helper()
	for _, kind := range []EventKind{EventToolCall, EventToolResult} {
		for _, replacement := range []bool{false, true} {
			name := string(kind)
			if replacement {
				name += "/replacement"
			}
			test.Run(name, func(test *testing.T) {
				assertNumericPublicationAdmission(test, func() numericPublicationFixture {
					return numericPublicationToolFixture(kind, replacement, payload(), payloadJSON, metadataToken)
				}, accepted)
			})
		}
	}
}

func TestNumericPublicationGenericNumberAdmission(test *testing.T) {
	for _, scenario := range []struct {
		name, token string
		accepted    bool
	}{
		{"positive_overflow", "1e400", false},
		{"negative_overflow", "-1e400", false},
		{"underflow", "1e-400", false},
		{"inexact_integer", "9007199254740993", false},
		{"decimal_integer", "1.0", false},
		{"uppercase_exponent", "1E+3", false},
		{"missing_exponent_sign", "1e21", false},
		{"zero", "0", true},
		{"negative_zero", "-0", true},
		{"small_exponent", "1e-7", true},
		{"large_exponent", "1e+21", true},
		{"max_finite", "1.7976931348623157e+308", true},
	} {
		for _, representation := range []string{"number", "number_pointer", "nested_map_slice", "raw_message", "custom_marshaler"} {
			test.Run(scenario.name+"/"+representation, func(test *testing.T) {
				payloadJSON := scenario.token
				if representation == "nested_map_slice" {
					payloadJSON = `{"nested":[{"value":` + scenario.token + `},` + scenario.token + `]}`
				}
				assertNumericPublicationLocations(test, func() any {
					number := json.Number(scenario.token)
					switch representation {
					case "number":
						return number
					case "number_pointer":
						return &number
					case "nested_map_slice":
						return map[string]any{"nested": []any{map[string]any{"value": &number}, number}}
					case "raw_message":
						return json.RawMessage(scenario.token)
					default:
						return numericPublicationMarshaler(scenario.token)
					}
				}, payloadJSON, "", scenario.accepted)
			})
		}
	}
}

func TestNumericPublicationIntegerAdmission(test *testing.T) {
	for _, scenario := range []struct {
		name, token string
		value       any
		accepted    bool
	}{
		{"int64_inexact", "9007199254740993", int64(9007199254740993), false},
		{"uint64_inexact", "9007199254740993", uint64(9007199254740993), false},
		{"int64_min", "-9223372036854775808", int64(-9223372036854775808), false},
		{"int64_max", "9223372036854775807", int64(9223372036854775807), false},
		{"uint64_max", "18446744073709551615", uint64(18446744073709551615), false},
		{"int64_exact_large", "9007199254740994", int64(9007199254740994), true},
		{"uint64_exact_large", "9007199254740994", uint64(9007199254740994), true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			assertNumericPublicationLocations(test, func() any { return scenario.value }, scenario.token, "", scenario.accepted)
		})
	}
}

func TestNumericPublicationProviderCreateTimeSemantics(test *testing.T) {
	for _, metadataToken := range []string{"1E+3", "9007199254740993", "1e400"} {
		for _, payloadToken := range []string{"1e-7", "1.0"} {
			test.Run(metadataToken+"/generic_"+payloadToken, func(test *testing.T) {
				assertNumericPublicationLocations(test, func() any { return json.Number(payloadToken) }, payloadToken, metadataToken, payloadToken == "1e-7")
			})
		}
	}
}

func TestNumericPublicationNullablePayloadAdmission(test *testing.T) {
	for _, scenario := range []struct {
		name    string
		payload func() any
	}{
		{"nil_number_pointer", func() any { return (*json.Number)(nil) }},
		{"nil_integer_pointer", func() any { return (*int64)(nil) }},
		{"raw_null", func() any { return json.RawMessage(`null`) }},
		{"custom_null", func() any { return numericPublicationMarshaler(`null`) }},
	} {
		for _, kind := range []EventKind{EventToolCall, EventToolResult} {
			for _, replacement := range []bool{false, true} {
				name := scenario.name + "/" + string(kind)
				if replacement {
					name += "/replacement"
				}
				test.Run(name, func(test *testing.T) {
					assertNumericPublicationAdmission(test, func() numericPublicationFixture {
						return numericPublicationToolFixture(kind, replacement, scenario.payload(), "null", "")
					}, kind == EventToolCall)
				})
			}
		}
	}
}

func TestNumericPublicationProofOwnsNumericInput(test *testing.T) {
	number := json.Number("1e-7")
	rawNumber := json.RawMessage(`1e+21`)
	shared := map[string]any{"number": &number, "raw": rawNumber}
	payload := []any{shared}
	fixture := numericPublicationToolFixture(EventToolResult, true, payload, `[{"number":1e-7,"raw":1e+21}]`, "1E+3")
	expected := numericPublicationToolFixture(EventToolResult, true,
		[]any{map[string]any{"number": json.Number("1e-7"), "raw": json.Number("1e+21")}},
		`[{"number":1e-7,"raw":1e+21}]`, "1E+3")
	typed, err := VerifySessionDoc(fixture.document)
	if err != nil {
		test.Fatal(err)
	}
	var verifier CanonicalDocVerifier
	wireProof, err := verifier.Verify(context.Background(), fixture.document.Hash, fixture.raw)
	if err != nil {
		test.Fatal(err)
	}
	stored, err := VerifyStoredDocBytes(fixture.document.Hash, fixture.raw)
	if err != nil {
		test.Fatal(err)
	}
	if number != "1e-7" || !bytes.Equal(rawNumber, []byte(`1e+21`)) || shared["number"] != &number || payload[0] == nil {
		test.Fatal("verification changed numeric values or aliases")
	}
	number = "1.0"
	rawNumber[0] = '9'
	shared["number"] = json.Number("1e400")
	payload[0] = "changed"
	*fixture.document.CIR.Events[0].Replacement[0].ProviderMetadata.CreateTime = json.Number("0")
	fixture.document.CIR.Events[0].Replacement[0].Output = "changed"
	fixture.raw[0] = '!'
	assertNumericPublicationReadable(test, typed, expected)
	assertNumericPublicationReadable(test, wireProof, expected)
	if !stored.Valid() || stored.Hash() != expected.document.Hash {
		test.Fatal("caller mutation changed the stored numeric content identity")
	}
}

func TestNumericPublicationMarshalerCannotEraseRequiredValidation(test *testing.T) {
	for _, replacement := range []bool{false, true} {
		for _, operation := range []string{"proof", "bytes", "hash"} {
			name := operation
			if replacement {
				name += "/replacement"
			}
			test.Run(name, func(test *testing.T) {
				fixture := numericPublicationToolFixture(EventToolCall, replacement, nil, "1e400", "")
				event := &fixture.document.CIR.Events[0]
				if replacement {
					event = &event.Replacement[0]
				}
				event.Input["value"] = numericPublicationMutatingMarshaler{input: event.Input}
				var err error
				switch operation {
				case "proof":
					var proof VerifiedSessionDoc
					proof, err = VerifySessionDoc(fixture.document)
					if proof.Valid() || proof.Hash() != "" {
						test.Error("mutating custom marshaler minted a publication proof")
					}
				case "bytes":
					var raw []byte
					raw, err = ValidatedSessionDocBytes(fixture.document)
					if len(raw) != 0 {
						test.Error("mutating custom marshaler returned publishable bytes")
					}
				default:
					err = ValidateSessionDocHash(fixture.document)
				}
				if !errors.Is(err, ErrIntegrity) || len(event.Input) != 0 {
					test.Fatalf("custom marshaler did not exercise the rejected mutation: input=%v err=%v", event.Input, err)
				}
			})
		}
	}
}
