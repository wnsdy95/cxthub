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

func wave42LegacyGitType(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.StarExpr:
		return "*" + wave42LegacyGitType(value.X)
	case *ast.SelectorExpr:
		return wave42LegacyGitType(value.X) + "." + value.Sel.Name
	case *ast.ArrayType:
		if value.Len == nil {
			return "[]" + wave42LegacyGitType(value.Elt)
		}
	case *ast.IndexExpr:
		return wave42LegacyGitType(value.X)
	case *ast.IndexListExpr:
		return wave42LegacyGitType(value.X)
	case *ast.ParenExpr:
		return wave42LegacyGitType(value.X)
	}
	return ""
}

func wave42LegacyGitFunctionName(expression ast.Expr) string {
	name := wave42LegacyGitType(expression)
	if separator := strings.LastIndex(name, "."); separator >= 0 {
		return name[separator+1:]
	}
	return name
}

func wave42LegacyGitCalls(node ast.Node) map[string]bool {
	calls := map[string]bool{}
	if node == nil {
		return calls
	}
	ast.Inspect(node, func(inspected ast.Node) bool {
		if call, ok := inspected.(*ast.CallExpr); ok {
			calls[wave42LegacyGitFunctionName(call.Fun)] = true
		}
		return true
	})
	return calls
}

func wave42LegacyGitMentions(node ast.Node, name string) bool {
	found := false
	ast.Inspect(node, func(inspected ast.Node) bool {
		if identifier, ok := inspected.(*ast.Ident); ok && identifier.Name == name {
			found = true
		}
		return true
	})
	return found
}

type wave42LegacyGitSourceSet struct {
	files   map[string]*ast.File
	methods map[string]*ast.FuncDecl
	types   map[string]*ast.TypeSpec
}

func wave42LegacyGitParseSources(sources map[string]string) (wave42LegacyGitSourceSet, error) {
	set := wave42LegacyGitSourceSet{files: map[string]*ast.File{}, methods: map[string]*ast.FuncDecl{}, types: map[string]*ast.TypeSpec{}}
	for path, source := range sources {
		file, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
		if err != nil {
			return set, err
		}
		set.files[path] = file
		for _, declaration := range file.Decls {
			switch value := declaration.(type) {
			case *ast.FuncDecl:
				name := value.Name.Name
				if value.Recv != nil {
					name = strings.TrimPrefix(wave42LegacyGitType(value.Recv.List[0].Type), "*") + "." + name
				}
				set.methods[name] = value
			case *ast.GenDecl:
				for _, specification := range value.Specs {
					if typed, ok := specification.(*ast.TypeSpec); ok {
						set.types[typed.Name.Name] = typed
					}
				}
			}
		}
	}
	return set, nil
}

func wave42LegacyGitReadSources(test *testing.T, directory string) map[string]string {
	test.Helper()
	paths, err := filepath.Glob(filepath.Join(directory, "*.go"))
	if err != nil {
		test.Fatal(err)
	}
	sources := map[string]string{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			test.Fatal(err)
		}
		sources[path] = string(raw)
	}
	if len(sources) == 0 {
		test.Fatal("actual source package is missing", directory)
	}
	return sources
}
