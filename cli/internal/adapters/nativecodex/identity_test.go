package nativecodex

import (
	"strings"
	"testing"
)

func TestFreshThreadRequiresUnambiguousEmptyHistory(t *testing.T) {
	const valid = `{"model":"m","modelProvider":"p","thread":{"id":"fresh","cwd":"/work","turns":[]}}`
	if got, ok := freshThread([]byte(valid), "/work", "p"); !ok || got.ID != "fresh" {
		t.Fatalf("valid identity rejected: %+v %v", got, ok)
	}
	for _, tc := range []struct{ name, from, to string }{
		{"missing turns", `,"turns":[]`, ""},
		{"null turns", `"turns":[]`, `"turns":null`},
		{"nonempty turns", `"turns":[]`, `"turns":[{}]`},
		{"duplicate turns", `"turns":[]`, `"turns":[{}],"turns":[]`},
		{"aliased turns", `"turns":[]`, `"Turns":[{}],"turns":[]`},
		{"aliased only turns", `"turns":[]`, `"Turns":[]`},
		{"duplicate thread", `"thread":`, `"thread":{"turns":[{}]},"thread":`},
		{"aliased thread", `"thread":`, `"Thread":{"turns":[{}]},"thread":`},
		{"duplicate model", `"model":"m"`, `"model":"old","model":"m"`},
		{"aliased model", `"model":"m"`, `"Model":"old","model":"m"`},
		{"aliased provider", `"modelProvider":"p"`, `"ModelProvider":"other","modelProvider":"p"`},
		{"duplicate id", `"id":"fresh"`, `"id":"existing","id":"fresh"`},
		{"aliased cwd", `"cwd":"/work"`, `"Cwd":"/other","cwd":"/work"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := freshThread([]byte(strings.Replace(valid, tc.from, tc.to, 1)), "/work", "p"); ok {
				t.Fatal("ambiguous or unproved empty history accepted")
			}
		})
	}
}
