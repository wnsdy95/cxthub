package domain

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Mirror the CLI's strict catalog entity decoder for backend Merkle inputs.
func decodeCatalogMerkleJSON(raw []byte, out any) error {
	if !utf8.Valid(raw) {
		return catalogMerkleInvalid("invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := catalogMerkleJSONShape(decoder, reflect.TypeOf(out).Elem()); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return catalogMerkleInvalid("trailing JSON")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return catalogMerkleInvalid("invalid JSON field value")
	}
	return nil
}

type catalogMerkleJSONField struct {
	typ      reflect.Type
	required bool
}

var catalogMerkleJSONSchemas sync.Map // map[reflect.Type]map[string]catalogMerkleJSONField

func catalogMerkleJSONFields(typ reflect.Type) map[string]catalogMerkleJSONField {
	if cached, ok := catalogMerkleJSONSchemas.Load(typ); ok {
		return cached.(map[string]catalogMerkleJSONField)
	}
	fields := make(map[string]catalogMerkleJSONField, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		tag := strings.Split(field.Tag.Get("json"), ",")
		fields[tag[0]] = catalogMerkleJSONField{typ: field.Type, required: len(tag) == 1}
	}
	actual, _ := catalogMerkleJSONSchemas.LoadOrStore(typ, fields)
	return actual.(map[string]catalogMerkleJSONField)
}

func catalogMerkleJSONShape(decoder *json.Decoder, typ reflect.Type) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeFor[json.RawMessage]() {
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return catalogMerkleInvalid("invalid or null entity value")
		}
		return nil // ValidateCatalogEntry checks the complete typed entity below.
	}
	token, err := decoder.Token()
	if err != nil || token == nil {
		return catalogMerkleInvalid("missing, invalid, or null JSON value")
	}
	if typ == reflect.TypeFor[time.Time]() {
		if _, ok := token.(string); !ok {
			return catalogMerkleInvalid("timestamp type")
		}
		return nil // time.Time.UnmarshalJSON validates the date, offset, and range.
	}
	switch typ.Kind() {
	case reflect.Struct:
		if token != json.Delim('{') {
			return catalogMerkleInvalid("expected object")
		}
		fields := catalogMerkleJSONFields(typ)
		seen := make(map[string]bool, len(fields))
		for decoder.More() {
			token, err := decoder.Token()
			key, isString := token.(string)
			field, known := fields[key]
			if err != nil || !isString || !known || seen[key] {
				return catalogMerkleInvalid("unknown, duplicate, or case-aliased JSON field")
			}
			seen[key] = true
			if typ == reflect.TypeFor[Snapshot]() && field.typ.Kind() == reflect.Slice {
				// Existing snapshot JSON may encode absent root parents, models,
				// or graft parents as null. Preserve that wire representation;
				// only the catalog's Entries array must always be nonnull.
				var raw json.RawMessage
				if decoder.Decode(&raw) != nil {
					return catalogMerkleInvalid("invalid snapshot collection")
				}
				if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
					collection := reflect.New(field.typ).Interface()
					if err := decodeCatalogMerkleJSON(raw, collection); err != nil {
						return err
					}
				}
				continue
			}
			if err := catalogMerkleJSONShape(decoder, field.typ); err != nil {
				return err
			}
		}
		if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
			return catalogMerkleInvalid("unterminated object")
		}
		for key, field := range fields {
			if field.required && !seen[key] {
				return catalogMerkleInvalid("missing required JSON field " + key)
			}
		}
	case reflect.Slice:
		if token != json.Delim('[') {
			return catalogMerkleInvalid("expected array")
		}
		count := 0
		for decoder.More() {
			count++
			if typ.Elem() == reflect.TypeFor[CatalogEntry]() && count > MaxCatalogLimit {
				return catalogMerkleInvalid("entry count")
			}
			if err := catalogMerkleJSONShape(decoder, typ.Elem()); err != nil {
				return err
			}
		}
		if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
			return catalogMerkleInvalid("unterminated array")
		}
	case reflect.String:
		if _, ok := token.(string); !ok {
			return catalogMerkleInvalid("expected string")
		}
	case reflect.Bool:
		if _, ok := token.(bool); !ok {
			return catalogMerkleInvalid("expected boolean")
		}
	case reflect.Int, reflect.Int64, reflect.Uint64:
		if _, ok := token.(json.Number); !ok {
			return catalogMerkleInvalid("expected integer")
		}
	default:
		return catalogMerkleInvalid("unsupported JSON schema type")
	}
	return nil
}
