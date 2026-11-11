package docker

import (
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CARA-92: a Failed Project is stopped and checked by what Docker holds for it,
// not by the services its spec names.

func stopped(f *recoverFixture) []string {
	var ids []string
	for _, m := range f.docker.mutations {
		if len(m) > len("ContainerStop:") && m[:len("ContainerStop:")] == "ContainerStop:" {
			ids = append(ids, m[len("ContainerStop:"):])
		}
	}
	return ids
}

// A service the spec has dropped still has a container of this assignment on
// the node. A walk over the spec cannot see it; StopAssignment stops it too.
func TestStopAssignmentStopsAServiceTheSpecNoLongerDeclares(t *testing.T) {
	f := newStrictFixture(t)
	f.container("db", "running")
	f.container("worker", "running") // this generation's, but not in the spec

	require.NoError(t, f.runtime.StopAssignment(t.Context(), f.project))

	assert.ElementsMatch(t, []string{fakeID("blog-db"), fakeID("blog-worker")}, stopped(f))
}

// A leftover of another assignment is not this one's to stop, and does not
// fail the stop of the containers that are, which StopProject would.
func TestStopAssignmentLeavesAnotherAssignmentsContainer(t *testing.T) {
	f := newStrictFixture(t)
	f.project.Spec.Services = append(f.project.Spec.Services, v1.ServiceDef{Name: "web", Image: "nginx"})
	f.container("web", "running")
	f.staleContainer("db", "exited", labelGeneration, "6")

	require.ErrorIs(t, f.runtime.StopProject(t.Context(), f.project), ErrContainerNotOwned,
		"the per-service stop refuses the whole Project")
	require.NoError(t, f.runtime.StopAssignment(t.Context(), f.project))

	assert.Equal(t, []string{fakeID("blog-web")}, stopped(f), "only this assignment's container is stopped")
}

// Reverse spec order, as StopProject, then what the spec no longer declares.
func TestStopAssignmentOrder(t *testing.T) {
	project := &v1.Project{Spec: v1.ProjectSpec{Services: []v1.ServiceDef{{Name: "db"}, {Name: "api"}, {Name: "web"}}}}
	listed := []ProjectContainer{
		{Name: "blog-api", Service: "api"},
		{Name: "blog-old", Service: "old"},
		{Name: "blog-db", Service: "db"},
		{Name: "blog-stale", Service: "web", NotOwned: ErrContainerNotOwned},
		{Name: "blog-web", Service: "web"},
	}

	var names []string
	for _, c := range stopOrder(project, listed) {
		names = append(names, c.Name)
	}
	assert.Equal(t, []string{"blog-web", "blog-api", "blog-db", "blog-old"}, names)
}

func TestProjectContainersClassifiesEveryLabelledContainer(t *testing.T) {
	f := newStrictFixture(t)
	f.container("db", "exited")
	f.container("worker", "running")
	f.staleContainer("cache", "running", labelUID, "uid-previous-lifetime")
	f.docker.withContainer("other-web", "running", map[string]string{
		labelProject: "other", labelService: "web", labelNamespace: "default",
		labelUID: "uid-9", labelGeneration: "1",
	})

	listed, err := f.runtime.ProjectContainers(t.Context(), f.project)

	require.NoError(t, err)
	require.Len(t, listed, 3, "another Project's container is not listed")
	byName := map[string]ProjectContainer{}
	for _, c := range listed {
		byName[c.Name] = c
	}
	assert.NoError(t, byName["blog-db"].NotOwned)
	assert.True(t, byName["blog-db"].Stopped())
	assert.NoError(t, byName["blog-worker"].NotOwned)
	assert.False(t, byName["blog-worker"].Stopped())
	assert.ErrorIs(t, byName["blog-cache"].NotOwned, ErrContainerNotOwned)
	assert.Empty(t, f.docker.mutations, "listing is read-only")
}

func TestProjectContainerStopped(t *testing.T) {
	for state, want := range map[string]bool{
		"exited": true, "created": true, "dead": true,
		"running": false, "restarting": false, "paused": false, "removing": false, "": false,
	} {
		assert.Equal(t, want, ProjectContainer{State: state}.Stopped(), state)
	}
}

// RestartContainer must not start what is stopped: a node that reported a
// Project stopped must not see a settings change bring it back (CARA-92).
func TestRestartContainerNeverStartsAStoppedContainer(t *testing.T) {
	f := newStrictFixture(t)
	f.container("db", "exited")

	err := f.runtime.RestartContainer(t.Context(), ContainerName("blog", "db"))

	require.ErrorIs(t, err, ErrContainerNotRunning)
	assert.Empty(t, f.docker.mutations, "neither stopped nor started")
}

func TestRestartContainerRestartsARunningContainer(t *testing.T) {
	f := newStrictFixture(t)
	f.container("db", "running")

	require.NoError(t, f.runtime.RestartContainer(t.Context(), ContainerName("blog", "db")))

	assert.Equal(t, []string{"ContainerStop:blog-db", "ContainerStart:blog-db"}, f.docker.mutations)
}
