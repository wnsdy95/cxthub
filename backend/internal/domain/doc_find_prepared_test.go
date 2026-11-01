package domain

import (
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestDocPreparedFindSelectorsPreserveIndependentGenerationAndOffsets(test *testing.T) {
	root, legacy := findTestSelector(), legacyFindTestSelector()
	prepared := DocFindPreparedProgress{Page: 17, Generation: HashContent([]byte("prepared descriptor"))}
	root.Version, root.Representation, root.Prepared = 3, DocFindPreparedRepresentation, &prepared
	legacy.Version, legacy.Representation, legacy.Prepared = 3, DocFindPreparedRepresentation, &prepared
	root.Field, root.Byte, legacy.Field, legacy.Byte = 0, 700_000, 0, 700_000
	encoded, err := root.Encode()
	if err != nil {
		test.Fatal(err)
	}
	decoded, err := DecodeDocFindSelector(encoded)
	if err != nil || !reflect.DeepEqual(decoded, root) || decoded.Selection.Generation == decoded.Prepared.Generation {
		test.Fatal(decoded, err)
	}
	encodedLegacy, err := legacy.Encode()
	if err != nil {
		test.Fatal(err)
	}
	decodedLegacy, err := DecodeDocLegacyFindSelector(encodedLegacy)
	if err != nil || !reflect.DeepEqual(decodedLegacy, legacy) {
		test.Fatal(decodedLegacy, err)
	}
	for _, pair := range []struct {
		encoded string
		decode  func(string) error
	}{{encoded, func(encoded string) error { _, err := DecodeDocFindSelector(encoded); return err }},
		{encodedLegacy, func(encoded string) error { _, err := DecodeDocLegacyFindSelector(encoded); return err }}} {
		raw, err := base64.RawURLEncoding.DecodeString(pair.encoded)
		if err != nil {
			test.Fatal(err)
		}
		for _, replacement := range [][2]string{
			{`"prepared":{`, `"prepared":{"unknown":0,`},
			{`"page":17`, `"page":17,"page":17`},
			{`"version":3`, `"version":2`},
			{`"representation":"display-prepared-v1"`, `"representation":"display-v1"`},
		} {
			mutated := strings.Replace(string(raw), replacement[0], replacement[1], 1)
			if mutated == string(raw) || !errors.Is(pair.decode(base64.RawURLEncoding.EncodeToString([]byte(mutated))), ErrValidation) {
				test.Fatal("mixed or noncanonical cursor accepted", mutated)
			}
		}
	}
}

func TestDocPreparedFindSelectorsRejectInvalidProgress(test *testing.T) {
	for name, mutate := range map[string]func(*DocFindSelector){
		"missing":            func(selector *DocFindSelector) { selector.Prepared = nil },
		"negative-page":      func(selector *DocFindSelector) { selector.Prepared.Page = -1 },
		"page-limit":         func(selector *DocFindSelector) { selector.Prepared.Page = MaxDocRangePages },
		"no-generation":      func(selector *DocFindSelector) { selector.Prepared.Generation = "" },
		"invalid-generation": func(selector *DocFindSelector) { selector.Prepared.Generation = "unverified" },
		"field":              func(selector *DocFindSelector) { selector.Field = 1 },
		"end-page":           func(selector *DocFindSelector) { selector.Index = selector.To },
		"preview-with-progress": func(selector *DocFindSelector) {
			selector.Version, selector.Representation = 2, DocFindDisplayRepresentation
		},
		"source-with-progress": func(selector *DocFindSelector) { selector.Version, selector.Representation = 1, "" },
		"small-event-overflow": func(selector *DocFindSelector) {
			selector.Prepared.Page, selector.Prepared.Generation, selector.Byte = 0, "", MaxDocFindDisplayBytes+1
		},
	} {
		test.Run(name, func(test *testing.T) {
			root := findTestSelector()
			root.Version, root.Representation = 3, DocFindPreparedRepresentation
			root.Prepared = &DocFindPreparedProgress{Page: 1, Generation: HashContent([]byte("prepared"))}
			mutate(&root)
			legacy := legacyFindTestSelector()
			legacy.Version, legacy.Representation, legacy.Prepared, legacy.Index, legacy.Field, legacy.Byte = root.Version, root.Representation, root.Prepared, root.Index, root.Field, root.Byte
			legacy.From, legacy.Inherited = 0, 0
			for _, selector := range []interface{ Validate() error }{root, legacy} {
				if err := selector.Validate(); !errors.Is(err, ErrValidation) {
					test.Fatal("invalid prepared state accepted", selector, err)
				}
			}
		})
	}
	root := findTestSelector()
	root.Version, root.Representation, root.Prepared, root.Index = 3, DocFindPreparedRepresentation, &DocFindPreparedProgress{}, root.To
	if err := root.Validate(); err != nil {
		test.Fatal("valid terminal prepared cursor rejected", err)
	}
}
