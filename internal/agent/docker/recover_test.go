package docker

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
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
	// volumeLabels holds the labels of volumes that have any, for
	// VolumeInspect and the label filters of VolumeList.
	volumeLabels map[string]map[string]string
	// networks maps network name → its labels, for NetworkInspect.
	networks map[string]map[string]string
	// endpoints maps network name → the containers attached to it.
	endpoints map[string]map[string]bool

	// execs maps a command (its words joined by spaces) to what running it
	// produces. A command not listed exits 0 with no output.
	execs map[string]ExecResult
	// lastExec is the command and user of the most recent exec.
	lastExec     []string
	lastExecUser string
	// waitCode is the exit code ContainerWait reports.
	waitCode int64
	// missingImages are images not on the Node until pulled.
	missingImages map[string]bool
	// execHangs makes every exec run on without output or end.
	execHangs bool
	hungPeers []net.Conn

	// mutations records every call that changes Docker state, in order.
	mutations []string

	// commands records the Cmd of every container created, in order.
	commands [][]string

	// onInspect, when set, runs at the start of every ContainerInspect, so a
	// test can change what Docker holds between two reads of it.
	onInspect func(name string)

	// info and infoErr are what Info returns.
	info    system.Info
	infoErr error

	// pullErr makes ImagePull fail for the named images, to drive a
	// ReconcileProject into its rollback part-way through.
	pullErr map[string]error
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		containers:   map[string]types.ContainerJSON{},
		volumes:      map[string]bool{},
		volumeLabels: map[string]map[string]string{},
		networks:     map[string]map[string]string{},
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

func (f *fakeDocker) ContainerCreate(_ context.Context, cfg *container.Config, _ *container.HostConfig,
	_ *network.NetworkingConfig, _ *ocispec.Platform, containerName string) (container.CreateResponse, error) {
	if cfg != nil && f.missingImages[cfg.Image] {
		return container.CreateResponse{}, errdefs.NotFound(fmt.Errorf("No such image: %s", cfg.Image))
	}
	f.record("ContainerCreate:" + containerName)
	var labels map[string]string
	if cfg != nil {
		labels = cfg.Labels
		f.commands = append(f.commands, cfg.Cmd)
	}
	f.withContainer(containerName, "created", labels)
	return container.CreateResponse{ID: fakeID(containerName)}, nil
}

func (f *fakeDocker) ContainerStart(_ context.Context, containerID string, _ container.StartOptions) error {
	f.record("ContainerStart:" + containerID)
	for _, c := range f.containers {
		if c.ID == containerID {
			c.State.Status, c.State.Running = "running", true
		}
	}
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
	return volume.Volume{Name: volumeID, Labels: f.volumeLabels[volumeID]}, nil
}

func (f *fakeDocker) VolumeCreate(_ context.Context, options volume.CreateOptions) (volume.Volume, error) {
	f.record("VolumeCreate:" + options.Name)
	f.volumes[options.Name] = true
	f.volumeLabels[options.Name] = options.Labels
	return volume.Volume{Name: options.Name, Labels: options.Labels}, nil
}

func (f *fakeDocker) VolumeRemove(_ context.Context, volumeID string, _ bool) error {
	f.record("VolumeRemove:" + volumeID)
	delete(f.volumes, volumeID)
	delete(f.volumeLabels, volumeID)
	return nil
}

func (f *fakeDocker) NetworkInspect(_ context.Context, networkID string, _ network.InspectOptions) (network.Inspect, error) {
	labels, ok := f.networks[networkID]
	if !ok {
		return network.Inspect{}, errFakeNotFound
	}
	containers := map[string]network.EndpointResource{}
	for c := range f.endpoints[networkID] {
		containers["id-"+c] = network.EndpointResource{Name: c}
	}
	return network.Inspect{ID: "net-" + networkID, Name: networkID, Labels: labels, Containers: containers}, nil
}

func (f *fakeDocker) NetworkConnect(_ context.Context, networkID, containerID string, _ *network.EndpointSettings) error {
	f.record("NetworkConnect:" + networkID + ":" + containerID)
	if _, ok := f.networks[networkID]; !ok {
		return errFakeNotFound
	}
	if f.endpoints == nil {
		f.endpoints = map[string]map[string]bool{}
	}
	if f.endpoints[networkID] == nil {
		f.endpoints[networkID] = map[string]bool{}
	}
	f.endpoints[networkID][containerID] = true
	return nil
}

func (f *fakeDocker) NetworkDisconnect(_ context.Context, networkID, containerID string, _ bool) error {
	f.record("NetworkDisconnect:" + networkID + ":" + containerID)
	delete(f.endpoints[networkID], containerID)
	return nil
}

func (f *fakeDocker) ContainerWait(_ context.Context, containerID string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	ch := make(chan container.WaitResponse, 1)
	ch <- container.WaitResponse{StatusCode: f.waitCode}
	return ch, make(chan error)
}

func (f *fakeDocker) ContainerKill(_ context.Context, containerID, signal string) error {
	f.record("ContainerKill:" + containerID + ":" + signal)
	return nil
}

// fakeExecs carries an exec's planned result from create to inspect.
var fakeExecs = map[string]ExecResult{}

func (f *fakeDocker) ContainerExecCreate(_ context.Context, containerID string, options container.ExecOptions) (types.IDResponse, error) {
	f.record("ContainerExecCreate:" + containerID)
	f.lastExec, f.lastExecUser = options.Cmd, options.User
	id := fmt.Sprintf("exec-%d", len(fakeExecs))
	fakeExecs[id] = f.execs[strings.Join(options.Cmd, " ")]
	return types.IDResponse{ID: id}, nil
}

func (f *fakeDocker) ContainerExecAttach(_ context.Context, execID string, _ container.ExecAttachOptions) (types.HijackedResponse, error) {
	result := fakeExecs[execID]
	if f.execHangs {
		// A command that runs on without writing: the read ends only when
		// the connection is closed.
		conn, peer := net.Pipe()
		f.hungPeers = append(f.hungPeers, peer)
		return types.HijackedResponse{Conn: conn, Reader: bufio.NewReader(conn)}, nil
	}
	var stream bytes.Buffer
	_, _ = stdcopy.NewStdWriter(&stream, stdcopy.Stdout).Write([]byte(result.Stdout))
	_, _ = stdcopy.NewStdWriter(&stream, stdcopy.Stderr).Write([]byte(result.Stderr))
	conn, peer := net.Pipe()
	_ = peer.Close()
	return types.HijackedResponse{Conn: conn, Reader: bufio.NewReader(&stream)}, nil
}

func (f *fakeDocker) ContainerExecInspect(_ context.Context, execID string) (container.ExecInspect, error) {
	return container.ExecInspect{ExecID: execID, ExitCode: fakeExecs[execID].ExitCode}, nil
}

func (f *fakeDocker) NetworkCreate(_ context.Context, name string, options network.CreateOptions) (network.CreateResponse, error) {
	f.record("NetworkCreate:" + name)
	labels := options.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	f.networks[name] = labels
	return network.CreateResponse{ID: "net-" + name}, nil
}

func (f *fakeDocker) NetworkRemove(_ context.Context, networkID string) error {
	f.record("NetworkRemove:" + networkID)
	delete(f.networks, strings.TrimPrefix(networkID, "net-"))
	return nil
}

func (f *fakeDocker) ImagePull(_ context.Context, refStr string, _ image.PullOptions) (io.ReadCloser, error) {
	f.record("ImagePull:" + refStr)
	if err := f.pullErr[refStr]; err != nil {
		return nil, err
	}
	delete(f.missingImages, refStr)
	return io.NopCloser(strings.NewReader("")), nil
}

// ContainerList honours label filters, "key=value" and bare "key", which is
// all the runtime uses.
func (f *fakeDocker) ContainerList(_ context.Context, opts container.ListOptions) ([]types.Container, error) {
	var out []types.Container
	for name, c := range f.containers {
		var labels map[string]string
		if c.Config != nil {
			labels = c.Config.Labels
		}
		if !matchesLabelFilters(labels, opts.Filters.Get("label")) {
			continue
		}
		out = append(out, types.Container{ID: c.ID, Names: []string{"/" + name}, Labels: labels})
	}
	return out, nil
}

func matchesLabelFilters(labels map[string]string, filters []string) bool {
	for _, f := range filters {
		key, want, hasValue := strings.Cut(f, "=")
		got, ok := labels[key]
		if !ok || (hasValue && got != want) {
			return false
		}
	}
	return true
}

func (f *fakeDocker) ContainerLogs(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *fakeDocker) VolumeList(_ context.Context, opts volume.ListOptions) (volume.ListResponse, error) {
	var out volume.ListResponse
	for name := range f.volumes {
		labels := f.volumeLabels[name]
		if !matchesLabelFilters(labels, opts.Filters.Get("label")) {
			continue
		}
		out.Volumes = append(out.Volumes, &volume.Volume{Name: name, Labels: labels})
	}
	return out, nil
}

func (f *fakeDocker) Info(context.Context) (system.Info, error) {
	return f.info, f.infoErr
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

func TestCreatedContainerUsesTheServiceCommand(t *testing.T) {
	f := newRecoverFixture(t, v1.VolumeTypeManaged)
	f.provisionManaged(t)
	f.project.Spec.Services[0].Command = []string{"sh", "-c", "echo hello"}

	require.NoError(t, f.runtime.RecoverServices(t.Context(), f.project, []string{"db"}))

	require.Len(t, f.docker.commands, 1)
	assert.Equal(t, []string{"sh", "-c", "echo hello"}, []string(f.docker.commands[0]))
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
	f.docker.volumeLabels[VolumeName("blog", "db-data")] = map[string]string{
		labelProject: "blog", labelNamespace: "default", labelUID: f.project.UID,
	}

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

// newStrictFixture is a fixture with its Managed volume provisioned.
func newStrictFixture(t *testing.T) *recoverFixture {
	t.Helper()
	f := newRecoverFixture(t, v1.VolumeTypeManaged)
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
			f := newRecoverFixture(t, v1.VolumeTypeManaged)
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

// The namespace label is required, not merely consistent.
func TestRecoverServicesRequiresTheNamespaceLabel(t *testing.T) {
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
		_, err := f.runtime.ensureContainer(t.Context(), f.project.Namespace, f.project.Name,
			f.project.UID, f.project.Status.AssignmentGeneration, f.project.Spec.Services[0], f.project.Spec.Volumes)
		return err
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

	t.Run("refuses another project's container", func(t *testing.T) {
		f := newRecoverFixture(t, v1.VolumeTypeManaged)
		f.staleContainer("db", "exited", labelProject, "someone-else")

		require.ErrorIs(t, ensure(f), ErrContainerNotOwned)
		assert.Empty(t, f.docker.mutations)
	})
}

// An Ephemeral volume under the right name but of a same-name predecessor
// lifetime is not this Project's data. Recovery must not recreate a container
// against it; it is reported unavailable, as a missing one is, and nothing is
// touched.
func TestRecoverServicesRefusesAPredecessorsEphemeralVolume(t *testing.T) {
	f := newRecoverFixture(t, v1.VolumeTypeEphemeral)
	f.docker.volumes[VolumeName("blog", "db-data")] = true
	f.docker.volumeLabels[VolumeName("blog", "db-data")] = map[string]string{
		labelProject: "blog", labelNamespace: "default", labelUID: "uid-previous-lifetime",
	}

	err := f.runtime.RecoverServices(t.Context(), f.project, []string{"db"})

	require.ErrorIs(t, err, ErrRecoveryVolumeUnavailable)
	assert.ErrorContains(t, err, "another Project lifetime")
	assert.Empty(t, f.docker.mutations)
}
