//go:build monorepo

package domain

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestConversationManifestWireAndFixtureParity(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Go tests run in the package directory, including builds using -trimpath.
	root := filepath.Clean(filepath.Join(dir, "..", "..", ".."))
	// Verification caches are module-local. Keep the wire format, builder and
	// identical semantic fixture suite aligned while allowing different readers.
	for _, name := range []string{"conversation_manifest.go", "conversation_manifest_build.go", "conversation_manifest_test.go"} {
		backend, err := os.ReadFile(filepath.Join(root, "backend", "internal", "domain", name))
		if err != nil {
			t.Fatal(err)
		}
		cli, err := os.ReadFile(filepath.Join(root, "cli", "internal", "domain", name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(backend, cli) {
			t.Fatalf("independent wire/fixture drift: %s", name)
		}
	}
}

func TestCatalogMerkleMirroredImplementationAndFixtures(t *testing.T) {
	for _, name := range []string{"catalog_merkle.go", "catalog_merkle_test.go"} {
		local, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		other := "../../../cli/internal/domain/" + name
		// Resolve the peer without importing its independent internal package.
		if cwd, err := os.Getwd(); err != nil {
			t.Fatal(err)
		} else if strings.HasSuffix(cwd, "/cli/internal/domain") {
			other = "../../../backend/internal/domain/" + name
		}
		peer, err := os.ReadFile(other)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(local, peer) {
			t.Fatalf("mirrored algorithm/fixtures drift: %s", name)
		}
	}
}

func TestCatalogWireStructParityWithBackend(t *testing.T) {
	// The modules cannot import each other's internal packages. Parse the
	// server's actual declarations to catch wire/tag drift without a dependency.
	file, err := parser.ParseFile(token.NewFileSet(), "../../../backend/internal/domain/catalog.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ours := map[string]reflect.Type{
		"CatalogCheckpoint": reflect.TypeFor[CatalogCheckpoint](),
		"CatalogRequest":    reflect.TypeFor[CatalogRequest](),
		"CatalogEntry":      reflect.TypeFor[CatalogEntry](),
		"CatalogPage":       reflect.TypeFor[CatalogPage](),
	}
	checked := 0
	for _, declaration := range file.Decls {
		gen, ok := declaration.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			cliType, wanted := ours[typeSpec.Name.Name]
			if !wanted {
				continue
			}
			serverType, ok := typeSpec.Type.(*ast.StructType)
			if !ok || len(serverType.Fields.List) != cliType.NumField() {
				t.Fatalf("%s field count changed", typeSpec.Name.Name)
			}
			for i, field := range serverType.Fields.List {
				if field.Tag == nil || len(field.Names) != 1 {
					t.Fatal("unexpected backend wire declaration")
				}
				tag, err := strconv.Unquote(field.Tag.Value)
				if err != nil || field.Names[0].Name != cliType.Field(i).Name || tag != string(cliType.Field(i).Tag) {
					t.Fatalf("%s field %d/tag drift", typeSpec.Name.Name, i)
				}
				if got := catalogTestASTType(field.Type); got != catalogTestReflectType(cliType.Field(i).Type) {
					t.Fatalf("%s.%s type drift: server=%s cli=%s", typeSpec.Name.Name, field.Names[0].Name, got, catalogTestReflectType(cliType.Field(i).Type))
				}
			}
			checked++
		}
	}
	if checked != len(ours) {
		t.Fatalf("checked %d of %d wire types", checked, len(ours))
	}
}

func catalogTestASTType(expr ast.Expr) string {
	switch typ := expr.(type) {
	case *ast.Ident:
		if typ.Name == "ContentHash" {
			return "string"
		}
		return typ.Name
	case *ast.SelectorExpr:
		return catalogTestASTType(typ.X) + "." + typ.Sel.Name
	case *ast.StarExpr:
		return "*" + catalogTestASTType(typ.X)
	case *ast.ArrayType:
		return "[]" + catalogTestASTType(typ.Elt)
	}
	return "unsupported"
}

func catalogTestReflectType(typ reflect.Type) string {
	if typ.Kind() == reflect.Pointer {
		return "*" + catalogTestReflectType(typ.Elem())
	}
	if typ == reflect.TypeFor[json.RawMessage]() {
		return "json.RawMessage"
	}
	if typ.Kind() == reflect.Slice {
		return "[]" + catalogTestReflectType(typ.Elem())
	}
	return typ.Name()
}
