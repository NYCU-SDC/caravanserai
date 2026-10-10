//go:build e2e

package controller

import (
	"context"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/dailywindow"
	"NYCU-SDC/caravanserai/internal/server/adapter"
	ctrlpkg "NYCU-SDC/caravanserai/internal/server/controller"
	"NYCU-SDC/caravanserai/test/integration/controllerhelper"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedClock reads as one instant, so a test decides whether it is inside
// the fail-back window.
type fixedClock time.Time

func (c fixedClock) Now() time.Time                  { return time.Time(c) }
func (c fixedClock) Since(t time.Time) time.Duration { return time.Time(c).Sub(t) }

var (
	taipeiNight = time.Date(2026, 10, 9, 18, 30, 0, 0, time.UTC) // 02:30 Taiwan
	taipeiDay   = time.Date(2026, 10, 10, 2, 30, 0, 0, time.UTC) // 10:30 Taiwan
)

// newFailback builds a fail-back controller over the suite's store with a
// 02:00-05:00 Taiwan window and the clock fixed at now. It is driven by hand:
// the suite's manager does not run it.
func newFailback(t *testing.T, s *controllerhelper.Suite, now time.Time) *ctrlpkg.FailbackController {
	t.Helper()
	w, err := dailywindow.Parse("02:00-05:00", "Asia/Taipei")
	require.NoError(t, err)
	return ctrlpkg.NewFailbackController(shared.logger,
		adapter.NewProjectStoreAdapter(s.Store), adapter.NewNodeReadyAdapter(s.Store),
		&w, ctrlpkg.WithClock(fixedClock(now)))
}

func runFailback(t *testing.T, fb *ctrlpkg.FailbackController) {
	t.Helper()
	_, err := fb.Reconcile(context.Background(), "failback")
	require.NoError(t, err)
}

// TestFailbackMovesProjectBackToPrimary validates fail-back end to end inside
// the window: the project on backup is moved through the eviction controller
// and placed on primary by the scheduler.
func TestFailbackMovesProjectBackToPrimary(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)

	createLedgerNode(t, s, "fb-pve1", "primary", controllerhelper.RoomyAllocatable(), false)
	createLedgerNode(t, s, "fb-twcc1", "backup", controllerhelper.RoomyAllocatable(), false)
	createLedgerNode(t, s, "fb-twcc2", "backup", controllerhelper.RoomyAllocatable(), false)
	createRunningProject(t, s, "fb-app", "fb-twcc1")
	s.Start(t)

	runFailback(t, newFailback(t, s, taipeiNight))

	ctx := context.Background()
	p, err := s.Store.GetProject(ctx, "fb-app")
	require.NoError(t, err)
	require.NotNil(t, p.Status.Eviction, "a fail-back move was asked for")
	assert.Equal(t, ctrlpkg.FailBackReason, p.Status.Eviction.Reason)
	assert.Equal(t, v1.NodeTierPrimary, p.Status.Eviction.TargetTier)

	waitForEvictionState(t, s, "fb-app", v1.EvictionStateBackingUp)
	agentReportsBackup(t, s, "fb-app", v1.ConditionTrue, "backup-1")

	controllerhelper.WaitForCondition(t, 20*time.Second, 200*time.Millisecond, "back on fb-pve1", func() bool {
		p, err := s.Store.GetProject(ctx, "fb-app")
		return err == nil && p.Status.NodeRef == "fb-pve1" && p.Status.Eviction == nil
	})
}

// TestFailbackOutsideWindowDoesNothing validates that fail-back never moves
// a project outside its window, however much room primary has.
func TestFailbackOutsideWindowDoesNothing(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)

	createLedgerNode(t, s, "fb-pve1", "primary", controllerhelper.RoomyAllocatable(), false)
	createLedgerNode(t, s, "fb-twcc1", "backup", controllerhelper.RoomyAllocatable(), false)
	createRunningProject(t, s, "fb-app", "fb-twcc1")
	s.Start(t)

	runFailback(t, newFailback(t, s, taipeiDay))

	p, err := s.Store.GetProject(context.Background(), "fb-app")
	require.NoError(t, err)
	assert.Nil(t, p.Status.Eviction)
	assert.Equal(t, "fb-twcc1", p.Status.NodeRef)
	assert.Equal(t, v1.ProjectPhaseRunning, p.Status.Phase)
}

// TestFailbackOneAtATime validates that fail-back asks for the next move only
// after the last one is over.
func TestFailbackOneAtATime(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)

	createLedgerNode(t, s, "fb-pve1", "primary", controllerhelper.RoomyAllocatable(), false)
	createLedgerNode(t, s, "fb-twcc1", "backup", controllerhelper.RoomyAllocatable(), false)
	createRunningProject(t, s, "fb-a", "fb-twcc1")
	createRunningProject(t, s, "fb-b", "fb-twcc1")
	s.Start(t)

	fb := newFailback(t, s, taipeiNight)
	runFailback(t, fb)
	waitForEvictionState(t, s, "fb-a", v1.EvictionStateBackingUp)

	runFailback(t, fb)
	assert.Equal(t, v1.EvictionState(""), evictionState(t, s, "fb-b"), "fb-b waits while fb-a is moving")

	agentReportsBackup(t, s, "fb-a", v1.ConditionTrue, "backup-a")
	controllerhelper.WaitForCondition(t, 20*time.Second, 200*time.Millisecond, "fb-a done", func() bool {
		return evictionState(t, s, "fb-a") == ""
	})

	runFailback(t, fb)
	assert.NotEqual(t, v1.EvictionState(""), evictionState(t, s, "fb-b"), "fb-b is asked for once fb-a is over")
}
