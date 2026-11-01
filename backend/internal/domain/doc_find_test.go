package domain

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func findTestSelector() DocFindSelector {
	return DocFindSelector{Version: 1, Selection: DocVisibleSelector{Version: 1, Purpose: "units", Repo: HashContent([]byte("repo")),
		Hash: HashContent([]byte("doc")), Generation: HashContent([]byte("generation")), View: "chat", Total: 30, Limit: 20},
		QueryHash: HashContent([]byte("needle")), To: 30}
}

func TestDocFindSelectorCanonicalAndBounded(test *testing.T) {
	selector := findTestSelector()
	encoded, err := selector.Encode()
	if err != nil {
		test.Fatal(err)
	}
	decoded, err := DecodeDocFindSelector(encoded)
	if err != nil || decoded != selector {
		test.Fatal(decoded, err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(encoded)
	for _, invalid := range []string{encoded + "=", base64.RawURLEncoding.EncodeToString(append([]byte(" "), raw...)),
		base64.RawURLEncoding.EncodeToString(append([]byte(`{"extra":1,`), raw[1:]...)),
		base64.RawURLEncoding.EncodeToString(append([]byte(`{"version":1,`), raw[1:]...)), strings.Repeat("A", 6000)} {
		if _, err := DecodeDocFindSelector(invalid); !errors.Is(err, ErrValidation) {
			test.Fatal("noncanonical selector accepted", err)
		}
	}
	for _, mutate := range []func(*DocFindSelector){
		func(value *DocFindSelector) { value.Version = 2 }, func(value *DocFindSelector) { value.From = -1 },
		func(value *DocFindSelector) { value.To = 31 }, func(value *DocFindSelector) { value.Index = -1 },
		func(value *DocFindSelector) { value.Byte = -1 }, func(value *DocFindSelector) { value.Field = -1 },
		func(value *DocFindSelector) { value.QueryHash = "no hash" },
		func(value *DocFindSelector) { value.Selection.Purpose = "fold" },
		func(value *DocFindSelector) { value.Index = value.To; value.Byte = 1 },
	} {
		invalid := selector
		mutate(&invalid)
		if _, err := invalid.Encode(); !errors.Is(err, ErrValidation) {
			test.Fatal("invalid selector accepted", invalid, err)
		}
	}
	selector.Index = selector.To
	if _, err := selector.Encode(); err != nil {
		test.Fatal("terminal find cursor invalid", err)
	}
}

func TestDocFindTextUnicodeByteCoordinatesAndOverlap(test *testing.T) {
	for _, fixture := range []struct{ text, query string }{
		{"\uc55e🙂Kelvin kelvin KELVIN \ub4a4", "kelvin"}, {"Σ σ ς \ud55c\uad6d\uc5b4", "σ"}, {"banana ANANA", "ana"},
		{"quoted \"line\"\n\ub2e4\uc74c \uc904", "\"LINE\"\n\ub2e4"}, {"abc", " "}, {"a🙂a🙂", "🙂"},
	} {
		got, err := FindDocText(context.Background(), fixture.text, fixture.query, 0, DocFindPageMatches)
		if err != nil {
			test.Fatal(err)
		}
		want := naiveDocFind(fixture.text, fixture.query)
		if !reflect.DeepEqual(got, want) {
			test.Fatal(fixture, got, want)
		}
	}
}

func naiveDocFind(text, query string) []DocFindMatch {
	var matches []DocFindMatch
	count := utf8.RuneCountInString(query)
	for offset := range text {
		end, runes := offset, 0
		for end < len(text) && runes < count {
			_, size := utf8.DecodeRuneInString(text[end:])
			end += size
			runes++
		}
		if runes == count && strings.EqualFold(text[offset:end], query) {
			matches = append(matches, DocFindMatch{Offset: offset, Length: end - offset})
			if len(matches) == DocFindPageMatches {
				break
			}
		}
	}
	return matches
}

func TestDocFindTextMatchesIndependentUnicodeOracle(test *testing.T) {
	random := rand.New(rand.NewSource(42))
	alphabet := []rune("abABΣσςKkK\ud55c🙂 ")
	for sample := 0; sample < 1000; sample++ {
		textRunes := make([]rune, 128)
		queryRunes := make([]rune, 1+random.Intn(8))
		for index := range textRunes {
			textRunes[index] = alphabet[random.Intn(len(alphabet))]
		}
		for index := range queryRunes {
			queryRunes[index] = alphabet[random.Intn(len(alphabet))]
		}
		text, query := string(textRunes), string(queryRunes)
		got, err := FindDocText(context.Background(), text, query, 0, DocFindPageMatches)
		if err != nil || !reflect.DeepEqual(got, naiveDocFind(text, query)) {
			test.Fatal("matcher differs from independent oracle", sample, text, query, got, err)
		}
	}
}

func TestDocFindQueryLimitsAndCancellation(test *testing.T) {
	for _, query := range []string{"a", " ", "\n", strings.Repeat("\ud55c", MaxDocFindQueryRunes)} {
		if err := ValidateDocFindQuery(query); err != nil {
			test.Fatal(err)
		}
	}
	for _, query := range []string{"", "\xff", strings.Repeat("a", MaxDocFindQueryRunes+1)} {
		if err := ValidateDocFindQuery(query); !errors.Is(err, ErrValidation) {
			test.Fatal("invalid query accepted", err)
		}
	}
	if _, err := FindDocText(context.Background(), "\ud55c\uae00", "\uae00", 1, 20); !errors.Is(err, ErrValidation) {
		test.Fatal("split rune accepted", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := FindDocText(ctx, strings.Repeat("a", 128<<10), "z", 0, 20); !errors.Is(err, context.Canceled) {
		test.Fatal(err)
	}
}

func TestDocFindTextsIncludeDecodedToolsButNotLockedState(test *testing.T) {
	event := CIREvent{Kind: EventToolCall, ToolName: "shell", ProviderToolName: "Bash", Input: map[string]any{
		"z": []any{"\ub05d", true}, "command": "printf \"\uc548\ub155\"\nnext", "count": float64(3), "empty": nil,
	}, Locked: &LockedBlob{Provider: ProviderCodex, Scheme: "encrypted", Blob: "not searchable"}}
	want := []string{"Bash", "command", "printf \"\uc548\ub155\"\nnext", "count", "3", "empty", "null", "z", "\ub05d", "true"}
	if got := DocFindTexts(event); !reflect.DeepEqual(got, want) {
		test.Fatal(got, want)
	}
	event.Kind, event.RedactedSummary = EventReasoning, "Visible summary"
	if got := DocFindTexts(event); !reflect.DeepEqual(got, []string{"Visible summary"}) {
		test.Fatal("locked reasoning leaked into find fields", got)
	}
	event.Kind, event.Output = EventToolResult, "\ud55c\uae00 <decoded>\nnew line"
	if got := DocFindTexts(event); !reflect.DeepEqual(got, []string{"\ud55c\uae00 <decoded>\nnew line"}) {
		test.Fatal(got)
	}
}

func TestDocFindPreviewBoundariesAndPageWireBudget(test *testing.T) {
	text := strings.Repeat("\uc55e🙂", 100) + strings.Repeat("<", MaxDocFindQueryRunes) + strings.Repeat("\ub4a4🙂", 100)
	match := DocFindMatch{Offset: len(strings.Repeat("\uc55e🙂", 100)), Length: MaxDocFindQueryRunes}
	preview, err := WithDocFindPreview(text, match)
	if err != nil || utf8.RuneCountInString(preview.Preview) != MaxDocFindQueryRunes+64 ||
		preview.PreviewOffset >= match.Offset || !utf8.ValidString(preview.Preview) ||
		preview.Preview[match.Offset-preview.PreviewOffset:match.Offset-preview.PreviewOffset+match.Length] != strings.Repeat("<", MaxDocFindQueryRunes) {
		test.Fatal("preview lost exact UTF-8 match or exceeded its context", preview, err)
	}
	page := DocFindPage{Projection: DocFindProjection}
	next := strings.Repeat("A", base64.RawURLEncoding.EncodedLen(MaxDocFindSelectorBytes))
	page.NextCursor = &next
	for index := 0; index < DocFindPageMatches; index++ {
		value := preview
		value.RevealCursor = strings.Repeat("A", base64.RawURLEncoding.EncodedLen(MaxDocVisibleSelectorBytes))
		page.Matches = append(page.Matches, value)
	}
	raw, err := json.Marshal(page)
	if err != nil || len(raw) > 128<<10 {
		test.Fatal("maximum bounded previews/selectors exceed the page budget", len(raw), err)
	}
	for _, invalid := range []DocFindMatch{{Offset: -1, Length: 1}, {Offset: 1, Length: 1}, {Offset: len(text), Length: 1},
		{Offset: match.Offset, Length: MaxDocFindQueryRunes + 4}} {
		if _, err := WithDocFindPreview(text, invalid); !errors.Is(err, ErrValidation) {
			test.Fatal("invalid preview span accepted", invalid, err)
		}
	}
}
