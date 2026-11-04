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

func TestMinimumInfrastructureDoesNotConstructApplicationRuntime(test *testing.T) {
	module, err := readGoModule(filepath.Join("..", ".."))
	if err != nil {
		test.Fatal(err)
	}
	path := filepath.Join(module.directory, "internal", "serverruntime", "infrastructure.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		test.Fatal(err)
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.GoStmt:
			test.Error("minimum infrastructure starts a background goroutine")
		case *ast.StarExpr:
			if selector, ok := value.X.(*ast.SelectorExpr); ok && (selector.Sel.Name == "Service" || selector.Sel.Name == "IdentityService") {
				test.Error("minimum infrastructure retains application services")
			}
		case *ast.CallExpr:
			if selector, ok := value.Fun.(*ast.SelectorExpr); ok {
				switch selector.Sel.Name {
				case "NewService", "NewIdentityService", "NewReadServices", "NewFirebaseVerifier", "NewDevVerifier":
					test.Error("minimum infrastructure constructs", selector.Sel.Name)
				}
			}
		}
		return true
	})
	file, err = parser.ParseFile(token.NewFileSet(), filepath.Join(module.directory, "internal", "serverruntime", "runtime.go"), nil, 0)
	if err != nil {
		test.Fatal(err)
	}
	sharedOpen := 0
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "OpenInfrastructure" {
			sharedOpen++
		}
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
			if owner, ok := selector.X.(*ast.Ident); ok && owner.Name == "store" && selector.Sel.Name == "Open" {
				test.Error("API/MCP runtime bypasses shared infrastructure")
			}
		}
		return true
	})
	if sharedOpen != 1 {
		test.Fatal("API/MCP runtime must share one infrastructure opening", sharedOpen)
	}
}

func TestDisplayMaintenanceOwnerDoesNotDependOnCommandRuntime(test *testing.T) {
	module, err := readGoModule(filepath.Join("..", ".."))
	if err != nil {
		test.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(module.directory, "internal", "app", "displaymaintenance", "*.go"))
	if err != nil || len(paths) == 0 {
		test.Fatal("actual display maintenance owner is missing", err)
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			test.Fatal(err)
		}
		for _, dependency := range file.Imports {
			name, err := strconv.Unquote(dependency.Path.Value)
			if err != nil {
				test.Fatal(err)
			}
			if name == module.path+"/internal/app" || strings.Contains(name, "/internal/adapters/") || strings.Contains(name, "/internal/serverruntime") {
				test.Error(filepath.Base(path), "depends on command/infrastructure composition", name)
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if pointer, ok := node.(*ast.StarExpr); ok {
				if name, ok := pointer.X.(*ast.Ident); ok && name.Name == "Service" {
					test.Error(filepath.Base(path), "retains command Service")
				}
				if selector, ok := pointer.X.(*ast.SelectorExpr); ok && selector.Sel.Name == "Service" {
					test.Error(filepath.Base(path), "retains qualified command Service")
				}
			}
			return true
		})
	}
}

func TestAPIStartupDoesNotLaunchUnownedWorkers(test *testing.T) {
	module, err := readGoModule(filepath.Join("..", ".."))
	if err != nil {
		test.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(module.directory, "cmd", "cxtd", "main.go"), nil, 0)
	if err != nil {
		test.Fatal(err)
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "serve" {
			continue
		}
		registered := make(map[string]int)
		var registrationOrder []string
		maintenanceOwner := false
		deferredClose := false
		documentOwners := 0
		projectionOwners := 0
		notificationOwners := 0
		systemMaintenanceOwners := 0
		gitChangeOwners := 0
		gitIntegrationOwners := 0
		memoryPreparationOwners := 0
		prPromotionOwners := 0
		invitationEmailOwners := 0
		githubOwners := 0
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if _, ok := node.(*ast.GoStmt); ok {
				test.Error("API serve bypasses joined worker lifecycle")
			}
			if deferred, ok := node.(*ast.DeferStmt); ok {
				if selector, ok := deferred.Call.Fun.(*ast.SelectorExpr); ok {
					if owner, ok := selector.X.(*ast.Ident); ok && owner.Name == "lifecycle" && selector.Sel.Name == "close" {
						deferredClose = true
					}
				}
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			owner, ok := selector.X.(*ast.Ident)
			if !ok {
				return true
			}
			maintenanceOwner = maintenanceOwner || owner.Name == "app" && selector.Sel.Name == "NewDisplayMaintenanceWorker"
			if owner.Name == "svc" && selector.Sel.Name == "DocumentWorkersForProfile" {
				documentOwners++
			}
			if owner.Name == "svc" && selector.Sel.Name == "ProjectionWorkersForProfile" {
				projectionOwners++
			}
			if owner.Name == "app" && selector.Sel.Name == "NewNotificationWorker" {
				notificationOwners++
			}
			if owner.Name == "app" && selector.Sel.Name == "NewMaintenanceWorkers" {
				systemMaintenanceOwners++
			}
			if owner.Name == "app" && selector.Sel.Name == "NewGitChangeWorker" {
				gitChangeOwners++
			}
			if owner.Name == "app" && selector.Sel.Name == "NewGitIntegrationWorkers" {
				gitIntegrationOwners++
			}
			if owner.Name == "app" && selector.Sel.Name == "NewMemoryPreparationWorkers" {
				memoryPreparationOwners++
			}
			if owner.Name == "app" && selector.Sel.Name == "NewPRPromotionWorker" {
				prPromotionOwners++
			}
			if owner.Name == "app" && selector.Sel.Name == "NewInvitationEmailWorker" {
				invitationEmailOwners++
				if len(call.Args) != 1 {
					test.Error("API invitation email owner must use the identity service's worker configuration")
				} else if configuration, ok := call.Args[0].(*ast.CallExpr); !ok || workerSelectorPath(configuration.Fun) != "idSvc.InvitationEmailWorkerConfig" || len(configuration.Args) != 1 {
					test.Error("API invitation email owner bypasses the identity service's worker configuration")
				}
			}
			if owner.Name == "app" && selector.Sel.Name == "NewGitHubWorker" {
				githubOwners++
			}
			if owner.Name == "lifecycle" && selector.Sel.Name == "start" && len(call.Args) == 1 {
				method := workerSelectorPath(call.Args[0])
				if method == "" {
					test.Error("API worker registration is not an explicit named owner")
				}
				registered[method]++
				registrationOrder = append(registrationOrder, method)
			}
			return true
		})
		for _, method := range []string{
			"githubWorker.Run", "gitChangeWorker.Run", "gitIntegrationWorkers.Run", "gitIntegrationWorkers.RunReconciler",
			"documentWorkers.Finalization.Run", "documentWorkers.Range.Run", "documentWorkers.FindFields.Run",
			"documentWorkers.Display.Run", "worker.Run", "projectionWorkers.Pending.Run",
			"projectionWorkers.Graph.Run", "memoryPreparations.Run",
			"projectionWorkers.Qualification.Run", "prPromotionWorker.Run", "notificationWorker.Run",
			"invitationEmailWorker.Run", "maintenanceWorkers.Runtime.Run", "maintenanceWorkers.Storage.Run",
		} {
			if registered[method] != 1 {
				test.Error("API worker must be registered exactly once", method, registered[method])
			}
		}
		if len(registered) != 18 {
			test.Error("API registered an unexpected additional worker", registered)
		}
		for position, method := range registrationOrder {
			if method == "githubWorker.Run" && (position != 0 || position+1 >= len(registrationOrder) || registrationOrder[position+1] != "gitChangeWorker.Run") {
				test.Error("API GitHub owner moved from its original optional lifecycle slot", registrationOrder)
			}
			if method == "invitationEmailWorker.Run" && (position == 0 || position+1 >= len(registrationOrder) || registrationOrder[position-1] != "notificationWorker.Run" || registrationOrder[position+1] != "maintenanceWorkers.Runtime.Run") {
				test.Error("API invitation email owner moved from its original lifecycle slot", registrationOrder)
			}
		}
		if !maintenanceOwner || !deferredClose || documentOwners != 1 || projectionOwners != 1 || notificationOwners != 1 || systemMaintenanceOwners != 1 || gitChangeOwners != 1 || gitIntegrationOwners != 1 || memoryPreparationOwners != 1 || prPromotionOwners != 1 || invitationEmailOwners != 1 || githubOwners != 1 {
			test.Error("API does not own concrete worker construction and deferred worker drain", maintenanceOwner, documentOwners, projectionOwners, notificationOwners, systemMaintenanceOwners, gitChangeOwners, gitIntegrationOwners, memoryPreparationOwners, prPromotionOwners, invitationEmailOwners, githubOwners, deferredClose)
		}
		return
	}
	test.Fatal("actual API serve entrypoint is missing")
}

func TestAPIGitHubWorkerUsesOneOptionalPublicLifecycleOwner(test *testing.T) {
	module, err := readGoModule(filepath.Join("..", ".."))
	if err != nil {
		test.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(module.directory, "cmd", "cxtd", "main.go"), nil, 0)
	if err != nil {
		test.Fatal(err)
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "serve" {
			continue
		}
		guards := make(map[string][]*ast.BlockStmt)
		configurations := make(map[string]*ast.CallExpr)
		assignedOwners := make(map[*ast.CallExpr]bool)
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if conditional, ok := node.(*ast.IfStmt); ok {
				if condition, ok := conditional.Cond.(*ast.BinaryExpr); ok && condition.Op == token.NEQ {
					left, right := workerSelectorPath(condition.X), workerSelectorPath(condition.Y)
					if right == "nil" {
						guards[left] = append(guards[left], conditional.Body)
					} else if left == "nil" {
						guards[right] = append(guards[right], conditional.Body)
					}
				}
			}
			assignment, ok := node.(*ast.AssignStmt)
			if !ok || len(assignment.Rhs) != 1 || len(assignment.Lhs) == 0 {
				return true
			}
			call, ok := assignment.Rhs[0].(*ast.CallExpr)
			if !ok {
				return true
			}
			switch workerSelectorPath(call.Fun) {
			case "githubConnections.GitHubWorkerConfig":
				configurations[workerSelectorPath(assignment.Lhs[0])] = call
			case "app.NewGitHubWorker":
				assignedOwners[call] = workerSelectorPath(assignment.Lhs[0]) == "githubWorker"
			}
			return true
		})
		guardedBy := func(node ast.Node, owner string) bool {
			for _, block := range guards[owner] {
				if node.Pos() > block.Pos() && node.End() < block.End() {
					return true
				}
			}
			return false
		}
		constructors, configurationCalls, bindings := 0, 0, 0
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch workerSelectorPath(call.Fun) {
			case "githubConnections.GitHubWorkerConfig":
				configurationCalls++
				if len(call.Args) != 1 || !guardedBy(call, "githubConnections") {
					test.Error("API must copy the profiled GitHub worker configuration only when GitHub is enabled")
				}
			case "app.NewGitHubWorker":
				constructors++
				if !assignedOwners[call] || !guardedBy(call, "githubConnections") {
					test.Error("API must construct the named public GitHub owner only when GitHub is enabled")
				}
				if len(call.Args) != 1 {
					test.Error("API GitHub owner must consume one copied worker configuration")
					return true
				}
				configuration, inline := call.Args[0].(*ast.CallExpr)
				if !inline {
					configuration = configurations[workerSelectorPath(call.Args[0])]
				}
				if configuration == nil || workerSelectorPath(configuration.Fun) != "githubConnections.GitHubWorkerConfig" || len(configuration.Args) != 1 {
					test.Error("API GitHub owner bypasses the connection's copied worker configuration")
				}
			case "lifecycle.start":
				if len(call.Args) == 1 && workerSelectorPath(call.Args[0]) == "githubWorker.Run" {
					bindings++
					if !guardedBy(call, "githubConnections") && !guardedBy(call, "githubWorker") {
						test.Error("API must keep the public GitHub owner in an optional lifecycle slot")
					}
				}
			}
			return true
		})
		if constructors != 1 || configurationCalls != 1 || bindings != 1 {
			test.Error("API must have exactly one public GitHub constructor, copied configuration, and optional Run binding", constructors, configurationCalls, bindings)
		}
		return
	}
	test.Fatal("actual API serve entrypoint is missing")
}

func workerSelectorPath(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		if owner := workerSelectorPath(value.X); owner != "" {
			return owner + "." + value.Sel.Name
		}
	}
	return ""
}

func TestDocumentWorkerOwnersDoNotCaptureCommandServices(test *testing.T) {
	module, err := readGoModule(filepath.Join("..", ".."))
	if err != nil {
		test.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(module.directory, "internal", "app", "documentworker", "*.go"))
	if err != nil || len(paths) == 0 {
		test.Fatal("actual document worker owners are missing", err)
	}
	paths = append(paths, filepath.Join(module.directory, "internal", "app", "document_worker_scope.go"), filepath.Join(module.directory, "internal", "app", "document_workers.go"))
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			test.Fatal(err)
		}
		for _, dependency := range file.Imports {
			name, err := strconv.Unquote(dependency.Path.Value)
			if err != nil {
				test.Fatal(err)
			}
			if name == module.path+"/internal/app" || strings.Contains(name, "/internal/adapters/") || strings.Contains(name, "/internal/serverruntime") {
				test.Error(filepath.Base(path), "depends on command/infrastructure composition", name)
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "NewService" {
					test.Error(filepath.Base(path), "constructs a partial command service")
				}
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "NewService" {
					test.Error(filepath.Base(path), "constructs a qualified command service")
				}
			}
			if fields, ok := node.(*ast.StructType); ok {
				ast.Inspect(fields, func(field ast.Node) bool {
					if pointer, ok := field.(*ast.StarExpr); ok && strings.HasSuffix(workerSelectorPath(pointer.X), "Service") {
						test.Error(filepath.Base(path), "retains a command service")
					}
					return true
				})
			}
			return true
		})
	}
}
