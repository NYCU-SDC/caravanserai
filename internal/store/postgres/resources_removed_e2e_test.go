//go:build e2e

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/event"
	"NYCU-SDC/caravanserai/internal/store"
	pgstore "NYCU-SDC/caravanserai/internal/store/postgres"
	"NYCU-SDC/caravanserai/test/integration/testhelper"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func newE2EStore(t *testing.T) (*pgstore.Store, *event.Bus) {
	t.Helper()
	pool, databaseURL, cleanup, err := testhelper.StartPostgres()
	require.NoError(t, err)
	pool.Close()
	t.Cleanup(cleanup)

	bus := event.New(zap.NewNop(), 64)
	s, err := pgstore.New(context.Background(), databaseURL, zap.NewNop(), bus)
	require.NoError(t, err)
	// Registered after the container cleanup, so it runs first: the pool
	// closes before the database it points at is removed.
	t.Cleanup(s.Close)
	return s, bus
}

// drain empties ch and returns how many events it held.
func drain(ch event.Handler) int {
	n := 0
	for {
		select {
		case <-ch:
			n++
		default:
			return n
		}
	}
}

func createRunning(t *testing.T, s *pgstore.Store, name, node string, generation int64) *v1.Project {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.CreateProject(ctx, &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: name},
		Spec:       v1.ProjectSpec{Services: []v1.ServiceDef{{Name: "web", Image: "nginx:alpine"}}},
	}))
	require.NoError(t, s.UpdateProjectStatusWithRetry(ctx, name, func(st *v1.ProjectStatus) error {
		st.Phase = v1.ProjectPhaseRunning
		st.NodeRef = node
		st.AssignmentGeneration = generation
		return nil
	}))
	p, err := s.GetProject(ctx, name)
	require.NoError(t, err)
	return p
}

// bumpTo writes server-side status changes until the Project reaches version.
func bumpTo(t *testing.T, s *pgstore.Store, name string, version int64) *v1.Project {
	t.Helper()
	ctx := context.Background()
	for {
		p, err := s.GetProject(ctx, name)
		require.NoError(t, err)
		if p.ObjectMeta.ResourceVersion == version {
			return p
		}
		require.Less(t, p.ObjectMeta.ResourceVersion, version, "overshot the target version")
		require.NoError(t, s.UpdateProjectStatusWithRetry(ctx, name, func(st *v1.ProjectStatus) error {
			if st.Recovery == nil {
				st.Recovery = &v1.ProjectRecoveryStatus{}
			}
			st.Recovery.RescheduleAttempts++
			return nil
		}))
	}
}

// CARA-84: a ResourcesRemoved request that read lifetime U1 must not land on
// a same-name lifetime U2 created before its write, even when U2 has reached
// the very version U1 had when it was read. A version-only swap would write
// U1's whole status — nodeRef, generation, evidence — over U2.
func TestResourcesRemovedCannotLandOnRecreatedSameNameProject(t *testing.T) {
	s, bus := newE2EStore(t)
	ctx := context.Background()
	const name = "race"

	u1 := createRunning(t, s, name, "node-a", 3)
	u1 = bumpTo(t, s, name, u1.ObjectMeta.ResourceVersion+2)
	owner := store.AssignmentRef{UID: u1.ObjectMeta.UID, NodeRef: "node-a", Generation: 3}

	events := bus.Subscribe(event.TopicProjectUpdated)
	var u2 *v1.Project
	hook := func() {
		require.NoError(t, s.DeleteProject(ctx, name))
		createRunning(t, s, name, "node-b", 1)
		u2 = bumpTo(t, s, name, u1.ObjectMeta.ResourceVersion)
		require.NotEqual(t, u1.ObjectMeta.UID, u2.ObjectMeta.UID)
		drain(events)
	}

	err := pgstore.RecordResourcesRemovedWithHook(s, ctx, name, owner, time.Now(), hook)
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrStaleAssignment), "got %v, want ErrStaleAssignment", err)

	after, getErr := s.GetProject(ctx, name)
	require.NoError(t, getErr)
	assert.Equal(t, u2.ObjectMeta.UID, after.ObjectMeta.UID)
	assert.Equal(t, u2.ObjectMeta.ResourceVersion, after.ObjectMeta.ResourceVersion, "the stale request wrote to the new lifetime")
	assert.Equal(t, "node-b", after.Status.NodeRef)
	assert.Equal(t, int64(1), after.Status.AssignmentGeneration)
	assert.Nil(t, after.Status.Recovery.ResourcesRemoved)
	assert.Zero(t, drain(events), "a refused write published an event")
}

// Without a race the same request succeeds and publishes exactly one event;
// reasserting it writes nothing and publishes nothing.
func TestResourcesRemovedEventsOnlyOnWrite(t *testing.T) {
	s, bus := newE2EStore(t)
	ctx := context.Background()
	const name = "events"

	p := createRunning(t, s, name, "node-a", 2)
	owner := store.AssignmentRef{UID: p.ObjectMeta.UID, NodeRef: "node-a", Generation: 2}
	events := bus.Subscribe(event.TopicProjectUpdated)

	require.NoError(t, s.RecordResourcesRemoved(ctx, name, owner, time.Now()))
	assert.Equal(t, 1, drain(events))

	require.NoError(t, s.RecordResourcesRemoved(ctx, name, owner, time.Now()))
	assert.Zero(t, drain(events), "reasserting identical evidence published an event")

	stale := store.AssignmentRef{UID: p.ObjectMeta.UID, NodeRef: "node-a", Generation: 1}
	assert.True(t, errors.Is(s.RecordResourcesRemoved(ctx, name, stale, time.Now()), store.ErrStaleAssignment))
	assert.Zero(t, drain(events), "a refused write published an event")
}
