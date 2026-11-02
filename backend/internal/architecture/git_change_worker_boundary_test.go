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

const wave42LegacyGitModule = "github.com/wnsdy95/cxthub/backend"

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

func wave42LegacyGitWorkerForbiddenImport(dependency string) bool {
	return dependency == wave42LegacyGitModule+"/internal/app" ||
		strings.HasPrefix(dependency, wave42LegacyGitModule+"/internal/adapters/") ||
		dependency == wave42LegacyGitModule+"/internal/adapters" ||
		dependency == wave42LegacyGitModule+"/internal/serverruntime" || strings.HasPrefix(dependency, wave42LegacyGitModule+"/internal/serverruntime/") ||
		dependency == wave42LegacyGitModule+"/internal/mcpserver" || strings.HasPrefix(dependency, wave42LegacyGitModule+"/internal/mcpserver/") ||
		dependency == "database/sql" || strings.HasPrefix(dependency, "database/sql/") ||
		dependency == "net/http" || strings.HasPrefix(dependency, "net/http/") ||
		dependency == "github.com/jackc/pgx" || strings.HasPrefix(dependency, "github.com/jackc/pgx/")
}

func wave42LegacyGitSourceViolations(path, source string) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
	if err != nil {
		return nil, err
	}
	clean := "/" + strings.TrimPrefix(filepath.ToSlash(filepath.Clean(path)), "/")
	leaf := strings.Contains(clean, "/internal/app/gitchangeworker/") || strings.Contains(clean, "/internal/app/evidencequery/")
	aliases := map[string]string{}
	var violations []string
	for _, imported := range file.Imports {
		dependency, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			return nil, err
		}
		alias := filepath.Base(dependency)
		if imported.Name != nil {
			alias = imported.Name.Name
		}
		aliases[alias] = dependency
		if leaf && wave42LegacyGitWorkerForbiddenImport(dependency) {
			violations = append(violations, "leaf imports command/infrastructure owner: "+dependency)
		}
	}
	normalize := func(expression ast.Expr) string {
		name := wave42LegacyGitType(expression)
		for alias, dependency := range aliases {
			if dependency == wave42LegacyGitModule+"/internal/ports/outbound" {
				name = strings.ReplaceAll(name, alias+".", "outbound.")
			}
			if dependency == wave42LegacyGitModule+"/internal/app/evidencequery" {
				name = strings.ReplaceAll(name, alias+".", "evidencequery.")
			}
			if dependency == wave42LegacyGitModule+"/internal/domain" {
				name = strings.ReplaceAll(name, alias+".", "domain.")
			}
			if dependency == wave42LegacyGitModule+"/internal/app/gitchangeworker" {
				name = strings.ReplaceAll(name, alias+".", "gitchangeworker.")
			}
		}
		return name
	}
	workerFields := map[string]bool{
		"Config": true, "Profile": true, "Scope": true, "RepositoryReader": true,
		"outbound.GitChangeStore": true, "outbound.GitEvidenceReader": true, "outbound.GitSourceAuthorizer": true,
		"evidencequery.GitDeltaReader": true, "evidencequery.ReversalReader": true,
	}
	workFields := map[string]bool{"outbound.MetadataStore": true, "outbound.RepositoryStore": true, "[]domain.DocumentIdentity": true,
		"*RepositoryAuthority": true, "outbound.RepositoryTransactions": true, "outbound.RepositoryRevisions": true,
		"outbound.EvidenceRevisions": true, "error": true}
	factoryFields := map[string]bool{"gitchangeworker.Profile": true, "outbound.MetadataStore": true, "outbound.BlobStore": true, "outbound.RepositoryStore": true, "outbound.GitEvidenceReader": true, "outbound.GitSourceAuthorizer": true}
	ast.Inspect(file, func(node ast.Node) bool {
		if strings.Contains(clean, "/internal/app/evidencequery/") {
			if call, called := node.(*ast.CallExpr); called && (wave42LegacyGitFunctionName(call.Fun) == "WithValue" || wave42LegacyGitFunctionName(call.Fun) == "WithGitRepository") {
				violations = append(violations, "pure evidence policy introduces context binding")
			}
		}
		if leaf {
			switch value := node.(type) {
			case *ast.Ident:
				if value.Name == "Service" || value.Name == "IdentityService" || value.Name == "NewService" || value.Name == "NewIdentityService" {
					violations = append(violations, "leaf retains a command service")
				}
			case *ast.SelectorExpr:
				if value.Sel.Name == "Service" || value.Sel.Name == "IdentityService" || value.Sel.Name == "NewService" || value.Sel.Name == "NewIdentityService" {
					violations = append(violations, "leaf retains a qualified command service")
				}
			}
		}
		declaration, typed := node.(*ast.TypeSpec)
		if !typed {
			return true
		}
		worker := strings.Contains(clean, "/internal/app/gitchangeworker/") && (declaration.Name.Name == "Worker" || declaration.Name.Name == "Config")
		work := declaration.Name.Name == "repositoryWork"
		factory := declaration.Name.Name == "GitChangeWorkerConfig"
		if !worker && !work && !factory {
			return true
		}
		shape, concrete := declaration.Type.(*ast.StructType)
		if !concrete {
			violations = append(violations, "worker/work owner is not concrete")
			return true
		}
		allowed := workerFields
		if work {
			allowed = workFields
		}
		if factory {
			allowed = factoryFields
		}
		for _, field := range shape.Fields.List {
			if !allowed[normalize(field.Type)] {
				violations = append(violations, "worker/work field hides callback, resource state or broad owner: "+normalize(field.Type))
			}
		}
		return true
	})
	return violations, nil
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

func wave42LegacyGitRequireCalls(set wave42LegacyGitSourceSet, method string, required ...string) []string {
	declaration := set.methods[method]
	if declaration == nil || declaration.Body == nil {
		return []string{"missing actual implementation: " + method}
	}
	calls := map[string]bool{}
	for _, owned := range wave42LegacyGitOwnedMethods(set, method) {
		for called := range wave42LegacyGitCalls(owned.Body) {
			calls[called] = true
		}
	}
	var violations []string
	for _, operation := range required {
		if !calls[operation] {
			violations = append(violations, method+" does not own "+operation)
		}
	}
	return violations
}

func wave42LegacyGitOwnedMethods(set wave42LegacyGitSourceSet, entry string) []*ast.FuncDecl {
	var owned []*ast.FuncDecl
	seen := map[string]bool{}
	pending := []string{entry}
	for len(pending) != 0 {
		name := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		declaration := set.methods[name]
		if seen[name] || declaration == nil || declaration.Body == nil {
			continue
		}
		seen[name] = true
		owned = append(owned, declaration)
		ast.Inspect(declaration.Body, func(node ast.Node) bool {
			call, called := node.(*ast.CallExpr)
			if !called {
				return true
			}
			operation := call.Fun
			if indexed, generic := operation.(*ast.IndexExpr); generic {
				operation = indexed.X
			}
			if indexed, generic := operation.(*ast.IndexListExpr); generic {
				operation = indexed.X
			}
			if plain, local := operation.(*ast.Ident); local {
				pending = append(pending, plain.Name)
			}
			if selected, method := operation.(*ast.SelectorExpr); method && declaration.Recv != nil && len(declaration.Recv.List[0].Names) == 1 {
				receiver, local := selected.X.(*ast.Ident)
				if local && receiver.Name == declaration.Recv.List[0].Names[0].Name {
					owner := strings.TrimPrefix(wave42LegacyGitType(declaration.Recv.List[0].Type), "*")
					pending = append(pending, owner+"."+selected.Sel.Name)
				}
			}
			return true
		})
	}
	return owned
}

func wave42LegacyGitAlgorithms(set wave42LegacyGitSourceSet) []string {
	violations := wave42LegacyGitRequireCalls(set, "Worker.Process", "WorkerContext", "WithinWork", "ClaimGitChange", "RunLease")
	violations = append(violations, wave42LegacyGitRequireCalls(set, "Worker.Run", "Process", "NewTicker")...)
	violations = append(violations, wave42LegacyGitRequireCalls(set, "Worker.RunLease", "WithTimeout", "WithoutCancel", "ReadRepository", "VerifyReversal", "WithinEvidenceWrite", "FinishGitChange", "Join")...)
	return violations
}

func wave42LegacyGitRootViolations(set wave42LegacyGitSourceSet) []string {
	var violations []string
	for _, method := range []string{"newRepositoryWork", "repositoryWork.authorizeRepositoryWrite", "repositoryWork.checkDocumentIdentity", "repositoryWork.revisionWrite", "repositoryWork.auditRepositoryWrite", "repositoryWriteWithWork", "repositoryReadWithWork", "repositoryWriteErrorWithWork", "RepositoryAuthority.AuthorizeWrite", "RepositoryAuthority.AuthorizeSecrets", "RepositoryAuthority.loadAccess", "RepositoryAuthority.AuditWrite", "repositoryRoleSources.roleFor", "appendRepositoryAuditFromSources", "NewGitChangeWorker", "newGitChangeWorker"} {
		declaration := set.methods[method]
		if declaration == nil || declaration.Body == nil {
			violations = append(violations, "missing Service-free work implementation: "+method)
			continue
		}
		ast.Inspect(declaration, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.Ident:
				if value.Name == "Service" || value.Name == "IdentityService" || value.Name == "NewService" || value.Name == "NewIdentityService" {
					violations = append(violations, method+" retains/reconstructs a command service")
				}
			case *ast.SelectorExpr:
				if value.Sel.Name == "Service" || value.Sel.Name == "IdentityService" || value.Sel.Name == "NewService" || value.Sel.Name == "NewIdentityService" {
					violations = append(violations, method+" retains/reconstructs a qualified command service")
				}
			case *ast.GoStmt:
				violations = append(violations, method+" starts hidden background work")
			}
			return true
		})
	}
	for method, operations := range map[string][]string{
		"repositoryWriteWithWork":                 {"authorizeRepositoryWrite", "checkDocumentIdentity", "WithinRepository", "revisionWrite", "auditRepositoryWrite"},
		"repositoryReadWithWork":                  {"WithinReadSnapshot"},
		"repositoryWriteErrorWithWork":            {"repositoryWriteWithWork"},
		"repositoryWork.authorizeRepositoryWrite": {"AuthorizeWrite"},
		"RepositoryAuthority.AuthorizeWrite":      {"RepositoryActor", "loadAccess", "CanWriteRepositoryContext"},
		"RepositoryAuthority.AuthorizeSecrets":    {"loadAccess", "CanEditSecrets"},
		"RepositoryAuthority.loadAccess":          {"GetRepo", "LockRepositoryAccess", "GetRepository", "roleFor"},
		"repositoryRoleSources.roleFor":           {"ListMembers", "RepositoryTeamAccess", "RepositoryOrganizationAccess", "EffectiveRepositoryRole"},
		"repositoryWork.revisionWrite":            {"AdvanceEvidenceRevision", "AdvanceRepositoryRevision"},
		"repositoryWork.auditRepositoryWrite":     {"AuditWrite"},
		"RepositoryAuthority.AuditWrite":          {"RepositoryActor", "GetRepo", "appendRepositoryAuditFromSources"},
		"appendRepositoryAuditFromSources":        {"GetRepository", "GetNamespace", "AppendOrganizationAudit", "organizationAudit"},
		"repositoryWrite":                         {"repositoryWriteWithWork"},
		"repositoryRead":                          {"repositoryReadWithWork"},
		"repositoryWriteError":                    {"repositoryWriteErrorWithWork"},
		"revisionWrite":                           {"revisionWrite"},
		"Service.authorizeRepositoryWrite":        {"authorizeRepositoryWrite"},
		"Service.checkDocumentIdentity":           {"checkRepositoryDocumentIdentity"},
		"repositoryWork.checkDocumentIdentity":    {"checkRepositoryDocumentIdentity"},
		"Service.auditRepositoryWrite":            {"auditRepositoryWrite"},
		"verifyGitReversal":                       {"VerifyReversal"},
	} {
		violations = append(violations, wave42LegacyGitRequireCalls(set, method, operations...)...)
	}
	for _, method := range []string{"GitChanges.Process", "GitChanges.run", "GitChanges.Run", "verifyGitReversal", "cachedGitEvidence.ReadCommitDelta", "cachedGitEvidence.IsGitAncestor", "repositoryWrite", "repositoryRead", "repositoryWriteError", "revisionWrite", "Service.authorizeRepositoryWrite", "Service.checkDocumentIdentity", "Service.auditRepositoryWrite"} {
		declaration := set.methods[method]
		if declaration == nil {
			violations = append(violations, "missing compatibility facade: "+method)
			continue
		}
		ast.Inspect(declaration.Body, func(node ast.Node) bool {
			switch node.(type) {
			case *ast.ForStmt, *ast.RangeStmt, *ast.MapType, *ast.GoStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt:
				violations = append(violations, method+" retains worker/policy algorithm")
			}
			return true
		})
	}
	for alias, target := range map[string]string{"GitReadFence": "outbound.GitReadFence", "GitSourceAuthorizer": "outbound.GitSourceAuthorizer"} {
		declaration := set.types[alias]
		if declaration == nil || !declaration.Assign.IsValid() || wave42LegacyGitType(declaration.Type) != target {
			violations = append(violations, "source authorization compatibility is not an alias: "+alias)
		}
	}
	return violations
}

func wave42LegacyGitCapturedJobsViolations(set wave42LegacyGitSourceSet) []string {
	var violations []string
	legacy := set.methods["GitChanges.worker"]
	if legacy == nil || legacy.Recv == nil || len(legacy.Recv.List[0].Names) != 1 {
		return []string{"missing legacy worker assembly with captured job store"}
	}
	receiver := legacy.Recv.List[0].Names[0].Name
	captured := false
	ast.Inspect(legacy.Body, func(node ast.Node) bool {
		call, called := node.(*ast.CallExpr)
		if !called || wave42LegacyGitFunctionName(call.Fun) != "newGitChangeWorker" || len(call.Args) != 2 {
			return true
		}
		if original, selector := call.Args[1].(*ast.SelectorExpr); selector && wave42LegacyGitType(original.X) == receiver && original.Sel.Name == "store" {
			captured = true
		}
		return true
	})
	if !captured {
		violations = append(violations, "legacy assembly rediscovers jobs instead of injecting changes.store")
	}
	public := set.methods["NewGitChangeWorker"]
	discovered := false
	if public != nil {
		ast.Inspect(public.Body, func(node ast.Node) bool {
			asserted, capability := node.(*ast.TypeAssertExpr)
			if capability && wave42LegacyGitType(asserted.Type) == "outbound.GitChangeStore" && strings.HasSuffix(wave42LegacyGitType(asserted.X), ".Metadata") {
				discovered = true
			}
			return true
		})
	}
	if !discovered {
		violations = append(violations, "public assembly does not explicitly declare its metadata GitChangeStore")
	}
	for _, method := range []string{"GitChanges.Process", "GitChanges.run", "GitChanges.Run"} {
		violations = append(violations, wave42LegacyGitRequireCalls(set, method, "worker")...)
	}
	return violations
}

func wave42LegacyGitScopeViolations(set wave42LegacyGitSourceSet) []string {
	var violations []string
	scopes := 0
	for name, declaration := range set.methods {
		if !strings.HasSuffix(name, ".WithinEvidenceWrite") || declaration.Recv == nil {
			continue
		}
		scopes++
		owner := strings.TrimSuffix(name, ".WithinEvidenceWrite")
		ownerType := set.types[owner]
		if ownerType == nil {
			violations = append(violations, "evidence publication scope has no concrete owner")
			continue
		}
		shape, concrete := ownerType.Type.(*ast.StructType)
		if !concrete {
			violations = append(violations, "evidence publication scope is an alias/interface")
			continue
		}
		workFields := map[string]bool{}
		for _, field := range shape.Fields.List {
			kind := wave42LegacyGitType(field.Type)
			if kind == "*repositoryWork" {
				for _, identifier := range field.Names {
					workFields[identifier.Name] = true
				}
				continue
			}
			if kind != "outbound.MetadataStore" && kind != "[]domain.DocumentIdentity" {
				violations = append(violations, "publication scope hides callback/broad/resource owner: "+kind)
			}
		}
		if len(workFields) != 1 {
			violations = append(violations, "publication scope must share exactly one repositoryWork")
		}
		for method, required := range map[string][]string{
			"WithinWork":          {"admittedWork"},
			"WithinEvidenceWrite": {"repositoryWriteErrorWithWork", "evidenceWriteContext"},
		} {
			violations = append(violations, wave42LegacyGitRequireCalls(set, owner+"."+method, required...)...)
		}
		shared := false
		ast.Inspect(declaration.Body, func(node ast.Node) bool {
			call, called := node.(*ast.CallExpr)
			if !called || wave42LegacyGitFunctionName(call.Fun) != "repositoryWriteErrorWithWork" {
				return true
			}
			for _, argument := range call.Args {
				selected, selector := argument.(*ast.SelectorExpr)
				if !selector || !workFields[selected.Sel.Name] {
					continue
				}
				receiver, named := selected.X.(*ast.Ident)
				if named && len(declaration.Recv.List[0].Names) == 1 && receiver.Name == declaration.Recv.List[0].Names[0].Name {
					shared = true
				}
			}
			return true
		})
		if !shared {
			violations = append(violations, "publication does not pass the scope's actual repositoryWork to the original write gate")
		}
		bound := false
		for _, factory := range wave42LegacyGitOwnedMethods(set, "NewGitChangeWorker") {
			if wave42LegacyGitMentions(factory, owner) {
				bound = true
			}
		}
		if !bound {
			violations = append(violations, "factory does not bind the checked concrete worker scope")
		}
	}
	if scopes != 1 {
		violations = append(violations, "expected one actual Git change publication scope")
	}
	return violations
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

func TestWave42LegacyGitChangeActualWorkerAndSharedWorkBoundaries(test *testing.T) {
	rootSources := wave42LegacyGitReadSources(test, filepath.Join("..", "app"))
	workerSources := wave42LegacyGitReadSources(test, filepath.Join("..", "app", "gitchangeworker"))
	evidenceSources := wave42LegacyGitReadSources(test, filepath.Join("..", "app", "evidencequery"))
	for _, sources := range []map[string]string{workerSources, evidenceSources} {
		for path, source := range sources {
			violations, err := wave42LegacyGitSourceViolations(filepath.Join("internal", "app", filepath.Base(filepath.Dir(path)), filepath.Base(path)), source)
			if err != nil || len(violations) != 0 {
				test.Error(path, violations, err)
			}
		}
	}
	for path, source := range rootSources {
		violations, err := wave42LegacyGitSourceViolations(path, source)
		if err != nil || len(violations) != 0 {
			test.Error(path, violations, err)
		}
	}
	root, err := wave42LegacyGitParseSources(rootSources)
	if err != nil {
		test.Fatal(err)
	}
	worker, err := wave42LegacyGitParseSources(workerSources)
	if err != nil {
		test.Fatal(err)
	}
	evidence, err := wave42LegacyGitParseSources(evidenceSources)
	if err != nil {
		test.Fatal(err)
	}
	violations := wave42LegacyGitAlgorithms(worker)
	violations = append(violations, wave42LegacyGitRootViolations(root)...)
	violations = append(violations, wave42LegacyGitScopeViolations(root)...)
	violations = append(violations, wave42LegacyGitCapturedJobsViolations(root)...)
	violations = append(violations, wave42LegacyGitRequireCalls(evidence, "VerifyReversal", "ReadCommitDelta", "IsGitAncestor", "AssessGitReversal")...)
	violations = append(violations, wave42LegacyGitRequireCalls(evidence, "CachedReversalReader.ReadCommitDelta", "GetGitDelta", "ReadCommitDelta")...)
	violations = append(violations, wave42LegacyGitRequireCalls(evidence, "CachedReversalReader.IsGitAncestor", "GetGitDelta", "IsGitAncestor")...)
	for _, violation := range violations {
		test.Error(violation)
	}
}

func TestWave42LegacyGitWorkerGuardRejectsForbiddenSourcePathsIndependently(test *testing.T) {
	for _, dependency := range []string{wave42LegacyGitModule + "/internal/app", wave42LegacyGitModule + "/internal/adapters/store", wave42LegacyGitModule + "/internal/serverruntime", wave42LegacyGitModule + "/internal/mcpserver", "net/http", "net/http/httptest", "database/sql", "database/sql/driver", "github.com/jackc/pgx/v5", "github.com/jackc/pgx/v5/pgxpool"} {
		for _, directory := range []string{"gitchangeworker", "evidencequery"} {
			path := filepath.Join("fixture", "internal", "app", directory, "renamed_owner.go")
			violations, err := wave42LegacyGitSourceViolations(path, "package owner\nimport hidden "+strconv.Quote(dependency))
			if err != nil || len(violations) == 0 {
				test.Fatal("independent source guard missed forbidden dependency", path, dependency, violations, err)
			}
		}
	}
	for _, fixture := range []struct{ path, source string }{
		{"internal/app/gitchangeworker/worker.go", `type Worker struct { run func() error }`},
		{"internal/app/gitchangeworker/worker.go", `type Callback interface { Call() error }; type Worker struct { runner Callback }`},
		{"internal/app/gitchangeworker/worker.go", `type Config struct { service *IdentityService }`},
		{"internal/app/gitchangeworker/worker.go", `type privateService interface { Process() error }; type Worker struct { runner privateService }`},
		{"internal/app/gitchangeworker/worker.go", `type Worker struct { queue chan int }`},
		{"internal/app/repository_work.go", `type repositoryWork struct { service *Service }`},
		{"internal/app/repository_work.go", `type repositoryWork struct { Call struct { Callback func() error } }`},
		{"internal/app/repository_work.go", `type repositoryWork = Service`},
		{"internal/app/git_workers.go", `type GitChangeWorkerConfig struct { privateService interface { Run() } }`},
		{"internal/app/evidencequery/reversal.go", `func read(ctx Context) { context.WithValue(ctx, repositoryKey{}, "repo") }`},
	} {
		violations, err := wave42LegacyGitSourceViolations(fixture.path, "package owner\n"+fixture.source)
		if err != nil || len(violations) == 0 {
			test.Fatal("guard accepted fake extraction", fixture.path, violations, err)
		}
	}
	allowed := "package gitchangeworker\nimport wire " + strconv.Quote(wave42LegacyGitModule+"/internal/ports/outbound") + "\ntype Worker struct { jobs wire.GitChangeStore; scope Scope }"
	if violations, err := wave42LegacyGitSourceViolations("internal/app/gitchangeworker/worker.go", allowed); err != nil || len(violations) != 0 {
		test.Fatal("guard rejected narrow aliased ports", violations, err)
	}
}

func TestWave42LegacyGitWorkerGuardRejectsCallbackBodiesAndDisconnectedPublication(test *testing.T) {
	fakeWorker, err := wave42LegacyGitParseSources(map[string]string{"worker.go": `package gitchangeworker
type Worker struct{}
func (worker *Worker) Process(ctx Context, limit int) error { return worker.Call.Callback(ctx, limit) }
func (worker *Worker) RunLease(ctx Context, job Job) error { return worker.Call.Callback(ctx, job) }
func (worker *Worker) Run(ctx Context) { worker.Call.Callback(ctx) }
`})
	if err != nil || len(wave42LegacyGitAlgorithms(fakeWorker)) == 0 {
		test.Fatal("guard accepted worker body delegated back to Service", err)
	}
	for _, publish := range []string{
		`return scope.Call.Callback(ctx, repo, write)`,
		`return repositoryWriteErrorWithWork(evidenceWriteContext(ctx), newRepositoryWork(metadata, repositories, supported), repo, write)`,
	} {
		source := `package app
type repositoryWork struct{}
type workerScope struct { work *repositoryWork }
func NewGitChangeWorker(config Config) { workerScope{} }
func (scope *workerScope) WithinWork(ctx Context, run Work) error { return admittedWork(ctx, run) }
func (scope *workerScope) WithinEvidenceWrite(ctx Context, repo Hash, write Work) error { ` + publish + ` }
`
		set, err := wave42LegacyGitParseSources(map[string]string{"scope.go": source})
		if err != nil || len(wave42LegacyGitScopeViolations(set)) == 0 {
			test.Fatal("guard accepted callback or disconnected publication owner", publish, err)
		}
	}
	valid := `package app
type repositoryWork struct{}
type workerScope struct { work *repositoryWork }
func NewGitChangeWorker(config Config) { workerScope{} }
func (scope *workerScope) WithinWork(ctx Context, run Work) error { return admittedWork(ctx, run) }
func (scope *workerScope) WithinEvidenceWrite(ctx Context, repo Hash, write Work) error { return repositoryWriteErrorWithWork(evidenceWriteContext(ctx), scope.work, repo, write) }
`
	set, err := wave42LegacyGitParseSources(map[string]string{"scope.go": valid})
	if err != nil || len(wave42LegacyGitScopeViolations(set)) != 0 {
		test.Fatal("guard rejected original shared publication/admission", fmt.Sprint(wave42LegacyGitScopeViolations(set)), err)
	}
}

func TestWave42LegacyGitWorkerGuardRejectsRediscoveredLegacyJobs(test *testing.T) {
	for _, original := range []bool{false, true} {
		jobs := "config.Metadata.(outbound.GitChangeStore)"
		if original {
			jobs = "changes.store"
		}
		source := `package app
func NewGitChangeWorker(config Config) { jobs := config.Metadata.(outbound.GitChangeStore); newGitChangeWorker(config, jobs) }
func (changes *GitChanges) worker() { return newGitChangeWorker(config, ` + jobs + `) }
func (changes *GitChanges) Process() { changes.worker().Process() }
func (changes *GitChanges) run() { changes.worker().RunLease() }
func (changes *GitChanges) Run() { changes.worker().Run() }
`
		set, err := wave42LegacyGitParseSources(map[string]string{"assembly.go": source})
		if err != nil {
			test.Fatal(err)
		}
		violations := wave42LegacyGitCapturedJobsViolations(set)
		if original && len(violations) != 0 || !original && len(violations) == 0 {
			test.Fatal("captured original jobs guard disagrees with legacy contract", original, violations)
		}
	}
}
