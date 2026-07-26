// Package codec — tool_mapping.go: Normal tool name mapping table (bidirectional).
// provider compatibility rules This is the canonical tool_name source of truth (compatibility rules).
// CIR tool_call.tool_name (canonical name) ↔ provider original name (provider_tool_name) mapping management.
package codec

import (
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Canonical tool_name vocabulary (canonical, compatibility rules):
//
//	shell, apply_patch, read_file, list_dir, grep, web_search, update_plan,
//	mcp:<server>:<tool>, unknown:<original_name>.
const (
	toolShell      = "shell"
	toolApplyPatch = "apply_patch"
	toolReadFile   = "read_file"
	toolListDir    = "list_dir"
	toolGrep       = "grep"
	toolWebSearch  = "web_search"
	toolUpdatePlan = "update_plan"
)

// canonicalToolName converts the provider original tool name to the CIR canonical tool name (provider compatibility rules = source of truth).
//
//	claude Bash→shell, Edit/MultiEdit/Write/NotebookEdit→apply_patch, Read→read_file, Glob/LS→list_dir,
//	       Grep→grep, WebSearch/WebFetch→web_search, TodoWrite→update_plan, mcp__s__t→mcp:s:t
//	codex  shell/exec/exec_command/unified_exec/write_stdin→shell, apply_patch/write_file→apply_patch,
//	       read_file/view_image→read_file, list_dir→list_dir, grep→grep,
//	       update_plan→update_plan, web_search→web_search
//	unknown   → unknown:<original_name>
func canonicalToolName(providerName string) string {
	switch providerName {
	case "Bash":
		return toolShell
	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		return toolApplyPatch
	case "Read":
		return toolReadFile
	case "Glob", "LS":
		return toolListDir
	case "Grep":
		return toolGrep
	case "WebSearch", "WebFetch":
		return toolWebSearch
	case "TodoWrite":
		return toolUpdatePlan
	// codex original name (empirically verified: exec_command is most frequent, write_stdin is exec session stdin injection)
	case "shell", "exec", "exec_command", "unified_exec", "write_stdin":
		return toolShell
	case "apply_patch", "write_file":
		return toolApplyPatch
	case "read_file", "view_image":
		return toolReadFile
	case "list_dir":
		return toolListDir
	case "grep":
		return toolGrep
	case "update_plan":
		return toolUpdatePlan
	case "web_search":
		return toolWebSearch
	}
	// MCP tool: mcp__<server>__<tool> → mcp:<server>:<tool>
	if rest, ok := strings.CutPrefix(providerName, "mcp__"); ok {
		if server, tool, found := strings.Cut(rest, "__"); found {
			return "mcp:" + server + ":" + tool
		}
		return "mcp:" + rest
	}
	// Unknown tool: preserved as unknown:<original_name> (provider compatibility rules)
	return "unknown:" + providerName
}

// providerToolName converts the CIR canonical tool name to the target provider's original tool name (for cross-encoding).
//
// Note (provider compatibility rules): For the same provider encoding, the preserved provider_tool_name is prioritized to ensure lossless conversion,
// and this reverse mapping is only used for the best-effort downgrade of **cross-provider encodings**.
// N:1 mapping (claude Edit/MultiEdit/Write→apply_patch) is downgraded to the representative value (Edit).
func providerToolName(provider domain.ProviderKind, canonical string) string {
	// mcp:<server>:<tool> reverse transformation
	if rest, ok := strings.CutPrefix(canonical, "mcp:"); ok {
		if server, tool, found := strings.Cut(rest, ":"); found {
			if provider == domain.ProviderClaude {
				return "mcp__" + server + "__" + tool
			}
			return server + "__" + tool
		}
		return rest
	}
	// unknown:<original title> → original text recovery
	if orig, ok := strings.CutPrefix(canonical, "unknown:"); ok {
		return orig
	}
	switch provider {
	case domain.ProviderClaude:
		switch canonical {
		case toolShell:
			return "Bash"
		case toolApplyPatch:
			return "Edit" // N:1 fallback default value
		case toolReadFile:
			return "Read"
		case toolListDir:
			return "LS"
		case toolGrep:
			return "Grep"
		case toolWebSearch:
			return "WebSearch"
		case toolUpdatePlan:
			return "TodoWrite"
		}
	case domain.ProviderCodex:
		switch canonical {
		case toolShell, toolReadFile, toolListDir, toolGrep:
			return "shell" // codex is promoted to shell for file operations (provider compatibility rules)
		case toolApplyPatch:
			return "apply_patch"
		case toolUpdatePlan:
			return "update_plan"
		case toolWebSearch:
			return "web_search"
		}
	}
	// If no mapping, use the regular name as is (complementary).
	return canonical
}
