package domain

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestDocFindRepresentationConstants(test *testing.T) {
	if DocFindDisplayRepresentation != "display-v1" || DocFindDisplayProjection != "find-display-v1" ||
		DocLegacyFindDisplayProjection != "find-legacy-display-v1" || DocFindProjection != "find-v1" || DocLegacyFindProjection != "find-legacy-v1" {
		test.Fatal("find representation or projection contract changed")
	}
}

func TestDocFindRepresentationSourceCanonicalBytes(test *testing.T) {
	root, legacy := findTestSelector(), legacyFindTestSelector()
	rootJSON := fmt.Sprintf(`{"byte":0,"field":0,"from":0,"index":0,"query_hash":%q,"selection":{"base":"","generation":%q,"hash":%q,"inherited":0,"limit":20,"next":0,"purpose":"units","repo":%q,"start":0,"total":30,"unit":0,"version":1,"view":"chat"},"to":30,"version":1}`,
		root.QueryHash, root.Selection.Generation, root.Selection.Hash, root.Selection.Repo)
	legacyJSON := fmt.Sprintf(`{"base":%q,"byte_offset":19,"event_index":7,"field":2,"from":4,"hash":%q,"inherited":4,"query_hash":%q,"repo":%q,"to":30,"total":30,"version":1}`,
		legacy.Base, legacy.Hash, legacy.QueryHash, legacy.Repo)
	for _, fixture := range []struct {
		name     string
		selector interface{ Encode() (string, error) }
		want     string
	}{{"root", root, rootJSON}, {"legacy", legacy, legacyJSON}} {
		test.Run(fixture.name, func(test *testing.T) {
			encoded, err := fixture.selector.Encode()
			if err != nil || encoded != base64.RawURLEncoding.EncodeToString([]byte(fixture.want)) {
				test.Fatal("source selector canonical bytes changed", encoded, err)
			}
		})
	}
}

func TestDocFindSelectorRepresentationVersionBinding(test *testing.T) {
	for _, fixture := range []struct {
		version        int
		representation string
		valid          bool
	}{
		{1, "", true}, {2, DocFindDisplayRepresentation, true},
		{0, "", false}, {2, "", false}, {3, "", false},
		{0, DocFindDisplayRepresentation, false}, {1, DocFindDisplayRepresentation, false}, {3, DocFindDisplayRepresentation, false},
		{1, "source-v1", false}, {2, "source-v1", false}, {2, "display-v2", false},
		{2, "DISPLAY-V1", false}, {2, " display-v1", false}, {2, "display-v1 ", false},
	} {
		test.Run(fmt.Sprintf("version-%d/%q", fixture.version, fixture.representation), func(test *testing.T) {
			root := findTestSelector()
			root.Version, root.Representation = fixture.version, fixture.representation
			legacy := legacyFindTestSelector()
			legacy.Version, legacy.Representation = fixture.version, fixture.representation
			if fixture.representation == DocFindDisplayRepresentation {
				legacy.Field = 0
			}
			for _, selector := range []interface {
				Validate() error
				Encode() (string, error)
			}{root, legacy} {
				if err := selector.Validate(); (err == nil) != fixture.valid || err != nil && !errors.Is(err, ErrValidation) {
					test.Fatal("representation/version validation changed", selector, err)
				}
				if encoded, err := selector.Encode(); (err == nil) != fixture.valid || !fixture.valid && (encoded != "" || !errors.Is(err, ErrValidation)) {
					test.Fatal("representation/version encoding changed", selector, encoded, err)
				}
			}
			rootRaw, err := canonicalJSON(root)
			if err != nil {
				test.Fatal(err)
			}
			decodedRoot, rootErr := DecodeDocFindSelector(base64.RawURLEncoding.EncodeToString(rootRaw))
			legacyRaw, err := canonicalJSON(legacy)
			if err != nil {
				test.Fatal(err)
			}
			decodedLegacy, legacyErr := DecodeDocLegacyFindSelector(base64.RawURLEncoding.EncodeToString(legacyRaw))
			if fixture.valid {
				if rootErr != nil || decodedRoot != root || legacyErr != nil || decodedLegacy != legacy {
					test.Fatal("find representation did not round trip", decodedRoot, rootErr, decodedLegacy, legacyErr)
				}
			} else if !errors.Is(rootErr, ErrValidation) || decodedRoot != (DocFindSelector{}) ||
				!errors.Is(legacyErr, ErrValidation) || decodedLegacy != (DocLegacyFindSelector{}) {
				test.Fatal("forged representation/version decoded", decodedRoot, rootErr, decodedLegacy, legacyErr)
			}
		})
	}
}

func TestDocFindRepresentationRejectsNoncanonicalSelectors(test *testing.T) {
	for _, representation := range []string{"", DocFindDisplayRepresentation} {
		root, legacy := findTestSelector(), legacyFindTestSelector()
		root.Representation, legacy.Representation = representation, representation
		if representation != "" {
			root.Version, legacy.Version = 2, 2
			legacy.Field = 0
		}
		rootToken, err := root.Encode()
		if err != nil {
			test.Fatal(err)
		}
		legacyToken, err := legacy.Encode()
		if err != nil {
			test.Fatal(err)
		}
		for _, fixture := range []struct {
			name     string
			token    string
			foreign  string
			maxBytes int
			decode   func(string) error
		}{
			{"root", rootToken, legacyToken, MaxDocFindSelectorBytes, func(encoded string) error {
				_, err := DecodeDocFindSelector(encoded)
				return err
			}},
			{"legacy", legacyToken, rootToken, MaxDocLegacyFindSelectorBytes, func(encoded string) error {
				_, err := DecodeDocLegacyFindSelector(encoded)
				return err
			}},
		} {
			test.Run(fixture.name+"/"+representation, func(test *testing.T) {
				raw, err := base64.RawURLEncoding.DecodeString(fixture.token)
				if err != nil || len(raw) > fixture.maxBytes {
					test.Fatal("selector exceeds its bound", len(raw), err)
				}
				invalid := []string{"", fixture.foreign, fixture.token + "=", fixture.token + "\n",
					strings.Repeat("A", base64.RawURLEncoding.EncodedLen(fixture.maxBytes)+1),
					base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat(" ", fixture.maxBytes+1)))}
				for _, forged := range []string{
					" " + string(raw), string(raw) + "\n", string(raw) + "{}",
					strings.Replace(string(raw), `{`, `{"unknown":0,`, 1),
					strings.Replace(string(raw), `{`, `{"representation":"",`, 1),
					strings.Replace(string(raw), `{`, `{"representation":null,`, 1),
					strings.Replace(string(raw), `{`, `{"representation":"display-v1",`, 1),
					strings.Replace(string(raw), `{`, `{"version":1,`, 1),
					strings.Replace(string(raw), `"field":`, `"Field":`, 1),
				} {
					invalid = append(invalid, base64.RawURLEncoding.EncodeToString([]byte(forged)))
				}
				if representation != "" {
					for _, replacement := range [][2]string{
						{`"representation":"display-v1",`, ""},
						{`"representation":"display-v1"`, `"representation":""`},
						{`"representation":"display-v1"`, `"representation":null`},
						{`"representation":"display-v1"`, `"representation":"display-v2"`},
						{`"representation":"display-v1"`, `"Representation":"display-v1"`},
						{`"representation":"display-v1"`, `"repre\u0073entation":"display-v1"`},
						{`"version":2`, `"version":1`},
					} {
						forged := strings.Replace(string(raw), replacement[0], replacement[1], 1)
						invalid = append(invalid, base64.RawURLEncoding.EncodeToString([]byte(forged)))
					}
				}
				for index, encoded := range invalid {
					if err := fixture.decode(encoded); !errors.Is(err, ErrValidation) {
						test.Fatal("noncanonical or forged representation accepted", index, err)
					}
				}
			})
		}
	}
}

func TestDocLegacyFindSelectionRepresentationBinding(test *testing.T) {
	for _, representation := range []string{"", DocFindDisplayRepresentation} {
		test.Run(representation, func(test *testing.T) {
			selector := legacyFindTestSelector()
			selector.Representation = representation
			if representation != "" {
				selector.Version, selector.Field = 2, 0
			}
			initial := DocFindPageRequest{Legacy: true, Representation: representation, Query: "needle", Base: selector.Base,
				Total: selector.Total, Inherited: selector.Inherited, From: selector.From, To: selector.To}
			want := selector
			want.Index, want.Field, want.Byte = want.From, 0, 0
			if got, err := DocLegacyFindSelection(selector.Repo, selector.Hash, initial); err != nil || got != want {
				test.Fatal("initial representation lost source/range/query bindings", got, err)
			}
			cursor, err := selector.Encode()
			if err != nil {
				test.Fatal(err)
			}
			continuation := DocFindPageRequest{Legacy: true, Representation: representation, Query: "needle", Cursor: cursor}
			if got, err := DocLegacyFindSelection(selector.Repo, selector.Hash, continuation); err != nil || got != selector {
				test.Fatal("continuation representation lost selector identity", got, err)
			}
			for _, other := range []string{"", DocFindDisplayRepresentation, "source-v1", "display-v2", "DISPLAY-V1", " display-v1"} {
				if other == representation {
					continue
				}
				invalid := continuation
				invalid.Representation = other
				if got, err := DocLegacyFindSelection(selector.Repo, selector.Hash, invalid); !errors.Is(err, ErrValidation) || got != (DocLegacyFindSelector{}) {
					test.Fatal("continuation accepted a different representation", other, got, err)
				}
				if other != "" && other != DocFindDisplayRepresentation {
					invalid = initial
					invalid.Representation = other
					if _, err := DocLegacyFindSelection(selector.Repo, selector.Hash, invalid); !errors.Is(err, ErrValidation) {
						test.Fatal("initial request accepted unknown representation", other, err)
					}
				}
			}
			for _, mutate := range []func(*DocFindPageRequest){
				func(request *DocFindPageRequest) { request.Query = "NEEDLE" },
				func(request *DocFindPageRequest) { request.Legacy = false },
				func(request *DocFindPageRequest) { request.Selection = "selection" },
				func(request *DocFindPageRequest) { request.Base = selector.Base },
				func(request *DocFindPageRequest) { request.Total = selector.Total },
				func(request *DocFindPageRequest) { request.Inherited = selector.Inherited },
				func(request *DocFindPageRequest) { request.From = selector.From },
				func(request *DocFindPageRequest) { request.To = selector.To },
			} {
				invalid := continuation
				mutate(&invalid)
				if _, err := DocLegacyFindSelection(selector.Repo, selector.Hash, invalid); !errors.Is(err, ErrValidation) {
					test.Fatal("continuation accepted changed bindings", invalid, err)
				}
			}
			for _, route := range [][2]ContentHash{{HashContent([]byte("other repo")), selector.Hash}, {selector.Repo, HashContent([]byte("other doc"))}} {
				if _, err := DocLegacyFindSelection(route[0], route[1], continuation); !errors.Is(err, ErrValidation) {
					test.Fatal("continuation accepted a foreign source", route, err)
				}
			}
		})
	}
}
