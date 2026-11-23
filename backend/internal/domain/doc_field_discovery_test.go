package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"errors"
	"strings"
	"testing"
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
