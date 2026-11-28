package remotecfg

import (
	"context"
	"encoding/json"
)

// Fixture-only full-map replacement always reads an explicit CAS expectation.
// Commands use Add and Remove, so a stale caller-owned map never overwrites peers.
func configFixtureSave(root string, remotes Remotes) error {
	ctx := context.Background()
	observed, err := Observe(ctx, root)
	if err != nil {
		return err
	}
	canonical, err := canonicalRemotes(remotes)
	if err != nil {
		return err
	}
	return mutate(ctx, observed.root, &observed, func(fields map[string]json.RawMessage, _ Observation) error {
		fields["remotes"], _ = json.Marshal(canonical)
		return nil
	})
}
