package docker

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// errFakeNotFound is what the fake returns for anything it does not hold. It
// is wrapped as an errdefs NotFound because the code under test asks the
// Docker client's own IsErrNotFound in some places and this package's
// isNotFound in others; only a real 404-shaped error satisfies both.
var errFakeNotFound = errdefs.NotFound(errors.New("No such container or volume"))

// fakeDocker is a dockerAPI that records mutating calls and answers reads from
// what the test put in it. Its purpose is the negative assertion: a preflight
// that refuses a recovery is only provable by showing that mutations is empty.
type fakeDocker struct {
	// containers maps container name → state, for ContainerInspect.
	containers map[string]types.ContainerJSON
	// volumes is the set of Docker named volumes that exist.
	volumes map[string]bool
	// networks maps network name → its labels, for NetworkInspect.
	networks map[string]map[string]string

	// mutations records every call that changes Docker state, in order.
	mutations []string

	// onInspect, when set, runs at the start of every ContainerInspect, so a
	// test can change what Docker holds between two reads of it.
	onInspect func(name string)
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		containers: map[string]types.ContainerJSON{},
		volumes:    map[string]bool{},
		networks:   map[string]map[string]string{},
	}
}

// withContainer registers a container under its Docker name with the given
// status and labels. A "running" status also sets State.Running, which is
// what RecoverServices branches on first.
func (f *fakeDocker) withContainer(name, status string, labels map[string]string) *fakeDocker {
	f.containers[name] = types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{
			ID: fakeID(name),
			State: &types.ContainerState{
				Status:  status,
				Running: status == "running",
			},
		},
		Config: &container.Config{Labels: labels},
	}
	return f
}

func (f *fakeDocker) record(op string) { f.mutations = append(f.mutations, op) }

// fakeID mints a container ID for a container name. Docker IDs are long hex
// strings and the code under test abbreviates them for logging, so a short id
// would panic on the slice rather than fail an assertion.
func fakeID(containerName string) string {
	return containerName + strings.Repeat("0", 64-len(containerName))
}

func (f *fakeDocker) ContainerInspect(_ context.Context, containerID string) (types.ContainerJSON, error) {
	if f.onInspect != nil {
		f.onInspect(containerID)
	}
	c, ok := f.containers[containerID]
	if !ok {
		return types.ContainerJSON{}, errFakeNotFound
	}
	return c, nil
}

func (f *fakeDocker) ContainerCreate(_ context.Context, _ *container.Config, _ *container.HostConfig,
	_ *network.NetworkingConfig, _ *ocispec.Platform, containerName string) (container.CreateResponse, error) {
	f.record("ContainerCreate:" + containerName)
	return container.CreateResponse{ID: fakeID(containerName)}, nil
}

func (f *fakeDocker) ContainerStart(_ context.Context, containerID string, _ container.StartOptions) error {
	f.record("ContainerStart:" + containerID)
	return nil
}

func (f *fakeDocker) ContainerRemove(_ context.Context, containerID string, _ container.RemoveOptions) error {
	f.record("ContainerRemove:" + containerID)
	// A removed container stops existing, so a later inspect must miss it.
	// Without this the dead-container path would look like it repaired a
	// container it had just destroyed.
	for name, c := range f.containers {
		if c.ID == containerID {
			delete(f.containers, name)
		}
	}
	return nil
}

func (f *fakeDocker) ContainerStop(_ context.Context, containerID string, _ container.StopOptions) error {
	f.record("ContainerStop:" + containerID)
	return nil
}

func (f *fakeDocker) VolumeInspect(_ context.Context, volumeID string) (volume.Volume, error) {
	if !f.volumes[volumeID] {
		return volume.Volume{}, errFakeNotFound
	}
	return volume.Volume{Name: volumeID}, nil
}

func (f *fakeDocker) VolumeCreate(_ context.Context, options volume.CreateOptions) (volume.Volume, error) {
	f.record("VolumeCreate:" + options.Name)
	f.volumes[options.Name] = true
	return volume.Volume{Name: options.Name}, nil
}

func (f *fakeDocker) VolumeRemove(_ context.Context, volumeID string, _ bool) error {
	f.record("VolumeRemove:" + volumeID)
	return nil
}

func (f *fakeDocker) NetworkInspect(_ context.Context, networkID string, _ network.InspectOptions) (network.Inspect, error) {
	labels, ok := f.networks[networkID]
	if !ok {
		return network.Inspect{}, errFakeNotFound
	}
	return network.Inspect{Name: networkID, Labels: labels}, nil
}

func (f *fakeDocker) NetworkCreate(_ context.Context, name string, _ network.CreateOptions) (network.CreateResponse, error) {
	f.record("NetworkCreate:" + name)
	f.networks[name] = map[string]string{}
	return network.CreateResponse{ID: "net-" + name}, nil
}

func (f *fakeDocker) NetworkRemove(_ context.Context, networkID string) error {
	f.record("NetworkRemove:" + networkID)
	return nil
}

func (f *fakeDocker) ImagePull(_ context.Context, refStr string, _ image.PullOptions) (io.ReadCloser, error) {
	f.record("ImagePull:" + refStr)
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *fakeDocker) ContainerList(context.Context, container.ListOptions) ([]types.Container, error) {
	return nil, nil
}

func (f *fakeDocker) ContainerLogs(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *fakeDocker) VolumeList(context.Context, volume.ListOptions) (volume.ListResponse, error) {
	return volume.ListResponse{}, nil
}

func (f *fakeDocker) Close() error { return nil }

// recoverFixture builds a one-service Project mounting one Managed volume,
// wired to a fake Docker and a temporary data root.
type recoverFixture struct {
	runtime *DockerRuntime
	docker  *fakeDocker
	project *v1.Project
	root    string
}

func newRecoverFixture(t *testing.T, volType v1.VolumeType) *recoverFixture {
	t.Helper()

	root := t.TempDir()
	fake := newFakeDocker()

	p := &v1.Project{}
	p.Namespace = "default"
	p.Name = "blog"
	p.UID = "uid-1"
	p.Status.AssignmentGeneration = 7
	p.Spec.Volumes = []v1.VolumeDef{{Name: "db-data", Type: volType}}
	p.Spec.Services = []v1.ServiceDef{{
		Name:         "db",
		Image:        "postgres:16",
		VolumeMounts: []v1.VolumeMount{{Name: "db-data", MountPath: "/var/lib/postgresql/data"}},
	}}

	f := &recoverFixture{
		runtime: &DockerRuntime{client: fake, logger: zap.NewNop(), dataRoot: root},
		docker:  fake,
		project: p,
		root:    root,
	}

	// The network exists in every case; a missing one is a separate concern
	// from a missing volume and would add an unrelated mutation to assert on.
	fake.networks[NetworkName("blog")] = map[string]string{
		labelProject:   p.Name,
		labelNamespace: p.Namespace,
		labelUID:       p.UID,
	}
	return f
}

// ownedLabels are the labels ensureContainer puts on a service's container
// for the fixture's current assignment.
func (f *recoverFixture) ownedLabels(service string) map[string]string {
	return map[string]string{
		labelProject:    f.project.Name,
		labelService:    service,
		labelNamespace:  f.project.Namespace,
		labelUID:        f.project.UID,
		labelGeneration: generationLabel(f.project.Status.AssignmentGeneration),
	}
}

// container registers a container for service that belongs to the current
// assignment.
func (f *recoverFixture) container(service, status string) {
	f.docker.withContainer(ContainerName(f.project.Name, service), status, f.ownedLabels(service))
}

// staleContainer registers a container for service under the right name but
// with one label changed — left behind by another assignment or lifetime.
func (f *recoverFixture) staleContainer(service, status, label, value string) {
	labels := f.ownedLabels(service)
	labels[label] = value
	f.docker.withContainer(ContainerName(f.project.Name, service), status, labels)
}

// provisionManaged creates the host directory a Managed volume needs, as a
// healthy Project would already have.
func (f *recoverFixture) provisionManaged(t *testing.T) string {
	t.Helper()
	path := filepath.Join(f.root, "volumes", "default", "blog", "db-data", "data")
	require.NoError(t, os.MkdirAll(path, 0o700))
	return path
}

// An exited container with its data intact is started. This is the case the
// demo depends on, and the baseline the refusals below are measured against.
func TestRecoverServicesStartsAnExitedContainer(t *testing.T) {
	f := newRecoverFixture(t, v1.VolumeTypeManaged)
	f.provisionManaged(t)
	f.container("db", "exited")

	require.NoError(t, f.runtime.RecoverServices(t.Context(), f.project, []string{"db"}))

	assert.Equal(t, []string{"ContainerStart:" + fakeID("blog-db")}, f.docker.mutations)
}

// A container that is already running again is left alone: an earlier attempt
// succeeded and this poll had not yet seen it.
func TestRecoverServicesSkipsARunningContainer(t *testing.T) {
	f := newRecoverFixture(t, v1.VolumeTypeManaged)
	f.provisionManaged(t)
	f.container("db", "running")

	require.NoError(t, f.runtime.RecoverServices(t.Context(), f.project, []string{"db"}))

	assert.Empty(t, f.docker.mutations)
}

// The four refusals. In each one the fault is repairable at the container
// level and would look repaired afterwards — which is exactly why the missing
// data has to stop it before Docker is touched at all.
func TestRecoverServicesRefusesWhenVolumeDataIsGone(t *testing.T) {
	t.Run("exited container, managed path missing: no start", func(t *testing.T) {
		f := newRecoverFixture(t, v1.VolumeTypeManaged)
		f.container("db", "exited")

		err := f.runtime.RecoverServices(t.Context(), f.project, []string{"db"})

		require.Error(t, err)
		assert.ErrorContains(t, err, "db-data")
		assert.Empty(t, f.docker.mutations, "a start would have recreated the bind source empty")
	})

	t.Run("missing container, managed path missing: no create", func(t *testing.T) {
		f := newRecoverFixture(t, v1.VolumeTypeManaged)
		// No container registered at all.

		err := f.runtime.RecoverServices(t.Context(), f.project, []string{"db"})

		require.Error(t, err)
		assert.Empty(t, f.docker.mutations, "no image pull and no container create")
	})

	t.Run("dead container, managed path missing: no remove", func(t *testing.T) {
		f := newRecoverFixture(t, v1.VolumeTypeManaged)
		f.container("db", "dead")

		err := f.runtime.RecoverServices(t.Context(), f.project, []string{"db"})

		require.Error(t, err)
		assert.Empty(t, f.docker.mutations,
			"the dead container must survive for an operator to inspect")
	})

	t.Run("ephemeral volume gone: nothing is created", func(t *testing.T) {
		f := newRecoverFixture(t, v1.VolumeTypeEphemeral)
		f.container("db", "exited")
		// f.docker.volumes is empty, so the named volume no longer exists.

		err := f.runtime.RecoverServices(t.Context(), f.project, []string{"db"})

		require.Error(t, err)
		assert.ErrorContains(t, err, "no longer exists")
		assert.Empty(t, f.docker.mutations, "recovery must not create the volume back, empty")
	})
}

// The point of a separate first pass: a Project is never left half-repaired
// because the second service's data turned out to be gone.
func TestRecoverServicesRefusesBeforeTouchingAnyServiceWhenOneIsUnsafe(t *testing.T) {
	f := newRecoverFixture(t, v1.VolumeTypeManaged)
	f.provisionManaged(t)

	// A second service, mounting a volume that was never provisioned. It is
	// last in spec order, so a per-service check would have started "db" first.
	f.project.Spec.Volumes = append(f.project.Spec.Volumes,
		v1.VolumeDef{Name: "cache-data", Type: v1.VolumeTypeManaged})
	f.project.Spec.Services = append(f.project.Spec.Services, v1.ServiceDef{
		Name:         "cache",
		Image:        "redis:7",
		VolumeMounts: []v1.VolumeMount{{Name: "cache-data", MountPath: "/data"}},
	})

	f.container("db", "exited")
	f.container("cache", "exited")

	err := f.runtime.RecoverServices(t.Context(), f.project, []string{"db", "cache"})

	require.Error(t, err)
	assert.ErrorContains(t, err, "cache-data")
	assert.Empty(t, f.docker.mutations, "db must not be started when cache cannot be")
}

// Only the named services are touched, and only their volumes are required —
// an unrelated service whose data is missing must not block a recovery that
// does not involve it.
func TestRecoverServicesIgnoresServicesItWasNotAskedFor(t *testing.T) {
	f := newRecoverFixture(t, v1.VolumeTypeManaged)
	f.provisionManaged(t)

	f.project.Spec.Volumes = append(f.project.Spec.Volumes,
		v1.VolumeDef{Name: "cache-data", Type: v1.VolumeTypeManaged})
	f.project.Spec.Services = append(f.project.Spec.Services, v1.ServiceDef{
		Name:         "cache",
		Image:        "redis:7",
		VolumeMounts: []v1.VolumeMount{{Name: "cache-data", MountPath: "/data"}},
	})

	f.container("db", "exited")
	f.container("cache", "exited")

	require.NoError(t, f.runtime.RecoverServices(t.Context(), f.project, []string{"db"}))

	assert.Equal(t, []string{"ContainerStart:" + fakeID("blog-db")}, f.docker.mutations)
}

// The recreate path, with the data where it should be: the container is built
// and started, and nothing else is provisioned on its behalf — in particular
// no volume is created, which is the whole point of the preflight above.
func TestRecoverServicesRecreatesAMissingContainer(t *testing.T) {
	f := newRecoverFixture(t, v1.VolumeTypeManaged)
	f.provisionManaged(t)
	// No container registered: this is the "missing" fault.

	require.NoError(t, f.runtime.RecoverServices(t.Context(), f.project, []string{"db"}))

	assert.Equal(t, []string{
		"ImagePull:postgres:16",
		"ContainerCreate:blog-db",
		"ContainerStart:" + fakeID("blog-db"),
	}, f.docker.mutations)
}

// A dead container is removed first, then rebuilt. The removal is allowed only
// because the preflight has already confirmed the data it mounts is present.
func TestRecoverServicesReplacesADeadContainer(t *testing.T) {
	f := newRecoverFixture(t, v1.VolumeTypeManaged)
	f.provisionManaged(t)
	f.container("db", "dead")

	require.NoError(t, f.runtime.RecoverServices(t.Context(), f.project, []string{"db"}))

	require.NotEmpty(t, f.docker.mutations)
	assert.Equal(t, "ContainerRemove:"+fakeID("blog-db"), f.docker.mutations[0],
		"the dead container is discarded before anything is built")
	assert.Contains(t, f.docker.mutations, "ContainerCreate:blog-db")
}

// The window the second preflight exists for: the caller's PreflightRecovery
// passes, the data is deleted, and only then is RecoverServices called. It
// must refuse on its own evidence, before its first Docker call, with the
// typed error the caller uses to tell "blocked" from "failed".
func TestRecoverServicesRechecksVolumesAfterTheCallersPreflight(t *testing.T) {
	f := newRecoverFixture(t, v1.VolumeTypeManaged)
	path := f.provisionManaged(t)
	f.container("db", "exited")

	require.NoError(t, f.runtime.PreflightRecovery(t.Context(), f.project, []string{"db"}))

	require.NoError(t, os.RemoveAll(path)) // deleted in the window

	err := f.runtime.RecoverServices(t.Context(), f.project, []string{"db"})

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRecoveryVolumeUnavailable)
	assert.Empty(t, f.docker.mutations, "no start against a bind source Docker would recreate empty")
}

// PreflightRecovery is read-only: it inspects, and never creates or starts.
func TestPreflightRecoveryMutatesNothing(t *testing.T) {
	f := newRecoverFixture(t, v1.VolumeTypeEphemeral)
	f.container("db", "exited")
	f.docker.volumes[VolumeName("blog", "db-data")] = true

	require.NoError(t, f.runtime.PreflightRecovery(t.Context(), f.project, []string{"db"}))
	assert.Empty(t, f.docker.mutations)

	delete(f.docker.volumes, VolumeName("blog", "db-data"))
	err := f.runtime.PreflightRecovery(t.Context(), f.project, []string{"db"})
	assert.ErrorIs(t, err, ErrRecoveryVolumeUnavailable)
	assert.Empty(t, f.docker.mutations, "a failed preflight must not create the volume back")
}

// Spec mistakes are refused too, but they are not "data is missing" and must
// not be reported as such.
func TestPreflightRecoverySpecErrorsAreNotVolumeUnavailable(t *testing.T) {
	f := newRecoverFixture(t, v1.VolumeTypeManaged)
	f.project.Spec.Services[0].VolumeMounts = []v1.VolumeMount{{Name: "nowhere", MountPath: "/data"}}

	err := f.runtime.PreflightRecovery(t.Context(), f.project, []string{"db"})

	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrRecoveryVolumeUnavailable)
}

// The preflight names the failing service and volume in fields the caller can
// publish, and keeps the host path and OS error in Detail, which it cannot.
func TestPreflightRecoveryReportsWhichVolumeIsMissing(t *testing.T) {
	t.Run("managed", func(t *testing.T) {
		f := newRecoverFixture(t, v1.VolumeTypeManaged)

		err := f.runtime.PreflightRecovery(t.Context(), f.project, []string{"db"})

		var vu *VolumeUnavailableError
		require.ErrorAs(t, err, &vu)
		assert.Equal(t, "db", vu.Service)
		assert.Equal(t, "db-data", vu.Volume)
		assert.Equal(t, v1.VolumeTypeManaged, vu.Type)
		assert.Contains(t, vu.Detail, f.root, "the host path is kept, for the log")
		assert.ErrorIs(t, err, ErrRecoveryVolumeUnavailable)
	})

	t.Run("ephemeral", func(t *testing.T) {
		f := newRecoverFixture(t, v1.VolumeTypeEphemeral)

		err := f.runtime.PreflightRecovery(t.Context(), f.project, []string{"db"})

		var vu *VolumeUnavailableError
		require.ErrorAs(t, err, &vu)
		assert.Equal(t, "db", vu.Service)
		assert.Equal(t, "db-data", vu.Volume)
		assert.Equal(t, v1.VolumeTypeEphemeral, vu.Type)
	})
}

// ── Container ownership ──────────────────────────────────────────────────────

// newStrictFixture is a fixture with UID enforcement on, the mode CARA-83's
// generation fence is enforced in.
func newStrictFixture(t *testing.T) *recoverFixture {
	t.Helper()
	f := newRecoverFixture(t, v1.VolumeTypeManaged)
	f.runtime.enforceUID = true
	f.provisionManaged(t)
	return f
}

// Generation 7 left an exited container; the Project is now generation 8. The
// name is the same, so only the label tells them apart — and a start would
// run generation 7's container as generation 8's.
func TestRecoverServicesRefusesToStartAStaleGenerationContainer(t *testing.T) {
	f := newStrictFixture(t)
	f.staleContainer("db", "exited", labelGeneration, "6")

	err := f.runtime.RecoverServices(t.Context(), f.project, []string{"db"})

	var notOwned *ContainerNotOwnedError
	require.ErrorAs(t, err, &notOwned)
	assert.ErrorIs(t, err, ErrContainerNotOwned)
	assert.Equal(t, labelGeneration, notOwned.Label)
	assert.Equal(t, "6", notOwned.Got)
	assert.Equal(t, "7", notOwned.Want)
	assert.Empty(t, f.docker.mutations, "a stale container must not be started")
}

// A dead container from a previous lifetime of the same name is not ours to
// remove, however dead it is.
func TestRecoverServicesRefusesToRemoveAStaleUIDContainer(t *testing.T) {
	f := newStrictFixture(t)
	f.staleContainer("db", "dead", labelUID, "uid-previous-lifetime")

	err := f.runtime.RecoverServices(t.Context(), f.project, []string{"db"})

	require.ErrorIs(t, err, ErrContainerNotOwned)
	assert.Empty(t, f.docker.mutations, "a stale container must not be removed")
}

// A stale container that happens to be running is not a recovery of this
// assignment and must not be counted as one.
func TestRecoverServicesDoesNotCountAStaleRunningContainerAsRecovered(t *testing.T) {
	f := newStrictFixture(t)
	f.staleContainer("db", "running", labelGeneration, "6")

	err := f.runtime.RecoverServices(t.Context(), f.project, []string{"db"})

	require.ErrorIs(t, err, ErrContainerNotOwned)
	assert.Empty(t, f.docker.mutations)
}

// The current assignment's own container is recovered normally.
func TestRecoverServicesRecoversTheCurrentAssignmentsContainer(t *testing.T) {
	f := newStrictFixture(t)
	f.container("db", "exited")

	require.NoError(t, f.runtime.RecoverServices(t.Context(), f.project, []string{"db"}))

	assert.Equal(t, []string{"ContainerStart:" + fakeID("blog-db")}, f.docker.mutations)
}

// The ownership check is part of the preflight, so the caller's gate refuses
// before an attempt is spent — and one stale service refuses the whole call
// before a healthy neighbour is touched.
func TestPreflightRecoveryRefusesAStaleContainer(t *testing.T) {
	f := newStrictFixture(t)
	f.project.Spec.Services = append(f.project.Spec.Services, v1.ServiceDef{Name: "cache", Image: "redis:7"})
	f.container("db", "exited")
	f.staleContainer("cache", "exited", labelUID, "uid-previous-lifetime")

	assert.ErrorIs(t, f.runtime.PreflightRecovery(t.Context(), f.project, []string{"db", "cache"}),
		ErrContainerNotOwned)

	err := f.runtime.RecoverServices(t.Context(), f.project, []string{"db", "cache"})
	require.ErrorIs(t, err, ErrContainerNotOwned)
	assert.Empty(t, f.docker.mutations, "db must not be started when cache's container is stale")
}

// Some labels are checked in every mode, because they are what makes a
// container this Project's at all.
func TestRecoverServicesAlwaysChecksProjectServiceAndNamespace(t *testing.T) {
	cases := []struct{ label, value string }{
		{labelProject, "another-project"},
		{labelService, "another-service"},
		{labelNamespace, "another-namespace"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			f := newRecoverFixture(t, v1.VolumeTypeManaged) // compatibility mode
			f.provisionManaged(t)
			f.staleContainer("db", "exited", tc.label, tc.value)

			err := f.runtime.RecoverServices(t.Context(), f.project, []string{"db"})

			require.ErrorIs(t, err, ErrContainerNotOwned)
			assert.Empty(t, f.docker.mutations)
		})
	}
}

// A container with no labels cannot be shown to be ours.
func TestRecoverServicesRefusesAnUnlabelledContainer(t *testing.T) {
	f := newRecoverFixture(t, v1.VolumeTypeManaged)
	f.provisionManaged(t)
	f.docker.withContainer(ContainerName("blog", "db"), "exited", nil)

	require.ErrorIs(t, f.runtime.RecoverServices(t.Context(), f.project, []string{"db"}), ErrContainerNotOwned)
	assert.Empty(t, f.docker.mutations)
}

// Compatibility mode keeps its documented contract: during a mixed-agent
// rollout ownership falls back to (namespace, name), so a container without
// the UID and generation labels — or with older ones — is still recovered,
// exactly as ensureContainer would adopt it. A missing namespace label is
// tolerated for the same reason. Strict mode is what closes these.
func TestRecoverServicesCompatibilityModeIgnoresUIDAndGeneration(t *testing.T) {
	f := newRecoverFixture(t, v1.VolumeTypeManaged)
	f.provisionManaged(t)
	f.docker.withContainer(ContainerName("blog", "db"), "exited", map[string]string{
		labelProject:    "blog",
		labelService:    "db",
		labelUID:        "uid-previous-lifetime",
		labelGeneration: "6",
	})

	require.NoError(t, f.runtime.RecoverServices(t.Context(), f.project, []string{"db"}))
	assert.Equal(t, []string{"ContainerStart:" + fakeID("blog-db")}, f.docker.mutations)
}

// In strict mode the namespace label is required, not merely consistent.
func TestRecoverServicesStrictModeRequiresTheNamespaceLabel(t *testing.T) {
	f := newStrictFixture(t)
	labels := f.ownedLabels("db")
	delete(labels, labelNamespace)
	f.docker.withContainer(ContainerName("blog", "db"), "exited", labels)

	require.ErrorIs(t, f.runtime.RecoverServices(t.Context(), f.project, []string{"db"}), ErrContainerNotOwned)
	assert.Empty(t, f.docker.mutations)
}

// The container is swapped for a stale one between the preflight's inspect and
// the inspect the start acts on. Pass 1 saw a container it owned; the check
// on the second read is what stops the start.
func TestRecoverServicesRechecksOwnershipOnTheCopyItActsOn(t *testing.T) {
	f := newStrictFixture(t)
	f.container("db", "exited")

	inspections := 0
	f.docker.onInspect = func(string) {
		inspections++
		if inspections == 2 {
			f.staleContainer("db", "exited", labelGeneration, "6")
		}
	}

	err := f.runtime.RecoverServices(t.Context(), f.project, []string{"db"})

	require.ErrorIs(t, err, ErrContainerNotOwned)
	assert.Empty(t, f.docker.mutations, "the start must use the ownership of what it is about to start")
}

// ── InspectProject and ensureContainer share the ownership rule ──────────────

// InspectProject finds containers by name, so it reports for each whether the
// container under that name is this assignment's. Without that a stale
// running container reads as a healthy Project.
func TestInspectProjectReportsOwnership(t *testing.T) {
	f := newStrictFixture(t)
	f.project.Spec.Services = append(f.project.Spec.Services, v1.ServiceDef{Name: "cache", Image: "redis:7"})
	f.container("db", "running")
	f.staleContainer("cache", "running", labelGeneration, "6")

	states, err := f.runtime.InspectProject(t.Context(), f.project)

	require.NoError(t, err)
	require.Len(t, states, 2)
	assert.Nil(t, states[0].NotOwned, "db belongs to generation 7")
	var no *ContainerNotOwnedError
	require.ErrorAs(t, states[1].NotOwned, &no)
	assert.Equal(t, "cache", no.Service)
	assert.Equal(t, "running", states[1].Status, "the Docker facts are still reported")
	assert.Empty(t, f.docker.mutations, "inspecting is read-only")
}

// ensureContainer adopts only by the same rule — the one place a divergent
// copy of it used to live.
func TestEnsureContainerUsesTheSharedOwnershipRule(t *testing.T) {
	ensure := func(f *recoverFixture) error {
		return f.runtime.ensureContainer(t.Context(), f.project.Namespace, f.project.Name,
			f.project.UID, f.project.Status.AssignmentGeneration, f.project.Spec.Services[0], f.project.Spec.Volumes)
	}

	t.Run("adopts and starts its own stopped container", func(t *testing.T) {
		f := newStrictFixture(t)
		f.container("db", "exited")

		require.NoError(t, ensure(f))
		assert.Equal(t, []string{"ContainerStart:" + fakeID("blog-db")}, f.docker.mutations)
	})

	t.Run("refuses a stale generation", func(t *testing.T) {
		f := newStrictFixture(t)
		f.staleContainer("db", "exited", labelGeneration, "6")

		err := ensure(f)
		require.ErrorIs(t, err, ErrContainerNotOwned)
		assert.ErrorContains(t, err, "refuse to adopt")
		assert.Empty(t, f.docker.mutations)
	})

	// Previously the UID and generation checks were skipped entirely when the
	// container's config could not be read.
	t.Run("fails closed when the config is missing", func(t *testing.T) {
		f := newStrictFixture(t)
		f.docker.containers[ContainerName("blog", "db")] = types.ContainerJSON{
			ContainerJSONBase: &types.ContainerJSONBase{
				ID:    fakeID("blog-db"),
				State: &types.ContainerState{Status: "exited"},
			},
		}

		require.ErrorIs(t, ensure(f), ErrContainerNotOwned)
		assert.Empty(t, f.docker.mutations)
	})

	// Previously compatibility mode adopted any container with the right
	// name. The project and service labels are now required in every mode.
	t.Run("refuses another project's container in compatibility mode", func(t *testing.T) {
		f := newRecoverFixture(t, v1.VolumeTypeManaged)
		f.staleContainer("db", "exited", labelProject, "someone-else")

		require.ErrorIs(t, ensure(f), ErrContainerNotOwned)
		assert.Empty(t, f.docker.mutations)
	})
}
