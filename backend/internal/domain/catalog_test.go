package domain

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCatalogRequestValidation(t *testing.T) {
	checkpoint := CatalogCheckpoint{Version: 1, RepoID: HashContent([]byte("catalog repo")), Epoch: "e66213bd-003e-47d1-b965-e690036bfb08", Sequence: 0}
	for _, tc := range []struct {
		name   string
		mutate func(*CatalogRequest)
		want   error
	}{
		{"baseline", func(r *CatalogRequest) {}, nil},
		{"default_limit", func(r *CatalogRequest) { r.Limit = 0 }, nil},
		{"max_limit", func(r *CatalogRequest) { r.Limit = MaxCatalogLimit }, nil},
		{"opaque_cursor", func(r *CatalogRequest) { r.Cursor = "adapter-owned" }, nil},
		{"checkpoint_zero", func(r *CatalogRequest) { r.After = &checkpoint }, nil},
		{"version_missing", func(r *CatalogRequest) { r.Version = 0 }, ErrValidation},
		{"version_unknown", func(r *CatalogRequest) { r.Version = 2 }, ErrValidation},
		{"negative_limit", func(r *CatalogRequest) { r.Limit = -1 }, ErrValidation},
		{"oversized_limit", func(r *CatalogRequest) { r.Limit = 1001 }, ErrValidation},
		{"exclusive", func(r *CatalogRequest) { r.After = &checkpoint; r.Cursor = "cursor" }, ErrValidation},
		{"checkpoint_version", func(r *CatalogRequest) { c := checkpoint; c.Version = 2; r.After = &c }, ErrCatalogResetRequired},
		{"checkpoint_repo", func(r *CatalogRequest) { c := checkpoint; c.RepoID = "../repo"; r.After = &c }, ErrValidation},
		{"checkpoint_sequence", func(r *CatalogRequest) { c := checkpoint; c.Sequence = -1; r.After = &c }, ErrValidation},
		{"epoch_empty", func(r *CatalogRequest) { c := checkpoint; c.Epoch = ""; r.After = &c }, ErrValidation},
		{"epoch_invalid_hex", func(r *CatalogRequest) {
			c := checkpoint
			c.Epoch = "g66213bd-003e-47d1-b965-e690036bfb08"
			r.After = &c
		}, ErrValidation},
		{"epoch_invalid_separator", func(r *CatalogRequest) {
			c := checkpoint
			c.Epoch = strings.ReplaceAll(c.Epoch, "-", "_")
			r.After = &c
		}, ErrValidation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := CatalogRequest{Version: CatalogVersion}
			tc.mutate(&r)
			if err := r.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate()=%v, want %v", err, tc.want)
			}
		})
	}
}

func TestCatalogRawMetadataAndProgressRoundTrip(t *testing.T) {
	// These synthetic future history fields and raw lifecycle tags must survive
	// transport without narrowing values through projected snapshot/ref DTOs.
	entries := []CatalogEntry{
		{Sequence: 4, Kind: "ref", Key: `["tag","lifecycle/raw"]`, Value: json.RawMessage(`{"kind":"tag","name":"lifecycle/raw","branch_id":"identity","future":{"ordered":[2,1]}}`)},
		{Sequence: 4, Kind: "history", Key: "event", Value: json.RawMessage(`{"id":"event","parents":["b","a"],"future":true}`)},
		{Sequence: 5, Kind: "snapshot", Key: "gone", Deleted: true},
	}
	page := CatalogPage{Version: 1, RepoID: HashContent([]byte("catalog repo")), Epoch: "e66213bd-003e-47d1-b965-e690036bfb08", Through: 5, Mode: "delta", Entries: entries, NextCursor: "opaque"}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"checkpoint"`) {
		t.Fatal("partial page published a checkpoint")
	}
	var got CatalogPage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(page, got) {
		t.Fatalf("metadata changed: %s", raw)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	tombstone := wire["entries"].([]any)[2].(map[string]any)
	if _, present := tombstone["value"]; present {
		t.Fatal("tombstone included a value")
	}
	page.NextCursor = ""
	page.Checkpoint = &CatalogCheckpoint{Version: 1, RepoID: page.RepoID, Epoch: page.Epoch, Sequence: page.Through}
	raw, err = json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"next_cursor"`) || !strings.Contains(string(raw), `"checkpoint"`) {
		t.Fatalf("final progress: %s", raw)
	}
}
