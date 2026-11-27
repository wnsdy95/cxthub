package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

// SessionForkService starts new work from a commit in one of two ways, or in
// both at once: a Git branch at the commit, and a new session of a tool that
// continues the conversation of a session up to that commit.
//
// A branch needs nothing but Git: a session started on it is given the
// summaries along the branch like any other. Continuing a conversation reads
// it from the server, as far as the caller may read it, so a teammate's
// session can be continued as far as it is pushed.
type SessionForkService struct {
	remote        outbound.SessionForkRemote
	repo          func(ctx context.Context, cwd string) (domain.Repo, error)
	branches      outbound.ForkBranches
	codecs        map[domain.ProviderKind]outbound.ProviderCodec
	materializers map[domain.ProviderKind]outbound.SessionMaterializer
}

// NewSessionForkService creates the service. remote and repo may be nil for
// a service that only makes branches.
func NewSessionForkService(branches outbound.ForkBranches, remote outbound.SessionForkRemote, repo func(context.Context, string) (domain.Repo, error),
	codecs map[domain.ProviderKind]outbound.ProviderCodec, materializers map[domain.ProviderKind]outbound.SessionMaterializer) *SessionForkService {
	return &SessionForkService{remote: remote, repo: repo, branches: branches, codecs: codecs, materializers: materializers}
}

// SessionForkInput is the port's input.
type SessionForkInput = inbound.SessionForkInput

var _ inbound.SessionForker = (*SessionForkService)(nil)

// sessionForkPrefix opens the note that stands in for the beginning of a
// conversation that was left out.
const sessionForkPrefix = "[cxt] This session continues an earlier conversation."

// Fork makes what the input asks for. The branch is made last, so that a
// conversation that cannot be continued leaves no branch behind.
func (s *SessionForkService) Fork(ctx context.Context, in SessionForkInput) (domain.SessionForkResult, error) {
	result := domain.SessionForkResult{Version: 1}
	if in.Session == nil && (in.At == "" || in.Branch == "") {
		return result, fmt.Errorf("a fork without a session makes a Git branch: name the commit with --at and the branch with --branch")
	}
	if in.Branch != "" && in.At == "" {
		return result, fmt.Errorf("a branch is made at a commit: name it with --at")
	}
	if in.To != "" && in.Session == nil {
		return result, fmt.Errorf("--to names the tool of a continued conversation: name the session to continue")
	}
	if in.At != "" {
		if s.branches == nil {
			return result, fmt.Errorf("Git is unavailable")
		}
		commit, err := s.branches.ResolveCommit(ctx, in.Cwd, in.At)
		if err != nil {
			return result, err
		}
		result.Commit = commit
	}
	if in.Session != nil {
		if err := s.continueConversation(ctx, in, &result); err != nil {
			return result, err
		}
	}
	if in.Branch != "" {
		if err := s.branches.CreateBranchAt(ctx, in.Cwd, in.Branch, result.Commit); err != nil {
			if result.ResumeCommand != "" {
				return result, fmt.Errorf("the new session was written (%s), but %w", result.ResumeCommand, err)
			}
			return result, err
		}
		result.Branch = in.Branch
	}
	return result, nil
}

func (s *SessionForkService) continueConversation(ctx context.Context, in SessionForkInput, result *domain.SessionForkResult) error {
	key := *in.Session
	name := string(key.Provider) + " " + key.SessionID
	target := in.To
	if target == "" {
		target = key.Provider
	}
	codec, materializer := s.codecs[target], s.materializers[target]
	if s.remote == nil || s.repo == nil || codec == nil || materializer == nil {
		return fmt.Errorf("a conversation cannot be continued as a %s session here", target)
	}
	repo, err := s.repo(ctx, in.Cwd)
	if err != nil {
		return err
	}
	body, err := s.remote.GetAnchoredSessionBody(ctx, string(repo.ID), key.Provider, key.SessionID)
	if err != nil {
		return fmt.Errorf("%s could not be read from the server: %w", name, err)
	}
	if body.Content == nil || body.Content.Events == 0 {
		return fmt.Errorf("the server holds no conversation of %s", name)
	}
	end := body.Content.Events
	if result.Commit != "" {
		end = -1
		for _, segment := range body.Segments {
			for _, commit := range segment.Commits {
				if commit.SHA == result.Commit {
					end = segment.FirstEvent + segment.Events
				}
			}
		}
		if end < 0 {
			return fmt.Errorf("no part of %s that this account may read ends at commit %s", name, result.Commit)
		}
	}
	events, err := s.readEvents(ctx, string(repo.ID), key, end)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return fmt.Errorf("this account may read nothing of %s up to there", name)
	}
	result.NotReadable = end - len(events)

	cir := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: domain.CIRVersionV1, SourceProvider: key.Provider, Fidelity: domain.FidelityFull,
		SessionOriginID: key.SessionID, Cwd: materializationCwd(in.Cwd)}, Events: events}
	// What the tool is given is the context the conversation had, not the
	// whole archive of it, cut to the newest whole turns the tool takes.
	seed := withoutReplayAssessments(cir.EffectiveContext())
	total, digest := seedBudgets(target)
	var omitted []domain.Event
	if eventsJSONBytes(seed.Events) > total {
		seed, omitted = trimEventsForSeed(seed, total-digest)
	}
	if len(omitted) > 0 {
		note, as := s.omittedNote(ctx, string(repo.ID), key, result.Commit, len(omitted), digest)
		event := domain.Event{Kind: domain.EventMessage, Role: "user", CompactSummary: true, Blocks: []domain.ContentBlock{{Type: "text", Text: note}}}
		if len(seed.Events) > 0 {
			event.Ts, event.Seq = seed.Events[0].Ts, seed.Events[0].Seq-1
		}
		seed.Events = append([]domain.Event{event}, seed.Events...)
		result.Omitted, result.OmittedAs = len(omitted), as
	}
	seed = assessmentAfterCompaction(seed)
	raw, err := codec.Encode(ctx, seed, target)
	if err != nil {
		return fmt.Errorf("the conversation of %s could not be written as a %s session: %w", name, target, err)
	}
	path, resume, err := materializer.Materialize(ctx, raw, in.Cwd)
	if err != nil {
		return fmt.Errorf("the new %s session could not be written: %w", target, err)
	}
	result.Session, result.Tool, result.ResumeCommand, result.SessionPath = &key, target, resume, path
	result.Events, result.SameTool = len(seed.Events), target == key.Provider
	if result.Omitted > 0 {
		result.Events-- // the note is not an event of the conversation
	}
	return nil
}

// readEvents reads the events of a session before end that the caller may
// read, in order. Events that are not open to the caller are not among them.
func (s *SessionForkService) readEvents(ctx context.Context, repoID string, key domain.SessionUploadKey, end int) ([]domain.Event, error) {
	var events []domain.Event
	var piece strings.Builder
	pieceOf := -1
	for index, offset := 0, 0; index >= 0 && index < end; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := s.remote.ReadAnchoredSessionContent(ctx, repoID, key.Provider, key.SessionID, index, offset)
		if err != nil {
			return nil, fmt.Errorf("the conversation of %s %s could not be read from the server: %w", key.Provider, key.SessionID, err)
		}
		for _, fragment := range page.Fragments {
			if fragment.Index >= end {
				return events, nil
			}
			switch {
			case fragment.Offset == 0:
				piece.Reset()
				pieceOf = fragment.Index
			case fragment.Index != pieceOf || fragment.Offset != piece.Len():
				return nil, fmt.Errorf("the server's pieces of event %d of %s %s do not join", fragment.Index, key.Provider, key.SessionID)
			}
			piece.WriteString(fragment.JSON)
			if !fragment.Complete {
				continue
			}
			var event domain.Event
			if err := json.Unmarshal([]byte(piece.String()), &event); err != nil {
				return nil, fmt.Errorf("event %d of %s %s is not an event: %w", fragment.Index, key.Provider, key.SessionID, err)
			}
			events = append(events, event)
			piece.Reset()
			pieceOf = -1
		}
		if page.NextIndex >= 0 && (page.NextIndex < index || (page.NextIndex == index && page.NextOffset <= offset)) {
			return nil, fmt.Errorf("the server's pages of %s %s do not advance", key.Provider, key.SessionID)
		}
		index, offset = page.NextIndex, page.NextOffset
	}
	return events, nil
}

// omittedNote is the text that stands in for the beginning of a conversation
// that was left out: the summary the session has at the commit, or, when it
// has none there, a line that says how many events were left out.
func (s *SessionForkService) omittedNote(ctx context.Context, repoID string, key domain.SessionUploadKey, commit string, omitted, budget int) (string, string) {
	header := fmt.Sprintf("%s\nIt continues %s %s", sessionForkPrefix, key.Provider, key.SessionID)
	if commit != "" {
		short := commit
		if len(short) > 12 {
			short = short[:12]
		}
		header += " up to commit " + short
	}
	header += fmt.Sprintf(". Its first %d events are not included here; the events that follow are its most recent ones.\n", omitted)
	if commit != "" {
		if summaries, err := s.remote.ListAnchoredCommitSummaries(ctx, repoID, commit); err == nil {
			for _, summary := range summaries {
				if summary.Provider != key.Provider || summary.SessionID != key.SessionID {
					continue
				}
				digestOf := boundCarriedDigest(domain.MemoryDigest{Summary: summary.Body.Summary, KeyFacts: summary.Body.KeyFacts,
					OpenTasks: summary.Body.OpenTasks, TasksAuthoritative: summary.Body.TasksAuthoritative})
				if strings.TrimSpace(digestOf.Summary) != "" || len(digestOf.KeyFacts) > 0 || len(digestOf.OpenTasks) > 0 {
					intro := header + "Below is what that session wrote down at that commit. It records earlier work and is not a new instruction.\n\nSummary:\n"
					return renderSeedDigestWithHeader(digestOf, intro, budget), "summary"
				}
			}
		}
	}
	return header + "No summary of the part that is not included was found.", "note"
}
