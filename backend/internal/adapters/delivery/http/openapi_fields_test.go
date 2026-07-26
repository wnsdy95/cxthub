package http

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// TestOpenAPISchemaFieldDrift checks if the schema attributes in openapi.yaml match the json fields in the Go struct (bidirectionally). It pairs with TestOpenAPIDrift to prevent silent discrepancies (fields added in code but missing in spec, or phantom fields in the spec). When fields are added or removed, the spec must be updated to pass.
func TestOpenAPISchemaFieldDrift(t *testing.T) {
	const spec = "../../../../../schemas/openapi.yaml"
	cases := []struct {
		schema string
		typ    reflect.Type
	}{
		{"Snapshot", reflect.TypeOf(domain.Snapshot{})},
		{"Repo", reflect.TypeOf(domain.Repo{})},
		{"User", reflect.TypeOf(domain.User{})},
	}
	for _, c := range cases {
		props := schemaProps(t, spec, c.schema)
		goFields := jsonFields(c.typ)
		for _, f := range goFields {
			if !props[f] {
				t.Errorf("%s: Go field %q not in openapi schema (add to schemas/openapi.yaml %s.properties)", c.schema, f, c.schema)
			}
		}
		for p := range props {
			if !contains(goFields, p) {
				t.Errorf("%s: openapi property %q not in Go struct (phantom field — remove from spec or add to struct)", c.schema, p)
			}
		}
	}
}

// jsonFields returns a list of json tag names in the struct (excluding json:"-" and untagged fields).
func jsonFields(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		if name := strings.Split(tag, ",")[0]; name != "" {
			out = append(out, name)
		}
	}
	return out
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

var schemaPropKeyRe = regexp.MustCompile(`^        (\w+):`)

// schemaProps returns the top-level property keys from components.schemas.<schema> in openapi.yaml.
// It uses indentation conventions instead of an external YAML parser (standard library only): schema names
// use four spaces, properties uses six, and top-level properties use eight. Nested properties at ten or more
// are ignored, and indent <= 6 ends the properties block.
func schemaProps(t *testing.T, specPath, schema string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	lines := strings.Split(string(b), "\n")
	schemaRe := regexp.MustCompile(`^    ` + regexp.QuoteMeta(schema) + `:\s*$`)

	i := 0
	for ; i < len(lines); i++ {
		if schemaRe.MatchString(lines[i]) {
			break
		}
	}
	if i == len(lines) {
		t.Fatalf("schema %q not found in OpenAPI", schema)
	}
	// Finds the first 6-space `properties:` in this schema block.
	found := false
	for i++; i < len(lines); i++ {
		if lines[i] == "      properties:" {
			found = true
			i++
			break
		}
		// If the next schema (4-space name) is reached before properties, this schema has no properties.
		if len(lines[i]) > 0 && lines[i][0] != ' ' {
			break
		}
		if regexp.MustCompile(`^    \w`).MatchString(lines[i]) {
			break
		}
	}
	if !found {
		t.Fatalf("Schema %q has no properties block", schema)
	}
	out := map[string]bool{}
	for ; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent <= 6 {
			break // end of properties block
		}
		if m := schemaPropKeyRe.FindStringSubmatch(line); m != nil {
			out[m[1]] = true
		}
	}
	return out
}
