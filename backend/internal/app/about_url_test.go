package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestNormalizeAboutWebsiteContract(t *testing.T) {
	valid := map[string]string{
		"":                            "",
		"example.com":                 "https://example.com",
		"example.com:8080":            "https://example.com:8080",
		"localhost:3000":              "https://localhost:3000",
		"http://Example.COM/docs?q=1": "http://example.com/docs?q=1",
		"https://example.com/":        "https://example.com",
	}
	for input, want := range valid {
		got, err := normalizeAboutWebsite(input)
		if err != nil || got != want {
			t.Errorf("normalizeAboutWebsite(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{
		"javascript:alert(1)",
		"data:text/html,hello",
		"https://user:password@example.com",
		"https://example.com\nhttps://evil.example",
		"https://example.com\\@evil.example",
		strings.Repeat("a", 2049) + ".com",
	} {
		if _, err := normalizeAboutWebsite(input); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("normalizeAboutWebsite(%q) error = %v", input, err)
		}
	}
}
