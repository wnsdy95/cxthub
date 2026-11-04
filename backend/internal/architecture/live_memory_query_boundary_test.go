package architecture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLiveMemoryAndFrozenCompilerHaveSingleActualOwners(test *testing.T) {
	for _, name := range []string{"live_effective.go", "live_integration.go", "live_branch.go", "live_delivery.go", "contributions.go", "contributions_branch.go", "serving.go", "compiler.go"} {
		raw, err := os.ReadFile(filepath.Join("..", "app", "memoryquery", name))
		if err != nil {
			test.Fatal(err)
		}
		if violations := preparedMemoryBoundaryViolations(raw); len(violations) != 0 {
			test.Fatal(name, violations)
		}
	}
	for _, name := range []string{"memory_integration.go", "memory_read_assessment.go"} {
		if _, err := os.Stat(filepath.Join("..", "app", name)); !os.IsNotExist(err) {
			test.Fatal("root retains a duplicate live calculation owner", name, err)
		}
	}
	for _, name := range []string{"memory_captured.go", "memory_capture_bundle.go", "memory_read.go", "memory_read_comparison.go"} {
		raw, err := os.ReadFile(filepath.Join("..", "app", name))
		if err != nil {
			test.Fatal(err)
		}
		if strings.Contains(string(raw), "&Service{") || strings.Contains(string(raw), "NewService(") {
			test.Error("frozen calculation or reference reconstructs the broad service", name)
		}
	}
	for _, name := range []string{"memory_admission.go", "branch_memory.go"} {
		raw, err := os.ReadFile(filepath.Join("..", "adapters", "delivery", "mcp", name))
		if err != nil {
			test.Fatal(err)
		}
		if strings.Contains(string(raw), ".(inbound.MemoryReadScope)") || strings.Contains(string(raw), ".(inbound.BranchMemoryQuery)") {
			test.Error("MCP rediscovers a live query or resource scope from the broad backend", name)
		}
	}
}
