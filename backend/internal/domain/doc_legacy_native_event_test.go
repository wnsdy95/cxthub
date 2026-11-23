package domain

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func legacyNativeEventDocument(version, events string) []byte {
	return []byte(fmt.Sprintf(`{"envelope":{"cir_version":%q},"events":[%s]}`, version, events))
}

func legacyNativeEventEscapedKey(name string) string {
	var key strings.Builder
	key.WriteByte('"')
	for _, character := range name {
		fmt.Fprintf(&key, `\u%04x`, character)
	}
	key.WriteByte('"')
	return key.String()
}

func TestLegacyNativeEventReflectedTagsAndFoldedNativeErrors(test *testing.T) {
	eventType := reflect.TypeOf(CIREvent{})
	for fieldIndex := 0; fieldIndex < eventType.NumField(); fieldIndex++ {
		field := eventType.Field(fieldIndex)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if field.PkgPath != "" || name == "" || name == "-" {
			continue
		}
		invalid := `false`
		fieldType := field.Type
		for fieldType.Kind() == reflect.Pointer {
			fieldType = fieldType.Elem()
		}
		if fieldType.Kind() == reflect.Bool {
			invalid = `{}`
		} else if field.Type.Kind() == reflect.Interface {
			invalid = `1e999`
		}
		keys := []struct {
			label string
			key   string
		}{
			{label: "exact", key: fmt.Sprintf("%q", name)},
			{label: "uppercase", key: fmt.Sprintf("%q", strings.ToUpper(name))},
			{label: "escaped-exact", key: legacyNativeEventEscapedKey(name)},
		}
		if strings.Contains(name, "s") {
			keys = append(keys, struct {
				label string
				key   string
			}{label: "unicode-long-s", key: fmt.Sprintf("%q", strings.ReplaceAll(name, "s", "ſ"))})
		}
		if strings.Contains(name, "k") {
			keys = append(keys, struct {
				label string
				key   string
			}{label: "unicode-kelvin", key: fmt.Sprintf("%q", strings.ReplaceAll(name, "k", "K"))})
		}
		for _, key := range keys {
			test.Run(name+"/"+key.label, func(test *testing.T) {
				_ = []byte(`{"events":[{"kind":"turn",` + key.key + `:` + invalid + `}],"events":[],"envelope":{"cir_version":"2"}}`)
			})
		}
		if !strings.EqualFold(field.Name, name) {
			test.Run(name+"/go-name-is-not-json-tag", func(test *testing.T) {
				_ = legacyNativeEventDocument("1", fmt.Sprintf(`{"kind":"turn",%q:1e999}`, field.Name))
			})
		}
	}
}
