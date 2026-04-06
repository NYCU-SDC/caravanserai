package docker

import (
	"context"
	"fmt"
	"io"
	"strings"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	dockerclient "github.com/docker/docker/client"
	"go.uber.org/zap"
)

const (
	// labelProject is attached to every container/network/volume created by
	// the agent so that RemoveProject can find them reliably.
	labelProject = "cara.project"
	// labelService tags each container with its ServiceDef name.
	labelService = "cara.service"
)

// DockerRuntime is the production implementation of Runtime backed by the
// Docker Engine API.
type DockerRuntime struct {
	client *dockerclient.Client
	logger *zap.Logger
}

// NewDockerRuntime creates a DockerRuntime connected to the Docker daemon at
// host (e.g. "unix:///var/run/docker.sock" or "tcp://127.0.0.1:2375").
// WithAPIVersionNegotiation is always enabled so the client works with a range
// of Docker daemon versions.
func NewDockerRuntime(host string, logger *zap.Logger) (*DockerRuntime, error) {
	c, err := dockerclient.NewClientWithOpts(
		dockerclient.WithHost(host),
		dockerclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("docker: create client: %w", err)
	}
	return &DockerRuntime{client: c, logger: logger}, nil
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

	// 2. Ensure all Ephemeral volumes exist.
	if err := r.ensureVolumes(ctx, project.Name, project.Spec.Volumes); err != nil {
		r.rollback(ctx, project, log)
		return fmt.Errorf("ensure volumes: %w", err)
	}

	// 3. Ensure every service container exists and is running.
	for _, svc := range project.Spec.Services {
		if err := r.ensureContainer(ctx, project.Name, svc); err != nil {
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
	if err := r.RemoveProject(ctx, project.Name, project.Spec); err != nil {
		log.Error("Rollback failed, resources may leak",
			zap.Error(err))
	}
}

// RemoveProject implements Runtime.
func (r *DockerRuntime) RemoveProject(ctx context.Context, projectName string, spec v1.ProjectSpec) error {
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
		timeout := 10 // seconds
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

	// Remove Ephemeral volumes.
	for _, vol := range spec.Volumes {
		if vol.Type != v1.VolumeTypeEphemeral {
			continue
		}
		vName := VolumeName(projectName, vol.Name)
		if err := r.client.VolumeRemove(ctx, vName, false); err != nil {
			if !isNotFound(err) {
				log.Warn("Failed to remove volume", zap.String("volume", vName), zap.Error(err))
			}
		}
	}

	log.Info("Project resources removed")
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

// ensureVolumes creates any Ephemeral volumes that do not yet exist.
func (r *DockerRuntime) ensureVolumes(ctx context.Context, projectName string, vols []v1.VolumeDef) error {
	for _, vol := range vols {
		if vol.Type != v1.VolumeTypeEphemeral {
			r.logger.Warn("Unsupported volume type, skipping",
				zap.String("volume", vol.Name), zap.String("type", string(vol.Type)))
			continue
		}

		vName := VolumeName(projectName, vol.Name)
		_, err := r.client.VolumeInspect(ctx, vName)
		if err == nil {
			r.logger.Debug("Volume already exists", zap.String("volume", vName))
			continue
		}
		if !isNotFound(err) {
			return fmt.Errorf("inspect volume %q: %w", vName, err)
		}

		if _, err := r.client.VolumeCreate(ctx, volume.CreateOptions{
			Name:   vName,
			Labels: map[string]string{labelProject: projectName},
		}); err != nil {
			return fmt.Errorf("create volume %q: %w", vName, err)
		}
		r.logger.Info("Volume created", zap.String("volume", vName))
	}
	return nil
}

// ensureContainer creates and starts the container for a single service if it
// is not already running.
func (r *DockerRuntime) ensureContainer(ctx context.Context, projectName string, svc v1.ServiceDef) error {
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

	// Build bind-mount slice: ["cara-proj-volname:/mountpath", ...]
	var binds []string
	for _, vm := range svc.VolumeMounts {
		vName := VolumeName(projectName, vm.Name)
		binds = append(binds, vName+":"+vm.MountPath)
	}

	netName := NetworkName(projectName)

	resp, err := r.client.ContainerCreate(ctx,
		&container.Config{
			Image: svc.Image,
			Env:   env,
			Labels: map[string]string{
				labelProject: projectName,
				labelService: svc.Name,
			},
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
