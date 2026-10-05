package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Copy mutable receipt fields while retaining the non-serializable prompt
// reservation. Deserializing a stored receipt alone never authorizes delivery.
func cloneAgentContextPackage(p domain.AgentContextPackage) (domain.AgentContextPackage, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return domain.AgentContextPackage{}, err
	}
	var copy domain.AgentContextPackage
	if err := json.Unmarshal(raw, &copy); err != nil {
		return copy, err
	}
	copy.BindInitialPrompt(p.InitialPromptReservation())
	return copy, nil
}

func persistAgentInputPackage(ctx context.Context, root string, p domain.AgentContextPackage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := p.Artifact()
	if err != nil {
		return err
	}
	return providerfs.WriteRepoFileDurable(root, filepath.Join(".cxt", "input-packages", strings.TrimPrefix(string(p.ID), "sha256:")+".json"), raw, 0600)
}
