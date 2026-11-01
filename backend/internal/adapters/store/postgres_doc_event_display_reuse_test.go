//go:build postgres

package store

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docEventDisplayReusePlan struct {
	NodeType       string                     `json:"Node Type"`
	Relation       string                     `json:"Relation Name"`
	Index          string                     `json:"Index Name"`
	Condition      string                     `json:"Index Cond"`
	Filter         string                     `json:"Filter"`
	Rows           float64                    `json:"Actual Rows"`
	Loops          float64                    `json:"Actual Loops"`
	Filtered       float64                    `json:"Rows Removed by Filter"`
	JoinFiltered   float64                    `json:"Rows Removed by Join Filter"`
	IndexRechecked float64                    `json:"Rows Removed by Index Recheck"`
	Plans          []docEventDisplayReusePlan `json:"Plans"`
}

type docDisplayPeerPlanCase struct {
	name, profile, query, index string
	protocol, limit             int
}

func TestDocEventDisplayCompletedPeerPGSyntheticPointPlans(test *testing.T) {
	for _, selected := range []docDisplayPeerPlanCase{
		{name: "public", profile: domain.DocDisplayProjectionProfile, protocol: 5, query: docEventDisplayCompletedPeerSQL,
			index: "doc_display_jobs_v1_completed_reuse", limit: domain.MaxDocEventDisplayPreparationBytes + 1},
		{name: "pre", profile: domain.DocDisplayStringProfile, protocol: 2, query: docStringDisplayCompletedPeerSQL,
			index: "doc_display_jobs_v1_completed_string_reuse", limit: domain.MaxDocDisplayStateBytes + 1},
	} {
		test.Run(selected.name, func(test *testing.T) {
			assertDocDisplayCompletedPeerSyntheticPointPlans(test, selected)
		})
	}
}

func assertDocDisplayCompletedPeerSyntheticPointPlans(test *testing.T, selected docDisplayPeerPlanCase) {
	test.Helper()
	storage, ctx := rootProjectionIsolatedPG(test)
	repo, _ := rootPGRepo(test, ctx, storage)
	source, event := maintenanceSource(test, ctx, storage, "synthetic completed peer lookup "+selected.name)
	document := domain.HashContent([]byte("synthetic lookup-only document"))
	candidate := maintenanceObject(test, map[string]any{"synthetic": "completed peer lookup candidate"})
	filler := maintenanceObject(test, map[string]any{"synthetic": "ineligible peer lookup filler"})
	const fillerCount = 5000
	const otherProfileCount = 16
	otherProfile, otherProtocol := domain.DocDisplayStringProfile, 2
	if selected.profile == domain.DocDisplayStringProfile {
		otherProfile, otherProtocol = domain.DocDisplayProjectionProfile, 5
	}
	test.Log("Synthetic hash-consistent job rows exercise SQL selection and plans only; no typed checkpoints or approved publications are seeded.")
	tag, err := docDisplayJobCorruptionPG(ctx, storage, `WITH entries AS (
 SELECT ordinal,CASE WHEN ordinal=$5 THEN $6::text ELSE $8::text END AS hash,
 CASE WHEN ordinal=$5 THEN $7::bytea ELSE $9::bytea END AS bytes
 FROM generate_series(0,$5::integer) AS entry(ordinal)
 ) INSERT INTO doc_display_jobs_v1
 (repo_id,doc_hash,profile,event_index,event_hash,event_offset,event_length,event_seq,event_role,
 source_id,source_version,range_generation,state,created_at,next_attempt,
 source_hash,checkpoint_hash,checkpoint,preparation_hash,preparation,worker_protocol,catalog_epoch)
 SELECT $1,$2,$10::text,entry.ordinal,$3,0,100,0,'',
 gen_random_uuid(),0,$2,
 CASE WHEN entry.ordinal=$5 THEN 'completed' WHEN entry.ordinal%2=0 THEN 'waiting' ELSE 'rejected' END,
 clock_timestamp(),clock_timestamp(),$4,
 CASE WHEN $10::text='tool-result-string-v1' THEN entry.hash END,
 CASE WHEN $10::text='tool-result-string-v1' THEN entry.bytes END,
 CASE WHEN $10::text='public-event-display-v1' THEN entry.hash END,
 CASE WHEN $10::text='public-event-display-v1' THEN entry.bytes END,$11::integer,
 CASE WHEN $10::text='public-event-display-v1' THEN gen_random_uuid() END
 FROM entries entry`, repo, document, event, source,
		fillerCount, candidate.Hash, candidate.Bytes, filler.Hash, filler.Bytes, selected.profile, selected.protocol)
	if err != nil || tag.RowsAffected() != fillerCount+1 {
		test.Fatal("seed same-source synthetic candidate and waiting/rejected prefix", tag.RowsAffected(), err)
	}
	tag, err = docDisplayJobCorruptionPG(ctx, storage, `INSERT INTO doc_display_jobs_v1
 (repo_id,doc_hash,profile,event_index,event_hash,event_offset,event_length,event_seq,event_role,
 source_id,source_version,range_generation,state,created_at,next_attempt,
 source_hash,checkpoint_hash,checkpoint,preparation_hash,preparation,worker_protocol,catalog_epoch)
 SELECT $1,$2,$8::text,entry.ordinal,$3,0,100,0,'',
 gen_random_uuid(),0,$2,'completed',clock_timestamp(),clock_timestamp(),$4,
 CASE WHEN $8::text='tool-result-string-v1' THEN $5::text END,
 CASE WHEN $8::text='tool-result-string-v1' THEN $6::bytea END,
 CASE WHEN $8::text='public-event-display-v1' THEN $5::text END,
 CASE WHEN $8::text='public-event-display-v1' THEN $6::bytea END,$9::integer,
 CASE WHEN $8::text='public-event-display-v1' THEN gen_random_uuid() END
 FROM generate_series(0,$7::integer-1) AS entry(ordinal)`, repo, document, event, source,
		filler.Hash, filler.Bytes, otherProfileCount, otherProfile, otherProtocol)
	if err != nil || tag.RowsAffected() != otherProfileCount {
		test.Fatal("seed legal completed wrong-profile/protocol peers", tag.RowsAffected(), err)
	}
	var total, eligible, waiting, rejected, otherProfileRows int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*),
 count(*) FILTER (WHERE profile=$2 AND worker_protocol=$3 AND state='completed'
 AND CASE WHEN $2::text='tool-result-string-v1' THEN checkpoint_hash IS NOT NULL AND checkpoint IS NOT NULL
 ELSE preparation_hash IS NOT NULL AND preparation IS NOT NULL END),
 count(*) FILTER (WHERE state='waiting'),count(*) FILTER (WHERE state='rejected'),
 count(*) FILTER (WHERE profile=$4 AND worker_protocol=$5 AND state='completed')
 FROM doc_display_jobs_v1 WHERE source_hash=$1`, source, selected.profile, selected.protocol, otherProfile, otherProtocol).Scan(&total, &eligible, &waiting, &rejected, &otherProfileRows); err != nil {
		test.Fatal(err)
	}
	if total != fillerCount+otherProfileCount+1 || eligible != 1 || waiting != fillerCount/2 || rejected != fillerCount/2 || otherProfileRows != otherProfileCount {
		test.Fatal("synthetic lookup fixture lacks its adversarial prefix or unique candidate", total, eligible, waiting, rejected, otherProfileRows)
	}
	var publications int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM doc_display_generations_v1 WHERE source_hash=$1`, source).Scan(&publications); err != nil || publications != 0 {
		test.Fatal("synthetic plan fixture must not seed approved publication evidence", publications, err)
	}
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	var selectedHash domain.ContentHash
	var selectedRaw []byte
	arguments := []any{source, selected.limit}
	destinations := []any{&selectedHash, &selectedRaw}
	var epoch *string
	if selected.profile == domain.DocDisplayProjectionProfile {
		destinations = append(destinations, &epoch)
	}
	if err := transaction.QueryRow(ctx, selected.query, arguments...).Scan(destinations...); err != nil {
		test.Fatal("select synthetic lookup candidate", err)
	}
	if selectedHash != candidate.Hash || !bytes.Equal(selectedRaw, candidate.Bytes) || domain.HashContent(selectedRaw) != selectedHash {
		test.Fatal("peer lookup selected an ineligible row or lost its stored hash proof", selectedHash)
	}
	if _, err := transaction.Exec(ctx, `ANALYZE doc_display_jobs_v1`); err != nil {
		test.Fatal(err)
	}
	for _, sequential := range []string{"on", "off"} {
		if _, err := transaction.Exec(ctx, `SET LOCAL enable_seqscan=`+sequential); err != nil {
			test.Fatal(err)
		}
		for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
			encoded := explainDocToolOrdersPGTraceQuery(test, ctx, transaction, selected.query, arguments, mode)
			var plans []struct{ Plan docEventDisplayReusePlan }
			if err := json.Unmarshal(encoded, &plans); err != nil || len(plans) != 1 {
				test.Fatal("invalid completed peer query plan", mode, sequential, err)
			}
			if plans[0].Plan.Rows != 1 || plans[0].Plan.Loops != 1 {
				test.Fatalf("completed peer plan did not return exactly one candidate: mode=%s seqscan=%s plan=%s", mode, sequential, encoded)
			}
			probes := 0
			var inspect func(docEventDisplayReusePlan)
			inspect = func(plan docEventDisplayReusePlan) {
				if plan.Filter != "" || plan.Filtered != 0 || plan.JoinFiltered != 0 || plan.IndexRechecked != 0 ||
					plan.NodeType == "Sort" || plan.NodeType == "Incremental Sort" {
					test.Fatalf("completed peer lookup filtered or sorted a prefix: mode=%s seqscan=%s plan=%s", mode, sequential, encoded)
				}
				if plan.Relation == "doc_display_jobs_v1" {
					probes++
					if plan.Index != selected.index ||
						plan.NodeType != "Index Scan" && plan.NodeType != "Index Only Scan" ||
						!strings.Contains(plan.Condition, "source_hash =") || plan.Rows != 1 || plan.Loops != 1 {
						test.Fatalf("completed peer lookup is not a one-candidate partial-index point probe: mode=%s seqscan=%s plan=%s", mode, sequential, encoded)
					}
				}
				for _, child := range plan.Plans {
					inspect(child)
				}
			}
			inspect(plans[0].Plan)
			if probes != 1 {
				test.Fatalf("completed peer lookup did not use exactly one job-table probe: mode=%s seqscan=%s probes=%d plan=%s", mode, sequential, probes, encoded)
			}
			test.Logf("profile=%s protocol=%d mode=%s seqscan=%s same_source_waiting_rejected=%d wrong_profile_protocol_completed=%d candidates=1 partial_index=%s filtered=0", selected.profile, selected.protocol, mode, sequential, fillerCount, otherProfileCount, selected.index)
		}
	}
}
