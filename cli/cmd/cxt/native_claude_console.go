package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/nativeclaude"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// The first exchange owns the terminal until native TUI handoff. Serialize
// concurrent native dialogs, but let a withdrawn dialog leave the queue. The
// reader must honor cancellation and must not buffer another dialog's input.
type nativeClaudeConsole struct {
	gate chan struct{}
	read func(context.Context) (string, error)
	out  io.Writer
}

func newNativeClaudeConsole(read func(context.Context) (string, error), out io.Writer) *nativeClaudeConsole {
	return &nativeClaudeConsole{gate: make(chan struct{}, 1), read: read, out: out}
}

func (c *nativeClaudeConsole) enter(ctx context.Context) error {
	if c == nil || c.read == nil || c.out == nil {
		return nativeclaude.ErrInteraction
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case c.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-c.gate
			return err
		}
		return nil
	}
}

func (c *nativeClaudeConsole) question(ctx context.Context) (domain.AgentInitialPrompt, error) {
	if err := c.enter(ctx); err != nil {
		return domain.AgentInitialPrompt{}, err
	}
	defer func() { <-c.gate }()
	if _, err := fmt.Fprintln(c.out, "Enter your first question. Put /submit on its own line to send, or /cancel to exit."); err != nil {
		return domain.AgentInitialPrompt{}, err
	}
	var lines []string
	size := 0
	for {
		line, err := c.read(ctx)
		if err != nil {
			return domain.AgentInitialPrompt{}, err
		}
		switch line {
		case "/cancel":
			return domain.AgentInitialPrompt{}, context.Canceled
		case "/submit":
			text := strings.Join(lines, "\n")
			if strings.TrimSpace(text) == "" {
				return domain.AgentInitialPrompt{}, nativeclaude.ErrState
			}
			return domain.NewAgentInitialPrompt(text), ctx.Err()
		}
		size += len(line) + 1
		if size > 64<<10 {
			return domain.AgentInitialPrompt{}, nativeclaude.ErrLimit
		}
		lines = append(lines, line)
	}
}

func (c *nativeClaudeConsole) handlers() nativeclaude.InteractionHandlers {
	return nativeclaude.InteractionHandlers{CanUseTool: c.permission, AskUserQuestion: c.questions}
}

func (c *nativeClaudeConsole) permission(ctx context.Context, req nativeclaude.ToolPermissionRequest) (nativeclaude.ToolPermissionDecision, error) {
	if err := c.enter(ctx); err != nil {
		return 0, err
	}
	defer func() { <-c.gate }()
	var input any
	if err := json.Unmarshal(req.Input, &input); err != nil {
		return 0, nativeclaude.ErrInteraction
	}
	// JSON encoding escapes control characters. Native content cannot emit
	// terminal escape sequences or overwrite the approval prompt.
	raw, err := json.MarshalIndent(input, "", "  ")
	if err != nil {
		return 0, nativeclaude.ErrInteraction
	}
	if _, err = fmt.Fprintf(c.out, "\nClaude requests %q\n%s\n", req.ToolName, raw); err != nil {
		return 0, err
	}
	for _, explanation := range []string{req.Description, req.DecisionReason} {
		if explanation != "" {
			if _, err = fmt.Fprintf(c.out, "%q\n", explanation); err != nil {
				return 0, err
			}
		}
	}
	for {
		if _, err = fmt.Fprint(c.out, "Allow this invocation once? [yes/no; /cancel stops the turn] "); err != nil {
			return 0, err
		}
		answer, err := c.read(ctx)
		if err != nil {
			return 0, err
		}
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "yes":
			return nativeclaude.AllowOnce, nil
		case "", "no":
			return nativeclaude.DenyOnce, nil
		case "/cancel":
			return 0, context.Canceled
		}
	}
}

func (c *nativeClaudeConsole) questions(ctx context.Context, req nativeclaude.UserQuestionRequest) (nativeclaude.UserQuestionAnswers, error) {
	empty := nativeclaude.UserQuestionAnswers{}
	if err := c.enter(ctx); err != nil {
		return empty, err
	}
	defer func() { <-c.gate }()
	answers := nativeclaude.UserQuestionAnswers{Answers: make(map[string][]string)}
	for _, question := range req.Questions {
		if _, err := fmt.Fprintf(c.out, "\n%q\n", question.Question); err != nil {
			return empty, err
		}
		for i, option := range question.Options {
			if _, err := fmt.Fprintf(c.out, "%d. %q - %q\n", i+1, option.Label, option.Description); err != nil {
				return empty, err
			}
			if option.Preview != "" {
				if _, err := fmt.Fprintf(c.out, "   Preview: %q\n", option.Preview); err != nil {
					return empty, err
				}
			}
		}
		for {
			mode := "one number"
			if question.MultiSelect {
				mode = "numbers separated by commas"
			}
			if _, err := fmt.Fprintf(c.out, "Choose %s, text:<custom answer>, /skip, or /cancel: ", mode); err != nil {
				return empty, err
			}
			line, err := c.read(ctx)
			if err != nil {
				return empty, err
			}
			if err := ctx.Err(); err != nil {
				return empty, err
			}
			if line == "/cancel" {
				return empty, context.Canceled
			}
			if line == "/skip" {
				break
			}
			selected, ok := nativeConsoleSelection(line, question)
			if !ok {
				continue
			}
			answers.Answers[question.Question] = selected
			break
		}
	}
	return answers, ctx.Err()
}

func nativeConsoleSelection(line string, question nativeclaude.UserQuestion) ([]string, bool) {
	if custom, ok := strings.CutPrefix(line, "text:"); ok {
		return []string{custom}, custom != ""
	}
	parts := strings.Split(line, ",")
	if !question.MultiSelect && len(parts) != 1 {
		return nil, false
	}
	seen := make(map[int]bool)
	var selected []string
	for _, part := range parts {
		index, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || index < 1 || index > len(question.Options) || seen[index] {
			return nil, false
		}
		seen[index] = true
		selected = append(selected, question.Options[index-1].Label)
	}
	return selected, len(selected) != 0
}

// Ordinary interaction changes only the native handler. Package preparation,
// source authorization, receipts and one-shot release remain shared.
type nativeClaudeOrdinaryRunner struct {
	*nativeclaude.FirstExchange
	handlers nativeclaude.InteractionHandlers
}

func (e nativeClaudeOrdinaryRunner) Run(ctx context.Context, question string, admit func(context.Context, nativeclaude.FirstQuestionEvidence) error) (nativeclaude.FirstExchangeResult, error) {
	return e.RunOrdinary(ctx, question, admit, e.handlers)
}
