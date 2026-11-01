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

func repositoryQueryViolations(source string, module string) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "repository-query.go", source, 0)
	if err != nil {
		return nil, err
	}
	var violations []string
	for _, imported := range file.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			return nil, err
		}
		if strings.Contains(path, ".") && path != module+"/internal/domain" && path != module+"/internal/ports/inbound" && path != module+"/internal/app/contextprojection" && path != module+"/internal/app/evidencequery" {
			violations = append(violations, "repository query imports forbidden dependency "+path)
		}
	}
	allowed := map[string]bool{"Scope": true, "Resources": true, "MetadataReader": true, "RevisionReader": true, "HistoryReader": true,
		"ArchiveReader": true, "PendingReader": true, "PendingStatusReader": true, "GraphStatusReader": true, "Profile": true, "bool": true,
		"GraphScope": true, "GraphResources": true, "PublishedGraphReader": true, "ServingGraphRepair": true, "ProjectionGraphRepair": true,
		"ProjectionCacheScope": true, "GraphGenerationReader": true, "ProjectionMetadataReader": true, "ContributionEvidence": true, "GraphReadScope": true,
		"ContextReadScope": true, "ContextIndexReader": true}
	ast.Inspect(file, func(node ast.Node) bool {
		switch inspected := node.(type) {
		case *ast.TypeAssertExpr:
			violations = append(violations, "repository query performs runtime capability discovery")
		case *ast.ChanType:
			violations = append(violations, "repository query owns an extra admission channel")
		case *ast.StarExpr:
			if name, ok := inspected.X.(*ast.Ident); ok && name.Name == "Service" {
				violations = append(violations, "repository query retains a Service backreference")
			}
		case *ast.TypeSpec:
			if inspected.Name.Name != "Config" && inspected.Name.Name != "Queries" && inspected.Name.Name != "PublishedGraphConfig" && inspected.Name.Name != "PublishedGraphQueries" && inspected.Name.Name != "LegacyProjectionConfig" && inspected.Name.Name != "LegacyProjection" && inspected.Name.Name != "LegacyGraphConfig" && inspected.Name.Name != "LegacyGraphQueries" && inspected.Name.Name != "GraphConfig" && inspected.Name.Name != "GraphQueries" && inspected.Name.Name != "RepositoryViewReaders" && inspected.Name.Name != "ContextConfig" && inspected.Name.Name != "ContextQueries" {
				break
			}
			shape, ok := inspected.Type.(*ast.StructType)
			if !ok {
				violations = append(violations, "repository query owner is not a concrete struct")
				break
			}
			for _, field := range shape.Fields.List {
				switch dependency := field.Type.(type) {
				case *ast.Ident:
					if allowed[dependency.Name] {
						continue
					}
				case *ast.ArrayType:
					if name, ok := dependency.Elt.(*ast.SelectorExpr); ok && dependency.Len == nil && name.Sel.Name == "ContentHash" {
						continue
					}
				case *ast.StarExpr:
					if name, ok := dependency.X.(*ast.Ident); ok && (name.Name == "GraphCache" || name.Name == "BranchCache" || name.Name == "LegacyProjection" || name.Name == "SegmentCache") {
						continue
					}
				case *ast.SelectorExpr:
					if owner, ok := dependency.X.(*ast.Ident); ok && owner.Name == "inbound" && dependency.Sel.Name == "RepositoryGraphQueries" {
						continue
					}
					if owner, ok := dependency.X.(*ast.Ident); ok && owner.Name == "evidencequery" && (dependency.Sel.Name == "GitTreeReader" || dependency.Sel.Name == "GitDeltaReader") {
						continue
					}
				case *ast.MapType:
					key, keyOK := dependency.Key.(*ast.SelectorExpr)
					value, valueOK := dependency.Value.(*ast.Ident)
					if keyOK && valueOK && key.Sel.Name == "ContentHash" && value.Name == "bool" {
						continue
					}
				}
				violations = append(violations, "repository query owner has an unexpected dependency")
			}
		}
		return true
	})
	return violations, nil
}

func TestRepositoryStateQueryProductionBoundary(test *testing.T) {
	module, err := readGoModule(filepath.Join("..", ".."))
	if err != nil {
		test.Fatal(err)
	}
	directory := filepath.Join(module.directory, "internal", "app", "repositoryquery")
	entries, err := os.ReadDir(directory)
	if err != nil {
		test.Fatal(err)
	}
	owned := map[string]string{"RepositoryRevision": "readRepository", "RepositoryChangeState": "readRepository", "GetPendingState": "readRepository",
		"GetRepositoryView": "readPublishedGraphResult", "GetPendingView": "readPublishedGraphResult", "QueryGraphState": "readPublishedGraphResult"}
	present := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			test.Fatal(err)
		}
		violations, err := repositoryQueryViolations(string(raw), module.path)
		if err != nil || len(violations) != 0 {
			test.Fatal(entry.Name(), violations, err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), entry.Name(), raw, 0)
		if err != nil {
			test.Fatal(err)
		}
		for _, declaration := range file.Decls {
			method, ok := declaration.(*ast.FuncDecl)
			if !ok || method.Recv == nil {
				continue
			}
			receiver := documentQueryReceiver(method.Recv.List[0].Type)
			if receiver != "Queries" && receiver != "PublishedGraphQueries" {
				continue
			}
			boundary, expected := owned[method.Name.Name]
			if !expected {
				continue
			}
			ast.Inspect(method.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if ok {
					if name, ok := call.Fun.(*ast.Ident); ok && name.Name == boundary {
						present[method.Name.Name] = true
					}
					if generic, ok := call.Fun.(*ast.IndexListExpr); ok {
						if name, ok := generic.X.(*ast.Ident); ok && name.Name == boundary {
							present[method.Name.Name] = true
						}
					}
				}
				return true
			})
		}
	}
	for method := range owned {
		if !present[method] {
			test.Error("new query must own the actual scoped implementation", method)
		}
	}
}

func TestRepositoryStateQueryBoundaryRejectsBadFixtures(test *testing.T) {
	for _, fixture := range []struct{ code, want string }{
		{`import "github.com/wnsdy95/cxthub/backend/internal/app"`, "forbidden dependency"},
		{`func read(value any) { _ = value.(interface{ Write() }) }`, "capability discovery"},
		{`type Other struct { parent *Service }`, "Service backreference"},
		{`type Other struct { admission chan struct{} }`, "admission channel"},
		{`type Queries struct { parent func() error }`, "unexpected dependency"},
		{`type PublishedGraphQueries struct { parent func() error }`, "unexpected dependency"},
		{`type PublishedGraphConfig struct { parent func() error }`, "unexpected dependency"},
		{`type ContextConfig struct { parent func() error }`, "unexpected dependency"},
		{`type ContextQueries struct { parent func() error }`, "unexpected dependency"},
	} {
		violations, err := repositoryQueryViolations("package repositoryquery\n"+fixture.code, "github.com/wnsdy95/cxthub/backend")
		if err != nil || !strings.Contains(fmt.Sprint(violations), fixture.want) {
			test.Fatal("forbidden query boundary escaped the guard", fixture.want, violations, err)
		}
	}
}
