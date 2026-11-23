package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

type forkBranchesFake struct {
	known   map[string]string
	made    []string
	makeErr error
}

func (f *forkBranchesFake) ResolveCommit(_ context.Context, _ string, revision string) (string, error) {
	if sha, ok := f.known[revision]; ok {
		return sha, nil
	}
	return "", fmt.Errorf("this clone holds no commit %s; fetch it first", revision)
}

func (f *forkBranchesFake) CreateBranchAt(_ context.Context, _ string, name, commit string) error {
	if f.makeErr != nil {
		return f.makeErr
	}
	f.made = append(f.made, name+"@"+commit)
	return nil
}

// forkRemoteFake serves a conversation in pages of two events, and cuts an
// event longer than cut bytes into pieces. open lists the events the reader
// may read.
type forkRemoteFake struct {
	body      domain.AnchoredSessionBody
	events    []domain.Event
	open      map[int]bool
	cut       int
	summaries []domain.AnchoredSummary
	reads     int
	err       error
}

func (f *forkRemoteFake) GetAnchoredSessionBody(context.Context, string, domain.ProviderKind, string) (domain.AnchoredSessionBody, error) {
	return f.body, f.err
}

func (f *forkRemoteFake) ListAnchoredCommitSummaries(context.Context, string, string) ([]domain.AnchoredSummary, error) {
	return f.summaries, nil
}

func (f *forkRemoteFake) ReadAnchoredSessionContent(_ context.Context, _ string, _ domain.ProviderKind, _ string, index, offset int) (domain.AnchoredContentPage, error) {
	f.reads++
	page := domain.AnchoredContentPage{Total: len(f.events), NextIndex: -1}
	add := func(at, from int, complete bool, text string) {
		page.Fragments = append(page.Fragments, struct {
			Index    int    `json:"event_index"`
			Offset   int    `json:"byte_offset"`
			Complete bool   `json:"event_complete"`
			JSON     string `json:"json_fragment"`
		}{at, from, complete, text})
	}
	for at := index; at < len(f.events) && len(page.Fragments) < 2; at++ {
		if f.open != nil && !f.open[at] {
			continue // reading goes on at the next open event
		}
		raw, _ := json.Marshal(f.events[at])
		from := 0
		if at == index {
			from = offset
		}
		if f.cut > 0 && len(raw)-from > f.cut {
			add(at, from, false, string(raw[from:from+f.cut]))
			page.NextIndex, page.NextOffset = at, from+f.cut
			return page, nil
		}
		add(at, from, true, string(raw[from:]))
		page.NextIndex, page.NextOffset = at+1, 0
	}
	if page.NextIndex >= len(f.events) {
		page.NextIndex = -1
	}
	return page, nil
}

type forkCodecFake struct {
	provider domain.ProviderKind
	encoded  []domain.CIRDocument
	err      error
}

func (f *forkCodecFake) Provider() domain.ProviderKind { return f.provider }
func (f *forkCodecFake) Decode(context.Context, []byte) (domain.CIRDocument, error) {
	return domain.CIRDocument{}, errors.New("not used")
}
func (f *forkCodecFake) Encode(_ context.Context, cir domain.CIRDocument, target domain.ProviderKind) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.encoded = append(f.encoded, cir)
	return []byte(fmt.Sprintf("%s session of %d events", target, len(cir.Events))), nil
}

type forkMaterializerFake struct {
	provider domain.ProviderKind
	written  []string
	err      error
}

func (f *forkMaterializerFake) Provider() domain.ProviderKind { return f.provider }
func (f *forkMaterializerFake) Materialize(_ context.Context, raw []byte, cwd string) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	f.written = append(f.written, cwd+": "+string(raw))
	return "/sessions/new.jsonl", string(f.provider) + " --resume new", nil
}

func forkMessage(seq int, role, text string) domain.Event {
	return domain.Event{Kind: domain.EventMessage, Seq: seq, Role: role, Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}
}

type forkFixture struct {
	service  *SessionForkService
	branches *forkBranchesFake
	remote   *forkRemoteFake
	claude   *forkCodecFake
	codex    *forkCodecFake
	written  *forkMaterializerFake
	key      domain.SessionUploadKey
	commit   string
}

func newForkFixture() *forkFixture {
	commit, later := strings.Repeat("a", 40), strings.Repeat("b", 40)
	f := &forkFixture{key: domain.SessionUploadKey{Provider: domain.ProviderClaude, SessionID: "session-1"}, commit: commit,
		branches: &forkBranchesFake{known: map[string]string{commit: commit, "aaaaaaa": commit, later: later}},
		claude:   &forkCodecFake{provider: domain.ProviderClaude}, codex: &forkCodecFake{provider: domain.ProviderCodex},
		written: &forkMaterializerFake{provider: domain.ProviderClaude}}
	f.remote = &forkRemoteFake{
		events: []domain.Event{forkMessage(0, "user", "Plan the parser."), forkMessage(1, "assistant", "Planned."), forkMessage(2, "user", "Write it."),
			forkMessage(3, "assistant", "Written and committed."), forkMessage(4, "user", "Now the lexer."), forkMessage(5, "assistant", "Started.")},
		body: domain.AnchoredSessionBody{Records: 6, Content: &domain.AnchoredContent{Doc: "sha256:doc", Events: 6},
			Segments: []domain.AnchoredSegment{
				{AnchoredRange: domain.AnchoredRange{FirstEvent: 0, Events: 4}, Commits: []domain.AnchoredCommit{{SHA: commit}}},
				{AnchoredRange: domain.AnchoredRange{FirstEvent: 4, Events: 2}, Commits: []domain.AnchoredCommit{{SHA: later}}}}},
	}
	repo := func(context.Context, string) (domain.Repo, error) {
		return domain.Repo{ID: domain.HashContent([]byte("synthetic repository"))}, nil
	}
	f.service = NewSessionForkService(f.branches, f.remote, repo,
		map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderClaude: f.claude, domain.ProviderCodex: f.codex},
		map[domain.ProviderKind]outbound.SessionMaterializer{domain.ProviderClaude: f.written, domain.ProviderCodex: &forkMaterializerFake{provider: domain.ProviderCodex}})
	return f
}

func TestForkMakesAGitBranchAtACommitAndNothingElse(t *testing.T) {
	f := newForkFixture()
	result, err := f.service.Fork(context.Background(), SessionForkInput{Cwd: "/work/app", At: "aaaaaaa", Branch: "topic/again"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Branch != "topic/again" || result.Commit != f.commit || result.Session != nil || result.ResumeCommand != "" {
		t.Fatalf("result: %+v", result)
	}
	if len(f.branches.made) != 1 || f.branches.made[0] != "topic/again@"+f.commit || f.remote.reads != 0 || len(f.written.written) != 0 {
		t.Fatalf("made: %v reads: %d written: %v", f.branches.made, f.remote.reads, f.written.written)
	}
	for name, in := range map[string]SessionForkInput{
		"no commit":                         {Cwd: "/work/app", Branch: "topic/again"},
		"no branch":                         {Cwd: "/work/app", At: f.commit},
		"a tool without a session":          {Cwd: "/work/app", At: f.commit, Branch: "topic/again", To: domain.ProviderCodex},
		"a commit this clone does not hold": {Cwd: "/work/app", At: "ccccccc", Branch: "topic/again"},
	} {
		if _, err := f.service.Fork(context.Background(), in); err == nil {
			t.Fatalf("%s: a fork was made", name)
		}
	}
	if len(f.branches.made) != 1 {
		t.Fatalf("a refused fork made a branch: %v", f.branches.made)
	}
}

func TestForkContinuesAConversationUpToACommitAsANewSession(t *testing.T) {
	f := newForkFixture()
	f.remote.cut = 40 // every event is read in pieces
	result, err := f.service.Fork(context.Background(), SessionForkInput{Cwd: "/work/app", Session: &f.key, At: f.commit})
	if err != nil {
		t.Fatal(err)
	}
	// The conversation up to the segment that ends at the commit, as a
	// session of the tool that recorded it, in the folder asked for.
	if result.Events != 4 || result.Omitted != 0 || result.NotReadable != 0 || result.Tool != domain.ProviderClaude || !result.SameTool ||
		result.ResumeCommand != "claude --resume new" || result.SessionPath != "/sessions/new.jsonl" || result.Branch != "" || *result.Session != f.key {
		t.Fatalf("result: %+v", result)
	}
	if len(f.claude.encoded) != 1 || len(f.written.written) != 1 || f.written.written[0] != "/work/app: claude session of 4 events" {
		t.Fatalf("encoded: %d written: %v", len(f.claude.encoded), f.written.written)
	}
	sent := f.claude.encoded[0]
	if sent.Envelope.SourceProvider != domain.ProviderClaude || sent.Envelope.SessionOriginID != "session-1" || sent.Envelope.Cwd != "/work/app" ||
		sent.Events[0].Blocks[0].Text != "Plan the parser." || sent.Events[3].Blocks[0].Text != "Written and committed." {
		t.Fatalf("the conversation that was written: %+v", sent)
	}

	// Without a commit: all of it. With a branch: the branch is made too,
	// after the session was written.
	f = newForkFixture()
	result, err = f.service.Fork(context.Background(), SessionForkInput{Cwd: "/work/app", Session: &f.key})
	if err != nil || result.Events != 6 || result.Commit != "" {
		t.Fatalf("the whole conversation: %+v %v", result, err)
	}
	result, err = f.service.Fork(context.Background(), SessionForkInput{Cwd: "/work/app", Session: &f.key, At: f.commit, Branch: "topic/again"})
	if err != nil || result.Branch != "topic/again" || result.Events != 4 || len(f.branches.made) != 1 {
		t.Fatalf("with a branch: %+v %v", result, err)
	}

	// Another tool gets a rebuilt conversation through its own codec.
	f = newForkFixture()
	result, err = f.service.Fork(context.Background(), SessionForkInput{Cwd: "/work/app", Session: &f.key, At: f.commit, To: domain.ProviderCodex})
	if err != nil || result.Tool != domain.ProviderCodex || result.SameTool || result.ResumeCommand != "codex --resume new" || len(f.codex.encoded) != 1 || len(f.claude.encoded) != 0 {
		t.Fatalf("another tool: %+v %v", result, err)
	}
}

func TestForkReadsOnlyWhatTheReaderMayRead(t *testing.T) {
	// A teammate's session of which only the first segment is pushed.
	f := newForkFixture()
	f.remote.open = map[int]bool{0: true, 1: true, 2: true, 3: true}
	f.remote.body.Segments[1].Commits = nil // a part that is not open names no commit
	result, err := f.service.Fork(context.Background(), SessionForkInput{Cwd: "/work/app", Session: &f.key, At: f.commit})
	if err != nil || result.Events != 4 || result.NotReadable != 0 {
		t.Fatalf("the open segment: %+v %v", result, err)
	}
	later := strings.Repeat("b", 40)
	if _, err := f.service.Fork(context.Background(), SessionForkInput{Cwd: "/work/app", Session: &f.key, At: later}); err == nil || !strings.Contains(err.Error(), "no part of claude session-1 that this account may read ends at commit") {
		t.Fatalf("a commit of a part that is not open: %v", err)
	}
	// The whole conversation is asked for: what is not open is not read, and
	// the result says how much that is.
	result, err = f.service.Fork(context.Background(), SessionForkInput{Cwd: "/work/app", Session: &f.key})
	if err != nil || result.Events != 4 || result.NotReadable != 2 {
		t.Fatalf("all that is open: %+v %v", result, err)
	}
	// Nothing open at all, no conversation on the server, and a server that
	// refuses are each said, and nothing is written.
	f.written.written = nil
	f.remote.open = map[int]bool{}
	if _, err := f.service.Fork(context.Background(), SessionForkInput{Cwd: "/work/app", Session: &f.key}); err == nil || !strings.Contains(err.Error(), "may read nothing") {
		t.Fatalf("nothing open: %v", err)
	}
	f.remote.body.Content = nil
	if _, err := f.service.Fork(context.Background(), SessionForkInput{Cwd: "/work/app", Session: &f.key}); err == nil || !strings.Contains(err.Error(), "holds no conversation") {
		t.Fatalf("no conversation: %v", err)
	}
	f.remote.err = errors.New("server answered 404")
	if _, err := f.service.Fork(context.Background(), SessionForkInput{Cwd: "/work/app", Session: &f.key}); err == nil || len(f.written.written) != 0 {
		t.Fatalf("a refusing server: %v written=%v", err, f.written.written)
	}
}

func TestForkOfALongConversationKeepsItsNewestTurnsAndSaysWhatIsLeftOut(t *testing.T) {
	f := newForkFixture()
	total, _ := seedBudgets(domain.ProviderClaude)
	long := strings.Repeat("x", total/8)
	f.remote.events = nil
	for index := 0; index < 24; index++ {
		role := "user"
		if index%2 == 1 {
			role = "assistant"
		}
		f.remote.events = append(f.remote.events, forkMessage(index, role, fmt.Sprintf("turn %d %s", index/2, long)))
	}
	f.remote.body.Content.Events = 24
	f.remote.body.Segments = []domain.AnchoredSegment{{AnchoredRange: domain.AnchoredRange{FirstEvent: 0, Events: 24}, Commits: []domain.AnchoredCommit{{SHA: f.commit}}}}
	f.remote.summaries = []domain.AnchoredSummary{{Provider: domain.ProviderClaude, SessionID: "other", Commit: f.commit, Body: domain.AnchoredSummaryBody{Summary: "Another session."}},
		{Provider: domain.ProviderClaude, SessionID: "session-1", Commit: f.commit, Body: domain.AnchoredSummaryBody{Summary: "Wrote the parser in twelve turns.", KeyFacts: []string{"The parser reads TOML."}}}}
	result, err := f.service.Fork(context.Background(), SessionForkInput{Cwd: "/work/app", Session: &f.key, At: f.commit})
	if err != nil {
		t.Fatal(err)
	}
	sent := f.claude.encoded[0]
	if result.Omitted == 0 || result.OmittedAs != "summary" || result.Events+result.Omitted != 24 || len(sent.Events) != result.Events+1 {
		t.Fatalf("result: %+v events written: %d", result, len(sent.Events))
	}
	// The summary the session has at the commit stands in for what was left
	// out, as a note the tool reads first; the newest turn is kept whole.
	note := sent.Events[0]
	text := note.Blocks[0].Text
	if !note.CompactSummary || note.Role != "user" || !strings.HasPrefix(text, sessionForkPrefix) || !strings.Contains(text, fmt.Sprintf("Its first %d events are not included", result.Omitted)) ||
		!strings.Contains(text, "Wrote the parser in twelve turns.") || !strings.Contains(text, "The parser reads TOML.") || strings.Contains(text, "Another session.") {
		t.Fatalf("note: %q", text[:min(len(text), 600)])
	}
	if last := sent.Events[len(sent.Events)-1]; !strings.HasPrefix(last.Blocks[0].Text, "turn 11 ") || sent.Events[1].Role != "user" {
		t.Fatalf("the tail does not end with the newest turn or does not begin a turn: %s / %s", last.Blocks[0].Text[:8], sent.Events[1].Role)
	}
	if size := eventsJSONBytes(sent.Events); size > total {
		t.Fatalf("the session written holds %d bytes of events, more than %d", size, total)
	}

	// Without a summary at the commit a line says how much was left out.
	f.remote.summaries = nil
	f.claude.encoded = nil
	result, err = f.service.Fork(context.Background(), SessionForkInput{Cwd: "/work/app", Session: &f.key, At: f.commit})
	if err != nil || result.OmittedAs != "note" || !strings.Contains(f.claude.encoded[0].Events[0].Blocks[0].Text, "No summary of the part that is not included was found.") {
		t.Fatalf("no summary: %+v %v", result, err)
	}
}

func TestForkSaysWhatCouldNotBeWritten(t *testing.T) {
	f := newForkFixture()
	f.claude.err = errors.New("synthetic encoding failure")
	if _, err := f.service.Fork(context.Background(), SessionForkInput{Cwd: "/work/app", Session: &f.key, At: f.commit, Branch: "topic/again"}); err == nil || len(f.branches.made) != 0 {
		t.Fatalf("an unwritable conversation: %v branches=%v", err, f.branches.made)
	}
	f.claude.err, f.written.err = nil, errors.New("synthetic disk failure")
	if _, err := f.service.Fork(context.Background(), SessionForkInput{Cwd: "/work/app", Session: &f.key, At: f.commit, Branch: "topic/again"}); err == nil || len(f.branches.made) != 0 {
		t.Fatalf("an unwritable session file: %v branches=%v", err, f.branches.made)
	}
	// The session was written and the branch could not be made: both are said.
	f.written.err, f.branches.makeErr = nil, errors.New("the branch topic/again already exists")
	result, err := f.service.Fork(context.Background(), SessionForkInput{Cwd: "/work/app", Session: &f.key, At: f.commit, Branch: "topic/again"})
	if err == nil || !strings.Contains(err.Error(), "the new session was written (claude --resume new), but the branch topic/again already exists") || result.ResumeCommand == "" || result.Branch != "" {
		t.Fatalf("a branch that exists: %+v %v", result, err)
	}
	// A tool there is no writer for.
	bare := NewSessionForkService(f.branches, f.remote, func(context.Context, string) (domain.Repo, error) { return domain.Repo{}, nil }, nil, nil)
	if _, err := bare.Fork(context.Background(), SessionForkInput{Cwd: "/work/app", Session: &f.key}); err == nil {
		t.Fatal("a conversation was continued without a writer")
	}
}
