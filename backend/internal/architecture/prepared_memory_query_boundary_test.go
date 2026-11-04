package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func preparedMemoryBoundaryViolations(raw []byte) []string {
	file, err := parser.ParseFile(token.NewFileSet(), "memory-query.go", raw, 0)
	if err != nil {
		return []string{err.Error()}
	}
	var violations []string
	for _, imported := range file.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			violations = append(violations, "invalid import")
			continue
		}
		if (withinPackage(path, documentQueryModule+"/internal/app") && path != documentQueryModule+"/internal/app/contextprojection" && path != documentQueryModule+"/internal/app/evidencequery") || withinPackage(path, documentQueryModule+"/internal/adapters") {
			violations = append(violations, "query depends on parent application or adapter: "+path)
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.TypeAssertExpr:
			if !memoryProjectionValidationAssertion(typed.Type) {
				violations = append(violations, "runtime capability rediscovery")
			}
		case *ast.SelectorExpr:
			switch typed.Sel.Name {
			case "MetadataStore", "BlobStore", "MemoryReadDemandStore", "MemoryReadWorkStore", "MemoryReadQualificationStore", "Service":
				violations = append(violations, "broad or writing query dependency: "+typed.Sel.Name)
			}
		case *ast.GoStmt:
			violations = append(violations, "query starts a worker")
		}
		return true
	})
	return violations
}

func memoryProjectionValidationAssertion(expression ast.Expr) bool {
	contract, ok := expression.(*ast.InterfaceType)
	if !ok || contract.Methods == nil || len(contract.Methods.List) != 1 {
		return false
	}
	method := contract.Methods.List[0]
	if len(method.Names) != 1 || method.Names[0].Name != "memoryValidated" {
		return false
	}
	signature, ok := method.Type.(*ast.FuncType)
	if !ok || signature.Params == nil || len(signature.Params.List) != 2 || signature.Results == nil || len(signature.Results.List) != 1 {
		return false
	}
	first, firstOK := signature.Params.List[0].Type.(*ast.SelectorExpr)
	second, secondOK := signature.Params.List[1].Type.(*ast.SelectorExpr)
	result, resultOK := signature.Results.List[0].Type.(*ast.Ident)
	return firstOK && secondOK && resultOK && first.Sel.Name == "Context" && second.Sel.Name == "ContentHash" && result.Name == "bool"
}

func TestPreparedMemoryQueryOwnsActualAlgorithmsAndDirectConsumers(test *testing.T) {
	directory := filepath.Join("..", "app", "memoryquery")
	entries, err := os.ReadDir(directory)
	if err != nil {
		test.Fatal(err)
	}
	methods := map[string]bool{"ObserveMemoryReadRequirement": false, "QueryMinimumMemory": false, "QueryPreparedMemory": false, "ReadPage": false}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			test.Fatal(err)
		}
		for _, violation := range preparedMemoryBoundaryViolations(raw) {
			test.Error(entry.Name(), violation)
		}
		file, err := parser.ParseFile(token.NewFileSet(), entry.Name(), raw, 0)
		if err != nil {
			test.Fatal(err)
		}
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok {
				if _, required := methods[function.Name.Name]; required && function.Body != nil && len(function.Body.List) > 2 {
					methods[function.Name.Name] = true
				}
			}
		}
	}
	for method, present := range methods {
		if !present {
			test.Error("actual memory query algorithm is not owned by the new package", method)
		}
	}
	for _, name := range []string{"memory_minimum_read.go", "memory_read.go"} {
		raw, err := os.ReadFile(filepath.Join("..", "app", name))
		if err != nil {
			test.Fatal(err)
		}
		if strings.Contains(string(raw), "type minimumMemoryRangeReader") || strings.Contains(string(raw), "func minimumMemoryContinuation") {
			test.Error("old minimum algorithm retained a second owner", name)
		}
	}
	for _, kind := range []string{"http", "mcp"} {
		raw, err := os.ReadFile(filepath.Join("..", "adapters", "delivery", kind, "prepared_memory.go"))
		if err != nil {
			test.Fatal(err)
		}
		if !strings.Contains(string(raw), "inbound.PreparedMemoryReader") || strings.Contains(string(raw), ".(inbound.MemoryReadScope)") {
			test.Error("prepared transport does not consume the explicit query and scope", kind)
		}
	}
}

func TestPreparedMemoryBoundaryRejectsParentWriterAndWorkerDependencies(test *testing.T) {
	for _, source := range []string{
		`package memoryquery; import parent "github.com/wnsdy95/cxthub/backend/internal/app"; type broken struct{ owner *parent.Service }`,
		`package memoryquery; import store "github.com/wnsdy95/cxthub/backend/internal/adapters/store"; var broken *store.PostgresStore`,
		`package memoryquery; import outbound "github.com/wnsdy95/cxthub/backend/internal/ports/outbound"; type broken struct{ store outbound.MemoryReadDemandStore }`,
		`package memoryquery; import outbound "github.com/wnsdy95/cxthub/backend/internal/ports/outbound"; type broken struct{ store outbound.MemoryReadQualificationStore }`,
		`package memoryquery; func broken(value any){ _ = value.(interface{ Read() }) }`,
		`package memoryquery; func broken(value any){ _ = value.(interface{ memoryValidated(any,string) bool; Write() }) }`,
		`package memoryquery; func broken(){go run()}`,
	} {
		if len(preparedMemoryBoundaryViolations([]byte(source))) == 0 {
			test.Error("forbidden query dependency escaped the guard", source)
		}
	}
	if failures := preparedMemoryBoundaryViolations([]byte(`package memoryquery; import outbound "github.com/wnsdy95/cxthub/backend/internal/ports/outbound"; type valid struct{ reader outbound.MinimumMemoryReadStore }`)); len(failures) != 0 {
		test.Fatal("valid narrow reader was rejected", failures)
	}
}
