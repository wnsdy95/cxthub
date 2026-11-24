package domain

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCollectorLine(t *testing.T) {
	for text, want := range map[string]string{
		"":                      "",
		"plain":                 "plain",
		"  two\twords \n":       "two words",
		"a\r\nb\x00c":           "a b c",
		"a b":                   "a b",
		strings.Repeat("x", 12): "xxxxxxxxxx",
		"12345678 0":            "12345678 0",
		"123456789 after":       "123456789",
		"12345678éé":            "12345678é",
		"1234567éé":             "1234567é",
		"éééééé":                "ééééé",
	} {
		got := CollectorLine(text, 10)
		if got != want || len(got) > 10 || !utf8.ValidString(got) {
			t.Errorf("CollectorLine(%q, 10) = %q, want %q", text, got, want)
		}
	}
}

func TestCollectorRunError(t *testing.T) {
	if said := CollectorRunError(nil); said != "" {
		t.Fatalf("a run that ended well: %q", said)
	}
	if said := CollectorRunError(errors.New("stopped after 1 of 2 session reports:\n the server refused")); said != "stopped after 1 of 2 session reports: the server refused" {
		t.Fatalf("what went wrong: %q", said)
	}
	if said := CollectorRunError(errors.New(strings.Repeat("e", CollectorErrorLimit+40))); len(said) != CollectorErrorLimit {
		t.Fatalf("a long error is %d bytes", len(said))
	}
	// A failed run is never reported as one that ended well.
	if said := CollectorRunError(errors.New(" \n ")); said == "" {
		t.Fatal("a failure without words was reported as nothing")
	}
}
