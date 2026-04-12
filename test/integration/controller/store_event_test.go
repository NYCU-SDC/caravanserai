//go:build e2e

package controller

import (
	"context"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/event"
	pgstore "NYCU-SDC/caravanserai/internal/store/postgres"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUpdateNodeStatusOnlyPublishesOnPhaseChange validates that
// UpdateNodeStatus publishes a TopicNodeUpdated event only when the node's
// phase (state) actually changes, not on every call.
//
// Acceptance criteria covered:
//   - Same-state calls produce no event after the first
//   - State transitions (Ready→NotReady, NotReady→Ready) produce events
func TestUpdateNodeStatusOnlyPublishesOnPhaseChange(t *testing.T) {
	ctx := context.Background()

	bus := event.New(shared.logger, 256)
	store, err := pgstore.New(ctx, shared.databaseURL, shared.logger, bus)
	require.NoError(t, err, "pgstore.New")
	defer store.Close()

	// Clean slate.
	_, err = shared.pool.Exec(ctx, "TRUNCATE TABLE resources")
	require.NoError(t, err, "truncate")

	// Subscribe to node.updated events before any operations.
	sub := bus.Subscribe(event.TopicNodeUpdated)

	// Create a node in Ready state.
	node := &v1.Node{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Node"},
		ObjectMeta: v1.ObjectMeta{Name: "event-test-node"},
		Spec:       v1.NodeSpec{Hostname: "event-test-node"},
		Status: v1.NodeStatus{
			State:         v1.NodeStateReady,
			LastHeartbeat: time.Now().UTC(),
		},
	}
	require.NoError(t, store.CreateNode(ctx, node))

	// Drain the node.created event (CreateNode does not publish
	// TopicNodeUpdated, but let's be safe).
	drainEvents(sub, 100*time.Millisecond)

	// --- Test 1: First UpdateNodeStatus with same state → event (phase is
	// written from the initial CreateNode value, but the CTE compares old
	// vs. new; since CreateNode already set phase="Ready" and we're
	// updating with State=Ready, old==new, so NO event should fire).
	readyStatus := v1.NodeStatus{
		State:         v1.NodeStateReady,
		LastHeartbeat: time.Now().UTC(),
	}
	require.NoError(t, store.UpdateNodeStatus(ctx, "event-test-node", readyStatus))

	events := collectEvents(sub, 200*time.Millisecond)
	assert.Empty(t, events, "same-state UpdateNodeStatus (Ready→Ready) should not publish an event")

	// --- Test 2: Second call with same state → still no event.
	readyStatus.LastHeartbeat = time.Now().UTC()
	require.NoError(t, store.UpdateNodeStatus(ctx, "event-test-node", readyStatus))

	events = collectEvents(sub, 200*time.Millisecond)
	assert.Empty(t, events, "repeated same-state UpdateNodeStatus should not publish an event")

	// --- Test 3: Transition Ready → NotReady → event published.
	notReadyStatus := v1.NodeStatus{
		State:         v1.NodeStateNotReady,
		LastHeartbeat: time.Now().UTC(),
	}
	require.NoError(t, store.UpdateNodeStatus(ctx, "event-test-node", notReadyStatus))

	events = collectEvents(sub, 200*time.Millisecond)
	require.Len(t, events, 1, "Ready→NotReady transition should publish exactly one event")
	assert.Equal(t, "event-test-node", events[0].Name)

	// --- Test 4: Same NotReady state again → no event.
	notReadyStatus.LastHeartbeat = time.Now().UTC()
	require.NoError(t, store.UpdateNodeStatus(ctx, "event-test-node", notReadyStatus))

	events = collectEvents(sub, 200*time.Millisecond)
	assert.Empty(t, events, "same-state UpdateNodeStatus (NotReady→NotReady) should not publish an event")

	// --- Test 5: Transition NotReady → Ready → event published.
	readyStatus.LastHeartbeat = time.Now().UTC()
	require.NoError(t, store.UpdateNodeStatus(ctx, "event-test-node", readyStatus))

	events = collectEvents(sub, 200*time.Millisecond)
	require.Len(t, events, 1, "NotReady→Ready transition should publish exactly one event")
	assert.Equal(t, "event-test-node", events[0].Name)
}

// drainEvents reads and discards all events from the channel until the timeout
// elapses with no new events.
func drainEvents(ch event.Handler, timeout time.Duration) {
	for {
		select {
		case <-ch:
		case <-time.After(timeout):
			return
		}
	}
}

// collectEvents reads events from the channel until the timeout elapses with
// no new events.
func collectEvents(ch event.Handler, timeout time.Duration) []event.Event {
	var events []event.Event
	for {
		select {
		case e := <-ch:
			events = append(events, e)
		case <-time.After(timeout):
			return events
		}
	}
}
