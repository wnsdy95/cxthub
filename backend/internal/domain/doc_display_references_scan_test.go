package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func docDisplayHashCandidatesTokenOracle(raw []byte) ([]ContentHash, error) {
	if len(raw) == 0 || len(raw) > MaxDocDisplayPageBytes || !utf8.Valid(raw) {
		return nil, ErrDocDisplayReferencesUnsupported
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	found := make(map[ContentHash]struct{})
	depth, roots := 0, 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			if depth != 0 || roots != 1 {
				return nil, ErrDocDisplayReferencesUnsupported
			}
			break
		}
		if err != nil {
			return nil, ErrDocDisplayReferencesUnsupported
		}
		if depth == 0 {
			roots++
			if roots > 1 {
				return nil, ErrDocDisplayReferencesUnsupported
			}
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				depth++
				if depth > 512 {
					return nil, ErrDocDisplayReferencesUnsupported
				}
			case '}', ']':
				depth--
			}
		} else if value, ok := token.(string); ok {
			hash := ContentHash(value)
			if ValidateContentHash(hash) == nil {
				found[hash] = struct{}{}
			}
		}
	}
	result := make([]ContentHash, 0, len(found))
	for hash := range found {
		result = append(result, hash)
	}
	slices.Sort(result)
	return result, nil
}

func assertDocDisplayHashCandidateParity(test testing.TB, raw []byte) {
	test.Helper()
	before := bytes.Clone(raw)
	want, wantErr := docDisplayHashCandidatesTokenOracle(raw)
	got, gotErr := DocDisplayHashCandidates(context.Background(), raw)
	if !errors.Is(gotErr, wantErr) || !reflect.DeepEqual(got, want) || !bytes.Equal(raw, before) {
		test.Fatalf("reference extraction changed: got %v (%v), want %v (%v), input %q", got, gotErr, want, wantErr, raw)
	}
}

func TestDocDisplayHashCandidatesEscapedKeysAndValues(test *testing.T) {
	first, second := HashContent([]byte("first")), HashContent([]byte("second"))
	var escaped strings.Builder
	for _, character := range first {
		fmt.Fprintf(&escaped, `\u%04x`, character)
	}
	raw := []byte(`{"` + escaped.String() + `":["` + string(second) + `","` + escaped.String() + `"],"same":"` + string(first) + `","same":"` + string(second) + `","literal":"\\\"{}[]","surrogate":"\ud800"}`)
	got, err := DocDisplayHashCandidates(context.Background(), raw)
	want := []ContentHash{first, second}
	slices.Sort(want)
	if err != nil || !reflect.DeepEqual(got, want) {
		test.Fatal("escaped or duplicate JSON keys lost retention references", got, err)
	}
	assertDocDisplayHashCandidateParity(test, raw)
}

func TestDocDisplayHashCandidatesScanBoundaries(test *testing.T) {
	hash := string(HashContent([]byte("retained")))
	for name, raw := range map[string]string{
		"depth-limit":     strings.Repeat("[", 512) + `"` + hash + `"` + strings.Repeat("]", 512),
		"depth-overflow":  strings.Repeat("[", 513) + `"` + hash + `"` + strings.Repeat("]", 513),
		"string-brackets": `{"literal":"` + strings.Repeat("[", 513) + `","hash":"` + hash + `"}`,
		"escaped-prefix":  `"\u0073ha256:` + hash[len(contentHashPrefix):] + `"`,
		"escaped-hex":     `"` + hash[:len(hash)-1] + fmt.Sprintf(`\u%04x`, hash[len(hash)-1]) + `"`,
		"maximum-bytes":   `"` + hash + `"` + strings.Repeat(" ", MaxDocDisplayPageBytes-len(hash)-2),
		"too-many-bytes":  `"` + hash + `"` + strings.Repeat(" ", MaxDocDisplayPageBytes-len(hash)-1),
		"invalid-tail":    `["` + hash + `",true,]`,
		"multiple-roots":  `"` + hash + `" {}`,
		"missing-colon":   `{"` + hash + `" "value"}`,
		"long-number":     `{"hash":"` + hash + `","number":1e9999999999}`,
		"invalid-utf8":    `{"hash":"` + hash + `","invalid":"` + string([]byte{0xff}) + `"}`,
		"not-a-hash":      `"sha256:` + strings.Repeat("g", 64) + `"`,
	} {
		test.Run(name, func(test *testing.T) {
			assertDocDisplayHashCandidateParity(test, []byte(raw))
		})
	}
}

func FuzzDocDisplayHashCandidatesTokenParity(fuzz *testing.F) {
	hash := string(HashContent([]byte("retained")))
	for _, raw := range []string{
		`null`, `true`, `-1.2e9999999`, `{}`, `[]`, `"` + hash + `"`,
		`{"` + hash + `":"\u0073ha256:` + hash[len(contentHashPrefix):] + `"}`,
		`{"duplicate":"` + hash + `","duplicate":null}`,
		`["\\\"{}[]","\ud800","🙂"]`, `{"bad":true,}`, `"unterminated`,
		strings.Repeat("[", 512) + `"` + hash + `"` + strings.Repeat("]", 512),
		strings.Repeat("[", 513) + `0` + strings.Repeat("]", 513),
		string([]byte{'"', 0xff, '"'}),
	} {
		fuzz.Add([]byte(raw))
	}
	fuzz.Fuzz(func(test *testing.T, raw []byte) {
		assertDocDisplayHashCandidateParity(test, raw)
	})
}

func BenchmarkDocDisplayHashCandidatesPage(benchmark *testing.B) {
	raw, err := json.Marshal(struct {
		Kind   string `json:"kind"`
		Output string `json:"output"`
	}{Kind: "tool_result", Output: strings.Repeat("a🙂<&>\n", 200)})
	if err != nil {
		benchmark.Fatal(err)
	}
	source, err := BuildDocDisplayStringSourceReader(context.Background(), HashContent(raw), len(raw), bytes.NewReader(raw))
	if err != nil {
		benchmark.Fatal(err)
	}
	prior, err := NewDocDisplayStringState(source)
	if err != nil {
		benchmark.Fatal(err)
	}
	step, err := StepDocDisplayString(context.Background(), source, prior, func(_ context.Context, offset, length int) ([]byte, error) {
		return raw[offset : offset+length], nil
	})
	if err != nil || step.Page == nil || len(step.Page.Runs) != DocDisplayPageRuns {
		benchmark.Fatal("fixture did not fill the original page run limit", err)
	}
	page := step.Objects[len(step.Objects)-1].Bytes
	for _, candidate := range []struct {
		name string
		read func([]byte) ([]ContentHash, error)
	}{
		{"token-oracle", docDisplayHashCandidatesTokenOracle},
		{"bounded-scan", func(raw []byte) ([]ContentHash, error) { return DocDisplayHashCandidates(context.Background(), raw) }},
	} {
		benchmark.Run(candidate.name, func(benchmark *testing.B) {
			benchmark.ReportAllocs()
			benchmark.SetBytes(int64(len(page)))
			for benchmark.Loop() {
				if hashes, err := candidate.read(page); err != nil || len(hashes) == 0 {
					benchmark.Fatal(hashes, err)
				}
			}
		})
	}
}
