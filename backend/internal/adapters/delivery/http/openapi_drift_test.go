package http

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestOpenAPIDrift checks if all routes registered with the mux are documented in schemas/openapi.yaml (including reverse routes — phantom paths that exist only in the spec also fail). When routes are added or removed, the spec must be updated accordingly to pass: a guard against silently outdated contract documents.
//
// Comparison rules: path parameters are considered the same if their names differ (e.g., {repoID}≡{id}≡{}), and the remaining matching in go 1.22's {name...} is normalized to {name}.
func TestOpenAPIDrift(t *testing.T) {
	registered := routesFromSource(t, "server.go", "identity.go")
	specced := routesFromSpec(t, "../../../../../schemas/openapi.yaml")

	for r := range registered {
		if !specced[r] {
			t.Errorf("Spec missing: %s (add to schemas/openapi.yaml)", r)
		}
	}
	for r := range specced {
		if !registered[r] {
			t.Errorf("Phantom spec: %s (route not found on server — remove from spec or implement)", r)
		}
	}
	if len(registered) == 0 || len(specced) == 0 {
		t.Fatalf("Parsing failed: registered=%d, specced=%d", len(registered), len(specced))
	}
}

var handleFuncRe = regexp.MustCompile(`mux\.HandleFunc\("([A-Z]+) (/api/v1[^"]*)"`)

// routesFromSource collects mux.HandleFunc("METHOD /api/v1/...") patterns from the source.
func routesFromSource(t *testing.T, files ...string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range handleFuncRe.FindAllStringSubmatch(string(b), -1) {
			out[m[1]+" "+normalizePath(strings.TrimPrefix(m[2], "/api/v1"))] = true
		}
	}
	return out
}

var specPathRe = regexp.MustCompile(`^  (/\S+):\s*$`)
var specMethodRe = regexp.MustCompile(`^    (get|post|put|patch|delete):\s*$`)

// routesFromSpec collects (METHOD, path) pairs from the paths section of openapi.yaml. It reads using indentation rules (paths are 2 spaces, methods are 4 spaces) without an external YAML parser (stdlib only).
func routesFromSpec(t *testing.T, specPath string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	out := map[string]bool{}
	inPaths := false
	current := ""
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case strings.HasPrefix(line, "paths:"):
			inPaths = true
		case inPaths && len(line) > 0 && line[0] != ' ' && line[0] != '#':
			inPaths = false // end at the next top-level key (e.g., tags:)
		}
		if !inPaths {
			continue
		}
		if m := specPathRe.FindStringSubmatch(line); m != nil {
			current = normalizePath(m[1])
			continue
		}
		if m := specMethodRe.FindStringSubmatch(line); m != nil && current != "" {
			out[strings.ToUpper(m[1])+" "+current] = true
		}
	}
	return out
}

var paramRe = regexp.MustCompile(`\{[^}]*\}`)

// normalizePath normalizes path parameter names (e.g., {repoID}≡{id}) and {name...} to {}.
func normalizePath(p string) string {
	return paramRe.ReplaceAllString(p, "{}")
}
