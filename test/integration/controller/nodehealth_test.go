//go:build e2e

package controller

import (
	"context"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/controller"
	"NYCU-SDC/caravanserai/test/integration/controllerhelper"

	"github.com/stretchr/testify/require"
)

// TestNodeHealthMarksStaleNodeNotReady validates:
//
//  1. Create a Node with a heartbeat timestamp in the past (>90s ago)
//  2. Start controllers
//  3. Wait for NodeHealthController to mark it NotReady
func TestNodeHealthMarksStaleNodeNotReady(t *testing.T) {
	// Use a fake clock so we can control "now" without waiting.
	clock := &fakeClock{now: time.Now().UTC()}

	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger,
		controller.WithClock(clock),
	)
	s.TruncateAll(t)

	ctx := context.Background()

	// Create a node with a heartbeat that is stale relative to the fake clock.
	// Set the heartbeat to 2 minutes before the fake clock's "now", which is
	// well past the 90-second timeout.
	staleHeartbeat := clock.Now().Add(-2 * time.Minute)
	node := &v1.Node{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Node"},
		ObjectMeta: v1.ObjectMeta{Name: "health-node-01"},
		Spec:       v1.NodeSpec{Hostname: "health-node-01"},
		Status: v1.NodeStatus{
			State:         v1.NodeStateReady,
			LastHeartbeat: staleHeartbeat,
		},
	}
	require.NoError(t, s.Store.CreateNode(ctx, node))

	// Start the controller manager.
	s.Start(t)

	// Wait for NodeHealth to detect the stale heartbeat and mark NotReady.
	controllerhelper.WaitForNodeState(t, s.Store, 15*time.Second, "health-node-01", v1.NodeStateNotReady)
}
