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

func wave48File(test *testing.T, parts ...string) *ast.File {
	test.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(parts...), nil, 0)
	if err != nil {
		test.Fatal(err)
	}
	return file
}

func wave48Package(test *testing.T, directory string) wave42LegacyGitSourceSet {
	test.Helper()
	set, err := wave42LegacyGitParseSources(wave42LegacyGitReadSources(test, directory))
	if err != nil {
		test.Fatal(err)
	}
	return set
}

func wave48Calls(node ast.Node) map[string]int {
	counts := map[string]int{}
	ast.Inspect(node, func(nested ast.Node) bool {
		if call, ok := nested.(*ast.CallExpr); ok {
			path, name := workerSelectorPath(call.Fun), wave42LegacyGitFunctionName(call.Fun)
			counts[name]++
			if path != name {
				counts[path]++
			}
		}
		return true
	})
	return counts
}

func wave48Inside(node ast.Node, blocks []*ast.BlockStmt) bool {
	for _, block := range blocks {
		if node.Pos() > block.Pos() && node.End() < block.End() {
			return true
		}
	}
	return false
}

func wave48RuntimeGuards(function *ast.FuncDecl, method string) []*ast.BlockStmt {
	var blocks []*ast.BlockStmt
	ast.Inspect(function.Body, func(node ast.Node) bool {
		conditional, ok := node.(*ast.IfStmt)
		if !ok || conditional.Init != nil || conditional.Else != nil {
			return true
		}
		if negated, ok := conditional.Cond.(*ast.UnaryExpr); ok && negated.Op == token.NOT {
			if call, ok := negated.X.(*ast.CallExpr); ok && workerSelectorPath(call.Fun) == "runtime."+method && len(call.Args) == 0 {
				blocks = append(blocks, conditional.Body)
			}
		}
		return true
	})
	return blocks
}

func wave48FullWorkers(expression ast.Expr, flags map[string]bool) bool {
	switch value := expression.(type) {
	case *ast.Ident:
		return flags[value.Name]
	case *ast.ParenExpr:
		return wave48FullWorkers(value.X, flags)
	case *ast.BinaryExpr:
		if value.Op == token.EQL {
			return workerSelectorPath(value.X) == "outbound.BoundedWorkersProfile" || workerSelectorPath(value.Y) == "outbound.BoundedWorkersProfile"
		}
		if value.Op == token.LAND {
			return wave48FullWorkers(value.X, flags) || wave48FullWorkers(value.Y, flags)
		}
	}
	return false
}

func wave48CheckRoot(test *testing.T, file *ast.File, providers bool) {
	test.Helper()
	for _, imported := range file.Imports {
		dependency, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			test.Fatal(err)
		}
		if dependency == "net/http" || dependency == "database/sql" || dependency == "os/exec" || strings.Contains(dependency, "/pgx") ||
			strings.Contains(dependency, "/mcpserver") || strings.Contains(dependency, "/delivery/") || strings.Contains(dependency, "/semaphore") {
			test.Error("worker root imports server execution or independent resource ownership", dependency)
		}
		if !providers && (strings.HasSuffix(dependency, "/githubapp") || strings.HasSuffix(dependency, "/gitevidence") ||
			strings.HasSuffix(dependency, "/resend") || strings.HasSuffix(dependency, "/webhookdelivery") || strings.HasSuffix(dependency, "/workerserver")) {
			test.Error("shared worker root imports worker-only providers", dependency)
		}
	}
	var running []*ast.BlockStmt
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Recv != nil && function.Name.Name == "Run" &&
			wave42LegacyGitType(function.Recv.List[0].Type) == "*WorkerRuntime" {
			running = append(running, function.Body)
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.Ident:
			switch value.Name {
			case "Service", "IdentityService", "GitHubConnections", "NewService", "NewIdentityService", "NewReadServices", "NewGitHubConnections":
				test.Error("worker root captures or constructs broad application runtime", value.Name)
			}
		case *ast.GoStmt:
			if !wave48Inside(value, running) {
				test.Error("worker constructor starts a goroutine")
			}
		case *ast.ChanType:
			test.Error("worker root introduces a queue/gate channel")
		case *ast.CallExpr:
			name := wave42LegacyGitFunctionName(value.Fun)
			if path := workerSelectorPath(value.Fun); path == "store.Open" || path == "Open" {
				test.Error("worker root bypasses shared storage/application composition", path)
			}
			switch name {
			case "OpenFSStore", "OpenProcessAdmission", "ConfigureProcessAdmission", "NewPool", "NewGate", "NewQueue", "NewSemaphore", "NewWeighted":
				test.Error("worker root bypasses shared infrastructure/admission", name)
			case "Run", "RunReconciler", "Tick", "Process", "Start", "NewTicker", "AfterFunc":
				if !wave48Inside(value, running) {
					test.Error("worker constructor executes or schedules work", name)
				}
			}
		}
		return true
	})
}

func TestWave48DocumentWorkerRootDoesNotCaptureOrAutostartRuntime(test *testing.T) {
	set := wave48Package(test, filepath.Join("..", "serverruntime"))
	for _, name := range []string{"document_worker.go", "application_workers.go", "application_worker_sources.go"} {
		file := set.files[filepath.Join("..", "serverruntime", name)]
		if file == nil {
			test.Fatal("missing actual shared worker root", name)
		}
		wave48CheckRoot(test, file, false)
	}
	root := set.files[filepath.Join("..", "serverruntime", "document_worker.go")]
	counts := wave48Calls(root)
	for _, name := range []string{"OpenInfrastructure", "NewDocumentWorkers", "NewDisplayMaintenanceWorker", "newApplicationWorkers"} {
		if counts[name] != 1 {
			test.Error("shared root must assemble one existing infrastructure/document/application owner", name, counts[name])
		}
	}
	if counts["NewGitHubWorker"] != 0 || counts["EvidenceInstallation"] != 0 || set.methods["OpenDocumentWorker"] == nil {
		test.Error("shared root must preserve the document entrypoint and leave GitHub provider assembly outside core")
	}
	helpers := set.files[filepath.Join("..", "serverruntime", "application_worker_sources.go")]
	for _, declaration := range helpers.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && (function.Recv != nil || function.Name.Name != "WorkerRepositories" && function.Name.Name != "ValidateGitHubOrigin") {
			test.Error("shared source helpers retain provider bootstrap", function.Name.Name)
		}
	}
	if set.methods["WorkerRepositories"] == nil || set.methods["ValidateGitHubOrigin"] == nil {
		test.Error("shared source validation helpers are missing")
	}
}

func TestWave48ApplicationWorkerRootUsesDirectCompleteOwners(test *testing.T) {
	set := wave48Package(test, filepath.Join("..", "serverruntime"))
	function := set.methods["newApplicationWorkers"]
	if function == nil {
		test.Fatal("missing actual full worker assembly")
	}
	counts := wave48Calls(function.Body)
	for _, name := range []string{"NewProjectionWorkers", "NewNotificationWorker", "NewMaintenanceWorkers", "NewGitChangeWorker", "NewGitIntegrationWorkers",
		"NewMemoryPreparationResources", "NewMemoryPreparationWorkers", "NewPRPromotionWorker", "NewInvitationEmailWorker", "gitengine.NewEngine"} {
		if counts[name] != 1 {
			test.Error("full root must construct exactly one direct existing owner/resource", name, counts[name])
		}
	}
	if counts["NewGitHubWorker"] != 0 || counts["webhookdelivery.New"] != 0 {
		test.Error("full assembly must consume the shared GitHub owner and injected notification sender")
	}
	bindings := 0
	ast.Inspect(function.Body, func(node ast.Node) bool {
		field, ok := node.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		if workerSelectorPath(field.Key) == "Profile" && !strings.HasSuffix(workerSelectorPath(field.Value), "Complete") {
			test.Error("full assembly weakens an explicit Complete worker profile")
		}
		if workerSelectorPath(field.Key) == "Sender" && !strings.HasSuffix(workerSelectorPath(field.Value), ".NotificationSender") &&
			!strings.HasSuffix(workerSelectorPath(field.Value), ".InvitationSender") {
			test.Error("full assembly must consume the injected worker provider sender")
		}
		if workerSelectorPath(field.Key) == "run" {
			bindings++
			method, ok := field.Value.(*ast.SelectorExpr)
			if !ok || method.Sel.Name != "Run" && method.Sel.Name != "RunReconciler" {
				test.Error("worker binding must name an existing owner method, not a callback")
			}
		}
		return true
	})
	if bindings != 13 {
		test.Error("full assembly must retain twelve existing bindings and optional GitHub", bindings)
	}
}

func TestWave48TemporarySourceFactoryIsConstructionOnly(test *testing.T) {
	set := wave48Package(test, filepath.Join("..", "serverruntime"))
	declaration := set.types["WorkerSourceFactory"]
	if declaration == nil {
		test.Fatal("missing temporary worker source constructor")
	}
	factory, ok := declaration.Type.(*ast.FuncType)
	if !ok || len(factory.Params.List) != 1 || wave42LegacyGitType(factory.Params.List[0].Type) != "store.Store" ||
		factory.Results == nil || len(factory.Results.List) != 2 || wave42LegacyGitType(factory.Results.List[0].Type) != "ApplicationWorkerConfig" ||
		wave42LegacyGitType(factory.Results.List[1].Type) != "error" {
		test.Fatal("source factory must build configuration from the existing store, not delegate business execution")
	}
	for _, owner := range []string{"WorkerRuntime", "workerBinding", "ApplicationWorkerConfig"} {
		declaration := set.types[owner]
		if declaration == nil {
			test.Fatal("missing concrete worker ownership type", owner)
		}
		structure, ok := declaration.Type.(*ast.StructType)
		if !ok {
			test.Fatal("worker ownership must remain concrete", owner)
		}
		for _, field := range structure.Fields.List {
			_, callback := field.Type.(*ast.FuncType)
			binding := owner == "workerBinding" && len(field.Names) == 1 && field.Names[0].Name == "run"
			if callback && !binding || strings.Contains(wave42LegacyGitType(field.Type), "WorkerSourceFactory") {
				test.Error("actual worker ownership retains a constructor/business callback", owner)
			}
		}
	}
	validator := set.methods["ValidateWorkerExecutionProfile"]
	if validator == nil || !wave42LegacyGitCalls(validator.Body)["executionProfileFromEnvironment"] || wave42LegacyGitCalls(validator.Body)["OpenInfrastructure"] {
		test.Fatal("worker prevalidation must validate the actual profile without opening storage")
	}
	invocations := 0
	root := set.files[filepath.Join("..", "serverruntime", "document_worker.go")]
	for _, declaration := range root.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		flags := map[string]bool{}
		var infrastructure *ast.CallExpr
		var conditionals []*ast.IfStmt
		var sourceCalls []*ast.CallExpr
		ast.Inspect(function.Body, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.AssignStmt:
				if len(value.Lhs) == 1 && len(value.Rhs) == 1 && wave48FullWorkers(value.Rhs[0], flags) {
					flags[workerSelectorPath(value.Lhs[0])] = true
				}
			case *ast.IfStmt:
				conditionals = append(conditionals, value)
			case *ast.CallExpr:
				if workerSelectorPath(value.Fun) == "OpenInfrastructure" {
					infrastructure = value
				}
				if _, indexed := value.Fun.(*ast.IndexExpr); indexed && len(value.Args) == 1 && strings.HasSuffix(workerSelectorPath(value.Args[0]), ".Store") {
					sourceCalls = append(sourceCalls, value)
				}
			}
			return true
		})
		for _, call := range sourceCalls {
			invocations++
			guarded, rejected := false, false
			for _, conditional := range conditionals {
				if wave48FullWorkers(conditional.Cond, flags) && wave48Inside(call, []*ast.BlockStmt{conditional.Body}) {
					guarded = true
				}
				if infrastructure != nil && conditional.End() < infrastructure.Pos() && wave42LegacyGitCalls(conditional.Cond)["len"] {
					ast.Inspect(conditional.Body, func(node ast.Node) bool {
						if _, returned := node.(*ast.ReturnStmt); returned {
							rejected = true
						}
						return true
					})
				}
			}
			if infrastructure == nil || call.Pos() < infrastructure.End() || !guarded || !rejected {
				test.Error("source construction must follow shared storage, run only for the full profile, and reject missing sources before storage")
			}
		}
	}
	if invocations != 1 {
		test.Error("full root must invoke its temporary source factory exactly once", invocations)
	}
}

func TestWave48ProviderBootstrapOwnsAdaptersAndValidatesBeforeCore(test *testing.T) {
	set := wave48Package(test, filepath.Join("..", "workerserver"))
	file := set.files[filepath.Join("..", "workerserver", "runtime.go")]
	if file == nil || set.methods["OpenWorker"] == nil {
		test.Fatal("missing actual worker-only provider bootstrap")
	}
	wave48CheckRoot(test, file, true)
	open := set.methods["OpenWorker"]
	counts := wave48Calls(open.Body)
	if counts["serverruntime.ValidateWorkerExecutionProfile"] != 1 || counts["serverruntime.OpenWorker"] != 2 {
		test.Fatal("worker bootstrap must retain one core delegation per document/full profile branch")
	}
	if len(open.Body.List) < 2 || wave48Calls(open.Body.List[1])["serverruntime.ValidateWorkerExecutionProfile"] != 1 {
		test.Error("profile validation must precede environment/provider/storage assembly")
	}
	selection, ok := open.Body.List[0].(*ast.IfStmt)
	if !ok || selection.Init != nil || selection.Else != nil || len(selection.Body.List) != 1 {
		test.Fatal("document/default profile must retain one explicit early delegation")
	}
	condition, ok := selection.Cond.(*ast.BinaryExpr)
	if !ok || condition.Op != token.NEQ || workerSelectorPath(condition.Y) != "outbound.BoundedWorkersProfile" {
		test.Fatal("only the complete profile may consume external provider sources")
	}
	ast.Inspect(open.Body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && workerSelectorPath(call.Fun) == "serverruntime.OpenWorker" {
			if wave48Inside(call, []*ast.BlockStmt{selection.Body}) {
				if len(call.Args) != 1 || workerSelectorPath(call.Args[0]) != "ctx" {
					test.Error("document profile must not assemble application providers")
				}
				return true
			}
			if len(call.Args) != 2 || workerSelectorPath(call.Args[0]) != "ctx" || !strings.HasSuffix(workerSelectorPath(call.Args[1]), ".sources") {
				test.Error("provider bootstrap must supply context and the temporary environment.sources constructor")
			}
		}
		return true
	})
	counts = wave48Calls(file)
	for _, name := range []string{"NewGitHubWorker", "EvidenceInstallation", "webhookdelivery.New"} {
		if counts[name] != 1 {
			test.Error("provider bootstrap must own one shared GitHub owner/evidence resolver and notification sender", name, counts[name])
		}
	}
}

func TestWave48WorkerArgvCallsOpenWorker(test *testing.T) {
	set := wave48Package(test, filepath.Join("..", "..", "cmd", "cxt-worker"))
	main, run := set.methods["main"], set.methods["run"]
	if main == nil || run == nil {
		test.Fatal("missing actual worker argv entrypoint")
	}
	argv := false
	ast.Inspect(main.Body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && workerSelectorPath(call.Fun) == "run" && len(call.Args) == 2 {
			if arguments, ok := call.Args[1].(*ast.SliceExpr); ok && workerSelectorPath(arguments.X) == "os.Args" && arguments.High == nil {
				if first, ok := arguments.Low.(*ast.BasicLit); ok && first.Value == "1" {
					argv = true
				}
			}
		}
		return true
	})
	counts := wave48Calls(run.Body)
	if !argv || counts["workerserver.OpenWorker"] != 1 || counts["worker.Run"] != 1 || counts["worker.Close"] != 1 ||
		counts["serverruntime.OpenWorker"] != 0 || counts["OpenDocumentWorker"] != 0 {
		test.Error("actual argv must open through workerserver and run/close the same worker, without bypassing provider/profile selection")
	}
}

func TestWave48APIKeepsExistingWorkersInsideExactExternalGuard(test *testing.T) {
	set := wave48Package(test, filepath.Join("..", "..", "cmd", "cxtd"))
	serve := set.methods["serve"]
	if serve == nil {
		test.Fatal("missing actual API serve body")
	}
	external, documents := wave48RuntimeGuards(serve, "ExternalWorkers"), wave48RuntimeGuards(serve, "BoundedDocuments")
	if len(external) != 1 || len(documents) != 1 || !wave48Inside(documents[0], external) {
		test.Fatal("API must retain the document guard inside exactly !runtime.ExternalWorkers()")
	}
	constructors := map[string]bool{
		"ProjectionWorkersForProfile": true, "NewNotificationWorker": true, "NewMaintenanceWorkers": true, "NewGitChangeWorker": true,
		"NewGitIntegrationWorkers": true, "NewMemoryPreparationWorkers": true, "NewPRPromotionWorker": true,
		"NewInvitationEmailWorker": true, "NewGitHubWorker": true, "DocumentWorkersForProfile": true, "NewDisplayMaintenanceWorker": true,
	}
	registrations := 0
	ast.Inspect(serve.Body, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok && workerSelectorPath(selector) == "lifecycle.start" && !wave48Inside(selector, external) {
			test.Error("API starts or captures a worker outside the external-worker guard")
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		name, path := wave42LegacyGitFunctionName(call.Fun), workerSelectorPath(call.Fun)
		if constructors[name] {
			document := name == "DocumentWorkersForProfile" || name == "NewDisplayMaintenanceWorker"
			if !wave48Inside(call, external) || wave48Inside(call, documents) != document {
				test.Error("API worker construction escapes its exact profile ownership", name)
			}
		}
		if path == "lifecycle.start" {
			registrations++
			if len(call.Args) != 1 {
				test.Error("API lifecycle registration must retain one explicit owner")
			} else {
				method := workerSelectorPath(call.Args[0])
				document := strings.HasPrefix(method, "documentWorkers.") || method == "worker.Run"
				if wave48Inside(call, documents) != document {
					test.Error("API changes ownership of the original five document bindings", method)
				}
			}
		}
		if path == "serverruntime.Serve" && wave48Inside(call, external) {
			test.Error("external-worker API must retain its HTTP serving path")
		}
		return true
	})
	if registrations != 18 {
		test.Error("API must keep its existing eighteen registrations directly in serve", registrations)
	}
}
