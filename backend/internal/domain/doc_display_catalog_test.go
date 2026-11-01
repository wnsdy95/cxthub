package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type docDisplayCatalogTestFixture struct {
	source     DocDisplayCatalogSource
	receipt    DocDisplayCatalogReceipt
	incomplete []byte
}

func docDisplayCatalogTestPublic(test *testing.T, raw string) docDisplayCatalogTestFixture {
	test.Helper()
	event := []byte(raw)
	entry := authoritativeDocRangeEntry(test, event)
	initial, err := NewDocEventDisplayPreparation(entry)
	if err != nil {
		test.Fatal(err)
	}
	var state DocEventDisplayPreparation
	switch entry.Kind {
	case EventReasoning:
		fixture := newEventDisplayTestFixture(test, event, entry.EmptyReasoning)
		fixture.finish(test)
		state = fixture.state
	case EventMessage:
		fixture := newEventMessageFixture(test, event)
		fixture.finish(test)
		state = fixture.state
	case EventToolCall, EventToolResult:
		fixture := newDocToolPolicyFixture(test, raw)
		fixture.finish(test)
		state = fixture.state
	default:
		test.Fatalf("unsupported fixture kind %q", entry.Kind)
	}
	if !bytes.Equal(event, []byte(raw)) {
		test.Fatal("fixture mutated raw event bytes")
	}
	source, err := state.Source.Encode()
	if err != nil {
		test.Fatal(err)
	}
	preparation, err := state.Encode()
	if err != nil {
		test.Fatal(err)
	}
	incomplete, err := initial.Encode()
	if err != nil {
		test.Fatal(err)
	}
	return docDisplayCatalogTestFixture{
		source: DocDisplayCatalogSource{EventHash: entry.Hash, Profile: DocDisplayProjectionProfile, Hash: HashContent(source), Bytes: source},
		receipt: DocDisplayCatalogReceipt{
			EventHash: entry.Hash, EventBytes: entry.Length, Profile: DocDisplayProjectionProfile,
			State: "completed", WorkerProtocol: 5, Source: HashContent(source),
			PreparationHash: HashContent(preparation), Preparation: preparation,
		},
		incomplete: incomplete,
	}
}

func docDisplayCatalogTestPRE(test *testing.T, raw string) docDisplayCatalogTestFixture {
	test.Helper()
	event := []byte(raw)
	built := docDisplayStringTestBuildAll(test, event, true)
	if !bytes.Equal(event, []byte(raw)) {
		test.Fatal("fixture mutated raw event bytes")
	}
	source, err := built.source.Encode()
	if err != nil {
		test.Fatal(err)
	}
	checkpoint := docDisplayStringTestCheckpoint(test, built.source, built.state)
	initial, err := NewDocDisplayStringState(built.source)
	if err != nil {
		test.Fatal(err)
	}
	var incomplete []byte
	if !initial.Done {
		incomplete = docDisplayStringTestCheckpoint(test, built.source, initial)
	}
	return docDisplayCatalogTestFixture{
		source: DocDisplayCatalogSource{EventHash: built.source.EventHash, Profile: DocDisplayStringProfile, Hash: HashContent(source), Bytes: source},
		receipt: DocDisplayCatalogReceipt{
			EventHash: built.source.EventHash, EventBytes: len(event), Profile: DocDisplayStringProfile,
			State: "completed", WorkerProtocol: 2, Source: HashContent(source),
			CheckpointHash: HashContent(checkpoint), Checkpoint: checkpoint,
		},
		incomplete: incomplete,
	}
}

func docDisplayCatalogTestFixtures(test *testing.T) map[string]docDisplayCatalogTestFixture {
	test.Helper()
	return map[string]docDisplayCatalogTestFixture{
		"public_reasoning":   docDisplayCatalogTestPublic(test, `{"kind":"reasoning","redacted_summary":"**public 한글🙂**","opaque":"private"}`),
		"public_message":     docDisplayCatalogTestPublic(test, `{"kind":"message","blocks":[{"type":"text","text":"**public 한글🙂**"}]}`),
		"public_tool_call":   docDisplayCatalogTestPublic(test, `{"kind":"tool_call","tool_name":"Bash","input":{"command":"echo public"}}`),
		"public_tool_result": docDisplayCatalogTestPublic(test, `{"kind":"tool_result","output":{"value":[true,1,"public 한글🙂"]}}`),
		"pre_string":         docDisplayCatalogTestPRE(test, `{"kind":"tool_result","output":"public 한글🙂\nend"}`),
	}
}

func docDisplayCatalogTestJSON(test *testing.T, raw []byte, field string, value any) []byte {
	test.Helper()
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		test.Fatal(err)
	}
	fields[field] = value
	encoded, err := canonicalJSON(fields)
	if err != nil {
		test.Fatal(err)
	}
	return encoded
}

func assertDocDisplayCatalogSource(test *testing.T, source DocDisplayCatalogSource, valid bool) {
	test.Helper()
	before := source
	before.Bytes = bytes.Clone(source.Bytes)
	err := source.Validate()
	if !reflect.DeepEqual(source, before) {
		test.Fatal("source validation mutated raw bytes or identity")
	}
	if valid && err != nil || !valid && !errors.Is(err, ErrIntegrity) {
		test.Fatalf("Validate() = %v, want valid=%t", err, valid)
	}
}

func assertDocDisplayCatalogProtection(test *testing.T, receipt DocDisplayCatalogReceipt, source DocDisplayCatalogSource, want string) {
	test.Helper()
	beforeSource := source
	beforeSource.Bytes = bytes.Clone(source.Bytes)
	beforeReceipt := receipt
	beforeReceipt.Checkpoint = bytes.Clone(receipt.Checkpoint)
	beforeReceipt.Preparation = bytes.Clone(receipt.Preparation)
	got := receipt.CatalogProtection(source)
	if !reflect.DeepEqual(source, beforeSource) || !reflect.DeepEqual(receipt, beforeReceipt) {
		test.Fatal("catalog classification mutated raw bytes or receipt identity")
	}
	if got != want {
		test.Fatalf("CatalogProtection() = %q, want %q", got, want)
	}
}

func TestDocDisplayCatalogTerminalReceipts(test *testing.T) {
	fixtures := docDisplayCatalogTestFixtures(test)
	fixtures["public_empty_reasoning"] = docDisplayCatalogTestPublic(test, `{"kind":"reasoning","redacted_summary":null}`)
	fixtures["public_empty_message"] = docDisplayCatalogTestPublic(test, `{"kind":"message","blocks":[]}`)
	fixtures["public_summary"] = docDisplayCatalogTestPublic(test, `{"kind":"message","compact_summary":true,"blocks":[{"text":"**summary**"}]}`)
	fixtures["public_tool_string"] = docDisplayCatalogTestPublic(test, `{"kind":"tool_result","output":"public literal"}`)
	fixtures["public_empty_tool"] = docDisplayCatalogTestPublic(test, `{"kind":"tool_result","output":null}`)
	fixtures["pre_empty_string"] = docDisplayCatalogTestPRE(test, `{"kind":"tool_result","output":""}`)
	fixtures["pre_whitespace_string"] = docDisplayCatalogTestPRE(test, `{"kind":"tool_result","output":" \t\r\n "}`)
	for name, fixture := range fixtures {
		test.Run(name, func(test *testing.T) {
			assertDocDisplayCatalogSource(test, fixture.source, true)
			assertDocDisplayCatalogProtection(test, fixture.receipt, fixture.source, "")
			assertDocDisplayCatalogProtection(test, fixture.receipt, fixture.source, "")
		})
	}
}

func TestDocDisplayCatalogNoncompletedReceipts(test *testing.T) {
	for name, fixture := range docDisplayCatalogTestFixtures(test) {
		test.Run(name, func(test *testing.T) {
			for _, scenario := range []struct {
				name string
				job  DocRangeIndexJob
			}{
				{"waiting", DocRangeIndexJob{State: "waiting"}},
				{"running", DocRangeIndexJob{State: "running"}},
				{"retrying", DocRangeIndexJob{State: "retrying"}},
				{"rejected", DocRangeIndexJob{State: "rejected"}},
				{"expired_running_lease", DocRangeIndexJob{State: "running", LeaseUntil: time.Unix(1, 0)}},
				{"expired_retrying_lease", DocRangeIndexJob{State: "retrying", LeaseUntil: time.Unix(1, 0)}},
				{"empty", DocRangeIndexJob{}},
				{"unknown", DocRangeIndexJob{State: "future"}},
				{"wrong_case", DocRangeIndexJob{State: "Completed"}},
			} {
				test.Run(scenario.name, func(test *testing.T) {
					receipt := fixture.receipt
					receipt.State = scenario.job.State
					assertDocDisplayCatalogProtection(test, receipt, fixture.source, "resumable_job")
					receipt = DocDisplayCatalogReceipt{State: scenario.job.State, Discovering: true, WorkerProtocol: -1}
					assertDocDisplayCatalogProtection(test, receipt, DocDisplayCatalogSource{}, "resumable_job")
				})
			}
		})
	}
}

func TestDocDisplayCatalogSourceValidation(test *testing.T) {
	for name, fixture := range docDisplayCatalogTestFixtures(test) {
		test.Run(name, func(test *testing.T) {
			for scenario, mutate := range map[string]func(*DocDisplayCatalogSource){
				"missing_hash":    func(source *DocDisplayCatalogSource) { source.Hash = "" },
				"invalid_hash":    func(source *DocDisplayCatalogSource) { source.Hash = "bad" },
				"wrong_hash":      func(source *DocDisplayCatalogSource) { source.Hash = HashContent([]byte("other source")) },
				"missing_owner":   func(source *DocDisplayCatalogSource) { source.EventHash = "" },
				"invalid_owner":   func(source *DocDisplayCatalogSource) { source.EventHash = "bad" },
				"wrong_owner":     func(source *DocDisplayCatalogSource) { source.EventHash = HashContent([]byte("other event")) },
				"missing_profile": func(source *DocDisplayCatalogSource) { source.Profile = "" },
				"unknown_profile": func(source *DocDisplayCatalogSource) { source.Profile = "future" },
				"other_profile": func(source *DocDisplayCatalogSource) {
					if source.Profile == DocDisplayStringProfile {
						source.Profile = DocDisplayProjectionProfile
					} else {
						source.Profile = DocDisplayStringProfile
					}
				},
			} {
				test.Run(scenario, func(test *testing.T) {
					source := fixture.source
					mutate(&source)
					assertDocDisplayCatalogSource(test, source, false)
					assertDocDisplayCatalogProtection(test, fixture.receipt, source, "unverified_completion")
				})
			}
			for scenario, raw := range map[string][]byte{
				"missing_bytes":         nil,
				"empty_bytes":           {},
				"oversized_bytes":       []byte(strings.Repeat(" ", MaxDocDisplaySourceBytes+1)),
				"invalid_json":          []byte(`{"`),
				"null_json":             []byte(`null`),
				"noncanonical":          append(bytes.Clone(fixture.source.Bytes), '\n'),
				"unknown_field":         docDisplayCatalogTestJSON(test, fixture.source.Bytes, "unknown", true),
				"duplicate_field":       bytes.Replace(fixture.source.Bytes, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
				"bad_version":           docDisplayCatalogTestJSON(test, fixture.source.Bytes, "version", 2),
				"bad_event_hash":        docDisplayCatalogTestJSON(test, fixture.source.Bytes, "event_hash", "bad"),
				"bad_profile":           docDisplayCatalogTestJSON(test, fixture.source.Bytes, "profile", "future"),
				"negative_event_bytes":  docDisplayCatalogTestJSON(test, fixture.source.Bytes, "event_bytes", -1),
				"zero_event_bytes":      docDisplayCatalogTestJSON(test, fixture.source.Bytes, "event_bytes", 0),
				"oversized_event_bytes": docDisplayCatalogTestJSON(test, fixture.source.Bytes, "event_bytes", MaxConversationDocumentBytes+1),
			} {
				test.Run(scenario, func(test *testing.T) {
					source := fixture.source
					source.Bytes, source.Hash = raw, HashContent(raw)
					receipt := fixture.receipt
					receipt.Source = source.Hash
					assertDocDisplayCatalogSource(test, source, false)
					assertDocDisplayCatalogProtection(test, receipt, source, "unverified_completion")
				})
			}
		})
	}
}

func TestDocDisplayCatalogInvalidCompletions(test *testing.T) {
	for name, fixture := range docDisplayCatalogTestFixtures(test) {
		test.Run(name, func(test *testing.T) {
			for scenario, mutate := range map[string]func(*DocDisplayCatalogReceipt){
				"missing_source":  func(receipt *DocDisplayCatalogReceipt) { receipt.Source = "" },
				"invalid_source":  func(receipt *DocDisplayCatalogReceipt) { receipt.Source = "bad" },
				"wrong_source":    func(receipt *DocDisplayCatalogReceipt) { receipt.Source = HashContent([]byte("other source")) },
				"missing_owner":   func(receipt *DocDisplayCatalogReceipt) { receipt.EventHash = "" },
				"invalid_owner":   func(receipt *DocDisplayCatalogReceipt) { receipt.EventHash = "bad" },
				"wrong_owner":     func(receipt *DocDisplayCatalogReceipt) { receipt.EventHash = HashContent([]byte("other event")) },
				"missing_profile": func(receipt *DocDisplayCatalogReceipt) { receipt.Profile = "" },
				"unknown_profile": func(receipt *DocDisplayCatalogReceipt) { receipt.Profile = "future" },
				"other_profile": func(receipt *DocDisplayCatalogReceipt) {
					if receipt.Profile == DocDisplayStringProfile {
						receipt.Profile = DocDisplayProjectionProfile
					} else {
						receipt.Profile = DocDisplayStringProfile
					}
				},
				"negative_event_bytes":  func(receipt *DocDisplayCatalogReceipt) { receipt.EventBytes = -1 },
				"zero_event_bytes":      func(receipt *DocDisplayCatalogReceipt) { receipt.EventBytes = 0 },
				"wrong_event_bytes":     func(receipt *DocDisplayCatalogReceipt) { receipt.EventBytes++ },
				"oversized_event_bytes": func(receipt *DocDisplayCatalogReceipt) { receipt.EventBytes = MaxConversationDocumentBytes + 1 },
				"missing_protocol":      func(receipt *DocDisplayCatalogReceipt) { receipt.WorkerProtocol = 0 },
				"negative_protocol":     func(receipt *DocDisplayCatalogReceipt) { receipt.WorkerProtocol = -1 },
				"unknown_protocol":      func(receipt *DocDisplayCatalogReceipt) { receipt.WorkerProtocol = 99 },
				"other_protocol":        func(receipt *DocDisplayCatalogReceipt) { receipt.WorkerProtocol = 7 - receipt.WorkerProtocol },
				"discovering":           func(receipt *DocDisplayCatalogReceipt) { receipt.Discovering = true },
				"nonfamily_hash": func(receipt *DocDisplayCatalogReceipt) {
					if receipt.Profile == DocDisplayStringProfile {
						receipt.PreparationHash = HashContent([]byte("unexpected preparation"))
					} else {
						receipt.CheckpointHash = HashContent([]byte("unexpected checkpoint"))
					}
				},
				"nonfamily_bytes": func(receipt *DocDisplayCatalogReceipt) {
					if receipt.Profile == DocDisplayStringProfile {
						receipt.Preparation = []byte(`{}`)
					} else {
						receipt.Checkpoint = []byte(`{}`)
					}
				},
				"nonfamily_hash_and_bytes": func(receipt *DocDisplayCatalogReceipt) {
					if receipt.Profile == DocDisplayStringProfile {
						receipt.Preparation, receipt.PreparationHash = []byte(`{}`), HashContent([]byte(`{}`))
					} else {
						receipt.Checkpoint, receipt.CheckpointHash = []byte(`{}`), HashContent([]byte(`{}`))
					}
				},
			} {
				test.Run(scenario, func(test *testing.T) {
					receipt := fixture.receipt
					mutate(&receipt)
					assertDocDisplayCatalogProtection(test, receipt, fixture.source, "unverified_completion")
				})
			}
			terminal := fixture.receipt.Preparation
			limit := MaxDocEventDisplayPreparationBytes
			if fixture.source.Profile == DocDisplayStringProfile {
				terminal, limit = fixture.receipt.Checkpoint, MaxDocDisplayStateBytes
			}
			for scenario, hash := range map[string]ContentHash{
				"missing_terminal_hash": "",
				"invalid_terminal_hash": "bad",
				"wrong_terminal_hash":   HashContent([]byte("other terminal")),
			} {
				test.Run(scenario, func(test *testing.T) {
					receipt := fixture.receipt
					if receipt.Profile == DocDisplayStringProfile {
						receipt.CheckpointHash = hash
					} else {
						receipt.PreparationHash = hash
					}
					assertDocDisplayCatalogProtection(test, receipt, fixture.source, "unverified_completion")
				})
			}
			for scenario, raw := range map[string][]byte{
				"missing_terminal_bytes":   nil,
				"empty_terminal_bytes":     {},
				"oversized_terminal_bytes": []byte(strings.Repeat(" ", limit+1)),
				"invalid_terminal_json":    []byte(`{"`),
				"null_terminal_json":       []byte(`null`),
				"noncanonical_terminal":    append(bytes.Clone(terminal), '\n'),
				"unknown_terminal_field":   docDisplayCatalogTestJSON(test, terminal, "unknown", true),
				"duplicate_terminal_field": bytes.Replace(terminal, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
				"bad_terminal_version":     docDisplayCatalogTestJSON(test, terminal, "version", 2),
				"incomplete_terminal":      fixture.incomplete,
			} {
				test.Run(scenario, func(test *testing.T) {
					receipt := fixture.receipt
					if receipt.Profile == DocDisplayStringProfile {
						receipt.Checkpoint, receipt.CheckpointHash = raw, HashContent(raw)
					} else {
						receipt.Preparation, receipt.PreparationHash = raw, HashContent(raw)
					}
					assertDocDisplayCatalogProtection(test, receipt, fixture.source, "unverified_completion")
				})
			}
		})
	}
}

func TestDocDisplayCatalogSourceIdentityBinding(test *testing.T) {
	for name, fixture := range docDisplayCatalogTestFixtures(test) {
		test.Run(name, func(test *testing.T) {
			for _, scenario := range []struct {
				name   string
				field  string
				value  any
				rebind bool
			}{
				{"source_event_length", "event_bytes", fixture.receipt.EventBytes + 1, false},
				{"source_and_receipt_event_length", "event_bytes", fixture.receipt.EventBytes + 1, true},
				{"source_and_receipt_owner", "event_hash", HashContent([]byte("foreign event")), true},
			} {
				test.Run(scenario.name, func(test *testing.T) {
					source, receipt := fixture.source, fixture.receipt
					source.Bytes = docDisplayCatalogTestJSON(test, source.Bytes, scenario.field, scenario.value)
					source.Hash = HashContent(source.Bytes)
					receipt.Source = source.Hash
					if scenario.field == "event_hash" {
						source.EventHash = scenario.value.(ContentHash)
					}
					if scenario.rebind {
						receipt.EventHash = source.EventHash
						if scenario.field == "event_bytes" {
							receipt.EventBytes = scenario.value.(int)
						}
					}
					assertDocDisplayCatalogSource(test, source, true)
					assertDocDisplayCatalogProtection(test, receipt, source, "unverified_completion")
				})
			}
			if fixture.source.Profile == DocDisplayStringProfile {
				test.Run("checkpoint_owner", func(test *testing.T) {
					receipt := fixture.receipt
					receipt.Checkpoint = docDisplayCatalogTestJSON(test, receipt.Checkpoint, "source", HashContent([]byte("foreign source")))
					receipt.CheckpointHash = HashContent(receipt.Checkpoint)
					assertDocDisplayCatalogProtection(test, receipt, fixture.source, "unverified_completion")
				})
				return
			}
			test.Run("preparation_source_bytes", func(test *testing.T) {
				state, err := DecodeDocEventDisplayPreparation(fixture.receipt.PreparationHash, fixture.receipt.Preparation)
				if err != nil {
					test.Fatal(err)
				}
				source := fixture.source
				switch state.Source.Kind {
				case EventReasoning:
					state.Source.EmptyReasoning = !state.Source.EmptyReasoning
				case EventMessage:
					state.Source.Summary = !state.Source.Summary
				case EventToolCall:
					state.Source.Kind = EventToolResult
				case EventToolResult:
					state.Source.Kind = EventToolCall
				}
				source.Bytes, err = state.Source.Encode()
				if err != nil {
					test.Fatal(err)
				}
				source.Hash = HashContent(source.Bytes)
				receipt := fixture.receipt
				receipt.Source = source.Hash
				assertDocDisplayCatalogSource(test, source, true)
				assertDocDisplayCatalogProtection(test, receipt, source, "unverified_completion")
			})
		})
	}
}
