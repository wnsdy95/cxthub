package architecture

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func evidenceQueryViolations(source, module string) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "evidence-query.go", source, 0)
	if err != nil {
		return nil, err
	}
	allowedImports := map[string]bool{"context": true, "encoding/json": true, "errors": true, "fmt": true, "reflect": true, "slices": true, "strings": true, "time": true,
		module + "/internal/domain": true, module + "/internal/app/contextprojection": true}
	var violations []string
	for _, imported := range file.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			return nil, err
		}
		if !allowedImports[path] {
			violations = append(violations, "evidence query imports forbidden dependency "+path)
		}
	}
	allowedFields := map[string]bool{"Profile": true, "MetadataReader": true, "PRLedgerReader": true, "PRHistoryReader": true,
		"GitLedgerReader": true, "GitHeadReader": true, "GitAdmissionReader": true, "GitReceiptReader": true}
	statusFields := map[string]map[string]bool{
		"ScanConfig":    {"Scope": true, "MetadataReader": true, "ScanReader": true, "HeadScanReader": true, "bool": true},
		"ScanQueries":   {"Scope": true, "MetadataReader": true, "ScanReader": true, "HeadScanReader": true, "bool": true},
		"ChangeConfig":  {"Scope": true, "ChangeReader": true, "bool": true},
		"ChangeQueries": {"Scope": true, "ChangeReader": true, "bool": true},
		"CodeConfig":    {"Scope": true, "MetadataReader": true, "RevisionReader": true, "GitTreeReader": true, "GitDeltaReader": true, "bool": true},
		"CodeQueries":   {"Scope": true, "MetadataReader": true, "RevisionReader": true, "GitTreeReader": true, "GitDeltaReader": true, "bool": true},
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch inspected := node.(type) {
		case *ast.TypeAssertExpr:
			violations = append(violations, "evidence query discovers runtime capabilities")
		case *ast.ChanType, *ast.GoStmt:
			violations = append(violations, "evidence query owns background execution or admission")
		case *ast.StarExpr:
			if name, ok := inspected.X.(*ast.Ident); ok && name.Name == "Service" {
				violations = append(violations, "evidence query retains a Service backreference")
			}
		case *ast.TypeSpec:
			fields := allowedFields
			if status, exists := statusFields[inspected.Name.Name]; exists {
				fields = status
			} else if inspected.Name.Name != "Queries" && inspected.Name.Name != "Config" {
				break
			}
			shape, ok := inspected.Type.(*ast.StructType)
			if !ok {
				violations = append(violations, "evidence query owner is not concrete")
				break
			}
			for _, field := range shape.Fields.List {
				dependency, ok := field.Type.(*ast.Ident)
				if !ok || !fields[dependency.Name] {
					violations = append(violations, "evidence query owner retains an undeclared dependency")
				}
			}
		}
		return true
	})
	return violations, nil
}

func TestEvidenceQueryProductionBoundary(test *testing.T) {
	module, err := readGoModule(filepath.Join("..", ".."))
	if err != nil {
		test.Fatal(err)
	}
	directory := filepath.Join(module.directory, "internal", "app", "evidencequery")
	entries, err := os.ReadDir(directory)
	if err != nil {
		test.Fatal(err)
	}
	owned := map[string]string{"VerifiedPR": "ListPRContributions", "VerifiedGit": "ListGitContributions"}
	present := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			test.Fatal(err)
		}
		violations, err := evidenceQueryViolations(string(raw), module.path)
		if err != nil || len(violations) != 0 {
			test.Fatal(entry.Name(), violations, err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), entry.Name(), raw, 0)
		if err != nil {
			test.Fatal(err)
		}
		for _, declaration := range file.Decls {
			method, ok := declaration.(*ast.FuncDecl)
			if !ok || method.Recv == nil || documentQueryReceiver(method.Recv.List[0].Type) != "Queries" {
				continue
			}
			operation, expected := owned[method.Name.Name]
			if !expected {
				continue
			}
			ast.Inspect(method.Body, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok {
					if selected, ok := call.Fun.(*ast.SelectorExpr); ok && selected.Sel.Name == operation {
						present[method.Name.Name] = true
					}
				}
				return true
			})
		}
	}
	for method := range owned {
		if !present[method] {
			test.Error("verified evidence has no actual read implementation", method)
		}
	}
}

func TestEvidenceQueryBoundaryRejectsWritersAndParentCallbacks(test *testing.T) {
	for _, fixture := range []struct{ code, want string }{
		{`import "github.com/wnsdy95/cxthub/backend/internal/app"`, "forbidden dependency"},
		{`import "github.com/wnsdy95/cxthub/backend/internal/ports/outbound"`, "forbidden dependency"},
		{`import "net/http"`, "forbidden dependency"},
		{`type Queries struct { parent *Service }`, "Service backreference"},
		{`type Config struct { read func() error }`, "undeclared dependency"},
		{`type CodeConfig struct { read func() error }`, "undeclared dependency"},
		{`type ScanQueries struct { storage interface{ Put() } }`, "undeclared dependency"},
		{`type ChangeConfig struct { parent *Service }`, "Service backreference"},
		{`func read(value any) { _ = value.(interface{ Put() }) }`, "runtime capabilities"},
		{`type Extra struct { gate chan struct{} }`, "background execution or admission"},
		{`func work() { go work() }`, "background execution or admission"},
	} {
		violations, err := evidenceQueryViolations("package evidencequery\n"+fixture.code, "github.com/wnsdy95/cxthub/backend")
		if err != nil || !strings.Contains(fmt.Sprint(violations), fixture.want) {
			test.Fatal("forbidden evidence query dependency escaped the guard", fixture.want, violations, err)
		}
	}
}
