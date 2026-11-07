//go:build e2e

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/adapter"
	"NYCU-SDC/caravanserai/internal/store"
	pgstore "NYCU-SDC/caravanserai/internal/store/postgres"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func allow(*v1.Project) string { return "" }

func refuse(*v1.Project) string { return "not yet" }

func createPending(t *testing.T, s *pgstore.Store, name string) *v1.Project {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.CreateProject(ctx, &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: name},
		Spec:       v1.ProjectSpec{Services: []v1.ServiceDef{{Name: "web", Image: "nginx:alpine"}}},
		Status:     v1.InitialProjectStatus(),
	}))
	p, err := s.GetProject(ctx, name)
	require.NoError(t, err)
	return p
}

func TestFinalizeProject(t *testing.T) {
	s, _ := newE2EStore(t)
	ctx := context.Background()

	t.Run("deletes the record when the predicate holds", func(t *testing.T) {
		p := createPending(t, s, "fin-allowed")

		require.NoError(t, s.FinalizeProject(ctx, p.Name, p.ObjectMeta.UID, allow))

		_, err := s.GetProject(ctx, p.Name)
		assert.ErrorIs(t, err, store.ErrNotFound)
		assert.ErrorIs(t, s.FinalizeProject(ctx, p.Name, p.ObjectMeta.UID, allow), store.ErrNotFound)
	})

	t.Run("keeps the record when the predicate refuses", func(t *testing.T) {
		p := createPending(t, s, "fin-refused")

		err := s.FinalizeProject(ctx, p.Name, p.ObjectMeta.UID, refuse)

		var blocked *store.FinalizationBlockedError
		require.ErrorAs(t, err, &blocked)
		assert.Equal(t, "not yet", blocked.Reason)
		assert.ErrorIs(t, err, store.ErrConflictState)
		_, err = s.GetProject(ctx, p.Name)
		assert.NoError(t, err, "a refused finalization deleted the record")
	})

	t.Run("does not delete another lifetime of the same name", func(t *testing.T) {
		old := createPending(t, s, "fin-recreated")
		require.NoError(t, s.DeleteProject(ctx, old.Name))
		current := createPending(t, s, old.Name)

		err := s.FinalizeProject(ctx, old.Name, old.ObjectMeta.UID, allow)

		assert.ErrorIs(t, err, store.ErrStaleAssignment)
		got, err := s.GetProject(ctx, old.Name)
		require.NoError(t, err, "the newer lifetime was deleted")
		assert.Equal(t, current.ObjectMeta.UID, got.ObjectMeta.UID)
	})

	t.Run("the predicate sees the row as it is when locked", func(t *testing.T) {
		p := createPending(t, s, "fin-sees-latest")
		require.NoError(t, s.UpdateProjectStatusWithRetry(ctx, p.Name, func(st *v1.ProjectStatus) error {
			st.NodeRef = "node-late"
			return nil
		}))

		var seen string
		require.NoError(t, s.FinalizeProject(ctx, p.Name, p.ObjectMeta.UID, func(locked *v1.Project) string {
			seen = locked.Status.NodeRef
			return ""
		}))
		assert.Equal(t, "node-late", seen)
	})
}

// A write that reaches the row while finalization holds it waits, and then
// finds the row gone. It can neither land between the check and the delete nor
// be silently applied to a record that no longer exists.
func TestFinalizeProjectHoldsTheRowUntilTheDelete(t *testing.T) {
	s, _ := newE2EStore(t)
	ctx := context.Background()
	p := createPending(t, s, "fin-locked")

	writeDone := make(chan error, 1)
	err := pgstore.FinalizeProjectWithAttach(s, ctx, p.Name, p.ObjectMeta.UID, allow,
		func(ctx context.Context, _ pgx.Tx) error {
			// The predicate has passed and the row is locked. A status write
			// started now must not complete until this transaction ends.
			go func() {
				writeDone <- s.UpdateProjectStatusWithRetry(context.Background(), p.Name, func(st *v1.ProjectStatus) error {
					st.NodeRef = "node-racer"
					return nil
				})
			}()
			select {
			case err := <-writeDone:
				t.Errorf("a status write completed while finalization held the row: %v", err)
			case <-time.After(500 * time.Millisecond):
			}
			return nil
		})
	require.NoError(t, err)

	select {
	case err := <-writeDone:
		assert.ErrorIs(t, err, store.ErrNotFound, "the blocked write should find the record finalized")
	case <-time.After(10 * time.Second):
		t.Fatal("the status write never returned after finalization committed")
	}
	_, err = s.GetProject(ctx, p.Name)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// Writes attached to the finalization transaction commit with the delete, and
// roll back with it.
func TestFinalizeProjectAttachedWrites(t *testing.T) {
	s, _ := newE2EStore(t)
	ctx := context.Background()
	require.NoError(t, s.CreateNode(ctx, &v1.Node{
		ObjectMeta: v1.ObjectMeta{Name: "witness"},
		Spec:       v1.NodeSpec{Hostname: "witness"},
	}))

	mark := func(value string) func(context.Context, pgx.Tx) error {
		return func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`UPDATE resources SET labels = jsonb_build_object('finalized', $1::text) WHERE kind = 'Node' AND name = 'witness'`,
				value)
			return err
		}
	}
	witness := func() string {
		n, err := s.GetNode(ctx, "witness")
		require.NoError(t, err)
		return n.Labels["finalized"]
	}

	t.Run("commits with the delete", func(t *testing.T) {
		p := createPending(t, s, "fin-attach-commit")

		require.NoError(t, pgstore.FinalizeProjectWithAttach(s, ctx, p.Name, p.ObjectMeta.UID, allow, mark(p.Name)))

		assert.Equal(t, p.Name, witness())
		_, err := s.GetProject(ctx, p.Name)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("rolls back with it", func(t *testing.T) {
		p := createPending(t, s, "fin-attach-rollback")
		failing := errors.New("attached write failed")

		err := pgstore.FinalizeProjectWithAttach(s, ctx, p.Name, p.ObjectMeta.UID, allow,
			func(ctx context.Context, tx pgx.Tx) error {
				if err := mark(p.Name)(ctx, tx); err != nil {
					return err
				}
				return failing
			})

		require.ErrorIs(t, err, failing)
		assert.Equal(t, "fin-attach-commit", witness(), "the attached write survived a failed finalization")
		_, err = s.GetProject(ctx, p.Name)
		assert.NoError(t, err, "the record was deleted although the finalization failed")
	})
}

// The API reads a Project, finds it never assigned and decides to delete it
// directly. If the Scheduler commits the first assignment before the delete,
// the store must refuse: the record now represents containers about to start.
func TestImmediateDeletionRacesFirstAssignment(t *testing.T) {
	s, _ := newE2EStore(t)
	ctx := context.Background()
	p := createPending(t, s, "fin-first-assignment")
	require.Empty(t, v1.ImmediateDeletionBlocker(p), "the API's read must nominate it for direct deletion")

	// The first assignment commits between the API's read and its delete.
	require.NoError(t, adapter.NewProjectStoreAdapter(s).SetProjectScheduled(ctx, p.Name, "node-a"))

	err := s.FinalizeProject(ctx, p.Name, p.ObjectMeta.UID, v1.ImmediateDeletionBlocker)

	assert.ErrorIs(t, err, store.ErrConflictState)
	got, err := s.GetProject(ctx, p.Name)
	require.NoError(t, err, "the record of a just-assigned Project was deleted")
	assert.Equal(t, v1.ProjectPhaseScheduled, got.Status.Phase)
	assert.Equal(t, "node-a", got.Status.NodeRef)
}
