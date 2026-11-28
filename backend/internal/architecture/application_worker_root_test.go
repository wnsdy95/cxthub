package architecture

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

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
