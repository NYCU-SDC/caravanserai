package v1

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var t0 = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func readyCond(s NodeStatus) Condition {
	c, _ := s.readyCondition()
	return c
}

func TestNodeStatusSetState(t *testing.T) {
	t.Run("a transition stamps the Ready condition", func(t *testing.T) {
		s := NodeStatus{State: NodeStateNotReady}
		s.SetState(NodeStateReady, "AgentHeartbeat", "back", t0)

		assert.Equal(t, NodeStateReady, s.State)
		c := readyCond(s)
		assert.Equal(t, ConditionTrue, c.Status)
		assert.Equal(t, "AgentHeartbeat", c.Reason)
		assert.Equal(t, t0, c.LastTransitionTime)
		since, ok := s.ReadySince()
		assert.True(t, ok)
		assert.Equal(t, t0, since)
	})

	t.Run("re-asserting the same state changes nothing", func(t *testing.T) {
		s := NodeStatus{State: NodeStateNotReady}
		s.SetState(NodeStateReady, "AgentHeartbeat", "back", t0)
		s.SetState(NodeStateReady, "AgentHeartbeat", "still here", t0.Add(30*time.Second))
		s.SetState(NodeStateReady, "AgentReady", "health check agrees", t0.Add(time.Minute))

		c := readyCond(s)
		assert.Equal(t, t0, c.LastTransitionTime, "every heartbeat must not restart the clock")
		assert.Equal(t, "back", c.Message)
	})

	t.Run("going NotReady is recorded", func(t *testing.T) {
		s := NodeStatus{State: NodeStateReady}
		s.SetState(NodeStateNotReady, "HeartbeatTimeout", "gone", t0)
		s.SetState(NodeStateNotReady, "HeartbeatTimeout", "still gone", t0.Add(time.Minute))
		s.SetState(NodeStateReady, "AgentHeartbeat", "back", t0.Add(time.Hour))
		s.SetState(NodeStateNotReady, "HeartbeatTimeout", "gone again", t0.Add(2*time.Hour))

		assert.Equal(t, []time.Time{t0, t0.Add(2 * time.Hour)}, s.RecentNotReady, "one entry per transition")
		assert.Equal(t, ConditionFalse, readyCond(s).Status)
		_, ok := s.ReadySince()
		assert.False(t, ok, "not Ready, so no Ready time")
	})

	t.Run("a condition out of step with the state is corrected and stamped now", func(t *testing.T) {
		// What a heartbeat that set State directly used to leave behind.
		s := NodeStatus{
			State:      NodeStateReady,
			Conditions: []Condition{{Type: ConditionTypeReady, Status: ConditionFalse, Reason: "HeartbeatTimeout", LastTransitionTime: t0.Add(-48 * time.Hour)}},
		}
		_, ok := s.ReadySince()
		assert.False(t, ok, "a False condition is not a Ready time")

		s.SetState(NodeStateReady, "AgentHeartbeat", "back", t0)
		since, ok := s.ReadySince()
		require.True(t, ok)
		assert.Equal(t, t0, since)
		assert.Empty(t, s.RecentNotReady, "correcting the condition is not a NotReady transition")
	})

	t.Run("a Node with no condition yet gets one", func(t *testing.T) {
		s := NodeStatus{State: NodeStateReady}
		s.SetState(NodeStateReady, "AgentHeartbeat", "up", t0)
		since, ok := s.ReadySince()
		require.True(t, ok)
		assert.Equal(t, t0, since)
	})

	t.Run("other conditions are kept", func(t *testing.T) {
		s := NodeStatus{State: NodeStateNotReady, Conditions: []Condition{{Type: ConditionTypeDiskPressure, Status: ConditionFalse}}}
		s.SetState(NodeStateReady, "AgentHeartbeat", "back", t0)
		require.Len(t, s.Conditions, 2)
		assert.Equal(t, ConditionTypeDiskPressure, s.Conditions[0].Type)
	})

	t.Run("does not write through to the caller's slices", func(t *testing.T) {
		conds := []Condition{{Type: ConditionTypeReady, Status: ConditionFalse, LastTransitionTime: t0.Add(-time.Hour)}}
		s := NodeStatus{State: NodeStateNotReady, Conditions: conds}
		s.SetState(NodeStateReady, "AgentHeartbeat", "back", t0)
		assert.Equal(t, ConditionFalse, conds[0].Status, "the original slice is untouched")
	})
}

func TestNodeStatusNotReadyHistoryIsBounded(t *testing.T) {
	t.Run("transitions older than the retention are dropped", func(t *testing.T) {
		s := NodeStatus{State: NodeStateReady, RecentNotReady: []time.Time{t0.Add(-8 * 24 * time.Hour), t0.Add(-6 * 24 * time.Hour)}}
		s.SetState(NodeStateNotReady, "HeartbeatTimeout", "gone", t0)
		assert.Equal(t, []time.Time{t0.Add(-6 * 24 * time.Hour), t0}, s.RecentNotReady)
	})

	t.Run("at most the newest NotReadyHistoryMax are kept", func(t *testing.T) {
		s := NodeStatus{State: NodeStateReady}
		for i := range NotReadyHistoryMax + 5 {
			at := t0.Add(time.Duration(i) * time.Hour)
			s.SetState(NodeStateNotReady, "HeartbeatTimeout", "gone", at)
			s.SetState(NodeStateReady, "AgentHeartbeat", "back", at.Add(time.Minute))
		}
		require.Len(t, s.RecentNotReady, NotReadyHistoryMax)
		assert.Equal(t, t0.Add(time.Duration(NotReadyHistoryMax+4)*time.Hour), s.RecentNotReady[len(s.RecentNotReady)-1])
		assert.Equal(t, t0.Add(5*time.Hour), s.RecentNotReady[0])
	})
}
