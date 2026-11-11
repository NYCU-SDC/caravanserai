//go:build e2e

package integration

import (
	"context"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/adapter"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Review A5: the heartbeat, the health controller and the drain controller all
// write the same Node status. Each changes only its own fields, on the status
// as it is when written.

func createStatusNode(t *testing.T, name string, heartbeat time.Time) {
	t.Helper()
	require.NoError(t, suite.store.CreateNode(context.Background(), &v1.Node{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Node"},
		ObjectMeta: v1.ObjectMeta{Name: name},
	}))
	// Other tests in the package list every Node.
	t.Cleanup(func() { _ = suite.store.DeleteNode(context.Background(), name) })
	require.NoError(t, suite.store.UpdateNodeStatusWithRetry(context.Background(), name, func(s *v1.NodeStatus) error {
		s.State = v1.NodeStateReady
		s.LastHeartbeat = heartbeat
		return nil
	}))
}

func nodeStatus(t *testing.T, name string) v1.NodeStatus {
	t.Helper()
	n, err := suite.store.GetNode(context.Background(), name)
	require.NoError(t, err)
	return n.Status
}

// The drain controller read the Node before the health controller marked it
// NotReady. Its write lands after; the Node stays NotReady and gets the drain
// status too.
func TestNodeStatusWriteKeepsAConcurrentWrite(t *testing.T) {
	const name = "e2e-status-writers"
	ctx := context.Background()
	heartbeat := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	createStatusNode(t, name, heartbeat)
	nodes := adapter.NewNodeStoreAdapter(suite.store)

	attempts := 0
	require.NoError(t, suite.store.UpdateNodeStatusWithRetry(ctx, name, func(s *v1.NodeStatus) error {
		attempts++
		if attempts == 1 {
			// Lands between this writer's read and its write.
			require.NoError(t, nodes.SetNodeState(ctx, name, v1.NodeStateNotReady, "HeartbeatTimeout", "timed out", heartbeat))
		}
		s.Drain = &v1.NodeDrainStatus{Phase: v1.NodeDrainDraining, Since: time.Now().UTC()}
		return nil
	}))

	assert.Equal(t, 2, attempts, "the stale write was retried on the new status")
	st := nodeStatus(t, name)
	assert.Equal(t, v1.NodeStateNotReady, st.State, "the drain write did not bring a dead Node back to Ready")
	require.NotNil(t, st.Drain)
	assert.Equal(t, v1.NodeDrainDraining, st.Drain.Phase)
}

// A NotReady decided on a heartbeat that has since been superseded is not
// written.
func TestNodeHealthDoesNotMarkANodeThatHasJustBeenHeardFrom(t *testing.T) {
	const name = "e2e-status-fresh-heartbeat"
	ctx := context.Background()
	stale := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	createStatusNode(t, name, stale)
	require.NoError(t, suite.store.UpdateNodeStatusWithRetry(ctx, name, func(s *v1.NodeStatus) error {
		s.LastHeartbeat = time.Now().UTC()
		return nil
	}))

	require.NoError(t, adapter.NewNodeStoreAdapter(suite.store).
		SetNodeState(ctx, name, v1.NodeStateNotReady, "HeartbeatTimeout", "timed out", stale))

	assert.Equal(t, v1.NodeStateReady, nodeStatus(t, name).State)
}

// The drain controller's write changes nothing it does not own.
func TestNodeDrainStatusTouchesOnlyTheDrain(t *testing.T) {
	const name = "e2e-status-drain-only"
	ctx := context.Background()
	heartbeat := time.Now().UTC().Truncate(time.Microsecond)
	createStatusNode(t, name, heartbeat)
	require.NoError(t, suite.store.UpdateNodeStatusWithRetry(ctx, name, func(s *v1.NodeStatus) error {
		s.Allocatable = v1.ResourceList{"cpu": "4"}
		return nil
	}))
	before := nodeStatus(t, name)

	require.NoError(t, adapter.NewDrainStoreAdapter(suite.store).SetNodeDrainStatus(ctx, name,
		&v1.NodeDrainStatus{Phase: v1.NodeDrainIncomplete, Blocking: []string{"blog"}, Since: heartbeat}))

	after := nodeStatus(t, name)
	assert.Equal(t, before.State, after.State)
	assert.True(t, before.LastHeartbeat.Equal(after.LastHeartbeat))
	assert.Equal(t, before.Allocatable, after.Allocatable)
	require.NotNil(t, after.Drain)
	assert.Equal(t, []string{"blog"}, after.Drain.Blocking)
}
