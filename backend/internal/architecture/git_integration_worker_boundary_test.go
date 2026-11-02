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

func wave43ForbiddenOwner(node ast.Node) bool {
	forbidden := false
	ast.Inspect(node, func(current ast.Node) bool {
		name := ""
		switch value := current.(type) {
		case *ast.Ident:
			name = value.Name
		case *ast.SelectorExpr:
			name = value.Sel.Name
		}
		switch name {
		case "Service", "IdentityService", "GitScans", "NewService", "NewIdentityService", "NewGitScans", "Callback", "Delegate":
			forbidden = true
		}
		return true
	})
	return forbidden
}

func wave43IntegrationFields(set wave42LegacyGitSourceSet) []string {
	allowed := map[string]bool{"*repositoryWork": true, "outbound.GitScanStore": true, "outbound.GitCommitReader": true, "outbound.GitSourceAuthorizer": true, "outbound.RepositoryReader": true, "MemoryProjectionSource": true, "bool": true, "inbound.RepositoryStateQueries": true, "error": true}
	declaration := set.types["GitIntegrationWorkers"]
	if declaration == nil {
		return []string{"missing concrete GitIntegrationWorkers"}
	}
	shape, concrete := declaration.Type.(*ast.StructType)
	if !concrete {
		return []string{"integration owner is an alias or private callback interface"}
	}
	var violations []string
	work := 0
	for _, field := range shape.Fields.List {
		kind := wave42LegacyGitType(field.Type)
		if !allowed[kind] || wave43ForbiddenOwner(field) {
			violations = append(violations, "integration retains a broad/private/callback owner: "+kind)
		}
		if kind == "*repositoryWork" {
			work++
		}
	}
	if work != 1 {
		violations = append(violations, "integration must share exactly one actual repositoryWork")
	}
	for _, root := range []string{"NewGitIntegrationWorkers", "gitIntegrationRevisionQueries"} {
		for _, function := range wave43OwnedMethods(set, root) {
			startsWork := false
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if _, starts := node.(*ast.GoStmt); starts {
					startsWork = true
				}
				return true
			})
			if wave43ForbiddenOwner(function) || startsWork {
				violations = append(violations, "factory reconstructs a command owner or starts work: "+root)
			}
		}
	}
	for name, function := range set.methods {
		if strings.HasPrefix(name, "GitIntegrationWorkers.") {
			for _, owned := range wave43OwnedMethods(set, name) {
				if wave43ForbiddenOwner(owned) {
					violations = append(violations, "integration delegates through a legacy owner: "+function.Name.Name)
				}
			}
		}
	}
	return violations
}

func wave43OwnedMethods(set wave42LegacyGitSourceSet, name string) []*ast.FuncDecl {
	seen := map[string]bool{}
	var functions []*ast.FuncDecl
	var visit func(string)
	visit = func(name string) {
		function := set.methods[name]
		if function == nil || seen[name] {
			return
		}
		seen[name] = true
		functions = append(functions, function)
		receiver := ""
		if function.Recv != nil && len(function.Recv.List[0].Names) == 1 {
			receiver = function.Recv.List[0].Names[0].Name
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, invoked := node.(*ast.CallExpr)
			if !invoked {
				return true
			}
			if selected, method := call.Fun.(*ast.SelectorExpr); method {
				if owner, direct := selected.X.(*ast.Ident); direct && owner.Name == receiver {
					visit("GitIntegrationWorkers." + selected.Sel.Name)
				}
			} else {
				visit(wave42LegacyGitFunctionName(call.Fun))
			}
			return true
		})
	}
	visit(name)
	return functions
}

func wave43IntegrationAlgorithms(set wave42LegacyGitSourceSet) []string {
	var violations []string
	for method, calls := range map[string][]string{
		"Process":                             {"workerDocumentIdentityContext", "WithSystemActor", "admittedJob", "ClaimGitScan", "run"},
		"run":                                 {"workerRepository", "authorizeGitRead", "ReadCommitTree", "ReadCommitDeltas", "planGitIndex", "validateIndexTree", "FindGitInverses", "repositoryWriteErrorWithWork", "FinishGitScan", "WithoutCancel"},
		"Run":                                 {"Process", "ProcessContributions", "NewTicker"},
		"Reconcile":                           {"reconcile"},
		"reconcile":                           {"ListRepos", "resourceWork", "ClaimGitHeadScan", "freezeGitAdmissionBatch", "freezeGitHeadContext", "readGitAdmissionSources", "ListGitHeads", "repositoryWriteErrorWithWork", "reuseGitAdmissionSnapshot", "FinishGitHeadScan", "persistGitContextHeads", "persistGitAdmissionBatch"},
		"RunReconciler":                       {"reconcile", "NewTimer"},
		"ProcessContributions":                {"WithSystemActor", "workerDocumentIdentityContext", "admittedJob", "ClaimGitContributionWork", "runGitContribution", "processGitContributionSupplements"},
		"runGitContribution":                  {"authorizeGitAdmission", "prepareGitContribution", "applyGitContribution", "FinishGitContributionWork", "repositoryWriteErrorWithWork", "WithoutCancel"},
		"prepareGitContribution":              {"repositoryReadForRepoWithWork", "readGitAdmissionEvidence", "newCodeEvidence", "locateGitContributionIntroduction", "locateGitContributionInRange", "verifyContributionCapture", "contributionPublicationMemorySource"},
		"applyGitContribution":                {"repositoryWriteWithWork", "workerRepository", "reuseGitAdmissionSnapshot", "PutGitContributionProof"},
		"processGitContributionSupplements":   {"admittedJob", "ClaimGitContributionSupplement", "runGitContributionSupplement"},
		"runGitContributionSupplement":        {"authorizeGitAdmission", "prepareGitContributionSupplement", "applyGitContributionSupplement", "repositoryWriteErrorWithWork", "WithoutCancel"},
		"prepareGitContributionSupplement":    {"repositoryReadForRepoWithWork", "readGitAdmissionEvidence", "contributionPublicationMemorySource"},
		"applyGitContributionSupplement":      {"repositoryWriteErrorWithWork", "reuseGitAdmissionSnapshot", "FinishGitContributionSupplement"},
		"discoverGitAssociations":             {"CatalogChanges", "SaveGitAssociationDiscovery", "repositoryWriteErrorWithWork"},
		"freezeGitAdmission":                  {"repositoryReadForRepoWithWork", "readGitAdmissionEvidence"},
		"readGitAdmissionSnapshot":            {"repositoryReadForRepoWithWork", "readGitAdmissionSnapshotInRead"},
		"readGitAdmissionSnapshotInRead":      {"ListHistory", "readGitAdmissionSnapshotWithHistory", "captureGitAdmissionGeneration"},
		"workerRepository":                    {"ReadRepository"},
		"verifyContributionCapture":           {"verifyContributionCaptureFromReader"},
		"contributionPublicationMemorySource": {"contributionPublicationMemorySourceFromReader"},
		"contributionDestination":             {"contributionDestinationFromReader"},
	} {
		violations = append(violations, wave42LegacyGitRequireCalls(set, "GitIntegrationWorkers."+method, calls...)...)
	}
	for _, method := range []string{"Process", "ProcessContributions"} {
		declaration := set.methods["GitIntegrationWorkers.Run"]
		matched := false
		if declaration != nil {
			ast.Inspect(declaration.Body, func(node ast.Node) bool {
				call, called := node.(*ast.CallExpr)
				if called && wave42LegacyGitFunctionName(call.Fun) == method && len(call.Args) == 2 {
					if limit, literal := call.Args[1].(*ast.BasicLit); literal && limit.Value == "2" {
						matched = true
					}
				}
				return true
			})
		}
		if !matched {
			violations = append(violations, "Run changed its original two-plus-two loop: "+method)
		}
	}
	return violations
}

func wave43IntegrationFacades(set wave42LegacyGitSourceSet) []string {
	var violations []string
	for legacy, methods := range map[string][]string{
		"GitScans": {"run", "Process", "Run", "Reconcile", "reconcile", "RunReconciler", "validateIndexTree", "freezeGitAdmissionBatch", "prepareGitAdmissionBatch", "persistGitAdmissionBatch", "freezeGitHeadContext", "persistGitContextHeads", "readGitAdmissionSources", "discoverGitAssociations", "ProcessContributions", "runGitContribution", "prepareGitContribution", "applyGitContribution", "processGitContributionSupplements", "runGitContributionSupplement", "prepareGitContributionSupplement", "applyGitContributionSupplement"},
		"Service":  {"freezeGitAdmission", "persistGitAdmission", "persistGitAdmissionInWrite", "readGitAdmissionSnapshot", "readGitAdmissionSnapshotInRead", "captureGitAdmissionGeneration", "readGitAdmissionHistory", "readGitAdmissionSnapshotWithHistory", "reuseGitAdmissionSnapshot", "readGitAdmissionEvidence", "hydrateGitAdmissionEvidence"},
	} {
		assembly := "integration"
		if legacy == "Service" {
			assembly = "gitIntegration"
		}
		for _, method := range methods {
			declaration := set.methods[legacy+"."+method]
			if declaration == nil || declaration.Body == nil {
				violations = append(violations, "missing compatibility facade: "+legacy+"."+method)
				continue
			}
			calls := wave42LegacyGitCalls(declaration.Body)
			if !calls[assembly] || !calls[method] || set.methods["GitIntegrationWorkers."+method] == nil {
				violations = append(violations, "facade does not call its same actual-owner method: "+legacy+"."+method)
			}
			ast.Inspect(declaration.Body, func(node ast.Node) bool {
				switch value := node.(type) {
				case *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.GoStmt, *ast.FuncLit:
					violations = append(violations, "legacy facade retains an algorithm: "+legacy+"."+method)
				case *ast.SelectorExpr:
					if value.Sel.Name == "meta" || value.Sel.Name == "store" || value.Sel.Name == "core" {
						violations = append(violations, "legacy facade bypasses the actual owner: "+legacy+"."+method)
					}
				}
				return true
			})
		}
	}
	return violations
}

func wave43IntegrationRegistration(source string) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", source, 0)
	if err != nil {
		return nil, err
	}
	factories, bound := 0, 0
	registrations := map[string]int{}
	ast.Inspect(file, func(node ast.Node) bool {
		if assignment, assigned := node.(*ast.AssignStmt); assigned {
			for _, expression := range assignment.Rhs {
				if call, called := expression.(*ast.CallExpr); called && wave42LegacyGitFunctionName(call.Fun) == "NewGitIntegrationWorkers" && len(assignment.Lhs) > 0 && wave42LegacyGitType(assignment.Lhs[0]) == "gitIntegrationWorkers" {
					bound++
				}
			}
		}
		call, called := node.(*ast.CallExpr)
		if !called {
			return true
		}
		if wave42LegacyGitFunctionName(call.Fun) == "NewGitIntegrationWorkers" {
			factories++
		}
		if wave42LegacyGitType(call.Fun) == "lifecycle.start" && len(call.Args) == 1 {
			selected, selector := call.Args[0].(*ast.SelectorExpr)
			if selector && (selected.Sel.Name == "Run" || selected.Sel.Name == "RunReconciler") {
				registrations[wave42LegacyGitType(selected)]++
			}
		}
		return true
	})
	var violations []string
	if factories != 1 || bound != 1 {
		violations = append(violations, "API must construct exactly one named direct integration owner")
	}
	for _, method := range []string{"Run", "RunReconciler"} {
		if registrations["gitIntegrationWorkers."+method] != 1 {
			violations = append(violations, "API is not directly registered to gitIntegrationWorkers."+method)
		}
		if registrations["scans."+method] != 0 {
			violations = append(violations, "API still runs the legacy scans facade")
		}
	}
	return violations, nil
}

func TestWave43GitIntegrationActualOwnerAlgorithmsAndDirectAPIRegistration(test *testing.T) {
	set, err := wave42LegacyGitParseSources(wave42LegacyGitReadSources(test, filepath.Join("..", "app")))
	if err != nil {
		test.Fatal(err)
	}
	violations := wave43IntegrationFields(set)
	violations = append(violations, wave43IntegrationAlgorithms(set)...)
	violations = append(violations, wave43IntegrationFacades(set)...)
	source, err := os.ReadFile(filepath.Join("..", "..", "cmd", "cxtd", "main.go"))
	if err != nil {
		test.Fatal(err)
	}
	registration, err := wave43IntegrationRegistration(string(source))
	if err != nil {
		test.Fatal(err)
	}
	for _, violation := range append(violations, registration...) {
		test.Error(violation)
	}
}

func TestWave43GitIntegrationGuardRejectorsAndFalsePositiveFixtures(test *testing.T) {
	for _, body := range []string{
		`type GitIntegrationWorkers struct { core *Service }`,
		`type GitIntegrationWorkers struct { identity *IdentityService }`,
		`type GitIntegrationWorkers struct { scans *GitScans }`,
		`type GitIntegrationWorkers struct { work *repositoryWork; run func() error }`,
		`type privateRunner interface { Process() }; type GitIntegrationWorkers struct { work *repositoryWork; runner privateRunner }`,
		`type GitIntegrationWorkers = Service`,
		`type GitIntegrationWorkers struct { work *repositoryWork }; func (workers *GitIntegrationWorkers) Process() { NewService().Process() }`,
		`type GitIntegrationWorkers struct { work *repositoryWork }; func hidden() { legacy.NewIdentityService() }; func (workers *GitIntegrationWorkers) Process() { hidden() }`,
	} {
		set, err := wave42LegacyGitParseSources(map[string]string{"renamed.go": "package app\n" + body})
		if err != nil || len(wave43IntegrationFields(set)) == 0 {
			test.Fatal("guard missed reverse delegation or private callback ownership", body, err)
		}
	}
	valid := `package app
type GitIntegrationWorkers struct { work *repositoryWork; store outbound.GitScanStore; memory MemoryProjectionSource }
func (workers *GitIntegrationWorkers) Process() { message := "Service NewGitScans Callback"; _ = message; workers.store.ClaimGitScan() }
`
	set, err := wave42LegacyGitParseSources(map[string]string{"arbitrary_name.go": valid})
	if err != nil || len(wave43IntegrationFields(set)) != 0 {
		test.Fatal("guard rejected narrow ports, harmless strings or Service-free actual bodies", err)
	}
	fake, err := wave42LegacyGitParseSources(map[string]string{"fake.go": `package app; func (workers *GitIntegrationWorkers) Process() { workers.runner.Process() }`})
	if err != nil || len(wave43IntegrationAlgorithms(fake)) == 0 {
		test.Fatal("guard accepted a callback shell as the actual algorithms", err)
	}
	good := `package main; func start() { gitIntegrationWorkers, err := app.NewGitIntegrationWorkers(config); _ = err; lifecycle.start(gitIntegrationWorkers.Run); lifecycle.start(gitIntegrationWorkers.RunReconciler) }`
	for _, source := range []string{strings.ReplaceAll(good, "gitIntegrationWorkers.Run", "scans.Run"), good + "\nfunc extra() { app.NewGitIntegrationWorkers(config) }", strings.ReplaceAll(good, "lifecycle.start(gitIntegrationWorkers.Run)", `note := "gitIntegrationWorkers.Run"; _ = note`)} {
		if violations, err := wave43IntegrationRegistration(source); err != nil || len(violations) == 0 {
			test.Fatal("guard accepted disconnected, string-only or duplicate API registration", violations, err)
		}
	}
	if violations, err := wave43IntegrationRegistration(good); err != nil || len(violations) != 0 {
		test.Fatal("guard rejected the actual named direct lifecycle shape", violations, err)
	}
	owned := `package app
type GitIntegrationWorkers struct { work *repositoryWork }
func NewGitIntegrationWorkers() {}
func gitIntegrationRevisionQueries() {}
func (workers *GitIntegrationWorkers) Reconcile() { workers.reconcile() }
func (workers *GitIntegrationWorkers) reconcile() { go readHead() }
func (legacy *GitScans) reconcile() { NewService() }
`
	ownedSet, err := wave42LegacyGitParseSources(map[string]string{"owner.go": owned})
	if err != nil || len(wave43IntegrationFields(ownedSet)) != 0 {
		test.Fatal("guard confused same-named legacy methods or bounded in-job goroutines with a legacy dependency", err)
	}
	for _, source := range []string{
		strings.Replace(owned, "func NewGitIntegrationWorkers() {}", "func NewGitIntegrationWorkers() { go start() }", 1),
		strings.Replace(owned, "workers.reconcile()", "NewService()", 1),
	} {
		set, err := wave42LegacyGitParseSources(map[string]string{"bad.go": source})
		if err != nil || len(wave43IntegrationFields(set)) == 0 {
			test.Fatal("guard accepted constructor autostart or actual legacy delegation", err)
		}
	}
}
