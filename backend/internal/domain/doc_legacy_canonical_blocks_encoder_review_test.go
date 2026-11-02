package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestLegacyCanonicalBlocksEncoderReviewExactStringUnits(test *testing.T) {
	for _, fixture := range []struct {
		name   string
		field  string
		unit   string
		prefix string
		block  ContentBlock
		array  bool
	}{
		{name: "block-text-quote", field: "text", unit: "\"", prefix: `{"text":"`, block: ContentBlock{Type: "kind"}},
		{name: "array-text-html", field: "text", unit: "<", prefix: `[{"text":"`, block: ContentBlock{Type: "kind"}, array: true},
		{name: "block-type-separator", field: "type", unit: "\u2028", prefix: `{"text":"lead","type":"`, block: ContentBlock{Text: "lead"}},
		{name: "array-type-rune", field: "type", unit: "🙂", prefix: `[{"text":"lead","type":"`, block: ContentBlock{Text: "lead"}, array: true},
	} {
		encodedUnit, err := json.Marshal(fixture.unit)
		if err != nil {
			test.Fatal(err)
		}
		width := len(encodedUnit) - 2
		for _, short := range []int{0, 1} {
			for _, eager := range []bool{true, false} {
				test.Run(fmt.Sprintf("%s/short-%d/eager-%t", fixture.name, short, eager), func(test *testing.T) {
					padding := LegacyCanonicalEncodingBytes - len(fixture.prefix) - (width - short)
					filler := strings.Repeat("\"", padding/2)
					if padding%2 != 0 {
						filler += "a"
					}
					text := filler + fixture.unit + "tail"
					block := fixture.block
					if fixture.field == "text" {
						block.Text = text
					} else {
						block.Type = text
					}
					var value any = block
					var oracle any = map[string]any{"text": block.Text, "type": block.Type}
					if fixture.array {
						value = []ContentBlock{block}
						oracle = []any{oracle}
					}
					want, err := json.Marshal(oracle)
					if err != nil {
						test.Fatal(err)
					}
					encoding, err := newLegacyCanonicalEncodingWithNumbers(context.Background(), value, eager)
					if err != nil {
						test.Fatal(err)
					}
					first, complete, err := encoding.Next(context.Background())
					expectedBytes, expectedPosition := len(fixture.prefix)+padding, len(filler)
					if short == 0 {
						expectedBytes += width
						expectedPosition += len(fixture.unit)
					}
					if err != nil || complete || len(first) != expectedBytes || cap(first) > LegacyCanonicalEncodingBytes || !bytes.Equal(first, want[:expectedBytes]) {
						test.Fatal("block string unit was split or consumed with insufficient output room", len(first), expectedBytes, complete, err)
					}
					if encoding.frames.length() == 0 {
						test.Fatal("unfinished block string lost its frame")
					}
					frame := encoding.frames.at(encoding.frames.length() - 1)
					if frame.kind != 's' || frame.text != text || frame.position != expectedPosition {
						test.Fatal("block string source cursor did not preserve the complete encoding unit", frame.kind, frame.position, expectedPosition)
					}
					rest, _ := collectLegacyCanonicalEncoding(test, encoding)
					if !bytes.Equal(append(first, rest...), want) {
						test.Fatal("resuming an exact block string boundary changed canonical bytes")
					}
				})
			}
		}
	}
}

func TestLegacyCanonicalBlocksEncoderReviewOrderingAfterActionYield(test *testing.T) {
	blocks := make([]ContentBlock, 128)
	projected := make([]map[string]any, len(blocks))
	for index := range blocks {
		blocks[index] = ContentBlock{Text: "<"}
		projected[index] = map[string]any{"text": "<", "type": ""}
	}
	before := map[string]any{"before": true}
	prefix := strings.Repeat("k", LegacyCanonicalEncodingBytes+7)
	after := map[string]any{
		prefix + "z": map[string]any{"marker": "last ordered child"},
		prefix + "a": []map[string]any{{"marker": "first ordered child"}},
	}
	tail := map[string]any{"tail": true}
	value := []any{before, blocks, after, tail}
	want, err := json.Marshal([]any{before, projected, after, tail})
	if err != nil {
		test.Fatal(err)
	}
	for _, eager := range []bool{true, false} {
		test.Run(fmt.Sprintf("eager-%t", eager), func(test *testing.T) {
			encoding, err := newLegacyCanonicalEncodingWithNumbers(context.Background(), value, eager)
			if err != nil {
				test.Fatal(err)
			}
			first, complete, err := encoding.Next(context.Background())
			if err != nil || complete || len(first) == 0 || len(first) >= LegacyCanonicalEncodingBytes || encoding.frames.length() < 2 {
				test.Fatal("typed array did not yield on structural actions before its following map", len(first), complete, err)
			}
			array := encoding.frames.at(1)
			if array.kind != 'c' || array.position == 0 || array.position >= len(blocks) || array.keysPosition != 1 || encoding.keyWork != 0 {
				test.Fatal("action yield lost the following map cursor inside the typed array", array.kind, array.position, array.keysPosition, encoding.keyWork)
			}
			got := bytes.Clone(first)
			orderedWithoutOutput := false
			for step := 0; step < 64; step++ {
				beforeWork, beforeOffset := encoding.keyWork, encoding.order.compareOffset
				chunk, done, err := encoding.Next(context.Background())
				if err != nil || len(chunk) > LegacyCanonicalEncodingBytes || cap(chunk) > LegacyCanonicalEncodingBytes {
					test.Fatal("following map exceeded its encoding quantum", len(chunk), cap(chunk), err)
				}
				if len(chunk) == 0 && !done {
					if !encoding.ordering || encoding.keyWork <= beforeWork || encoding.frames.length() == 0 {
						test.Fatal("following map yielded without ordering progress")
					}
					frame := encoding.frames.at(encoding.frames.length() - 1)
					if frame.kind != 'o' || frame.keysPosition != 1 || frame.keysEnd != 4 {
						test.Fatal("typed array completion selected the wrong following map key range", frame.kind, frame.keysPosition, frame.keysEnd)
					}
					compared := 2 * (encoding.order.compareOffset - beforeOffset)
					if compared <= 0 || compared > LegacyCanonicalEncodingBytes {
						test.Fatal("following map comparison exceeded its two-sided source-byte budget", compared)
					}
					orderedWithoutOutput = true
				}
				got = append(got, chunk...)
				if done {
					if !orderedWithoutOutput || !bytes.Equal(got, want) {
						test.Fatal("post-array ordering failed to progress or changed child/sibling pairing", orderedWithoutOutput)
					}
					return
				}
			}
			test.Fatal("post-array ordering did not terminate within the finite review fixture")
		})
	}
}
