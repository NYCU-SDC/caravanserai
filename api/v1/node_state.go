package v1

import (
	"slices"
	"time"
)

const (
	// NotReadyHistoryRetention is how long a NotReady transition is kept in
	// NodeStatus.RecentNotReady. Anything counting flaps over a longer span
	// would undercount.
	NotReadyHistoryRetention = 7 * 24 * time.Hour

	// NotReadyHistoryMax caps NodeStatus.RecentNotReady, so a Node that
	// flaps all week does not grow its status without bound.
	NotReadyHistoryMax = 32
)

// SetState moves the Node to state and keeps what depends on it consistent:
// the Ready condition, whose LastTransitionTime says since when the Node has
// been in its current readiness, and RecentNotReady.
//
// It changes nothing when the Node is already in state and the Ready
// condition agrees, so a caller re-asserting the state on every heartbeat
// does not move the transition time. When the condition disagrees — as it
// did on any Node a heartbeat had brought back before this was the one way
// state changed — it is corrected and stamped now: the Node is known to be in
// state from now, not since when.
func (s *NodeStatus) SetState(state NodeState, reason, message string, now time.Time) {
	want := ConditionFalse
	if state == NodeStateReady {
		want = ConditionTrue
	}

	ready, found := s.readyCondition()
	if s.State == state && found && ready.Status == want {
		return
	}

	if state == NodeStateNotReady && s.State != NodeStateNotReady {
		s.RecentNotReady = append(slices.Clone(s.RecentNotReady), now)
	}
	s.RecentNotReady = pruneNotReady(s.RecentNotReady, now)

	s.State = state
	s.Conditions = UpsertCondition(slices.Clone(s.Conditions), Condition{
		Type:               ConditionTypeReady,
		Status:             want,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	}, true)
}

// ReadySince returns when the Node became Ready, and false when it is not
// Ready or that time is not known.
func (s NodeStatus) ReadySince() (time.Time, bool) {
	ready, found := s.readyCondition()
	if s.State != NodeStateReady || !found || ready.Status != ConditionTrue || ready.LastTransitionTime.IsZero() {
		return time.Time{}, false
	}
	return ready.LastTransitionTime, true
}

func (s NodeStatus) readyCondition() (Condition, bool) {
	for _, c := range s.Conditions {
		if c.Type == ConditionTypeReady {
			return c, true
		}
	}
	return Condition{}, false
}

// pruneNotReady drops transitions older than the retention and keeps at most
// the newest NotReadyHistoryMax.
func pruneNotReady(times []time.Time, now time.Time) []time.Time {
	cutoff := now.Add(-NotReadyHistoryRetention)
	kept := times[:0:0]
	for _, t := range times {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) > NotReadyHistoryMax {
		kept = kept[len(kept)-NotReadyHistoryMax:]
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}
