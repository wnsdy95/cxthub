package mcp

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

var (
	mcpExecution  = regexp.MustCompile(`\*\*Execution\*\*: MCP read via \x60([a-z_]+)\x60`)
	legacyToolRef = regexp.MustCompile(`\b(?:repo_init|session_save|session_list|session_fork|session_checkout|session_load|session_diff|memory_save|sync_push|sync_pull)\b`)
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve integration contract test path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../../../../.."))
}

func actualMCPTools(t *testing.T) []string {
	t.Helper()
	tools := toolDefs()
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		name, ok := tool["name"].(string)
		if !ok || name == "" {
			t.Fatalf("invalid MCP tool definition: %#v", tool)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func TestIntegrationAssetsMatchReadOnlyMCPContract(t *testing.T) {
	wantTools := []string{"context_fetch", "context_list", "context_search", "memory_load"}
	if got := actualMCPTools(t); !reflect.DeepEqual(got, wantTools) {
		t.Fatalf("MCP tools = %v, want read-only contract %v", got, wantTools)
	}
	if len(serverInstructions) > 512 {
		t.Fatalf("MCP instructions are %d bytes; keep the first 512 bytes self-contained", len(serverInstructions))
	}

	root := repoRoot(t)
	for _, config := range []string{
		"integrations/codex/config.snippet.toml",
		"integrations/claude-code/.mcp.json",
	} {
		body, err := os.ReadFile(filepath.Join(root, config))
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		if !strings.Contains(text, "https://cxthub.com/mcp") {
			t.Errorf("%s does not use the cloud MCP endpoint", config)
		}
		if strings.Contains(text, `args = ["mcp"]`) || strings.Contains(text, `"args": ["mcp"]`) {
			t.Errorf("%s still makes local stdio the product default", config)
		}
	}
	allowed := make(map[string]bool, len(wantTools))
	for _, name := range wantTools {
		allowed[name] = true
	}

	for _, readme := range []string{
		"integrations/codex/README.md",
		"integrations/claude-code/README.md",
	} {
		body, err := os.ReadFile(filepath.Join(root, readme))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range wantTools {
			if !strings.Contains(string(body), "`"+name+"`") {
				t.Errorf("%s does not document %s", readme, name)
			}
		}
	}

	expected := map[string]string{
		"cxt-init.md":        "cli:init",
		"cxt-save.md":        "cli:save",
		"cxt-list.md":        "mcp:context_list",
		"cxt-log.md":         "mcp:context_list",
		"cxt-fetch.md":       "mcp:context_fetch",
		"cxt-search.md":      "mcp:context_search",
		"cxt-memory-load.md": "mcp:memory_load",
		"cxt-fork.md":        "cli:fork",
		"cxt-checkout.md":    "cli:checkout",
		"cxt-load.md":        "cli:load",
		"cxt-memorize.md":    "cli:memorize",
		"cxt-push.md":        "cli:push",
		"cxt-pull.md":        "cli:pull",
	}
	for _, relDir := range []string{
		"integrations/codex/prompts",
		"integrations/claude-code/commands",
	} {
		entries, err := os.ReadDir(filepath.Join(root, relDir))
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasPrefix(entry.Name(), "cxt-") || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			contract, ok := expected[entry.Name()]
			if !ok {
				t.Errorf("%s contains undeclared integration command %s", relDir, entry.Name())
				continue
			}
			seen[entry.Name()] = true
			body, err := os.ReadFile(filepath.Join(root, relDir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			text := string(body)
			if strings.Count(text, "**Execution**:") != 1 {
				t.Errorf("%s/%s must declare exactly one execution boundary", relDir, entry.Name())
			}
			kind, value, _ := strings.Cut(contract, ":")
			switch kind {
			case "mcp":
				match := mcpExecution.FindStringSubmatch(text)
				if len(match) != 2 || match[1] != value || !allowed[value] {
					t.Errorf("%s/%s MCP declaration = %v, want %s", relDir, entry.Name(), match, value)
				}
			case "cli":
				if !strings.Contains(text, "**Execution**: explicit `cxt` CLI") ||
					!strings.Contains(text, "`cxt "+value) {
					t.Errorf("%s/%s must route through explicit cxt %s", relDir, entry.Name(), value)
				}
			}
		}
		for name := range expected {
			if !seen[name] {
				t.Errorf("%s is missing %s", relDir, name)
			}
		}
	}

	err := filepath.WalkDir(filepath.Join(root, "integrations"), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if match := legacyToolRef.Find(body); match != nil {
			t.Errorf("%s advertises nonexistent MCP tool %s", filepath.Base(path), match)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
