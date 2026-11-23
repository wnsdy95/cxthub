package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

type dpc1Finding struct {
	rule   string
	detail string
}

type dpc1CallVisitor struct {
	base  rac1AssetCallVisitor
	visit func(*ast.CallExpr, string)
}

func (walker dpc1CallVisitor) Visit(node ast.Node) ast.Visitor {
	if node == nil {
		return nil
	}
	walker.base = walker.base.Visit(node).(rac1AssetCallVisitor)
	if call, called := node.(*ast.CallExpr); called {
		target := rac1AssetCallTarget(walker.base.visitor, call.Fun)
		if strings.HasPrefix(target, "inbound.DocumentPublicationCommands.") && walker.base.visitor.set.types["Server"] != nil && !strings.HasPrefix(rac1AssetCallOrigin(walker.base, call.Fun), "Server.documentPublications.") {
			target = "unbound." + target
		}
		walker.visit(call, target)
	}
	return walker
}

func dpc1Calls(set wave42LegacyGitSourceSet, function *ast.FuncDecl, node ast.Node, visit func(*ast.CallExpr, string)) map[string]bool {
	calls := map[string]bool{}
	if function == nil || function.Body == nil || node == nil {
		return calls
	}
	visitor := wave45PRCallVisitor{set: set, bindings: map[string]string{}, functions: map[string]string{}, visit: func(string) {}}
	for _, fields := range []*ast.FieldList{function.Recv, function.Type.Params, function.Type.Results} {
		if fields != nil {
			for _, field := range fields.List {
				for _, name := range field.Names {
					visitor.bind(name.Name, nil, wave44PreparationNamedType(field.Type))
				}
			}
		}
	}
	ast.Walk(dpc1CallVisitor{base: rac1AssetCallVisitor{visitor: visitor, origins: map[string]string{}, visit: func(*ast.CallExpr, string) {}}, visit: func(call *ast.CallExpr, target string) {
		if call.Pos() < node.Pos() || call.End() > node.End() {
			return
		}
		calls[target] = true
		if visit != nil {
			visit(call, target)
		}
	}}, function.Body)
	return calls
}

func dpc1Reachable(set wave42LegacyGitSourceSet, entry string) map[string]*ast.FuncDecl {
	visited := map[string]*ast.FuncDecl{}
	var walk func(string)
	walk = func(target string) {
		function := set.methods[target]
		if function == nil || function.Body == nil || visited[target] != nil {
			return
		}
		visited[target] = function
		for called := range dpc1Calls(set, function, function.Body, nil) {
			walk(called)
		}
	}
	walk(entry)
	return visited
}

func dpc1ExecutionCalls(set wave42LegacyGitSourceSet, entry string) map[string]bool {
	calls := map[string]bool{}
	for _, function := range dpc1Reachable(set, entry) {
		for target := range dpc1Calls(set, function, function.Body, nil) {
			calls[target] = true
		}
	}
	return calls
}

func dpc1Fields(set wave42LegacyGitSourceSet, owner string, allowed, required []string) []dpc1Finding {
	var findings []dpc1Finding
	for _, detail := range rac1AssetFields(set, owner, allowed, required) {
		findings = append(findings, dpc1Finding{"fields", detail})
	}
	return findings
}

func dpc1NarrowSource(set wave42LegacyGitSourceSet, expression ast.Expr, active map[string]bool) bool {
	kind := wave42LegacyGitType(expression)
	switch kind {
	// FinalizedDocAttestor is a single-method port: it looks up the completed
	// finalization job that already verified the stored document.
	case "[]domain.DocumentIdentity", "outbound.RepositoryReader", "outbound.StoredDocVerifier", "outbound.FinalizedDocAttestor":
		return true
	}
	if declaration := set.types[kind]; declaration != nil && !active[kind] {
		active[kind] = true
		defer delete(active, kind)
		return dpc1NarrowSource(set, declaration.Type, active)
	}
	contract, narrow := expression.(*ast.InterfaceType)
	if !narrow || len(contract.Methods.List) == 0 {
		return false
	}
	for _, method := range contract.Methods.List {
		if len(method.Names) == 0 {
			return false
		}
		if _, callable := method.Type.(*ast.FuncType); !callable {
			return false
		}
		for _, name := range method.Names {
			switch name.Name {
			case "GetRepo", "GetDoc", "VerifyStoredDoc", "VerifyIntegrity":
			default:
				return false
			}
		}
	}
	return true
}

func dpc1Interface(set wave42LegacyGitSourceSet, owner string, methods []string) []dpc1Finding {
	declaration := set.types[owner]
	if declaration == nil {
		return []dpc1Finding{{"ports", "missing " + owner}}
	}
	contract, narrow := declaration.Type.(*ast.InterfaceType)
	if !narrow || declaration.Assign.IsValid() {
		return []dpc1Finding{{"ports", "aliased/aggregate contract " + owner}}
	}
	wanted := map[string]bool{}
	for _, method := range methods {
		wanted[method] = true
	}
	var findings []dpc1Finding
	for _, method := range contract.Methods.List {
		if len(method.Names) == 0 {
			findings = append(findings, dpc1Finding{"ports", "embedded authority in " + owner})
		}
		for _, name := range method.Names {
			if !wanted[name.Name] {
				findings = append(findings, dpc1Finding{"ports", "unrelated capability " + owner + "." + name.Name})
			}
			delete(wanted, name.Name)
		}
	}
	for method := range wanted {
		findings = append(findings, dpc1Finding{"ports", "missing capability " + owner + "." + method})
	}
	return findings
}

func dpc1Compare(expression ast.Expr, operation token.Token, left, right string) bool {
	if parenthesized, wrapped := expression.(*ast.ParenExpr); wrapped {
		return dpc1Compare(parenthesized.X, operation, left, right)
	}
	comparison, compared := expression.(*ast.BinaryExpr)
	return compared && comparison.Op == operation && workerSelectorPath(comparison.X) == left && workerSelectorPath(comparison.Y) == right
}

func dpc1Joined(expression ast.Expr, operation token.Token, predicate func(ast.Expr) bool) bool {
	if predicate(expression) {
		return true
	}
	if parenthesized, wrapped := expression.(*ast.ParenExpr); wrapped {
		return dpc1Joined(parenthesized.X, operation, predicate)
	}
	joined, binary := expression.(*ast.BinaryExpr)
	return binary && joined.Op == operation && (dpc1Joined(joined.X, operation, predicate) || dpc1Joined(joined.Y, operation, predicate))
}

func dpc1ReturnsError(block *ast.BlockStmt, expected string) bool {
	if !rac1AssetRejects(block) {
		return false
	}
	returned := block.List[len(block.List)-1].(*ast.ReturnStmt)
	return workerSelectorPath(returned.Results[len(returned.Results)-1]) == expected
}

func dpc1Constructor(set wave42LegacyGitSourceSet) []dpc1Finding {
	constructor := set.methods["NewDocumentPublicationCommands"]
	if constructor == nil || constructor.Body == nil || constructor.Type.Params == nil || len(constructor.Type.Params.List) == 0 || len(constructor.Type.Params.List[0].Names) == 0 {
		return []dpc1Finding{{"constructor", "missing explicit owner constructor"}}
	}
	config := constructor.Type.Params.List[0].Names[0].Name
	allocation := constructor.Body.End()
	ast.Inspect(constructor.Body, func(node ast.Node) bool {
		if literal, composite := node.(*ast.CompositeLit); composite && wave42LegacyGitType(literal.Type) == "DocumentPublicationCommands" && literal.Pos() < allocation {
			allocation = literal.Pos()
		}
		return true
	})
	var findings []dpc1Finding
	if allocation == constructor.Body.End() {
		findings = append(findings, dpc1Finding{"constructor", "constructor does not allocate its independent owner"})
	}
	guarded := map[string]bool{}
	ast.Inspect(constructor.Body, func(node ast.Node) bool {
		conditional, condition := node.(*ast.IfStmt)
		if !condition || conditional.End() >= allocation {
			return true
		}
		if dpc1ReturnsError(conditional.Body, "domain.ErrValidation") {
			for _, dependency := range []string{"Store", "Chunks", "Scope"} {
				guarded[dependency] = guarded[dependency] || dpc1Joined(conditional.Cond, token.LOR, func(expression ast.Expr) bool {
					return rac1AssetAbsent(expression, config+"."+dependency)
				})
			}
			profiles := true
			for _, profile := range []string{"DocumentPublicationCompatibility", "DocumentPublicationComplete"} {
				profiles = profiles && dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool {
					return dpc1Compare(expression, token.NEQ, config+".Profile", profile)
				})
			}
			guarded["profiles"] = guarded["profiles"] || profiles
			if negated, negative := conditional.Cond.(*ast.UnaryExpr); negative && negated.Op == token.NOT {
				if indexed, index := negated.X.(*ast.IndexExpr); index && workerSelectorPath(indexed.Index) == "domain.DocumentIdentityLegacy" {
					guarded["legacy identity"] = true
				}
			}
		}
		if dpc1ReturnsError(conditional.Body, "domain.ErrCatalogUnsupported") {
			complete := dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool {
				return dpc1Compare(expression, token.EQL, config+".Profile", "DocumentPublicationComplete")
			})
			for _, dependency := range []string{"Sources", "Verified"} {
				guarded["complete "+dependency] = guarded["complete "+dependency] || complete && dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool {
					return dpc1Joined(expression, token.LOR, func(candidate ast.Expr) bool {
						return dpc1Compare(candidate, token.EQL, config+"."+dependency, "nil")
					})
				})
			}
		}
		return true
	})
	ast.Inspect(constructor.Body, func(node ast.Node) bool {
		loop, ranged := node.(*ast.RangeStmt)
		if !ranged || loop.End() >= allocation {
			return true
		}
		variable, named := loop.Value.(*ast.Ident)
		values, listed := loop.X.(*ast.CompositeLit)
		if named && listed {
			for _, statement := range loop.Body.List {
				conditional, condition := statement.(*ast.IfStmt)
				if !condition || !dpc1ReturnsError(conditional.Body, "domain.ErrValidation") {
					continue
				}
				nonNil := dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool {
					return dpc1Compare(expression, token.NEQ, variable.Name, "nil")
				})
				typedNil := dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool {
					return rac1AssetAbsent(expression, variable.Name)
				})
				if nonNil && typedNil {
					for _, value := range values.Elts {
						guarded["typed nil "+workerSelectorPath(value)] = true
					}
				}
			}
		}
		if workerSelectorPath(loop.X) == config+".SupportedIdentities" && variable != nil {
			for _, statement := range loop.Body.List {
				if conditional, condition := statement.(*ast.IfStmt); condition && dpc1ReturnsError(conditional.Body, "domain.ErrValidation") && wave42LegacyGitCalls(conditional.Cond)["Validate"] {
					guarded["valid identities"] = true
				}
			}
		}
		return true
	})
	for _, requirement := range []string{"Store", "Chunks", "Scope", "profiles", "legacy identity", "valid identities", "complete Sources", "complete Verified", "typed nil " + config + ".Sources", "typed nil " + config + ".Verified", "typed nil " + config + ".LegacyWriter"} {
		if !guarded[requirement] {
			findings = append(findings, dpc1Finding{"constructor", "missing explicit rejection: " + requirement})
		}
	}
	presence := set.methods["rootAdapterPresent"]
	if presence == nil || !wave42LegacyGitCalls(presence.Body)["ValueOf"] || !wave42LegacyGitCalls(presence.Body)["IsNil"] {
		findings = append(findings, dpc1Finding{"constructor", "typed-nil presence implementation missing"})
	}
	return findings
}

func dpc1Owner(set wave42LegacyGitSourceSet) []dpc1Finding {
	ports := []string{"DocumentPublicationProfile", "DocumentPublicationStore", "DocumentPublicationChunkStore", "DocumentPublicationScope", "DocumentPublicationSources", "outbound.VerifiedDocStore", "DocumentPublicationLegacyWriter", "[]domain.DocumentIdentity"}
	findings := dpc1Fields(set, "DocumentPublicationCommands", append(append([]string{}, ports...), "bool"), ports)
	findings = append(findings, dpc1Fields(set, "DocumentPublicationCommandsConfig", append(append([]string{}, ports...), "DocumentPublicationProfile"), append(append([]string{}, ports...), "DocumentPublicationProfile"))...)
	for owner, methods := range map[string][]string{
		"DocumentPublicationStore":        {"GetRepo", "ListSnapshots", "GetSettingsObject", "GetSnapshot", "PutSnapshot"},
		"DocumentPublicationChunkStore":   {"GetChunk", "PutChunks"},
		"DocumentPublicationLegacyWriter": {"PutDoc"},
		"DocumentPublicationSources":      {"VerifySnapshotDocument"},
		"DocumentPublicationScope":        {"WithinDocumentCommit", "WithinChunkStore", "GraphChanged"},
	} {
		findings = append(findings, dpc1Interface(set, owner, methods)...)
	}
	sources := set.types["documentPublicationSources"]
	if sources == nil {
		findings = append(findings, dpc1Finding{"sources", "missing narrow stored-document source adapter"})
	} else if shape, concrete := sources.Type.(*ast.StructType); !concrete || sources.Assign.IsValid() {
		findings = append(findings, dpc1Finding{"sources", "source adapter must be concrete"})
	} else {
		for _, field := range shape.Fields.List {
			if len(field.Names) == 0 || !dpc1NarrowSource(set, field.Type, map[string]bool{}) {
				findings = append(findings, dpc1Finding{"sources", "broad/service/callback source field " + wave42LegacyGitType(field.Type)})
			}
		}
	}
	sourceCalls := dpc1ExecutionCalls(set, "documentPublicationSources.VerifySnapshotDocument")
	sharedCalls := dpc1ExecutionCalls(set, "verifyStoredSnapshotDocument")
	if !sourceCalls["verifyStoredSnapshotDocument"] && !sharedCalls["documentPublicationSources.VerifySnapshotDocument"] {
		findings = append(findings, dpc1Finding{"sources", "source adapter does not reuse actual stored-document validation policy"})
	}
	for _, operation := range []string{"outbound.StoredDocVerifier.VerifyStoredDoc", "documentPublicationLegacyReader.GetDoc", "documentPublicationIntegrityVerifier.VerifyIntegrity", "requireRootDocumentRepository"} {
		if !sourceCalls[operation] {
			findings = append(findings, dpc1Finding{"sources", "shared source policy lost current-byte/identity validation: " + operation})
		}
	}
	for entry, required := range map[string][]string{
		"Commit":      {"DocumentPublicationScope.WithinDocumentCommit", "DocumentPublicationCommands.commit", "domain.AssembleDocChunks", "domain.VerifySessionDoc", "domain.ValidateSnapshotHashes", "domain.ValidateSnapshotGraphUpdate", "DocumentPublicationChunkStore.GetChunk", "DocumentPublicationStore.GetRepo", "DocumentPublicationStore.ListSnapshots", "DocumentPublicationStore.GetSettingsObject", "DocumentPublicationSources.VerifySnapshotDocument", "outbound.VerifiedDocStore.PutVerifiedDoc", "DocumentPublicationLegacyWriter.PutDoc", "DocumentPublicationStore.GetSnapshot", "DocumentPublicationStore.PutSnapshot", "DocumentPublicationScope.GraphChanged"},
		"StoreChunks": {"DocumentPublicationScope.WithinChunkStore", "DocumentPublicationCommands.storeChunks", "domain.ValidateContentHash", "domain.HashContent", "bytes.Equal", "DocumentPublicationStore.GetRepo", "DocumentPublicationChunkStore.PutChunks"},
	} {
		root := "DocumentPublicationCommands." + entry
		calls := dpc1ExecutionCalls(set, root)
		for _, operation := range required {
			if !calls[operation] {
				findings = append(findings, dpc1Finding{"payload", root + " lost actual policy/storage call " + operation})
			}
		}
		scoped := false
		dpc1Calls(set, set.methods[root], bodyOfDPC1(set.methods[root]), func(call *ast.CallExpr, target string) {
			if target != required[0] {
				return
			}
			for _, argument := range call.Args {
				callback, literal := argument.(*ast.FuncLit)
				if !literal || callback.Type.Params == nil || len(callback.Type.Params.List) == 0 || len(callback.Type.Params.List[0].Names) == 0 {
					continue
				}
				bound := callback.Type.Params.List[0].Names[0].Name
				dpc1Calls(set, set.methods[root], callback.Body, func(nested *ast.CallExpr, selected string) {
					scoped = scoped || selected == required[1] && len(nested.Args) > 0 && workerSelectorPath(nested.Args[0]) == bound
				})
			}
		})
		if !scoped {
			findings = append(findings, dpc1Finding{"scope", root + " does not execute its owned payload with the bound context"})
		}
	}
	for root := range set.methods {
		if root != "NewDocumentPublicationCommands" && !strings.HasPrefix(root, "DocumentPublicationCommands.") && !strings.HasPrefix(root, "documentPublicationSources.") {
			continue
		}
		for key, function := range dpc1Reachable(set, root) {
			if strings.HasPrefix(key, "Service.") || strings.HasPrefix(key, "IdentityService.") {
				findings = append(findings, dpc1Finding{"reverse", root + " delegates back to " + key})
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if identifier, named := node.(*ast.Ident); named && (identifier.Name == "Service" || identifier.Name == "NewService" || identifier.Name == "IdentityService" || identifier.Name == "NewIdentityService") {
					findings = append(findings, dpc1Finding{"reverse", "reachable facade construction/callback in " + key})
				}
				return true
			})
		}
	}
	return append(findings, dpc1Constructor(set)...)
}

func bodyOfDPC1(function *ast.FuncDecl) ast.Node {
	if function == nil {
		return nil
	}
	return function.Body
}

func dpc1Body(test *testing.T, statements string) *ast.BlockStmt {
	test.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "negative.go", "package fixture\nfunc mutation() { "+statements+" }", 0)
	if err != nil {
		test.Fatal(err)
	}
	return file.Decls[0].(*ast.FuncDecl).Body
}

func dpc1RequireRule(test *testing.T, findings []dpc1Finding, rule string) {
	test.Helper()
	for _, finding := range findings {
		if finding.rule == rule {
			return
		}
	}
	test.Fatalf("negative model escaped %s guard: %+v", rule, findings)
}

func TestDPC1CallgraphDistinguishesAliasesSharedWorkAndReverseDelegation(test *testing.T) {
	const fixture = `package app
type DocumentPublicationCommands struct { scope *documentPublicationScope }
type documentPublicationScope struct { work *repositoryWork }
type repositoryWork struct { metadata outbound.MetadataStore; blobs outbound.BlobStore }
type Service struct {}
func (commands *DocumentPublicationCommands) Commit() { alias := commands; execute := alias.commit; execute() }
func (commands *DocumentPublicationCommands) commit() { sharedPolicy[int](); commands.scope.within() }
func sharedPolicy[Result any]() {}
func (scope *documentPublicationScope) within() { scope.work.write() }
func (work *repositoryWork) write() {}
func (service *Service) commit() { NewService() }
func commit() { NewService() }
func unrelated() { NewService() }
`
	set, err := wave42LegacyGitParseSources(map[string]string{"fixture.go": fixture})
	if err != nil {
		test.Fatal(err)
	}
	reachable := dpc1Reachable(set, "DocumentPublicationCommands.Commit")
	for _, required := range []string{"DocumentPublicationCommands.commit", "sharedPolicy", "documentPublicationScope.within", "repositoryWork.write"} {
		if reachable[required] == nil {
			test.Fatal("callgraph lost receiver alias, method value, generic helper or shared scope", required)
		}
	}
	if reachable["Service.commit"] != nil || reachable["commit"] != nil || reachable["unrelated"] != nil {
		test.Fatal("callgraph confuses same-named receivers or unrelated helpers")
	}
	set.methods["sharedPolicy"].Body = dpc1Body(test, "legacy := &Service{}; callback := legacy.commit; callback()")
	if dpc1Reachable(set, "DocumentPublicationCommands.Commit")["Service.commit"] == nil {
		test.Fatal("negative model hid facade back-delegation behind generic helper and method value")
	}
	dpc1RequireRule(test, dpc1Owner(set), "reverse")
}
