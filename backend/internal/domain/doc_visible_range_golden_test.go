package domain

import (
	"context"
	"fmt"
	"testing"
)

func TestDocVisibleRangePassGolden(test *testing.T) {
	expected := map[int]ContentHash{
		0:   "sha256:896e4c98de73c3ad644f3bc29f82bf7734c483d54f89100617942e778412613a",
		1:   "sha256:5a7617157938a0b5bf6a879f44f7001d27a0f40a67744c5cdbb863550d6e7219",
		129: "sha256:5a4a74a93b20905416ada0a7ff026e78057409c7de00b47116aa861237f85fdd",
		259: "sha256:4cf31443b56b2a2574c580a670742ab69dc447cb52fc505903ca31f79d37d180",
	}
	fingerprint := func(generation DocVisibleGeneration, objects []DocRangeObject) ContentHash {
		return HashContent(docVisibleTestJSON(test, struct {
			Generation DocVisibleGeneration
			Objects    []DocRangeObject
		}{Generation: generation, Objects: objects}))
	}
	for _, count := range []int{0, 1, 129, 259} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			fixture := docVisibleTestFixture(test, docVisibleRangePassEvents(count))
			proof := docVisibleTestRangeProof(test, fixture.streamed)
			var objects []DocRangeObject
			generation, err := docVisibleRangePassPredecessor(context.Background(), proof, docVisibleRangePassEmitter(test, &objects), nil)
			if err != nil {
				test.Fatal(err)
			}
			if actual := fingerprint(generation, objects); actual != expected[count] {
				test.Fatal("predecessor metadata bytes or emission order changed", count, actual, expected[count])
			}
			objects = nil
			generation, err = BuildDocVisibleGenerationWithRangePages(context.Background(), proof, docVisibleRangePassEmitter(test, &objects), func(ctx context.Context, _ int, _ DocRangeObject) error {
				return ctx.Err()
			})
			if err != nil {
				test.Fatal(err)
			}
			if actual := fingerprint(generation, objects); actual != expected[count] {
				test.Fatal("shared-pass metadata bytes or emission order changed", count, actual, expected[count])
			}
		})
	}
}
