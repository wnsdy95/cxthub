//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docToolValueWorkerStore struct {
	*store.PostgresStore
	steps int
}

func (storage *docToolValueWorkerStore) SaveDocEventDisplay(ctx context.Context, job domain.DocDisplayJob, step domain.DocEventDisplayStep) (domain.DocEventDisplayPreparation, error) {
	state, err := storage.PostgresStore.SaveDocEventDisplay(ctx, job, step)
	if err == nil && (state.Phase == "tool_values" || state.Phase == "tool_values_done" || state.Phase == "tool_policy") {
		storage.steps++
	}
	return state, err
}

func TestPGDocToolTreeActualWorkerRootAndLegacy(test *testing.T) {
	for _, kind := range []domain.EventKind{domain.EventToolCall, domain.EventToolResult} {
		for _, legacy := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/legacy=%t", kind, legacy), func(test *testing.T) {
				fixture := newDocDisplayPGFixture(test)
				fixture.service.ConfigureDocumentDisplay(fixture.storage)
				values := make([]any, 40)
				for index := range values {
					values[index] = map[string]any{"ordinal": index, "nested": []any{true, nil, "text"}}
				}
				event := domain.CIREvent{Kind: kind, CallID: "actual-worker-" + string(fixture.repo), Seq: 1}
				if kind == domain.EventToolCall {
					event.ToolName = "generic"
					event.Input = map[string]any{"values": values}
				} else {
					event.Output = values
				}
				var hash domain.ContentHash
				if legacy {
					hash = seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{event}).Hash
					prepareLegacyFindGenerationPG(test, fixture, hash)
				} else {
					hash = seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{event}})
				}
				target := docDisplayTargetPG(test, fixture, hash, 0)
				target.Profile = domain.DocDisplayProjectionProfile
				if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now()); err != nil {
					test.Fatal(err)
				}
				batches := 0
				probe := &denseDocDisplayBlobProbe{PostgresStore: fixture.storage}
				valuesStore := &docToolValueWorkerStore{PostgresStore: fixture.storage}
				for ; batches < 40; batches++ {
					restarted := NewService(fixture.storage, probe, nil, nil, fixture.storage)
					restarted.ConfigureDocumentDisplay(valuesStore)
					err := restarted.ProcessDocDisplayJobs(fixture.ctx, 1)
					if errors.Is(err, domain.ErrDocFindDisplayUnavailable) {
						break
					}
					if err != nil {
						test.Fatal("actual worker rejected tool before private tree completion", err)
					}
					if _, err := readDocEventDisplayGenerationPG(fixture, target); err == nil {
						break
					} else if !errors.Is(err, domain.ErrNotFound) {
						test.Fatal("public result read failed", err)
					}
				}
				var phase, keyPhase, orderPhase, reason, jobState string
				var nodes, members, plans int
				var preparation []byte
				if err := fixture.pool.QueryRow(fixture.ctx, `SELECT tree.state,tree.node_count,keys.state,keys.member_count,
 orders.state,orders.plan_count,job.reason,job.state,
 substring(job.preparation FROM 1 FOR $4)
 FROM doc_display_jobs_v1 job JOIN doc_tool_tree_sources_v1 tree ON tree.source_hash=job.source_hash
	JOIN doc_tool_key_sources_v1 keys ON keys.tree_scope=tree.scope
	JOIN doc_tool_order_sources_v1 orders ON orders.tree_scope=tree.scope AND orders.keys_scope=keys.scope
 WHERE job.repo_id=$1 AND job.doc_hash=$2 AND job.profile=$3 AND job.event_index=0`, fixture.repo, hash, target.DisplayProfile(), domain.MaxDocEventDisplayPreparationBytes+1).
					Scan(&phase, &nodes, &keyPhase, &members, &orderPhase, &plans, &reason, &jobState, &preparation); err != nil {
					test.Fatal(err)
				}
				expectedMembers := 80
				if kind == domain.EventToolCall {
					expectedMembers++
				}
				if phase != "complete" || keyPhase != "complete" || orderPhase != "complete" || plans != expectedMembers+1 || members != expectedMembers || nodes < 100 || batches == 40 {
					test.Fatal("worker did not finish source-bound preparation", phase, keyPhase, orderPhase, members, plans, nodes, reason, batches)
				}
				if jobState != "completed" || reason != "" {
					test.Fatal("tool publication was not acknowledged", jobState, reason)
				}
				prepared, err := domain.DecodeDocEventDisplayPreparation(domain.HashContent(preparation), preparation)
				if err != nil || prepared.Phase != "completed" || prepared.ToolOrders == nil || prepared.ToolKeys == nil || prepared.ToolKeys.Nodes != nodes || prepared.Plan == "" {
					test.Fatal("worker skipped nested value progress", prepared.Phase, err)
				}
				owner, _ := prepared.Source.Hash()
				load := func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
					var raw []byte
					err := fixture.pool.QueryRow(ctx, `SELECT substring(bytes FROM 1 FOR $3) FROM doc_display_objects_v1 WHERE source_hash=$1 AND hash=$2`, owner, hash, domain.MaxDocDisplayPageBytes+1).Scan(&raw)
					return raw, err
				}
				valuePlan := prepared.ToolPlan
				if kind == domain.EventToolCall {
					raw, err := load(fixture.ctx, prepared.ToolEvent)
					var eventPlan struct {
						Parts []struct {
							Kind string             `json:"kind"`
							Plan domain.ContentHash `json:"plan"`
						} `json:"parts"`
					}
					if err != nil || json.Unmarshal(raw, &eventPlan) != nil || prepared.ToolEvent != prepared.Plan {
						test.Fatal("call lost its composite plan", err)
					}
					for _, part := range eventPlan.Parts {
						if part.Kind == "value" {
							valuePlan = part.Plan
						}
					}
				}
				planRaw, err := load(fixture.ctx, valuePlan)
				if err != nil {
					test.Fatal(err)
				}
				plan, err := domain.DecodeDocToolValuePlan(valuePlan, planRaw)
				if err != nil || plan.Source.Tree != *prepared.ToolTree || plan.Source.Orders != *prepared.ToolOrders {
					test.Fatal("stored value plan lost its exact inputs", err)
				}
				var output strings.Builder
				for index := 0; index < plan.Pages.Count; index++ {
					page, err := domain.ReadDocToolValuePage(fixture.ctx, plan, index, load)
					if err != nil || page.Display.From != output.Len() {
						test.Fatal("stored value pages are not contiguous", err)
					}
					output.WriteString(page.Display.Text)
				}
				eventRaw, _ := json.Marshal(event)
				previous, err := domain.BuildDocToolDisplay(fixture.ctx, eventRaw)
				if err != nil {
					test.Fatal(err)
				}
				want := previous.Text()
				if kind == domain.EventToolCall {
					if !strings.HasPrefix(want, "generic\n") {
						test.Fatal("call fixture lost its separate event header")
					}
					want = strings.TrimPrefix(want, "generic\n")
				}
				if output.String() != want || output.Len() != plan.TextBytes {
					test.Fatal("actual restarted worker changed nested pretty output")
				}
				{
					generation, err := readDocEventDisplayGenerationPG(fixture, target)
					if err != nil || generation.Source.Plan != prepared.Plan || generation.Source.TextBytes != len(previous.Text()) {
						test.Fatal("published result lost its typed plan", err)
					}
					var public strings.Builder
					_, err = repositoryReadForRepo(fixture.ctx, fixture.service, fixture.repo, func(read context.Context) (bool, error) {
						for index := 0; index < generation.Pages.Count; index++ {
							page, err := fixture.storage.ReadDocEventDisplayPage(read, target, index)
							if err != nil || page.From != public.Len() {
								return false, domain.ErrIntegrity
							}
							for offset, character := range page.Text {
								width := utf8.RuneLen(character)
								span, err := page.SourceRange(offset, width)
								want, baselineError := previous.SourceRange(page.From+offset, width)
								if err != nil || baselineError != nil || span != want {
									return false, domain.ErrIntegrity
								}
							}
							public.WriteString(page.Text)
						}
						return true, nil
					})
					if err != nil || public.String() != previous.Text() {
						test.Fatal("public projection read changed text or byte ranges", err)
					}
				}
				if probe.rawReads == 0 || probe.rawReads > 2*(nodes/domain.DocToolTreeStepNodes+4)+valuesStore.steps {
					test.Fatal("small source refills repeatedly fetched the same source chunk", probe.rawReads, nodes)
				}
				test.Logf("kind=%s legacy=%t private_nodes=%d key_members=%d order_plans=%d value_steps=%d value_pages=%d value_text_bytes=%d worker_batches=%d source_blob_reads=%d", kind, legacy, nodes, members, plans, valuesStore.steps, plan.Pages.Count, plan.TextBytes, batches+1, probe.rawReads)
			})
		}
	}
}
