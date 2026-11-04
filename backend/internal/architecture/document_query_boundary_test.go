package architecture

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const documentQueryModule = "github.com/wnsdy95/cxthub/backend"

type documentQuerySource struct {
	file    *ast.File
	imports map[string]string
	root    bool
}

type documentQueryType struct {
	expression ast.Expr
	imports    map[string]string
}

func documentQueryReceiver(expression ast.Expr) string {
	switch receiver := expression.(type) {
	case *ast.Ident:
		return receiver.Name
	case *ast.StarExpr:
		return documentQueryReceiver(receiver.X)
	case *ast.ParenExpr:
		return documentQueryReceiver(receiver.X)
	case *ast.SelectorExpr:
		return receiver.Sel.Name
	}
	return ""
}

func documentQueryReadPort(name string) bool {
	switch name {
	case "DocReadStore", "VerifiedDocReader", "VerifiedRootEventReader":
		return true
	case "DocRangeSnapshotStore", "DocRangeGenerationStore", "DocRangeReadinessStore",
		"DocVisibleGenerationStore", "DocVisibleGenerationRead", "DocVisiblePreparationStore", "DocRangeBoundaryStore",
		"ConversationChunkReader", "LegacyDerivedByteRangeReader", "DocByteRangePreparationStore",
		"DocDisplayReader", "DocEventDisplayReader", "DocReadOnly", "WithDocReadOnly":
		return true
	}
	return false
}

func documentQueryReadOperation(name string) bool {
	switch name {
	case "GetDoc", "GetDocManifest", "GetChunk", "DocReadIndex", "SearchDocEvents", "ReadVerifiedDoc", "ReadVerifiedRootEvents":
		return true
	case "GetSnapshot", "DocRangeIndex", "DocRangePage", "OpenDocRange", "OpenDocRangeGeneration", "HasDocRangeIndex",
		"OpenDocVisibleGeneration", "HasDocVisibleIndex", "DocRangeBoundary", "ReadConversationChunk",
		"ReadLegacyDerivedByteRangeChunk", "LegacyByteRangePublicationEnabled", "HasDocByteRangeIndex", "ReadDocFindFields",
		"ReadDocDisplayGeneration", "ReadDocDisplayPage", "ReadDocEventDisplayGeneration", "ReadDocEventDisplayPage", "InReadOnlyTransaction":
		return true
	}
	return false
}

func documentQueryDependency(expression ast.Expr, imports map[string]string) bool {
	switch dependency := expression.(type) {
	case *ast.Ident:
		switch dependency.Name {
		case "Profile", "bool", "Scope", "SnapshotReader", "LegacyReader":
			return true
		case "ViewDependencies", "FindDependencies", "DocRangeReader", "LegacyRangePreparation", "DocRangeEnqueuer",
			"ViewResources", "FindFields", "FindDisplayDemand", "FindFieldsReader", "FindFieldsDemand",
			"DocVisibleReadiness", "DocByteRangeReadiness", "DocByteRangeDemand", "DocRangeBoundaryReader", "DocRangeBoundaryDemand":
			return true
		}
	case *ast.StarExpr:
		return documentQueryDependency(dependency.X, imports)
	case *ast.SelectorExpr:
		if qualifier, ok := dependency.X.(*ast.Ident); ok {
			return imports[qualifier.Name] == documentQueryModule+"/internal/ports/outbound" && documentQueryReadPort(dependency.Sel.Name)
		}
	}
	return false
}

func documentQueryCapability(expression ast.Expr, types map[string]documentQueryType, imports map[string]string, seen map[string]bool) bool {
	switch capability := expression.(type) {
	case nil, *ast.InterfaceType:
		return true
	case *ast.ParenExpr:
		return documentQueryCapability(capability.X, types, imports, seen)
	case *ast.Ident:
		if capability.Name == "any" {
			return true
		}
		if definition, exists := types[capability.Name]; exists && !seen[capability.Name] {
			seen[capability.Name] = true
			return documentQueryCapability(definition.expression, types, definition.imports, seen)
		}
	case *ast.SelectorExpr:
		if qualifier, ok := capability.X.(*ast.Ident); ok {
			return withinPackage(imports[qualifier.Name], documentQueryModule+"/internal/ports")
		}
	}
	return false
}

func documentQueryScopeCall(expression ast.Expr, seen map[*ast.Object]bool) bool {
	switch callable := expression.(type) {
	case *ast.ParenExpr:
		return documentQueryScopeCall(callable.X, seen)
	case *ast.SelectorExpr:
		return callable.Sel.Name == "WithinDocumentRead" || callable.Sel.Name == "WithinDocumentSnapshot"
	case *ast.Ident:
		if callable.Obj == nil || seen[callable.Obj] {
			return false
		}
		seen[callable.Obj] = true
		switch declaration := callable.Obj.Decl.(type) {
		case *ast.AssignStmt:
			for index, target := range declaration.Lhs {
				if name, ok := target.(*ast.Ident); ok && name.Name == callable.Name && index < len(declaration.Rhs) {
					return documentQueryScopeCall(declaration.Rhs[index], seen)
				}
			}
		case *ast.ValueSpec:
			for index, name := range declaration.Names {
				if name.Name == callable.Name && index < len(declaration.Values) {
					return documentQueryScopeCall(declaration.Values[index], seen)
				}
			}
		}
	}
	return false
}

func documentQueryScopedRead(call *ast.CallExpr) bool {
	if !documentQueryScopeCall(call.Fun, make(map[*ast.Object]bool)) {
		return false
	}
	for _, argument := range call.Args {
		callback, ok := argument.(*ast.FuncLit)
		if !ok || callback.Type.Params == nil || len(callback.Type.Params.List) == 0 || len(callback.Type.Params.List[0].Names) != 1 {
			continue
		}
		bound := callback.Type.Params.List[0].Names[0].Obj
		if bound == nil {
			continue
		}
		reads := false
		ast.Inspect(callback.Body, func(node ast.Node) bool {
			invocation, ok := node.(*ast.CallExpr)
			if !ok || len(invocation.Args) == 0 {
				return true
			}
			context, ok := invocation.Args[0].(*ast.Ident)
			if !ok || context.Obj != bound {
				return true
			}
			switch target := invocation.Fun.(type) {
			case *ast.SelectorExpr:
				reads = reads || documentQueryReadOperation(target.Sel.Name)
			case *ast.Ident:
				if target.Obj != nil {
					if parameter, ok := target.Obj.Decl.(*ast.Field); ok {
						_, delegated := parameter.Type.(*ast.FuncType)
						reads = reads || delegated
					}
				}
			}
			return true
		})
		if reads {
			return true
		}
	}
	return false
}

func scanDocumentQueryBoundary(directory string) ([]string, error) {
	positions := token.NewFileSet()
	var sources []documentQuerySource
	types := make(map[string]documentQueryType)
	functions := make(map[string]*ast.FuncDecl)
	methods := make(map[string]*ast.FuncDecl)
	rootSource := false
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(positions, path, nil, 0)
		if err != nil {
			return err
		}
		source := documentQuerySource{file: file, imports: make(map[string]string), root: filepath.Dir(path) == filepath.Clean(directory)}
		if source.root {
			rootSource = true
			if file.Name.Name != "documentquery" {
				return fmt.Errorf("%s: expected actual documentquery source, got package %s", path, file.Name.Name)
			}
		}
		for _, imported := range file.Imports {
			dependency, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				return err
			}
			alias := filepath.Base(dependency)
			if imported.Name != nil {
				alias = imported.Name.Name
			}
			source.imports[alias] = dependency
		}
		if source.root {
			for _, declaration := range file.Decls {
				switch declared := declaration.(type) {
				case *ast.GenDecl:
					for _, specification := range declared.Specs {
						if named, ok := specification.(*ast.TypeSpec); ok {
							types[named.Name.Name] = documentQueryType{expression: named.Type, imports: source.imports}
						}
					}
				case *ast.FuncDecl:
					if declared.Recv == nil {
						functions[declared.Name.Name] = declared
					} else if documentQueryReceiver(declared.Recv.List[0].Type) == "Queries" {
						methods[declared.Name.Name] = declared
					}
				}
			}
		}
		sources = append(sources, source)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !rootSource {
		return nil, fmt.Errorf("%s: missing production documentquery source", directory)
	}
	var violations []string
	reject := func(node ast.Node, message string) {
		violations = append(violations, fmt.Sprintf("%s: %s", positions.Position(node.Pos()), message))
	}
	for _, source := range sources {
		assertions := make(map[token.Pos]bool)
		for _, declaration := range source.file.Decls {
			if general, ok := declaration.(*ast.GenDecl); ok {
				for _, specification := range general.Specs {
					if value, ok := specification.(*ast.ValueSpec); ok && len(value.Names) == 1 && value.Names[0].Name == "_" && value.Type != nil {
						assertions[value.Type.Pos()] = true
					}
				}
			}
		}
		for alias, dependency := range source.imports {
			if alias == "." {
				reject(source.file, "documentquery cannot hide dependency ownership behind a dot import")
			}
			if withinPackage(dependency, documentQueryModule+"/internal") &&
				!withinPackage(dependency, documentQueryModule+"/internal/domain") &&
				dependency != documentQueryModule+"/internal/ports/inbound" &&
				dependency != documentQueryModule+"/internal/ports/outbound" &&
				!withinPackage(dependency, documentQueryModule+"/internal/app/documentquery") {
				reject(source.file, "documentquery must not import parent app, storage or delivery: "+dependency)
			}
		}
		ast.Inspect(source.file, func(node ast.Node) bool {
			switch inspected := node.(type) {
			case *ast.ChanType:
				reject(inspected, "documentquery cannot own channels or duplicate admission; inject the shared resource scope")
			case *ast.StarExpr:
				if documentQueryReceiver(inspected.X) == "Service" {
					reject(inspected, "documentquery must not retain or call back into *Service")
				}
			case *ast.SelectorExpr:
				qualifier, ok := inspected.X.(*ast.Ident)
				if ok && source.imports[qualifier.Name] == documentQueryModule+"/internal/ports/outbound" && !documentQueryReadPort(inspected.Sel.Name) {
					reject(inspected, "documentquery cannot acquire broad, worker or command ports: "+inspected.Sel.Name)
				}
				profile := inspected.Sel.Name == "DocumentQueryProfile" || inspected.Sel.Name == "DocumentQueryCompatibility" || inspected.Sel.Name == "DocumentQueryVerifiedRoots"
				readContext := inspected.Sel.Name == "DocumentReadAuthorizer" || inspected.Sel.Name == "DocumentSnapshotReadAuthorizer" || inspected.Sel.Name == "RepositoryActor"
				readContext = readContext || inspected.Sel.Name == "RetainDocumentResponse"
				if ok && source.imports[qualifier.Name] == documentQueryModule+"/internal/ports/inbound" && !assertions[inspected.Pos()] && !profile && !readContext {
					reject(inspected, "documentquery cannot retain or call driving/command ports: "+inspected.Sel.Name)
				}
			case *ast.InterfaceType:
				for _, field := range inspected.Methods.List {
					if len(field.Names) == 0 {
						reject(field, "documentquery interfaces must declare narrow reads, not embed other capabilities")
					}
					for _, name := range field.Names {
						infrastructure := name.Name == "AuthorizeDocumentRead" || name.Name == "AuthorizeDocumentPreparation" || name.Name == "AcquireInteractive" || name.Name == "AcquireBulk" || name.Name == "WithinDocumentRead" || name.Name == "WithinDocumentSnapshot"
						demand := name.Name == "EnqueueDocRangeJob" || name.Name == "EnqueueDocVisibleIndex" || name.Name == "EnqueueDocRangeBoundary" || name.Name == "EnqueueDocByteRangeJob" || name.Name == "EnqueueDocFindFields" || name.Name == "EnqueueDocDisplay"
						legacy := name.Name == "EnsureDocRangeIndex" && types["LegacyRangePreparation"].expression == inspected
						if !documentQueryReadOperation(name.Name) && !infrastructure && !demand && !legacy {
							reject(field, "documentquery cannot retain write, worker or facade methods: "+name.Name)
						}
					}
				}
			case *ast.TypeAssertExpr:
				if documentQueryCapability(inspected.Type, types, source.imports, make(map[string]bool)) {
					reject(inspected, "documentquery must use Config capabilities, not rediscover storage capabilities at runtime")
				}
			case *ast.TypeSpec:
				if inspected.Name.Name == "Config" || inspected.Name.Name == "Queries" || inspected.Name.Name == "ViewDependencies" || inspected.Name.Name == "FindDependencies" {
					structure, ok := inspected.Type.(*ast.StructType)
					if !ok || inspected.Assign.IsValid() {
						reject(inspected, inspected.Name.Name+" must own a concrete narrow dependency struct")
						break
					}
					for _, field := range structure.Fields.List {
						if len(field.Names) == 0 || !documentQueryDependency(field.Type, source.imports) {
							reject(field, inspected.Name.Name+" cannot hold facade delegates, callbacks or broad storage capabilities")
						}
					}
				}
			}
			return true
		})
	}
	for _, name := range []string{"Config", "Queries"} {
		if _, exists := types[name]; !exists {
			violations = append(violations, "missing actual documentquery."+name+" declaration")
		}
	}
	for name, required := range map[string][]string{
		"Scope":          {"AuthorizeDocumentRead", "AuthorizeDocumentPreparation", "WithinDocumentRead", "WithinDocumentSnapshot"},
		"SnapshotReader": {"GetSnapshot"},
		"LegacyReader":   {"GetDoc", "GetDocManifest", "GetChunk"},
	} {
		contract, ok := types[name].expression.(*ast.InterfaceType)
		if !ok {
			violations = append(violations, name+" must declare its own narrow read interface")
			continue
		}
		expected := make(map[string]bool)
		for _, method := range required {
			expected[method] = true
		}
		for _, field := range contract.Methods.List {
			for _, method := range field.Names {
				if !expected[method.Name] {
					reject(field, name+" cannot widen its read capability with "+method.Name)
				}
				delete(expected, method.Name)
			}
		}
		for method := range expected {
			reject(contract, name+" is missing the explicit read capability "+method)
		}
	}
	for _, name := range []string{"ReadDocEvents", "ReadDocFragments", "SearchDocEvents", "ReadDocViewEvents", "ReadDocViewFragment", "ReadDocVisiblePage", "FindDocPage", "ReadDocFindFragment"} {
		method := methods[name]
		if method == nil || method.Body == nil {
			violations = append(violations, "Queries must directly own the core method "+name)
			continue
		}
		seen := make(map[*ast.FuncDecl]bool)
		scoped, reads := false, false
		var inspectCalls func(*ast.FuncDecl)
		inspectCalls = func(function *ast.FuncDecl) {
			if function == nil || function.Body == nil || seen[function] {
				return
			}
			seen[function] = true
			owners := make(map[string]bool)
			for _, fields := range []*ast.FieldList{function.Recv, function.Type.Params} {
				if fields == nil {
					continue
				}
				for _, field := range fields.List {
					if documentQueryReceiver(field.Type) == "Queries" {
						for _, owner := range field.Names {
							owners[owner.Name] = true
						}
					}
				}
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok {
					if documentQueryScopedRead(call) {
						scoped = true
					}
					target := call.Fun
					switch generic := target.(type) {
					case *ast.IndexExpr:
						target = generic.X
					case *ast.IndexListExpr:
						target = generic.X
					}
					switch target := target.(type) {
					case *ast.Ident:
						inspectCalls(functions[target.Name])
					case *ast.SelectorExpr:
						if owner, ok := target.X.(*ast.Ident); ok && owners[owner.Name] {
							inspectCalls(methods[target.Sel.Name])
						} else if documentQueryReadOperation(target.Sel.Name) {
							reads = true
						}
					}
				}
				return true
			})
		}
		inspectCalls(method)
		if !scoped || !reads {
			reject(method, "Queries."+name+" must execute owned read logic through the explicit document scope and read ports")
		}
	}
	return violations, nil
}

func TestDocumentQueryCoreThreeSourceBoundary(test *testing.T) {
	violations, err := scanDocumentQueryBoundary(filepath.Join("..", "app", "documentquery"))
	if err != nil {
		test.Fatal(err)
	}
	for _, violation := range violations {
		test.Error(violation)
	}
}

const documentQueryBoundaryFixture = `package documentquery
import (
 "context"
 "github.com/wnsdy95/cxthub/backend/internal/domain"
 ports "github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)
type Profile uint8
type Scope interface {
 AuthorizeDocumentRead(context.Context, domain.ContentHash) (context.Context, error)
 AuthorizeDocumentPreparation(context.Context, domain.ContentHash) (context.Context, error)
 WithinDocumentRead(context.Context, domain.ContentHash, func(context.Context) error) error
 WithinDocumentSnapshot(context.Context, domain.ContentHash, func(context.Context) error) error
}
type SnapshotReader interface { GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error) }
type LegacyReader interface {
 GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error)
 GetDocManifest(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocChunkManifest, error)
 GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error)
}
type Config struct {
 Profile Profile
 Scope Scope
 Snapshots SnapshotReader
 Legacy LegacyReader
 Index ports.DocReadStore
 Verified ports.VerifiedDocReader
 RootEvents ports.VerifiedRootEventReader
 AtomicRead bool
}
type Queries struct {
 scope Scope
 legacy LegacyReader
}
func (queries *Queries) ReadDocEvents(ctx context.Context) error { return queries.read(ctx) }
func (queries *Queries) ReadDocFragments(ctx context.Context) error { return queries.read(ctx) }
func (queries *Queries) SearchDocEvents(ctx context.Context) error { return queries.read(ctx) }
func (queries *Queries) ReadDocViewEvents(ctx context.Context) error { return queries.read(ctx) }
func (queries *Queries) ReadDocViewFragment(ctx context.Context) error { return queries.read(ctx) }
func (queries *Queries) ReadDocVisiblePage(ctx context.Context) error { return queries.read(ctx) }
func (queries *Queries) FindDocPage(ctx context.Context) error { return queries.read(ctx) }
func (queries *Queries) ReadDocFindFragment(ctx context.Context) error { return queries.read(ctx) }
func (queries *Queries) read(ctx context.Context) error {
 return queries.scope.WithinDocumentRead(ctx, "", func(bound context.Context) error {
  _, err := queries.legacy.GetDoc(bound, "", "")
  return err
 })
}
`

func TestDocumentQueryBoundaryRejectsForbiddenOwnership(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		source string
		want   string
	}{
		{name: "narrow owned reads", source: documentQueryBoundaryFixture},
		{name: "explicit readonly readiness and fields", source: documentQueryBoundaryFixture + `
type DocVisibleReadiness interface { HasDocVisibleIndex(context.Context, domain.ContentHash, domain.ContentHash) (bool, error) }
type DocRangeBoundaryReader interface {
 InReadOnlyTransaction(context.Context) bool
 DocRangeBoundary(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash) (int, error)
}
type FindFieldsReader interface { ReadDocFindFields(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, int) (domain.DocFindFields, error) }
type ViewDependencies struct { VisibleReadiness DocVisibleReadiness; BoundaryRead DocRangeBoundaryReader }
type FindDependencies struct { FieldsRead FindFieldsReader }
`},
		{name: "readonly type name cannot hide writes", source: documentQueryBoundaryFixture + "type DocVisibleReadiness interface { PutDoc(context.Context) error }\n", want: "PutDoc"},
		{name: "malformed production source", source: documentQueryBoundaryFixture + `imported`, want: "parse"},
		{name: "broad blob port", source: strings.Replace(documentQueryBoundaryFixture, "legacy LegacyReader", "legacy ports.BlobStore", 1), want: "broad"},
		{name: "broad metadata alias", source: documentQueryBoundaryFixture + "type Broad = ports.MetadataStore\n", want: "MetadataStore"},
		{name: "worker port", source: documentQueryBoundaryFixture + "type Worker = ports.DocRangeWorkStore\n", want: "DocRangeWorkStore"},
		{name: "command interface", source: documentQueryBoundaryFixture + "type Command interface { PutDoc(context.Context) error }\n", want: "PutDoc"},
		{name: "service pointer", source: documentQueryBoundaryFixture + "type BackReference struct { parent *Service }\n", want: "*Service"},
		{name: "facade callback", source: strings.Replace(documentQueryBoundaryFixture, "type Config struct {", "type Config struct {\n Run func(context.Context) error", 1), want: "callbacks"},
		{name: "embedded reader", source: strings.Replace(documentQueryBoundaryFixture, "type Queries struct {", "type Queries struct {\n ports.DocReadStore", 1), want: "delegates"},
		{name: "missing core method", source: strings.Replace(documentQueryBoundaryFixture, "ReadDocFragments", "OtherFragments", 1), want: "ReadDocFragments"},
		{name: "inherited implementation", source: strings.Replace(documentQueryBoundaryFixture, "type Queries struct {\n scope Scope\n legacy LegacyReader\n}", "type Queries = Config", 1), want: "concrete"},
		{name: "storage rediscovery", source: documentQueryBoundaryFixture + "func discover(value any) { _ = value.(ports.DocReadStore) }\n", want: "rediscover"},
		{name: "aliased rediscovery", source: documentQueryBoundaryFixture + "type OptionalIndex = ports.DocReadStore\nfunc discover(value any) { _ = value.(OptionalIndex) }\n", want: "rediscover"},
		{name: "type switch rediscovery", source: documentQueryBoundaryFixture + "func discover(value any) { switch value.(type) { case ports.DocReadStore: } }\n", want: "rediscover"},
		{name: "unscoped stubs", source: strings.Replace(documentQueryBoundaryFixture, "return queries.read(ctx)", "return nil", 1), want: "owned read logic"},
		{name: "scope without reads", source: strings.Replace(documentQueryBoundaryFixture, "_, err := queries.legacy.GetDoc(bound, \"\", \"\")\n  return err", "return nil", 1), want: "owned read logic"},
		{name: "scope ends before read", source: strings.Replace(documentQueryBoundaryFixture, "return queries.scope.WithinDocumentRead(ctx, \"\", func(bound context.Context) error {\n  _, err := queries.legacy.GetDoc(bound, \"\", \"\")\n  return err\n })", "_ = queries.scope.WithinDocumentRead(ctx, \"\", func(bound context.Context) error { return nil })\n _, err := queries.legacy.GetDoc(ctx, \"\", \"\")\n return err", 1), want: "owned read logic"},
		{name: "scope discards bound context", source: strings.Replace(documentQueryBoundaryFixture, "queries.legacy.GetDoc(bound,", "queries.legacy.GetDoc(ctx,", 1), want: "owned read logic"},
		{name: "query owned admission", source: strings.Replace(documentQueryBoundaryFixture, "func (queries *Queries) read(ctx context.Context) error {", "var extraGate = make(chan struct{}, 1)\nfunc (queries *Queries) read(ctx context.Context) error {\n extraGate <- struct{}{}\n defer func() { <-extraGate }()", 1), want: "admission"},
		{name: "unused scope selector", source: strings.Replace(documentQueryBoundaryFixture, "return queries.scope.WithinDocumentRead(ctx, \"\", func(bound context.Context) error {\n  _, err := queries.legacy.GetDoc(bound, \"\", \"\")\n  return err\n })", "_ = queries.scope.WithinDocumentRead\n _, err := queries.legacy.GetDoc(ctx, \"\", \"\")\n return err", 1), want: "owned read logic"},
		{name: "widened scope", source: strings.Replace(documentQueryBoundaryFixture, "type Scope interface {", "type Scope interface {\n SearchDocEvents(context.Context) error", 1), want: "cannot widen"},
		{name: "erased scope", source: strings.Replace(documentQueryBoundaryFixture, "type Scope interface {\n AuthorizeDocumentRead(context.Context, domain.ContentHash) (context.Context, error)\n AuthorizeDocumentPreparation(context.Context, domain.ContentHash) (context.Context, error)\n WithinDocumentRead(context.Context, domain.ContentHash, func(context.Context) error) error\n WithinDocumentSnapshot(context.Context, domain.ContentHash, func(context.Context) error) error\n}", "type Scope interface{}", 1), want: "missing the explicit"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			root := test.TempDir()
			writeDependencyFixture(test, root, "queries.go", scenario.source)
			violations, err := scanDocumentQueryBoundary(root)
			if scenario.want == "parse" {
				if err == nil {
					test.Fatal("malformed production source passed")
				}
				return
			}
			if err != nil {
				test.Fatal(err)
			}
			if scenario.want == "" {
				if len(violations) != 0 {
					test.Fatal("narrow Config/Queries with real scoped reads was rejected", violations)
				}
				return
			}
			if !strings.Contains(strings.Join(violations, "\n"), scenario.want) {
				test.Fatalf("missed %s boundary: %v", scenario.want, violations)
			}
		})
	}
}

func TestDocumentQueryBoundaryResolvesCrossFileCapabilityAliases(test *testing.T) {
	root := test.TempDir()
	writeDependencyFixture(test, root, "queries.go", documentQueryBoundaryFixture+"type OptionalIndex = ports.DocReadStore\n")
	writeDependencyFixture(test, root, "discover.go", "package documentquery\nfunc discover(value any) { _ = value.(OptionalIndex) }\n")
	violations, err := scanDocumentQueryBoundary(root)
	if err != nil || !strings.Contains(strings.Join(violations, "\n"), "rediscover") {
		test.Fatal("cross-file alias hid runtime capability discovery", violations, err)
	}
}

func TestDocumentQueryBoundaryRejectsForbiddenImports(test *testing.T) {
	for _, dependency := range []string{
		documentQueryModule + "/internal/app",
		documentQueryModule + "/internal/app/service",
		documentQueryModule + "/internal/adapters/store",
		documentQueryModule + "/internal/adapters/delivery/http",
		documentQueryModule + "/internal/adapters/delivery/mcp",
		documentQueryModule + "/internal/ports/commands",
	} {
		for _, relative := range []string{"forbidden.go", "helper/forbidden.go"} {
			test.Run(dependency+"/"+relative, func(test *testing.T) {
				root := test.TempDir()
				writeDependencyFixture(test, root, "queries.go", documentQueryBoundaryFixture)
				writeDependencyFixture(test, root, relative, fmt.Sprintf("package documentquery\nimport hidden %q\n", dependency))
				violations, err := scanDocumentQueryBoundary(root)
				if err != nil || !strings.Contains(strings.Join(violations, "\n"), dependency) {
					test.Fatal("aliased or nested forbidden dependency passed", violations, err)
				}
			})
		}
	}
}

func TestDocumentQueryBoundaryRequiresActualProductionSource(test *testing.T) {
	for _, scenario := range []string{"absent package", "test files only", "wrong package"} {
		test.Run(scenario, func(test *testing.T) {
			root := test.TempDir()
			switch scenario {
			case "absent package":
				root = filepath.Join(root, "missing")
			case "test files only":
				writeDependencyFixture(test, root, "ignored_test.go", "invalid test source must not substitute production ownership")
			case "wrong package":
				writeDependencyFixture(test, root, "queries.go", strings.Replace(documentQueryBoundaryFixture, "package documentquery", "package app", 1))
			}
			if _, err := scanDocumentQueryBoundary(root); err == nil {
				test.Fatal("missing or misplaced documentquery production source silently passed")
			}
		})
	}
}
