package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// segmentsTestTree records every path under root with its type, size and
// content, to prove the command left the repository and the home directory as
// they were.
func segmentsTestTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		tree[path] = fmt.Sprintf("%v %d %d", info.Mode(), info.Size(), info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func segmentsTestRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := segmentsTestRunErr(t, args...)
	if err != nil {
		t.Fatalf("cxt %v: %v", args, err)
	}
	return out
}

// segmentsTestRunErr runs the command from the entry point and returns what
// it printed together with its error.
func segmentsTestRunErr(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = out
	runErr := run(append([]string{"cxt"}, args...))
	os.Stdout = previous
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(raw), runErr
}

// segmentsTestFixture makes a real repository with one commit, and synthetic
// session files in a home directory made for the test. It leaves the process
// in the repository.
func segmentsTestFixture(t *testing.T) (base, repo, sha string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, hooks := filepath.Join(base, "home"), filepath.Join(base, "hooks")
	repo = filepath.Join(base, "app")
	for _, dir := range []string{home, repo, hooks} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	mainTestGit(t, repo, "init", "-b", "main")
	for _, setting := range [][2]string{{"user.name", "cxt test"}, {"user.email", "cxt@example.test"}, {"core.hooksPath", hooks}, {"gc.auto", "0"}, {"commit.gpgsign", "false"}} {
		mainTestGit(t, repo, "config", setting[0], setting[1])
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("synthetic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mainTestGit(t, repo, "add", "README.md")
	commit := exec.Command("git", "-C", repo, "commit", "-m", "first")
	commit.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_DATE=2031-01-02T03:00:05Z", "GIT_COMMITTER_DATE=2031-01-02T03:00:05Z")
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	head, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	sha = strings.TrimSpace(string(head))

	record := func(kind, uuid, session, prompt, at string, content any) string {
		line := map[string]any{"type": kind, "uuid": uuid, "sessionId": session, "timestamp": "2031-01-02T03:00:" + at + "Z", "cwd": repo, "gitBranch": "main", "message": map[string]any{"content": content}}
		if prompt != "" {
			line["promptId"] = prompt
		}
		raw, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw) + "\n"
	}
	shared := func(session string) string {
		return record("user", "m1", session, "p1", "00.000", "commit it") +
			record("assistant", "m2", session, "", "01.000", []any{map[string]any{"type": "tool_use", "id": "call-1", "name": "Bash"}}) +
			record("user", "m3", session, "p1", "06.000", []any{map[string]any{"type": "tool_result", "tool_use_id": "call-1", "content": "[main (root-commit) " + sha[:7] + "] first"}}) +
			record("assistant", "m4", session, "", "07.000", []any{map[string]any{"type": "text", "text": "committed"}})
	}
	projects := filepath.Join(home, ".claude", "projects", "-synthetic")
	files := map[string]string{
		filepath.Join(projects, "original.jsonl"): shared("original"),
		// A fork copies the earlier records and adds its own.
		filepath.Join(projects, "fork.jsonl"):                                  shared("fork") + record("user", "f1", "fork", "p2", "40.000", "continue elsewhere"),
		filepath.Join(projects, "original", "subagents", "agent-helper.jsonl"): record("user", "h1", "original", "", "02.000", "sub task"),
		filepath.Join(projects, "fork", "subagents", "agent-helper.jsonl"):     record("user", "h1", "original", "", "02.000", "sub task"),
		filepath.Join(projects, "unreadable.jsonl"):                            "<not a session record>\n",
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Chdir(repo)
	return base, repo, sha
}

// The whole command, from the entry point.
func TestSegmentsCommandReadsRecordsAndWritesNothing(t *testing.T) {
	base, repo, sha := segmentsTestFixture(t)
	before := segmentsTestTree(t, base)
	first := segmentsTestRun(t, "segments", "--json")
	second := segmentsTestRun(t, "segments", "--json")
	text := segmentsTestRun(t, "segments")
	if after := segmentsTestTree(t, base); !reflect.DeepEqual(before, after) {
		t.Fatal("cxt segments changed a file in the repository or the home directory")
	}
	if first != second {
		t.Fatalf("two runs over the same records differ:\n%s\n%s", first, second)
	}
	var report domain.SegmentReport
	if err := json.Unmarshal([]byte(first), &report); err != nil {
		t.Fatalf("output is not a report: %v\n%s", err, first)
	}
	summary := report.Summary
	if summary.Sessions.Claude != 2 || summary.Sessions.ClaudeSub != 1 || summary.LinkedCommits != 1 || summary.LinkedByPrintedNumber != 1 ||
		summary.CommitsWithOneSession != 1 || summary.HeadLogCommits != 1 || summary.CopiedRecords != 4 || summary.SessionFileCopies != 1 ||
		summary.Spawns != 1 || summary.UnreadRecords != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if report.Repository.Root != repo || len(report.Commits) != 1 || report.Commits[0].SHA != sha || report.Commits[0].Sessions[0].ID != "original" || report.Commits[0].Local.State != domain.LocalCommitOnBranch {
		t.Fatalf("commits = %+v", report.Commits)
	}
	for _, session := range report.Sessions {
		switch session.ID {
		case "original":
			if len(session.Segments) != 1 || session.Segments[0].First != "m1" || session.Segments[0].Last != "m4" || !session.Segments[0].ClosesTurn || session.Uncommitted != nil {
				t.Fatalf("original = %+v", session)
			}
		case "fork":
			if len(session.Segments) != 0 || session.Records != 1 || session.Uncommitted == nil || session.Uncommitted.Places[0].Branch != "main" {
				t.Fatalf("fork = %+v", session)
			}
		case "helper":
			if session.Parent != "original" || session.FileCopies != 1 {
				t.Fatalf("helper = %+v", session)
			}
		}
	}
	if !strings.Contains(text, "linked by printed-number") || !strings.Contains(text, "Unread session records (1)") {
		t.Fatalf("text output:\n%s", text)
	}
}

// segmentsTestServer is a server made for the test. It keeps reports, and it
// takes documents the way the real one does: it says which chunks it lacks,
// receives those, and then the manifest that names them.
type segmentsTestServer struct {
	mu        sync.Mutex
	puts      []segmentsTestRequest
	summaries []segmentsTestRequest
	chunks    map[string][]byte
	manifests map[string]segmentsTestManifest
	uploaded  int // chunk bodies received
	requests  int // every request of any kind
	branches  [][]domain.BranchRegistration
	worktrees []string
	// collectors holds what the collector said about itself.
	collectors []string
	answer     func(session string) (int, string)
	token      string
}

type segmentsTestRequest struct {
	method, path, auth string
	body               string
}

type segmentsTestManifest struct {
	Hash     string          `json:"hash"`
	Envelope json.RawMessage `json:"envelope"`
	Chunks   []string        `json:"chunks"`
}

func (s *segmentsTestServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer "+s.token {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"unauthorized"}}`))
		return
	}
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/push/negotiate"):
		var req struct {
			DocHaves   []string `json:"doc_haves"`
			ChunkHaves []string `json:"chunk_haves"`
		}
		_ = json.Unmarshal(raw, &req)
		docWants, chunkWants := []string{}, []string{}
		for _, hash := range req.DocHaves {
			if _, ok := s.manifests[hash]; !ok {
				docWants = append(docWants, hash)
			}
		}
		for _, hash := range req.ChunkHaves {
			if _, ok := s.chunks[hash]; !ok {
				chunkWants = append(chunkWants, hash)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"snapshot_wants": []string{}, "doc_wants": docWants, "chunk_wants": chunkWants,
			"chunks_supported": true, "bounded_chunks_supported": true, "chunk_formats_supported": []string{"cxt-doc-chunks-v1", "cxt-doc-chunks-v2"}, "cir_versions_supported": []string{"1", "2"}})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/push/chunks"):
		var req struct {
			Chunks []struct {
				Hash string `json:"hash"`
				Data []byte `json:"data"`
			} `json:"chunks"`
		}
		_ = json.Unmarshal(raw, &req)
		for _, chunk := range req.Chunks {
			s.chunks[chunk.Hash] = chunk.Data
			s.uploaded++
		}
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/push/objects"):
		var req struct {
			ChunkedDocs []segmentsTestManifest `json:"chunked_docs"`
		}
		_ = json.Unmarshal(raw, &req)
		for _, manifest := range req.ChunkedDocs {
			s.manifests[manifest.Hash] = manifest
		}
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/worktrees"):
		s.worktrees = append(s.worktrees, string(raw))
		_, _ = w.Write([]byte(`[]`))
	case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/collector"):
		s.collectors = append(s.collectors, string(raw))
		_, _ = w.Write([]byte(`{"heard":true}`))
	case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/branch-ids"):
		var req struct {
			Branches []domain.BranchRegistration `json:"branches"`
		}
		_ = json.Unmarshal(raw, &req)
		s.branches = append(s.branches, req.Branches)
		results := []domain.BranchRegistrationResult{}
		for _, branch := range req.Branches {
			results = append(results, domain.BranchRegistrationResult{Name: branch.Name, LocalID: branch.LocalID, LedgerID: branch.LocalID, Outcome: "opened"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"branches": results})
	case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/anchored-sessions/claude/") && strings.Contains(r.URL.Path, "/summaries/"):
		// A summary of a session at a commit, sent after the session's report.
		s.summaries = append(s.summaries, segmentsTestRequest{r.Method, r.URL.Path, r.Header.Get("Authorization"), string(raw)})
		_, _ = w.Write([]byte(`{"body_hash":"sha256:` + strings.Repeat("0", 64) + `","changed":true}`))
	case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/anchored-sessions/claude/"):
		s.puts = append(s.puts, segmentsTestRequest{r.Method, r.URL.Path, r.Header.Get("Authorization"), string(raw)})
		status, body := s.answer(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"not found"}}`))
	}
}

// document rebuilds a stored document from its manifest and chunks.
func (s *segmentsTestServer) document(t *testing.T, hash string) (string, []map[string]any) {
	t.Helper()
	manifest, ok := s.manifests[hash]
	if !ok {
		t.Fatalf("document %s was not uploaded", hash)
	}
	var stream strings.Builder
	for _, chunk := range manifest.Chunks {
		stream.Write(s.chunks[chunk])
	}
	raw := `{"envelope":` + string(manifest.Envelope) + `,"events":[` + stream.String() + `]}`
	if got := string(domain.HashContent([]byte(raw))); got != hash {
		t.Fatalf("the uploaded chunks rebuild %s, not %s", got, hash)
	}
	var doc struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	return raw, doc.Events
}

// segmentsTestConfig reads one setting of the repository's own configuration.
func segmentsTestConfig(t *testing.T, repo, key string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "config", "--local", "--get", key)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git config %s: %v", key, err)
	}
	return strings.TrimSpace(string(out))
}

// segmentsTestPushTree is the tree an upload must leave as it was: everything
// but Git's configuration file, where branch IDs go, and the lock taken while
// they are assigned.
func segmentsTestPushTree(t *testing.T, base, repo string) map[string]string {
	t.Helper()
	tree := segmentsTestTree(t, base)
	for _, path := range []string{filepath.Join(repo, ".git"), filepath.Join(repo, ".git", "config"), filepath.Join(repo, ".git", "cxt"), filepath.Join(repo, ".git", "cxt", "branch-ids.lock")} {
		delete(tree, path)
	}
	// The collector a hook wakes keeps its memory and its lock in the same place.
	for path := range tree {
		if strings.HasPrefix(path, filepath.Join(repo, ".git", "cxt")+string(filepath.Separator)) {
			delete(tree, path)
		}
	}
	return tree
}

// The upload, from the entry point, against a server made for the test: one
// report per session, its conversation stored once, nothing but names in the
// report, and nothing written here but branch IDs in Git's own configuration.
func TestSegmentsPushUploadsEachSessionAndWritesOnlyBranchIDs(t *testing.T) {
	base, repo, sha := segmentsTestFixture(t)
	// The branch is on origin, and a second branch never left this computer.
	origin := filepath.Join(base, "origin.git")
	mainTestGit(t, base, "init", "--bare", "-b", "main", origin)
	mainTestGit(t, repo, "remote", "add", "origin", origin)
	mainTestGit(t, repo, "push", "-q", "-u", "origin", "main")
	mainTestGit(t, repo, "branch", "never-pushed")
	config, err := os.ReadFile(filepath.Join(repo, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	stored := `{"revision":1,"body_hash":"sha256:` + strings.Repeat("0", 64) + `","changed":true}`
	fake := &segmentsTestServer{chunks: map[string][]byte{}, manifests: map[string]segmentsTestManifest{}, token: "synthetic-token",
		answer: func(string) (int, string) { return http.StatusOK, stored }}
	server := httptest.NewServer(fake)
	defer server.Close()
	t.Setenv("CXT_REMOTE", server.URL+"/api/v1")
	t.Setenv("CXT_TOKEN", "synthetic-token")

	before := segmentsTestPushTree(t, base, repo)
	out := segmentsTestRun(t, "segments", "push", "--json")
	if after := segmentsTestPushTree(t, base, repo); !reflect.DeepEqual(before, after) {
		t.Fatal("cxt segments push changed a file other than Git's branch configuration")
	}
	var result domain.SegmentPushResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("output is not a result: %v\n%s", err, out)
	}
	// Every branch got an ID in Git's own configuration, and what was there stays.
	after, err := os.ReadFile(filepath.Join(repo, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(after), strings.TrimRight(string(config), "\n")) {
		t.Fatalf("the configuration that was there changed:\n%s", after)
	}
	mainID := segmentsTestConfig(t, repo, "branch.main.cxtBranchId")
	otherID := segmentsTestConfig(t, repo, "branch.never-pushed.cxtBranchId")
	if !domain.ValidBranchID(mainID) || !domain.ValidBranchID(otherID) || mainID == otherID ||
		segmentsTestConfig(t, repo, "branch.main.cxtBranchIdIssuedFor") != "main" {
		t.Fatalf("branch IDs: main=%q never-pushed=%q", mainID, otherID)
	}
	// Only the branch Git shows on origin is registered.
	if want := (&domain.SegmentPushBranches{WithID: 2, Assigned: 2, OnOrigin: 1, Registered: 1}); result.Branches == nil || *result.Branches != *want {
		t.Fatalf("branches = %+v", result.Branches)
	}
	if !reflect.DeepEqual(fake.branches, [][]domain.BranchRegistration{{{Name: "main", LocalID: mainID}}}) {
		t.Fatalf("registered = %+v", fake.branches)
	}
	// The worktree is listed by its folder's name and branch, never by its path.
	if result.Worktrees == nil || *result.Worktrees != (domain.SegmentPushWorktrees{Listed: 1, Reported: true}) || len(fake.worktrees) != 1 ||
		!strings.Contains(fake.worktrees[0], `"worktrees":[{"name":"`+filepath.Base(repo)+`","branch":"main"}]`) || strings.Contains(fake.worktrees[0], base) {
		t.Fatalf("worktrees = %+v sent=%q", result.Worktrees, fake.worktrees)
	}
	if result.Stored != 3 || result.Unchanged != 0 || result.Refused != 0 || result.ContentNotUploaded != 0 || len(result.Sessions) != 3 || len(result.Unread) != 1 {
		t.Fatalf("result = %+v", result)
	}
	if len(fake.puts) != 3 {
		t.Fatalf("reports = %+v", fake.puts)
	}
	// The one session that made a commit is written down at that commit,
	// after its report, from its own conversation.
	if result.Summaries != 1 || len(fake.summaries) != 1 || !strings.Contains(fake.summaries[0].path, "/anchored-sessions/claude/original/summaries/") ||
		fake.summaries[0].auth != "Bearer synthetic-token" || !strings.Contains(fake.summaries[0].body, "committed") {
		t.Fatalf("summaries = %d sent=%+v", result.Summaries, fake.summaries)
	}
	bodies := map[string]domain.AnchoredSessionBody{}
	for _, sent := range fake.puts {
		if !strings.HasPrefix(sent.path, "/api/v1/repos/") {
			t.Fatalf("request = %+v", sent)
		}
		// A worktree is a name on the server; no folder of this computer is sent.
		if strings.Contains(sent.body, base) || strings.Contains(sent.body, filepath.Dir(repo)) {
			t.Fatalf("a path was uploaded: %s", sent.body)
		}
		var body domain.AnchoredSessionBody
		if err := json.Unmarshal([]byte(sent.body), &body); err != nil {
			t.Fatalf("body is not a report: %v\n%s", err, sent.body)
		}
		bodies[sent.path[strings.LastIndex(sent.path, "/")+1:]] = body
	}
	original, fork, helper := bodies["original"], bodies["fork"], bodies["helper"]
	if original.RuleVersion != domain.AnchoredSessionRuleVersion || original.Records != 4 || len(original.Segments) != 1 || original.Uncommitted != nil {
		t.Fatalf("original = %+v", original)
	}
	if commit := original.Segments[0].Commits[0]; commit.SHA != sha || commit.Rule != domain.SegmentLinkPrintedNumber || commit.Subject != "first" ||
		commit.AuthorEmail != "cxt@example.test" || commit.AuthoredAt != "2031-01-02T03:00:05Z" || commit.CreatedAt != "2031-01-02T03:00:05Z" {
		t.Fatalf("commit = %+v", commit)
	}
	if fork.CopiedRecords != 4 || !reflect.DeepEqual(fork.CopiedFrom, []string{"original"}) || fork.Records != 1 || fork.Uncommitted == nil ||
		!reflect.DeepEqual(fork.Uncommitted.Places, []domain.AnchoredPlace{{Worktree: "app", Branch: "main", Rule: domain.SessionPlaceRecorded}}) {
		t.Fatalf("fork = %+v", fork)
	}
	if !helper.Sub || helper.Parent != "original" || helper.Spawn == nil || helper.FileCopies != 1 {
		t.Fatalf("helper = %+v", helper)
	}
	if host, _ := os.Hostname(); original.Machine != host {
		t.Fatalf("machine = %q, host = %q", original.Machine, host)
	}

	// Each session's conversation is one document of its own records. The four
	// records of the original give four events, all in its one segment.
	if original.Content == nil || original.Content.Events != 4 || original.Segments[0].FirstEvent != 0 || original.Segments[0].Events != 4 {
		t.Fatalf("original content = %+v segment = %+v", original.Content, original.Segments[0].AnchoredRange)
	}
	raw, events := fake.document(t, string(original.Content.Last()))
	if len(events) != 4 || events[0]["kind"] != "message" || events[1]["kind"] != "tool_call" || events[2]["kind"] != "tool_result" || !strings.Contains(raw, "committed") {
		t.Fatalf("original document: %s", raw)
	}
	// The fork's copied records are stored once, with the original: its own
	// document holds only what it added.
	if fork.Content == nil || fork.Content.Events != 1 || fork.Uncommitted.FirstEvent != 0 || fork.Uncommitted.Events != 1 {
		t.Fatalf("fork content = %+v part = %+v", fork.Content, fork.Uncommitted.AnchoredRange)
	}
	if raw, events := fake.document(t, string(fork.Content.Last())); len(events) != 1 || !strings.Contains(raw, "continue elsewhere") || strings.Contains(raw, "commit it") {
		t.Fatalf("fork document: %s", raw)
	}
	if helper.Content == nil || helper.Content.Events != 1 {
		t.Fatalf("helper content = %+v", helper.Content)
	}
	if len(fake.manifests) != 3 {
		t.Fatalf("documents = %d", len(fake.manifests))
	}
	// A document names its session, and no folder of this computer.
	for hash, manifest := range fake.manifests {
		if strings.Contains(string(manifest.Envelope), base) || strings.Contains(string(manifest.Envelope), `"cwd":"/`) || !strings.Contains(string(manifest.Envelope), `"session_origin_id"`) {
			t.Fatalf("document %s envelope: %s", hash, manifest.Envelope)
		}
	}

	// The same records again: every document is already there, so no chunk is sent.
	fake.mu.Lock()
	fake.puts, fake.uploaded = nil, 0
	fake.answer = func(session string) (int, string) {
		if session == "fork" {
			return http.StatusConflict, `{"error":{"code":"conflict","message":"conflict"}}`
		}
		return http.StatusOK, `{"revision":1,"body_hash":"sha256:` + strings.Repeat("0", 64) + `","changed":false}`
	}
	fake.mu.Unlock()
	// A report the server refuses is listed and fails the command; the others are still sent.
	text, err := segmentsTestRunErr(t, "segments", "push")
	if err == nil || !strings.Contains(err.Error(), "1 of 3 session reports were refused") || len(fake.puts) != 3 || fake.uploaded != 0 ||
		!strings.Contains(text, "Session reports: 0 stored, 2 unchanged, 1 refused (3 sessions)") || !strings.Contains(text, "  claude fork: ") ||
		!strings.Contains(text, "Conversation: uploaded for 3 sessions, not uploaded for 0") ||
		!strings.Contains(text, "Branch IDs: 2 branches have one (0 new) · 1 of 1 on origin registered") {
		t.Fatalf("err=%v reports=%d chunks=%d\n%s", err, len(fake.puts), fake.uploaded, text)
	}

	// A caller the server does not accept stops the run at the first request.
	fake.mu.Lock()
	fake.puts, fake.token = nil, "another-token"
	fake.mu.Unlock()
	if _, err := segmentsTestRunErr(t, "segments", "push"); err == nil || len(fake.puts) != 0 || !strings.Contains(err.Error(), "stopped after 0 of 3 session reports") {
		t.Fatalf("err=%v reports=%d", err, len(fake.puts))
	}
	if after := segmentsTestPushTree(t, base, repo); !reflect.DeepEqual(before, after) {
		t.Fatal("a failed upload changed a file other than Git's branch configuration")
	}
	// The IDs are the same on every run.
	if again := segmentsTestConfig(t, repo, "branch.main.cxtBranchId"); again != mainID {
		t.Fatalf("the ID of main changed from %s to %s", mainID, again)
	}
}

// The collector a hook wakes, from the entry point: the same reports as on
// request for sessions recorded since it began, and after that only what changed.
func TestSegmentCollectorUploadsOnlyWhatChanged(t *testing.T) {
	base, repo, _ := segmentsTestFixture(t)
	// The collector runs only where cxt was set up.
	if _, err := gitctx.NewSetupMarker().MarkSetUp(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	stored := `{"revision":1,"body_hash":"sha256:` + strings.Repeat("0", 64) + `","changed":true}`
	fake := &segmentsTestServer{chunks: map[string][]byte{}, manifests: map[string]segmentsTestManifest{}, token: "synthetic-token",
		answer: func(string) (int, string) { return http.StatusOK, stored }}
	server := httptest.NewServer(fake)
	defer server.Close()
	t.Setenv("CXT_REMOTE", server.URL+"/api/v1")
	t.Setenv("CXT_TOKEN", "synthetic-token")
	statePath := filepath.Join(repo, ".git", "cxt", "collector-v1.json")
	// A run starts at once only when the last one is a while ago; the test
	// moves the last run back instead of waiting.
	collect := func() {
		t.Helper()
		if raw, err := os.ReadFile(statePath); err == nil {
			var state domain.CollectorState
			if err := json.Unmarshal(raw, &state); err != nil || state.LastRun == nil {
				t.Fatalf("collector state: %v\n%s", err, raw)
			}
			state.LastRun.At = "2026-01-01T00:00:00Z"
			moved, _ := json.Marshal(state)
			if err := os.WriteFile(statePath, moved, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		fake.mu.Lock()
		fake.puts, fake.requests, fake.uploaded = nil, 0, 0
		fake.mu.Unlock()
		if _, err := segmentsTestRunErr(t, "git-hook", "segment-collect"); err != nil {
			t.Fatal(err)
		}
	}
	// Beside Git's own directory the collector writes two more places: the
	// user's cache of what it made of unchanged sources, and the copies of
	// what it sent, kept under .cxt/sessions where cxt is set up.
	cacheDir := filepath.Join(base, "home", ".cache", "cxthub", "session-records-v1")
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "home", ".cache"))
	tree := func() map[string]string {
		t.Helper()
		all := segmentsTestPushTree(t, base, repo)
		for path := range all {
			for _, own := range []string{filepath.Join(base, "home", ".cache"), filepath.Join(repo, ".cxt", "sessions")} {
				if path == own || strings.HasPrefix(path, own+string(filepath.Separator)) {
					delete(all, path)
				}
			}
		}
		return all
	}
	before := tree()

	// The fixture's sessions are recorded after automatic reading begins (now),
	// so the first run sends all three, each with its conversation.
	collect()
	if len(fake.puts) != 3 || len(fake.manifests) != 3 || len(fake.worktrees) != 1 || len(fake.summaries) != 1 {
		t.Fatalf("first run: reports=%d documents=%d worktree lists=%d summaries=%d", len(fake.puts), len(fake.manifests), len(fake.worktrees), len(fake.summaries))
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state domain.CollectorState
	if err := json.Unmarshal(raw, &state); err != nil || state.Version != domain.CollectorStateVersion || state.EnabledAt == "" || len(state.Sessions) != 3 ||
		state.LastRun == nil || state.LastRun.Stored != 3 || state.LastRun.Error != "" {
		t.Fatalf("state after the first run: %v\n%s", err, raw)
	}
	for key, session := range state.Sessions {
		if session.Sent == "" || session.Content == nil {
			t.Fatalf("%s is not remembered: %+v", key, session)
		}
	}
	// The memory holds what was sent, and no folder of this computer.
	if strings.Contains(string(raw), base) {
		t.Fatalf("the state names a path: %s", raw)
	}

	// The collector also said once who it is: this program's version, the
	// pushes of this clone, which made none, and nothing that went wrong.
	if len(fake.collectors) != 1 || !strings.Contains(fake.collectors[0], `"cxt_version":"`+version+`"`) || !strings.Contains(fake.collectors[0], `"pushes":[]`) ||
		strings.Contains(fake.collectors[0], "run_error") || strings.Contains(fake.collectors[0], base) || state.Reported == "" {
		t.Fatalf("what the collector said about itself: %q, remembered %q", fake.collectors, state.Reported)
	}

	// Nothing changed: the server is not asked anything.
	collect()
	if fake.requests != 0 {
		t.Fatalf("second run made %d requests", fake.requests)
	}

	// One session went on: its document grows by what was appended and its
	// report is replaced. Nothing else is sent.
	appended := `{"type":"user","uuid":"f2","sessionId":"fork","timestamp":"2031-01-02T03:00:50.000Z","cwd":"` + repo + `","gitBranch":"main","promptId":"p3","message":{"content":"and one more"}}` + "\n"
	forkFile := filepath.Join(base, "home", ".claude", "projects", "-synthetic", "fork.jsonl")
	file, err := os.OpenFile(forkFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(appended); err != nil {
		t.Fatal(err)
	}
	file.Close()
	before = tree()
	collect()
	if len(fake.puts) != 1 || !strings.HasSuffix(fake.puts[0].path, "/anchored-sessions/claude/fork") || fake.uploaded == 0 {
		t.Fatalf("after one session went on: reports=%+v chunks=%d", fake.puts, fake.uploaded)
	}
	var body domain.AnchoredSessionBody
	if err := json.Unmarshal([]byte(fake.puts[0].body), &body); err != nil || body.Records != 2 || body.Content == nil || body.Content.Events != 2 {
		t.Fatalf("the grown report: %v %+v", err, body)
	}
	if after := tree(); !reflect.DeepEqual(before, after) {
		t.Fatal("the collector changed a file outside Git's own directory, its cache and .cxt/sessions")
	}
	// The cache is this user's alone and holds no conversation.
	entries, err := os.ReadDir(cacheDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("read cache: %v %v", entries, err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("cache entry %s: %v %v", entry.Name(), info, err)
		}
		content := string(mustRead(t, filepath.Join(cacheDir, entry.Name())))
		for _, text := range []string{"commit it", "continue elsewhere", "and one more", "sub task"} {
			if strings.Contains(content, text) {
				t.Fatalf("cache entry %s holds conversation text %q", entry.Name(), text)
			}
		}
	}
	// A caller the server does not accept: the run ends quietly and the state says why.
	fake.mu.Lock()
	fake.token = "another-token"
	fake.mu.Unlock()
	if err := os.WriteFile(forkFile, []byte(strings.Replace(string(mustRead(t, forkFile)), "and one more", "and one more", 1)+strings.Replace(appended, `"f2"`, `"f3"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	collect()
	raw = mustRead(t, statePath)
	if err := json.Unmarshal(raw, &state); err != nil || state.LastRun == nil || !strings.Contains(state.LastRun.Error, "401") {
		t.Fatalf("state after a refused caller: %v\n%s", err, raw)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Where cxt is set up, an upload keeps a copy of what it sent for each
// session under .cxt/sessions, and cxt segments log reads the copies back
// without the server.
func TestSegmentsPushKeepsCopiesThatSegmentsLogReads(t *testing.T) {
	base, repo, _ := segmentsTestFixture(t)
	if err := os.MkdirAll(filepath.Join(repo, ".cxt"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".cxt", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stored := `{"revision":1,"body_hash":"sha256:` + strings.Repeat("0", 64) + `","changed":true}`
	fake := &segmentsTestServer{chunks: map[string][]byte{}, manifests: map[string]segmentsTestManifest{}, token: "synthetic-token",
		answer: func(string) (int, string) { return http.StatusOK, stored }}
	server := httptest.NewServer(fake)
	defer server.Close()
	t.Setenv("CXT_REMOTE", server.URL+"/api/v1")
	t.Setenv("CXT_TOKEN", "synthetic-token")

	var result domain.SegmentPushResult
	if err := json.Unmarshal([]byte(segmentsTestRun(t, "segments", "push", "--json")), &result); err != nil {
		t.Fatal(err)
	}
	if result.LocalRecords == nil || result.LocalRecords.Kept != 3 || result.LocalRecords.NotKept != "" {
		t.Fatalf("local records = %+v", result.LocalRecords)
	}
	dir := filepath.Join(repo, ".cxt", "sessions", "claude", "original")
	for _, name := range []string{"report.json", "log.jsonl"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", name, info, err)
		}
	}
	// The conversation is kept in the one document it was sent in, the
	// user's alone, under the hash the report names it by.
	documents, err := os.ReadDir(filepath.Join(dir, "conversation"))
	if err != nil || len(documents) != 1 || !strings.HasSuffix(documents[0].Name(), ".json.gz") {
		t.Fatalf("conversation copies: %v %v", documents, err)
	}
	if info, err := documents[0].Info(); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("%s: %v %v", documents[0].Name(), info, err)
	}
	// The copy of the report is the report that was sent: it names no folder
	// of this computer either.
	report, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil || strings.Contains(string(report), base) || !strings.Contains(string(report), `"session_id": "original"`) {
		t.Fatalf("report copy: %v\n%s", err, report)
	}

	// The same upload again sends the same thing: no new log line.
	segmentsTestRun(t, "segments", "push", "--json")
	var listed struct {
		Sessions []domain.SessionUploadListing `json:"sessions"`
	}
	server.Close() // the log is read from this computer alone
	if err := json.Unmarshal([]byte(segmentsTestRun(t, "segments", "log", "--json")), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Sessions) != 3 {
		t.Fatalf("listed = %+v", listed.Sessions)
	}
	for _, session := range listed.Sessions {
		if !session.ReportKept || session.Lines != 1 || len(session.Log) != 1 || session.Log[0].Report != domain.SegmentPushStored || session.Log[0].Revision != 1 || session.Log[0].Automatic {
			t.Fatalf("session = %+v", session)
		}
	}
	one := segmentsTestRun(t, "segments", "log", "claude", "original")
	for _, want := range []string{"claude original", "report copy        1 segments", "conversation copy  kept; the report names", "requested  report stored (revision 1) · conversation of", "1 summaries stored"} {
		if !strings.Contains(one, want) {
			t.Fatalf("missing %q in:\n%s", want, one)
		}
	}
	if _, err := segmentsTestRunErr(t, "segments", "log", "claude", "never-uploaded"); err == nil || !strings.Contains(err.Error(), "no upload record is kept for claude never-uploaded") {
		t.Fatalf("a session nothing is kept for: %v", err)
	}
}
