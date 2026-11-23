package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

func messagePlaceholderReviewFixture(test *testing.T, blockType, name string) (DocMessagePlaceholderPlan, map[ContentHash][]byte) {
	test.Helper()
	block, err := json.Marshal(map[string]any{"type": blockType, "name": name, "text": "hidden", "opaque": map[string]any{"name": "locked"}})
	if err != nil {
		test.Fatal(err)
	}
	raw := append([]byte(`{"kind":"message","blocks":[`), block...)
	raw = append(raw, ']', '}')
	fixture := buildMessageInputFixture(test, raw)
	input := messageFixtureInputs(test, fixture)[0]
	typeSource, nameSource, err := input.PlaceholderSources()
	if err != nil {
		test.Fatal(err)
	}
	decodedType := buildDecodedTextSourceFixture(test, raw, *typeSource)
	decodedName := buildDecodedTextSourceFixture(test, raw, *nameSource)
	plan, err := NewDocMessagePlaceholderPlan(input, &decodedType.generation, &decodedName.generation)
	if err != nil {
		test.Fatal(err)
	}
	objects := make(map[ContentHash][]byte)
	for _, prepared := range []decodedTextFixture{decodedType, decodedName} {
		for hash, body := range prepared.objects {
			objects[hash] = body
		}
	}
	return plan, objects
}

func TestDocMessagePlaceholderReviewSelectedPartAndPlanBinding(test *testing.T) {
	plan, objects := messagePlaceholderReviewFixture(test, strings.Repeat("x", 70000), "\ud55c\uae00🙂")
	load := messageInputReviewLoad(objects)
	cursor, err := NewDocMessagePlaceholderCursor(plan)
	if err != nil {
		test.Fatal(err)
	}
	cursor.From = 1 + plan.Type.TextBytes + 2
	loads := 0
	fragment, err := ReadDocMessagePlaceholderFragment(context.Background(), plan, cursor, func(ctx context.Context, hash ContentHash) ([]byte, error) {
		loads++
		return load(ctx, hash)
	})
	if err != nil || fragment.Text() != "\ud55c\uae00🙂]" || loads != 4 {
		test.Fatal("late name read scanned prior type pages", fragment.Text(), loads, err)
	}
	cursor.From = plan.TextBytes - 1
	fragment, err = ReadDocMessagePlaceholderFragment(context.Background(), plan, cursor, func(context.Context, ContentHash) ([]byte, error) {
		test.Fatal("closing bracket read prepared string objects")
		return nil, nil
	})
	if err != nil || fragment.Text() != "]" {
		test.Fatal("closing bracket resume failed", err)
	}
	encoded, err := plan.Encode()
	if err != nil {
		test.Fatal(err)
	}
	for _, malformed := range [][]byte{
		append(bytes.Clone(encoded), ' '),
		append([]byte(`{"version":1,`), encoded[1:]...),
		append([]byte(`{"unknown":true,`), encoded[1:]...),
	} {
		if _, err := DecodeDocMessagePlaceholderPlan(HashContent(malformed), malformed); !errors.Is(err, ErrIntegrity) {
			test.Fatal("noncanonical plan accepted", err)
		}
	}
	changed := plan
	changed.Type, changed.Name = changed.Name, changed.Type
	if !errors.Is(changed.Validate(), ErrIntegrity) {
		test.Fatal("decoded type/name generations could be exchanged")
	}
	for _, from := range []int{-1, plan.TextBytes, math.MaxInt} {
		changedCursor := cursor
		changedCursor.From = from
		result, err := ReadDocMessagePlaceholderFragment(context.Background(), plan, changedCursor, func(context.Context, ContentHash) ([]byte, error) {
			test.Fatal("invalid cursor caused an object read")
			return nil, nil
		})
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(result, DocMessagePlaceholderFragment{}) {
			test.Fatal("invalid cursor returned partial placeholder", from, err)
		}
	}
	cursor.Plan = HashContent([]byte("another plan"))
	if _, err := ReadDocMessagePlaceholderFragment(context.Background(), plan, cursor, load); !errors.Is(err, ErrIntegrity) {
		test.Fatal("foreign plan cursor accepted", err)
	}
}

func TestDocMessagePlaceholderReviewCancellationAndInteriorRuneCursor(test *testing.T) {
	plan, objects := messagePlaceholderReviewFixture(test, "image", "🙂")
	load := messageInputReviewLoad(objects)
	cursor, err := NewDocMessagePlaceholderCursor(plan)
	if err != nil {
		test.Fatal(err)
	}
	for _, cancelAt := range []int{0, 1, 4} {
		ctx, cancel := context.WithCancel(context.Background())
		if cancelAt == 0 {
			cancel()
		}
		loads := 0
		fragment, err := ReadDocMessagePlaceholderFragment(ctx, plan, cursor, func(ctx context.Context, hash ContentHash) ([]byte, error) {
			loads++
			if loads == cancelAt {
				cancel()
			}
			return load(ctx, hash)
		})
		cancel()
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(fragment, DocMessagePlaceholderFragment{}) || loads > cancelAt {
			test.Fatal("cancelled placeholder leaked a prefix or kept reading", cancelAt, loads, err)
		}
	}
	cursor.From = 1 + plan.Type.TextBytes + 2 + 1
	fragment, err := ReadDocMessagePlaceholderFragment(context.Background(), plan, cursor, load)
	if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(fragment, DocMessagePlaceholderFragment{}) {
		test.Fatal("interior UTF-8 cursor returned an invalid fragment", err)
	}
}
