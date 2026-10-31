package docker

import (
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTwoServiceFixture is a Project of two services, db then web.
func newTwoServiceFixture(t *testing.T) *recoverFixture {
	t.Helper()
	f := newRecoverFixture(t, v1.VolumeTypeManaged)
	f.project.Spec.Services = append(f.project.Spec.Services, v1.ServiceDef{Name: "web", Image: "nginx:alpine"})
	return f
}

func TestStopAndStartProjectActOnTheAssignmentsContainers(t *testing.T) {
	f := newTwoServiceFixture(t)
	f.container("db", "running")
	f.container("web", "running")

	require.NoError(t, f.runtime.StopProject(t.Context(), f.project))
	assert.Equal(t, []string{"ContainerStop:" + fakeID("blog-web"), "ContainerStop:" + fakeID("blog-db")},
		f.docker.mutations, "stopped in reverse spec order")

	f.docker.mutations = nil
	require.NoError(t, f.runtime.StartProject(t.Context(), f.project))
	assert.Equal(t, []string{"ContainerStart:" + fakeID("blog-db"), "ContainerStart:" + fakeID("blog-web")},
		f.docker.mutations, "started in spec order")
}

func TestStopAndStartProjectTolerateAMissingContainer(t *testing.T) {
	f := newTwoServiceFixture(t)
	f.container("web", "running")

	require.NoError(t, f.runtime.StopProject(t.Context(), f.project))
	require.NoError(t, f.runtime.StartProject(t.Context(), f.project))

	assert.Equal(t, []string{"ContainerStop:" + fakeID("blog-web"), "ContainerStart:" + fakeID("blog-web")}, f.docker.mutations)
}

// The stale container is the last one checked, so anything acting as it goes
// would already have touched the first.
func TestStopAndStartProjectTouchNothingIfAnyContainerIsAnotherAssignments(t *testing.T) {
	for _, tc := range []struct{ label, value string }{
		{labelGeneration, "6"},
		{labelUID, "uid-previous-lifetime"},
		{labelProject, "another-project"},
		{labelNamespace, "another-namespace"},
		{labelService, "another-service"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			f := newTwoServiceFixture(t)
			f.container("db", "running")
			f.staleContainer("web", "running", tc.label, tc.value)

			require.ErrorIs(t, f.runtime.StopProject(t.Context(), f.project), ErrContainerNotOwned)
			require.ErrorIs(t, f.runtime.StartProject(t.Context(), f.project), ErrContainerNotOwned)

			assert.Empty(t, f.docker.mutations)
		})
	}
}

// A container with no labels cannot be shown to be this assignment's.
func TestStopProjectRefusesAnUnlabelledContainer(t *testing.T) {
	f := newTwoServiceFixture(t)
	f.docker.withContainer(ContainerName("blog", "db"), "running", nil)

	require.ErrorIs(t, f.runtime.StopProject(t.Context(), f.project), ErrContainerNotOwned)
	assert.Empty(t, f.docker.mutations)
}

// The name can be taken by a replacement between the check and the stop, so the
// stop goes to the container that was checked.
func TestStopProjectActsOnTheContainerItChecked(t *testing.T) {
	f := newTwoServiceFixture(t)
	f.container("db", "running")

	require.NoError(t, f.runtime.StopProject(t.Context(), f.project))

	assert.Equal(t, []string{"ContainerStop:" + fakeID("blog-db")}, f.docker.mutations, "stopped by ID, not by name")
}
