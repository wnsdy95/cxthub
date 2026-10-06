package remotecfg

import (
	"context"
)

// Fixture-only full-map replacement always reads an explicit CAS expectation.
func configFixtureSave(root string, remotes Remotes) error {
	observed, err := Observe(context.Background(), root)
	if err != nil {
		return err
	}
	return Replace(context.Background(), observed, remotes)
}
