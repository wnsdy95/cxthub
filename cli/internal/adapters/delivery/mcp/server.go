// Package mcp contains the stdio MCP server driver (SPINE §7.1 — read-only redefinition as of 2026-07-09).
//
// Started by the cxt mcp subcommand, Claude Code / Codex CLI etc. connect to stdio. The tool exposes only **read-only 4** — write (save/push/checkout/memorize) is automated git hooks and calling by an agent at any point could disrupt the thread. The goal is to use cxthub as a "team knowledge base that the agent queries during its session".
//
// context_list    → current repo commit (snapshot) list (local store)
// context_fetch   → metadata + memory summary + recent chat tail for a specific ref/commit
// memory_load     → MemoryDigest of ref (or closest ancestor if none exists)
// context_search  → team server search (commit message · chat body — origin required)
//
// Transmission: stdio, newline-delimited JSON-RPC 2.0 (MCP as of 2024-11-05). stderr is for logs only.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// ReadStore is the local store read set that MCP requires (consumer-defined — FileStore implements).
type ReadStore interface {
	ListSnapshots(ctx context.Context, repoID, branch string) ([]domain.Snapshot, error)
	GetSnapshot(ctx context.Context, id domain.ContentHash) (domain.Snapshot, error)
	GetDoc(ctx context.Context, hash domain.ContentHash) (domain.SessionDoc, error)
	GetMemory(ctx context.Context, hash domain.ContentHash) (domain.MemoryDigest, error)
	GetRef(ctx context.Context, repoID string, kind domain.RefKind, name string) (domain.Ref, error)
}

// Searcher is the team server search (backendclient implements — hit types share outbound ports).
type Searcher interface {
	Search(ctx context.Context, repoID, query string) ([]outbound.SearchHit, bool, error)
}

// RepoResolver is the current repo/branch resolution (consumer-defined — gitctx(remotecfg wrap) implements).
type RepoResolver interface {
	CurrentRepo(ctx context.Context, cwd string) (domain.Repo, error)
	CurrentBranch(ctx context.Context, cwd string) (string, error)
}

// Server is the stdio MCP server (read-only).
type Server struct {
	gitCtx RepoResolver
	store  ReadStore
	remote Searcher // returns an error if context_search is nil

	in  io.Reader // default os.Stdin (for test injection)
	out io.Writer // default os.Stdout
}

// NewServer creates a read-only MCP server.
func NewServer(gitCtx RepoResolver, store ReadStore, remote Searcher) *Server {
	return &Server{gitCtx: gitCtx, store: store, remote: remote}
}

// SetIO replaces the transmission stream (test-only — nil is ignored).
func (s *Server) SetIO(in io.Reader, out io.Writer) {
	if in != nil {
		s.in = in
	}
	if out != nil {
		s.out = out
	}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Run starts an stdio MCP server (newline-delimited JSON-RPC).
func (s *Server) Run() error {
	in, out := s.in, s.out
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	enc := json.NewEncoder(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue // framing broken lines are ignored (fail-open)
		}
		result, rerr := s.dispatch(req)
		if len(req.ID) == 0 || string(req.ID) == "null" {
			continue // notification — no response
		}
		resp := map[string]interface{}{"jsonrpc": "2.0", "id": json.RawMessage(req.ID)}
		if rerr != nil {
			resp["error"] = rerr
		} else {
			resp["result"] = result
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (s *Server) dispatch(req rpcRequest) (interface{}, *rpcError) {
	switch req.Method {
	case "initialize":
		return map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]interface{}{"name": "cxt", "version": "1"},
		}, nil
	case "notifications/initialized", "notifications/cancelled":
		return nil, nil
	case "ping":
		return map[string]interface{}{}, nil
	case "tools/list":
		return map[string]interface{}{"tools": toolDefs()}, nil
	case "tools/call":
		return s.callTool(req.Params)
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
}

func toolDefs() []map[string]interface{} {
	obj := func(props map[string]interface{}, required ...string) map[string]interface{} {
		schema := map[string]interface{}{"type": "object", "properties": props}
		if len(required) > 0 {
			schema["required"] = required
		}
		return schema
	}
	return []map[string]interface{}{
		{
			"name":        "context_list",
			"description": "List of context commits (agent session snapshots) in the current repo. Used to quickly review what work sessions team members have left.",
			"inputSchema": obj(map[string]interface{}{
				"branch": map[string]interface{}{"type": "string", "description": "Branch filter (empty for all)"},
				"limit":  map[string]interface{}{"type": "number", "description": "Maximum number (default 20)"},
			}),
		},
		{
			"name":        "context_fetch",
			"description": "Context for a specific ref (branch name/commit hash/HEAD): metadata + memory summary + recent conversation tail. Use for deep inspection of a specific task context.",
			"inputSchema": obj(map[string]interface{}{
				"ref":    map[string]interface{}{"type": "string", "description": "Branch name, sha256:hash, short hash, or HEAD (default: current branch)"},
				"events": map[string]interface{}{"type": "number", "description": "Number of recent messages to include (default: 12)"},
			}),
		},
		{
			"name":        "memory_load",
			"description": "Compressed memory for a ref (MemoryDigest: agent compaction summary, key decisions, and unresolved tasks). Falls back to the nearest ancestor.",
			"inputSchema": obj(map[string]interface{}{
				"ref": map[string]interface{}{"type": "string", "description": "Branch name or commit hash (default: current branch)"},
			}),
		},
		{
			"name":        "context_search",
			"description": "Search commit messages and conversation bodies on the team server. Use for team knowledge queries like 'Who touched this bug?'",
			"inputSchema": obj(map[string]interface{}{
				"query": map[string]interface{}{"type": "string", "description": "Search term (at least 2 characters)"},
			}, "query"),
		},
	}
}

func (s *Server) callTool(params json.RawMessage) (interface{}, *rpcError) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid params"}
	}
	ctx := context.Background()
	cwd, _ := os.Getwd()
	text, err := s.runTool(ctx, cwd, call.Name, call.Arguments)
	if err != nil {
		// MCP protocol: Tool execution failure is reported as isError content (not a protocol error).
		return toolText("Error: "+err.Error(), true), nil
	}
	return toolText(text, false), nil
}

func toolText(text string, isErr bool) map[string]interface{} {
	out := map[string]interface{}{
		"content": []map[string]interface{}{{"type": "text", "text": text}},
	}
	if isErr {
		out["isError"] = true
	}
	return out
}

func (s *Server) runTool(ctx context.Context, cwd, name string, rawArgs json.RawMessage) (string, error) {
	var args struct {
		Branch string  `json:"branch"`
		Limit  float64 `json:"limit"`
		Ref    string  `json:"ref"`
		Events float64 `json:"events"`
		Query  string  `json:"query"`
	}
	if len(rawArgs) > 0 {
		_ = json.Unmarshal(rawArgs, &args)
	}
	repo, err := s.gitCtx.CurrentRepo(ctx, cwd)
	if err != nil {
		return "", fmt.Errorf("Current directory is not a cxt repo: %w", err)
	}
	switch name {
	case "context_list":
		return s.toolList(ctx, repo.ID, args.Branch, int(args.Limit))
	case "context_fetch":
		return s.toolFetch(ctx, cwd, repo, args.Ref, int(args.Events))
	case "memory_load":
		return s.toolMemory(ctx, cwd, repo, args.Ref)
	case "context_search":
		if s.remote == nil {
			return "", fmt.Errorf("Team server disconnected — use cxt remote add origin <url> after connecting")
		}
		return s.toolSearch(ctx, repo.ID, args.Query)
	default:
		return "", fmt.Errorf("Unknown tool %q", name)
	}
}

func (s *Server) toolList(ctx context.Context, repoID, branch string, limit int) (string, error) {
	snaps, err := s.store.ListSnapshots(ctx, repoID, branch)
	if err != nil {
		return "", err
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].CreatedAt.After(snaps[j].CreatedAt) })
	if limit <= 0 {
		limit = 20
	}
	var b strings.Builder
	n := 0
	for _, sn := range snaps {
		if sn.Branch == domain.StashBranchLabel || strings.HasPrefix(sn.Message, domain.HookMessagePrefix) {
			continue // Exclude local-only/progress remnants — commit history only
		}
		mem := ""
		if sn.MemoryHash != "" {
			mem = " ◆mem"
		}
		fmt.Fprintf(&b, "%s [%s] %s · %s · %s%s\n",
			shortHash(sn.ID), sn.Branch, firstLine(sn.Message), sn.Author.Name, sn.CreatedAt.Format("2006-01-02 15:04"), mem)
		n++
		if n >= limit {
			break
		}
	}
	if n == 0 {
		return "No commits", nil
	}
	return b.String(), nil
}

func (s *Server) toolFetch(ctx context.Context, cwd string, repo domain.Repo, ref string, events int) (string, error) {
	snap, err := s.resolve(ctx, cwd, repo, ref)
	if err != nil {
		return "", err
	}
	if events <= 0 {
		events = 12
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Commit %s [%s] %s\nAuthor %s · %s · provider %s\n",
		shortHash(snap.ID), snap.Branch, firstLine(snap.Message), snap.Author.Name,
		snap.CreatedAt.Format("2006-01-02 15:04"), snap.Provider)
	if d, ok := s.nearestDigest(ctx, snap); ok {
		fmt.Fprintf(&b, "\n## Memory Summary\n%s\n", truncateRunes(d.Summary, 2000))
	}
	doc, err := s.store.GetDoc(ctx, snap.DocHash)
	if err == nil {
		fmt.Fprintf(&b, "\n## Recent Chat (Last %d Messages)\n", events)
		msgs := make([]domain.Event, 0, events)
		for i := len(doc.CIR.Events) - 1; i >= 0 && len(msgs) < events; i-- {
			ev := doc.CIR.Events[i]
			if ev.Kind == domain.EventMessage && len(ev.Blocks) > 0 && strings.TrimSpace(ev.Blocks[0].Text) != "" {
				msgs = append(msgs, ev)
			}
		}
		for i := len(msgs) - 1; i >= 0; i-- {
			fmt.Fprintf(&b, "[%s] %s\n", msgs[i].Role, truncateRunes(msgs[i].Blocks[0].Text, 500))
		}
	}
	return b.String(), nil
}

func (s *Server) toolMemory(ctx context.Context, cwd string, repo domain.Repo, ref string) (string, error) {
	snap, err := s.resolve(ctx, cwd, repo, ref)
	if err != nil {
		return "", err
	}
	d, ok := s.nearestDigest(ctx, snap)
	if !ok {
		return "No memory digest for this sequence (cxt memorize or automatic commit memorize required)", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "memory digest (Snapshot %s based)\n\n%s\n", shortHash(d.SnapshotID), truncateRunes(d.Summary, 8000))
	if len(d.KeyFacts) > 0 {
		b.WriteString("\nKey facts:\n")
		for _, f := range d.KeyFacts {
			b.WriteString("- " + f + "\n")
		}
	}
	if len(d.OpenTasks) > 0 {
		b.WriteString("\nOpen tasks:\n")
		for _, t := range d.OpenTasks {
			b.WriteString("- " + t + "\n")
		}
	}
	return b.String(), nil
}

func (s *Server) toolSearch(ctx context.Context, repoID, query string) (string, error) {
	if len(strings.TrimSpace(query)) < 2 {
		return "", fmt.Errorf("Search term must be at least 2 characters")
	}
	hits, truncated, err := s.remote.Search(ctx, repoID, query)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "No search results", nil
	}
	var b strings.Builder
	for _, h := range hits {
		role := h.Kind
		if h.Role != "" {
			role = h.Role
		}
		when := h.CreatedAt
		if len(when) >= 10 {
			when = when[:10]
		}
		fmt.Fprintf(&b, "%s [%s] (%s) %s · %s\n", shortHash(domain.ContentHash(h.SnapshotID)), h.Branch, role,
			truncateRunes(h.Snippet, 200), when)
	}
	if truncated {
		b.WriteString("(Limit reached — Narrow down your search term)\n")
	}
	return b.String(), nil
}

// resolve interprets ref (branch/tag/hash/short hash/HEAD/empty value) as a snapshot.
func (s *Server) resolve(ctx context.Context, cwd string, repo domain.Repo, ref string) (domain.Snapshot, error) {
	if ref == "" || ref == "HEAD" {
		branch, _ := s.gitCtx.CurrentBranch(ctx, cwd)
		if branch == "" || branch == "HEAD" {
			branch = repo.DefaultBranch
		}
		if branch == "" {
			branch = "main"
		}
		ref = branch
	}
	if strings.HasPrefix(ref, "sha256:") {
		return s.store.GetSnapshot(ctx, domain.ContentHash(ref))
	}
	if r, err := s.store.GetRef(ctx, repo.ID, domain.RefBranch, ref); err == nil && r.Target != "" {
		return s.store.GetSnapshot(ctx, r.Target)
	}
	if r, err := s.store.GetRef(ctx, repo.ID, domain.RefTag, ref); err == nil && r.Target != "" {
		return s.store.GetSnapshot(ctx, r.Target)
	}
	// Short hash prefix match.
	if len(ref) >= 6 {
		snaps, err := s.store.ListSnapshots(ctx, repo.ID, "")
		if err == nil {
			for _, sn := range snaps {
				if strings.HasPrefix(strings.TrimPrefix(string(sn.ID), "sha256:"), ref) {
					return sn, nil
				}
			}
		}
	}
	return domain.Snapshot{}, fmt.Errorf("ref %q not found (branch/tag/hash)", ref)
}

// nearestDigest finds the closest MemoryDigest to the snapshot itself, following the parent chain.
func (s *Server) nearestDigest(ctx context.Context, snap domain.Snapshot) (domain.MemoryDigest, bool) {
	seen := map[domain.ContentHash]bool{}
	queue := []domain.ContentHash{snap.ID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		sn, err := s.store.GetSnapshot(ctx, id)
		if err != nil {
			continue
		}
		if sn.MemoryHash != "" {
			if d, derr := s.store.GetMemory(ctx, sn.MemoryHash); derr == nil {
				return d, true
			}
		}
		queue = append(queue, sn.ReachabilityParents()...)
	}
	return domain.MemoryDigest{}, false
}

func shortHash(h domain.ContentHash) string {
	x := strings.TrimPrefix(string(h), "sha256:")
	if len(x) > 10 {
		return x[:10]
	}
	return x
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
