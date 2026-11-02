package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func discoverDocFields(test *testing.T, raw []byte, size int) (DocFieldDiscoveryState, error) {
	test.Helper()
	state, err := NewDocFieldDiscoveryState(HashContent(raw), len(raw))
	if err != nil {
		return state, err
	}
	for !state.Done {
		before, err := state.Encode()
		if err != nil {
			return DocFieldDiscoveryState{}, err
		}
		step, err := AdvanceDocFieldDiscovery(context.Background(), state, raw[state.Next:min(state.Next+size, len(raw))])
		if err != nil {
			return DocFieldDiscoveryState{}, err
		}
		after, err := state.Encode()
		if err != nil || !bytes.Equal(before, after) || step.From() != state.Next || state.Next > 0 && step.Previous() != HashContent(before) {
			test.Fatal("step changed prior state or lost its predecessor", err)
		}
		checkpoint, encoded := step.Checkpoint()
		state, err = DecodeDocFieldDiscoveryState(checkpoint, encoded)
		if err != nil || len(encoded) > MaxDocFieldDiscoveryStateBytes {
			test.Fatal("checkpoint did not survive restart", err)
		}
		if !state.Done {
			if _, err := state.DisplaySource(); !errors.Is(err, ErrIntegrity) {
				test.Fatal("partial discovery became source evidence", err)
			}
		}
	}
	return state, nil
}

func TestDocFieldDiscoveryResumesLexicalAndContainerStates(test *testing.T) {
	for _, body := range []string{
		`{"kind":"reasoning","redacted_summary":"visible"}`,
		"{\"kind\":\"message\",\"blocks\":[{\"text\":\"\ud55c\uae00🙂\",\"type\":\"text\"}]}",
		`{"kind":"reasoning","unknown":[null,true,false,-0,0.5,1e+99,-4.2E-9,1e99999,{"nested":["\ud83d\ude42\ud800"]}],"redacted_summary":"last"}`,
		`{"kind":"reasoning","redacted_summary":false,"redacted\u005fsummary":"last","blocks":false}`,
		`{"kind":"message","blocks":{},"blocks":null,"blocks":[],"redacted_summary":false}`,
		`{"kind":"tool_result","output":" \t\n\r\f\u000b\u00a0\u1680\u2000\u200a\u2028\u2029\u202f\u205f\u3000\ufeff"}`,
		`{"kind":"tool_result","output":"first","output":{},"output":"\ud800\ud83d\ude42"}`,
		`{"k\u0069nd":"tool_result","nested":{"output":"ignored"},"out\u0070ut":"value"}`,
		`{"kind":"reasoning","redacted_summary":"first","redacted_summary":null}`,
	} {
		for padding := 0; padding < 64; padding++ {
			raw := []byte(strings.Repeat(" ", padding) + body + "\r\n\t ")
			state, err := discoverDocFields(test, raw, 64)
			if err != nil {
				test.Fatalf("padding=%d body=%s: %v", padding, body, err)
			}
			want, wantErr := BuildDocFindFields(HashContent(raw), raw)
			got, gotErr := state.FindFields()
			if !reflect.DeepEqual(want, got) || (wantErr == nil) != (gotErr == nil) {
				test.Fatalf("fields differ: raw=%s want=%+v/%v got=%+v/%v", raw, want, wantErr, got, gotErr)
			}
			wantedSource, sourceErr := BuildDocDisplayStringSourceReader(context.Background(), HashContent(raw), len(raw), bytes.NewReader(raw))
			actualSource, actualErr := state.DisplaySource()
			if !reflect.DeepEqual(wantedSource, actualSource) || !errors.Is(sourceErr, actualErr) {
				test.Fatalf("display source differs: raw=%s want=%+v/%v got=%+v/%v", raw, wantedSource, sourceErr, actualSource, actualErr)
			}
		}
	}
}

func TestDocFieldDiscoveryBoundedOpaqueAndDepth(test *testing.T) {
	for _, size := range []int{1 << 20, 10 << 20} {
		body := `{"opaque":"` + strings.Repeat("opaque-secret-", size/14) + `","` + strings.Repeat("private-key-", 7000) + `":true,"kind":"tool_result","output":"visible"}`
		state, err := discoverDocFields(test, []byte(body), DocFieldDiscoveryStepBytes)
		if err != nil {
			test.Fatal(err)
		}
		source, err := state.DisplaySource()
		encoded, encodeErr := state.Encode()
		if err != nil || encodeErr != nil || source.Empty || source.Token.To-source.Token.From != len(`"visible"`) || bytes.Contains(encoded, []byte("opaque-secret")) || bytes.Contains(encoded, []byte("private-key")) {
			test.Fatal("opaque data leaked into discovery checkpoint", source, err, encodeErr)
		}
		test.Logf("source_bytes=%d final_checkpoint_bytes=%d", len(body), len(encoded))
	}
	for _, depth := range []int{64, 9998, 9999, 10000} {
		raw := []byte(`{"kind":"reasoning","unknown":` + strings.Repeat("[", depth) + `0` + strings.Repeat("]", depth) + `}`)
		want, wantErr := BuildDocFindFields(HashContent(raw), raw)
		state, err := discoverDocFields(test, raw, 64)
		if err == nil {
			actual, actualErr := state.FindFields()
			if !reflect.DeepEqual(actual, want) || (actualErr == nil) != (wantErr == nil) {
				test.Fatal("depth semantic mismatch", depth, actual, actualErr, wantErr)
			}
		} else if wantErr == nil {
			test.Fatal("valid depth rejected", depth, err)
		}
	}
}

func TestDocFieldDiscoveryRejectsPartialInvalidAndWrongIdentity(test *testing.T) {
	for _, body := range []string{
		`[]`, `null`, `{"kind":"reasoning",}`, `{"kind":"reasoning","unknown":[1,]}`,
		`{"kind":"reasoning","value":01}`, `{"kind":"reasoning","value":1.}`,
		`{"kind":"reasoning","value":-}`, `{"kind":"reasoning","value":1e+}`,
		`{"kind":"reasoning","value":tru}`, `{"kind":"reasoning","value":falsex}`,
		`{"kind":"reasoning","value":"\q"}`, `{"kind":"reasoning","value":"\u12"}`,
		`{"kind":"reasoning"}null`, `{"kind":"reasoning"} {}`,
		"{\"kind\":\"reasoning\",\"unknown\":\"invalid\xff\"}",
		"{\"kind\":\"reasoning\",\"unknown\":\"invalid\xed\xa0\x80\"}",
		"{\"kind\":\"reasoning\",\"unknown\":\"invalid\xf4\x90\x80\x80\"}",
	} {
		if _, err := discoverDocFields(test, []byte(body), 64); !errors.Is(err, ErrIntegrity) {
			test.Fatalf("invalid JSON accepted: %q: %v", body, err)
		}
	}
	raw := []byte(`{"kind":"tool_result","output":"` + strings.Repeat("x", 1000) + `"}`)
	state, err := NewDocFieldDiscoveryState(HashContent([]byte("wrong source")), len(raw))
	if err != nil {
		test.Fatal(err)
	}
	if _, err := AdvanceDocFieldDiscovery(context.Background(), state, raw); !errors.Is(err, ErrIntegrity) {
		test.Fatal("wrong source hash accepted", err)
	}
	state, _ = NewDocFieldDiscoveryState(HashContent(raw), len(raw))
	before, _ := state.Encode()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := AdvanceDocFieldDiscovery(cancelled, state, raw[:64]); !errors.Is(err, context.Canceled) {
		test.Fatal(err)
	}
	if _, err := AdvanceDocFieldDiscovery(context.Background(), state, raw[:63]); !errors.Is(err, ErrIntegrity) {
		test.Fatal("unaligned partial state accepted", err)
	}
	after, _ := state.Encode()
	if !bytes.Equal(before, after) {
		test.Fatal("failed advancement changed prior state")
	}
}

func TestDocFieldDiscoveryAcceptsValidUTF8BoundaryExtremes(test *testing.T) {
	for _, character := range []rune{0x80, 0x7ff, 0x800, 0xd7ff, 0xe000, 0xffff, 0x10000, 0x10ffff} {
		for split := 1; split < len(string(character)); split++ {
			head := `{"kind":"tool_result","output":"`
			raw := []byte(strings.Repeat(" ", 64-len(head)-split) + head + string(character) + `"}`)
			state, err := discoverDocFields(test, raw, 64)
			if err != nil {
				test.Fatalf("valid UTF-8 rune=%#x split=%d rejected: %v", character, split, err)
			}
			source, err := state.DisplaySource()
			if err != nil || source.Empty || source.Token.To-source.Token.From != len(string(character))+2 {
				test.Fatal("valid split lost output source", source, err)
			}
		}
	}
}

func TestDocFieldDiscoveryNativeSHAStateContract(test *testing.T) {
	for _, length := range []int{0, 1, 63, 64, 65, 128, 129} {
		digest := sha256.New()
		_, _ = digest.Write(bytes.Repeat([]byte{'x'}, length))
		raw, err := digest.(encoding.BinaryMarshaler).MarshalBinary()
		if err != nil {
			test.Fatal(err)
		}
		if validDocFieldHashState(raw, length) != (length%sha256.BlockSize == 0) {
			test.Fatal("native SHA format or alignment changed", length, len(raw))
		}
		if validDocFieldHashState(raw, length+sha256.BlockSize) {
			test.Fatal("SHA byte counter differs from discovery position", length)
		}
		if length%sha256.BlockSize == 0 {
			poisoned := bytes.Clone(raw)
			poisoned[4+sha256.Size] = 'x'
			if validDocFieldHashState(poisoned, length) {
				test.Fatal("aligned SHA checkpoint retained raw source tail", length)
			}
		}
	}
}

func FuzzDocFieldDiscoveryMatchesRawFields(test *testing.F) {
	for _, raw := range []string{`{"kind":"message","blocks":[]}`, "{\"kind\":\"reasoning\",\"redacted_summary\":\"\ud55c\uae00\"}", `{"kind":"reasoning","x":1e99999}`, `{"kind":"reasoning","redacted_summary":false,"redacted_summary":"last"}`} {
		test.Add(raw)
	}
	test.Fuzz(func(test *testing.T, body string) {
		if len(body) == 0 || len(body) > 256<<10 || !utf8.ValidString(body) || !json.Valid([]byte(body)) {
			return
		}
		raw := []byte(body)
		want, wantErr := BuildDocFindFields(HashContent(raw), raw)
		stepBytes := 64
		if len(raw) > 4096 {
			stepBytes = DocFieldDiscoveryStepBytes
		}
		state, err := discoverDocFields(test, raw, stepBytes)
		if err != nil {
			if wantErr == nil {
				test.Fatal("valid raw fields rejected", err)
			}
			return
		}
		actual, actualErr := state.FindFields()
		if !reflect.DeepEqual(want, actual) || (wantErr == nil) != (actualErr == nil) {
			test.Fatal("raw projection mismatch", want, wantErr, actual, actualErr)
		}
	})
}
