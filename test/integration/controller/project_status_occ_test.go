//go:build e2e

package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/event"
	"NYCU-SDC/caravanserai/internal/store"
	pgstore "NYCU-SDC/caravanserai/internal/store/postgres"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover what a fake store cannot: the SQL version guard itself, the
// version column advancing, and the event the store publishes. The unit tests
// beside the implementation prove the retry loop re-reads; only Postgres can
// prove the compare-and-swap actually rejects a stale write.

// occFixture builds a store with its own bus, truncates the table, and creates
// one Project to contend over.
type occFixture struct {
	store *pgstore.Store
	bus   *event.Bus
	sub   event.Handler
	name  string
}

func newOCCFixture(t *testing.T, name string) *occFixture {
	t.Helper()
	ctx := context.Background()

	bus := event.New(shared.logger, 256)
	st, err := pgstore.New(ctx, shared.databaseURL, shared.logger, bus)
	require.NoError(t, err, "pgstore.New")
	t.Cleanup(st.Close)

	_, err = shared.pool.Exec(ctx, "TRUNCATE TABLE resources")
	require.NoError(t, err, "truncate")

	require.NoError(t, st.CreateProject(ctx, &v1.Project{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Project"},
		ObjectMeta: v1.ObjectMeta{Name: name},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{{Name: "web", Image: "nginx:latest"}},
		},
		Status: v1.ProjectStatus{Phase: v1.ProjectPhasePending},
	}))

	sub := bus.Subscribe(event.TopicProjectUpdated)
	drainEvents(sub, 100*time.Millisecond)

	return &occFixture{store: st, bus: bus, sub: sub, name: name}
}

func (f *occFixture) version(t *testing.T) int64 {
	t.Helper()
	p, err := f.store.GetProject(context.Background(), f.name)
	require.NoError(t, err)
	return p.ObjectMeta.ResourceVersion
}

func (f *occFixture) status(t *testing.T) v1.ProjectStatus {
	t.Helper()
	p, err := f.store.GetProject(context.Background(), f.name)
	require.NoError(t, err)
	return p.Status
}

// interloperOnce returns a mutation that, the first time it runs, commits an
// unrelated change out of band — deterministically losing the race that
// follows. Two goroutines cannot be scheduled into a collision on demand; this
// can, which is the difference between a test that proves the guard works and
// one that passes whether or not it does.
//
// The out-of-band write is a side effect inside a mutation, which production
// code must never do. Here it is the test playing the part of the other writer.
func (f *occFixture) interloperOnce(t *testing.T, nodeRef string, inner func(*v1.ProjectStatus) error) func(*v1.ProjectStatus) error {
	t.Helper()
	var once sync.Once
	return func(status *v1.ProjectStatus) error {
		once.Do(func() {
			require.NoError(t, f.store.UpdateProjectStatusWithRetry(context.Background(), f.name,
				func(s *v1.ProjectStatus) error {
					s.NodeRef = nodeRef
					return nil
				}))
		})
		return inner(status)
	}
}

// P1: a write carrying a version that has moved must be rejected, and the retry
// must land on top of the newer state rather than clobbering it.
func TestOCCRejectsStaleWriteAndRetriesOnFreshState(t *testing.T) {
	f := newOCCFixture(t, "occ-stale-write")
	start := f.version(t)

	err := f.store.UpdateProjectStatusWithRetry(context.Background(), f.name,
		f.interloperOnce(t, "node-interloper", func(s *v1.ProjectStatus) error {
			s.Phase = v1.ProjectPhaseRunning
			return nil
		}))
	require.NoError(t, err)

	final := f.status(t)
	assert.Equal(t, v1.ProjectPhaseRunning, final.Phase, "our own change must land")
	assert.Equal(t, "node-interloper", final.NodeRef,
		"the interloper's change must survive; if it is gone, the retry resent a stale status")
	assert.Equal(t, start+2, f.version(t), "two successful commits, two version increments")
}

// P2: the guarantee the ticket exists for. Two writers touching different
// fields must both end up in the stored status.
func TestOCCConcurrentWritersPreserveEachOthersFields(t *testing.T) {
	f := newOCCFixture(t, "occ-preserve-fields")
	start := f.version(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make([]error, 2)

	wg.Add(2)
	go func() {
		defer wg.Done()
		errs[0] = f.store.UpdateProjectStatusWithRetry(ctx, f.name, func(s *v1.ProjectStatus) error {
			s.NodeRef = "node-a"
			return nil
		})
	}()
	go func() {
		defer wg.Done()
		errs[1] = f.store.UpdateProjectStatusWithRetry(ctx, f.name, func(s *v1.ProjectStatus) error {
			for i := range s.Conditions {
				if s.Conditions[i].Type == v1.ConditionTypePhase {
					s.Conditions[i].Reason = "ContainersReady"
					return nil
				}
			}
			s.Conditions = append(s.Conditions, v1.Condition{
				Type:   v1.ConditionTypePhase,
				Status: v1.ConditionTrue,
				Reason: "ContainersReady",
			})
			return nil
		})
	}()
	wg.Wait()

	require.NoError(t, errs[0], "writer A")
	require.NoError(t, errs[1], "writer B")

	final := f.status(t)
	assert.Equal(t, "node-a", final.NodeRef, "writer A's field was lost")
	require.Len(t, final.Conditions, 1, "writer B's condition was lost")
	assert.Equal(t, "ContainersReady", final.Conditions[0].Reason)
	assert.Equal(t, start+2, f.version(t))
}

// P3: when both writers decide the same field, one of them simply wins. This is
// not a statement about which writer has authority — see ADR-0016; optimistic
// concurrency has nothing to say about it.
func TestOCCSameFieldKeepsLastSuccessfulCommit(t *testing.T) {
	f := newOCCFixture(t, "occ-same-field")
	start := f.version(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make([]error, 2)

	wg.Add(2)
	for i, phase := range []v1.ProjectPhase{v1.ProjectPhaseRunning, v1.ProjectPhaseFailed} {
		go func(idx int, p v1.ProjectPhase) {
			defer wg.Done()
			errs[idx] = f.store.UpdateProjectStatusWithRetry(ctx, f.name, func(s *v1.ProjectStatus) error {
				s.Phase = p
				return nil
			})
		}(i, phase)
	}
	wg.Wait()

	require.NoError(t, errs[0])
	require.NoError(t, errs[1])

	final := f.status(t)
	assert.Contains(t, []v1.ProjectPhase{v1.ProjectPhaseRunning, v1.ProjectPhaseFailed}, final.Phase,
		"the final phase must be one of the two written, not a mixture")
	assert.Equal(t, start+2, f.version(t))
}

// P4: a Project deleted between attempts is not recreated, and the retry stops.
func TestOCCStopsWhenProjectIsDeletedMidRetry(t *testing.T) {
	f := newOCCFixture(t, "occ-deleted-mid-retry")
	ctx := context.Background()

	var once sync.Once
	err := f.store.UpdateProjectStatusWithRetry(ctx, f.name, func(s *v1.ProjectStatus) error {
		once.Do(func() {
			// Move the version, then remove the row: the CAS misses, and the
			// re-read that classifies the miss finds nothing.
			require.NoError(t, f.store.UpdateProjectStatusWithRetry(ctx, f.name,
				func(inner *v1.ProjectStatus) error {
					inner.NodeRef = "doomed"
					return nil
				}))
			require.NoError(t, f.store.DeleteProject(ctx, f.name))
		})
		s.Phase = v1.ProjectPhaseRunning
		return nil
	})

	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrNotFound), "err = %v, want ErrNotFound", err)
	assert.False(t, errors.Is(err, store.ErrVersionConflict),
		"a deleted Project must not be reported as a version conflict")

	_, getErr := f.store.GetProject(ctx, f.name)
	assert.True(t, errors.Is(getErr, store.ErrNotFound), "the Project must not have been recreated")
}

// P5: an event means "the stored status changed", nothing weaker. The agent
// re-reports the same status on every tick from every node; if that published,
// subscribers would wake constantly to discover nothing had happened.
func TestOCCEventSemantics(t *testing.T) {
	f := newOCCFixture(t, "occ-events")
	ctx := context.Background()

	set := func(mutate func(*v1.ProjectStatus) error) error {
		return f.store.UpdateProjectStatusWithRetry(ctx, f.name, mutate)
	}

	t.Run("phase change publishes once", func(t *testing.T) {
		require.NoError(t, set(func(s *v1.ProjectStatus) error {
			s.Phase = v1.ProjectPhaseRunning
			return nil
		}))
		assert.Len(t, collectEvents(f.sub, 200*time.Millisecond), 1)
	})

	t.Run("identical reassertion publishes nothing", func(t *testing.T) {
		require.NoError(t, set(func(s *v1.ProjectStatus) error {
			s.Phase = v1.ProjectPhaseRunning
			return nil
		}))
		assert.Empty(t, collectEvents(f.sub, 200*time.Millisecond),
			"re-reporting an unchanged status must not publish")
	})

	t.Run("nodeRef change publishes once", func(t *testing.T) {
		require.NoError(t, set(func(s *v1.ProjectStatus) error {
			s.NodeRef = "node-a"
			return nil
		}))
		assert.Len(t, collectEvents(f.sub, 200*time.Millisecond), 1,
			"a change outside phase is still a change")
	})

	t.Run("condition change publishes once", func(t *testing.T) {
		require.NoError(t, set(func(s *v1.ProjectStatus) error {
			s.Conditions = append(s.Conditions, v1.Condition{
				Type: v1.ConditionTypeMaintenance, Status: v1.ConditionTrue,
			})
			return nil
		}))
		assert.Len(t, collectEvents(f.sub, 200*time.Millisecond), 1)
	})

	t.Run("mutation error publishes nothing", func(t *testing.T) {
		sentinel := errors.New("refused")
		err := set(func(*v1.ProjectStatus) error { return sentinel })
		require.ErrorIs(t, err, sentinel)
		assert.Empty(t, collectEvents(f.sub, 200*time.Millisecond))
	})

	t.Run("conflict then success publishes once in total", func(t *testing.T) {
		before := f.version(t)
		require.NoError(t, f.store.UpdateProjectStatusWithRetry(ctx, f.name,
			f.interloperOnce(t, "node-b", func(s *v1.ProjectStatus) error {
				s.Phase = v1.ProjectPhaseFailed
				return nil
			})))

		// Two commits happened — the interloper's and ours — so two events. The
		// point is that the failed attempt in between added none.
		assert.Len(t, collectEvents(f.sub, 300*time.Millisecond), 2)
		assert.Equal(t, before+2, f.version(t))
	})
}

// Regression guard for the condition helpers. They already behave correctly;
// this pins the behaviour so a later refactor cannot quietly reintroduce an
// event on every no-op patch.
func TestConditionPatchNoOpPublishesNothing(t *testing.T) {
	f := newOCCFixture(t, "occ-condition-noop")
	ctx := context.Background()

	cond := v1.Condition{Type: v1.ConditionTypeMaintenance, Status: v1.ConditionTrue, Reason: "BackingUp"}

	require.NoError(t, f.store.PatchProjectCondition(ctx, f.name, cond))
	assert.Len(t, collectEvents(f.sub, 200*time.Millisecond), 1, "a real patch publishes once")
	afterFirst := f.version(t)

	require.NoError(t, f.store.PatchProjectCondition(ctx, f.name, cond))
	assert.Empty(t, collectEvents(f.sub, 200*time.Millisecond), "an identical patch must not publish")
	assert.Equal(t, afterFirst, f.version(t), "an identical patch must not advance the version")

	require.NoError(t, f.store.ClearProjectCondition(ctx, f.name, v1.ConditionTypeMaintenance))
	assert.Len(t, collectEvents(f.sub, 200*time.Millisecond), 1, "a real clear publishes once")
	afterClear := f.version(t)

	require.NoError(t, f.store.ClearProjectCondition(ctx, f.name, v1.ConditionTypeMaintenance))
	assert.Empty(t, collectEvents(f.sub, 200*time.Millisecond), "clearing an absent condition must not publish")
	assert.Equal(t, afterClear, f.version(t))
}

// The legacy method keeps its no-op guard too: it is still reachable from test
// fixtures, and an event storm from it would be just as misleading.
func TestLegacyUpdateProjectStatusNoOpPublishesNothing(t *testing.T) {
	f := newOCCFixture(t, "occ-legacy-noop")
	ctx := context.Background()

	status := f.status(t)
	status.Phase = v1.ProjectPhaseRunning
	require.NoError(t, f.store.UpdateProjectStatus(ctx, f.name, status))
	assert.Len(t, collectEvents(f.sub, 200*time.Millisecond), 1)
	after := f.version(t)

	require.NoError(t, f.store.UpdateProjectStatus(ctx, f.name, status))
	assert.Empty(t, collectEvents(f.sub, 200*time.Millisecond))
	assert.Equal(t, after, f.version(t))

	require.True(t, errors.Is(
		f.store.UpdateProjectStatus(ctx, "no-such-project", status), store.ErrNotFound),
		"a missing Project must still be reported as not found, not as a no-op")
}
