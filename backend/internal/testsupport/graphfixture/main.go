//go:build graphfixture

// graphfixture lets browser contract tests consume the actual domain projection
// without duplicating business rules in a JavaScript fixture. It is not shipped.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/graphwire"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func main() {
	var input struct {
		Mode     string                `json:"mode"`
		View     domain.RepositoryView `json:"view"`
		Position string                `json:"position"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		fail(err)
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
	v.Graph = &g
	var result any = v
	if input.Mode == "wire" {
		result = struct {
			domain.RepositoryView
			Graph graphwire.State `json:"graph"`
		}{v, graphwire.Encode(g)}
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fail(err)
	}
}
func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
