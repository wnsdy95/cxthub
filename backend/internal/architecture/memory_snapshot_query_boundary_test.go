package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLiveMemoryProjectionAndSnapshotQueriesOwnActualAlgorithms(test *testing.T) {
	for _, name := range []string{"projection.go", "lineage.go", "snapshots.go"} {
		raw, err := os.ReadFile(filepath.Join("..", "app", "memoryquery", name))
		if err != nil {
			test.Fatal(err)
		}
		if problems := preparedMemoryBoundaryViolations(raw); len(problems) != 0 {
			test.Fatal(name, problems)
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, raw, 0)
		if err != nil {
			test.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			declaration, ok := node.(*ast.TypeSpec)
			if !ok || declaration.Name.Name != "ProjectionReader" {
				return true
			}
			state, ok := declaration.Type.(*ast.StructType)
			if !ok {
				test.Fatal("projection reader has no owned private state")
			}
			for _, field := range state.Fields.List {
				for _, name := range field.Names {
					if name.IsExported() {
						test.Error("mutable projection state is exported", name.Name)
					}
				}
			}
			return true
		})
	}
	if _, err := os.Stat(filepath.Join("..", "app", "memory_lineage.go")); !os.IsNotExist(err) {
		test.Fatal("root still owns a duplicate lineage algorithm", err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "app", "memory_projection.go"))
	if err != nil {
		test.Fatal(err)
	}
	if strings.Contains(string(raw), "type projectionReader") || strings.Contains(string(raw), "newMemoryProjectionFingerprinter") || !strings.Contains(string(raw), "memoryquery.ProjectMemory") {
		test.Fatal("root projection still owns live computation or cache")
	}
	for _, kind := range []string{"http", "mcp"} {
		directory := filepath.Join("..", "adapters", "delivery", kind)
		entries, err := os.ReadDir(directory)
		if err != nil {
			test.Fatal(err)
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(directory, entry.Name()))
			if err != nil {
				test.Fatal(err)
			}
			for _, forbidden := range []string{"s.b.GetMemoryObject", "s.b.GetMemoryDigest", "s.context.GetMemoryObject", "s.context.GetMemoryDigest", "s.context.GetMemoryProjection"} {
				if strings.Contains(string(raw), forbidden) {
					test.Error("transport rediscovered a broad memory getter", kind, entry.Name(), forbidden)
				}
			}
		}
	}
}
