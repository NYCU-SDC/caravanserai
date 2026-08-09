package docker

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	v1 "NYCU-SDC/caravanserai/api/v1"
	caravolume "NYCU-SDC/caravanserai/internal/agent/volume"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	dockerclient "github.com/docker/docker/client"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

const (
	// labelProject is attached to every container/network/volume created by
	// the agent so that RemoveProject can find them reliably.
	labelProject = "cara.project"
	// labelService tags each container with its ServiceDef name.
	labelService = "cara.service"
	// labelNamespace records the Project's namespace on containers using a
	// Managed volume, so operators and future tooling can map a running
	// container back to its host data directory.
	labelNamespace = "cara.namespace"
	// labelVolume lists the Managed volumes bound into a container (comma
	// separated).
	labelVolume = "cara.volume"
	// labelVolumeType records that a container carries a Managed volume.
	labelVolumeType = "cara.volume.type"

	// stopTimeoutSeconds is how long Docker waits for a container to exit
	// after SIGTERM before killing it.
	stopTimeoutSeconds = 10
)

// DockerRuntime is the production implementation of Runtime backed by the
// Docker Engine API.
type DockerRuntime struct {
	client   *dockerclient.Client
	logger   *zap.Logger
	dataRoot string
}

// NewDockerRuntime creates a DockerRuntime connected to the Docker daemon at
// host (e.g. "unix:///var/run/docker.sock" or "tcp://127.0.0.1:2375").
// WithAPIVersionNegotiation is always enabled so the client works with a range
// of Docker daemon versions.
//
// dataRoot is the directory the agent owns for Managed volume data; Managed
// volumes are bind-mounted from {dataRoot}/volumes/{namespace}/{project}/{volume}/data.
func NewDockerRuntime(host, dataRoot string, logger *zap.Logger) (*DockerRuntime, error) {
	c, err := dockerclient.NewClientWithOpts(
		dockerclient.WithHost(host),
		dockerclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("docker: create client: %w", err)
	}
	return &DockerRuntime{client: c, logger: logger, dataRoot: dataRoot}, nil
}

// Close releases the underlying HTTP connection to the Docker daemon.
func (r *DockerRuntime) Close() error {
	return r.client.Close()
}

// ── Runtime interface ────────────────────────────────────────────────────────

// ReconcileProject implements Runtime.
func (r *DockerRuntime) ReconcileProject(ctx context.Context, project *v1.Project) error {
	log := r.logger.With(zap.String("project", project.Name))

	// 1. Ensure the bridge network exists.
	if err := r.ensureNetwork(ctx, project.Name); err != nil {
		return fmt.Errorf("ensure network: %w", err)
	}

	// 2. Ensure all volumes exist: Ephemeral as Docker named volumes, Managed
	//    as agent-owned host bind directories.
	if err := r.ensureVolumes(ctx, project.Namespace, project.Name, project.Spec.Volumes); err != nil {
		r.rollback(ctx, project, log)
		return fmt.Errorf("ensure volumes: %w", err)
	}

	// 3. Ensure every service container exists and is running.
	for _, svc := range project.Spec.Services {
		if err := r.ensureContainer(ctx, project.Namespace, project.Name, svc, project.Spec.Volumes); err != nil {
			r.rollback(ctx, project, log)
			return fmt.Errorf("ensure container %q: %w", svc.Name, err)
		}
		log.Info("Service container reconciled", zap.String("service", svc.Name))
	}

	return nil
}

// rollback removes all Docker resources (containers, network, volumes) that
// were partially created during a failed ReconcileProject. It uses
// RemoveProject which is already idempotent and tolerates missing resources.
func (r *DockerRuntime) rollback(ctx context.Context, project *v1.Project, log *zap.Logger) {
	log.Warn("Reconcile failed, rolling back Docker resources")
	if err := r.RemoveProject(ctx, project.Namespace, project.Name, project.Spec); err != nil {
		log.Error("Rollback failed, resources may leak",
			zap.Error(err))
	}
}

// volumeRemovalPlan describes what RemoveProject does with a project's volumes:
// which Docker named volumes to delete, and which Managed host directories are
// kept (their paths logged for the operator).
type volumeRemovalPlan struct {
	// removeNamedVolumes are Docker named volumes to delete. This covers both
	// Ephemeral volumes and any legacy named volume auto-created for a Managed
	// volume before bind mounts were wired up (pre-CARA-66).
	removeNamedVolumes []string
	// retainedManagedPaths are host directories left in place on disk.
	retainedManagedPaths []string
	// pathErrors holds a HostPath derivation failure per volume that could not
	// be resolved (should not happen for names that already passed
	// v1.ValidateName at create time, but the caller still needs to know so it
	// can log instead of silently skipping the retention log line).
	pathErrors []error
}

// planVolumeRemoval decides the fate of each volume without touching Docker or
// the filesystem, so the retention policy is unit-testable. Managed host data
// is always retained; its legacy named volume (if any) is still scheduled for
// removal so old deployments do not keep an orphan.
func planVolumeRemoval(dataRoot, namespace, projectName string, vols []v1.VolumeDef) volumeRemovalPlan {
	var plan volumeRemovalPlan
	for _, vol := range vols {
		named := VolumeName(projectName, vol.Name)
		switch vol.Type {
		case v1.VolumeTypeEphemeral:
			plan.removeNamedVolumes = append(plan.removeNamedVolumes, named)
		case v1.VolumeTypeManaged:
			// Retain the host data; sweep any pre-CARA-66 orphan named volume.
			plan.removeNamedVolumes = append(plan.removeNamedVolumes, named)
			hostPath, err := caravolume.HostPath(dataRoot, namespace, projectName, vol.Name)
			if err != nil {
				plan.pathErrors = append(plan.pathErrors,
					fmt.Errorf("derive host path for volume %q: %w", vol.Name, err))
				continue
			}
			plan.retainedManagedPaths = append(plan.retainedManagedPaths, hostPath)
		}
	}
	return plan
}

// RemoveProject implements Runtime.
func (r *DockerRuntime) RemoveProject(ctx context.Context, namespace, projectName string, spec v1.ProjectSpec) error {
	log := r.logger.With(zap.String("project", projectName))

	// Stop and remove containers tagged with this project.
	f := filters.NewArgs(filters.Arg("label", labelProject+"="+projectName))
	containers, err := r.client.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: f,
	})
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}
	for _, c := range containers {
		log.Info("Stopping container", zap.String("id", c.ID[:12]))
		timeout := stopTimeoutSeconds
		if err := r.client.ContainerStop(ctx, c.ID, container.StopOptions{Timeout: &timeout}); err != nil {
			log.Warn("Failed to stop container", zap.String("id", c.ID[:12]), zap.Error(err))
		}
		if err := r.client.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err != nil {
			log.Warn("Failed to remove container", zap.String("id", c.ID[:12]), zap.Error(err))
		}
	}

	// Remove the bridge network.
	netName := NetworkName(projectName)
	if err := r.client.NetworkRemove(ctx, netName); err != nil {
		if !isNotFound(err) {
			log.Warn("Failed to remove network", zap.String("network", netName), zap.Error(err))
		}
	}

	plan := planVolumeRemoval(r.dataRoot, namespace, projectName, spec.Volumes)
	for _, path := range plan.retainedManagedPaths {
		// Managed data is retained on delete — a `caractrl delete` typo must not
		// destroy a database. Log the host path so an operator can reclaim it.
		log.Info("Retaining managed volume data on host", zap.String("path", path))
	}
	for _, pathErr := range plan.pathErrors {
		log.Warn("Failed to derive managed volume host path; its data may be orphaned on disk", zap.Error(pathErr))
	}
	for _, vName := range plan.removeNamedVolumes {
		if err := r.client.VolumeRemove(ctx, vName, false); err != nil {
			if !isNotFound(err) {
				log.Warn("Failed to remove named volume", zap.String("volume", vName), zap.Error(err))
			}
		}
	}

	log.Info("Project resources removed")
	return nil
}

// ListLocalProjects implements Runtime.
func (r *DockerRuntime) ListLocalProjects(ctx context.Context) ([]string, error) {
	// Filtering on the bare label key (no "=value") matches every container the
	// agent created, whatever project it belongs to.
	f := filters.NewArgs(filters.Arg("label", labelProject))
	containers, err := r.client.ContainerList(ctx, container.ListOptions{
		// All includes stopped containers. An orphan whose process died is
		// still an orphan: its network and volumes remain, and it restarts on
		// the next daemon start unless it is removed.
		All:     true,
		Filters: f,
	})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	seen := make(map[string]struct{}, len(containers))
	names := make([]string, 0, len(containers))
	for _, c := range containers {
		name := c.Labels[labelProject]
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names, nil
}

// RemoveOrphanProject implements Runtime.
func (r *DockerRuntime) RemoveOrphanProject(ctx context.Context, projectName string) error {
	log := r.logger.With(zap.String("project", projectName))
	f := filters.NewArgs(filters.Arg("label", labelProject+"="+projectName))

	containers, err := r.client.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: f,
	})
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}
	for _, c := range containers {
		log.Info("Stopping orphaned container", zap.String("id", c.ID[:12]))
		timeout := stopTimeoutSeconds
		if err := r.client.ContainerStop(ctx, c.ID, container.StopOptions{Timeout: &timeout}); err != nil {
			log.Warn("Failed to stop orphaned container", zap.String("id", c.ID[:12]), zap.Error(err))
		}
		if err := r.client.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err != nil {
			log.Warn("Failed to remove orphaned container", zap.String("id", c.ID[:12]), zap.Error(err))
		}
	}

	netName := NetworkName(projectName)
	if err := r.client.NetworkRemove(ctx, netName); err != nil {
		if !isNotFound(err) {
			log.Warn("Failed to remove orphaned network", zap.String("network", netName), zap.Error(err))
		}
	}

	// Only Ephemeral volumes (and legacy pre-CARA-66 named volumes) carry this
	// label. Managed volume data lives in a host directory that no Docker
	// volume filter can reach, so it survives this sweep by construction.
	vols, err := r.client.VolumeList(ctx, volume.ListOptions{Filters: f})
	if err != nil {
		return fmt.Errorf("list volumes: %w", err)
	}
	for _, v := range vols.Volumes {
		if err := r.client.VolumeRemove(ctx, v.Name, false); err != nil {
			if !isNotFound(err) {
				log.Warn("Failed to remove orphaned volume", zap.String("volume", v.Name), zap.Error(err))
			}
		}
	}

	log.Info("Orphaned project resources removed")
	return nil
}

// StopProject implements Runtime.
//
// Services are stopped in reverse spec order so a dependent service goes down
// before what it depends on — stopping a database out from under a still-live
// web service invites write errors in the final moments before quiescing.
func (r *DockerRuntime) StopProject(ctx context.Context, project *v1.Project) error {
	log := r.logger.With(zap.String("project", project.Name))
	timeout := stopTimeoutSeconds

	for i := len(project.Spec.Services) - 1; i >= 0; i-- {
		svc := project.Spec.Services[i]
		name := ContainerName(project.Name, svc.Name)
		if err := r.client.ContainerStop(ctx, name, container.StopOptions{Timeout: &timeout}); err != nil {
			if isNotFound(err) {
				// Nothing to stop; treat as already satisfied so the caller's
				// flow is idempotent.
				continue
			}
			return fmt.Errorf("stop container %q: %w", name, err)
		}
		log.Debug("Container stopped", zap.String("service", svc.Name))
	}
	return nil
}

// StartProject implements Runtime.
//
// Services start in spec order, the mirror of StopProject.
func (r *DockerRuntime) StartProject(ctx context.Context, project *v1.Project) error {
	log := r.logger.With(zap.String("project", project.Name))

	for _, svc := range project.Spec.Services {
		name := ContainerName(project.Name, svc.Name)
		if err := r.client.ContainerStart(ctx, name, container.StartOptions{}); err != nil {
			if isNotFound(err) {
				continue
			}
			return fmt.Errorf("start container %q: %w", name, err)
		}
		log.Debug("Container started", zap.String("service", svc.Name))
	}
	return nil
}

// InspectProject implements Runtime.
func (r *DockerRuntime) InspectProject(ctx context.Context, project *v1.Project) ([]ContainerState, error) {
	var states []ContainerState

	for _, svc := range project.Spec.Services {
		name := ContainerName(project.Name, svc.Name)
		info, err := r.client.ContainerInspect(ctx, name)
		if err != nil {
			if dockerclient.IsErrNotFound(err) {
				// Container not yet created; omit from result.
				continue
			}
			return nil, fmt.Errorf("inspect container %q: %w", name, err)
		}

		states = append(states, ContainerState{
			ServiceName: svc.Name,
			ContainerID: info.ID,
			Status:      info.State.Status,
			ExitCode:    info.State.ExitCode,
		})
	}

	return states, nil
}

// GetContainerIPs implements Runtime.
func (r *DockerRuntime) GetContainerIPs(ctx context.Context, project *v1.Project) (map[string]string, error) {
	netName := NetworkName(project.Name)
	ips := make(map[string]string, len(project.Spec.Services))

	for _, svc := range project.Spec.Services {
		cName := ContainerName(project.Name, svc.Name)
		info, err := r.client.ContainerInspect(ctx, cName)
		if err != nil {
			if dockerclient.IsErrNotFound(err) {
				continue // container not created yet
			}
			return nil, fmt.Errorf("inspect container %q: %w", cName, err)
		}

		if net, ok := info.NetworkSettings.Networks[netName]; ok && net.IPAddress != "" {
			ips[svc.Name] = net.IPAddress
		}
	}

	return ips, nil
}

// ── Internal helpers ─────────────────────────────────────────────────────────

// ensureNetwork creates the project's bridge network if it does not yet exist.
func (r *DockerRuntime) ensureNetwork(ctx context.Context, projectName string) error {
	netName := NetworkName(projectName)

	_, err := r.client.NetworkInspect(ctx, netName, network.InspectOptions{})
	if err == nil {
		r.logger.Debug("Network already exists", zap.String("network", netName))
		return nil
	}
	if !dockerclient.IsErrNotFound(err) {
		return fmt.Errorf("inspect network: %w", err)
	}

	_, err = r.client.NetworkCreate(ctx, netName, network.CreateOptions{
		Driver: "bridge",
		Labels: map[string]string{labelProject: projectName},
	})
	if err != nil {
		return fmt.Errorf("create network: %w", err)
	}
	r.logger.Info("Network created", zap.String("network", netName))
	return nil
}

// ensureVolumes provisions every volume in the project: Ephemeral as Docker
// named volumes, Managed as agent-owned host bind directories. A Managed
// volume whose directory cannot be created or is not writable is a hard error
// so the caller can fail the project rather than start containers against a
// missing mount.
func (r *DockerRuntime) ensureVolumes(ctx context.Context, namespace, projectName string, vols []v1.VolumeDef) error {
	for _, vol := range vols {
		switch vol.Type {
		case v1.VolumeTypeManaged:
			if err := r.ensureManagedDir(namespace, projectName, vol.Name); err != nil {
				return err
			}
		case v1.VolumeTypeEphemeral:
			if err := r.ensureEphemeralVolume(ctx, namespace, projectName, vol.Name); err != nil {
				return err
			}
		default:
			return fmt.Errorf("volume %q: unsupported type %q", vol.Name, vol.Type)
		}
	}
	return nil
}

// ensureManagedDir creates the host bind directory for a Managed volume with
// owner-only permissions and verifies it is a writable directory. Existing
// content is left untouched — that data is the point of a Managed volume.
func (r *DockerRuntime) ensureManagedDir(namespace, projectName, volumeName string) error {
	hostPath, err := caravolume.HostPath(r.dataRoot, namespace, projectName, volumeName)
	if err != nil {
		return fmt.Errorf("derive host path for volume %q: %w", volumeName, err)
	}

	if err := os.MkdirAll(hostPath, 0o700); err != nil {
		return fmt.Errorf("create managed volume dir %q: %w", hostPath, err)
	}

	info, err := os.Stat(hostPath)
	if err != nil {
		return fmt.Errorf("stat managed volume dir %q: %w", hostPath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("managed volume path %q exists but is not a directory", hostPath)
	}
	if err := unix.Access(hostPath, unix.W_OK); err != nil {
		return fmt.Errorf("managed volume dir %q is not writable: %w", hostPath, err)
	}

	r.logger.Info("Managed volume directory ready",
		zap.String("volume", volumeName), zap.String("path", hostPath))
	return nil
}

// ensureEphemeralVolume creates the Docker named volume for an Ephemeral volume
// if it does not already exist.
func (r *DockerRuntime) ensureEphemeralVolume(ctx context.Context, namespace, projectName, volumeName string) error {
	vName := VolumeName(projectName, volumeName)
	_, err := r.client.VolumeInspect(ctx, vName)
	if err == nil {
		r.logger.Debug("Volume already exists", zap.String("volume", vName))
		return nil
	}
	if !isNotFound(err) {
		return fmt.Errorf("inspect volume %q: %w", vName, err)
	}

	if _, err := r.client.VolumeCreate(ctx, volume.CreateOptions{
		Name: vName,
		Labels: map[string]string{
			labelProject:    projectName,
			labelNamespace:  namespace,
			labelVolume:     volumeName,
			labelVolumeType: string(v1.VolumeTypeEphemeral),
		},
	}); err != nil {
		return fmt.Errorf("create volume %q: %w", vName, err)
	}
	r.logger.Info("Volume created", zap.String("volume", vName))
	return nil
}

// buildBinds resolves a service's volume mounts into Docker bind strings. The
// source depends on the volume type: Managed → absolute host path (a real bind
// mount under the data root), Ephemeral → the Docker named volume. Selecting by
// type is what stops a Managed volume from silently falling through to Docker's
// named-volume auto-create. It also returns the names of the Managed volumes
// bound, for labelling. An undeclared or unsupported volume is an error rather
// than a fall-through.
func (r *DockerRuntime) buildBinds(namespace, projectName string, svc v1.ServiceDef, vols []v1.VolumeDef) (binds, managedNames []string, err error) {
	byName := make(map[string]v1.VolumeDef, len(vols))
	for _, v := range vols {
		byName[v.Name] = v
	}
	for _, vm := range svc.VolumeMounts {
		vol, ok := byName[vm.Name]
		if !ok {
			// validateProjectSpec rejects this at apply time; guard here too so
			// a mount can never fall through to Docker's auto-create.
			return nil, nil, fmt.Errorf("service %q mounts undeclared volume %q", svc.Name, vm.Name)
		}
		switch vol.Type {
		case v1.VolumeTypeManaged:
			hostPath, herr := caravolume.HostPath(r.dataRoot, namespace, projectName, vm.Name)
			if herr != nil {
				return nil, nil, fmt.Errorf("derive host path for volume %q: %w", vm.Name, herr)
			}
			binds = append(binds, hostPath+":"+vm.MountPath)
			managedNames = append(managedNames, vm.Name)
		case v1.VolumeTypeEphemeral:
			binds = append(binds, VolumeName(projectName, vm.Name)+":"+vm.MountPath)
		default:
			return nil, nil, fmt.Errorf("service %q mounts volume %q of unsupported type %q", svc.Name, vm.Name, vol.Type)
		}
	}
	return binds, managedNames, nil
}

// ensureContainer creates and starts the container for a single service if it
// is not already running. vols is the project's volume list, used to resolve
// each mount's bind source by volume type.
func (r *DockerRuntime) ensureContainer(ctx context.Context, namespace, projectName string, svc v1.ServiceDef, vols []v1.VolumeDef) error {
	cName := ContainerName(projectName, svc.Name)
	log := r.logger.With(
		zap.String("container", cName),
		zap.String("image", svc.Image),
	)

	info, err := r.client.ContainerInspect(ctx, cName)
	if err != nil && !dockerclient.IsErrNotFound(err) {
		return fmt.Errorf("inspect: %w", err)
	}

	if err == nil {
		// Container exists.
		if info.State.Running {
			log.Debug("Container already running")
			return nil
		}
		// Stopped or exited — try to start it.
		log.Info("Container stopped, restarting", zap.String("status", info.State.Status))
		if startErr := r.client.ContainerStart(ctx, info.ID, container.StartOptions{}); startErr != nil {
			return fmt.Errorf("start existing container: %w", startErr)
		}
		return nil
	}

	// Container does not exist — pull image if needed, then create + start.

	// Pull image (pull-on-create would also work, but explicit pull gives a
	// better error message when the image is unavailable).
	log.Info("Pulling image")
	rc, pullErr := r.client.ImagePull(ctx, svc.Image, pullOptions())
	if pullErr != nil {
		return fmt.Errorf("pull image %q: %w", svc.Image, pullErr)
	}
	// Drain and discard the pull progress stream; errors are reflected in the
	// close of the reader.
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()

	// Build env slice: ["KEY=VALUE", ...]
	env := make([]string, len(svc.Env))
	for i, e := range svc.Env {
		env[i] = e.Name + "=" + e.Value
	}

	binds, managedNames, err := r.buildBinds(namespace, projectName, svc, vols)
	if err != nil {
		return err
	}

	labels := map[string]string{
		labelProject:   projectName,
		labelService:   svc.Name,
		labelNamespace: namespace,
	}
	if len(managedNames) > 0 {
		labels[labelVolume] = strings.Join(managedNames, ",")
		labels[labelVolumeType] = string(v1.VolumeTypeManaged)
	}

	netName := NetworkName(projectName)

	resp, err := r.client.ContainerCreate(ctx,
		&container.Config{
			Image:  svc.Image,
			Env:    env,
			Labels: labels,
		},
		&container.HostConfig{
			Binds: binds,
			// No RestartPolicy: let the server decide on failure handling.
		},
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				netName: {
					// Alias the container by its service name so other
					// containers on the same network can reach it by DNS.
					Aliases: []string{svc.Name},
				},
			},
		},
		nil, // platform
		cName,
	)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}

	log.Info("Container created", zap.String("id", resp.ID[:12]))

	if err := r.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start container: %w", err)
	}

	log.Info("Container started")
	return nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// ContainerLogResult holds a log stream from a Docker container together with
// its TTY flag, which the caller needs to decide whether to demultiplex the
// stream (non-TTY containers use Docker's 8-byte header framing).
type ContainerLogResult struct {
	// Reader is the raw log stream.  The caller must close it.
	Reader io.ReadCloser
	// TTY is true when the container was started with a pseudo-terminal.
	// When false the stream is multiplexed (stdout + stderr with 8-byte
	// headers) and must be processed with stdcopy.StdCopy.
	TTY bool
}

// ContainerLogs returns a streaming reader of the container logs for the
// given project/service pair.  It satisfies the narrow ContainerLogger
// interface consumed by the logs handler.
//
// If the container does not exist an error wrapping isNotFound is returned.
// If the container exists but is not running an error is still returned so
// the handler can map it to the appropriate HTTP status.
func (r *DockerRuntime) ContainerLogs(ctx context.Context, project, service string, follow bool, tail string, timestamps bool) (ContainerLogResult, error) {
	cName := ContainerName(project, service)

	// Inspect to verify the container exists and check its TTY flag.
	info, err := r.client.ContainerInspect(ctx, cName)
	if err != nil {
		if isNotFound(err) {
			return ContainerLogResult{}, fmt.Errorf("container %q: %w", cName, err)
		}
		return ContainerLogResult{}, fmt.Errorf("inspect container %q: %w", cName, err)
	}

	if !info.State.Running && follow {
		// For follow mode, the container must be running.
		return ContainerLogResult{}, fmt.Errorf("container %q is not running", cName)
	}

	rc, err := r.client.ContainerLogs(ctx, cName, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     follow,
		Tail:       tail,
		Timestamps: timestamps,
	})
	if err != nil {
		return ContainerLogResult{}, fmt.Errorf("container logs %q: %w", cName, err)
	}

	return ContainerLogResult{Reader: rc, TTY: info.Config.Tty}, nil
}

// InspectContainer returns the running state and bridge-network IP of the
// container for the given project/service pair.  It satisfies the narrow
// ContainerInspector interface consumed by the forward handler.
func (r *DockerRuntime) InspectContainer(ctx context.Context, project, service string) (ContainerInspectResult, error) {
	return r.containerInspectRaw(ctx, ContainerName(project, service))
}

// containerInspectRaw returns the running state and bridge-network IP of a
// single container identified by name.
func (r *DockerRuntime) containerInspectRaw(ctx context.Context, containerName string) (ContainerInspectResult, error) {
	info, err := r.client.ContainerInspect(ctx, containerName)
	if err != nil {
		return ContainerInspectResult{}, fmt.Errorf("inspect container %q: %w", containerName, err)
	}

	// Find the container's IP on any attached network.  Prefer the project
	// bridge network (cara-*) if multiple networks are attached.
	var ip string
	for netName, netInfo := range info.NetworkSettings.Networks {
		if netInfo.IPAddress != "" {
			ip = netInfo.IPAddress
			// Prefer the cara-* network.
			if len(netName) > 5 && netName[:5] == "cara-" {
				break
			}
		}
	}

	return ContainerInspectResult{
		Running:   info.State.Running,
		NetworkIP: ip,
	}, nil
}

// ContainerInspectResult holds the subset of Docker inspect data needed by
// the port-forward handler.
type ContainerInspectResult struct {
	Running   bool
	NetworkIP string
}

// pullOptions returns the options for ImagePull (no auth for now).
func pullOptions() image.PullOptions {
	return image.PullOptions{}
}

// isNotFound reports whether err is a Docker "not found" error.
// The Docker client wraps 404 responses, but the error message isn't always
// caught by IsErrNotFound — check both.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if dockerclient.IsErrNotFound(err) {
		return true
	}
	// Fallback: some Docker API calls return a different error type for 404.
	return strings.Contains(err.Error(), "No such") ||
		strings.Contains(err.Error(), "not found")
}
