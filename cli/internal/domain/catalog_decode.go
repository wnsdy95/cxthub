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

// DecodeCatalogPage rejects ambiguous or newer wire schemas before any cache
// write. Ordinary encoding/json decoding accepts duplicate/case-folded fields
// and null scalars and silently drops unknown fields; a v1 acquisition cannot.
// Value remains raw, but each known entity is also strictly decoded/validated.
func DecodeCatalogPage(raw []byte) (CatalogPage, error) {
	var page CatalogPage
	if err := decodeCatalogJSON(raw, &page); err != nil {
		return CatalogPage{}, err
	}
	var after *CatalogCheckpoint
	if page.Mode == "delta" {
		// All delta entries are after a nonnegative checkpoint. The caller must
		// additionally bind them to its actual checkpoint and preceding page.
		after = &CatalogCheckpoint{Version: CatalogVersion, RepoID: page.RepoID, Epoch: page.Epoch}
	}
	if err := ValidateCatalogPage(page.RepoID, after, nil, page); err != nil {
		return CatalogPage{}, err
	}
	return page, nil
}

func decodeCatalogJSON(raw []byte, out any) error {
	if !utf8.Valid(raw) {
		return catalogInvalid("invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := catalogJSONShape(decoder, reflect.TypeOf(out).Elem()); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return catalogInvalid("trailing JSON")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return catalogInvalid("invalid JSON field value")
	}
	return nil
}

type catalogJSONField struct {
	typ      reflect.Type
	required bool
}

var catalogJSONSchemas sync.Map // map[reflect.Type]map[string]catalogJSONField

func catalogJSONFields(typ reflect.Type) map[string]catalogJSONField {
	if cached, ok := catalogJSONSchemas.Load(typ); ok {
		return cached.(map[string]catalogJSONField)
	}
	fields := make(map[string]catalogJSONField, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		tag := strings.Split(field.Tag.Get("json"), ",")
		fields[tag[0]] = catalogJSONField{typ: field.Type, required: len(tag) == 1}
	}
	actual, _ := catalogJSONSchemas.LoadOrStore(typ, fields)
	return actual.(map[string]catalogJSONField)
}

func catalogJSONShape(decoder *json.Decoder, typ reflect.Type) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeFor[json.RawMessage]() {
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return catalogInvalid("invalid or null entity value")
		}
		return nil // ValidateCatalogEntry checks the complete typed entity below.
	}
	token, err := decoder.Token()
	if err != nil || token == nil {
		return catalogInvalid("missing, invalid, or null JSON value")
	}
	if typ == reflect.TypeFor[time.Time]() {
		if _, ok := token.(string); !ok {
			return catalogInvalid("timestamp type")
		}
		return nil // time.Time.UnmarshalJSON validates the date, offset, and range.
	}
	switch typ.Kind() {
	case reflect.Struct:
		if token != json.Delim('{') {
			return catalogInvalid("expected object")
		}
		fields := catalogJSONFields(typ)
		seen := make(map[string]bool, len(fields))
		for decoder.More() {
			token, err := decoder.Token()
			key, isString := token.(string)
			field, known := fields[key]
			if err != nil || !isString || !known || seen[key] {
				return catalogInvalid("unknown, duplicate, or case-aliased JSON field")
			}
			seen[key] = true
			if typ == reflect.TypeFor[CatalogPage]() && key == "next_cursor" {
				value, err := decoder.Token()
				cursor, ok := value.(string)
				if err != nil || !ok || cursor == "" {
					return catalogInvalid("empty or invalid continuation cursor")
				}
				continue
			}
			if typ == reflect.TypeFor[Snapshot]() && field.typ.Kind() == reflect.Slice {
				// Existing snapshot JSON may encode absent root parents, models,
				// or graft parents as null. Preserve that wire representation;
				// only the catalog's Entries array must always be nonnull.
				var raw json.RawMessage
				if decoder.Decode(&raw) != nil {
					return catalogInvalid("invalid snapshot collection")
				}
				if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
					collection := reflect.New(field.typ).Interface()
					if err := decodeCatalogJSON(raw, collection); err != nil {
						return err
					}
				}
				continue
			}
			if err := catalogJSONShape(decoder, field.typ); err != nil {
				return err
			}
		}
		if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
			return catalogInvalid("unterminated object")
		}
		for key, field := range fields {
			if field.required && !seen[key] {
				return catalogInvalid("missing required JSON field " + key)
			}
		}
	case reflect.Slice:
		if token != json.Delim('[') {
			return catalogInvalid("expected array")
		}
		count := 0
		for decoder.More() {
			count++
			if typ.Elem() == reflect.TypeFor[CatalogEntry]() && count > MaxCatalogLimit {
				return catalogInvalid("entry count")
			}
			if err := catalogJSONShape(decoder, typ.Elem()); err != nil {
				return err
			}
		}
		if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
			return catalogInvalid("unterminated array")
		}
	case reflect.String:
		if _, ok := token.(string); !ok {
			return catalogInvalid("expected string")
		}
	case reflect.Bool:
		if _, ok := token.(bool); !ok {
			return catalogInvalid("expected boolean")
		}
	case reflect.Int, reflect.Int64, reflect.Uint64:
		if _, ok := token.(json.Number); !ok {
			return catalogInvalid("expected integer")
		}
	default:
		return catalogInvalid("unsupported JSON schema type")
	}
	return nil
}
