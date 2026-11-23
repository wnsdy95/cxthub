package remotecfg

import (
	"context"
	"testing"
)

func observeConfig(t *testing.T, root string) Observation {
	t.Helper()
	o, e := Observe(context.Background(), root)
	if e != nil {
		t.Fatal(e)
	}
	return o
}
