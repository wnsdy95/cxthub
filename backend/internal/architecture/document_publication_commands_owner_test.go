package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
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
	case "[]domain.DocumentIdentity", "outbound.RepositoryReader", "outbound.StoredDocVerifier":
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

func dpc1SharedScope(set wave42LegacyGitSourceSet) []dpc1Finding {
	findings := dpc1Fields(set, "documentPublicationScope", []string{"*repositoryWork", "bool"}, []string{"*repositoryWork"})
	for _, entry := range []string{"WithinDocumentCommit", "WithinChunkStore"} {
		calls := dpc1ExecutionCalls(set, "documentPublicationScope."+entry)
		for _, required := range []string{"AcquireResources", "repositoryWriteWithWork"} {
			if !calls[required] {
				findings = append(findings, dpc1Finding{"scope", "publication scope lost shared resource/transaction policy: " + entry + " " + required})
			}
		}
	}
	if !dpc1ExecutionCalls(set, "documentPublicationScope.GraphChanged")["repositoryWork.revisionWrite"] {
		findings = append(findings, dpc1Finding{"scope", "graph change bypasses existing repository revision policy"})
	}
	for root, function := range set.methods {
		if !strings.HasPrefix(root, "DocumentPublicationCommands.") && !strings.HasPrefix(root, "documentPublicationScope.") && root != "NewDocumentPublicationCommands" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.GoStmt, *ast.ChanType:
				findings = append(findings, dpc1Finding{"scope", "publication owner starts independent work/admission: " + root})
			case *ast.CompositeLit:
				if wave42LegacyGitType(value.Type) == "repositoryWork" {
					findings = append(findings, dpc1Finding{"scope", "publication owner constructs replacement repository authority: " + root})
				}
			}
			return true
		})
	}
	return findings
}

func bodyOfDPC1(function *ast.FuncDecl) ast.Node {
	if function == nil {
		return nil
	}
	return function.Body
}

func dpc1Facade(set wave42LegacyGitSourceSet) []dpc1Finding {
	var findings []dpc1Finding
	for entry, delegate := range map[string]string{"Commit": "Commit", "StoreChunks": "StoreChunks", "commit": "commit"} {
		function := set.methods["Service."+entry]
		calls := dpc1Calls(set, function, bodyOfDPC1(function), nil)
		if !calls["Service.documentPublications"] || !calls["DocumentPublicationCommands."+delegate] {
			findings = append(findings, dpc1Finding{"facade", "missing thin factory/delegate Service." + entry})
		}
		for target := range calls {
			if target != "Service.documentPublications" && target != "DocumentPublicationCommands."+delegate {
				findings = append(findings, dpc1Finding{"facade", "policy retained in Service." + entry + ": " + target})
			}
		}
		if function == nil || function.Body == nil {
			continue
		}
		assemblyError := ""
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if assignment, assigned := node.(*ast.AssignStmt); assigned && len(assignment.Lhs) == 2 && len(assignment.Rhs) == 1 {
				if call, called := assignment.Rhs[0].(*ast.CallExpr); called && wave42LegacyGitFunctionName(call.Fun) == "documentPublications" {
					assemblyError = workerSelectorPath(assignment.Lhs[1])
					if len(call.Args) == 0 || workerSelectorPath(call.Args[0]) != "DocumentPublicationCompatibility" {
						findings = append(findings, dpc1Finding{"facade", "compatibility facade lost explicit profile: " + entry})
					}
				}
			}
			switch value := node.(type) {
			case *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.FuncLit, *ast.GoStmt, *ast.DeferStmt:
				findings = append(findings, dpc1Finding{"facade", "algorithm/scope retained in Service." + entry})
			case *ast.IfStmt:
				if assemblyError == "" || !dpc1Compare(value.Cond, token.NEQ, assemblyError, "nil") || !dpc1ReturnsError(value.Body, assemblyError) || value.Else != nil {
					findings = append(findings, dpc1Finding{"facade", "policy branch retained in Service." + entry})
				}
			}
			return true
		})
	}
	factory := set.methods["Service.DocumentPublicationCommands"]
	if factory == nil || factory.Body == nil || len(factory.Body.List) != 1 {
		findings = append(findings, dpc1Finding{"facade", "missing thin complete public factory"})
	} else if returned, returns := factory.Body.List[0].(*ast.ReturnStmt); !returns || len(returned.Results) != 1 {
		findings = append(findings, dpc1Finding{"facade", "public factory retained policy"})
	} else if call, called := returned.Results[0].(*ast.CallExpr); !called || wave42LegacyGitFunctionName(call.Fun) != "documentPublications" || len(call.Args) != 1 || workerSelectorPath(call.Args[0]) != "DocumentPublicationComplete" {
		findings = append(findings, dpc1Finding{"facade", "public factory lost explicit complete profile"})
	}
	if set.methods["Service.storeChunksCommand"] != nil {
		findings = append(findings, dpc1Finding{"facade", "old chunk payload remains on Service"})
	}
	borrowed := false
	for key, function := range set.methods {
		if dpc1Calls(set, function, function.Body, nil)["Service.commit"] {
			if key != "Service.PublishMemoryArchive" {
				findings = append(findings, dpc1Finding{"legacy", "private legacy commit acquired another caller: " + key})
			}
			borrowed = borrowed || key == "Service.PublishMemoryArchive"
		}
	}
	if !borrowed {
		findings = append(findings, dpc1Finding{"legacy", "memory archive lost its in-transaction private delegate"})
	}
	return findings
}

func dpc1SnapshotPolicy(application, domain wave42LegacyGitSourceSet) []dpc1Finding {
	var findings []dpc1Finding
	for compatibility, policy := range map[string]string{"validateSnapshotHashes": "ValidateSnapshotHashes", "validateSnapshotGraph": "ValidateSnapshotGraphUpdate"} {
		function := application.methods[compatibility]
		if compatibility == "validateSnapshotGraph" && function == nil {
			continue
		}
		thin := false
		if function != nil && function.Body != nil && len(function.Body.List) == 1 {
			if returned, returns := function.Body.List[0].(*ast.ReturnStmt); returns && len(returned.Results) == 1 {
				if call, called := returned.Results[0].(*ast.CallExpr); called && workerSelectorPath(call.Fun) == "domain."+policy {
					thin = true
				}
			}
		}
		if !thin {
			findings = append(findings, dpc1Finding{"snapshot", "duplicated or absent compatibility delegate: " + compatibility})
		}
	}
	for policy, required := range map[string][]string{"ValidateSnapshotHashes": {"ValidateContentHash", "ValidateOptionalContentHash"}, "ValidateSnapshotGraphUpdate": {"ReachabilityParents"}} {
		function := domain.methods[policy]
		if function == nil || function.Body == nil {
			findings = append(findings, dpc1Finding{"snapshot", "missing actual domain policy " + policy})
			continue
		}
		calls := map[string]bool{}
		for _, reachable := range dpc1Reachable(domain, policy) {
			for called := range wave42LegacyGitCalls(reachable.Body) {
				calls[called] = true
			}
			for _, fields := range []*ast.FieldList{reachable.Recv, reachable.Type.Params, reachable.Type.Results} {
				if fields == nil {
					continue
				}
				ast.Inspect(fields, func(node ast.Node) bool {
					if selector, selected := node.(*ast.SelectorExpr); selected && workerSelectorPath(selector.X) != "" {
						findings = append(findings, dpc1Finding{"snapshot", "domain snapshot validation accepts external authority: " + policy})
					}
					return true
				})
			}
		}
		for _, operation := range required {
			if !calls[operation] {
				findings = append(findings, dpc1Finding{"snapshot", "domain policy is a stub: " + policy + " " + operation})
			}
		}
	}
	return findings
}

func dpc1HTTP(set wave42LegacyGitSourceSet) []dpc1Finding {
	var findings []dpc1Finding
	if wave44PreparationMember(set, "Server", "documentPublications", false, map[string]bool{}) != "inbound.DocumentPublicationCommands" {
		findings = append(findings, dpc1Finding{"http", "Server lost narrow documentPublications field"})
	}
	setter := set.methods["Server.SetDocumentPublicationCommands"]
	if setter == nil || !wave42LegacyGitCalls(setter.Body)["IsNil"] {
		findings = append(findings, dpc1Finding{"http", "setter lost typed-nil rejection"})
	}
	bound := false
	if setter != nil && setter.Body != nil && setter.Recv != nil && len(setter.Recv.List[0].Names) > 0 && setter.Type.Params != nil {
		receiver := setter.Recv.List[0].Names[0].Name
		ast.Inspect(setter.Body, func(node ast.Node) bool {
			if assignment, assigned := node.(*ast.AssignStmt); assigned && len(assignment.Lhs) == 1 && len(assignment.Rhs) == 1 && workerSelectorPath(assignment.Lhs[0]) == receiver+".documentPublications" {
				for _, parameter := range setter.Type.Params.List {
					if wave42LegacyGitType(parameter.Type) == "inbound.DocumentPublicationCommands" {
						for _, name := range parameter.Names {
							bound = bound || workerSelectorPath(assignment.Rhs[0]) == name.Name
						}
					}
				}
			}
			return true
		})
	}
	if !bound {
		findings = append(findings, dpc1Finding{"http", "setter discards the injected narrow owner"})
	}
	for route, expected := range map[string][]string{
		"POST /api/v1/repos/{repoID}/push/chunks":  {"pushChunks", "StoreChunks"},
		"POST /api/v1/repos/{repoID}/push/objects": {"pushObjects", "Commit"},
	} {
		registered := false
		for _, function := range dpc1Reachable(set, "Server.Handler") {
			dpc1Calls(set, function, function.Body, func(call *ast.CallExpr, target string) {
				if !strings.HasSuffix(target, ".HandleFunc") || len(call.Args) != 2 {
					return
				}
				literal, quoted := call.Args[0].(*ast.BasicLit)
				if !quoted || literal.Kind != token.STRING {
					return
				}
				pattern, err := strconv.Unquote(literal.Value)
				if err != nil || pattern != route {
					return
				}
				ast.Inspect(call.Args[1], func(node ast.Node) bool {
					if selector, selected := node.(*ast.SelectorExpr); selected && selector.Sel.Name == expected[0] && function.Recv != nil {
						for _, receiver := range function.Recv.List {
							for _, name := range receiver.Names {
								registered = registered || wave44PreparationNamedType(receiver.Type) == "Server" && workerSelectorPath(selector.X) == name.Name
							}
						}
					}
					return true
				})
			})
		}
		if !registered {
			findings = append(findings, dpc1Finding{"http", "missing/misdirected live route " + route})
		}
		calls := dpc1ExecutionCalls(set, "Server."+expected[0])
		wanted := "inbound.DocumentPublicationCommands." + expected[1]
		if !calls[wanted] {
			findings = append(findings, dpc1Finding{"http", "route does not invoke injected documentPublications: " + route})
		}
		for target := range calls {
			if (strings.HasSuffix(target, ".Commit") || strings.HasSuffix(target, ".StoreChunks")) && target != wanted {
				findings = append(findings, dpc1Finding{"http", "route bypasses injected publication owner: " + target})
			}
		}
	}
	return findings
}

func dpc1Initializes(node ast.Node) bool {
	found := false
	if node == nil {
		return false
	}
	ast.Inspect(node, func(nested ast.Node) bool {
		if identifier, named := nested.(*ast.Ident); named {
			switch identifier.Name {
			case "DocumentPublications", "documentPublications", "DocumentPublicationCommands", "NewDocumentPublicationCommands", "ConfigureDocumentPublicationCommands", "SetDocumentPublicationCommands":
				found = true
			}
		}
		return true
	})
	return found
}

func dpc1Runtime(set wave42LegacyGitSourceSet) []dpc1Finding {
	var findings []dpc1Finding
	for entry, queryOnly := range map[string]string{"Open": "false", "OpenMCP": "true"} {
		correct := false
		function := set.methods[entry]
		dpc1Calls(set, function, bodyOfDPC1(function), func(call *ast.CallExpr, target string) {
			correct = correct || target == "openRuntime" && len(call.Args) > 0 && workerSelectorPath(call.Args[len(call.Args)-1]) == queryOnly
		})
		if !correct {
			findings = append(findings, dpc1Finding{"runtime", "API/MCP query-only entry changed: " + entry})
		}
	}
	function := set.methods["openRuntime"]
	if function == nil || function.Body == nil {
		return append(findings, dpc1Finding{"runtime", "missing runtime composition"})
	}
	boundary := -1
	for index, statement := range function.Body.List {
		if conditional, condition := statement.(*ast.IfStmt); condition && workerSelectorPath(conditional.Cond) == "queriesOnly" && conditional.Else == nil && len(conditional.Body.List) > 0 {
			_, returned := conditional.Body.List[len(conditional.Body.List)-1].(*ast.ReturnStmt)
			if returned && dpc1Calls(set, function, conditional.Body, nil)["Runtime.configureMCPReads"] {
				boundary = index
			}
		}
	}
	configured := false
	for index, statement := range function.Body.List {
		calls := dpc1Calls(set, function, statement, nil)
		if boundary >= 0 && index > boundary {
			configured = configured || calls["Runtime.ConfigureDocumentPublicationCommands"]
			continue
		}
		if dpc1Initializes(statement) || wave42LegacyGitCalls(statement)["NewService"] {
			findings = append(findings, dpc1Finding{"runtime", "publication owner initialized on MCP prefix"})
		}
		for target := range calls {
			for key, reachable := range dpc1Reachable(set, target) {
				if dpc1Initializes(reachable.Body) || wave42LegacyGitCalls(reachable.Body)["NewService"] {
					findings = append(findings, dpc1Finding{"runtime", "indirect publication initialization on MCP prefix: " + key})
				}
			}
		}
	}
	if boundary < 0 || !configured {
		findings = append(findings, dpc1Finding{"runtime", "API-only publication configuration missing after MCP return"})
	}
	configuration := set.methods["Runtime.ConfigureDocumentPublicationCommands"]
	output, bound, contextGuard := "", false, false
	if configuration != nil && configuration.Body != nil && configuration.Recv != nil && len(configuration.Recv.List[0].Names) > 0 {
		receiver := configuration.Recv.List[0].Names[0].Name
		for _, statement := range configuration.Body.List {
			if conditional, condition := statement.(*ast.IfStmt); condition && rac1AssetRejects(conditional.Body) {
				contextGuard = contextGuard || dpc1Joined(conditional.Cond, token.LOR, func(expression ast.Expr) bool {
					return dpc1Compare(expression, token.EQL, receiver+".Context", "nil")
				})
			}
		}
		ast.Inspect(configuration.Body, func(node ast.Node) bool {
			assignment, assigned := node.(*ast.AssignStmt)
			if !assigned || len(assignment.Rhs) != 1 || len(assignment.Lhs) == 0 {
				return true
			}
			if call, called := assignment.Rhs[0].(*ast.CallExpr); called && workerSelectorPath(call.Fun) == receiver+".Context.DocumentPublicationCommands" {
				output = workerSelectorPath(assignment.Lhs[0])
			}
			bound = bound || output != "" && workerSelectorPath(assignment.Lhs[0]) == receiver+".DocumentPublications" && workerSelectorPath(assignment.Rhs[0]) == output
			return true
		})
	}
	if !bound || !contextGuard {
		findings = append(findings, dpc1Finding{"runtime", "runtime lost Context-nil guard or actual owner result binding"})
	}
	return findings
}

func dpc1MCP(set wave42LegacyGitSourceSet) []dpc1Finding {
	function := set.methods["HandlerWithReadPreparation"]
	rejected := false
	if function != nil && function.Body != nil && function.Type.Params != nil && len(function.Type.Params.List[0].Names) > 0 {
		runtime := function.Type.Params.List[0].Names[0].Name
		for _, statement := range function.Body.List {
			if conditional, condition := statement.(*ast.IfStmt); condition && rac1AssetRejects(conditional.Body) {
				rejected = rejected || dpc1Joined(conditional.Cond, token.LOR, func(expression ast.Expr) bool {
					return dpc1Compare(expression, token.NEQ, runtime+".DocumentPublications", "nil")
				})
			}
		}
	}
	if !rejected {
		return []dpc1Finding{{"mcp", "MCP root accepts runtime.DocumentPublications"}}
	}
	return nil
}

func dpc1APIBinding(set wave42LegacyGitSourceSet) []dpc1Finding {
	configured, bound := false, false
	for _, function := range dpc1Reachable(set, "serve") {
		owner := ""
		dpc1Calls(set, function, function.Body, func(call *ast.CallExpr, target string) {
			if strings.HasSuffix(target, ".ConfigureDocumentPublicationCommands") {
				configured = true
				owner = strings.TrimSuffix(workerSelectorPath(call.Fun), ".ConfigureDocumentPublicationCommands")
			}
			bound = bound || owner != "" && strings.HasSuffix(target, ".SetDocumentPublicationCommands") && len(call.Args) == 1 && workerSelectorPath(call.Args[0]) == owner+".DocumentPublications"
		})
	}
	if !configured || !bound {
		return []dpc1Finding{{"binding", "API root must explicitly configure then inject runtime.DocumentPublications"}}
	}
	return nil
}

func dpc1Report(test *testing.T, findings []dpc1Finding) {
	test.Helper()
	for _, finding := range findings {
		test.Errorf("DPC-1 %s: %s", finding.rule, finding.detail)
	}
}

func TestDPC1DocumentPublicationOwnsPayloadWithoutServiceBackDelegation(test *testing.T) {
	application := wave48Package(test, filepath.Join("..", "app"))
	dpc1Report(test, dpc1Owner(application))
	dpc1Report(test, dpc1SharedScope(application))
	dpc1Report(test, dpc1Facade(application))
	dpc1Report(test, dpc1SnapshotPolicy(application, wave48Package(test, filepath.Join("..", "domain"))))
	dpc1Report(test, dpc1Interface(wave48Package(test, filepath.Join("..", "ports", "inbound")), "DocumentPublicationCommands", []string{"Commit", "StoreChunks"}))
}

func TestDPC1DocumentPublicationHTTPAndAPIOnlyComposition(test *testing.T) {
	dpc1Report(test, dpc1HTTP(wave48Package(test, filepath.Join("..", "adapters", "delivery", "http"))))
	dpc1Report(test, dpc1Runtime(wave48Package(test, filepath.Join("..", "serverruntime"))))
	dpc1Report(test, dpc1MCP(wave48Package(test, filepath.Join("..", "mcpserver"))))
	dpc1Report(test, dpc1APIBinding(wave48Package(test, filepath.Join("..", "..", "cmd", "cxtd"))))
	for _, boundary := range []struct {
		directory string
		entry     string
	}{
		{filepath.Join("..", "serverruntime"), "Runtime.configureMCPReads"},
		{filepath.Join("..", "app"), "NewReadServices"},
	} {
		set := wave48Package(test, boundary.directory)
		if set.methods[boundary.entry] == nil {
			test.Fatal("missing read-only composition", boundary.entry)
		}
		for key, function := range dpc1Reachable(set, boundary.entry) {
			if dpc1Initializes(function.Body) || wave42LegacyGitCalls(function.Body)["NewService"] {
				test.Error("read-only composition constructs publication owner", key)
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if assignment, assigned := node.(*ast.AssignStmt); assigned && function.Recv != nil {
					for _, target := range assignment.Lhs {
						for _, receiver := range function.Recv.List {
							for _, name := range receiver.Names {
								if wave44PreparationNamedType(receiver.Type) == "Runtime" && workerSelectorPath(target) == name.Name+".Context" {
									test.Error("read-only composition assigns command Context", key)
								}
							}
						}
					}
				}
				return true
			})
		}
	}
	for path, file := range wave48Package(test, filepath.Join("..", "..", "cmd", "cxt-mcp")).files {
		if dpc1Initializes(file) {
			test.Error("MCP process initializes publication commands", path)
		}
	}
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

func TestDPC1NegativeOwnerModelsRejectBroadFieldsShellsAndDuplicatePolicies(test *testing.T) {
	sources := wave42LegacyGitReadSources(test, filepath.Join("..", "app"))
	for _, scenario := range []struct {
		name   string
		rule   string
		mutate func(wave42LegacyGitSourceSet)
	}{
		{"owner service pointer", "fields", func(set wave42LegacyGitSourceSet) {
			shape := set.types["DocumentPublicationCommands"].Type.(*ast.StructType)
			shape.Fields.List = append(shape.Fields.List, &ast.Field{Names: []*ast.Ident{ast.NewIdent("service")}, Type: &ast.StarExpr{X: ast.NewIdent("Service")}})
		}},
		{"source full metadata", "sources", func(set wave42LegacyGitSourceSet) {
			shape := set.types["documentPublicationSources"].Type.(*ast.StructType)
			shape.Fields.List = append(shape.Fields.List, &ast.Field{Names: []*ast.Ident{ast.NewIdent("metadata")}, Type: &ast.SelectorExpr{X: ast.NewIdent("outbound"), Sel: ast.NewIdent("MetadataStore")}})
		}},
		{"source full blobs", "sources", func(set wave42LegacyGitSourceSet) {
			shape := set.types["documentPublicationSources"].Type.(*ast.StructType)
			shape.Fields.List = append(shape.Fields.List, &ast.Field{Names: []*ast.Ident{ast.NewIdent("blobs")}, Type: &ast.SelectorExpr{X: ast.NewIdent("outbound"), Sel: ast.NewIdent("BlobStore")}})
		}},
		{"commit shell", "payload", func(set wave42LegacyGitSourceSet) {
			set.methods["DocumentPublicationCommands.commit"].Body = dpc1Body(test, "return inbound.CommitOutput{}, nil")
		}},
		{"chunk shell", "payload", func(set wave42LegacyGitSourceSet) {
			set.methods["DocumentPublicationCommands.storeChunks"].Body = dpc1Body(test, "return inbound.StoreChunksOutput{}, nil")
		}},
		{"unscoped commit", "scope", func(set wave42LegacyGitSourceSet) {
			set.methods["DocumentPublicationCommands.Commit"].Body = dpc1Body(test, "return commands.commit(ctx, input)")
		}},
		{"constructor accepts missing ports", "constructor", func(set wave42LegacyGitSourceSet) {
			set.methods["NewDocumentPublicationCommands"].Body = dpc1Body(test, "return &DocumentPublicationCommands{}, nil")
		}},
		{"complete profile loses capability rejection", "constructor", func(set wave42LegacyGitSourceSet) {
			ast.Inspect(set.methods["NewDocumentPublicationCommands"].Body, func(node ast.Node) bool {
				if conditional, condition := node.(*ast.IfStmt); condition && dpc1ReturnsError(conditional.Body, "domain.ErrCatalogUnsupported") {
					conditional.Cond = ast.NewIdent("false")
				}
				return true
			})
		}},
		{"stored source policy stub", "sources", func(set wave42LegacyGitSourceSet) {
			set.methods["documentPublicationSources.VerifySnapshotDocument"].Body = dpc1Body(test, "return nil")
		}},
		{"service payload restored", "facade", func(set wave42LegacyGitSourceSet) {
			set.methods["Service.Commit"].Body = dpc1Body(test, "for _, document := range input.Docs { domain.VerifySessionDoc(document) }; return inbound.CommitOutput{}, nil")
		}},
		{"private commit creates public scope", "facade", func(set wave42LegacyGitSourceSet) {
			set.methods["Service.commit"].Body = dpc1Body(test, "return service.Commit(ctx, input)")
		}},
		{"duplicate app snapshot validation", "snapshot", func(set wave42LegacyGitSourceSet) {
			set.methods["validateSnapshotHashes"].Body = dpc1Body(test, "for _, parent := range snapshot.Parents { if err := domain.ValidateContentHash(parent); err != nil { return err } }; return domain.ValidateSnapshotHashes(snapshot)")
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			set, err := wave42LegacyGitParseSources(sources)
			if err != nil {
				test.Fatal(err)
			}
			baseline := append(dpc1Owner(set), dpc1Facade(set)...)
			domain := wave48Package(test, filepath.Join("..", "domain"))
			baseline = append(baseline, dpc1SnapshotPolicy(set, domain)...)
			if len(baseline) != 0 {
				test.Fatalf("production model must pass before mutation: %+v", baseline)
			}
			scenario.mutate(set)
			findings := append(dpc1Owner(set), dpc1Facade(set)...)
			findings = append(findings, dpc1SnapshotPolicy(set, domain)...)
			dpc1RequireRule(test, findings, scenario.rule)
		})
	}
}

func TestDPC1NegativeHTTPAndRuntimeModelsRejectDisconnectedBindings(test *testing.T) {
	for _, scenario := range []struct {
		name      string
		directory string
		rule      string
		check     func(wave42LegacyGitSourceSet) []dpc1Finding
		mutate    func(wave42LegacyGitSourceSet)
	}{
		{"HTTP backend fallback", filepath.Join("..", "adapters", "delivery", "http"), "http", dpc1HTTP, func(set wave42LegacyGitSourceSet) {
			function := set.methods["Server.pushObjects"]
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if selector, selected := node.(*ast.SelectorExpr); selected && selector.Sel.Name == "documentPublications" {
					selector.Sel.Name = "b"
				}
				return true
			})
		}},
		{"HTTP chunk stub", filepath.Join("..", "adapters", "delivery", "http"), "http", dpc1HTTP, func(set wave42LegacyGitSourceSet) {
			set.methods["Server.pushChunks"].Body = dpc1Body(test, "return")
		}},
		{"HTTP same-type unbound owner", filepath.Join("..", "adapters", "delivery", "http"), "http", dpc1HTTP, func(set wave42LegacyGitSourceSet) {
			shape := set.types["Server"].Type.(*ast.StructType)
			shape.Fields.List = append(shape.Fields.List, &ast.Field{Names: []*ast.Ident{ast.NewIdent("otherPublications")}, Type: &ast.SelectorExpr{X: ast.NewIdent("inbound"), Sel: ast.NewIdent("DocumentPublicationCommands")}})
			ast.Inspect(set.methods["Server.pushObjects"].Body, func(node ast.Node) bool {
				if selector, selected := node.(*ast.SelectorExpr); selected && selector.Sel.Name == "documentPublications" {
					selector.Sel.Name = "otherPublications"
				}
				return true
			})
		}},
		{"HTTP injection setter discards owner", filepath.Join("..", "adapters", "delivery", "http"), "http", dpc1HTTP, func(set wave42LegacyGitSourceSet) {
			ast.Inspect(set.methods["Server.SetDocumentPublicationCommands"].Body, func(node ast.Node) bool {
				if assignment, assigned := node.(*ast.AssignStmt); assigned {
					for _, target := range assignment.Lhs {
						if selector, selected := target.(*ast.SelectorExpr); selected && selector.Sel.Name == "documentPublications" {
							selector.Sel.Name = "otherPublications"
						}
					}
				}
				return true
			})
		}},
		{"API missing setter", filepath.Join("..", "..", "cmd", "cxtd"), "binding", dpc1APIBinding, func(set wave42LegacyGitSourceSet) {
			for _, function := range dpc1Reachable(set, "serve") {
				ast.Inspect(function.Body, func(node ast.Node) bool {
					if selector, selected := node.(*ast.SelectorExpr); selected && selector.Sel.Name == "SetDocumentPublicationCommands" {
						selector.Sel.Name = "SetUnrelatedCommands"
					}
					return true
				})
			}
		}},
		{"runtime discarded result", filepath.Join("..", "serverruntime"), "runtime", dpc1Runtime, func(set wave42LegacyGitSourceSet) {
			set.methods["Runtime.ConfigureDocumentPublicationCommands"].Body = dpc1Body(test, "return nil")
		}},
		{"runtime Context-nil guard removed", filepath.Join("..", "serverruntime"), "runtime", dpc1Runtime, func(set wave42LegacyGitSourceSet) {
			ast.Inspect(set.methods["Runtime.ConfigureDocumentPublicationCommands"].Body, func(node ast.Node) bool {
				if conditional, condition := node.(*ast.IfStmt); condition {
					ast.Inspect(conditional.Cond, func(nested ast.Node) bool {
						if selector, selected := nested.(*ast.SelectorExpr); selected && selector.Sel.Name == "Context" {
							selector.Sel.Name = "UnrelatedContext"
						}
						return true
					})
				}
				return true
			})
		}},
		{"MCP indirectly configures publication owner", filepath.Join("..", "serverruntime"), "runtime", dpc1Runtime, func(set wave42LegacyGitSourceSet) {
			set.methods["Runtime.configureMCPReads"].Body = dpc1Body(test, "return runtime.ConfigureDocumentPublicationCommands()")
		}},
		{"MCP query-only profile disabled", filepath.Join("..", "serverruntime"), "runtime", dpc1Runtime, func(set wave42LegacyGitSourceSet) {
			ast.Inspect(set.methods["OpenMCP"].Body, func(node ast.Node) bool {
				if call, called := node.(*ast.CallExpr); called && wave42LegacyGitFunctionName(call.Fun) == "openRuntime" {
					call.Args[len(call.Args)-1] = ast.NewIdent("false")
				}
				return true
			})
		}},
		{"MCP accepts commands", filepath.Join("..", "mcpserver"), "mcp", dpc1MCP, func(set wave42LegacyGitSourceSet) {
			ast.Inspect(set.methods["HandlerWithReadPreparation"].Body, func(node ast.Node) bool {
				if selector, selected := node.(*ast.SelectorExpr); selected && selector.Sel.Name == "DocumentPublications" {
					selector.Sel.Name = "UnrelatedCommands"
				}
				return true
			})
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			set := wave48Package(test, scenario.directory)
			if findings := scenario.check(set); len(findings) != 0 {
				test.Fatalf("production model must pass before mutation: %+v", findings)
			}
			scenario.mutate(set)
			dpc1RequireRule(test, scenario.check(set), scenario.rule)
		})
	}
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
