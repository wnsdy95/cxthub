package domain

import "strconv"

type PendingProjectionStatus struct {
	State       string      `json:"state"`
	SourceEpoch string      `json:"source_epoch"`
	Rules       int         `json:"rules"`
	Generation  ContentHash `json:"generation,omitempty"`
}

type GraphProjectionStatus struct {
	State        string      `json:"state"`
	SourceEpoch  string      `json:"source_epoch"`
	AppliedEpoch string      `json:"applied_epoch,omitempty"`
	Rules        int         `json:"rules"`
	Generation   ContentHash `json:"generation,omitempty"`
	Complete     bool        `json:"complete"`
	Current      bool        `json:"current"`
}

type RepositoryChangeState struct {
	RepositoryRevision
	PendingProjection *PendingProjectionStatus `json:"pending_projection,omitempty"`
	GraphProjection   *GraphProjectionStatus   `json:"graph_projection,omitempty"`
}

func (state RepositoryChangeState) Equal(other RepositoryChangeState) bool {
	if state.RepositoryRevision != other.RepositoryRevision {
		return false
	}
	if state.PendingProjection == nil || other.PendingProjection == nil {
		if state.PendingProjection != nil || other.PendingProjection != nil {
			return false
		}
	} else if *state.PendingProjection != *other.PendingProjection {
		return false
	}
	if state.GraphProjection == nil || other.GraphProjection == nil {
		return state.GraphProjection == nil && other.GraphProjection == nil
	}
	return *state.GraphProjection == *other.GraphProjection
}

func (projection GraphProjectionRead) Status() (GraphProjectionStatus, error) {
	if projection.Source.Scope != RepositoryGraphSourceScope || projection.Source.Validate() != nil || projection.ServingFence < 0 {
		return GraphProjectionStatus{}, ErrIntegrity
	}
	status := GraphProjectionStatus{
		State: "catching_up", Rules: GraphViewProjectionRules,
		SourceEpoch: projection.Source.Epoch + ":" + strconv.FormatInt(projection.Source.Sequence, 10),
	}
	if projection.Applied != (RepositoryViewSourceCheckpoint{}) {
		if projection.Applied.Scope != RepositoryGraphSourceScope || projection.Applied.Validate() != nil || projection.Applied.RepoID != projection.Source.RepoID ||
			(projection.Applied.Epoch == projection.Source.Epoch && projection.Applied.Sequence > projection.Source.Sequence) {
			return GraphProjectionStatus{}, ErrIntegrity
		}
		status.AppliedEpoch = projection.Applied.Epoch + ":" + strconv.FormatInt(projection.Applied.Sequence, 10)
	}
	if projection.Complete {
		dataset := projection.Source
		dataset.Epoch = projection.Dataset
		if projection.Applied == (RepositoryViewSourceCheckpoint{}) || projection.Applied.Epoch != projection.Source.Epoch || projection.Rules != GraphViewProjectionRules || dataset.Validate() != nil {
			return GraphProjectionStatus{}, ErrIntegrity
		}
		status.Complete = true
		status.Current = projection.Ready
		identity := projection.Dataset + ":" + status.AppliedEpoch + ":" + strconv.Itoa(projection.Rules)
		if projection.ServingFence > 0 {
			identity += ":serving:" + strconv.FormatInt(projection.ServingFence, 10)
		}
		status.Generation = HashContent([]byte(identity))
	}
	if projection.Ready && (!status.Complete || projection.Source != projection.Applied || !projection.Enabled || projection.NeedsAttention) {
		return GraphProjectionStatus{}, ErrIntegrity
	}
	switch {
	case !projection.Enabled:
		status.State = "disabled"
	case projection.NeedsAttention:
		status.State = "needs_attention"
	case projection.Ready:
		status.State = "ready"
	}
	return status, nil
}

func (projection PendingProjectionRead) Status() (PendingProjectionStatus, error) {
	if projection.Source.Scope != RepositoryViewSourceScope || projection.Source.Validate() != nil {
		return PendingProjectionStatus{}, ErrIntegrity
	}
	epoch := projection.Source.Epoch + ":" + strconv.FormatInt(projection.Source.Sequence, 10)
	status := PendingProjectionStatus{State: "catching_up", SourceEpoch: epoch, Rules: PendingProjectionRules}
	switch {
	case !projection.Enabled:
		status.State = "disabled"
	case projection.NeedsAttention:
		status.State = "needs_attention"
	case projection.Ready:
		dataset := projection.Source
		dataset.Epoch = projection.Dataset
		if projection.Source != projection.Applied || dataset.Validate() != nil {
			return PendingProjectionStatus{}, ErrIntegrity
		}
		status.State = "ready"
		status.Generation = HashContent([]byte(projection.Dataset + ":" + epoch + ":" + strconv.Itoa(PendingProjectionRules)))
	}
	return status, nil
}
