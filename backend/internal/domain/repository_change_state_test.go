package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestPendingProjectionStatusGenerationContract(t *testing.T) {
	checkpoint := RepositoryViewSourceCheckpoint{Version: 1, Scope: RepositoryViewSourceScope, RepoID: HashContent([]byte(t.Name())), Epoch: "12345678-abcd-4321-9876-123456789abc", Sequence: 9223372036854775807}
	projection := PendingProjectionRead{Enabled: true, Ready: true, Source: checkpoint, Applied: checkpoint, Dataset: "22345678-abcd-4321-9876-123456789abc"}
	status, err := projection.Status()
	if err != nil || status.State != "ready" || status.SourceEpoch != checkpoint.Epoch+":9223372036854775807" || ValidateContentHash(status.Generation) != nil || status.Rules != 1 {
		t.Fatal(status, err)
	}
	before := RepositoryChangeState{RepositoryRevision: RepositoryRevision{Graph: 1, Pending: 2}, PendingProjection: &status}
	copyStatus := status
	after := RepositoryChangeState{RepositoryRevision: before.RepositoryRevision, PendingProjection: &copyStatus}
	if !before.Equal(after) {
		t.Fatal("pointer identity treated as a new change")
	}
	copyStatus.SourceEpoch = checkpoint.Epoch + ":0"
	if before.Equal(after) {
		t.Fatal("source-only change ignored")
	}
	copyStatus = status
	copyStatus.Generation = HashContent([]byte("replacement dataset"))
	if before.Equal(after) || before.Equal(RepositoryChangeState{RepositoryRevision: before.RepositoryRevision}) {
		t.Fatal("projection generation/removal ignored")
	}
	for _, state := range []string{"catching_up", "needs_attention", "disabled"} {
		projection.Ready = false
		projection.NeedsAttention = state == "needs_attention"
		projection.Enabled = state != "disabled"
		got, err := projection.Status()
		if err != nil || got.State != state || got.Generation != "" || got.SourceEpoch != status.SourceEpoch {
			t.Fatal("incomplete status advertised a ready generation", got, err)
		}
	}
	projection.Enabled, projection.Ready = true, true
	projection.Applied.Sequence--
	if _, err := projection.Status(); !errors.Is(err, ErrIntegrity) {
		t.Fatal("mixed source/applied was advertised ready", err)
	}
	projection.Ready = false
	projection.Source.Epoch = "bad"
	if _, err := projection.Status(); !errors.Is(err, ErrIntegrity) {
		t.Fatal("malformed source escaped", err)
	}
}

func TestRepositoryChangeStateLegacyJSONContract(t *testing.T) {
	revision := RepositoryRevision{Graph: 3, Pending: 4, Evidence: 5}
	oldJSON, err := json.Marshal(revision)
	if err != nil {
		t.Fatal(err)
	}
	newJSON, err := json.Marshal(RepositoryChangeState{RepositoryRevision: revision})
	if err != nil || string(oldJSON) != string(newJSON) {
		t.Fatal("disabled notice changed legacy JSON", string(newJSON), err)
	}
}

func TestPendingProjectionStatusRejectsGraphSourceScope(t *testing.T) {
	checkpoint := RepositoryViewSourceCheckpoint{Version: 1, Scope: RepositoryGraphSourceScope, RepoID: HashContent([]byte(t.Name())), Epoch: "12345678-abcd-4321-9876-123456789abc", Sequence: 1}
	for _, enabled := range []bool{true, false} {
		projection := PendingProjectionRead{Enabled: enabled, Ready: enabled, Source: checkpoint, Applied: checkpoint, Dataset: "22345678-abcd-4321-9876-123456789abc"}
		if _, err := projection.Status(); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("graph source accepted as pending source: %v", err)
		}
	}
}

func graphProjectionStatusTestRead() GraphProjectionRead {
	checkpoint := RepositoryViewSourceCheckpoint{Version: 1, Scope: RepositoryGraphSourceScope, RepoID: HashContent([]byte("graph status")), Epoch: "12345678-abcd-4321-9876-123456789abc", Sequence: 9223372036854775805}
	return GraphProjectionRead{Enabled: true, Complete: true, Ready: true, Rules: GraphViewProjectionRules, Source: checkpoint, Applied: checkpoint, Dataset: "22345678-abcd-4321-9876-123456789abc"}
}

func TestGraphProjectionStatusGenerationContract(t *testing.T) {
	projection := graphProjectionStatusTestRead()
	ready, err := projection.Status()
	if err != nil || ready.State != "ready" || !ready.Current || !ready.Complete || ready.Rules != 2 || ready.SourceEpoch != projection.Source.Epoch+":9223372036854775805" || ready.AppliedEpoch != ready.SourceEpoch || ValidateContentHash(ready.Generation) != nil {
		t.Fatal("invalid ready status", ready, err)
	}
	projection.Source.Sequence++
	projection.Ready = false
	lagged, err := projection.Status()
	if err != nil || lagged.State != "catching_up" || lagged.Current || !lagged.Complete || lagged.Generation != ready.Generation || lagged.AppliedEpoch != ready.AppliedEpoch || lagged.SourceEpoch != projection.Source.Epoch+":9223372036854775806" {
		t.Fatal("lagged same-epoch generation lost identity", lagged, err)
	}
	projection.Source.Sequence++
	again, err := projection.Status()
	if err != nil || again.Generation != ready.Generation || again.SourceEpoch != projection.Source.Epoch+":9223372036854775807" {
		t.Fatal("moving source changed completed generation", again, err)
	}
	projection.Applied.Sequence++
	advanced, err := projection.Status()
	if err != nil || advanced.Generation == ready.Generation || advanced.SourceEpoch != again.SourceEpoch {
		t.Fatal("applied generation did not change", advanced, err)
	}
	projection.Dataset = "32345678-abcd-4321-9876-123456789abc"
	replaced, err := projection.Status()
	if err != nil || replaced.Generation == advanced.Generation {
		t.Fatal("replacement dataset reused generation", replaced, err)
	}
	projection.Parts = []GraphProjectionPart{{Kind: "not-a-bundle", Value: []byte("corrupt")}}
	metadata, err := projection.Status()
	if err != nil || metadata != replaced {
		t.Fatal("status unexpectedly validated or changed body metadata", metadata, err)
	}
	if _, err := DecodeGraphViewProjection(context.Background(), projection.Source.RepoID, projection.Parts); !errors.Is(err, ErrIntegrity) {
		t.Fatal("metadata status substituted for bundle validation", err)
	}
	for _, state := range []string{"disabled", "needs_attention", "catching_up"} {
		projection.Enabled = state != "disabled"
		projection.NeedsAttention = state == "needs_attention"
		got, err := projection.Status()
		if err != nil || got.State != state || !got.Complete || got.Current || got.Generation != replaced.Generation {
			t.Fatal("completed metadata was lost outside ready state", got, err)
		}
	}
	projection.Complete = false
	projection.Applied = RepositoryViewSourceCheckpoint{}
	projection.Dataset = ""
	got, err := projection.Status()
	if err != nil || got.Generation != "" || got.Complete || got.Current || got.AppliedEpoch != "" || got.SourceEpoch != again.SourceEpoch {
		t.Fatal("incomplete status advertised a generation", got, err)
	}
}

func TestGraphProjectionStatusRejectsInvalidMetadata(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*GraphProjectionRead)
	}{
		{"negative serving fence", func(read *GraphProjectionRead) { read.ServingFence = -1 }},
		{"source scope", func(read *GraphProjectionRead) { read.Source.Scope = RepositoryViewSourceScope }},
		{"source repo", func(read *GraphProjectionRead) { read.Source.RepoID = "bad" }},
		{"source epoch", func(read *GraphProjectionRead) { read.Source.Epoch = "bad" }},
		{"source sequence", func(read *GraphProjectionRead) { read.Source.Sequence = -1 }},
		{"applied scope", func(read *GraphProjectionRead) { read.Applied.Scope = RepositoryViewSourceScope }},
		{"applied repo", func(read *GraphProjectionRead) { read.Applied.RepoID = HashContent([]byte("foreign")) }},
		{"applied epoch", func(read *GraphProjectionRead) { read.Applied.Epoch = "bad" }},
		{"negative applied", func(read *GraphProjectionRead) { read.Applied.Sequence = -1 }},
		{"negative lag", func(read *GraphProjectionRead) { read.Applied.Sequence++ }},
		{"epoch rotation", func(read *GraphProjectionRead) {
			read.Source.Epoch = "32345678-abcd-4321-9876-123456789abc"
			read.Ready = false
		}},
		{"old rules", func(read *GraphProjectionRead) { read.Rules = GraphProjectionRules }},
		{"unknown rules", func(read *GraphProjectionRead) { read.Rules = GraphViewProjectionRules + 1 }},
		{"empty dataset", func(read *GraphProjectionRead) { read.Dataset = "" }},
		{"zero dataset", func(read *GraphProjectionRead) { read.Dataset = "00000000-0000-0000-0000-000000000000" }},
		{"malformed dataset", func(read *GraphProjectionRead) { read.Dataset = "invalid" }},
		{"missing applied", func(read *GraphProjectionRead) { read.Applied = RepositoryViewSourceCheckpoint{} }},
		{"ready incomplete", func(read *GraphProjectionRead) { read.Complete = false }},
		{"ready lagged", func(read *GraphProjectionRead) { read.Applied.Sequence-- }},
		{"ready disabled", func(read *GraphProjectionRead) { read.Enabled = false }},
		{"ready needs attention", func(read *GraphProjectionRead) { read.NeedsAttention = true }},
		{"invalid applied incomplete", func(read *GraphProjectionRead) { read.Ready, read.Complete = false, false; read.Applied.Sequence = -1 }},
		{"invalid source disabled", func(read *GraphProjectionRead) {
			read.Ready, read.Complete, read.Enabled = false, false, false
			read.Source.Scope = RepositoryViewSourceScope
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			read := graphProjectionStatusTestRead()
			test.change(&read)
			if status, err := read.Status(); !errors.Is(err, ErrIntegrity) || status != (GraphProjectionStatus{}) {
				t.Fatal("invalid metadata returned status", status, err)
			}
		})
	}
	read := graphProjectionStatusTestRead()
	read.Ready, read.Complete = false, false
	read.Rules = GraphProjectionRules
	read.Source.Epoch = "32345678-abcd-4321-9876-123456789abc"
	if status, err := read.Status(); err != nil || status.State != "catching_up" || status.Complete || status.Current || status.Generation != "" {
		t.Fatal("incompatible incomplete metadata claimed completion", status, err)
	}
	for _, rules := range []int{0, GraphProjectionRules} {
		read.Rules = rules
		if status, err := read.Status(); err != nil || status.Rules != GraphViewProjectionRules || status.Complete || status.Generation != "" {
			t.Fatal("incomplete notice did not advertise expected bundle rules", status, err)
		}
	}
	read = graphProjectionStatusTestRead()
	read.Ready = false
	if status, err := read.Status(); err != nil || status.Current || status.State != "catching_up" {
		t.Fatal("current flag disagrees with ready", status, err)
	}
}

func TestRepositoryChangeStateBothProjectionNotices(t *testing.T) {
	graph, err := graphProjectionStatusTestRead().Status()
	if err != nil {
		t.Fatal(err)
	}
	pending := PendingProjectionStatus{State: "ready", SourceEpoch: "pending-source", Rules: PendingProjectionRules, Generation: HashContent([]byte("pending"))}
	before := RepositoryChangeState{RepositoryRevision: RepositoryRevision{Graph: 3, Pending: 4}, GraphProjection: &graph}
	for _, includePending := range []bool{false, true} {
		if includePending {
			before.PendingProjection = &pending
		}
		copied := graph
		after := before
		after.GraphProjection = &copied
		if !before.Equal(after) {
			t.Fatal("graph notice pointer identity counted as change")
		}
		for index := 0; index < reflect.TypeFor[GraphProjectionStatus]().NumField(); index++ {
			copied = graph
			field := reflect.ValueOf(&copied).Elem().Field(index)
			switch field.Kind() {
			case reflect.String:
				field.SetString(field.String() + "changed")
			case reflect.Int:
				field.SetInt(field.Int() + 1)
			case reflect.Bool:
				field.SetBool(!field.Bool())
			}
			if before.Equal(after) || after.Equal(before) {
				t.Fatal("graph status field change ignored", index)
			}
		}
		after.GraphProjection = nil
		if before.Equal(after) || after.Equal(before) {
			t.Fatal("graph notice addition/removal ignored")
		}
	}
	after := before
	changedPending := pending
	changedPending.State = "catching_up"
	after.PendingProjection = &changedPending
	if before.Equal(after) {
		t.Fatal("graph notice masked pending notice change")
	}
	raw, err := json.Marshal(before)
	if err != nil || !strings.Contains(string(raw), `"graph_projection":`) || !strings.Contains(string(raw), `"pending_projection":`) {
		t.Fatal("notice JSON contract changed", string(raw), err)
	}
}

func TestGraphReadGenerationRuntimeJSONContract(t *testing.T) {
	generation := GraphReadGeneration{Generation: HashContent([]byte("generation")), SourceEpoch: "source:2", AppliedEpoch: "source:1", Rules: 2, Complete: true, Current: false}
	raw, err := json.Marshal(generation)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 6 {
		t.Fatal("read generation shape changed", string(raw), err)
	}
	for _, key := range []string{"generation", "source_epoch", "applied_epoch", "rules", "complete", "current"} {
		if _, exists := fields[key]; !exists {
			t.Fatal("runtime generation field missing", key)
		}
	}
	for _, value := range []any{&RepositoryView{}, &PendingView{}, &GraphState{}} {
		raw, err := json.Marshal(value)
		if err != nil || strings.Contains(string(raw), "read_generation") {
			t.Fatal("nil runtime metadata changed legacy JSON", string(raw), err)
		}
		reflect.ValueOf(value).Elem().FieldByName("ReadGeneration").Set(reflect.ValueOf(&generation))
		raw, err = json.Marshal(value)
		if err != nil || !strings.Contains(string(raw), `"read_generation":`) || !strings.Contains(string(raw), `"current":false`) {
			t.Fatal("runtime metadata absent from response JSON", string(raw), err)
		}
	}
	graphRead, pendingRead := reflect.TypeFor[GraphProjectionRead](), reflect.TypeFor[PendingProjectionRead]()
	if graphRead.NumField() != pendingRead.NumField()+4 {
		t.Fatal("graph read shape changed")
	}
	for index := 0; index < pendingRead.NumField(); index++ {
		field := pendingRead.Field(index)
		got, exists := graphRead.FieldByName(field.Name)
		if !exists || got.Type != field.Type {
			t.Fatal("graph read lost existing field", field.Name)
		}
	}
}
