package app

import (
	"strings"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

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
