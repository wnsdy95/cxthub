package architecture

import (
	"go/ast"
	"path/filepath"
	"strings"
	"testing"
)

type wave45PRCallVisitor struct {
	set       wave42LegacyGitSourceSet
	bindings  map[string]string
	functions map[string]string
	visit     func(string)
}

func (visitor wave45PRCallVisitor) call(expression ast.Expr) string {
	if identifier, named := expression.(*ast.Ident); named {
		if target := visitor.functions[identifier.Name]; target != "" {
			return target
		}
	}
	return wave44PreparationCall(visitor.set, expression, visitor.bindings)
}

func (visitor wave45PRCallVisitor) bind(name string, expression ast.Expr, kind string) {
	target := visitor.call(expression)
	if kind == "" {
		kind = wave44PreparationExpressionType(visitor.set, expression, visitor.bindings)
	}
	if kind == "" {
		kind = "?"
	}
	visitor.bindings[name] = kind
	delete(visitor.functions, name)
	if target != "" {
		visitor.functions[name] = target
	}
}

func (visitor wave45PRCallVisitor) Visit(node ast.Node) ast.Visitor {
	if node == nil {
		return nil
	}
	switch value := node.(type) {
	case *ast.BlockStmt, *ast.FuncLit:
		bindings, functions := map[string]string{}, map[string]string{}
		for name, kind := range visitor.bindings {
			bindings[name] = kind
		}
		for name, target := range visitor.functions {
			functions[name] = target
		}
		visitor.bindings, visitor.functions = bindings, functions
		if literal, ok := node.(*ast.FuncLit); ok {
			for _, fields := range []*ast.FieldList{literal.Type.Params, literal.Type.Results} {
				if fields != nil {
					for _, field := range fields.List {
						for _, name := range field.Names {
							visitor.bind(name.Name, nil, wave44PreparationNamedType(field.Type))
						}
					}
				}
			}
		}
	case *ast.AssignStmt:
		for index, left := range value.Lhs {
			if name, ok := left.(*ast.Ident); ok && index < len(value.Rhs) {
				visitor.bind(name.Name, value.Rhs[index], "")
			}
		}
	case *ast.ValueSpec:
		for index, name := range value.Names {
			var expression ast.Expr
			if index < len(value.Values) {
				expression = value.Values[index]
			}
			visitor.bind(name.Name, expression, wave44PreparationNamedType(value.Type))
		}
	case *ast.CallExpr:
		visitor.visit(visitor.call(value.Fun))
	}
	return visitor
}

func wave45PRReachable(set wave42LegacyGitSourceSet, entry string) map[string]*ast.FuncDecl {
	visited := map[string]*ast.FuncDecl{}
	var visit func(string)
	visit = func(key string) {
		function := set.methods[key]
		if function == nil || visited[key] != nil {
			return
		}
		visited[key] = function
		visitor := wave45PRCallVisitor{set: set, bindings: map[string]string{}, functions: map[string]string{}, visit: visit}
		for _, fields := range []*ast.FieldList{function.Recv, function.Type.Params, function.Type.Results} {
			if fields != nil {
				for _, field := range fields.List {
					for _, name := range field.Names {
						visitor.bind(name.Name, nil, wave44PreparationNamedType(field.Type))
					}
				}
			}
		}
		ast.Walk(visitor, function.Body)
	}
	visit(entry)
	return visited
}

func wave45PROwnerViolations(set wave42LegacyGitSourceSet) []string {
	var violations []string
	owners := map[string]map[string]string{
		"PRPromotionWorker": {"meta": "outbound.MetadataStore", "blobs": "outbound.BlobStore", "engine": "outbound.GitEngine",
			"repositories": "outbound.RepositoryStore", "work": "*repositoryWork", "refs": "*repositoryRefCommands",
			"history": "*historyRecorder", "evidence": "outbound.RepositoryReader", "requireCompleteMemory": "bool", "capturedEvidence": "bool"},
		"repositoryRefCommands": {"meta": "RepositoryRefStore", "boundedTags": "RepositoryRefBoundedTagStore", "engine": "RepositoryRefEngine", "scope": "RepositoryRefWriteScope",
			"history": "RepositoryRefHistory", "append": "RepositoryRefAppend", "revisions": "RepositoryRefRevisions",
			"notifications": "RepositoryRefNotifications", "transactional": "bool", "profile": "RepositoryRefProfile", "configurationError": "error"},
		"repositoryRefScope": {"work": "*repositoryWork", "metadata": "RepositoryAuthorityMetadata", "repositories": "repositoryRefRepositoryReader",
			"history": "repositoryRefHistoryReader", "notifications": "RepositoryNotificationQueue"},
		"historyRecorder": {"metadata": "HistoryMetadata", "events": "HistoryEventStore", "memories": "HistoryMemoryReader", "sources": "HistoryDocumentSources",
			"scope": "HistoryWriteScope", "followups": "HistoryFollowups", "preparation": "outbound.HistoryDocumentProofStore",
			"supported": "[]domain.DocumentIdentity", "profile": "HistoryProfile", "configurationError": "error"},
		"historyCommandScope": {"work": "*repositoryWork", "transactions": "outbound.RepositoryTransactions", "state": "outbound.RepositoryTransactionState",
			"metadata": "RepositoryAuthorityMetadata", "repositories": "repositoryRefRepositoryReader", "notifications": "RepositoryNotificationQueue",
			"gitScans": "historyGitScanQueue", "prWake": "historyPRWakeQueue"},
	}
	for owner, expected := range owners {
		declaration := set.types[owner]
		if declaration == nil {
			violations = append(violations, "missing actual PR owner: "+owner)
			continue
		}
		shape, concrete := declaration.Type.(*ast.StructType)
		if !concrete || declaration.Assign.IsValid() {
			violations = append(violations, "PR owner is an alias or callback interface: "+owner)
			continue
		}
		observed := map[string]bool{}
		for _, field := range shape.Fields.List {
			if len(field.Names) == 0 {
				violations = append(violations, "embedded PR owner dependency: "+owner)
			}
			for _, name := range field.Names {
				if kind, allowed := expected[name.Name]; !allowed || kind != wave42LegacyGitType(field.Type) {
					violations = append(violations, "broad/private/callback PR dependency: "+owner+"."+name.Name)
				}
				observed[name.Name] = true
			}
		}
		for name := range expected {
			if !observed[name] {
				violations = append(violations, "missing PR resource dependency: "+owner+"."+name)
			}
		}
	}
	if set.methods["NewPRPromotionWorker"] == nil {
		violations = append(violations, "missing direct public PR factory")
	}
	for root := range set.methods {
		constructor := root == "NewPRPromotionWorker"
		owned := false
		for owner := range owners {
			owned = owned || strings.HasPrefix(root, owner+".")
		}
		if !constructor && !owned {
			continue
		}
		for key, function := range wave45PRReachable(set, root) {
			if strings.HasPrefix(key, "Service.") || strings.HasPrefix(key, "IdentityService.") {
				violations = append(violations, "actual PR owner reaches legacy method: "+key)
			}
			ast.Inspect(function, func(node ast.Node) bool {
				switch value := node.(type) {
				case *ast.Ident:
					if value.Name == "Service" || value.Name == "IdentityService" || value.Name == "NewService" || value.Name == "NewIdentityService" {
						violations = append(violations, "reachable PR reverse dependency: "+key)
					}
				case *ast.GoStmt:
					if constructor {
						violations = append(violations, "PR factory starts hidden work: "+key)
					}
				}
				return true
			})
			if constructor && (strings.HasPrefix(function.Name.Name, "Run") || strings.HasPrefix(function.Name.Name, "Process") || function.Name.Name == "runPRJob") {
				violations = append(violations, "PR factory executes worker: "+key)
			}
		}
	}
	return violations
}

func TestWave45PRPromotionActualOwnersHaveNoReverseDependencyOrAutostart(test *testing.T) {
	set, err := wave42LegacyGitParseSources(wave42LegacyGitReadSources(test, filepath.Join("..", "app")))
	if err != nil {
		test.Fatal(err)
	}
	for _, violation := range wave45PROwnerViolations(set) {
		test.Error(violation)
	}
	for owner, methods := range map[string][]string{
		"PRPromotionWorker": {"Run", "RunPRPromotionWorker", "ProcessPRPromotions", "runPRJob", "promoteBoundPR", "promoteBoundPRInTransaction",
			"completePRPromotion", "resolvePRSource", "promoteHistoricalPRContribution"},
		"repositoryRefCommands": {"UpdateRef", "updateRefWithPending", "updateRef", "appendDiverged", "reconcileSharedPendingPointers", "checkContextWrite"},
		"historyRecorder":       {"recordHistory", "recordHistoryPrepared", "wakePublishedPRJobs"},
	} {
		for _, method := range methods {
			if set.methods[owner+"."+method] == nil {
				test.Error("original PR/ref/history algorithm missing on actual owner", owner, method)
			}
		}
	}
	for _, entry := range []string{"PRPromotionWorker.runPRJob", "PRPromotionWorker.promoteBoundPR", "repositoryRefScope.WithinRefWrite", "repositoryRefScope.WithinRefsWrite", "historyCommandScope.WithinHistoryWrite"} {
		if wave45PRReachable(set, entry)["repositoryWriteWithWork"] == nil {
			test.Error("actual owner lost shared repository write policy", entry)
		}
	}
	for entry, scope := range map[string]string{"UpdateRef": "WithinRefWrite", "UpdateRefs": "WithinRefsWrite"} {
		if !dpc1ExecutionCalls(set, "repositoryRefCommands."+entry)["RepositoryRefWriteScope."+scope] {
			test.Error("actual ref owner bypasses shared write scope", entry)
		}
	}
	if wave45PRReachable(set, "PRPromotionWorker.Run")["PRPromotionWorker.runPRJob"] == nil {
		test.Error("public PR Run does not reach actual job execution")
	}
	if wave45PRReachable(set, "PRPromotionWorker.promoteBoundPRInTransaction")["historyRecorder.recordHistoryPrepared"] == nil ||
		wave45PRReachable(set, "PRPromotionWorker.promoteBoundPRInTransaction")["repositoryRefCommands.updateRef"] == nil {
		test.Error("actual promotion does not reach extracted ref/history owners")
	}
	for owner, methods := range map[string][]string{
		"PRPromotionWorker":     {"runPRJob", "ProcessPRPromotions", "RunPRPromotionWorker", "promoteBoundPRInTransaction", "completePRPromotion", "resolvePRSource", "promoteHistoricalPRContribution"},
		"repositoryRefCommands": {"UpdateRef", "updateRefWithPending", "updateRef", "appendDiverged", "reconcileSharedPendingPointers", "checkContextWrite"},
		"historyRecorder":       {"recordHistory", "recordHistoryPrepared", "wakePublishedPRJobs"},
	} {
		for _, method := range methods {
			if wave45PRReachable(set, "Service."+method)[owner+"."+method] == nil {
				test.Error("legacy facade does not reach actual extracted owner", owner, method)
			}
		}
	}
}

func TestWave45PRPromotionGuardTracesExactReceiverFreeFunctionAndAliasCalls(test *testing.T) {
	const valid = `package app
type PRPromotionWorker struct {
 meta outbound.MetadataStore; blobs outbound.BlobStore; engine outbound.GitEngine; repositories outbound.RepositoryStore
 work *repositoryWork; refs *repositoryRefCommands; history *historyRecorder; evidence outbound.RepositoryReader; requireCompleteMemory bool; capturedEvidence bool
}
type repositoryRefCommands struct {
 meta RepositoryRefStore; boundedTags RepositoryRefBoundedTagStore; engine RepositoryRefEngine; scope RepositoryRefWriteScope
 history RepositoryRefHistory; append RepositoryRefAppend; revisions RepositoryRefRevisions
 notifications RepositoryRefNotifications; transactional bool; profile RepositoryRefProfile; configurationError error
}
type repositoryRefScope struct {
 work *repositoryWork; metadata RepositoryAuthorityMetadata; repositories repositoryRefRepositoryReader
 history repositoryRefHistoryReader; notifications RepositoryNotificationQueue
}
type historyRecorder struct {
 metadata HistoryMetadata; events HistoryEventStore; memories HistoryMemoryReader; sources HistoryDocumentSources
 scope HistoryWriteScope; followups HistoryFollowups; preparation outbound.HistoryDocumentProofStore
 supported []domain.DocumentIdentity; profile HistoryProfile; configurationError error
}
type historyCommandScope struct {
 work *repositoryWork; transactions outbound.RepositoryTransactions; state outbound.RepositoryTransactionState
 metadata RepositoryAuthorityMetadata; repositories repositoryRefRepositoryReader; notifications RepositoryNotificationQueue
 gitScans historyGitScanQueue; prWake historyPRWakeQueue
}
func NewPRPromotionWorker() { validatePR() }
func validatePR() {}
func (worker *PRPromotionWorker) Run() { alias := worker; alias.ProcessPRPromotions() }
func (worker *PRPromotionWorker) ProcessPRPromotions() { execute := worker.runPRJob; execute() }
func (worker *PRPromotionWorker) runPRJob() { worker.refs.updateRef(); worker.history.recordHistory(); go boundedJob() }
func (refs *repositoryRefCommands) updateRef() { sharedWrite() }
func (history *historyRecorder) recordHistory() {}
func sharedWrite() {}
func boundedJob() {}
func (service *Service) runPRJob() { NewService() }
func (service *Service) ProcessPRPromotions() { worker := &PRPromotionWorker{}; worker.ProcessPRPromotions() }
func runPRJob() { NewIdentityService() }
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
	reachable := wave45PRReachable(set, "PRPromotionWorker.Run")
	if reachable["PRPromotionWorker.runPRJob"] == nil || reachable["repositoryRefCommands.updateRef"] == nil || reachable["sharedWrite"] == nil ||
		reachable["Service.runPRJob"] != nil || reachable["runPRJob"] != nil || len(wave45PROwnerViolations(set)) != 0 {
		test.Fatal("guard confused receiver/free/alias scope, unrelated facade, or bounded internal work")
	}
	shadowed := strings.Replace(valid, "worker.refs.updateRef();", "runPRJob := func() {}; runPRJob(); worker.refs.updateRef();", 1)
	if len(wave45PROwnerViolations(parse(shadowed))) != 0 {
		test.Fatal("local function incorrectly resolved to unrelated package function")
	}
	for _, invalid := range []string{
		strings.Replace(valid, "execute()", "runPRJob()", 1),
		strings.Replace(valid, "execute()", "hidden()", 1) + "\nfunc hidden() { NewService() }",
		strings.Replace(valid, "execute()", "service := &Service{}; service.runPRJob()", 1),
		strings.Replace(valid, "func sharedWrite() {}", "func sharedWrite() { NewIdentityService() }", 1),
		strings.Replace(valid, "func validatePR() {}", "func validatePR() { go boundedJob() }", 1),
		strings.Replace(valid, "validatePR() }", "worker := &PRPromotionWorker{}; worker.Run() }", 1),
		strings.Replace(valid, "validatePR() }", "start := validatePR; start() }", 1) + "\nfunc (service *IdentityService) private() {}\n",
		strings.Replace(valid, "work *repositoryWork; refs", "work *Service; refs", 1),
		strings.Replace(valid, "work *repositoryWork; metadata", "work *Service; metadata", 1),
		strings.Replace(valid, "type historyRecorder struct {", "type historyRecorder struct { callback func();", 1),
		strings.Replace(valid, "func NewPRPromotionWorker() { validatePR() }", "func NewPRPromotionWorker() { NewIdentityService() }", 1),
	} {
		if strings.Contains(invalid, "start := validatePR") {
			invalid = strings.Replace(invalid, "func validatePR() {}", "func validatePR() { NewService() }", 1)
		}
		if len(wave45PROwnerViolations(parse(invalid))) == 0 {
			test.Fatal("guard missed reachable reverse dependency or constructor work")
		}
	}
}
