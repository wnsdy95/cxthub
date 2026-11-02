package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func pendingProjectionTestJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func pendingProjectionFixture(t *testing.T, protocol int) (ContentHash, []RepositoryViewSourceEntry, PendingStateView) {
	t.Helper()
	repo := HashContent([]byte("pending source repository"))
	snapshot := func(name, session string, stamp int64, parents ...ContentHash) Snapshot {
		identifier := HashContent([]byte(name))
		return Snapshot{ID: identifier, RepoID: repo, DocHash: identifier, Provider: ProviderCodex, Fidelity: FidelityFull, SessionID: session, Branch: "topic", Parents: append([]ContentHash{}, parents...), Message: name, CreatedAt: time.Unix(stamp, 0).UTC()}
	}
	root := snapshot("root", "root-session", 10)
	old := snapshot("reflog publication", "reflog-session", 20)
	published := snapshot("history publication", "history-session", 20)
	local := snapshot("local capture", "local-session", 30, root.ID)
	graft := snapshot("grafted capture", "graft-session", 40)
	graft.Grafted, graft.GraftSeq, graft.GraftParents = true, 2, []ContentHash{local.ID}
	live := snapshot("hook: live", "live-session", 50, graft.ID)
	dismissed := snapshot("dismissed capture", "dismissed-session", 50)
	archived := snapshot("archived capture", "archive-session", 70, root.ID)
	newCapture := snapshot("new archived capture with clock rollback", "archive-session", 60, archived.ID)
	lifecycle := snapshot("legacy archived branch", "legacy-session", 80)
	archival := SessionArchive{RepoID: repo, Key: SessionArchiveKey(archived), SnapshotID: archived.ID, Provider: archived.Provider, SessionID: archived.SessionID, ArchivedAt: time.Unix(100, 0).UTC(), ArchivedBy: "maintainer"}
	legacy, err := NewBranchLifecycleRef(repo, "legacy", lifecycle.ID, 1, BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	activity := time.Unix(999, 0).UTC()
	view := RepositoryView{
		DefaultBranch: "main", Revision: RepositoryRevision{Graph: 8, Pending: 13, Evidence: 21},
		Snapshots: []Snapshot{root, old, published, local, graft, live, dismissed, archived, newCapture, lifecycle},
		Refs:      []Ref{{RepoID: repo, Kind: RefBranch, Name: "main", Target: root.ID}, {RepoID: repo, Kind: RefBranch, Name: "legacy", Target: lifecycle.ID}, {RepoID: repo, Kind: RefHead, Name: "HEAD", Symbolic: "legacy"}, legacy},
		Unsync:    []Unsync{{RepoID: repo, User: "later-user", Branch: "topic", Target: local.ID, UpdatedAt: time.Unix(200, 0).UTC()}, {RepoID: repo, User: "earlier-user", Branch: "topic", Target: graft.ID, UpdatedAt: time.Unix(200, 0).UTC()}},
		Reflog:    []RefLogEntry{{Kind: RefBranch, Name: "deleted", Old: old.ID, New: root.ID, CreatedAt: time.Unix(12, 0).UTC()}, {Kind: RefBranch, Name: "deleted", New: old.ID, CreatedAt: time.Unix(11, 0).UTC()}},
	}
	for _, capture := range []Snapshot{root, old, published, live, dismissed, newCapture} {
		view.Pending = append(view.Pending, Pending{RepoID: repo, SessionID: capture.SessionID, Branch: capture.Branch, Provider: capture.Provider, Target: capture.ID, UpdatedAt: activity, ActivityAt: &activity, Dismissed: capture.ID == dismissed.ID})
	}
	birth := HistoryEvent{ID: fmt.Sprintf("%032x", 1), RepoID: string(repo), Kind: "birth", Branch: "topic", BranchID: "topic-id", CreatedAt: time.Unix(120, 0).UTC()}
	rename := HistoryEvent{ID: fmt.Sprintf("%032x", 2), RepoID: string(repo), Kind: "rename", Branch: "renamed", PreviousBranch: "topic", BranchID: birth.BranchID, BindingParent: birth.ID, CreatedAt: time.Unix(100, 0).UTC()}
	observation := HistoryEvent{ID: fmt.Sprintf("%032x", 3), RepoID: string(repo), Kind: "position", Branch: "main", BranchID: "main-id", Target: published.ID, Source: published.ID, GitAfter: strings.Repeat("a", 40), WorktreeID: strings.Repeat("b", 32), CreatedAt: time.Unix(30, 0).UTC()}
	publication := observation
	publication.ID, publication.Kind = fmt.Sprintf("%032x", 4), "publish"
	advance := observation
	advance.ID, advance.Kind, advance.Source = fmt.Sprintf("%032x", 5), "advance", root.ID
	view.History = []HistoryEvent{rename, advance, publication, observation, birth}
	entries := []RepositoryViewSourceEntry{}
	add := func(kind, key string, value any) {
		entries = append(entries, RepositoryViewSourceEntry{Sequence: int64(1000 - len(entries)), Kind: kind, Key: key, Value: pendingProjectionTestJSON(t, value)})
	}
	add("repository", string(repo), Repo{ID: repo, DefaultBranch: view.DefaultBranch, ContextProtocol: protocol})
	add("protocol", string(repo), map[string]int{"context_protocol": protocol})
	add("revision", string(repo), view.Revision)
	for _, capture := range view.Snapshots {
		add("snapshot", string(capture.ID), capture)
	}
	for _, ref := range view.Refs {
		add("ref", pendingProjectionPair(string(ref.Kind), ref.Name), ref)
	}
	for _, pending := range view.Pending {
		add("pending", pending.SessionID, pending)
	}
	for _, unsync := range view.Unsync {
		add("unsync", pendingProjectionPair(unsync.User, unsync.Branch), unsync)
	}
	for _, event := range view.History {
		add("history", event.ID, event)
	}
	add("reflog", "10", view.Reflog[0])
	add("reflog", "2", view.Reflog[1])
	add("session_archive", string(archival.Key), archival)
	sort.Slice(view.Snapshots, func(left, right int) bool {
		if view.Snapshots[left].CreatedAt.Equal(view.Snapshots[right].CreatedAt) {
			return view.Snapshots[left].ID < view.Snapshots[right].ID
		}
		return view.Snapshots[left].CreatedAt.After(view.Snapshots[right].CreatedAt)
	})
	sort.Slice(view.Refs, func(left, right int) bool {
		if view.Refs[left].Kind == view.Refs[right].Kind {
			return view.Refs[left].Name < view.Refs[right].Name
		}
		return view.Refs[left].Kind < view.Refs[right].Kind
	})
	if protocol == 0 {
		view.Refs, err = ProjectBranchLifecycleRefs(view.Refs)
		if err != nil {
			t.Fatal(err)
		}
	}
	view.ArchivedSessions = ProjectSessionArchives([]SessionArchive{archival}, view.Snapshots, view.History, view.DefaultBranch)
	for index := range view.Pending {
		for _, capture := range view.Snapshots {
			if capture.ID == view.Pending[index].Target && !capture.CreatedAt.IsZero() {
				view.Pending[index].UpdatedAt = capture.CreatedAt
			}
		}
	}
	want, err := ProjectPendingState(context.Background(), view)
	if err != nil {
		t.Fatal(err)
	}
	return repo, entries, want
}

func TestProjectPendingSourceBaselineParityAndPermutation(t *testing.T) {
	for _, protocol := range []int{0, 1} {
		t.Run(fmt.Sprintf("protocol-%d", protocol), func(t *testing.T) {
			repo, entries, want := pendingProjectionFixture(t, protocol)
			random := rand.New(rand.NewSource(19))
			var canonical []byte
			for iteration := 0; iteration < 20; iteration++ {
				random.Shuffle(len(entries), func(left, right int) { entries[left], entries[right] = entries[right], entries[left] })
				for index := range entries {
					entries[index].Sequence = random.Int63n(100000)
				}
				before := pendingProjectionTestJSON(t, entries)
				got, err := ProjectPendingSource(context.Background(), repo, entries)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("baseline parity:\ngot %s\nwant %s", pendingProjectionTestJSON(t, got), pendingProjectionTestJSON(t, want))
				}
				if !bytes.Equal(before, pendingProjectionTestJSON(t, entries)) {
					t.Fatal("source input mutated")
				}
				parts, err := EncodePendingProjection(context.Background(), repo, got)
				if err != nil {
					t.Fatal(err)
				}
				encoded := pendingProjectionTestJSON(t, parts)
				if iteration != 0 && !bytes.Equal(canonical, encoded) {
					t.Fatal("unordered source or sequence changed projected rows")
				}
				canonical = encoded
			}
			if !reflect.DeepEqual(want.OrphanSessions, []string{"archive-session", "live-session"}) || len(want.Hold) != 1 || len(want.Hold[0].IDs) != 2 {
				t.Fatalf("fixture does not exercise history, reflog, grafts, dismissal and live captures: %+v", want)
			}
			if len(want.ArchivedSessions) != 1 || want.ArchivedSessions[0].LatestSnapshotID == want.ArchivedSessions[0].SnapshotID || want.ArchivedSessions[0].Origin.ParentSnapshotID == "" {
				t.Fatal("archive new capture or origin missing")
			}
			for _, pending := range want.Pending {
				if pending.UpdatedAt.Equal(*pending.ActivityAt) {
					t.Fatal("pending target timestamp was not restored")
				}
			}
		})
	}
}

func TestPendingProjectionRoundTripPreservesHeaderOrderAndStableRows(t *testing.T) {
	repo, _, view := pendingProjectionFixture(t, 0)
	before := pendingProjectionTestJSON(t, view)
	parts, err := EncodePendingProjection(context.Background(), repo, view)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1+len(view.Refs)+len(view.Pending)+len(view.Snapshots)+len(view.ArchivedSessions)+len(view.Hold) {
		t.Fatal("projection is not row-oriented")
	}
	for left, right := 0, len(parts)-1; left < right; left, right = left+1, right-1 {
		parts[left], parts[right] = parts[right], parts[left]
	}
	partsBefore := pendingProjectionTestJSON(t, parts)
	decoded, err := DecodePendingProjection(context.Background(), repo, parts)
	if err != nil || !reflect.DeepEqual(view, decoded) || decoded.ReadGeneration != nil {
		t.Fatalf("round trip failed: %v\ngot %s\nwant %s", err, pendingProjectionTestJSON(t, decoded), before)
	}
	if !bytes.Equal(partsBefore, pendingProjectionTestJSON(t, parts)) {
		t.Fatal("decoding mutated parts")
	}
	decoded.Pending[0].Target = HashContent([]byte("changed"))
	decoded.Hold[0].IDs[0] = HashContent([]byte("changed hold"))
	decoded.HoldCounts["topic"] = 100
	if !bytes.Equal(before, pendingProjectionTestJSON(t, view)) || !bytes.Equal(partsBefore, pendingProjectionTestJSON(t, parts)) {
		t.Fatal("codec retained caller-owned mutable data")
	}
	view.Revision.Pending++
	for left, right := 0, len(view.Snapshots)-1; left < right; left, right = left+1, right-1 {
		view.Snapshots[left], view.Snapshots[right] = view.Snapshots[right], view.Snapshots[left]
	}
	next, err := EncodePendingProjection(context.Background(), repo, view)
	if err != nil {
		t.Fatal(err)
	}
	prior := map[[2]string]json.RawMessage{}
	for _, part := range parts {
		prior[[2]string{part.Kind, part.Key}] = part.Value
	}
	for _, part := range next {
		if part.Kind != "header" && !bytes.Equal(prior[[2]string{part.Kind, part.Key}], part.Value) {
			t.Fatalf("unchanged row copied under a generation identity: %s/%s", part.Kind, part.Key)
		}
	}
	decoded, err = DecodePendingProjection(context.Background(), repo, next)
	if err != nil || !reflect.DeepEqual(decoded, view) {
		t.Fatal("header did not preserve noncanonical caller ordering", err)
	}
	first, err := pendingProjectionRowKey(repo, view.Hold[0])
	if err != nil {
		t.Fatal(err)
	}
	view.Hold[0].IDs[0], view.Hold[0].IDs[1] = view.Hold[0].IDs[1], view.Hold[0].IDs[0]
	second, err := pendingProjectionRowKey(repo, view.Hold[0])
	if err != nil || first != second || ValidateContentHash(ContentHash(first)) != nil {
		t.Fatal("hold key is positional or order-sensitive", err)
	}
}

func TestProjectPendingSourceRejectsMalformedImages(t *testing.T) {
	repo, original, _ := pendingProjectionFixture(t, 0)
	foreign := HashContent([]byte("foreign"))
	cases := []struct {
		name   string
		kind   string
		change func(*RepositoryViewSourceEntry)
	}{
		{"tombstone", "pending", func(entry *RepositoryViewSourceEntry) { entry.Deleted = true }},
		{"negative sequence", "pending", func(entry *RepositoryViewSourceEntry) { entry.Sequence = -1 }},
		{"unknown kind", "pending", func(entry *RepositoryViewSourceEntry) { entry.Kind = "body" }},
		{"empty key", "pending", func(entry *RepositoryViewSourceEntry) { entry.Key = "" }},
		{"long key", "pending", func(entry *RepositoryViewSourceEntry) {
			entry.Key = strings.Repeat("x", MaxRepositoryViewSourceKeyBytes+1)
		}},
		{"null", "pending", func(entry *RepositoryViewSourceEntry) { entry.Value = []byte("null") }},
		{"array", "pending", func(entry *RepositoryViewSourceEntry) { entry.Value = []byte("[]") }},
		{"truncated", "pending", func(entry *RepositoryViewSourceEntry) { entry.Value = []byte("{") }},
		{"trailing JSON", "pending", func(entry *RepositoryViewSourceEntry) {
			entry.Value = append(bytes.Clone(entry.Value), []byte(" {}")...)
		}},
		{"unknown field", "pending", func(entry *RepositoryViewSourceEntry) {
			entry.Value = append([]byte(`{"unknown":true,`), entry.Value[1:]...)
		}},
		{"duplicate JSON field", "pending", func(entry *RepositoryViewSourceEntry) {
			entry.Value = append([]byte(`{"target":"",`), entry.Value[1:]...)
		}},
		{"wrong pending id", "pending", func(entry *RepositoryViewSourceEntry) { entry.Key = "different" }},
		{"wrong snapshot id", "snapshot", func(entry *RepositoryViewSourceEntry) { entry.Key = string(foreign) }},
		{"wrong history id", "history", func(entry *RepositoryViewSourceEntry) { entry.Key = strings.Repeat("f", 32) }},
		{"wrong archive id", "session_archive", func(entry *RepositoryViewSourceEntry) { entry.Key = string(foreign) }},
		{"wrong protocol repo", "protocol", func(entry *RepositoryViewSourceEntry) { entry.Key = string(foreign) }},
		{"wrong revision repo", "revision", func(entry *RepositoryViewSourceEntry) { entry.Key = string(foreign) }},
		{"wrong repository id", "repository", func(entry *RepositoryViewSourceEntry) { entry.Key = string(foreign) }},
		{"bad ref key", "ref", func(entry *RepositoryViewSourceEntry) { entry.Key = `["branch","main","extra"]` }},
		{"bad unsync key", "unsync", func(entry *RepositoryViewSourceEntry) { entry.Key = `["different","branch"]` }},
		{"non numeric reflog", "reflog", func(entry *RepositoryViewSourceEntry) { entry.Key = "invalid" }},
		{"non canonical reflog", "reflog", func(entry *RepositoryViewSourceEntry) { entry.Key = "02" }},
		{"zero reflog", "reflog", func(entry *RepositoryViewSourceEntry) { entry.Key = "0" }},
		{"overflow reflog", "reflog", func(entry *RepositoryViewSourceEntry) { entry.Key = "9223372036854775808" }},
		{"missing protocol value", "protocol", func(entry *RepositoryViewSourceEntry) { entry.Value = []byte(`{}`) }},
		{"unknown protocol", "protocol", func(entry *RepositoryViewSourceEntry) { entry.Value = []byte(`{"context_protocol":2}`) }},
		{"inconsistent protocol", "protocol", func(entry *RepositoryViewSourceEntry) { entry.Value = []byte(`{"context_protocol":1}`) }},
		{"missing revision counter", "revision", func(entry *RepositoryViewSourceEntry) { entry.Value = []byte(`{"graph":"0"}`) }},
		{"negative revision counter", "revision", func(entry *RepositoryViewSourceEntry) { entry.Value = []byte(`{"graph":"-1","pending":"0"}`) }},
	}
	for _, kind := range []string{"snapshot", "ref", "history", "pending", "unsync", "session_archive"} {
		cases = append(cases, struct {
			name   string
			kind   string
			change func(*RepositoryViewSourceEntry)
		}{"cross repository " + kind, kind, func(entry *RepositoryViewSourceEntry) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(entry.Value, &fields); err != nil {
				t.Fatal(err)
			}
			fields["repo_id"] = pendingProjectionTestJSON(t, foreign)
			entry.Value = pendingProjectionTestJSON(t, fields)
		}})
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			entries := append([]RepositoryViewSourceEntry{}, original...)
			for index := range entries {
				if entries[index].Kind == test.kind {
					test.change(&entries[index])
					break
				}
			}
			got, err := ProjectPendingSource(context.Background(), repo, entries)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(got, PendingStateView{}) {
				t.Fatalf("malformed source accepted or partial view leaked: %+v %v", got, err)
			}
		})
	}
	for _, kind := range []string{"repository", "protocol", "revision"} {
		t.Run("missing "+kind, func(t *testing.T) {
			entries := []RepositoryViewSourceEntry{}
			for _, entry := range original {
				if entry.Kind != kind {
					entries = append(entries, entry)
				}
			}
			got, err := ProjectPendingSource(context.Background(), repo, entries)
			if kind == "revision" {
				if err != nil || got.Revision != (RepositoryRevision{}) {
					t.Fatal("tombstoned revision did not default to zero", err)
				}
			} else if !errors.Is(err, ErrIntegrity) {
				t.Fatal("mandatory image missing", err)
			}
		})
	}
	for _, entry := range original {
		duplicate := entry
		if duplicate.Kind == "ref" || duplicate.Kind == "unsync" {
			duplicate.Key = strings.ReplaceAll(duplicate.Key, ",", ", ")
		}
		if _, err := ProjectPendingSource(context.Background(), repo, append(append([]RepositoryViewSourceEntry{}, original...), duplicate)); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("duplicate %s accepted: %v", duplicate.Kind, err)
		}
	}
}

func TestPendingProjectionCodecRejectsMalformedParts(t *testing.T) {
	repo, _, view := pendingProjectionFixture(t, 0)
	original, err := EncodePendingProjection(context.Background(), repo, view)
	if err != nil {
		t.Fatal(err)
	}
	foreign := HashContent([]byte("foreign"))
	cases := []struct {
		name   string
		change func([]PendingProjectionPart) []PendingProjectionPart
	}{
		{"empty", func(parts []PendingProjectionPart) []PendingProjectionPart { return nil }},
		{"missing row", func(parts []PendingProjectionPart) []PendingProjectionPart { return parts[1:] }},
		{"duplicate row", func(parts []PendingProjectionPart) []PendingProjectionPart { return append(parts, parts[0]) }},
		{"extra row", func(parts []PendingProjectionPart) []PendingProjectionPart {
			return append(parts, PendingProjectionPart{Kind: "unexpected", Key: "extra", Value: []byte(`{}`)})
		}},
		{"wrong key", func(parts []PendingProjectionPart) []PendingProjectionPart { parts[0].Key = "wrong"; return parts }},
	}
	for _, kind := range []string{"header", "ref", "pending", "snapshot", "archive", "hold"} {
		for _, mutation := range []string{"cross repository", "version", "malformed", "unknown field", "duplicate JSON field", "bad value"} {
			cases = append(cases, struct {
				name   string
				change func([]PendingProjectionPart) []PendingProjectionPart
			}{kind + " " + mutation, func(parts []PendingProjectionPart) []PendingProjectionPart {
				for index := range parts {
					if parts[index].Kind != kind {
						continue
					}
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(parts[index].Value, &fields); err != nil {
						t.Fatal(err)
					}
					switch mutation {
					case "cross repository":
						fields["repo_id"] = pendingProjectionTestJSON(t, foreign)
					case "version":
						fields["version"] = []byte(`2`)
					case "unknown field":
						fields["unknown"] = []byte(`true`)
					case "bad value":
						if kind == "header" {
							fields["projection"] = []byte(`"graph-v1"`)
						} else {
							fields["value"] = []byte(`{}`)
						}
					}
					parts[index].Value = pendingProjectionTestJSON(t, fields)
					if mutation == "malformed" {
						parts[index].Value = []byte(`null`)
					}
					if mutation == "duplicate JSON field" {
						parts[index].Value = append([]byte(`{"version":1,`), parts[index].Value[1:]...)
					}
					break
				}
				return parts
			}})
		}
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			parts := test.change(append([]PendingProjectionPart{}, original...))
			got, err := DecodePendingProjection(context.Background(), repo, parts)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(got, PendingStateView{}) {
				t.Fatalf("malformed parts accepted or partial view leaked: %+v %v", got, err)
			}
		})
	}
	if _, err := DecodePendingProjection(context.Background(), foreign, original); !errors.Is(err, ErrIntegrity) {
		t.Fatal("cross-repository read accepted", err)
	}
	view.ReadGeneration = &PendingReadGeneration{}
	if parts, err := EncodePendingProjection(context.Background(), repo, view); !errors.Is(err, ErrIntegrity) || parts != nil {
		t.Fatal("API read generation must never be persisted", err)
	}
}

func TestPendingProjectionLimitsAndCancellation(t *testing.T) {
	repo, entries, view := pendingProjectionFixture(t, 0)
	parts, err := EncodePendingProjection(context.Background(), repo, view)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	for _, ctx := range []context.Context{canceled, expired} {
		if _, err := ProjectPendingSource(ctx, repo, entries); !errors.Is(err, ctx.Err()) {
			t.Fatal("source cancellation", err)
		}
		if _, err := EncodePendingProjection(ctx, repo, view); !errors.Is(err, ctx.Err()) {
			t.Fatal("encoder cancellation", err)
		}
		if _, err := DecodePendingProjection(ctx, repo, parts); !errors.Is(err, ctx.Err()) {
			t.Fatal("decoder cancellation", err)
		}
	}
	if _, err := ProjectPendingSource(context.Background(), repo, make([]RepositoryViewSourceEntry, MaxPendingProjectionInputRows+1)); !errors.Is(err, ErrRepositoryViewSourceLimit) {
		t.Fatal("input row limit", err)
	}
	oversizedInput := []RepositoryViewSourceEntry{{Kind: "repository", Key: string(repo), Value: bytes.Repeat([]byte(" "), MaxPendingProjectionInputBytes+1)}}
	if _, err := ProjectPendingSource(context.Background(), repo, oversizedInput); !errors.Is(err, ErrRepositoryViewSourceLimit) {
		t.Fatal("input byte limit", err)
	}
	tooMany := view
	tooMany.Refs = make([]Ref, MaxPendingProjectionOutputRows)
	if _, err := EncodePendingProjection(context.Background(), repo, tooMany); !errors.Is(err, ErrRepositoryViewSourceLimit) {
		t.Fatal("encoded row limit", err)
	}
	if _, err := DecodePendingProjection(context.Background(), repo, make([]PendingProjectionPart, MaxPendingProjectionOutputRows+1)); !errors.Is(err, ErrRepositoryViewSourceLimit) {
		t.Fatal("decoded row limit", err)
	}
	oversizedParts := []PendingProjectionPart{{Kind: "header", Key: "view", Value: bytes.Repeat([]byte(" "), MaxPendingProjectionOutputBytes+1)}}
	if _, err := DecodePendingProjection(context.Background(), repo, oversizedParts); !errors.Is(err, ErrRepositoryViewSourceLimit) {
		t.Fatal("decoded byte limit", err)
	}
	view.Snapshots[0].Message = strings.Repeat("x", MaxPendingProjectionOutputBytes)
	if _, err := EncodePendingProjection(context.Background(), repo, view); !errors.Is(err, ErrRepositoryViewSourceLimit) {
		t.Fatal("encoded byte limit", err)
	}
	for index := range entries {
		if entries[index].Kind == "snapshot" && entries[index].Key == string(view.Snapshots[0].ID) {
			entries[index].Value = pendingProjectionTestJSON(t, view.Snapshots[0])
		}
	}
	if _, err := ProjectPendingSource(context.Background(), repo, entries); !errors.Is(err, ErrRepositoryViewSourceLimit) {
		t.Fatal("source projection bypassed output limit", err)
	}
}

func TestProjectPendingSourceValidatesHistoryAndAncestry(t *testing.T) {
	for _, scenario := range []string{"missing history dependency", "conflicting branch identities", "invalid branch", "natural cycle", "graft cycle", "missing pending target"} {
		t.Run(scenario, func(t *testing.T) {
			repo, entries, _ := pendingProjectionFixture(t, 0)
			for index := range entries {
				entry := &entries[index]
				if entry.Kind == "history" && (strings.Contains(scenario, "branch") || scenario == "missing history dependency") {
					var event HistoryEvent
					if err := json.Unmarshal(entry.Value, &event); err != nil {
						t.Fatal(err)
					}
					switch scenario {
					case "missing history dependency":
						event.BindingParent = strings.Repeat("f", 32)
					case "conflicting branch identities":
						event.Kind, event.Branch, event.BranchID, event.PreviousBranch, event.BindingParent = "birth", "topic", "other-identity", "", ""
					case "invalid branch":
						event.Branch = "../invalid"
					}
					entry.Value = pendingProjectionTestJSON(t, event)
					break
				}
				if entry.Kind == "snapshot" && strings.Contains(scenario, "cycle") {
					var snapshot Snapshot
					if err := json.Unmarshal(entry.Value, &snapshot); err != nil {
						t.Fatal(err)
					}
					if scenario == "natural cycle" {
						snapshot.Parents = []ContentHash{snapshot.ID}
					} else {
						snapshot.GraftParents = []ContentHash{snapshot.ID}
					}
					entry.Value = pendingProjectionTestJSON(t, snapshot)
					break
				}
				if entry.Kind == "pending" && scenario == "missing pending target" {
					var pending Pending
					if err := json.Unmarshal(entry.Value, &pending); err != nil {
						t.Fatal(err)
					}
					pending.Target = HashContent([]byte("missing"))
					entry.Value = pendingProjectionTestJSON(t, pending)
					break
				}
			}
			got, err := ProjectPendingSource(context.Background(), repo, entries)
			if err == nil || !reflect.DeepEqual(got, PendingStateView{}) {
				t.Fatalf("invalid domain input accepted: %+v %v", got, err)
			}
		})
	}
}

func TestProjectPendingSourceOriginFlagsAndPostgresEmptyArrays(t *testing.T) {
	repo, entries, want := pendingProjectionFixture(t, 0)
	for index := range entries {
		if entries[index].Kind != "repository" && entries[index].Kind != "snapshot" {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(entries[index].Value, &fields); err != nil {
			t.Fatal(err)
		}
		if entries[index].Kind == "repository" {
			fields["remote_url_supported"], fields["git_remote_url_supported"] = []byte(`false`), []byte(`false`)
		} else {
			if len(fields["graft_parents"]) == 0 {
				fields["graft_parents"] = []byte(`[]`)
			}
			fields["models"] = []byte(`[]`)
		}
		entries[index].Value = pendingProjectionTestJSON(t, fields)
	}
	got, err := ProjectPendingSource(context.Background(), repo, entries)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("origin flags or SQL empty arrays changed pending view: %v\ngot %s\nwant %s", err, pendingProjectionTestJSON(t, got), pendingProjectionTestJSON(t, want))
	}
}

func TestPendingProjectionHeaderAndEmbeddedIntegrity(t *testing.T) {
	repo, _, view := pendingProjectionFixture(t, 0)
	for _, scenario := range []string{"missing order kind", "duplicate ordered key", "extra ordered key", "missing ordered key", "wrong orphan", "wrong count", "missing revision", "foreign inner ref", "foreign inner pending", "foreign inner snapshot", "foreign inner archive", "foreign inner hold"} {
		t.Run(scenario, func(t *testing.T) {
			parts, err := EncodePendingProjection(context.Background(), repo, view)
			if err != nil {
				t.Fatal(err)
			}
			for index := range parts {
				if parts[index].Kind == "header" && !strings.HasPrefix(scenario, "foreign inner") {
					var header pendingProjectionHeader
					if err := json.Unmarshal(parts[index].Value, &header); err != nil {
						t.Fatal(err)
					}
					switch scenario {
					case "missing order kind":
						delete(header.Order, "ref")
					case "duplicate ordered key":
						header.Order["ref"] = append(header.Order["ref"], header.Order["ref"][0])
					case "extra ordered key":
						header.Order["ref"] = append(header.Order["ref"], "missing")
					case "missing ordered key":
						header.Order["ref"] = header.Order["ref"][1:]
					case "wrong orphan":
						header.OrphanSessions = append(header.OrphanSessions, "missing")
					case "wrong count":
						header.HoldCounts["topic"]++
					}
					parts[index].Value = pendingProjectionTestJSON(t, header)
					if scenario == "missing revision" {
						var fields map[string]json.RawMessage
						if err := json.Unmarshal(parts[index].Value, &fields); err != nil {
							t.Fatal(err)
						}
						fields["revision"] = []byte(`{}`)
						parts[index].Value = pendingProjectionTestJSON(t, fields)
					}
					break
				}
				if scenario != "foreign inner "+parts[index].Kind {
					continue
				}
				var row pendingProjectionRow[map[string]json.RawMessage]
				if err := json.Unmarshal(parts[index].Value, &row); err != nil {
					t.Fatal(err)
				}
				if parts[index].Kind == "hold" {
					var tips []Unsync
					if err := json.Unmarshal(row.Value["tips"], &tips); err != nil {
						t.Fatal(err)
					}
					tips[0].RepoID = HashContent([]byte("foreign"))
					row.Value["tips"] = pendingProjectionTestJSON(t, tips)
				} else {
					row.Value["repo_id"] = pendingProjectionTestJSON(t, HashContent([]byte("foreign")))
				}
				parts[index].Value = pendingProjectionTestJSON(t, row)
				break
			}
			if got, err := DecodePendingProjection(context.Background(), repo, parts); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(got, PendingStateView{}) {
				t.Fatalf("bad header or embedded entity accepted: %+v %v", got, err)
			}
		})
	}
}

type pendingProjectionCancelContext struct {
	context.Context
	remaining int
}

func (ctx *pendingProjectionCancelContext) Err() error {
	ctx.remaining--
	if ctx.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

func TestPendingProjectionCancellationDuringWork(t *testing.T) {
	repo, entries, view := pendingProjectionFixture(t, 0)
	parts, err := EncodePendingProjection(context.Background(), repo, view)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"project", "encode", "decode"} {
		t.Run(operation, func(t *testing.T) {
			ctx := &pendingProjectionCancelContext{Context: context.Background(), remaining: 50}
			var err error
			switch operation {
			case "project":
				_, err = ProjectPendingSource(ctx, repo, entries)
			case "encode":
				_, err = EncodePendingProjection(ctx, repo, view)
			case "decode":
				_, err = DecodePendingProjection(ctx, repo, parts)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("mid-operation cancellation ignored: %v", err)
			}
		})
	}
}

func TestPendingProjectionEmptyRoundTrip(t *testing.T) {
	repo := HashContent([]byte("empty repository"))
	for _, view := range []PendingStateView{
		{Projection: "on-hold-v1"},
		{Projection: "on-hold-v1", Refs: []Ref{}, Pending: []Pending{}, Snapshots: []Snapshot{}, ArchivedSessions: []SessionArchiveView{}, Hold: []GraphHoldCluster{}, OrphanSessions: []string{}, HoldCounts: map[string]int{}},
	} {
		parts, err := EncodePendingProjection(context.Background(), repo, view)
		if err != nil || len(parts) != 1 {
			t.Fatal("empty projection encoding failed", err)
		}
		got, err := DecodePendingProjection(context.Background(), repo, parts)
		if err != nil || !reflect.DeepEqual(got, view) {
			t.Fatal("empty projection changed null/empty shape", err)
		}
	}
}

func TestPendingProjectionRefKeysStayWithinSourceKeyBudget(t *testing.T) {
	repo := HashContent([]byte("repository"))
	view := PendingStateView{Projection: "on-hold-v1", Refs: []Ref{{RepoID: repo, Kind: RefBranch, Name: strings.Repeat("<", 1024), Target: HashContent([]byte("snapshot"))}}}
	parts, err := EncodePendingProjection(context.Background(), repo, view)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range parts {
		if len(part.Key) > MaxRepositoryViewSourceKeyBytes {
			t.Fatal("canonical key cannot be persisted", len(part.Key))
		}
	}
	got, err := DecodePendingProjection(context.Background(), repo, parts)
	if err != nil || !reflect.DeepEqual(got, view) {
		t.Fatal("valid long ref key changed", err)
	}
}
