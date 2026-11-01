package domain

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func findFragmentTestSelection() DocVisibleSelector {
	return DocVisibleSelector{Version: 1, Purpose: "units", Repo: HashContent([]byte("repo")), Hash: HashContent([]byte("doc")),
		Generation: HashContent([]byte("generation")), View: "all", Start: 4, Next: 4, Unit: 4, Total: 12, Limit: 1}
}

func TestDocFindFragmentSelectionAcceptsRevealAndLegacy(test *testing.T) {
	selection := findFragmentTestSelection()
	encoded, err := selection.Encode()
	if err != nil {
		test.Fatal(err)
	}
	for _, budget := range []int{4, 65536} {
		request := DocFindFragmentRequest{Selection: encoded, Offset: 87, Budget: budget}
		decoded, err := DocFindFragmentSelection(selection.Repo, selection.Hash, request)
		if err != nil || decoded != selection {
			test.Fatal("matched reveal rejected", decoded, err)
		}
		request = DocFindFragmentRequest{Legacy: true, Index: 4, Total: 12, Offset: 87, Budget: budget}
		decoded, err = DocFindFragmentSelection(selection.Repo, selection.Hash, request)
		if err != nil || decoded != (DocVisibleSelector{}) {
			test.Fatal("legacy invented a generation", decoded, err)
		}
	}
}

func TestDocFindFragmentSelectionRejectsMixedScopeAndNoncanonicalTokens(test *testing.T) {
	selection := findFragmentTestSelection()
	encoded, err := selection.Encode()
	if err != nil {
		test.Fatal(err)
	}
	requests := []DocFindFragmentRequest{
		{}, {Selection: encoded, Budget: 3}, {Selection: encoded, Budget: 65537}, {Selection: encoded, Offset: -1, Budget: 4},
		{Selection: encoded, Index: 4, Budget: 4}, {Selection: encoded, Total: 12, Budget: 4},
		{Selection: encoded, Legacy: true, Total: 12, Budget: 4}, {Legacy: true, Index: -1, Total: 12, Budget: 4},
		{Legacy: true, Index: 12, Total: 12, Budget: 4}, {Legacy: true, Total: 0, Budget: 4},
		{Legacy: true, Total: MaxConversationDocumentBytes + 1, Budget: 4},
	}
	for _, change := range []func(*DocVisibleSelector){
		func(value *DocVisibleSelector) { value.Repo = HashContent([]byte("foreign repo")) },
		func(value *DocVisibleSelector) { value.Hash = HashContent([]byte("foreign doc")) },
		func(value *DocVisibleSelector) { value.Purpose = "fold" },
		func(value *DocVisibleSelector) { value.View = "chat" },
		func(value *DocVisibleSelector) { value.View = "prompts" },
		func(value *DocVisibleSelector) { value.Limit = 2 },
		func(value *DocVisibleSelector) { value.Start, value.Next, value.Unit = value.Total, value.Total, 0 },
	} {
		changed := selection
		change(&changed)
		token, err := changed.Encode()
		if err != nil {
			test.Fatal(err)
		}
		requests = append(requests, DocFindFragmentRequest{Selection: token, Budget: 4})
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		test.Fatal(err)
	}
	for _, token := range []string{
		encoded + "=", encoded + "\n", "*", strings.Repeat("x", 4096),
		base64.RawURLEncoding.EncodeToString(append([]byte(" "), raw...)),
		base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), `"version":1`, `"version":1,"version":1`, 1))),
		base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), `"version":1`, `"version":1,"unknown":0`, 1))),
	} {
		requests = append(requests, DocFindFragmentRequest{Selection: token, Budget: 4})
	}
	for _, request := range requests {
		decoded, err := DocFindFragmentSelection(selection.Repo, selection.Hash, request)
		if !errors.Is(err, ErrValidation) || decoded != (DocVisibleSelector{}) {
			test.Fatalf("invalid request accepted: %+v: %+v, %v", request, decoded, err)
		}
	}
	for _, pair := range [][2]ContentHash{{"bad", selection.Hash}, {selection.Repo, "bad"}} {
		if _, err := DocFindFragmentSelection(pair[0], pair[1], DocFindFragmentRequest{Legacy: true, Total: 1, Budget: 4}); !errors.Is(err, ErrValidation) {
			test.Fatal("invalid route accepted", err)
		}
	}
}

func TestDocFindFragmentWireContract(test *testing.T) {
	page := DocFindFragmentPage{Projection: DocLegacyFindFragmentProjection, Hash: HashContent([]byte("doc")), Index: 4, Offset: 7,
		Total: 23, JSON: "\ud55c🙂", Next: 14, Previous: 0}
	raw, err := json.Marshal(page)
	if err != nil {
		test.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		test.Fatal(err)
	}
	if len(fields) != 9 || string(fields["generation"]) != `""` || string(fields["event_index"]) != "4" || string(fields["byte_offset"]) != "7" || string(fields["total_bytes"]) != "23" {
		test.Fatal("wire fields changed", string(raw))
	}
	var decoded DocFindFragmentPage
	if err := json.Unmarshal(raw, &decoded); err != nil || !reflect.DeepEqual(decoded, page) {
		test.Fatal("wire roundtrip changed", decoded, err)
	}
}
