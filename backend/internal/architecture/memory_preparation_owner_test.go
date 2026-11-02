package architecture

import (
	"go/ast"
	"path/filepath"
	"strings"
	"testing"
)

func wave44PreparationNamedType(expression ast.Expr) string {
	return strings.TrimPrefix(wave42LegacyGitType(expression), "*")
}

func wave44PreparationMember(set wave42LegacyGitSourceSet, owner, name string, method bool, seen map[string]bool) string {
	if owner == "" || seen[owner] {
		return ""
	}
	seen[owner] = true
	if method && set.methods[owner+"."+name] != nil {
		return owner + "." + name
	}
	declaration := set.types[owner]
	if declaration == nil {
		return ""
	}
	shape, concrete := declaration.Type.(*ast.StructType)
	if !concrete {
		return wave44PreparationMember(set, wave44PreparationNamedType(declaration.Type), name, method, seen)
	}
	for _, field := range shape.Fields.List {
		for _, identifier := range field.Names {
			if !method && identifier.Name == name {
				return wave44PreparationNamedType(field.Type)
			}
		}
		if len(field.Names) == 0 {
			embedded := wave44PreparationNamedType(field.Type)
			if !method && embedded == name {
				return embedded
			}
			if selected := wave44PreparationMember(set, embedded, name, method, seen); selected != "" {
				return selected
			}
		}
	}
	return ""
}

func wave44PreparationExpressionType(set wave42LegacyGitSourceSet, expression ast.Expr, bindings map[string]string) string {
	switch value := expression.(type) {
	case *ast.Ident:
		if bound := bindings[value.Name]; bound != "" {
			return bound
		}
		if set.types[value.Name] != nil {
			return value.Name
		}
	case *ast.UnaryExpr:
		return wave44PreparationExpressionType(set, value.X, bindings)
	case *ast.StarExpr:
		return wave44PreparationExpressionType(set, value.X, bindings)
	case *ast.ParenExpr:
		return wave44PreparationExpressionType(set, value.X, bindings)
	case *ast.CompositeLit:
		return wave44PreparationNamedType(value.Type)
	case *ast.TypeAssertExpr:
		return wave44PreparationNamedType(value.Type)
	case *ast.SelectorExpr:
		owner := wave44PreparationExpressionType(set, value.X, bindings)
		return wave44PreparationMember(set, owner, value.Sel.Name, false, map[string]bool{})
	case *ast.CallExpr:
		key := wave44PreparationCall(set, value.Fun, bindings)
		if function := set.methods[key]; function != nil && function.Type.Results != nil && len(function.Type.Results.List) > 0 {
			return wave44PreparationNamedType(function.Type.Results.List[0].Type)
		}
		if len(value.Args) == 1 && wave42LegacyGitFunctionName(value.Fun) == "new" {
			return wave44PreparationNamedType(value.Args[0])
		}
	}
	return ""
}

func wave44PreparationCall(set wave42LegacyGitSourceSet, expression ast.Expr, bindings map[string]string) string {
	switch value := expression.(type) {
	case *ast.Ident:
		if bindings[value.Name] == "" && set.methods[value.Name] != nil {
			return value.Name
		}
	case *ast.SelectorExpr:
		owner := wave44PreparationExpressionType(set, value.X, bindings)
		return wave44PreparationMember(set, owner, value.Sel.Name, true, map[string]bool{})
	case *ast.IndexExpr:
		return wave44PreparationCall(set, value.X, bindings)
	case *ast.IndexListExpr:
		return wave44PreparationCall(set, value.X, bindings)
	case *ast.ParenExpr:
		return wave44PreparationCall(set, value.X, bindings)
	}
	return ""
}

func wave44PreparationReachable(set wave42LegacyGitSourceSet, entry string) map[string]*ast.FuncDecl {
	visited := map[string]*ast.FuncDecl{}
	var visit func(string)
	visit = func(key string) {
		function := set.methods[key]
		if function == nil || visited[key] != nil {
			return
		}
		visited[key] = function
		bindings := map[string]string{}
		for _, list := range []*ast.FieldList{function.Recv, function.Type.Params} {
			if list != nil {
				for _, field := range list.List {
					for _, name := range field.Names {
						bindings[name.Name] = wave44PreparationNamedType(field.Type)
					}
				}
			}
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.AssignStmt:
				for index, left := range value.Lhs {
					if name, named := left.(*ast.Ident); named && index < len(value.Rhs) {
						if kind := wave44PreparationExpressionType(set, value.Rhs[index], bindings); kind != "" {
							bindings[name.Name] = kind
						}
					}
				}
			case *ast.ValueSpec:
				for index, name := range value.Names {
					kind := wave44PreparationNamedType(value.Type)
					if kind == "" && index < len(value.Values) {
						kind = wave44PreparationExpressionType(set, value.Values[index], bindings)
					}
					bindings[name.Name] = kind
				}
			case *ast.CallExpr:
				visit(wave44PreparationCall(set, value.Fun, bindings))
			}
			return true
		})
	}
	visit(entry)
	return visited
}

func wave44PreparationOwnerViolations(set wave42LegacyGitSourceSet) []string {
	var violations []string
	for owner, fields := range map[string]map[string]string{
		"MemoryPreparationWorkers": {"": "*MemoryPreparationResources", "memoryReadRepos": "[]domain.ContentHash"},
		"MemoryPreparationResources": {"meta": "outbound.MetadataStore", "blobs": "outbound.BlobStore", "work": "*repositoryWork",
			"scope": "*memorySnapshotScope", "delivery": "*memoryquery.DeliveryCache", "projectionMemory": "outbound.MemoryQueryReader",
			"projectionEvidence": "outbound.GraphEvidenceReader", "projectionMemorySource": "*domain.RepositoryMemorySourceGeneration"},
	} {
		declaration := set.types[owner]
		if declaration == nil {
			violations = append(violations, "missing actual owner: "+owner)
			continue
		}
		shape, concrete := declaration.Type.(*ast.StructType)
		if !concrete || declaration.Assign.IsValid() {
			violations = append(violations, "owner is an alias or callback interface: "+owner)
			continue
		}
		observed := map[string]bool{}
		for _, field := range shape.Fields.List {
			names := field.Names
			if len(names) == 0 {
				names = []*ast.Ident{{Name: ""}}
			}
			for _, name := range names {
				if expected, allowed := fields[name.Name]; !allowed || wave42LegacyGitType(field.Type) != expected {
					violations = append(violations, "broad/private/callback dependency: "+owner+"."+name.Name)
				}
				observed[name.Name] = true
			}
		}
		for name := range fields {
			if !observed[name] {
				violations = append(violations, "missing actual resource dependency: "+owner+"."+name)
			}
		}
	}
	constructors := map[string]bool{"NewMemoryPreparationResources": true, "NewMemoryPreparationWorkers": true}
	for constructor := range constructors {
		if set.methods[constructor] == nil {
			violations = append(violations, "missing direct public factory: "+constructor)
		}
	}
	if set.methods["Service.ConfigureMemoryReadPreparations"] == nil {
		violations = append(violations, "explicit preparation configuration no longer remains on Service")
	}
	if set.methods["MemoryPreparationWorkers.ConfigureMemoryReadPreparations"] != nil {
		violations = append(violations, "explicit preparation configuration moved onto the execution owner")
	}
	for _, entry := range []string{"NewMemoryPreparationResources", "NewMemoryPreparationWorkers", "MemoryPreparationWorkers.Run", "MemoryPreparationWorkers.RunMemoryReadPreparationWorker"} {
		if set.methods[entry] == nil {
			violations = append(violations, "missing constructor or execution entry point: "+entry)
		}
		for key := range wave44PreparationReachable(set, entry) {
			if key == "Service.ConfigureMemoryReadPreparations" || key == "MemoryPreparationWorkers.ConfigureMemoryReadPreparations" {
				violations = append(violations, "constructor or Run executes explicit configuration: "+entry)
			}
		}
	}
	for root := range set.methods {
		if !constructors[root] && !strings.HasPrefix(root, "MemoryPreparationWorkers.") && !strings.HasPrefix(root, "MemoryPreparationResources.") {
			continue
		}
		for key, function := range wave44PreparationReachable(set, root) {
			if strings.HasPrefix(key, "Service.") || strings.HasPrefix(key, "IdentityService.") {
				violations = append(violations, "actual owner calls a legacy method: "+key)
			}
			ast.Inspect(function, func(node ast.Node) bool {
				switch value := node.(type) {
				case *ast.Ident:
					if value.Name == "Service" || value.Name == "IdentityService" || value.Name == "NewService" || value.Name == "NewIdentityService" {
						violations = append(violations, "reachable broad/private reverse dependency: "+key)
					}
				case *ast.GoStmt:
					if constructors[root] {
						violations = append(violations, "factory starts hidden work through: "+key)
					}
				}
				return true
			})
			if constructors[root] && strings.HasPrefix(key, "MemoryPreparationWorkers.") {
				method := function.Name.Name
				if strings.HasPrefix(method, "Run") || strings.HasPrefix(method, "Process") || method == "ConfigureMemoryReadPreparations" {
					violations = append(violations, "factory executes worker/configuration command: "+key)
				}
			}
		}
	}
	return violations
}

func TestWave44MemoryPreparationActualOwnerHasNoReverseDependencyOrAutostart(test *testing.T) {
	set, err := wave42LegacyGitParseSources(wave42LegacyGitReadSources(test, filepath.Join("..", "app")))
	if err != nil {
		test.Fatal(err)
	}
	for _, violation := range wave44PreparationOwnerViolations(set) {
		test.Error(violation)
	}
	for _, method := range []string{"ProcessMemoryReadPreparations", "processMemoryReadPreparations", "runMemoryReadPreparation",
		"prepareMemoryReadWork", "RunMemoryReadPreparationWorker", "CaptureEffectiveMemorySource", "captureEffectiveMemorySource", "ValidateCapturedMemorySource",
		"PrepareMemoryRead", "PublishPreparedMemoryRead", "persistPreparedMemoryRead", "PrepareIncrementalMemoryRead", "prepareIncrementalMemoryRead", "findMemoryGraphAnchor", "memoryReadReference"} {
		if set.methods["MemoryPreparationWorkers."+method] == nil {
			test.Error("original preparation method is not on the actual owner", method)
		}
	}
}

func TestWave44MemoryPreparationGuardTracesExactReceiverAndFreeFunctionScope(test *testing.T) {
	const valid = `package app
type MemoryPreparationWorkers struct { *MemoryPreparationResources; memoryReadRepos []domain.ContentHash }
type MemoryPreparationResources struct {
 meta outbound.MetadataStore; blobs outbound.BlobStore; work *repositoryWork
 scope *memorySnapshotScope; delivery *memoryquery.DeliveryCache
 projectionMemory outbound.MemoryQueryReader; projectionEvidence outbound.GraphEvidenceReader
 projectionMemorySource *domain.RepositoryMemorySourceGeneration
}
func NewMemoryPreparationResources() {}
func NewMemoryPreparationWorkers() { validateSelection() }
func validateSelection() {}
func (service *Service) ConfigureMemoryReadPreparations() {}
func (workers *MemoryPreparationWorkers) Run() { workers.RunMemoryReadPreparationWorker() }
func (workers *MemoryPreparationWorkers) RunMemoryReadPreparationWorker() {}
func (workers *MemoryPreparationWorkers) Process() { alias := workers; alias.run() }
func (workers *MemoryPreparationWorkers) run() { go boundedJob() }
func (service *Service) run() { NewService() }
func run() { NewIdentityService() }
func unrelated() { go NewService() }
`
	parse := func(source string) wave42LegacyGitSourceSet {
		test.Helper()
		set, err := wave42LegacyGitParseSources(map[string]string{"renamed.go": source})
		if err != nil {
			test.Fatal(err)
		}
		return set
	}
	set := parse(valid)
	owned := wave44PreparationReachable(set, "MemoryPreparationWorkers.Process")
	if owned["MemoryPreparationWorkers.run"] == nil || owned["Service.run"] != nil || owned["run"] != nil || len(wave44PreparationOwnerViolations(set)) != 0 {
		test.Fatal("guard confused exact receiver/free scope or banned a bounded job goroutine")
	}
	for _, invalid := range []string{
		strings.Replace(valid, "alias.run()", "run()", 1),
		strings.Replace(valid, "alias.run()", "hidden()", 1) + "\nfunc hidden() { NewService() }",
		strings.Replace(valid, "alias.run()", "service := &Service{}; service.run()", 1),
		strings.Replace(valid, "func validateSelection() {}", "func validateSelection() { go start() }", 1),
		strings.Replace(valid, "validateSelection() }", "workers := &MemoryPreparationWorkers{}; workers.Process() }", 1),
		strings.Replace(valid, "func NewMemoryPreparationResources() {}", "func NewMemoryPreparationResources() { NewIdentityService() }", 1),
		strings.Replace(valid, "*MemoryPreparationResources;", "*Service;", 1),
		strings.Replace(valid, "func (service *Service) ConfigureMemoryReadPreparations() {}", "", 1),
		strings.Replace(valid, "func (service *Service) ConfigureMemoryReadPreparations() {}", "func (workers *MemoryPreparationWorkers) ConfigureMemoryReadPreparations() {}", 1),
		strings.Replace(valid, "validateSelection() }", "configure() }", 1) + "\nfunc configure() { service := &Service{}; service.ConfigureMemoryReadPreparations() }",
		strings.Replace(valid, "func (workers *MemoryPreparationWorkers) RunMemoryReadPreparationWorker() {}", "func (workers *MemoryPreparationWorkers) RunMemoryReadPreparationWorker() { service := &Service{}; service.ConfigureMemoryReadPreparations() }", 1),
	} {
		if len(wave44PreparationOwnerViolations(parse(invalid))) == 0 {
			test.Fatal("guard missed a reachable reverse call or factory autostart")
		}
	}
}
