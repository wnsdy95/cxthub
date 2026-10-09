package app

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type CaptureRecoveryService struct {
	journal  outbound.CaptureRecoveryJournal
	evidence outbound.CaptureRecoveryEvidence
}

func NewCaptureRecoveryService(j outbound.CaptureRecoveryJournal, e outbound.CaptureRecoveryEvidence) *CaptureRecoveryService {
	return &CaptureRecoveryService{journal: j, evidence: e}
}

func (s *CaptureRecoveryService) Inspect(ctx context.Context, repo string) ([]domain.CaptureRecoveryStatus, error) {
	statuses, _, err := s.inspect(ctx, repo, true)
	return statuses, err
}

func (s *CaptureRecoveryService) inspect(ctx context.Context, repo string, discover bool) ([]domain.CaptureRecoveryStatus, map[string]domain.CaptureAttempt, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repo)); err != nil {
		return nil, nil, err
	}
	passes, err := s.journal.ListCaptureAttempts(ctx, repo)
	if err != nil {
		return nil, nil, err
	}
	history, err := s.evidence.ListHistoryEvents(ctx, repo)
	if err != nil {
		return nil, nil, err
	}
	accepted := map[string]domain.HistoryEvent{}
	for _, e := range history {
		if e.RepoID != repo {
			return nil, nil, domain.ErrHashMismatch
		}
		if err := domain.ValidateHistoryEvent(e); err != nil {
			return nil, nil, err
		}
		if old, ok := accepted[e.ID]; ok && !reflect.DeepEqual(old, e) {
			return nil, nil, domain.ErrHashMismatch
		}
		accepted[e.ID] = e
	}
	byID := map[string]domain.CaptureAttempt{}
	for _, p := range passes {
		if err := p.Validate(); err != nil {
			return nil, nil, err
		}
		if p.Proof.RepoID != repo {
			return nil, nil, domain.ErrHashMismatch
		}
		if _, ok := byID[p.Proof.ID]; ok {
			return nil, nil, domain.ErrHashMismatch
		}
		byID[p.Proof.ID] = p
	}
	sort.Slice(passes, func(i, j int) bool { return passes[i].Proof.ID < passes[j].Proof.ID })
	var statuses []domain.CaptureRecoveryStatus
	for _, p := range passes {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		resolution, err := s.journal.ReadCaptureResolution(ctx, p)
		if err != nil {
			return nil, nil, err
		}
		st := domain.CaptureRecoveryStatus{ID: p.Proof.ID, RepoID: repo, WorktreeID: p.Proof.WorktreeID, Branch: p.Proof.Branch, CodeCommit: p.Proof.GitAfter, Fingerprint: p.Fingerprint(), State: "needs-review", Outcomes: p.Outcomes}
		// Diagnostics expose provenance and hashes, never private native-memory
		// bodies from the replay receipt (which may contain unmasked secrets).
		st.Outcomes = append([]domain.CaptureOutcome(nil), p.Outcomes...)
		for i, o := range st.Outcomes {
			if o.Input != nil {
				input := *o.Input
				input.NativeMemory = nil
				st.Outcomes[i].Input = &input
			}
		}
		if retryReader, ok := s.journal.(outbound.CaptureRetryReader); ok {
			st.Retry, err = retryReader.ReadCaptureRetry(ctx, p)
			if err != nil {
				return nil, nil, err
			}
		}
		if p.Complete {
			st.State = "ready-to-publish"
			if p.Proof.Target == "" || acceptedPublication(p, accepted) {
				st.State = "completed"
			}
		} else if resolution == nil {
			if p.Version == 2 && p.InputsReady {
				st.State = "ready-to-capture"
				if st.Retry != nil && st.Retry.Tries >= 8 {
					st.State = "capture-retry-paused"
				}
			}
			ready := true
			for _, o := range p.Outcomes {
				ready = ready && (o.State == "saved" || o.State == "absent")
			}
			if ready && st.State != "capture-retry-paused" {
				st.State = "ready-to-retry"
			}
			for _, candidate := range passes {
				if !discover {
					break
				}
				if candidate.Proof.ID == p.Proof.ID || !candidate.Complete || candidate.Proof.Target == "" ||
					!domain.SameCapturePosition(p, candidate) || candidate.Proof.CreatedAt.Before(p.Proof.CreatedAt) || !acceptedPublication(candidate, accepted) {
					continue
				}
				covers, err := s.covers(ctx, p, candidate)
				if err != nil {
					return nil, nil, err
				}
				if covers {
					st.State, st.ReplacementID, st.PublicationID = "superseded", candidate.Proof.ID, candidate.Publication().ID
					break
				}
			}
		}
		if resolution != nil {
			if resolution.AttemptHash != p.Fingerprint() {
				return nil, nil, fmt.Errorf("%w: capture resolution no longer matches attempt %s", domain.ErrSyncConflict, p.Proof.ID)
			}
			switch resolution.Kind {
			case "superseded":
				replacement, ok := byID[resolution.ReplacementID]
				if !ok || replacement.Proof.CreatedAt.Before(p.Proof.CreatedAt) || replacement.Fingerprint() != resolution.ReplacementHash || !domain.SameCapturePosition(p, replacement) || !acceptedPublication(replacement, accepted) || resolution.PublicationID != replacement.Publication().ID {
					return nil, nil, domain.ErrHashMismatch
				}
				st.State, st.ReplacementID, st.PublicationID = "superseded", resolution.ReplacementID, resolution.PublicationID
			case "acknowledged-gap":
				if p.Complete {
					return nil, nil, domain.ErrSyncConflict
				}
				st.State = "acknowledged-gap"
			default:
				return nil, nil, domain.ErrHashMismatch
			}
			st.Resolution = resolution
		}
		statuses = append(statuses, st)
	}
	return statuses, byID, nil
}

func acceptedPublication(p domain.CaptureAttempt, events map[string]domain.HistoryEvent) bool {
	if !p.Complete || p.Proof.Target == "" {
		return false
	}
	expected := p.Publication()
	got, ok := events[expected.ID]
	if !ok || got.CreatedAt.Before(p.Proof.CreatedAt) {
		return false
	}
	expected.CreatedAt = got.CreatedAt
	return reflect.DeepEqual(expected, got)
}

func (s *CaptureRecoveryService) covers(ctx context.Context, old, replacement domain.CaptureAttempt) (bool, error) {
	for _, o := range old.Outcomes {
		found := false
		for _, n := range replacement.Outcomes {
			if o.Provider == n.Provider && (o.State == "absent" || n.State == "saved") && (o.SessionPath == "" || o.SessionPath == n.SessionPath) {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	wanted := map[domain.ContentHash]bool{}
	if old.Initial != "" {
		wanted[old.Initial] = true
	}
	for _, o := range old.Outcomes {
		if o.Target != "" {
			wanted[o.Target] = true
		}
	}
	stack, seen := []domain.ContentHash{replacement.Proof.Target}, map[domain.ContentHash]bool{}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		snap, err := s.evidence.GetSnapshot(ctx, id)
		if err != nil {
			return false, err
		}
		if snap.ID != id || snap.RepoID != old.Proof.RepoID {
			return false, domain.ErrHashMismatch
		}
		delete(wanted, id)
		if len(wanted) == 0 {
			return true, nil
		}
		stack = append(stack, snap.ReachabilityParents()...)
	}
	return false, nil
}

// Resolve records operator/replacement evidence only. It creates no context,
// publication, branch movement, provider session, or retroactive completion.
func (s *CaptureRecoveryService) Resolve(ctx context.Context, repo, id string, expect domain.ContentHash, reason string, acknowledgeGap bool) (domain.CaptureResolution, error) {
	statuses, passes, err := s.inspect(ctx, repo, true)
	if err != nil {
		return domain.CaptureResolution{}, err
	}
	for _, st := range statuses {
		if st.ID != id {
			continue
		}
		if st.Fingerprint != expect {
			return domain.CaptureResolution{}, domain.ErrSyncConflict
		}
		if st.Resolution != nil {
			if acknowledgeGap != (st.Resolution.Kind == "acknowledged-gap") || acknowledgeGap && st.Resolution.Reason != strings.TrimSpace(reason) {
				return domain.CaptureResolution{}, domain.ErrSyncConflict
			}
			return *st.Resolution, nil
		}
		r := domain.CaptureResolution{Version: 1, RepoID: repo, WorktreeID: st.WorktreeID, AttemptID: id, AttemptHash: expect, CreatedAt: time.Now().UTC()}
		witnesses := []domain.CaptureAttempt{passes[id]}
		if acknowledgeGap {
			r.Kind, r.Reason = "acknowledged-gap", strings.TrimSpace(reason)
			if st.State != "needs-review" || len(r.Reason) == 0 || len(r.Reason) > 1000 {
				return r, fmt.Errorf("only an unproven capture can be acknowledged with a reason (1-1000 bytes)")
			}
		} else {
			if st.State != "superseded" {
				return r, fmt.Errorf("capture %s is %s; inspect evidence before retrying or acknowledging a gap", id, st.State)
			}
			r.Kind, r.ReplacementID, r.PublicationID = "superseded", st.ReplacementID, st.PublicationID
			r.ReplacementHash = passes[st.ReplacementID].Fingerprint()
			witnesses = append(witnesses, passes[st.ReplacementID])
		}
		return r, s.journal.WriteCaptureResolution(ctx, r, witnesses)
	}
	return domain.CaptureResolution{}, domain.ErrNotFound
}

// RetryAttempt never invents an outcome or scans a provider. The delivery layer
// can replay only the selected frozen attempt through the existing publisher.
func (s *CaptureRecoveryService) RetryAttempt(ctx context.Context, repo, id string, expect domain.ContentHash) (domain.CaptureAttempt, error) {
	statuses, passes, err := s.inspect(ctx, repo, true)
	if err != nil {
		return domain.CaptureAttempt{}, err
	}
	for _, st := range statuses {
		if st.ID != id {
			continue
		}
		if st.Fingerprint != expect {
			return domain.CaptureAttempt{}, domain.ErrSyncConflict
		}
		if st.Resolution != nil || (st.State != "capture-retry-paused" && st.State != "ready-to-capture" && st.State != "ready-to-retry" && st.State != "ready-to-publish" && st.State != "completed") {
			return domain.CaptureAttempt{}, fmt.Errorf("capture %s has no replayable completion evidence (%s)", id, st.State)
		}
		return passes[id], nil
	}
	return domain.CaptureAttempt{}, domain.ErrNotFound
}

// RecordedResolutions validates durable decisions without re-evaluating unrelated
// failed attempts against today's graph. Replay does not discover or accept new
// replacement evidence implicitly.
func (s *CaptureRecoveryService) RecordedResolutions(ctx context.Context, repo string) (map[string]domain.CaptureResolution, error) {
	states, _, err := s.inspect(ctx, repo, false)
	if err != nil {
		return nil, err
	}
	out := map[string]domain.CaptureResolution{}
	for _, st := range states {
		if st.Resolution != nil {
			out[st.ID] = *st.Resolution
		}
	}
	return out, nil
}
