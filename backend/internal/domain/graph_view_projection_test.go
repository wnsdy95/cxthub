package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func graphViewProjectionTestView(repo ContentHash) RepositoryView {
	graph := graphProjectionTestState(repo)
	first, second := graph.GraphIDs[1], graph.GraphIDs[0]
	stamp := time.Unix(1234567890, 123456789).UTC()
	author := TeamIdentity{Name: "Name", Email: "name@example.test", Team: "team"}
	return RepositoryView{
		Graph: &graph, Revision: graph.Revision, DefaultBranch: "main",
		Snapshots: []Snapshot{
			{ID: second, RepoID: repo, Branch: "topic", Branches: []string{"topic", "main", "topic"}, Parents: []ContentHash{first}, DocHash: second, DocIdentity: DocumentIdentityRootV1, MemoryHash: first,
				ClaudeSettings: first, AgentsSettings: second, CodexSettings: first, Provider: "codex", Fidelity: "full", Message: "preserve <canonical> & \ud55c\uae00", Author: author, CreatedAt: stamp,
				Grafted: true, GraftParents: []ContentHash{second, first}, GraftSeq: 1<<64 - 1, SessionID: "session", Models: []string{"z", "a", "z"}, CompactionCount: 3},
			{ID: first, RepoID: repo, Branches: []string{}, Parents: []ContentHash{}, GraftParents: []ContentHash{}, Models: []string{}, CreatedAt: stamp},
			{ID: first, RepoID: repo, CreatedAt: stamp},
		},
		Refs:   []Ref{{RepoID: repo, BranchID: "main-id", Kind: RefBranch, Name: "main", Target: second}, {RepoID: repo, Kind: RefHead, Name: "HEAD", Symbolic: "main"}},
		Reflog: []RefLogEntry{{Kind: RefBranch, Name: "main", Old: second, New: first, CreatedAt: stamp}, {Kind: RefBranch, Name: "main", New: second, CreatedAt: stamp}},
		History: []HistoryEvent{
			{RepoID: string(repo), ID: "event", Kind: "pr-merge", Branch: "main", BranchID: "main-id", SourceBranchID: "topic-id", Source: first, Target: second, SharedTarget: first, PRCompleted: true,
				PR:          &PullRequestMerge{Number: 17, BaseBranch: "main", HeadBranch: "topic", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)},
				Creation:    &GitCreation{Evidence: "process-argv", Command: []string{"git", "switch", "topic"}, StartRef: "HEAD", StartCommit: strings.Repeat("c", 40), OriginBranch: "main", OriginBranchID: "main-id"},
				LocalBranch: "local", PreviousBranch: "previous", BindingParent: "binding", NameParent: "name", MemorySelectionParent: "memory-selection", MemorySource: first, MemoryHash: second,
				MemoryPinned: true, RecoveryEvidence: "recorded-only", GitBefore: strings.Repeat("d", 40), GitAfter: strings.Repeat("e", 40), WorktreeID: "worktree", CreatedAt: stamp},
			{RepoID: string(repo), ID: "empty-command", Creation: &GitCreation{Command: []string{}}, PR: &PullRequestMerge{}},
			{RepoID: string(repo), ID: "nil-command", Creation: &GitCreation{}},
			{RepoID: string(repo), ID: "nil-creation"},
		},
		Pending: []Pending{{RepoID: repo, SessionID: "session", Branch: "topic", Provider: "codex", Target: second, Author: author, UpdatedAt: stamp, ActivityAt: &stamp, Dismissed: true}, {RepoID: repo, SessionID: "nil-activity"}},
		Unsync:  []Unsync{{RepoID: repo, User: "peer", Branch: "topic", Target: second, Author: author, UpdatedAt: stamp}},
		ArchivedSessions: []SessionArchiveView{
			{SessionArchive: SessionArchive{RepoID: repo, Key: first, SnapshotID: first, Provider: "codex", SessionID: "session", ArchivedAt: stamp, ArchivedBy: "peer"}, LatestSnapshotID: second,
				SnapshotIDs: []ContentHash{second, first, second}, Message: "archive", Branch: "topic", Author: author, UpdatedAt: stamp,
				Origin: SessionOrigin{ParentSnapshotID: first, ParentSessionID: "parent", ParentProvider: "codex", MainSnapshotID: second, MainGitCommit: strings.Repeat("f", 40), MainBranch: "main"}},
			{SessionArchive: SessionArchive{RepoID: repo}, SnapshotIDs: []ContentHash{}},
			{SessionArchive: SessionArchive{RepoID: repo}, SnapshotIDs: nil},
		},
		Semantics: ContextSemantics{Version: 1, Merges: []ContextMergeEvidence{{EventID: "event", BirthID: "birth", Completed: false, PlacementIntact: true, SourceAvailable: false, Lineage: "preserved-not-recomputed"}, {EventID: "other", Completed: true}}},
	}
}

func TestGraphViewProjectionRichRoundTrip(t *testing.T) {
	repo := HashContent([]byte(t.Name()))
	want := graphViewProjectionTestView(repo)
	before := graphProjectionTestJSON(t, want)
	parts, err := EncodeGraphViewProjection(context.Background(), repo, want)
	if err != nil {
		t.Fatal(err)
	}
	canonical := graphProjectionTestJSON(t, parts)
	random := rand.New(rand.NewSource(73))
	for iteration := 0; iteration < 5; iteration++ {
		random.Shuffle(len(parts), func(left, right int) { parts[left], parts[right] = parts[right], parts[left] })
		partsBefore := graphProjectionTestJSON(t, parts)
		got, err := DecodeGraphViewProjection(context.Background(), repo, parts)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("bundle lost canonical data: %v\ngot: %#v\nwant: %#v", err, got, want)
		}
		next, err := EncodeGraphViewProjection(context.Background(), repo, got)
		if err != nil || !bytes.Equal(canonical, graphProjectionTestJSON(t, next)) {
			t.Fatal("unstable bundle encoding", err)
		}
		got.Snapshots[0].Branches[0] = "changed"
		got.History[0].Creation.Command[0] = "changed"
		got.History[0].PR.Number++
		*got.Pending[0].ActivityAt = time.Time{}
		got.ArchivedSessions[0].SnapshotIDs[0] = "changed"
		got.Graph.Operations.Merges[0].identity = "changed"
		got.Graph.Integrations[0].Parents["merge"][0] = "changed"
		got.Semantics.Merges[0].Lineage = "changed"
		if !bytes.Equal(partsBefore, graphProjectionTestJSON(t, parts)) || !bytes.Equal(before, graphProjectionTestJSON(t, want)) {
			t.Fatal("bundle codec aliases caller data")
		}
	}
	header := parts[graphProjectionTestIndex(t, parts, "view")]
	kinds, keys, err := GraphViewProjectionPartKeys(header)
	if err != nil || len(keys) != len(parts)-1 || len(kinds) != len(keys) {
		t.Fatal("bundle discovery failed", err)
	}
	for index, key := range keys {
		if kinds[index] != "field" || parts[graphProjectionTestIndex(t, parts, key)].Kind != kinds[index] {
			t.Fatal("bundle descriptor identity changed")
		}
	}
	if _, _, err := GraphProjectionPartKeys(header); !errors.Is(err, ErrIntegrity) {
		t.Fatal("graph-only discovery accepted bundle", err)
	}
	if _, err := DecodeGraphProjection(context.Background(), repo, parts); !errors.Is(err, ErrIntegrity) {
		t.Fatal("graph-only decoder accepted bundle", err)
	}
}

func TestGraphViewProjectionNilEmptyAndJSONB(t *testing.T) {
	repo := HashContent([]byte(t.Name()))
	for _, empty := range []bool{false, true} {
		want := RepositoryView{Graph: &GraphState{Version: 1}}
		if empty {
			value := reflect.ValueOf(&want).Elem()
			for index := 0; index < value.NumField(); index++ {
				field := value.Field(index)
				if field.Kind() == reflect.Slice {
					field.Set(reflect.MakeSlice(field.Type(), 0, 0))
				}
			}
			want.Semantics.Merges = []ContextMergeEvidence{}
		}
		parts, err := EncodeGraphViewProjection(context.Background(), repo, want)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeGraphViewProjection(context.Background(), repo, parts)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatal("nil/empty bundle changed", empty, err)
		}
	}
	want := graphViewProjectionTestView(repo)
	parts, err := EncodeGraphViewProjection(context.Background(), repo, want)
	if err != nil {
		t.Fatal(err)
	}
	for index, part := range parts {
		decoder := json.NewDecoder(bytes.NewReader(part.Value))
		decoder.UseNumber()
		var normalized any
		if err := decoder.Decode(&normalized); err != nil {
			t.Fatal(err)
		}
		var raw bytes.Buffer
		encoder := json.NewEncoder(&raw)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", " ")
		if err := encoder.Encode(normalized); err != nil {
			t.Fatal(err)
		}
		parts[index].Value = raw.Bytes()
	}
	if got, err := DecodeGraphViewProjection(context.Background(), repo, parts); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("JSONB normalization changed bundle", err)
	}
}

func TestGraphViewProjectionBindings(t *testing.T) {
	repo, foreign := HashContent([]byte(t.Name())), HashContent([]byte("foreign"))
	cases := []struct {
		name   string
		change func(*RepositoryView)
	}{
		{"missing graph", func(view *RepositoryView) { view.Graph = nil }},
		{"graph version", func(view *RepositoryView) { view.Graph.Version++ }},
		{"historical position", func(view *RepositoryView) { view.Graph.PositionEvent = "position" }},
		{"mixed revision", func(view *RepositoryView) { view.Revision.Pending++ }},
		{"runtime view", func(view *RepositoryView) { view.ReadGeneration = &GraphReadGeneration{} }},
		{"runtime graph", func(view *RepositoryView) { view.Graph.ReadGeneration = &GraphReadGeneration{} }},
		{"foreign snapshot", func(view *RepositoryView) { view.Snapshots[0].RepoID = foreign }},
		{"foreign ref", func(view *RepositoryView) { view.Refs[0].RepoID = foreign }},
		{"foreign history", func(view *RepositoryView) { view.History[0].RepoID = string(foreign) }},
		{"foreign pending", func(view *RepositoryView) { view.Pending[0].RepoID = foreign }},
		{"foreign unsync", func(view *RepositoryView) { view.Unsync[0].RepoID = foreign }},
		{"foreign archive", func(view *RepositoryView) { view.ArchivedSessions[0].RepoID = foreign }},
		{"foreign graph hold", func(view *RepositoryView) { view.Graph.Hold[0].Tips[0].RepoID = foreign }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			view := graphViewProjectionTestView(repo)
			test.change(&view)
			if parts, err := EncodeGraphViewProjection(context.Background(), repo, view); !errors.Is(err, ErrIntegrity) || parts != nil {
				t.Fatal("invalid bundle encoded", err)
			}
		})
	}
	graph := GraphState{Version: 1, ReadGeneration: &GraphReadGeneration{}}
	if parts, err := EncodeGraphProjection(context.Background(), repo, graph); !errors.Is(err, ErrIntegrity) || parts != nil {
		t.Fatal("graph-only codec persisted runtime metadata", err)
	}
	parts, err := EncodeGraphViewProjection(context.Background(), repo, graphViewProjectionTestView(repo))
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []ContentHash{"", "invalid", foreign} {
		if _, err := DecodeGraphViewProjection(context.Background(), invalid, parts); !errors.Is(err, ErrIntegrity) {
			t.Fatal("bundle decoded against wrong repository", err)
		}
		if _, err := EncodeGraphViewProjection(context.Background(), invalid, graphViewProjectionTestView(repo)); !errors.Is(err, ErrIntegrity) {
			t.Fatal("bundle encoded against wrong repository", err)
		}
	}
	graphOnly, err := EncodeGraphProjection(context.Background(), repo, GraphState{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeGraphViewProjection(context.Background(), repo, graphOnly); !errors.Is(err, ErrIntegrity) {
		t.Fatal("bundle decoder accepted graph-only generation", err)
	}
	if _, _, err := GraphViewProjectionPartKeys(graphOnly[len(graphOnly)-1]); !errors.Is(err, ErrIntegrity) {
		t.Fatal("bundle discovery accepted graph-only generation", err)
	}
}

func TestGraphViewProjectionCorruption(t *testing.T) {
	repo := HashContent([]byte(t.Name()))
	original, err := EncodeGraphViewProjection(context.Background(), repo, graphViewProjectionTestView(repo))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change func([]GraphProjectionPart) []GraphProjectionPart
	}{
		{"missing header", func(parts []GraphProjectionPart) []GraphProjectionPart { return parts[:len(parts)-1] }},
		{"missing graph", func(parts []GraphProjectionPart) []GraphProjectionPart { return parts[31:] }},
		{"missing metadata", func(parts []GraphProjectionPart) []GraphProjectionPart { return append(parts[:31], parts[32:]...) }},
		{"duplicate part", func(parts []GraphProjectionPart) []GraphProjectionPart { return append(parts, parts[0]) }},
		{"rawdoc row", func(parts []GraphProjectionPart) []GraphProjectionPart { parts[0].Kind = "rawdoc"; return parts }},
		{"proof row", func(parts []GraphProjectionPart) []GraphProjectionPart {
			parts[0].Key = "git_contribution_proof"
			return parts
		}},
		{"runtime view", func(parts []GraphProjectionPart) []GraphProjectionPart {
			parts[0].Key = "read_generation"
			return parts
		}},
		{"runtime graph", func(parts []GraphProjectionPart) []GraphProjectionPart {
			parts[0].Key = "graph.read_generation"
			return parts
		}},
		{"digest mismatch", func(parts []GraphProjectionPart) []GraphProjectionPart {
			parts[graphProjectionTestIndex(t, parts, "refs")].Value = []byte(`{"value":[]}`)
			return parts
		}},
		{"mixed revision", func(parts []GraphProjectionPart) []GraphProjectionPart {
			graphProjectionTestRebind(t, parts, "revision", []byte(`{"value":{"graph":"0","pending":"0"}}`))
			return parts
		}},
		{"selected position", func(parts []GraphProjectionPart) []GraphProjectionPart {
			graphProjectionTestRebind(t, parts, "graph.position_event", []byte(`{"value":"selected"}`))
			return parts
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			parts := test.change(append([]GraphProjectionPart(nil), original...))
			if got, err := DecodeGraphViewProjection(context.Background(), repo, parts); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(got, RepositoryView{}) {
				t.Fatal("corrupt bundle returned data", err)
			}
		})
	}
	for _, key := range []string{"snapshots", "refs", "history", "pending", "unsync", "archived_sessions", "graph.hold"} {
		t.Run(key+" foreign repo", func(t *testing.T) {
			parts := append([]GraphProjectionPart(nil), original...)
			raw := parts[graphProjectionTestIndex(t, parts, key)].Value
			raw = bytes.Replace(raw, []byte(repo), []byte(HashContent([]byte("foreign"))), 1)
			graphProjectionTestRebind(t, parts, key, raw)
			if got, err := DecodeGraphViewProjection(context.Background(), repo, parts); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(got, RepositoryView{}) {
				t.Fatal("rebound foreign row accepted", err)
			}
		})
	}
}

func TestGraphViewProjectionStrictSchema(t *testing.T) {
	repo := HashContent([]byte(t.Name()))
	original, err := EncodeGraphViewProjection(context.Background(), repo, graphViewProjectionTestView(repo))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ key, from, to string }{
		{"snapshots", `"models":[]`, `"models":null,"models":[]`},
		{"snapshots", `"branches":[]`, `"Branches":[]`},
		{"snapshots", `"graft_parents":[]`, `"graft_parents":{}`},
		{"snapshots", `"models":[]`, `"models":[],"rawdoc":"body"`},
		{"snapshots", `"models":[]`, `"models":[],"read_generation":{}`},
		{"history", `"command":[]`, `"unknown":[]`},
		{"history", `"number":17`, `"number":null`},
		{"history", `"evidence":"process-argv"`, `"evidence":"process-argv","proof":{}`},
		{"archived_sessions", `"latest_snapshot_id":`, `"LATEST_SNAPSHOT_ID":`},
		{"semantics", `"completed":false`, `"completed":false,"read_generation":{}`},
		{"pending", `"activity_at":"2009-02-13T23:31:30.123456789Z"`, `"activity_at":{}`},
		{"graph.operations", `"identity":"private-source-identity"`, `"identity":"private-source-identity","read_generation":{}`},
	} {
		t.Run(test.key+" "+test.from, func(t *testing.T) {
			parts := append([]GraphProjectionPart(nil), original...)
			raw := parts[graphProjectionTestIndex(t, parts, test.key)].Value
			if !bytes.Contains(raw, []byte(test.from)) {
				t.Fatal("fixture missing replacement", test.from)
			}
			graphProjectionTestRebind(t, parts, test.key, bytes.Replace(raw, []byte(test.from), []byte(test.to), 1))
			if _, err := DecodeGraphViewProjection(context.Background(), repo, parts); !errors.Is(err, ErrIntegrity) {
				t.Fatal("invalid nested bundle schema accepted", err)
			}
		})
	}
}

func TestGraphViewProjectionSchemaDrift(t *testing.T) {
	view := RepositoryView{Graph: &GraphState{}}
	var rows graphViewProjectionRows
	var operations graphProjectionOperations
	fields := graphViewProjectionFields(&view, &operations, &rows)
	byKey := map[string]graphProjectionField{}
	for _, field := range fields {
		if _, exists := byKey[field.key]; exists {
			t.Fatal("duplicate bundle field", field.key)
		}
		byKey[field.key] = field
	}
	value := reflect.ValueOf(&view).Elem()
	for index := 0; index < value.NumField(); index++ {
		definition := value.Type().Field(index)
		key, _, _ := strings.Cut(definition.Tag.Get("json"), ",")
		if definition.Name == "ReadGeneration" {
			if definition.Type != reflect.TypeFor[*GraphReadGeneration]() || definition.Tag.Get("json") != "read_generation,omitempty" {
				t.Fatal("runtime field contract changed")
			}
			continue
		}
		if definition.Name == "Graph" {
			for _, field := range graphProjectionFields(view.Graph, &operations) {
				bundle, exists := byKey["graph."+field.key]
				if !exists || !reflect.DeepEqual(bundle.value, field.value) {
					t.Fatal("graph field absent from bundle", field.key)
				}
				delete(byKey, "graph."+field.key)
			}
			continue
		}
		field, exists := byKey[key]
		if !exists {
			t.Fatal("missing repository field", definition.Name)
		}
		delete(byKey, key)
		want := value.Field(index).Addr()
		switch definition.Name {
		case "Snapshots":
			want = reflect.ValueOf(&rows.snapshots)
		case "History":
			want = reflect.ValueOf(&rows.history)
		}
		got := reflect.ValueOf(field.value).FieldByName("Value")
		if got.Type() != want.Type() || got.Pointer() != want.Pointer() || field.decode == nil {
			t.Fatal("repository field is not bound to its wire value", key)
		}
	}
	if len(byKey) != 0 {
		t.Fatal("unexpected durable fields", byKey)
	}
	for _, schema := range []struct{ domain, wire reflect.Type }{
		{reflect.TypeFor[Snapshot](), reflect.TypeFor[graphViewProjectionSnapshot]()},
		{reflect.TypeFor[GitCreation](), reflect.TypeFor[graphViewProjectionCreation]()},
		{reflect.TypeFor[HistoryEvent](), reflect.TypeFor[graphViewProjectionHistory]()},
		{reflect.TypeFor[SessionArchiveView](), reflect.TypeFor[SessionArchiveView]()},
	} {
		domainFields, err := graphProjectionJSONFields(schema.domain)
		if err != nil {
			t.Fatal(err)
		}
		wireFields, err := graphProjectionJSONFields(schema.wire)
		if err != nil || len(domainFields) != len(wireFields) {
			t.Fatal("wire schema drift", schema.domain, err)
		}
		for key, field := range domainFields {
			wire, exists := wireFields[key]
			wantType := field.Type
			if schema.domain == reflect.TypeFor[HistoryEvent]() && field.Name == "Creation" {
				wantType = reflect.TypeFor[*graphViewProjectionCreation]()
			}
			if !exists || wire.Type != wantType || ((wire.Type.Kind() == reflect.Map || wire.Type.Kind() == reflect.Slice) && strings.Contains(wire.Tag.Get("json"), "omitempty")) {
				t.Fatal("missing or lossy durable field", schema.domain, key)
			}
		}
	}
}

func TestGraphViewProjectionConcurrentReaders(t *testing.T) {
	repo := HashContent([]byte(t.Name()))
	view := graphViewProjectionTestView(repo)
	parts, err := EncodeGraphViewProjection(context.Background(), repo, view)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Go(func() {
			for iteration := 0; iteration < 3; iteration++ {
				got, err := DecodeGraphViewProjection(context.Background(), repo, parts)
				if err != nil || !reflect.DeepEqual(got, view) {
					t.Error("concurrent bundle decode", err)
					return
				}
				next, err := EncodeGraphViewProjection(context.Background(), repo, view)
				if err != nil || !reflect.DeepEqual(next, parts) {
					t.Error("concurrent bundle encode", err)
					return
				}
				if _, _, err := GraphViewProjectionPartKeys(parts[len(parts)-1]); err != nil {
					t.Error("concurrent bundle discovery", err)
					return
				}
			}
		})
	}
	workers.Wait()
}
