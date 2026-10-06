package main

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
)

// Fixture-only full-map replacement always reads an explicit CAS expectation.
func configFixtureSave(root string, remotes remotecfg.Remotes) error {
	observed, err := remotecfg.Observe(context.Background(), root)
	if err != nil {
		return err
	}
	return remotecfg.Replace(context.Background(), observed, remotes)
}
