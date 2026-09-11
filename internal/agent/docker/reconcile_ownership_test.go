package docker

import (
	"errors"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CARA-93: ReconcileProject must neither adopt nor, through its rollback,
// remove a container that belongs to another assignment.

func removed(f *recoverFixture, container string) bool {
	for _, m := range f.docker.mutations {
		if m == "ContainerRemove:"+fakeID(container) {
			return true
		}
	}
	return false
}

// A stale container under a service's name is refused before anything is
// made — no network, no volume, no pull — so there is nothing to roll back.
func TestReconcileProjectRefusesAStaleContainerBeforeAnyMutation(t *testing.T) {
	f := newStrictFixture(t)
	f.staleContainer("db", "exited", labelGeneration, "6")

	err := f.runtime.ReconcileProject(t.Context(), f.project)

	require.ErrorIs(t, err, ErrContainerNotOwned)
	assert.Empty(t, f.docker.mutations, "refused before any Docker call, and not rolled back")
	assert.Contains(t, f.docker.containers, ContainerName("blog", "db"), "the stale container survives")
}

// The same holds for a stale container that is running.
func TestReconcileProjectRefusesAStaleRunningContainer(t *testing.T) {
	f := newStrictFixture(t)
	f.staleContainer("db", "running", labelUID, "uid-previous-lifetime")

	require.ErrorIs(t, f.runtime.ReconcileProject(t.Context(), f.project), ErrContainerNotOwned)
	assert.Empty(t, f.docker.mutations)
}

// A genuine failure part-way through rolls back what this assignment made, and
// only that. Generation 6 left a container for a service the spec has since
// dropped, so the step-0 check never sees it; the rollback must still leave it.
func TestReconcileRollbackLeavesAnotherAssignmentsContainer(t *testing.T) {
	f := newStrictFixture(t)
	f.project.Spec.Services = append(f.project.Spec.Services, v1.ServiceDef{Name: "cache", Image: "broken:image"})
	f.docker.pullErr = map[string]error{"broken:image": errors.New("manifest unknown")}
	f.staleContainer("worker", "exited", labelGeneration, "6")

	err := f.runtime.ReconcileProject(t.Context(), f.project)

	require.Error(t, err)
	assert.ErrorContains(t, err, "manifest unknown")
	assert.True(t, removed(f, "blog-db"), "the container this attempt created is rolled back")
	assert.False(t, removed(f, "blog-worker"), "generation 6's container is not this rollback's to remove")
	assert.Contains(t, f.docker.containers, ContainerName("blog", "worker"))
}

// Deleting a Project is broader than a rollback: every container labelled for
// it goes, whichever generation left it. RemoveProject keeps that.
func TestRemoveProjectStillRemovesEarlierGenerations(t *testing.T) {
	f := newStrictFixture(t)
	f.container("db", "running")
	f.staleContainer("worker", "exited", labelGeneration, "6")

	require.NoError(t, f.runtime.RemoveProject(t.Context(), f.project.Namespace, f.project.Name, f.project.Spec))

	assert.True(t, removed(f, "blog-db"))
	assert.True(t, removed(f, "blog-worker"), "a deleted Project takes its earlier generations' containers with it")
}

// Compatibility mode keeps its contract: a container without UID and
// generation labels is adopted, as it was before these checks existed.
func TestReconcileProjectCompatibilityModeAdoptsALegacyContainer(t *testing.T) {
	f := newRecoverFixture(t, v1.VolumeTypeManaged)
	f.provisionManaged(t)
	f.docker.withContainer(ContainerName("blog", "db"), "exited", map[string]string{
		labelProject: "blog",
		labelService: "db",
	})

	require.NoError(t, f.runtime.ReconcileProject(t.Context(), f.project))
	assert.Contains(t, f.docker.mutations, "ContainerStart:"+fakeID("blog-db"))
}
