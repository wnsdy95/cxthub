package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestProviderLaunchInitialPromptUsesArgumentDetails(t *testing.T) {
	for _, tc := range []struct {
		name, provider, text string
		args                 []string
		present              bool
	}{
		{"absent", "claude", "", nil, false},
		{"empty", "claude", "", []string{""}, true},
		{"whitespace", "codex", " \t\n ", []string{" \t\n "}, true},
		{"unicode", "claude", "\ud55c\uae00 e\u0301 🦊", []string{"\ud55c\uae00 e\u0301 🦊"}, true},
		{"literal flag", "codex", "--model=literal", []string{"--", "--model=literal"}, true},
		{"literal command", "codex", "resume", []string{"--", "resume"}, true},
		{"literal delimiter", "claude", "--", []string{"--", "--"}, true},
		{"empty after delimiter", "codex", "", []string{"--", ""}, true},
		{"delimiter only", "claude", "", []string{"--"}, false},
		{"dash", "codex", "-", []string{"-"}, true},
		{"attached settings", "codex", "exact task", []string{"-mresume", "-pteam", "-cmodel=exec", "exact task"}, true},
		{"settings consume values", "claude", "exact task", []string{"--model=resume", "--settings", `{"model":"exec"}`, "--system-prompt=private system text", "exact task"}, true},
		{"variadic image", "codex", "--literal", []string{"-i", "one.png", "two.png", "--", "--literal"}, true},
		{"variadic tools", "claude", "exact task", []string{"--allowedTools", "Read", "Write", "--", "exact task"}, true},
		{"optional value consumed", "claude", "", []string{"--debug", "api"}, false},
		{"optional value boundary", "claude", "--literal", []string{"--debug", "--", "--literal"}, true},
		{"flags after prompt", "codex", "exact task", []string{"exact task", "--yolo", "--model=fixture-model"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := ProviderLaunchRequest{Intent: LaunchIntent{Provider: tc.provider, Pull: true, ContextBudget: 200000, ProviderArgs: tc.args}}
			details, err := req.ArgumentDetails()
			if err != nil {
				t.Fatal(err)
			}
			prompt, err := req.InitialPrompt()
			if err != nil || prompt.Present() != tc.present || prompt.Text() != tc.text {
				t.Fatalf("prompt presence=%v text=%q error=%v", prompt.Present(), prompt.Text(), err)
			}
			if tc.present && (len(details.Prompts) != 1 || details.Prompts[0] != prompt.Text()) || !tc.present && len(details.Prompts) != 0 {
				t.Fatal("initial prompt disagrees with argument details")
			}
		})
	}
}

func TestProviderLaunchInitialPromptNormalizesOnlyCodexNewlines(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		for _, tc := range []struct{ name, raw, normalized string }{
			{"empty", "", ""},
			{"LF", "first\nsecond\n", "first\nsecond\n"},
			{"CRLF", "first\r\nsecond\r\n", "first\nsecond\n"},
			{"CR", "first\rsecond\r", "first\nsecond\n"},
			{"mixed", "\r\nfirst\rsecond\nthird\r\r\n\n\r", "\nfirst\nsecond\nthird\n\n\n\n"},
			{"whitespace", " \t\r\n \r\t ", " \t\n \n\t "},
			{"unicode and literal syntax", "--resume \"\ud55c\uae00\" e\u0301\r\n<|endoftext|>\r\u2028", "--resume \"\ud55c\uae00\" e\u0301\n<|endoftext|>\n\u2028"},
		} {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				args := []string{"--model=fixture-model", "--", tc.raw}
				req := ProviderLaunchRequest{Intent: LaunchIntent{Provider: provider, Pull: true, ContextBudget: 200000, ProviderArgs: append([]string(nil), args...)}}
				want := tc.raw
				if provider == "codex" {
					want = tc.normalized
				}
				prompt, err := req.InitialPrompt()
				if err != nil || !prompt.Present() || prompt.Text() != want {
					t.Fatalf("prompt presence=%v text=%q error=%v", prompt.Present(), prompt.Text(), err)
				}
				details, err := req.ArgumentDetails()
				if err != nil || !reflect.DeepEqual(details.Prompts, []string{tc.raw}) || !reflect.DeepEqual(req.Intent.ProviderArgs, args) {
					t.Fatal("normalization changed raw argument details or original argv")
				}
				resume, err := req.ResumeArguments(launchSessionID)
				if err != nil || !reflect.DeepEqual(resume, append(resumeArgs(provider, launchSessionID), args...)) {
					t.Fatal("normalization changed native submission argv")
				}
			})
		}
	}
}

func TestProviderLaunchInitialPromptReservesNormalizedCodexTextWithoutLeaking(t *testing.T) {
	const rawPrompt = "PRIVATE_INITIAL_PROMPT\r\nsecond\rthird\r\r\n"
	const submittedPrompt = "PRIVATE_INITIAL_PROMPT\nsecond\nthird\n\n"
	for _, reserveRaw := range []bool{false, true} {
		name := "normalized reservation"
		if reserveRaw {
			name = "raw reservation rejected"
		}
		t.Run(name, func(t *testing.T) {
			root, _ := providerLaunchFixture(t, "codex", "")
			intent := LaunchIntent{Provider: "codex", Pull: true, ContextBudget: 200000, ProviderArgs: []string{"--", rawPrompt}}
			var states []string
			hooks := ProviderLaunchHooks{Prepare: func(_ context.Context, req ProviderLaunchRequest) (PreparedProviderLaunch, error) {
				p := preparedLaunch(req)
				b := p.Budget
				// The fixture tokenizer counts runes, so CRLF changes the count.
				if b.InitialPromptTokens != len([]rune(submittedPrompt)) {
					t.Fatal("token counting ran before native prompt normalization")
				}
				if err := p.PromptReservation.Validate(domain.NewAgentInitialPrompt(submittedPrompt), b.Provider, b.Model, b.Tokenizer, b.InitialPromptTokens); err != nil {
					t.Fatalf("reservation does not bind the eventual native text: %v", err)
				}
				if reserveRaw {
					// Keep the count identical to isolate text binding from count
					// validation: raw text must not authorize normalized input.
					var err error
					p.PromptReservation, err = domain.NewAgentPromptReservation(domain.NewAgentInitialPrompt(rawPrompt), b.Provider, b.Model, domain.AgentTokenUsage{Tokens: b.InitialPromptTokens, Exact: true, Tokenizer: b.Tokenizer})
					if err != nil {
						t.Fatal(err)
					}
				}
				return p, nil
			}, Record: func(_ context.Context, r ProviderLaunchReceipt) error {
				states = append(states, r.State)
				encoded, err := json.Marshal(r)
				if err != nil {
					t.Fatal(err)
				}
				for _, secret := range []string{"PRIVATE_INITIAL_PROMPT", string(domain.HashContent([]byte(rawPrompt))), string(domain.HashContent([]byte(submittedPrompt)))} {
					if strings.Contains(string(encoded), secret) {
						t.Fatal("receipt exposed raw or normalized prompt text or hash")
					}
				}
				if r.Budget == nil || r.Budget.InitialPromptTokens != len([]rune(submittedPrompt)) {
					t.Fatal("receipt lost normalized prompt accounting")
				}
				return nil
			}}
			var summary bytes.Buffer
			runtime := launchTestRuntime()
			runtime.stderr = &summary
			err := runProviderLaunch(context.Background(), root, intent, hooks, runtime)
			if strings.Contains(summary.String(), "PRIVATE_INITIAL_PROMPT") {
				t.Fatal("launch summary exposed the initial prompt")
			}
			if reserveRaw {
				if !errors.Is(err, domain.ErrContextBudgetExceeded) || !reflect.DeepEqual(states, []string{"failed"}) {
					t.Fatalf("raw reservation: error=%v states=%v", err, states)
				}
				if _, err := os.Stat(filepath.Join(root, "launch.log")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("child started with an unnormalized reservation: %v", err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(states, []string{"prepared", "launched"}) {
				t.Fatalf("normalized reservation: error=%v states=%v", err, states)
			}
			log, err := os.ReadFile(filepath.Join(root, "launch.log"))
			want := strings.Join(append(resumeArgs("codex", launchSessionID), "--", rawPrompt), "\n") + "\n"
			if err != nil || string(log) != want || intent.ProviderArgs[1] != rawPrompt {
				t.Fatal("native child or caller did not retain raw original argv")
			}
		})
	}
}

func TestProviderLaunchInitialPromptRejectsAmbiguousAndMalformedArguments(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, args := range [][]string{
			{"first", "second"}, {"", ""}, {"--", "--literal", "second"},
			{"--model"}, {"--unknown", "task"}, {"task\x00"},
		} {
			t.Run(provider+"/"+strings.Join(args, " "), func(t *testing.T) {
				req := ProviderLaunchRequest{Intent: LaunchIntent{Provider: provider, Pull: true, ContextBudget: 200000, ProviderArgs: args}}
				prompt, err := req.InitialPrompt()
				if err == nil || prompt.Present() || prompt.Text() != "" {
					t.Fatal("ambiguous or malformed prompt accepted")
				}
			})
		}
	}
}

func TestProviderLaunchInitialPromptRejectsInvalidReservationBeforeChildStarts(t *testing.T) {
	const literal = "private first task"
	for _, provider := range []string{"claude", "codex"} {
		for _, tc := range []struct {
			name   string
			mutate func(ProviderLaunchRequest, *PreparedProviderLaunch)
		}{
			{"same token count substitution", func(req ProviderLaunchRequest, p *PreparedProviderLaunch) {
				// Preserve the exact launch argv, but return a reservation created
				// for a different request copy with the same measured token count.
				req.Intent.ProviderArgs = []string{"private other task"}
				other := preparedLaunch(req)
				if other.Budget.InitialPromptTokens != p.Budget.InitialPromptTokens {
					t.Fatal("substitution fixture must have the same token count")
				}
				p.PromptReservation = other.PromptReservation
			}},
			{"missing reservation", func(_ ProviderLaunchRequest, p *PreparedProviderLaunch) {
				p.PromptReservation = domain.AgentPromptReservation{}
			}},
			{"negative count", func(_ ProviderLaunchRequest, p *PreparedProviderLaunch) { p.Budget.InitialPromptTokens = -1 }},
			{"incorrect count", func(_ ProviderLaunchRequest, p *PreparedProviderLaunch) { p.Budget.InitialPromptTokens++ }},
			{"zero count", func(_ ProviderLaunchRequest, p *PreparedProviderLaunch) { p.Budget.InitialPromptTokens = 0 }},
			{"overflow count", func(_ ProviderLaunchRequest, p *PreparedProviderLaunch) {
				p.Budget.InitialPromptTokens = int(^uint(0) >> 1)
			}},
			{"missing tokenizer", func(_ ProviderLaunchRequest, p *PreparedProviderLaunch) { p.Budget.Tokenizer = "" }},
			{"different tokenizer", func(_ ProviderLaunchRequest, p *PreparedProviderLaunch) { p.Budget.Tokenizer = "other-tokenizer" }},
			{"different resolved model", func(_ ProviderLaunchRequest, p *PreparedProviderLaunch) { p.Budget.Model = "other-model" }},
			{"different provider reservation", func(req ProviderLaunchRequest, p *PreparedProviderLaunch) {
				if req.Intent.Provider == domain.ProviderClaude {
					req.Intent.Provider = domain.ProviderCodex
				} else {
					req.Intent.Provider = domain.ProviderClaude
				}
				p.PromptReservation = preparedLaunch(req).PromptReservation
			}},
		} {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				root, _ := providerLaunchFixture(t, provider, "")
				intent := LaunchIntent{Provider: provider, Pull: true, ContextBudget: 200000, ProviderArgs: []string{literal}}
				var states []string
				cleaned := false
				hooks := ProviderLaunchHooks{Prepare: func(_ context.Context, req ProviderLaunchRequest) (PreparedProviderLaunch, error) {
					p := preparedLaunch(req)
					tc.mutate(req, &p)
					p.Cleanup = func() error { cleaned = true; return nil }
					return p, nil
				}, Record: func(_ context.Context, r ProviderLaunchReceipt) error {
					states = append(states, r.State)
					raw, err := json.Marshal(r)
					if err != nil {
						t.Fatal(err)
					}
					for _, secret := range []string{literal, "private other task", string(domain.HashContent([]byte(literal)))} {
						if strings.Contains(string(raw), secret) {
							t.Fatal("failure receipt exposed an initial prompt or its raw hash")
						}
					}
					return nil
				}}
				err := runProviderLaunch(context.Background(), root, intent, hooks, launchTestRuntime())
				if err == nil || !cleaned || !reflect.DeepEqual(states, []string{"failed"}) {
					t.Fatalf("error=%v cleaned=%v states=%v", err, cleaned, states)
				}
				if _, err := os.Stat(filepath.Join(root, "launch.log")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("provider started with an invalid prompt reservation: %v", err)
				}
			})
		}
	}
}

func TestProviderLaunchInitialPromptReservationDistinguishesAbsentAndEmpty(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, tc := range []struct {
			name string
			args []string
		}{
			{"absent", nil}, {"empty", []string{""}}, {"whitespace", []string{" \t\n"}}, {"unicode", []string{"\ud55c\uae00 🦊"}},
		} {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				req := ProviderLaunchRequest{Intent: LaunchIntent{Provider: provider, Pull: true, ContextBudget: 200000, ProviderArgs: tc.args}}
				p := preparedLaunch(req)
				prompt, err := req.InitialPrompt()
				if err != nil {
					t.Fatal(err)
				}
				if p.Budget.InitialPromptTokens != len([]rune(prompt.Text())) {
					t.Fatal("initial prompt was not measured by the fixture tokenizer")
				}
				if err := validatePreparedProviderLaunch(req, p); err != nil {
					t.Fatal(err)
				}
				p.PromptReservation = domain.AgentPromptReservation{}
				err = validatePreparedProviderLaunch(req, p)
				if (err == nil) != !prompt.Present() {
					t.Fatalf("zero reservation must permit only an absent prompt: %v", err)
				}
			})
		}
	}

	for _, present := range []bool{false, true} {
		req := ProviderLaunchRequest{Intent: LaunchIntent{Provider: "claude", Pull: true, ContextBudget: 200000}}
		if present {
			req.Intent.ProviderArgs = []string{""}
		}
		p := preparedLaunch(req)
		if present {
			req.Intent.ProviderArgs = nil
		} else {
			req.Intent.ProviderArgs = []string{""}
		}
		p.Args, _ = req.ResumeArguments(p.SessionID)
		if err := validatePreparedProviderLaunch(req, p); err == nil {
			t.Fatal("changing only prompt presence preserved a reservation")
		}
	}
}

func TestProviderLaunchInitialPromptRevalidatedWithPreparedArguments(t *testing.T) {
	root := t.TempDir()
	req := ProviderLaunchRequest{Cwd: root, Intent: LaunchIntent{Provider: "claude", Pull: true, ContextBudget: 200000, ProviderArgs: []string{"first task"}}}
	p, _, err := prepareProviderLaunch(context.Background(), req, ProviderLaunchHooks{
		Prepare: func(_ context.Context, req ProviderLaunchRequest) (PreparedProviderLaunch, error) {
			return preparedLaunch(req), nil
		},
		Record: func(context.Context, ProviderLaunchReceipt) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	observed := time.Now()
	if err := validateReadyLaunch(context.Background(), req, observed, p); err != nil {
		t.Fatal(err)
	}
	// Replacing both copies of argv evades the argv equality check alone.
	req.Intent.ProviderArgs = []string{"other task"}
	p.Args, _ = req.ResumeArguments(p.SessionID)
	if err := validateReadyLaunch(context.Background(), req, observed, p); err == nil {
		t.Fatal("ready launch accepted another prompt with the same token count")
	}
}

func TestProviderLaunchInitialPromptAmbiguityRejectedBeforeChildStarts(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			root, _ := providerLaunchFixture(t, provider, "")
			intent := LaunchIntent{Provider: provider, Pull: true, ContextBudget: 200000, ProviderArgs: []string{"first", "second"}}
			hooks := ProviderLaunchHooks{Prepare: func(_ context.Context, req ProviderLaunchRequest) (PreparedProviderLaunch, error) {
				joined := req
				joined.Intent.ProviderArgs = []string{"first second"}
				p := preparedLaunch(joined)
				p.Args, _ = req.ResumeArguments(p.SessionID)
				return p, nil
			}, Record: func(context.Context, ProviderLaunchReceipt) error { return nil }}
			if err := runProviderLaunch(context.Background(), root, intent, hooks, launchTestRuntime()); err == nil {
				t.Fatal("managed history joined ambiguous prompt arguments")
			}
			if _, err := os.Stat(filepath.Join(root, "launch.log")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("provider started with ambiguous prompts: %v", err)
			}
		})
	}
}

func TestProviderLaunchInitialPromptReservationStaysPrivate(t *testing.T) {
	const literal = "private first prompt"
	req := ProviderLaunchRequest{Intent: LaunchIntent{Provider: "claude", Pull: true, ContextBudget: 200000, ProviderArgs: []string{literal}}}
	fixture := strictLaunchContextFixture(req, launchHostFixture(req))
	reservation := fixture.InitialPromptReservation()
	field, ok := reflect.TypeFor[PreparedProviderLaunch]().FieldByName("PromptReservation")
	if !ok || field.Tag.Get("json") != "-" {
		t.Fatal("prepared launch must exclude the reservation from JSON")
	}
	artifact, err := fixture.Artifact()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(reservation)
	if err != nil {
		t.Fatal(err)
	}
	for _, representation := range []string{string(artifact), string(raw), fmt.Sprint(reservation), fmt.Sprintf("%+v", reservation), fmt.Sprintf("%#v", reservation)} {
		for _, secret := range []string{literal, string(domain.HashContent([]byte(literal)))} {
			if strings.Contains(representation, secret) {
				t.Fatal("reservation or artifact exposed prompt bytes or their raw hash")
			}
		}
	}
	var decoded domain.AgentContextPackage
	if err := json.Unmarshal(artifact, &decoded); err != nil {
		t.Fatal(err)
	}
	prompt, err := req.InitialPrompt()
	if err != nil {
		t.Fatal(err)
	}
	if err := decoded.ValidateInitialPrompt(prompt); err == nil {
		t.Fatal("serialized accounting recreated a private launch reservation")
	}
}

func TestProviderLaunchInitialPromptLeavesMemoryBootstrapAndPassthroughUnchanged(t *testing.T) {
	for _, mode := range []string{"memory", "empty_bootstrap", "passthrough"} {
		t.Run(mode, func(t *testing.T) {
			root, _ := providerLaunchFixture(t, "claude", "")
			args := []string{"", " \t", "--", "--literal"}
			intent := LaunchIntent{Provider: "claude", ProviderArgs: args}
			var receipts []ProviderLaunchReceipt
			hooks := ProviderLaunchHooks{Prepare: func(_ context.Context, req ProviderLaunchRequest) (PreparedProviderLaunch, error) {
				if mode == "passthrough" {
					t.Fatal("passthrough prepared a package")
				}
				if mode == "empty_bootstrap" {
					return preparedBootstrap(req), nil
				}
				return preparedLaunch(req), nil
			}, Record: func(_ context.Context, r ProviderLaunchReceipt) error { receipts = append(receipts, r); return nil }}
			runtime := launchTestRuntime()
			if mode == "passthrough" {
				runtime.interactive = false
			}
			if err := runProviderLaunch(context.Background(), root, intent, hooks, runtime); err != nil {
				t.Fatal(err)
			}
			want := args
			if mode != "passthrough" {
				want = append(resumeArgs("claude", launchSessionID), args...)
				if len(receipts) != 2 {
					t.Fatalf("receipt count=%d", len(receipts))
				}
			} else if len(receipts) != 0 {
				t.Fatal("passthrough recorded delivery")
			}
			for _, r := range receipts {
				if r.Budget != nil || r.Mode != mode {
					t.Fatal("default launch gained strict history accounting")
				}
			}
			log, err := os.ReadFile(filepath.Join(root, "launch.log"))
			if err != nil || string(log) != strings.Join(want, "\n")+"\n" {
				t.Fatalf("launch changed argv: %q / %v", log, err)
			}
		})
	}
}
