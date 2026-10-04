//go:build darwin || linux

package nativecodex

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// Compare the versioned template with the installed native serializer, using
// only isolated fixture homes and a loopback provider that forbids inference.
// The first process is unseeded; the second explicitly requests an empty TUI
// table solely to observe native's own default serialization, without a TUI turn.
func TestWindowConfigNativeSerdeDefaults(t *testing.T) {
	opts, path := staticWindowNativeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	read := func(opts Options) (map[string]json.RawMessage, string) {
		t.Helper()
		s, err := Start(ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if !SupportedHostIdentity(s.HostIdentity()) {
			t.Fatal("unexpected native version")
		}
		config, hash, err := s.windowConfig(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		return config, hash
	}
	unseeded, before := read(opts)
	if value, present := unseeded["tui"]; present && string(value) != "null" {
		t.Fatal("native fixture unexpectedly seeded TUI configuration")
	}
	opts.ConfigArgs = append(opts.ConfigArgs, "-c", "tui={}")
	expanded, after := read(opts)
	actual, err := windowConfigValue(expanded["tui"], 0)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := windowTUI01571().normalize(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatal("installed native TUI defaults differ from the pinned schema")
	}
	if before != after {
		t.Fatal("empty TUI table changed normalized config fingerprint")
	}
	t.Log("unseeded and native expanded TUI defaults agree; provider calls=0")
}
