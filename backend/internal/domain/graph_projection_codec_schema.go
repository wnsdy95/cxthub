package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type graphProjectionBudget struct {
	ctx  context.Context
	size int
	rows int
}

type graphProjectionJSONField struct {
	definition reflect.StructField
	position   int
	required   bool
	quoted     bool
}

type graphProjectionJSONStructSchema struct {
	fields   map[string]graphProjectionJSONField
	required int
}

var graphProjectionJSONStructSchemas sync.Map

func graphProjectionJSONStruct(schema reflect.Type) (*graphProjectionJSONStructSchema, error) {
	if cached, exists := graphProjectionJSONStructSchemas.Load(schema); exists {
		return cached.(*graphProjectionJSONStructSchema), nil
	}
	fields, err := graphProjectionJSONFields(schema)
	if err != nil {
		return nil, err
	}
	compiled := &graphProjectionJSONStructSchema{fields: make(map[string]graphProjectionJSONField, len(fields))}
	for name, field := range fields {
		_, options, _ := strings.Cut(field.Tag.Get("json"), ",")
		required := !strings.Contains(options, "omitempty")
		compiled.fields[name] = graphProjectionJSONField{
			definition: field, position: len(compiled.fields), required: required,
			quoted: strings.Contains(field.Tag.Get("json"), ",string"),
		}
		if required {
			compiled.required++
		}
	}
	cached, _ := graphProjectionJSONStructSchemas.LoadOrStore(schema, compiled)
	return cached.(*graphProjectionJSONStructSchema), nil
}

func (budget *graphProjectionBudget) addBytes(count int) error {
	if err := budget.ctx.Err(); err != nil {
		return err
	}
	if count > MaxPendingProjectionOutputBytes-budget.size {
		return ErrRepositoryViewSourceLimit
	}
	budget.size += count
	return nil
}

func (budget *graphProjectionBudget) addRows(count int) error {
	if err := budget.ctx.Err(); err != nil {
		return err
	}
	if count > MaxPendingProjectionOutputRows-budget.rows {
		return ErrRepositoryViewSourceLimit
	}
	budget.rows += count
	return nil
}

func (budget *graphProjectionBudget) measureString(value string) error {
	if err := budget.addBytes(len(value) + 2); err != nil {
		return err
	}
	for offset := 0; offset < len(value); {
		if err := budget.ctx.Err(); err != nil {
			return err
		}
		character, width := utf8.DecodeRuneInString(value[offset:])
		if character == utf8.RuneError && width == 1 {
			return ErrIntegrity
		}
		extra := 0
		switch character {
		case '\\', '"', '\n', '\r', '\t', '\b', '\f':
			extra = 1
		case '<', '>', '&':
			extra = 5
		case '\u2028', '\u2029':
			extra = 3
		default:
			if character < 0x20 {
				extra = 5
			}
		}
		if extra != 0 {
			if err := budget.addBytes(extra); err != nil {
				return err
			}
		}
		offset += width
	}
	return nil
}

func (budget *graphProjectionBudget) measure(value reflect.Value) error {
	if err := budget.ctx.Err(); err != nil {
		return err
	}
	if value.Type() == reflect.TypeFor[time.Time]() {
		return budget.addBytes(40)
	}
	switch value.Kind() {
	case reflect.Pointer:
		if value.IsNil() {
			return budget.addBytes(4)
		}
		return budget.measure(value.Elem())
	case reflect.String:
		return budget.measureString(value.String())
	case reflect.Bool:
		return budget.addBytes(5)
	case reflect.Int, reflect.Int64, reflect.Uint64:
		return budget.addBytes(20)
	case reflect.Struct:
		if err := budget.addBytes(2); err != nil {
			return err
		}
		for index := 0; index < value.NumField(); index++ {
			if value.Type().Field(index).Anonymous {
				compiled, err := graphProjectionJSONStruct(value.Type())
				if err != nil {
					return err
				}
				for name, field := range compiled.fields {
					if err := budget.measureString(name); err != nil {
						return err
					}
					if err := budget.addBytes(2); err != nil {
						return err
					}
					if err := budget.measure(value.FieldByIndex(field.definition.Index)); err != nil {
						return err
					}
				}
				return nil
			}
		}
		for index := 0; index < value.NumField(); index++ {
			field := value.Type().Field(index)
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			if err := budget.measureString(name); err != nil {
				return err
			}
			if err := budget.addBytes(2); err != nil {
				return err
			}
			if err := budget.measure(value.Field(index)); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Map:
		if value.IsNil() {
			return budget.addBytes(4)
		}
		if err := budget.addRows(value.Len()); err != nil {
			return err
		}
		if err := budget.addBytes(2 + value.Len()); err != nil {
			return err
		}
		if value.Kind() == reflect.Slice {
			for index := 0; index < value.Len(); index++ {
				if err := budget.measure(value.Index(index)); err != nil {
					return err
				}
			}
			return nil
		}
		entries := value.MapRange()
		for entries.Next() {
			if err := budget.measure(entries.Key()); err != nil {
				return err
			}
			if err := budget.addBytes(1); err != nil {
				return err
			}
			if err := budget.measure(entries.Value()); err != nil {
				return err
			}
		}
	default:
		return ErrIntegrity
	}
	return nil
}

func graphProjectionJSONSchema(ctx context.Context, raw []byte, schema reflect.Type, budget *graphProjectionBudget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !utf8.Valid(raw) {
		return ErrIntegrity
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := graphProjectionJSONValue(decoder, schema, false, budget); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrIntegrity
	}
	return ctx.Err()
}

func graphProjectionJSONValue(decoder *json.Decoder, schema reflect.Type, quoted bool, budget *graphProjectionBudget) error {
	if err := budget.ctx.Err(); err != nil {
		return err
	}
	token, err := decoder.Token()
	if err != nil {
		return ErrIntegrity
	}
	if token == nil {
		if schema.Kind() == reflect.Slice || schema.Kind() == reflect.Map || schema.Kind() == reflect.Pointer {
			return nil
		}
		return ErrIntegrity
	}
	for schema.Kind() == reflect.Pointer {
		schema = schema.Elem()
	}
	if quoted || schema.Kind() == reflect.String || schema == reflect.TypeFor[time.Time]() {
		if _, valid := token.(string); !valid {
			return ErrIntegrity
		}
		return nil
	}
	switch schema.Kind() {
	case reflect.Bool:
		if _, valid := token.(bool); !valid {
			return ErrIntegrity
		}
		return nil
	case reflect.Int, reflect.Int64, reflect.Uint64:
		if _, valid := token.(json.Number); !valid {
			return ErrIntegrity
		}
		return nil
	case reflect.Slice:
		if token != json.Delim('[') {
			return ErrIntegrity
		}
		for decoder.More() {
			if err := budget.addRows(1); err != nil {
				return err
			}
			if err := graphProjectionJSONValue(decoder, schema.Elem(), false, budget); err != nil {
				return err
			}
		}
		if closing, err := decoder.Token(); err != nil || closing != json.Delim(']') {
			return ErrIntegrity
		}
		return nil
	case reflect.Map, reflect.Struct:
		if token != json.Delim('{') {
			return ErrIntegrity
		}
		var compiled *graphProjectionJSONStructSchema
		var seen []bool
		required := 0
		if schema.Kind() == reflect.Struct {
			compiled, err = graphProjectionJSONStruct(schema)
			if err != nil {
				return err
			}
			seen = make([]bool, len(compiled.fields))
			required = compiled.required
		}
		for decoder.More() {
			keyToken, err := decoder.Token()
			key, valid := keyToken.(string)
			if err != nil || !valid {
				return ErrIntegrity
			}
			if schema.Kind() == reflect.Map {
				if err := budget.addRows(1); err != nil {
					return err
				}
				if err := graphProjectionJSONValue(decoder, schema.Elem(), false, budget); err != nil {
					return err
				}
				continue
			}
			field, exists := compiled.fields[key]
			if !exists || seen[field.position] {
				return ErrIntegrity
			}
			seen[field.position] = true
			if field.required {
				required--
			}
			if err := graphProjectionJSONValue(decoder, field.definition.Type, field.quoted, budget); err != nil {
				return err
			}
		}
		if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') || required != 0 {
			return ErrIntegrity
		}
		return nil
	default:
		return ErrIntegrity
	}
}

func graphProjectionJSONFields(schema reflect.Type) (map[string]reflect.StructField, error) {
	fields := map[string]reflect.StructField{}
	ambiguous := map[string]bool{}
	for index := 0; index < schema.NumField(); index++ {
		field := schema.Field(index)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if (!field.IsExported() && !(field.Anonymous && field.Type.Kind() == reflect.Struct)) || name == "-" {
			continue
		}
		if field.Anonymous && name == "" && field.Type.Kind() == reflect.Struct {
			embedded, err := graphProjectionJSONFields(field.Type)
			if err != nil {
				return nil, err
			}
			for key, nested := range embedded {
				nested.Index = append([]int{index}, nested.Index...)
				prior, exists := fields[key]
				if !exists || len(nested.Index) < len(prior.Index) {
					fields[key], ambiguous[key] = nested, false
				} else if len(nested.Index) == len(prior.Index) {
					ambiguous[key] = true
				}
			}
			continue
		}
		if name == "" {
			return nil, ErrIntegrity
		}
		if prior, exists := fields[name]; exists && len(prior.Index) == 1 {
			return nil, ErrIntegrity
		}
		fields[name], ambiguous[name] = field, false
	}
	for _, conflict := range ambiguous {
		if conflict {
			return nil, ErrIntegrity
		}
	}
	return fields, nil
}
