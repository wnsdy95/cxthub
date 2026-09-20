package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/memory"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type promptReadFunc func(context.Context, string, domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error)

func (f promptReadFunc) QueryEffectiveMemory(c context.Context, r string, q domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
	return f(c, r, q)
}

type promptCode struct {
	oid, cwd string
	calls    int
}

func (p *promptCode) CurrentCommit(ctx context.Context, cwd string) (string, error) {
	p.calls++
	p.cwd = cwd
	return p.oid, ctx.Err()
}
func promptHash(s string) domain.ContentHash { return domain.HashContent([]byte(s)) }
func promptOID(n int) string                 { return fmt.Sprintf("%040x", n) }
func promptItem(text, state string) domain.EffectiveMemoryItem {
	return domain.EffectiveMemoryItem{ID: promptHash(text), SourceSnapshot: promptHash("source"), Kind: "code", Text: text, State: state, Reason: "declared_scope_matches", Code: &domain.MemoryCodeScope{Commit: promptOID(1), Paths: []string{"feature.go"}}}
}
func promptPage(q domain.EffectiveMemoryRequest, items ...domain.EffectiveMemoryItem) domain.EffectiveMemoryPage {
	p := domain.EffectiveMemoryPage{Content: q.Content, Selection: q.Selection, StateHash: promptHash(string(q.Selection.SnapshotID) + "generation"), LineageHash: promptHash("lineage"), Items: items, Total: len(items)}
	p.Revision.Graph = 2
	p.Revision.Evidence = 3
	return p
}
func TestEffectivePromptBoundedSelectionAndFailures(t *testing.T) {
	root, other := promptHash("root"), promptHash("other")
	for _, mode := range []string{"bounded", "old server", "state changed", "evidence changed", "cross root changed", "bad selection", "bad kind", "offline", "cursor loop", "duplicate page"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			roots := map[domain.ContentHash]int{}
			code := &promptCode{oid: promptOID(20)}
			reader := promptReadFunc(func(ctx context.Context, repo string, q domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
				calls++
				roots[q.Selection.SnapshotID]++
				if q.Selection.CodeCommit != code.oid || q.Content != "claims" || q.Limit != 50 {
					t.Fatal(q)
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > effectivePromptTimeout {
					t.Fatal("missing whole operation deadline")
				}
				items := make([]domain.EffectiveMemoryItem, 50)
				for i := range items {
					items[i] = promptItem(fmt.Sprintf("claim %s %d %d", q.Selection.SnapshotID, calls, i), "applied")
				}
				p := promptPage(q, items...)
				p.Total = 1000
				p.NextCursor = fmt.Sprint(calls)
				switch mode {
				case "old server":
					p.Content = ""
				case "state changed":
					if calls == 2 {
						p.StateHash = promptHash("changed")
					}
				case "evidence changed":
					if calls == 2 {
						p.Revision.Evidence++
					}
				case "cross root changed":
					if q.Selection.SnapshotID == other {
						p.Revision.Graph++
					}
				case "bad selection":
					p.Selection.CodeCommit = promptOID(99)
				case "bad kind":
					p.Items[0].Kind = "legacy_summary"
				case "offline":
					return p, errors.New("offline")
				case "cursor loop":
					p.NextCursor = "same"
				case "duplicate page":
					p.Items[0] = promptItem("duplicate", "applied")
					if calls > 1 {
						p.Items[0] = promptItem("duplicate", "applied")
					}
				}
				return p, nil
			})
			p, err := NewMemoryPromptService(reader, code, nil).prepare(context.Background(), string(promptHash("repo")), "/worker-two", root, other)
			if err != nil {
				t.Fatal(err)
			}
			if calls > 4 || code.cwd != "/worker-two" {
				t.Fatal(calls, code)
			}
			if mode == "bounded" {
				if calls != 4 || roots[root] != 2 || roots[other] != 2 || len(p.items) != 200 || !p.partial {
					t.Fatal(calls, roots, len(p.items), p.partial)
				}
				out := p.render(48<<10, func(n int) string {
					return renderSeedDigest(domain.MemoryDigest{Summary: strings.Repeat("archived ", 10000)}, 3, n)
				})
				if len(out) > 48<<10 || !strings.HasPrefix(out, seedSummaryPrefix) || !strings.Contains(out, "Additional claims omitted") || !strings.Contains(out, "Historical memory and conversation") {
					t.Fatal("bad bounded prompt", len(out))
				}
			} else if len(p.items) != 0 || p.status == "server_assessed" {
				t.Fatal("incoherent results survived", p.status, len(p.items))
			}
		})
	}
}

func TestEffectivePromptUnknownCodeCancellationAndConcurrentCheckout(t *testing.T) {
	ctx := context.Background()
	calls := 0
	code := &promptCode{}
	reader := promptReadFunc(func(ctx context.Context, _ string, q domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
		calls++
		return promptPage(q), nil
	})
	svc := NewMemoryPromptService(reader, code, nil)
	p, err := svc.prepare(ctx, "repo", "/target", promptHash("root"))
	if err != nil || calls != 0 || p.status != "code_position_unknown" {
		t.Fatal(p, err, calls)
	}
	code.oid = promptOID(2)
	if !errors.Is(p.check(ctx), domain.ErrSelectionChanged) {
		t.Fatal("new code appeared without invalidating unknown selection")
	}
	p, err = svc.prepare(ctx, "repo", "/target", promptHash("root"))
	if err != nil {
		t.Fatal(err)
	}
	code.oid = promptOID(3)
	if !errors.Is(p.check(ctx), domain.ErrSelectionChanged) {
		t.Fatal("concurrent checkout accepted")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = svc.prepare(canceled, "repo", "/target", promptHash("root")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	code.oid = promptOID(2)
	cancelReader := promptReadFunc(func(ctx context.Context, _ string, q domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
		cancel()
		return promptPage(q), context.Canceled
	})
	canceled, cancel = context.WithCancel(ctx)
	defer cancel()
	if _, err = NewMemoryPromptService(cancelReader, code, nil).prepare(canceled, "repo", "/target", promptHash("root")); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation degraded to successful fallback", err)
	}
}

type promptPositionStore struct {
	*storage.FileStore
	position domain.WorkingPosition
}

func (s *promptPositionStore) GetWorkingPosition(context.Context) (domain.WorkingPosition, error) {
	return s.position, nil
}
func (s *promptPositionStore) PutWorkingPosition(_ context.Context, p domain.WorkingPosition) error {
	s.position = p
	return nil
}

func TestEffectivePromptPinsAncestorAndEmptyMemory(t *testing.T) {
	ctx := context.Background()
	st := &promptPositionStore{FileStore: storage.NewFileStore(t.TempDir())}
	root, source := promptHash("selected"), promptHash("memory source")
	digest := domain.MemoryDigest{SnapshotID: source, Summary: "original pinned history"}
	h, err := st.PutMemory(ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	pos := domain.WorkingPosition{RepoID: "repo", Snapshot: root, MemorySource: source, MemoryHash: h, Rewound: true, MemoryPinned: true}
	if err = st.PutWorkingPosition(ctx, pos); err != nil {
		t.Fatal(err)
	}
	calls := 0
	reader := promptReadFunc(func(ctx context.Context, _ string, q domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
		calls++
		if q.Selection.SnapshotID != source || q.Selection.MemoryHash != h {
			t.Fatal("historical pin replaced", q)
		}
		return promptPage(q, promptItem("old scoped claim", "inactive")), nil
	})
	svc := NewMemoryPromptService(reader, &promptCode{oid: promptOID(2)}, st)
	p, err := svc.prepare(ctx, "repo", "/target", root)
	if err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
	if notice := p.notice(16 << 10); !strings.Contains(notice, string(h)) || !strings.Contains(notice, string(source)) {
		t.Fatal("pin retrieval provenance missing", notice)
	}
	pos.MemoryHash = ""
	if err = st.PutWorkingPosition(ctx, pos); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(p.check(ctx), domain.ErrSelectionChanged) {
		t.Fatal("same-code memory repin ignored")
	}
	p, err = svc.prepare(ctx, "repo", "/target", root)
	if err != nil || calls != 1 || p.status != "historical_memory_empty" {
		t.Fatal("empty pin queried latest", p, err, calls)
	}
}

func TestEffectivePromptUsesOnlyCurrentWorktreeBranchIntegration(t *testing.T) {
	ctx := context.Background()
	root := promptHash("integrated root")
	other := promptHash("departure root")
	for _, mode := range []string{"current", "other-code", "orphan", "different-repo", "detached"} {
		t.Run(mode, func(t *testing.T) {
			st := &promptPositionStore{FileStore: storage.NewFileStore(t.TempDir()), position: domain.WorkingPosition{RepoID: "repo", Branch: "main", Snapshot: root, GitCommit: promptOID(2)}}
			switch mode {
			case "other-code":
				st.position.GitCommit = promptOID(1)
			case "orphan":
				st.position.Orphan = true
			case "different-repo":
				st.position.RepoID = "other"
			case "detached":
				st.position.Branch = ""
			}
			calls := 0
			reader := promptReadFunc(func(_ context.Context, _ string, q domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
				calls++
				want := ""
				if mode == "current" && q.Selection.SnapshotID == root {
					want = "main"
				}
				if q.Selection.Branch != want {
					t.Fatalf("wrong inclusion scope: %+v", q.Selection)
				}
				if want != "" {
					if q.Content != "prompt" {
						t.Fatal("branch memory omitted from prompt")
					}
					return promptPage(q, domain.EffectiveMemoryItem{ID: promptHash("merged-excerpt"), SourceSnapshot: other, Kind: "legacy_summary", Text: "NEW MERGED HISTORY", State: "review", Reason: "untyped_historical_text"}), nil
				}
				return promptPage(q), nil
			})
			p, err := NewMemoryPromptService(reader, &promptCode{oid: promptOID(2)}, st).prepare(ctx, "repo", "/target", root, other)
			if err != nil || calls != 2 || p.status != "server_assessed" {
				t.Fatal(p, err, calls)
			}
			if mode == "current" && !strings.Contains(p.notice(16<<10), "NEW MERGED HISTORY") {
				t.Fatal("merged memory missing from rendered prompt")
			}
		})
	}
}

type promptMaterializer struct {
	provider domain.ProviderKind
	raw      []byte
	calls    int
}

func (m *promptMaterializer) Provider() domain.ProviderKind { return m.provider }
func (m *promptMaterializer) Materialize(_ context.Context, raw []byte, _ string) (string, string, error) {
	m.calls++
	m.raw = append([]byte{}, raw...)
	return "", "", nil
}

type promptDistillFunc func(context.Context, domain.CIRDocument, *domain.NativeMemory) (domain.MemoryDigest, error)

func (f promptDistillFunc) Distill(c context.Context, d domain.CIRDocument, n *domain.NativeMemory) (domain.MemoryDigest, error) {
	return f(c, d, n)
}

func TestEffectivePromptLoadAndBranchKeepOriginalMemory(t *testing.T) {
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		for _, trim := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/trim=%v", provider, trim), func(t *testing.T) {
				ctx := context.Background()
				st := storage.NewFileStore(t.TempDir())
				cwd := t.TempDir()
				repo := domain.Repo{ID: string(promptHash(t.Name())), DefaultBranch: "main", LocalPath: cwd}
				events := []domain.Event{seedMessage("user", "current request", 0)}
				if trim {
					for i := 0; i < 28; i++ {
						events = append(events, seedMessage("assistant", strings.Repeat("past work ", 2000), len(events)), seedMessage("user", fmt.Sprintf("continue %d", i), len(events)+1))
					}
				}
				original := domain.MemoryDigest{Summary: "A feature was implemented in the old session", ClaimsVersion: 1, Fragments: []domain.MemoryFragment{{SourceSnapshot: promptHash("source"), Claims: []domain.MemoryClaim{{Kind: "code", Text: "A feature", Code: &domain.MemoryCodeScope{Commit: promptOID(1), Paths: []string{"a.go"}}}}}}}
				root := putBranchSeedSnapshot(t, ctx, st, repo.ID, "main", events, nil, nil)
				original.Fragments[0].SourceSnapshot = root
				original.Fragments[0].Summary = original.Summary
				root = putBranchSeedSnapshot(t, ctx, st, repo.ID, "main", events, nil, &original)
				putBranchSeedRef(t, ctx, st, repo.ID, "main", root)
				snap, _ := st.GetSnapshot(ctx, root)
				storedBefore, _ := st.GetMemory(ctx, snap.MemoryHash)
				before, _ := domain.MemoryDigestHash(storedBefore)
				calls := 0
				code := &promptCode{oid: promptOID(4)}
				reader := promptReadFunc(func(ctx context.Context, r string, q domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
					calls++
					if r != repo.ID {
						t.Fatal("repository inferred from absent input instead of snapshot", r)
					}
					return promptPage(q, promptItem("A feature", "inactive"), promptItem("B feature", "applied")), nil
				})
				prompts := NewMemoryPromptService(reader, code, st)
				var cdc outbound.ProviderCodec = codec.NewClaudeCodec()
				if provider == domain.ProviderCodex {
					cdc = codec.NewCodexCodec()
				}
				mat := &promptMaterializer{provider: provider}
				sink := &recordingDigestSink{provider: provider}
				load := NewLoadSessionService(st, map[domain.ProviderKind]outbound.ProviderCodec{provider: cdc}, map[domain.ProviderKind]outbound.SessionMaterializer{provider: mat}, nil, memory.NewRuleDistiller(), map[domain.ProviderKind]outbound.MemorySink{provider: sink}).WithMemoryPrompts(prompts)
				out, err := load.Load(ctx, inbound.LoadInput{RepoID: repo.ID, Ref: string(root), Cwd: cwd, TargetProvider: provider})
				if err != nil || mat.calls != 1 {
					t.Fatal(out, err, mat.calls)
				}
				cir, err := cdc.Decode(ctx, mat.raw)
				if err != nil {
					t.Fatal(err)
				}
				var text strings.Builder
				for _, ev := range cir.EffectiveContext().Events {
					for _, block := range ev.Blocks {
						text.WriteString(block.Text)
					}
				}
				for _, want := range []string{"[inactive; code;", "[applied; code;", "Historical memory and conversation", promptOID(4)} {
					if !strings.Contains(text.String(), want) {
						t.Fatal("load missing", want)
					}
				}
				total, _ := seedBudgets(provider)
				if eventsJSONBytes(cir.EffectiveContext().Events) > total {
					t.Fatal("load exceeds wire budget")
				}
				if trim && out.TrimmedEvents == 0 {
					t.Fatal("trim path not exercised")
				}
				_, err = load.Load(ctx, inbound.LoadInput{Ref: string(root), Cwd: cwd, TargetProvider: provider, Mode: domain.FidelityMemory})
				if err != nil || !strings.Contains(sink.digest.Summary, "[inactive; code;") || len(sink.digest.Summary) > 48<<10 {
					t.Fatal("managed memory missing assessment", err)
				}
				var realSink outbound.MemorySink = memory.NewClaudeMemorySink()
				if provider == domain.ProviderCodex {
					realSink = memory.NewCodexMemorySink()
				}
				path, sinkErr := realSink.Inject(ctx, sink.digest, cwd)
				if sinkErr != nil {
					t.Fatal(sinkErr)
				}
				actual, sinkErr := os.ReadFile(path)
				if sinkErr != nil || !strings.Contains(string(actual), "[inactive; code;") {
					t.Fatal("real sink discarded fresh assessment", sinkErr)
				}
				seed := NewBranchSeedService(branchSeedGit{repo: repo}, st, memory.NewRuleDistiller(), nil, nil, nil).WithMemoryPrompts(prompts)
				seeded, err := seed.Seed(ctx, inbound.SeedInput{Cwd: cwd, FromBranch: "main", NewBranch: "feature/new", Provider: provider, SkipMaterialize: true})
				if err != nil {
					t.Fatal(err)
				}
				doc, _ := st.GetDoc(ctx, seeded.SnapshotID)
				if !strings.Contains(doc.CIR.Events[0].Blocks[0].Text, "[inactive; code;") || eventsJSONBytes(doc.CIR.Events) > total {
					t.Fatal("branch prompt lost assessment/budget")
				}
				ss, _ := st.GetSnapshot(ctx, seeded.SnapshotID)
				seedMemory, _ := st.GetMemory(ctx, ss.MemoryHash)
				if !seedMemory.HasMemoryClaims() || strings.Contains(seedMemory.Summary, domain.CodeAssessmentBegin) {
					t.Fatal("prompt projection replaced archived claims")
				}
				afterDigest, _ := st.GetMemory(ctx, snap.MemoryHash)
				after, _ := domain.MemoryDigestHash(afterDigest)
				if before != after || calls != 3 {
					t.Fatal("original mutated or repeated query", before, after, calls)
				}
			})
		}
	}
}

func TestEffectivePromptSelectionChangePreventsInstallation(t *testing.T) {
	for _, operation := range []string{"load", "branch"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			st := storage.NewFileStore(t.TempDir())
			cwd := t.TempDir()
			repo := domain.Repo{ID: "repo", LocalPath: cwd, DefaultBranch: "main"}
			root := putBranchSeedSnapshot(t, ctx, st, repo.ID, "main", []domain.Event{seedMessage("user", "request", 0)}, nil, nil)
			putBranchSeedRef(t, ctx, st, repo.ID, "main", root)
			code := &promptCode{oid: promptOID(1)}
			reader := promptReadFunc(func(ctx context.Context, _ string, q domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
				return promptPage(q), nil
			})
			prompts := NewMemoryPromptService(reader, code, st)
			distill := promptDistillFunc(func(context.Context, domain.CIRDocument, *domain.NativeMemory) (domain.MemoryDigest, error) {
				code.oid = promptOID(2)
				return domain.MemoryDigest{Summary: "fresh"}, nil
			})
			var err error
			if operation == "load" {
				sink := &recordingDigestSink{}
				load := NewLoadSessionService(st, nil, nil, nil, distill, map[domain.ProviderKind]outbound.MemorySink{domain.ProviderCodex: sink}).WithMemoryPrompts(prompts)
				_, err = load.Load(ctx, inbound.LoadInput{RepoID: repo.ID, Ref: string(root), Cwd: cwd, Mode: domain.FidelityMemory, TargetProvider: domain.ProviderCodex})
				if sink.digest.Summary != "" {
					t.Fatal("stale prompt installed")
				}
			} else {
				seed := NewBranchSeedService(branchSeedGit{repo: repo}, st, distill, nil, nil, nil).WithMemoryPrompts(prompts)
				_, err = seed.Seed(ctx, inbound.SeedInput{Cwd: cwd, FromBranch: "main", NewBranch: "new", SkipMaterialize: true})
				if _, e := st.GetRef(ctx, repo.ID, domain.RefBranch, "new"); !errors.Is(e, domain.ErrNotFound) {
					t.Fatal("stale branch persisted", e)
				}
			}
			if !errors.Is(err, domain.ErrSelectionChanged) {
				t.Fatal(err)
			}
		})
	}
}

func TestEffectivePromptBranchMaterializesWithWorktreePosition(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	oid := promptOID(3)
	st := storage.NewWorktreeFileStore(cwd, cwd+"/.git", "feature/new", oid)
	repo := domain.Repo{ID: string(promptHash("worktree repo")), DefaultBranch: "main", LocalPath: cwd}
	root := putBranchSeedSnapshot(t, ctx, st, repo.ID, "main", []domain.Event{seedMessage("user", "source", 0)}, nil, nil)
	putBranchSeedRef(t, ctx, st, repo.ID, "main", root)
	mat := &promptMaterializer{provider: domain.ProviderCodex}
	reader := promptReadFunc(func(ctx context.Context, _ string, q domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
		return promptPage(q), nil
	})
	svc := NewBranchSeedService(branchSeedGit{repo: repo}, st, memory.NewRuleDistiller(), map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderCodex: codec.NewCodexCodec()}, map[domain.ProviderKind]outbound.SessionMaterializer{domain.ProviderCodex: mat}, nil).WithMemoryPrompts(NewMemoryPromptService(reader, &promptCode{oid: oid}, st))
	out, err := svc.Seed(ctx, inbound.SeedInput{Cwd: cwd, FromBranch: "main", NewBranch: "feature/new", Provider: domain.ProviderCodex})
	if err != nil || mat.calls != 1 {
		t.Fatalf("own HEAD transition stopped materialization: %+v %v calls=%d", out, err, mat.calls)
	}
	p, err := st.GetWorkingPosition(ctx)
	if err != nil || p.Snapshot != out.SnapshotID {
		t.Fatal("seed position not installed", p, err)
	}
}

func TestEffectivePromptManagedTasksAndEncodedBudget(t *testing.T) {
	p := &memoryPrompt{repo: string(promptHash("repo")), commit: promptOID(4), roots: []domain.ContentHash{promptHash("root")}, status: "server_assessed", items: []domain.EffectiveMemoryItem{promptItem(strings.Repeat("\"", 6000), "inactive")}}
	text := p.render(48<<10, func(n int) string {
		return renderSeedDigest(domain.MemoryDigest{Summary: strings.Repeat("history ", 15000)}, 5, n)
	})
	event := seedMessage("user", text, 0)
	event.CompactSummary = true
	if got := eventsJSONBytes([]domain.Event{event}); got > 48<<10 {
		t.Fatalf("encoded digest exceeds reserved budget: %d", got)
	}
	for _, sink := range []outbound.MemorySink{memory.NewClaudeMemorySink(), memory.NewCodexMemorySink()} {
		d := domain.MemoryDigest{Summary: "1. Decision\nKeep history\n2. Pending Tasks\n- Finish rollback validation", OpenTasks: []string{"Finish rollback validation"}, TasksAuthoritative: true}
		path, err := sink.Inject(context.Background(), p.digest(d), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(raw), "Finish rollback validation") {
			t.Fatal("authoritative tasks lost", err)
		}
	}
}

func TestEffectivePromptFullLoadDropsOldBranchAssessment(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	st := storage.NewFileStore(cwd)
	old := &memoryPrompt{repo: "repo", commit: promptOID(1), status: "server_assessed", items: []domain.EffectiveMemoryItem{promptItem("OLD_APPLIED_ASSERTION", "applied")}}
	text := old.render(48<<10, func(n int) string {
		return renderSeedText("main", "feature", nil, domain.MemoryDigest{Summary: "historical rationale"}, n)
	})
	root := putBranchSeedSnapshot(t, ctx, st, "repo", "feature", []domain.Event{seedMessage("user", text, 0)}, nil, nil)
	reader := promptReadFunc(func(ctx context.Context, _ string, q domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
		return promptPage(q, promptItem("NEW_INACTIVE_ASSERTION", "inactive")), nil
	})
	mat := &promptMaterializer{provider: domain.ProviderCodex}
	svc := NewLoadSessionService(st, map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderCodex: codec.NewCodexCodec()}, map[domain.ProviderKind]outbound.SessionMaterializer{domain.ProviderCodex: mat}, nil, memory.NewRuleDistiller(), nil).WithMemoryPrompts(NewMemoryPromptService(reader, &promptCode{oid: promptOID(2)}, st))
	if _, err := svc.Load(ctx, inbound.LoadInput{Ref: string(root), Cwd: cwd, TargetProvider: domain.ProviderCodex}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mat.raw), "OLD_APPLIED_ASSERTION") || !strings.Contains(string(mat.raw), "NEW_INACTIVE_ASSERTION") || !strings.Contains(string(mat.raw), "historical rationale") {
		t.Fatal("stale assessment survived full replay")
	}
	original, _ := st.GetDoc(ctx, root)
	if !strings.Contains(original.CIR.Events[0].Blocks[0].Text, "OLD_APPLIED_ASSERTION") {
		t.Fatal("archive changed")
	}
}

func TestEffectivePromptEscapedHistoryKeepsSeedAndContent(t *testing.T) {
	p := &memoryPrompt{status: "server_assessed", repo: "repo", commit: promptOID(2)}
	text := p.render(48<<10, func(n int) string {
		return renderSeedDigest(domain.MemoryDigest{Summary: strings.Repeat("<div>history</div>", 10000)}, 100, n)
	})
	if !strings.HasPrefix(text, seedSummaryPrefix) || !strings.Contains(text, "<div>history</div>") {
		t.Fatal("escaped history and seed marker were discarded")
	}
	if got := eventsJSONBytes([]domain.Event{seedMessage("user", text, 0)}); got > 48<<10 {
		t.Fatal("encoded budget", got)
	}
}

func TestEffectivePromptIncompleteCompactionRetainsFreshAssessment(t *testing.T) {
	for _, provider := range []domain.ProviderKind{domain.ProviderCodex, domain.ProviderClaude} {
		t.Run(string(provider), func(t *testing.T) {
			ctx := context.Background()
			cwd := t.TempDir()
			st := storage.NewFileStore(cwd)
			old := domain.CodeAssessmentBegin + "\nOLD_NESTED_ASSESSMENT\n" + domain.CodeAssessmentEnd
			events := []domain.Event{
				seedMessage("user", "archived initial", 0),
				{Kind: domain.EventCompaction, Seq: 1, ReplacementComplete: true, Replacement: []domain.Event{seedMessage("user", old+"\nprovider history", 0)}},
				seedMessage("user", "after complete compaction", 2),
				{Kind: domain.EventCompaction, Seq: 3, ReplacementComplete: false, Replacement: []domain.Event{}},
				seedMessage("user", "latest request after incomplete compaction", 4),
			}
			root := putBranchSeedSnapshot(t, ctx, st, "repo", "main", events, nil, nil)
			reader := promptReadFunc(func(ctx context.Context, _ string, q domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
				return promptPage(q, promptItem("FRESH_ASSESSMENT", "inactive")), nil
			})
			var cdc outbound.ProviderCodec = codec.NewCodexCodec()
			if provider == domain.ProviderClaude {
				cdc = codec.NewClaudeCodec()
			}
			mat := &promptMaterializer{provider: provider}
			svc := NewLoadSessionService(st, map[domain.ProviderKind]outbound.ProviderCodec{provider: cdc}, map[domain.ProviderKind]outbound.SessionMaterializer{provider: mat}, nil, memory.NewRuleDistiller(), nil).WithMemoryPrompts(NewMemoryPromptService(reader, &promptCode{oid: promptOID(2)}, st))
			if _, err := svc.Load(ctx, inbound.LoadInput{Ref: string(root), Cwd: cwd, TargetProvider: provider}); err != nil {
				t.Fatal(err)
			}
			decoded, err := cdc.Decode(ctx, mat.raw)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := domain.CanonicalBytes(decoded.EffectiveContext())
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"FRESH_ASSESSMENT", "provider history", "latest request after incomplete compaction"} {
				if !strings.Contains(string(raw), want) {
					t.Fatal("effective replay lost", want)
				}
			}
			if strings.Contains(string(raw), "OLD_NESTED_ASSESSMENT") {
				t.Fatal("old nested assessment survived")
			}
			original, _ := st.GetDoc(ctx, root)
			if !strings.Contains(original.CIR.Events[1].Replacement[0].Blocks[0].Text, "OLD_NESTED_ASSESSMENT") {
				t.Fatal("archive mutated")
			}
		})
	}
}
