package docker

import (
	"errors"
	"strings"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func hashSpec() (v1.ServiceDef, v1.ProjectSpec) {
	svc := v1.ServiceDef{
		Name:  "web",
		Image: "nginx:1.27",
		Env: []v1.EnvVar{
			{Name: "MODE", Value: "prod"},
			{Name: "DB_PASSWORD", ValueFrom: &v1.EnvVarSource{SecretKeyRef: &v1.SecretKeySelector{Name: "db", Key: "password"}}},
		},
		VolumeMounts: []v1.VolumeMount{{Name: "data", MountPath: "/data"}},
	}
	return svc, v1.ProjectSpec{Services: []v1.ServiceDef{svc}, Volumes: []v1.VolumeDef{{Name: "data", Type: v1.VolumeTypeManaged}}}
}

// The hash is part of every container a Node runs: changing its input or its
// encoding recreates every container once. This pins it, so such a change is
// a decision someone makes, not a side effect.
func TestServiceSpecHashIsStable(t *testing.T) {
	svc, spec := hashSpec()
	assert.Equal(t, "ed815958ead53119711c3d1d2d84c40d07ed407033fdb6a6c5c379a07cfc4a58", ServiceSpecHash(svc, spec))
}

func TestServiceSpecHashNamesSecretsByReferenceOnly(t *testing.T) {
	svc, spec := hashSpec()
	before := ServiceSpecHash(svc, spec)

	// What the Agent's resolved copy looks like: the value fetched, the
	// reference kept.
	svc.Env[1].Value = "hunter2"
	assert.Equal(t, before, ServiceSpecHash(svc, spec), "the resolved value is not part of the hash")

	svc.Env[1].Value = "correct-horse"
	assert.Equal(t, before, ServiceSpecHash(svc, spec), "a rotated Secret recreates nothing on its own")

	svc.Env[1].ValueFrom.SecretKeyRef.Key = "other"
	assert.NotEqual(t, before, ServiceSpecHash(svc, spec), "pointing at another key is a change")
}

func TestServiceSpecHashFollowsWhatTheContainerIsBuiltFrom(t *testing.T) {
	svc, spec := hashSpec()
	base := ServiceSpecHash(svc, spec)
	for name, edit := range map[string]func(*v1.ServiceDef, *v1.ProjectSpec){
		"image":       func(s *v1.ServiceDef, _ *v1.ProjectSpec) { s.Image = "nginx:1.28" },
		"command":     func(s *v1.ServiceDef, _ *v1.ProjectSpec) { s.Command = []string{"nginx", "-g", "daemon off;"} },
		"env value":   func(s *v1.ServiceDef, _ *v1.ProjectSpec) { s.Env[0].Value = "dev" },
		"mount path":  func(s *v1.ServiceDef, _ *v1.ProjectSpec) { s.VolumeMounts[0].MountPath = "/srv" },
		"volume type": func(_ *v1.ServiceDef, p *v1.ProjectSpec) { p.Volumes[0].Type = v1.VolumeTypeEphemeral },
	} {
		t.Run(name, func(t *testing.T) {
			s, p := hashSpec()
			edit(&s, &p)
			assert.NotEqual(t, base, ServiceSpecHash(s, p))
		})
	}

	t.Run("other services and ingress are not part of it", func(t *testing.T) {
		s, p := hashSpec()
		p.Services = append(p.Services, v1.ServiceDef{Name: "api", Image: "api:1"})
		p.Ingress = []v1.IngressDef{{Name: "web"}}
		p.Volumes = append(p.Volumes, v1.VolumeDef{Name: "unused", Type: v1.VolumeTypeEphemeral})
		assert.Equal(t, base, ServiceSpecHash(s, p))
	})

	t.Run("an optional field left unset is not part of it", func(t *testing.T) {
		s, p := hashSpec()
		s.Command = []string{}
		assert.Equal(t, base, ServiceSpecHash(s, p), "empty encodes like absent")
	})
}

func TestSpecDrift(t *testing.T) {
	t.Run("a container built from the spec is up to date", func(t *testing.T) {
		f := newStrictFixture(t)
		f.container("db", "running")
		drift, err := f.runtime.SpecDrift(t.Context(), f.project)
		require.NoError(t, err)
		assert.Equal(t, SpecDrift{}, drift)
	})

	t.Run("another image, no label, a missing service and a removed one", func(t *testing.T) {
		f := newStrictFixture(t)
		f.project.Spec.Services = append(f.project.Spec.Services,
			v1.ServiceDef{Name: "cache", Image: "redis:7"}, v1.ServiceDef{Name: "web", Image: "nginx:1"})
		f.container("db", "running")
		f.project.Spec.Services[0].Image = "postgres:17"
		noLabel := f.ownedLabels("cache")
		delete(noLabel, labelSpecHash)
		f.docker.withContainer(ContainerName("blog", "cache"), "running", noLabel)
		f.docker.withContainer(ContainerName("blog", "worker"), "running", f.ownedLabels("worker"))
		// Another assignment's container under a removed service's name is not
		// this assignment's to remove.
		stale := f.ownedLabels("old")
		stale[labelGeneration] = "6"
		f.docker.withContainer(ContainerName("blog", "old"), "exited", stale)

		drift, err := f.runtime.SpecDrift(t.Context(), f.project)
		require.NoError(t, err)
		assert.Equal(t, []string{"db", "cache"}, drift.Outdated)
		assert.Equal(t, []string{"web"}, drift.Missing)
		require.Len(t, drift.Removed, 1)
		assert.Equal(t, "worker", drift.Removed[0].Service)
		assert.Empty(t, f.docker.mutations, "comparing changes nothing")
	})

	t.Run("another assignment's container under a service's name", func(t *testing.T) {
		f := newStrictFixture(t)
		f.staleContainer("db", "running", labelGeneration, "6")
		_, err := f.runtime.SpecDrift(t.Context(), f.project)
		assert.ErrorIs(t, err, ErrContainerNotOwned)
	})
}

func TestUpdateServicesPullsBeforeItStopsAnything(t *testing.T) {
	f := newStrictFixture(t)
	f.project.Spec.Services = append(f.project.Spec.Services, v1.ServiceDef{Name: "web", Image: "nginx:1.27"})
	f.container("db", "running")
	f.container("web", "running")
	f.docker.withContainer(ContainerName("blog", "worker"), "running", f.ownedLabels("worker"))
	f.project.Spec.Services[1].Image = "nginx:1.28"
	drift, err := f.runtime.SpecDrift(t.Context(), f.project)
	require.NoError(t, err)

	require.NoError(t, f.runtime.UpdateServices(t.Context(), f.project, drift))

	webID := fakeID(ContainerName("blog", "web"))
	assert.Equal(t, []string{
		"ImagePull:nginx:1.28",
		"ContainerStop:" + webID,
		"ContainerRemove:" + webID,
		"ContainerStop:" + fakeID(ContainerName("blog", "worker")),
		"ContainerRemove:" + fakeID(ContainerName("blog", "worker")),
		"ContainerCreate:" + ContainerName("blog", "web"),
		"ContainerStart:" + webID,
	}, withoutSetup(f.docker.mutations))
	_, dbStillThere := f.docker.containers[ContainerName("blog", "db")]
	assert.True(t, dbStillThere, "a service that did not change keeps its container")
	assert.Equal(t, ServiceSpecHash(f.project.Spec.Services[1], f.project.Spec),
		f.docker.containers[ContainerName("blog", "web")].Config.Labels[labelSpecHash])
}

func TestUpdateServicesLeavesEverythingRunningWhenAPullFails(t *testing.T) {
	f := newStrictFixture(t)
	f.container("db", "running")
	f.project.Spec.Services[0].Image = "postgres:nope"
	f.docker.pullErr = map[string]error{"postgres:nope": errors.New("manifest unknown")}
	drift, err := f.runtime.SpecDrift(t.Context(), f.project)
	require.NoError(t, err)

	err = f.runtime.UpdateServices(t.Context(), f.project, drift)
	require.ErrorIs(t, err, ErrImagePull)
	assert.Equal(t, []string{"ImagePull:postgres:nope"}, f.docker.mutations)
}

func TestPullErrorsInTheProgressStreamFail(t *testing.T) {
	err := drainPull(strings.NewReader(`{"status":"Pulling from library/nginx"}` + "\n" +
		`{"errorDetail":{"message":"manifest for nginx:nope not found"},"error":"manifest for nginx:nope not found"}` + "\n"))
	assert.ErrorContains(t, err, "manifest for nginx:nope not found")
	assert.NoError(t, drainPull(strings.NewReader(`{"status":"Downloaded newer image"}`+"\n")))
}

// While a Project is being placed, an owned container built from an older spec
// is recreated rather than adopted, with the new image pulled first.
func TestEnsureContainerRecreatesAnOutdatedContainer(t *testing.T) {
	f := newStrictFixture(t)
	f.container("db", "running")
	f.project.Spec.Services[0].Image = "postgres:17"

	require.NoError(t, f.runtime.ReconcileProject(t.Context(), f.project))

	id := fakeID(ContainerName("blog", "db"))
	assert.Equal(t, []string{
		"ImagePull:postgres:17",
		"ContainerStop:" + id,
		"ContainerRemove:" + id,
		"ContainerCreate:" + ContainerName("blog", "db"),
		"ContainerStart:" + id,
	}, withoutSetup(f.docker.mutations))
}

func TestReconcileRemovesTheContainerOfARemovedService(t *testing.T) {
	f := newStrictFixture(t)
	f.container("db", "running")
	f.docker.withContainer(ContainerName("blog", "worker"), "running", f.ownedLabels("worker"))

	require.NoError(t, f.runtime.ReconcileProject(t.Context(), f.project))

	_, left := f.docker.containers[ContainerName("blog", "worker")]
	assert.False(t, left)
	_, kept := f.docker.containers[ContainerName("blog", "db")]
	assert.True(t, kept)
}

// withoutSetup drops the network and volume calls a reconcile makes on the
// way, which these tests are not about.
func withoutSetup(mutations []string) []string {
	var out []string
	for _, m := range mutations {
		if strings.HasPrefix(m, "Network") || strings.HasPrefix(m, "Volume") {
			continue
		}
		out = append(out, m)
	}
	return out
}

func TestRolloutRestartChangesEveryServiceHash(t *testing.T) {
	svc, spec := hashSpec()
	other := v1.ServiceDef{Name: "api", Image: "api:1"}
	spec.Services = append(spec.Services, other)
	before := []string{ServiceSpecHash(svc, spec), ServiceSpecHash(other, spec)}

	at := time.Date(2026, 11, 7, 15, 0, 0, 0, time.UTC)
	spec.RestartedAt = &at
	after := []string{ServiceSpecHash(svc, spec), ServiceSpecHash(other, spec)}
	assert.NotEqual(t, before[0], after[0])
	assert.NotEqual(t, before[1], after[1])

	later := at.Add(time.Second)
	spec.RestartedAt = &later
	assert.NotEqual(t, after[0], ServiceSpecHash(svc, spec), "each restart is a new hash")
}
