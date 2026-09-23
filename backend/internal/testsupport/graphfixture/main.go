//go:build graphfixture

// graphfixture lets browser contract tests consume the actual domain projection
// without duplicating business rules in a JavaScript fixture. It is not shipped.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/graphwire"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func main() {
	var input struct {
		Snapshot domain.ContentHash    `json:"snapshot"`
		Mode     string                `json:"mode"`
		View     domain.RepositoryView `json:"view"`
		Position string                `json:"position"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		fail(err)
	}
	if input.Mode == "memory-positions" {
		if err := json.NewEncoder(os.Stdout).Encode(domain.ResolveMemoryPositions(input.Snapshot, input.Position, input.View.History)); err != nil {
			fail(err)
		}
		return
	}
	v := input.View
	v.Semantics = domain.ProjectContextSemantics(v.Snapshots, v.History)
	if input.Mode == "semantics" {
		if err := json.NewEncoder(os.Stdout).Encode(v); err != nil {
			fail(err)
		}
		return
	}
	var err error
	v.Refs, err = domain.ProjectBranchLifecycleRefs(v.Refs)
	if err != nil {
		fail(err)
	}
	g, err := domain.ProjectGraphState(v, v.DefaultBranch, input.Position)
	if err != nil {
		fail(err)
	}
	if strings.HasPrefix(input.Mode, "integrations") && input.View.Graph != nil {
		g.BranchContexts = input.View.Graph.BranchContexts
		for name, c := range g.BranchContexts {
			g.BranchSnapshots[name] = c.SnapshotIDs
		}
		domain.ApplyGraphIntegrations(&g, v.Snapshots)
	}
	v.Graph = &g
	var result any = v
	if strings.Contains(input.Mode, "wire") {
		var encoded any = graphwire.Encode(g)
		if strings.HasSuffix(input.Mode, "-v2") {
			encoded = graphwire.EncodeV2(g)
		}
		result = struct {
			domain.RepositoryView
			Graph any `json:"graph"`
		}{v, encoded}
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fail(err)
	}
}
func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
