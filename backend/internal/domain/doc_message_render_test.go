package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func messagePlaceholderFixture(test *testing.T, raw []byte) (DocMessagePlaceholderPlan, map[ContentHash][]byte) {
	test.Helper()
	inputs := messageFixtureInputs(test, buildMessageInputFixture(test, raw))
	if len(inputs) != 1 {
		test.Fatal("expected one placeholder input")
	}
	blockType, name, err := inputs[0].PlaceholderSources()
	if err != nil {
		test.Fatal(err)
	}
	objects := make(map[ContentHash][]byte)
	var prepared [2]*DocDecodedTextGeneration
	for index, source := range []*DocDecodedTextSource{blockType, name} {
		if source == nil {
			continue
		}
		fixture := buildDecodedTextSourceFixture(test, raw, *source)
		prepared[index] = &fixture.generation
		for hash, body := range fixture.objects {
			objects[hash] = bytes.Clone(body)
		}
	}
	plan, err := NewDocMessagePlaceholderPlan(inputs[0], prepared[0], prepared[1])
	if err != nil {
		test.Fatal(err)
	}
	encoded, err := plan.Encode()
	if err != nil {
		test.Fatal(err)
	}
	plan, err = DecodeDocMessagePlaceholderPlan(HashContent(encoded), encoded)
	if err != nil {
		test.Fatal(err)
	}
	return plan, objects
}

func TestDocMessagePlaceholderLargeNameReadsOnlySelectedPreparedPages(test *testing.T) {
	name := strings.Repeat("**\ud55c\uae00🙂** [asset](https://hidden.test)\n", 5000)
	raw, err := json.Marshal(map[string]any{"kind": "message", "blocks": []any{map[string]any{"type": "image", "name": name, "text": "HIDDEN_NON_TEXT_BODY", "opaque": strings.Repeat("LOCKED_PRIVATE_BODY", 10000)}}})
	if err != nil {
		test.Fatal(err)
	}
	plan, objects := messagePlaceholderFixture(test, raw)
	cursor, err := NewDocMessagePlaceholderCursor(plan)
	if err != nil {
		test.Fatal(err)
	}
	loads, maxLoads, fragments := 0, 0, 0
	load := func(_ context.Context, hash ContentHash) ([]byte, error) {
		loads++
		body, found := objects[hash]
		if !found {
			return nil, ErrNotFound
		}
		if bytes.Contains(body, []byte("HIDDEN_NON_TEXT_BODY")) || bytes.Contains(body, []byte("LOCKED_PRIVATE_BODY")) {
			test.Fatal("unselected private input copied into prepared objects")
		}
		return body, nil
	}
	var output strings.Builder
	for cursor.From < plan.TextBytes {
		before := loads
		fragment, err := ReadDocMessagePlaceholderFragment(context.Background(), plan, cursor, load)
		if err != nil || len(fragment.Text()) > DocDisplayPageTextBytes || !utf8.ValidString(fragment.Text()) || fragment.From() != cursor.From || fragment.Next().From <= cursor.From {
			test.Fatal("large placeholder did not make bounded progress", err)
		}
		maxLoads = max(maxLoads, loads-before)
		output.WriteString(fragment.Text())
		cursor = fragment.Next()
		fragments++
	}
	if output.String() != "[image: "+name+"]" || fragments < 3 || maxLoads > 256 {
		test.Fatal("large placeholder changed or rescanned the whole name", output.Len(), fragments, maxLoads)
	}
	lastCursor := cursor
	lastCursor.From -= 1
	before := loads
	fragment, err := ReadDocMessagePlaceholderFragment(context.Background(), plan, lastCursor, load)
	if err != nil || fragment.Text() != "]" || loads != before {
		test.Fatal("synthetic last byte scanned the complete name", loads-before, err)
	}
	test.Logf("event_bytes=%d name_bytes=%d fragments=%d max_fragment_object_loads=%d final_literal_object_loads=%d", len(raw), len(name), fragments, maxLoads, loads-before)
}

func TestDocMessagePlaceholderRejectsWrongOriginCorruptionAndCancelledRead(test *testing.T) {
	first, objects := messagePlaceholderFixture(test, []byte("{\"kind\":\"message\",\"blocks\":[{\"type\":\"image\",\"name\":\"\ud55c\uae00🙂\"}]}"))
	second, _ := messagePlaceholderFixture(test, []byte("{\"kind\":\"message\",\"opaque\":true,\"blocks\":[{\"type\":\"image\",\"name\":\"\ud55c\uae00🙂\"}]}"))
	cursor, err := NewDocMessagePlaceholderCursor(first)
	if err != nil {
		test.Fatal(err)
	}
	called := false
	fragment, err := ReadDocMessagePlaceholderFragment(context.Background(), second, cursor, func(context.Context, ContentHash) ([]byte, error) { called = true; return nil, ErrNotFound })
	if !errors.Is(err, ErrIntegrity) || called || !reflect.DeepEqual(fragment, DocMessagePlaceholderFragment{}) {
		test.Fatal("cursor switched original message", called, err)
	}
	wrong := first
	wrong.Type = second.Type
	if wrong.Validate() == nil {
		test.Fatal("prepared token from another message accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	fragment, err = ReadDocMessagePlaceholderFragment(ctx, first, cursor, func(_ context.Context, hash ContentHash) ([]byte, error) { cancel(); return objects[hash], nil })
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(fragment, DocMessagePlaceholderFragment{}) {
		test.Fatal("cancelled placeholder read returned partial text", err)
	}
	fragment, err = ReadDocMessagePlaceholderFragment(context.Background(), first, cursor, func(_ context.Context, hash ContentHash) ([]byte, error) {
		return append(bytes.Clone(objects[hash]), ' '), nil
	})
	if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(fragment, DocMessagePlaceholderFragment{}) {
		test.Fatal("modified prepared object was accepted", err)
	}
}
