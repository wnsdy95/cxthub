package main

import (
	"context"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
)

// configFixtureSave replaces the whole remote list through the same Remove and
// Add calls the commands use, each against a fresh observation.
func configFixtureSave(root string, remotes remotecfg.Remotes) error {
	ctx := context.Background()
	observed, err := remotecfg.Observe(ctx, root)
	if err != nil {
		return err
	}
	current, err := observed.Remotes()
	if err != nil {
		return err
	}
	for name := range current {
		if err := remotecfg.Remove(ctx, observed, name); err != nil {
			return err
		}
		if observed, err = remotecfg.Observe(ctx, root); err != nil {
			return err
		}
	}
	for name, url := range remotes {
		if err := remotecfg.Add(ctx, observed, name, url, nil); err != nil {
			return err
		}
		if observed, err = remotecfg.Observe(ctx, root); err != nil {
			return err
		}
	}
	return nil
}
