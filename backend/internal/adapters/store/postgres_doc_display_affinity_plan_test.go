//go:build postgres

package store

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocDisplayPGAffinityCandidateDiscoveryPointPlans(test *testing.T) {
	fixture, target, owners := docDisplayAffinityFixturePG(test)
	var definition string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT pg_get_functiondef('cxthub_queue_doc_display_job_roots'::regproc)`).Scan(&definition); err != nil {
		test.Fatal("read actual affinity trigger definition", err)
	}
	selected := regexp.MustCompile(`(?is)\bFOR\s+queued_source\s+IN\s+(SELECT\b.*?)\s+LOOP\b`).FindAllStringSubmatch(definition, -1)
	if len(selected) != 1 {
		test.Fatal("affinity trigger must contain exactly one queued-source discovery SELECT", definition)
	}
	query := strings.TrimSpace(selected[0][1])
	for index, variable := range []string{"previous_source", "next_source", "previous_event", "next_event", "previous_profile", "next_profile"} {
		identifier := regexp.MustCompile(`\b` + variable + `\b`)
		if !identifier.MatchString(query) || !regexp.MustCompile(`\b`+variable+`\s+text\s*;`).MatchString(definition) {
			test.Fatal("affinity discovery lost a declared text parameter", variable, query)
		}
		query = identifier.ReplaceAllStringFunc(query, func(string) string {
			return fmt.Sprintf("($%d::text)", index+1)
		})
	}
	const fillerCount = 5000
	events := make([]string, fillerCount)
	profiles := make([]string, fillerCount)
	hashes := make([]string, fillerCount)
	bodies := make([][]byte, fillerCount)
	for index := range fillerCount {
		event := domain.HashContent([]byte(fmt.Sprintf("synthetic affinity lookup event %d", index/2)))
		profile := domain.DocDisplayStringProfile
		if index%2 != 0 {
			profile = domain.DocDisplayProjectionProfile
		}
		raw, err := json.Marshal(struct {
			Synthetic string             `json:"synthetic"`
			Event     domain.ContentHash `json:"event_hash"`
			Profile   string             `json:"profile"`
		}{Synthetic: "affinity source-header lookup only", Event: event, Profile: profile})
		if err != nil {
			test.Fatal(err)
		}
		events[index], profiles[index], hashes[index], bodies[index] = string(event), profile, string(domain.HashContent(raw)), raw
	}
	test.Log("Synthetic hash-consistent source headers exercise SQL discovery only; their bytes are not typed approval or publication evidence.")
	tag, err := fixture.storage.pool.Exec(fixture.ctx, `INSERT INTO doc_display_sources_v1(event_hash,profile,source_hash,bytes)
 SELECT event_hash,profile,source_hash,bytes FROM unnest($1::text[],$2::text[],$3::text[],$4::bytea[])
 AS synthetic(event_hash,profile,source_hash,bytes)`, events, profiles, hashes, bodies)
	if err != nil || tag.RowsAffected() != fillerCount {
		test.Fatal("seed synthetic two-profile source-header prefix", tag.RowsAffected(), err)
	}
	expected := []domain.ContentHash{owners[domain.DocDisplayStringProfile], owners[domain.DocDisplayProjectionProfile]}
	slices.Sort(expected)
	if expected[0] == "" || expected[0] == expected[1] {
		test.Fatal("affinity fixture must expose two distinct real owners", expected)
	}
	transaction, err := fixture.storage.pool.Begin(fixture.ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	arguments := []any{expected[1], expected[0], target.Event.Hash, target.Event.Hash,
		domain.DocDisplayStringProfile, domain.DocDisplayProjectionProfile}
	for _, unbound := range []bool{false, true} {
		current := slices.Clone(arguments)
		if unbound {
			current[0], current[1] = nil, nil
		}
		rows, err := transaction.Query(fixture.ctx, query, current...)
		if err != nil {
			test.Fatal("execute extracted affinity candidate discovery", unbound, err)
		}
		var actual []domain.ContentHash
		for rows.Next() {
			var owner domain.ContentHash
			if err := rows.Scan(&owner); err != nil {
				rows.Close()
				test.Fatal(err)
			}
			actual = append(actual, owner)
		}
		err = rows.Err()
		rows.Close()
		if err != nil || !slices.Equal(actual, expected) {
			test.Fatal("affinity discovery did not return exactly two unique sorted owners", unbound, actual, expected, err)
		}
	}
	if _, err := transaction.Exec(fixture.ctx, `ANALYZE doc_display_sources_v1`); err != nil {
		test.Fatal(err)
	}
	for _, sequential := range []string{"on", "off"} {
		if _, err := transaction.Exec(fixture.ctx, `SET LOCAL enable_seqscan=`+sequential); err != nil {
			test.Fatal(err)
		}
		for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
			encoded := explainDocToolOrdersPGTraceQuery(test, fixture.ctx, transaction, query, arguments, mode)
			var plans []struct{ Plan docEventDisplayReusePlan }
			if err := json.Unmarshal(encoded, &plans); err != nil || len(plans) != 1 {
				test.Fatal("invalid affinity discovery query plan", mode, sequential, err)
			}
			if plans[0].Plan.Rows != 2 || plans[0].Plan.Loops != 1 {
				test.Fatalf("affinity discovery must return two owners: mode=%s seqscan=%s plan=%s", mode, sequential, encoded)
			}
			probes := 0
			var inspect func(docEventDisplayReusePlan)
			inspect = func(plan docEventDisplayReusePlan) {
				if plan.Relation == "doc_display_sources_v1" {
					probes++
					if plan.Index != "doc_display_sources_v1_pkey" ||
						plan.NodeType != "Index Scan" && plan.NodeType != "Index Only Scan" ||
						!strings.Contains(plan.Condition, "event_hash =") || !strings.Contains(plan.Condition, "profile =") ||
						plan.Rows != 1 || plan.Loops != 1 || plan.Filter != "" || plan.Filtered != 0 || plan.JoinFiltered != 0 || plan.IndexRechecked != 0 {
						test.Fatalf("affinity source discovery is not a one-row primary-key point probe: mode=%s seqscan=%s plan=%s", mode, sequential, encoded)
					}
				} else if plan.Rows > 4 {
					test.Fatalf("affinity candidate UNION/sort exceeded four input candidates: mode=%s seqscan=%s plan=%s", mode, sequential, encoded)
				}
				for _, child := range plan.Plans {
					inspect(child)
				}
			}
			inspect(plans[0].Plan)
			if probes != 2 {
				test.Fatalf("affinity discovery must use exactly two source-table point probes: mode=%s seqscan=%s probes=%d plan=%s", mode, sequential, probes, encoded)
			}
			test.Logf("mode=%s seqscan=%s synthetic_source_headers=%d source_pk_probes=2 max_probe_rows=1 probe_loops=1 owners=2 max_union_candidates=4", mode, sequential, fillerCount)
		}
	}
}
