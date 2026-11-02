package domain_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/json"
	"errors"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docFieldReviewCanonical(test *testing.T, state domain.DocFieldDiscoveryState) []byte {
	test.Helper()
	encoded, err := json.Marshal(state)
	if err != nil {
		test.Fatal(err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &members); err != nil {
		test.Fatal(err)
	}
	canonical, err := json.Marshal(members)
	if err != nil {
		test.Fatal(err)
	}
	return canonical
}

func docFieldReviewAdvance(test *testing.T, prior domain.DocFieldDiscoveryState, chunk []byte) domain.DocFieldDiscoveryState {
	test.Helper()
	before, err := prior.Encode()
	if err != nil {
		test.Fatal(err)
	}
	step, err := domain.AdvanceDocFieldDiscovery(context.Background(), prior, chunk)
	if err != nil {
		test.Fatalf("advance from=%d bytes=%d: %v", prior.Next, len(chunk), err)
	}
	after, err := prior.Encode()
	if err != nil || !bytes.Equal(before, after) {
		test.Fatal("advancement mutated the prior checkpoint", err)
	}
	wantPrevious := domain.ContentHash("")
	if prior.Next > 0 {
		wantPrevious = domain.HashContent(before)
	}
	if step.From() != prior.Next || step.Previous() != wantPrevious {
		test.Fatal("sealed step lost its predecessor", step.From(), step.Previous())
	}
	checkpointHash, checkpoint := step.Checkpoint()
	decoded, err := domain.DecodeDocFieldDiscoveryState(checkpointHash, checkpoint)
	if err != nil || decoded.Next != prior.Next+len(chunk) {
		test.Fatal("checkpoint failed to resume at the exact byte", decoded.Next, err)
	}
	sealed, err := step.State()
	if err != nil || !reflect.DeepEqual(sealed, decoded) {
		test.Fatal("sealed and persisted states differ", err)
	}
	if !bytes.Equal(checkpoint, docFieldReviewCanonical(test, decoded)) {
		test.Fatal("independent canonical encoding differs")
	}
	checkpoint[0] ^= 1
	checkpointHashAgain, checkpointAgain := step.Checkpoint()
	if checkpointHashAgain != checkpointHash || domain.HashContent(checkpointAgain) != checkpointHash {
		test.Fatal("checkpoint caller can mutate the sealed step")
	}
	decoded.HashState = bytes.Clone(decoded.HashState)
	decoded.Stack = bytes.Clone(decoded.Stack)
	return decoded
}

func docFieldReviewDiscover(test *testing.T, raw []byte) domain.DocFieldDiscoveryState {
	test.Helper()
	state, err := domain.NewDocFieldDiscoveryState(domain.HashContent(raw), len(raw))
	if err != nil {
		test.Fatal(err)
	}
	for !state.Done {
		state = docFieldReviewAdvance(test, state, raw[state.Next:min(state.Next+domain.DocFieldDiscoveryStepBytes, len(raw))])
	}
	if state.Digest != domain.HashContent(raw) || len(state.HashState) != 0 {
		test.Fatal("terminal state did not verify the whole source")
	}
	return state
}

func docFieldReviewDisplayOracle(test *testing.T, raw []byte) (domain.DocDisplayStringSource, error) {
	test.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		test.Fatal("invalid oracle fixture", opening, err)
	}
	var kind string
	var output json.RawMessage
	var span domain.DocFindSpan
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			test.Fatal(err)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			test.Fatal(err)
		}
		switch key {
		case "kind":
			kind = ""
			if len(value) > 0 && value[0] == '"' {
				if err := json.Unmarshal(value, &kind); err != nil {
					test.Fatal(err)
				}
			}
		case "output":
			output = bytes.Clone(value)
			span.To = int(decoder.InputOffset())
			span.From = span.To - len(value)
		}
	}
	if _, err := decoder.Token(); err != nil {
		test.Fatal(err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		test.Fatal("oracle did not consume the whole source", err)
	}
	if kind != string(domain.EventToolResult) || len(output) == 0 || output[0] != '"' {
		return domain.DocDisplayStringSource{}, domain.ErrDocFindDisplayUnavailable
	}
	var text string
	if err := json.Unmarshal(output, &text); err != nil {
		test.Fatal(err)
	}
	whitespace := "\t\n\v\f\r \u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff"
	return domain.DocDisplayStringSource{
		Version: 1, Profile: domain.DocDisplayStringProfile,
		EventHash: domain.HashContent(raw), EventBytes: len(raw), Token: span,
		Empty: strings.Trim(text, whitespace) == "",
	}, nil
}

func TestDocFieldDiscoveryReviewBoundaryOracles(test *testing.T) {
	for _, scenario := range []struct {
		name string
		head string
		tail string
	}{
		{
			name: "escaped_duplicate_key",
			head: `{"kind":"message","output":"same","out\u00`,
			tail: `70ut" : "same","locked":{"output":"same"},"kind":"tool_result"}`,
		},
		{
			name: "four_byte_utf8",
			head: `{"kind":"tool_result","output":"same","output":"` + "\xf0\x9f",
			tail: "\x99\x82" + `","locked":{"output":"🙂"}}`,
		},
		{
			name: "surrogate_pair_escape",
			head: `{"kind":"tool_result","output":"same","output":"\ud83d\`,
			tail: `ude42","locked":{"kind":"reasoning","output":"🙂"}}`,
		},
		{
			name: "number_override_exponent_sign",
			head: `{"kind":"tool_result","output":"same","output":-12.3e-`,
			tail: `007,"output":"same","locked":{"output":"same"}}`,
		},
		{
			name: "nested_container_after_invalid_blocks",
			head: `{"kind":"reasoning","blocks":false,"redacted_summary":{},"kind":"message","blocks":[{"text":"same","opaque":`,
			tail: `{"blocks":[],"kind":"tool_result","output":"same"}}],"output":"same"}`,
		},
		{
			name: "null_override_literal",
			head: `{"kind":"reasoning","redacted_summary":"same","redacted_summary":nu`,
			tail: `ll,"locked":{"redacted_summary":"same"},"blocks":false}`,
		},
		{
			name: "whitespace_replaces_nonempty",
			head: `{"kind":"tool_result","output":"same","output":" \u20`,
			tail: `2f\ufeff","locked":{"output":"same"}}`,
		},
		{
			name: "nonstring_replaces_output",
			head: `{"kind":"tool_result","output":"same","output":{`,
			tail: `"output":"same","kind":"tool_result"}}`,
		},
		{
			name: "nonstring_replaces_kind",
			head: `{"kind":"tool_result","output":"same","kind":fa`,
			tail: `lse,"locked":{"kind":"tool_result","output":"same"}}`,
		},
		{
			name: "nonwhitespace_nel_replaces_whitespace",
			head: `{"kind":"tool_result","output":" \ufeff","output":"` + "\xc2",
			tail: "\x85" + `","locked":{"output":" \ufeff"}}`,
		},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			raw := []byte(strings.Repeat(" ", domain.DocFieldDiscoveryStepBytes-len(scenario.head)) + scenario.head + scenario.tail)
			state := docFieldReviewDiscover(test, raw)
			wantFields, wantFieldsErr := domain.BuildDocFindFields(domain.HashContent(raw), raw)
			gotFields, gotFieldsErr := state.FindFields()
			if !reflect.DeepEqual(gotFields, wantFields) || !errors.Is(gotFieldsErr, wantFieldsErr) {
				test.Fatal("typed-field oracle differs", gotFields, gotFieldsErr, wantFields, wantFieldsErr)
			}
			wantSource, wantSourceErr := docFieldReviewDisplayOracle(test, raw)
			gotSource, gotSourceErr := state.DisplaySource()
			if gotSource != wantSource || !errors.Is(gotSourceErr, wantSourceErr) {
				test.Fatal("independent RawMessage span oracle differs", gotSource, gotSourceErr, wantSource, wantSourceErr)
			}
			readerSource, readerErr := domain.BuildDocDisplayStringSourceReader(context.Background(), domain.HashContent(raw), len(raw), bytes.NewReader(raw))
			if readerSource != wantSource || !errors.Is(readerErr, wantSourceErr) {
				test.Fatal("reader differs from independent display oracle", readerSource, readerErr, wantSource, wantSourceErr)
			}
		})
	}
}

func TestDocFieldDiscoveryReviewRejectsHashCounterRestart(test *testing.T) {
	const body = `{"kind":"reasoning","redacted_summary":"visible"}`
	raw := []byte(strings.Repeat(" ", sha256.BlockSize) + body)
	initial, err := domain.NewDocFieldDiscoveryState(domain.HashContent([]byte(body)), len(raw))
	if err != nil {
		test.Fatal(err)
	}
	prior := docFieldReviewAdvance(test, initial, raw[:sha256.BlockSize])
	prior.HashState = bytes.Clone(initial.HashState)
	checkpoint := docFieldReviewCanonical(test, prior)
	decoded, err := domain.DecodeDocFieldDiscoveryState(domain.HashContent(checkpoint), checkpoint)
	if errors.Is(err, domain.ErrIntegrity) {
		return
	}
	if err != nil {
		test.Fatal(err)
	}
	step, advanceErr := domain.AdvanceDocFieldDiscovery(context.Background(), decoded, []byte(body))
	if advanceErr != nil {
		test.Fatalf("decoder accepted Next=64 with SHA counter=0; resume failed later: %v", advanceErr)
	}
	finished, finishErr := step.State()
	fields, fieldsErr := finished.FindFields()
	_, oracleErr := domain.BuildDocFindFields(finished.EventHash, raw)
	test.Fatalf("SHA counter restart accepted: finish=%v fields=%+v/%v full-source oracle=%v declared=%s actual=%s", finishErr, fields, fieldsErr, oracleErr, finished.EventHash, domain.HashContent(raw))
}

func TestDocFieldDiscoveryReviewRejectsMalformedBoundaryContinuation(test *testing.T) {
	for _, scenario := range []struct {
		name         string
		head         string
		tail         string
		rejectPrefix bool
	}{
		{
			name: "three_byte_overlong_in_opaque_value",
			head: `{"kind":"tool_result","output":"visible","opaque":"` + "\xe0",
			tail: "\x80\xaf" + `"}`,
		},
		{
			name:         "four_byte_overlong_in_opaque_key",
			head:         `{"kind":"tool_result","output":"visible","` + "\xf0\x80",
			tail:         "\x80\xaf" + `":null}`,
			rejectPrefix: true,
		},
		{
			name:         "raw_surrogate_in_opaque_value",
			head:         `{"kind":"tool_result","output":"visible","opaque":"` + "\xed\xa0",
			tail:         "\x80" + `"}`,
			rejectPrefix: true,
		},
		{
			name:         "above_maximum_unicode_scalar",
			head:         `{"kind":"tool_result","output":"visible","opaque":"` + "\xf4\x90",
			tail:         "\x80\x80" + `"}`,
			rejectPrefix: true,
		},
		{
			name: "incomplete_utf8_at_full_eof",
			head: `{"kind":"tool_result","output":"visible","opaque":"` + "\xe2",
			tail: "\x82",
		},
		{
			name: "invalid_simple_escape",
			head: `{"kind":"tool_result","output":"visible","opaque":"\`,
			tail: `q"}`,
		},
		{
			name: "invalid_unicode_escape_digit",
			head: `{"kind":"tool_result","output":"visible","opaque":"\ud83`,
			tail: `g"}`,
		},
		{
			name: "negative_leading_zero",
			head: `{"kind":"tool_result","output":"visible","opaque":-0`,
			tail: `1}`,
		},
		{
			name: "exponent_sign_without_digit",
			head: `{"kind":"tool_result","output":"visible","opaque":1e+`,
			tail: `}`,
		},
		{
			name: "invalid_literal_ending",
			head: `{"kind":"tool_result","output":"visible","opaque":nu`,
			tail: `lx}`,
		},
		{
			name: "object_trailing_comma",
			head: `{"kind":"tool_result","output":"visible","opaque":{"key":0,`,
			tail: `}}`,
		},
		{
			name: "mismatched_container_closer",
			head: `{"kind":"tool_result","output":"visible","opaque":[{`,
			tail: `]}}`,
		},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			raw := []byte(strings.Repeat(" ", domain.DocFieldDiscoveryStepBytes-len(scenario.head)) + scenario.head + scenario.tail)
			initial, err := domain.NewDocFieldDiscoveryState(domain.HashContent(raw), len(raw))
			if err != nil {
				test.Fatal(err)
			}
			beforeInitial := docFieldReviewCanonical(test, initial)
			first, err := domain.AdvanceDocFieldDiscovery(context.Background(), initial, raw[:domain.DocFieldDiscoveryStepBytes])
			if err != nil {
				if !scenario.rejectPrefix || !errors.Is(err, domain.ErrIntegrity) {
					test.Fatal("extendable prefix rejected or malformed prefix returned unexpected error", err)
				}
				if !bytes.Equal(beforeInitial, docFieldReviewCanonical(test, initial)) {
					test.Fatal("rejected prefix mutated the retry checkpoint")
				}
				if _, err := first.State(); !errors.Is(err, domain.ErrIntegrity) {
					test.Fatal("malformed prefix minted a sealed step", err)
				}
				if _, err := domain.BuildDocDisplayStringSourceReader(context.Background(), domain.HashContent(raw), len(raw), bytes.NewReader(raw)); !errors.Is(err, domain.ErrIntegrity) {
					test.Fatal("reader accepted malformed full source", err)
				}
				return
			}
			prior, err := first.State()
			if err != nil {
				test.Fatal(err)
			}
			before := docFieldReviewCanonical(test, prior)
			step, err := domain.AdvanceDocFieldDiscovery(context.Background(), prior, raw[prior.Next:])
			if !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("malformed continuation minted a sealed step", err)
			}
			if _, err := step.State(); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("failed continuation returned usable evidence", err)
			}
			if !bytes.Equal(before, docFieldReviewCanonical(test, prior)) {
				test.Fatal("malformed continuation mutated the retry checkpoint")
			}
		})
	}
}

func TestDocFieldDiscoveryReviewRejectsNonalignedHashState(test *testing.T) {
	raw := []byte(strings.Repeat(" ", sha256.BlockSize) + `{"kind":"reasoning"}`)
	initial, err := domain.NewDocFieldDiscoveryState(domain.HashContent(raw), len(raw))
	if err != nil {
		test.Fatal(err)
	}
	prior := docFieldReviewAdvance(test, initial, raw[:sha256.BlockSize])
	digest := sha256.New()
	_, _ = digest.Write([]byte("opaque-checkpoint-secret"))
	prior.HashState, err = digest.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		test.Fatal(err)
	}
	checkpoint := docFieldReviewCanonical(test, prior)
	if _, err := domain.DecodeDocFieldDiscoveryState(domain.HashContent(checkpoint), checkpoint); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("aligned Next=64 accepted SHA state containing a 24-byte partial raw tail", err)
	}
}

func TestDocFieldDiscoveryReviewRejectsUnreachableUTF8Progress(test *testing.T) {
	head := `{"kind":"tool_result","output":"` + "\xf0"
	raw := []byte(strings.Repeat(" ", sha256.BlockSize-len(head)) + head + "\x9f\x99\x82" + `"}`)
	initial, err := domain.NewDocFieldDiscoveryState(domain.HashContent(raw), len(raw))
	if err != nil {
		test.Fatal(err)
	}
	valid := docFieldReviewAdvance(test, initial, raw[:sha256.BlockSize])
	if valid.UTF8Need != 3 || valid.UTF8Minimum != 0x10000 {
		test.Fatal("fixture missed the four-byte UTF-8 boundary", valid)
	}
	for _, scenario := range []struct {
		name    string
		need    int
		minimum uint32
		value   uint32
	}{
		{name: "two_byte_encoding_cannot_need_three", need: 3, minimum: 0x80, value: 2},
		{name: "three_byte_encoding_cannot_need_three", need: 3, minimum: 0x800, value: 1},
		{name: "four_byte_lead_payload_exceeds_f4", need: 3, minimum: 0x10000, value: 5},
		{name: "two_byte_lead_payload_exceeds_five_bits", need: 1, minimum: 0x80, value: 32},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			malformed := valid
			malformed.UTF8Need, malformed.UTF8Minimum, malformed.UTF8Value = scenario.need, scenario.minimum, scenario.value
			checkpoint := docFieldReviewCanonical(test, malformed)
			if _, err := domain.DecodeDocFieldDiscoveryState(domain.HashContent(checkpoint), checkpoint); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatalf("unreachable UTF-8 checkpoint accepted: need=%d minimum=%#x value=%#x error=%v", scenario.need, scenario.minimum, scenario.value, err)
			}
		})
	}
}

func TestDocFieldDiscoveryReviewRejectsOverflowedSpans(test *testing.T) {
	raw := []byte(`{"kind":"reasoning","redacted_summary":"visible"}`)
	valid := docFieldReviewDiscover(test, raw)
	for _, member := range []string{"summary", "blocks", "output"} {
		test.Run(member, func(test *testing.T) {
			malformed := valid
			span := &domain.DocFindSpan{From: math.MaxInt, To: -2}
			switch member {
			case "summary":
				malformed.Summary = span
			case "blocks":
				malformed.Blocks = span
			case "output":
				malformed.Output = span
			}
			checkpoint := docFieldReviewCanonical(test, malformed)
			decoded, err := domain.DecodeDocFieldDiscoveryState(domain.HashContent(checkpoint), checkpoint)
			if !errors.Is(err, domain.ErrIntegrity) {
				fields, fieldsErr := decoded.FindFields()
				test.Fatalf("overflowed %s span %+v decoded: error=%v fields=%+v/%v", member, *span, err, fields, fieldsErr)
			}
		})
	}
}

func TestDocFieldDiscoveryReviewClosedRootNeedsWholeHashAndLength(test *testing.T) {
	const body = `{"kind":"tool_result","output":"visible"}`
	prefix := body + strings.Repeat(" ", domain.DocFieldDiscoveryStepBytes-len(body))
	raw := []byte(prefix + strings.Repeat(" ", sha256.BlockSize) + "\r\n")
	initial, err := domain.NewDocFieldDiscoveryState(domain.HashContent(raw), len(raw))
	if err != nil {
		test.Fatal(err)
	}
	prior := docFieldReviewAdvance(test, initial, raw[:domain.DocFieldDiscoveryStepBytes])
	if !prior.RootDone || prior.Done {
		test.Fatal("fixture must close the root before EOF")
	}
	if _, err := prior.DisplaySource(); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("closed root exposed unverified display source", err)
	}
	if _, err := prior.FindFields(); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("closed root exposed unverified typed fields", err)
	}
	for _, scenario := range []struct {
		name string
		tail []byte
	}{
		{name: "trailing_invalid_utf8", tail: append(bytes.Clone(raw[prior.Next:len(raw)-1]), 0xff)},
		{name: "trailing_second_document", tail: append(bytes.Clone(raw[prior.Next:len(raw)-2]), '{', '}')},
		{name: "same_length_valid_whitespace_wrong_hash", tail: []byte(strings.Repeat("\t", len(raw)-prior.Next))},
		{name: "declared_length_overrun", tail: append(bytes.Clone(raw[prior.Next:]), ' ')},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			step, err := domain.AdvanceDocFieldDiscovery(context.Background(), prior, scenario.tail)
			if !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("unverified trailing bytes produced a step", step.From(), err)
			}
			if _, err := step.State(); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("failed advance returned a usable sealed step", err)
			}
		})
	}
	finished := docFieldReviewAdvance(test, prior, raw[prior.Next:])
	want, wantErr := docFieldReviewDisplayOracle(test, raw)
	got, gotErr := finished.DisplaySource()
	if got != want || !errors.Is(gotErr, wantErr) {
		test.Fatal("closed root failed after verified trailing whitespace", got, gotErr, want, wantErr)
	}
}

type docFieldReviewDeferredEOFReader struct {
	data       []byte
	emptyReads int
	terminal   error
	probes     int
}

func (reader *docFieldReviewDeferredEOFReader) Read(buffer []byte) (int, error) {
	if len(reader.data) > 0 {
		count := copy(buffer, reader.data)
		reader.data = reader.data[count:]
		return count, nil
	}
	reader.probes++
	if reader.emptyReads > 0 {
		reader.emptyReads--
		return 0, nil
	}
	return 0, reader.terminal
}

func TestDocFieldDiscoveryReviewReaderRequiresDeferredEOF(test *testing.T) {
	for _, kind := range []string{"reasoning", "tool_result"} {
		for _, terminal := range []error{io.EOF, io.ErrClosedPipe} {
			test.Run(kind+"/"+terminal.Error(), func(test *testing.T) {
				raw := []byte(`{"kind":"` + kind + `","output":"visible","redacted_summary":"visible"}`)
				reader := &docFieldReviewDeferredEOFReader{data: raw, emptyReads: 99, terminal: terminal}
				var err error
				if kind == "reasoning" {
					_, err = domain.BuildDocFindFieldsReader(context.Background(), domain.HashContent(raw), len(raw), reader)
				} else {
					_, err = domain.BuildDocDisplayStringSourceReader(context.Background(), domain.HashContent(raw), len(raw), reader)
				}
				if reader.probes != 100 || terminal == io.EOF && err != nil || terminal != io.EOF && !errors.Is(err, terminal) {
					test.Fatal("reader returned before verifying real EOF or lost the delayed error", reader.probes, err)
				}
			})
		}
	}
}
