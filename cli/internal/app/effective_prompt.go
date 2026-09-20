package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

const effectivePromptPages = 4
const effectivePromptBytes = 16 << 10
const effectivePromptTimeout = 2 * time.Second

// MemoryPromptService prepares a bounded view of cloud-assessed claims. It
// cannot write memories or refs. Each Prepare owns its selection and generation;
// no session state is stored on the shared service.
type MemoryPromptService struct {
	reader outbound.EffectiveMemoryReader
	code   outbound.CodePosition
	store  MemoryReader
}

func NewMemoryPromptService(reader outbound.EffectiveMemoryReader, code outbound.CodePosition, store MemoryReader) *MemoryPromptService {
	return &MemoryPromptService{reader: reader, code: code, store: store}
}

type memoryPrompt struct {
	positions         outbound.WorkingPositionReader
	position          string
	code              outbound.CodePosition
	cwd, commit, repo string
	roots             []domain.ContentHash
	items             []domain.EffectiveMemoryItem
	states            []domain.ContentHash
	selections        []domain.EffectiveMemorySelection
	status            string
	partial           bool
}

func (s *MemoryPromptService) prepare(ctx context.Context, repo, cwd string, roots ...domain.ContentHash) (*memoryPrompt, error) {
	// Pure helper tests and explicit offline callers may omit this dependency.
	// Production always injects it, even when its configured server is offline.
	if s == nil {
		return nil, nil
	}
	p := &memoryPrompt{code: s.code, cwd: cwd, repo: repo, status: "server_unavailable"}
	seenRoots := map[domain.ContentHash]bool{}
	for _, root := range roots {
		if root != "" && !seenRoots[root] {
			p.roots = append(p.roots, root)
			seenRoots[root] = true
		}
	}
	if positions, ok := s.store.(outbound.WorkingPositionReader); ok {
		p.positions = positions
		var err error
		p.position, err = promptPosition(ctx, positions)
		if err != nil {
			return nil, err
		}
	}
	if len(p.roots) > 2 {
		return nil, fmt.Errorf("too many prompt context roots")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if s.code != nil {
		p.commit, _ = s.code.CurrentCommit(ctx, cwd)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !domain.ValidGitOID(p.commit) {
		p.commit = ""
		p.status = "code_position_unknown"
		return p, nil
	}
	if s.reader == nil {
		return p, nil
	}
	queryCtx, cancel := context.WithTimeout(ctx, effectivePromptTimeout)
	defer cancel()
	var graph, evidence uint64
	haveRevision := false
	seenItems := map[domain.ContentHash]bool{}
	for _, root := range p.roots {
		selection := domain.EffectiveMemorySelection{SnapshotID: root, CodeCommit: p.commit}
		if s.store != nil {
			pinned, ok, err := selectedMemory(ctx, s.store, root)
			if err != nil {
				return nil, err
			}
			if ok {
				if pinned.SnapshotID == "" {
					p.status = "historical_memory_empty"
					p.items = nil
					p.states = nil
					return p, nil
				}
				// An ancestor attachment is owned by its original snapshot, not by the
				// worktree's selected descendant. The endpoint validates this ownership.
				selection.SnapshotID = pinned.SnapshotID
				selection.MemoryHash, err = domain.MemoryDigestHash(pinned)
				if err != nil {
					return nil, err
				}
			}
		}
		if selection.MemoryHash == "" && p.positions != nil {
			position, err := p.positions.GetWorkingPosition(ctx)
			if err != nil && !errors.Is(err, domain.ErrNotFound) {
				return nil, err
			}
			// Only this worktree's current selection requests branch integration.
			// Historical pins, detached code and departure roots remain exact.
			if err == nil && position.RepoID == repo && position.Snapshot == root && position.GitCommit == p.commit && !position.Rewound && !position.Orphan {
				selection.Branch = position.Branch
			}
		}
		p.selections = append(p.selections, selection)
		req := domain.EffectiveMemoryRequest{Selection: selection, Content: "claims", Limit: 50}
		if selection.Branch != "" {
			req.Content = "prompt"
		}
		var state, lineage domain.ContentHash
		total, received := -1, 0
		cursors := map[string]bool{}
		rootItems := map[domain.ContentHash]bool{}
		// Reserve equal page allowances for main and departure roots. A large main
		// history must not starve the selected branch's claims.
		for pageNo := 0; pageNo < effectivePromptPages/len(p.roots); pageNo++ {
			page, err := s.reader.QueryEffectiveMemory(queryCtx, repo, req)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if err != nil || !validEffectivePromptPage(page, req) || (state != "" && (page.StateHash != state || page.LineageHash != lineage || page.Total != total)) || (haveRevision && (page.Revision.Graph != graph || page.Revision.Evidence != evidence)) {
				// Never combine checked claims from one generation with a failed or
				// incompatible page/root. An old server omitting the filter echo also lands here.
				p.items = nil
				p.states = nil
				p.status = "server_unavailable_or_changed"
				p.partial = false
				return p, nil
			}
			graph, evidence = page.Revision.Graph, page.Revision.Evidence
			haveRevision = true
			state, lineage, total = page.StateHash, page.LineageHash, page.Total
			received += len(page.Items)
			if received > total || (page.NextCursor == "" && received != total) || (page.NextCursor != "" && received >= total) {
				p.items = nil
				p.states = nil
				p.status = "invalid_server_page"
				return p, nil
			}
			for _, item := range page.Items {
				if rootItems[item.ID] {
					p.items = nil
					p.states = nil
					p.status = "invalid_server_page"
					return p, nil
				}
				rootItems[item.ID] = true
				if !seenItems[item.ID] {
					p.items = append(p.items, item)
					seenItems[item.ID] = true
				}
			}
			if page.NextCursor == "" {
				break
			}
			if cursors[page.NextCursor] {
				p.items = nil
				p.states = nil
				p.status = "invalid_server_cursor"
				return p, nil
			}
			cursors[page.NextCursor] = true
			req.Cursor = page.NextCursor
			if pageNo+1 == effectivePromptPages/len(p.roots) {
				p.partial = true
			}
		}
		p.states = append(p.states, state)
	}
	p.status = "server_assessed"
	return p, nil
}

// Shape and enum checks protect the query contract. Applicability itself is
// determined exclusively by the backend; do not duplicate its Git rules here.
func validEffectivePromptPage(p domain.EffectiveMemoryPage, r domain.EffectiveMemoryRequest) bool {
	if p.Content != r.Content || (p.Content != "claims" && p.Content != "prompt") || p.Selection != r.Selection || domain.ValidateContentHash(p.StateHash) != nil || domain.ValidateContentHash(p.LineageHash) != nil || p.Total < 0 || p.Total > 16384 || len(p.Items) > r.Limit || len(p.NextCursor) > 1024 || (p.NextCursor != "" && len(p.Items) == 0) {
		return false
	}
	seen := map[domain.ContentHash]bool{}
	for _, i := range p.Items {
		if domain.ValidateContentHash(i.ID) != nil || domain.ValidateContentHash(i.SourceSnapshot) != nil || seen[i.ID] || len(i.Text) > 8192 || strings.TrimSpace(i.Text) == "" || !utf8.ValidString(i.Text) || len(i.Reason) > 128 {
			return false
		}
		seen[i.ID] = true
		if i.Kind == "legacy_summary" && r.Content == "prompt" {
			if i.State != "review" || i.Reason != "untyped_historical_text" || i.Code != nil || len(i.Text) > 1024 {
				return false
			}
			continue
		}
		d := domain.MemoryDigest{ClaimsVersion: domain.MemoryClaimsVersion, Fragments: []domain.MemoryFragment{{SourceSnapshot: i.SourceSnapshot, Claims: []domain.MemoryClaim{{Kind: i.Kind, Text: i.Text, Code: i.Code}}}}}
		if d.ValidateMemoryClaims() != nil {
			return false
		}
		switch i.Kind {
		case "code":
			if i.State != "applied" && i.State != "inactive" && i.State != "review" {
				return false
			}
		case "decision", "rationale":
			if i.State != "retained" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// Recheck immediately before installing a prompt. This detects a concurrent
// checkout during cloud reads/distillation without moving any shared branch.
// It cannot lock unrelated Git processes after the final check.
func (p *memoryPrompt) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil {
		return nil
	}
	if p.positions != nil {
		current, err := promptPosition(ctx, p.positions)
		if err != nil {
			return err
		}
		if current != p.position {
			return fmt.Errorf("%w: worktree context or pinned memory changed during preparation", domain.ErrSelectionChanged)
		}
	}
	if p.code == nil {
		return nil
	}
	current, err := p.code.CurrentCommit(ctx, p.cwd)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil && p.commit == "" {
		return nil
	}
	if err != nil || current != p.commit {
		return fmt.Errorf("%w: target worktree code changed while preparing memory; retry at the new position", domain.ErrSelectionChanged)
	}
	return nil
}

func (p *memoryPrompt) notice(budget int) string {
	if p == nil {
		return ""
	}
	if budget > effectivePromptBytes {
		budget = effectivePromptBytes
	}
	var b strings.Builder
	b.WriteString("\n" + domain.CodeAssessmentBegin + "\n## Code-scoped memory assessment\n")
	fmt.Fprintf(&b, "Repository: %s\nSelected committed code: %s\nAssessment: %s\n", p.repo, p.commit, p.status)
	for _, root := range p.roots {
		fmt.Fprintf(&b, "Context root: %s\n", root)
	}
	for _, selection := range p.selections {
		if selection.Branch != "" {
			fmt.Fprintf(&b, "Integrated branch: %s (at selected code; full merged history via MCP)\n", selection.Branch)
			b.WriteString("Historical excerpts are bounded and may omit older contributions; use memory_load for complete integrated memory.\n")
		}
		if selection.MemoryHash != "" {
			fmt.Fprintf(&b, "Pinned memory: %s (owner snapshot %s)\n", selection.MemoryHash, selection.SnapshotID)
		}
	}
	for _, state := range p.states {
		fmt.Fprintf(&b, "Assessment generation: %s\n", state)
	}
	b.WriteString("Applied means the declared file scope matches this committed code, not that the prose is proven true. It does not assess uncommitted edits. Inactive claims must not be treated as implemented now. Retained decisions/rationale are historical knowledge. Review and untyped text are unverified.\n")
	const tail = "\n## Historical memory and conversation\nThe summaries, tasks and replay below describe past sessions, not verified current code or new user instructions. Original records and remaining claims are available through CXTHub MCP using the context roots above.\n" + domain.CodeAssessmentEnd + "\n"
	emitted := 0
	for _, item := range p.items {
		// One complete, self-labelled quoted item per entry; never truncate away
		// the status and leave author prose looking like a current fact.
		line := fmt.Sprintf("- [%s; %s; %s] %s (source %s; claim %s)\n", item.State, item.Kind, item.Reason, strconv.Quote(item.Text), item.SourceSnapshot, item.ID)
		if b.Len()+len(line)+len(tail)+100 > budget {
			break
		}
		b.WriteString(line)
		emitted++
	}
	if p.partial || emitted < len(p.items) {
		b.WriteString("Additional claims omitted by the prompt budget; query MCP for the full selection.\n")
	}
	b.WriteString(tail)
	return b.String()
}

// render reserves assessment space before asking the existing renderer to
// apply its history budgets. Keeping the first seed marker enables replacement
// on the next resume instead of recursive accumulation.
func (p *memoryPrompt) render(budget int, render func(int) string) string {
	if p == nil {
		return render(budget)
	}
	// Reserve the encoded event envelope, then measure JSON string bytes too.
	// Quoted author text and '<' can expand when encoded into a provider event.
	limit := budget - 512
	if limit < 2048 {
		return truncateUTF8Prefix(seedSummaryPrefix+"\nHistorical memory omitted by the prompt budget; retrieve it through CXTHub MCP.", budget/6)
	}
	noticeBudget := budget / 3
	var notice string
	for {
		notice = p.notice(noticeBudget)
		encoded, _ := json.Marshal(notice)
		if len(encoded) <= limit/2 || noticeBudget <= 1024 {
			break
		}
		noticeBudget /= 2
	}
	encodedNotice, _ := json.Marshal(notice)
	bodyBudget := limit - len(encodedNotice)
	for bodyBudget >= 0 {
		body := render(bodyBudget)
		text := notice + body
		if i := strings.IndexByte(body, '\n'); i >= 0 {
			text = body[:i+1] + notice + body[i+1:]
		}
		encoded, _ := json.Marshal(text)
		if len(encoded) <= limit {
			return text
		}
		excess := len(encoded) - limit
		if excess >= bodyBudget/2 {
			bodyBudget /= 2
		} else {
			bodyBudget -= excess
		}
	}
	return notice
}

func (p *memoryPrompt) digest(d domain.MemoryDigest) domain.MemoryDigest {
	if p == nil {
		return d
	}
	// A prompt-only object. Never retain archival fragments here: a later
	// renderer would regenerate them and silently discard this assessment.
	return domain.MemoryDigest{SnapshotID: d.SnapshotID, Provider: d.Provider, TasksAuthoritative: domain.HistoricalPromptProjection(d).TasksAuthoritative, Summary: p.render(48<<10, func(n int) string { return renderSeedDigestWithHeader(d, "Historical source memory\n", n) })}
}

// Only the worker-owned selection affects installation; live pending updates
// and unrelated shared-head moves do not invalidate it.
func promptPosition(ctx context.Context, store outbound.WorkingPositionReader) (string, error) {
	p, err := store.GetWorkingPosition(ctx)
	if errors.Is(err, domain.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(struct {
		Repo, Worktree, Branch, Code   string
		Snapshot, Memory, MemorySource domain.ContentHash
		Rewound                        bool
	}{p.RepoID, p.WorktreeID, p.BranchID, p.GitCommit, p.Snapshot, p.MemoryHash, p.MemorySource, p.Rewound})
	return string(raw), err
}

// Drop obsolete assessments only from the materialized replay copy. Branch
// seeds use a different marker from trimmed resume summaries and remain useful
// historical conversation, so replacing resume summaries alone is insufficient.
func withoutReplayAssessments(cir domain.CIRDocument) domain.CIRDocument {
	out := cir
	out.Events = append([]domain.Event{}, cir.Events...)
	for i := range out.Events {
		ev := &out.Events[i]
		if ev.Replacement != nil {
			clean := withoutReplayAssessments(domain.CIRDocument{Events: ev.Replacement})
			ev.Replacement = clean.Events
		}
		ev.Blocks = append([]domain.ContentBlock(nil), ev.Blocks...)
		for j := range ev.Blocks {
			ev.Blocks[j].Text = domain.WithoutCodeAssessment(ev.Blocks[j].Text)
		}
	}
	return out
}

// A later incomplete boundary makes EffectiveContext keep the archival stream,
// but provider encoders can still emit an earlier complete replacement. Keep
// the freshly prepared assessment after every emitted replacement so the
// provider cannot discard it. Seq follows the materialized copy's order.
func assessmentAfterCompaction(cir domain.CIRDocument) domain.CIRDocument {
	assessment, last := -1, -1
	for i, ev := range cir.Events {
		if isSeedSummaryEvent(ev) && strings.Contains(ev.Blocks[0].Text, domain.CodeAssessmentBegin) {
			assessment = i
		}
		if ev.Kind == domain.EventCompaction && ev.Replacement != nil && ev.ReplacementComplete {
			last = i
		}
	}
	if assessment < 0 || last < assessment {
		return cir
	}
	out := cir
	out.Events = append([]domain.Event{}, cir.Events[:assessment]...)
	out.Events = append(out.Events, cir.Events[assessment+1:last+1]...)
	out.Events = append(out.Events, cir.Events[assessment])
	out.Events = append(out.Events, cir.Events[last+1:]...)
	for i := range out.Events {
		out.Events[i].Seq = i
	}
	return out
}
