package docker

import (
	"errors"
	"strings"
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

// ── Containers of services the spec no longer declares ───────────────────────

// The case a per-service walk cannot see: generation 6 declared "worker",
// generation 7's spec does not, and the worker container is still on the node.
// Asking Docker for everything labelled for the Project is what finds it.
func TestStaleContainersFindsAServiceTheSpecNoLongerDeclares(t *testing.T) {
	f := newStrictFixture(t)
	f.container("db", "running")                                // generation 7's own
	f.staleContainer("worker", "running", labelGeneration, "6") // dropped from the spec

	stale, err := f.runtime.StaleContainers(t.Context(), f.project)

	require.NoError(t, err)
	require.Len(t, stale, 1, "the dropped service's container must be found")
	assert.Equal(t, "blog-worker", stale[0].Name)
	assert.Equal(t, "worker", stale[0].Service)
	assert.ErrorIs(t, stale[0].Reason, ErrContainerNotOwned)
	assert.Empty(t, f.docker.mutations, "listing is read-only")
}

// A container from a previous lifetime counts too, whatever its generation.
func TestStaleContainersFindsAPreviousLifetime(t *testing.T) {
	f := newStrictFixture(t)
	f.staleContainer("db", "exited", labelUID, "uid-previous-lifetime")

	stale, err := f.runtime.StaleContainers(t.Context(), f.project)

	require.NoError(t, err)
	require.Len(t, stale, 1)
	assert.Equal(t, "blog-db", stale[0].Name)
}

func TestStaleContainersIsEmptyWhenEveryContainerIsOurs(t *testing.T) {
	f := newStrictFixture(t)
	f.container("db", "running")

	stale, err := f.runtime.StaleContainers(t.Context(), f.project)

	require.NoError(t, err)
	assert.Empty(t, stale)
}

// Compatibility mode does not compare UID or generation, so an earlier
// generation's container is not stale there — the same contract the rest of
// the ownership rule keeps.
func TestStaleContainersIgnoresGenerationInCompatibilityMode(t *testing.T) {
	f := newRecoverFixture(t, v1.VolumeTypeManaged)
	f.staleContainer("worker", "running", labelGeneration, "6")

	stale, err := f.runtime.StaleContainers(t.Context(), f.project)

	require.NoError(t, err)
	assert.Empty(t, stale)
}

// Another Project's containers are never this Project's problem.
func TestStaleContainersIgnoresOtherProjects(t *testing.T) {
	f := newStrictFixture(t)
	f.docker.withContainer("other-web", "running", map[string]string{
		labelProject: "other", labelService: "web", labelNamespace: "default",
		labelUID: "uid-9", labelGeneration: "1",
	})

	stale, err := f.runtime.StaleContainers(t.Context(), f.project)

	require.NoError(t, err)
	assert.Empty(t, stale)
}

// ── The refusal must not take shared resources with it ───────────────────────

func mutated(f *recoverFixture, prefix string) []string {
	var out []string
	for _, m := range f.docker.mutations {
		if strings.HasPrefix(m, prefix) {
			out = append(out, m)
		}
	}
	return out
}

// The real TOCTOU: step 0 passes, and a container becomes another
// assignment's before ensureContainer reaches it. The refusal must undo only
// what this attempt created. The network and the volumes carry no generation
// label, so they may be exactly what the container we just refused is using —
// removing them would destroy another assignment's runtime, and an Ephemeral
// volume outright.
func TestReconcileRefusalLeavesSharedResources(t *testing.T) {
	f := newStrictFixture(t)
	f.project.Spec.Services = append(f.project.Spec.Services, v1.ServiceDef{Name: "cache", Image: "redis:7"})
	f.docker.volumes[VolumeName("blog", "db-data")] = true

	// "blog-cache" appears, owned by generation 6, only once step 0 is past:
	// the first inspect of that name is ensureContainer's.
	f.docker.onInspect = func(name string) {
		if name == ContainerName("blog", "cache") && len(f.docker.containers) == 1 {
			f.staleContainer("cache", "running", labelGeneration, "6")
		}
	}

	err := f.runtime.ReconcileProject(t.Context(), f.project)

	require.ErrorIs(t, err, ErrContainerNotOwned)
	assert.True(t, removed(f, "blog-db"), "the container this attempt created is undone")
	assert.Contains(t, f.docker.containers, ContainerName("blog", "cache"), "the stale container survives")
	assert.False(t, removed(f, "blog-cache"), "and is never removed")
	assert.Empty(t, mutated(f, "NetworkRemove"), "the network may be the stale container's too")
	assert.Empty(t, mutated(f, "VolumeRemove"), "an Ephemeral volume removed here would be gone for good")
	assert.True(t, f.docker.volumes[VolumeName("blog", "db-data")], "the volume still exists")
}

// A refusal on the first service creates nothing, so there is nothing to undo.
func TestReconcileRefusalOnTheFirstServiceRemovesNothing(t *testing.T) {
	f := newStrictFixture(t)
	f.docker.onInspect = func(name string) {
		if name == ContainerName("blog", "db") && len(f.docker.containers) == 0 {
			f.staleContainer("db", "running", labelUID, "uid-previous-lifetime")
		}
	}

	err := f.runtime.ReconcileProject(t.Context(), f.project)

	require.ErrorIs(t, err, ErrContainerNotOwned)
	assert.Empty(t, mutated(f, "ContainerRemove"))
	assert.Empty(t, mutated(f, "NetworkRemove"))
	assert.Empty(t, mutated(f, "VolumeRemove"))
}
