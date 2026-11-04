package domain

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func legacyFindTestSelector() DocLegacyFindSelector {
	return DocLegacyFindSelector{Version: 1, Repo: HashContent([]byte("repo")), Hash: HashContent([]byte("doc")),
		Base: HashContent([]byte("base")), Total: 30, Inherited: 4, From: 4, To: 30, Index: 7, Field: 2, Byte: 19,
		QueryHash: HashContent([]byte("needle"))}
}

func TestDocLegacyFindSelectorCanonicalWire(test *testing.T) {
	selector := legacyFindTestSelector()
	encoded, err := selector.Encode()
	if err != nil {
		test.Fatal(err)
	}
	decoded, err := DecodeDocLegacyFindSelector(encoded)
	if err != nil || decoded != selector {
		test.Fatal(decoded, err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		test.Fatal(err)
	}
	want := fmt.Sprintf(`{"base":%q,"byte_offset":19,"event_index":7,"field":2,"from":4,"hash":%q,"inherited":4,"query_hash":%q,"repo":%q,"to":30,"total":30,"version":1}`,
		selector.Base, selector.Hash, selector.QueryHash, selector.Repo)
	if string(raw) != want || strings.Contains(encoded, "=") || DocLegacyFindProjection != "find-legacy-v1" {
		test.Fatalf("legacy wire changed: %s", raw)
	}
}

func TestDocLegacyFindSelectorBounds(test *testing.T) {
	for _, mutate := range []func(*DocLegacyFindSelector){
		func(value *DocLegacyFindSelector) { value.Base = "" },
		func(value *DocLegacyFindSelector) { value.Inherited = 0; value.From = 0; value.Index = 0 },
		func(value *DocLegacyFindSelector) { value.Inherited = value.Total },
		func(value *DocLegacyFindSelector) { value.Index = value.From },
		func(value *DocLegacyFindSelector) { value.Index = value.To; value.Field = 0; value.Byte = 0 },
		func(value *DocLegacyFindSelector) {
			value.To = value.From
			value.Index = value.From
			value.Field = 0
			value.Byte = 0
		},
		func(value *DocLegacyFindSelector) {
			value.Total, value.Inherited, value.From, value.To, value.Index, value.Field, value.Byte = 0, 0, 0, 0, 0, 0, 0
		},
		func(value *DocLegacyFindSelector) {
			value.Total, value.Inherited, value.To = MaxConversationDocumentBytes, MaxConversationDocumentBytes, MaxConversationDocumentBytes
			value.From, value.Index, value.Field, value.Byte = value.To-2, value.To-1, 128<<10, MaxConversationDocumentBytes
		},
	} {
		selector := legacyFindTestSelector()
		mutate(&selector)
		encoded, err := selector.Encode()
		if err != nil {
			test.Fatal("valid boundary rejected", selector, err)
		}
		decoded, err := DecodeDocLegacyFindSelector(encoded)
		if err != nil || decoded != selector {
			test.Fatal("boundary failed round trip", decoded, err)
		}
	}
	invalid := map[string]func(*DocLegacyFindSelector){
		"version-zero":       func(value *DocLegacyFindSelector) { value.Version = 0 },
		"version-next":       func(value *DocLegacyFindSelector) { value.Version = 2 },
		"repo-empty":         func(value *DocLegacyFindSelector) { value.Repo = "" },
		"repo-invalid":       func(value *DocLegacyFindSelector) { value.Repo = "invalid" },
		"repo-uppercase":     func(value *DocLegacyFindSelector) { value.Repo = ContentHash(strings.ToUpper(string(value.Repo))) },
		"hash-empty":         func(value *DocLegacyFindSelector) { value.Hash = "" },
		"hash-invalid":       func(value *DocLegacyFindSelector) { value.Hash = "invalid" },
		"base-invalid":       func(value *DocLegacyFindSelector) { value.Base = "invalid" },
		"base-uppercase":     func(value *DocLegacyFindSelector) { value.Base = ContentHash(strings.ToUpper(string(value.Base))) },
		"query-hash-empty":   func(value *DocLegacyFindSelector) { value.QueryHash = "" },
		"query-hash-invalid": func(value *DocLegacyFindSelector) { value.QueryHash = "invalid" },
		"total-negative":     func(value *DocLegacyFindSelector) { value.Total = -1 },
		"total-over-limit":   func(value *DocLegacyFindSelector) { value.Total = MaxConversationDocumentBytes + 1 },
		"inherited-negative": func(value *DocLegacyFindSelector) { value.Inherited = -1 },
		"inherited-over":     func(value *DocLegacyFindSelector) { value.Inherited = value.Total + 1 },
		"from-negative":      func(value *DocLegacyFindSelector) { value.From = -1 },
		"from-after-to":      func(value *DocLegacyFindSelector) { value.From = value.To + 1 },
		"to-negative":        func(value *DocLegacyFindSelector) { value.To = -1 },
		"to-over-total":      func(value *DocLegacyFindSelector) { value.To = value.Total + 1 },
		"index-before-from":  func(value *DocLegacyFindSelector) { value.Index = value.From - 1 },
		"index-after-to":     func(value *DocLegacyFindSelector) { value.Index = value.To + 1 },
		"field-negative":     func(value *DocLegacyFindSelector) { value.Field = -1 },
		"field-over-limit":   func(value *DocLegacyFindSelector) { value.Field = (128 << 10) + 1 },
		"byte-negative":      func(value *DocLegacyFindSelector) { value.Byte = -1 },
		"byte-over-limit":    func(value *DocLegacyFindSelector) { value.Byte = MaxConversationDocumentBytes + 1 },
		"terminal-field":     func(value *DocLegacyFindSelector) { value.Index = value.To; value.Byte = 0 },
		"terminal-byte":      func(value *DocLegacyFindSelector) { value.Index = value.To; value.Field = 0 },
	}
	for name, mutate := range invalid {
		test.Run(name, func(test *testing.T) {
			selector := legacyFindTestSelector()
			mutate(&selector)
			if err := selector.Validate(); !errors.Is(err, ErrValidation) {
				test.Fatal("invalid selector validated", selector, err)
			}
			if encoded, err := selector.Encode(); !errors.Is(err, ErrValidation) || encoded != "" {
				test.Fatal("invalid selector encoded", encoded, err)
			}
			raw, err := canonicalJSON(selector)
			if err != nil {
				test.Fatal(err)
			}
			if decoded, err := DecodeDocLegacyFindSelector(base64.RawURLEncoding.EncodeToString(raw)); !errors.Is(err, ErrValidation) || decoded != (DocLegacyFindSelector{}) {
				test.Fatal("invalid selector decoded", decoded, err)
			}
		})
	}
}

func TestDocLegacyFindSelectorRejectsNoncanonicalTokens(test *testing.T) {
	selector := legacyFindTestSelector()
	encoded, err := selector.Encode()
	if err != nil {
		test.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		test.Fatal(err)
	}
	invalid := []string{"", "!", "e30", encoded + "=", " " + encoded, encoded + "\n", encoded[:8] + "\r\n" + encoded[8:],
		strings.Repeat("A", base64.RawURLEncoding.EncodedLen(MaxDocLegacyFindSelectorBytes)+1),
		base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat(" ", MaxDocLegacyFindSelectorBytes+1)))}
	for _, value := range []string{" " + string(raw), string(raw) + "\n", string(raw) + "{}", "null", "[]", "{",
		strings.Replace(string(raw), `{`, `{"unknown":0,`, 1),
		strings.Replace(string(raw), `"version":1`, `"version":1.0`, 1),
		strings.Replace(string(raw), `"version":1`, `"version":1e0`, 1),
		strings.Replace(string(raw), `"version":1`, `"version":"1"`, 1),
		strings.Replace(string(raw), `"field":2`, `"field":null`, 1),
		strings.Replace(string(raw), `"event_index":7`, `"index":7`, 1),
		strings.Replace(string(raw), `"byte_offset":19`, `"byte":19`, 1),
		strings.Replace(string(raw), `"repo":`, `"Repo":`, 1),
		strings.Replace(string(raw), `"repo":`, `"re\u0070o":`, 1),
		strings.Replace(string(raw), `"sha256:`, `"\u0073ha256:`, 1),
	} {
		invalid = append(invalid, base64.RawURLEncoding.EncodeToString([]byte(value)))
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		test.Fatal(err)
	}
	for key, value := range fields {
		duplicate := append([]byte(fmt.Sprintf(`{%q:%s,`, key, value)), raw[1:]...)
		invalid = append(invalid, base64.RawURLEncoding.EncodeToString(duplicate))
		missing := strings.Replace(string(raw), fmt.Sprintf(`%q:%s,`, key, value), "", 1)
		if missing == string(raw) {
			missing = strings.Replace(string(raw), fmt.Sprintf(`,%q:%s`, key, value), "", 1)
		}
		invalid = append(invalid, base64.RawURLEncoding.EncodeToString([]byte(missing)))
	}
	for index, value := range invalid {
		test.Run(fmt.Sprintf("token-%d", index), func(test *testing.T) {
			if decoded, err := DecodeDocLegacyFindSelector(value); !errors.Is(err, ErrValidation) || decoded != (DocLegacyFindSelector{}) {
				test.Fatal("noncanonical selector accepted", decoded, err)
			}
		})
	}
	zero := selector
	zero.Base, zero.Inherited, zero.From, zero.Index, zero.Field, zero.Byte = "", 0, 0, 0, 0, 0
	canonical, err := canonicalJSON(zero)
	if err != nil {
		test.Fatal(err)
	}
	for _, field := range []string{`"base":"",`, `"inherited":0,`, `"from":0,`, `"event_index":0,`, `"field":0,`, `"byte_offset":0,`} {
		missing := strings.Replace(string(canonical), field, "", 1)
		if _, err := DecodeDocLegacyFindSelector(base64.RawURLEncoding.EncodeToString([]byte(missing))); !errors.Is(err, ErrValidation) {
			test.Fatal("missing zero-valued field accepted", field, err)
		}
	}
}

func TestDocLegacyFindSelectorMaximumSizeAndTokenIsolation(test *testing.T) {
	selector := legacyFindTestSelector()
	selector.Total, selector.Inherited, selector.To = MaxConversationDocumentBytes, MaxConversationDocumentBytes, MaxConversationDocumentBytes
	selector.From, selector.Index, selector.Field, selector.Byte = selector.To-2, selector.To-1, 128<<10, MaxConversationDocumentBytes
	encoded, err := selector.Encode()
	if err != nil {
		test.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || MaxDocLegacyFindSelectorBytes != 2048 || len(raw) > MaxDocLegacyFindSelectorBytes || len(encoded) > 2731 {
		test.Fatal("legacy selector exceeds explicit 2048-byte JSON/2731-byte token bound", len(raw), len(encoded), err)
	}
	if _, err := DecodeDocFindSelector(encoded); !errors.Is(err, ErrValidation) {
		test.Fatal("root accepted legacy token", err)
	}
	if _, err := DecodeDocVisibleSelector(encoded); !errors.Is(err, ErrValidation) {
		test.Fatal("visible selector accepted legacy token", err)
	}
	root := findTestSelector()
	rootEncoded, err := root.Encode()
	if err != nil {
		test.Fatal(err)
	}
	visibleEncoded, err := root.Selection.Encode()
	if err != nil {
		test.Fatal(err)
	}
	for _, token := range []string{rootEncoded, visibleEncoded} {
		if _, err := DecodeDocLegacyFindSelector(token); !errors.Is(err, ErrValidation) {
			test.Fatal("legacy accepted root/visible token", err)
		}
	}
}

func TestDocLegacyFindSelectionBindsRequest(test *testing.T) {
	selector := legacyFindTestSelector()
	initial := DocFindPageRequest{Legacy: true, Query: "needle", Base: selector.Base, Total: selector.Total,
		Inherited: selector.Inherited, From: selector.From, To: selector.To}
	want := selector
	want.Index, want.Field, want.Byte = want.From, 0, 0
	if got, err := DocLegacyFindSelection(selector.Repo, selector.Hash, initial); err != nil || got != want {
		test.Fatal("initial request not bound", got, err)
	}
	cursor, err := selector.Encode()
	if err != nil {
		test.Fatal(err)
	}
	continuation := DocFindPageRequest{Legacy: true, Query: "needle", Cursor: cursor}
	if got, err := DocLegacyFindSelection(selector.Repo, selector.Hash, continuation); err != nil || got != selector {
		test.Fatal("continuation not bound", got, err)
	}
	for _, request := range []DocFindPageRequest{initial, continuation} {
		for _, mutate := range []func(*DocFindPageRequest){
			func(value *DocFindPageRequest) { value.Legacy = false },
			func(value *DocFindPageRequest) { value.Selection = "root-selection" },
			func(value *DocFindPageRequest) { value.Query = "" },
			func(value *DocFindPageRequest) { value.Query = "\xff" },
			func(value *DocFindPageRequest) { value.Query = strings.Repeat("a", MaxDocFindQueryRunes+1) },
		} {
			invalid := request
			mutate(&invalid)
			if got, err := DocLegacyFindSelection(selector.Repo, selector.Hash, invalid); !errors.Is(err, ErrValidation) || got != (DocLegacyFindSelector{}) {
				test.Fatal("invalid request accepted", invalid, got, err)
			}
		}
		for _, route := range [][2]ContentHash{{"invalid", selector.Hash}, {selector.Repo, "invalid"}} {
			if _, err := DocLegacyFindSelection(route[0], route[1], request); !errors.Is(err, ErrValidation) {
				test.Fatal("invalid route accepted", route, err)
			}
		}
	}
	for _, mutate := range []func(*DocFindPageRequest){
		func(value *DocFindPageRequest) { value.Base = selector.Base },
		func(value *DocFindPageRequest) { value.Total = selector.Total },
		func(value *DocFindPageRequest) { value.Inherited = selector.Inherited },
		func(value *DocFindPageRequest) { value.From = selector.From },
		func(value *DocFindPageRequest) { value.To = selector.To },
		func(value *DocFindPageRequest) { value.Total = -1 },
		func(value *DocFindPageRequest) { value.Query = "NEEDLE" },
		func(value *DocFindPageRequest) { value.Cursor += "=" },
	} {
		invalid := continuation
		mutate(&invalid)
		if _, err := DocLegacyFindSelection(selector.Repo, selector.Hash, invalid); !errors.Is(err, ErrValidation) {
			test.Fatal("continuation accepted conflicting request fields", invalid, err)
		}
	}
	for _, route := range [][2]ContentHash{{HashContent([]byte("other repo")), selector.Hash}, {selector.Repo, HashContent([]byte("other doc"))}} {
		if _, err := DocLegacyFindSelection(route[0], route[1], continuation); !errors.Is(err, ErrValidation) {
			test.Fatal("continuation accepted foreign route", route, err)
		}
	}
	initial.To = initial.Total + 1
	if _, err := DocLegacyFindSelection(selector.Repo, selector.Hash, initial); !errors.Is(err, ErrValidation) {
		test.Fatal("invalid initial bounds accepted", err)
	}
}
