package docker

import (
	"context"
	"fmt"
	"maps"
	"slices"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
)

// labelNodeService marks a container the Agent runs for the Node itself, such
// as the pgBackRest repo host. Like a helper it carries no Project label, so
// no Project reconcile, teardown or orphan sweep acts on it.
const labelNodeService = "cara.node-service"

// NodeContainerSpec describes a long-running container the Agent keeps for the
// Node rather than for a Project.
type NodeContainerSpec struct {
	// Name is the container name; it identifies the service on the Node.
	Name  string
	Image string
	User  string
	Cmd   []string
	// Binds are host:container[:ro] mounts.
	Binds []string
	// Network is the container's primary network. It must be private to the
	// service: whatever else is on it can reach the container.
	Network string
	Labels  map[string]string
}

// NodeContainerState is what EnsureNodeContainer found or made.
type NodeContainerState struct {
	ID      string
	Running bool
	// Created reports that the container was created by this call.
	Created bool
}

// EnsureNodeNetwork creates a network of its own for a Node service's primary
// attachment, unless it exists. Nothing else joins it, so the service shares
// no network with any Project; it still has a route out, which a service such
// as the repo host needs to reach an object store outside the Node.
func (r *DockerRuntime) EnsureNodeNetwork(ctx context.Context, name string) error {
	if _, err := r.client.NetworkInspect(ctx, name, network.InspectOptions{}); err == nil {
		return nil
	} else if !isNotFound(err) {
		return fmt.Errorf("docker: inspect network %s: %w", name, err)
	}
	_, err := r.client.NetworkCreate(ctx, name, network.CreateOptions{
		Driver: "bridge",
		Labels: map[string]string{labelNodeService: name},
	})
	if err != nil {
		return fmt.Errorf("docker: create network %s: %w", name, err)
	}
	return nil
}

// EnsureNodeContainer returns the Node service's container, creating it when
// it is missing and recreating it when it runs a different image or was not
// made from this spec's identity. It does not start it.
//
// The container restarts on its own if its process dies (unless-stopped): a
// Node service is infrastructure, unlike a Project's containers, whose
// restarts the Agent decides.
func (r *DockerRuntime) EnsureNodeContainer(ctx context.Context, spec NodeContainerSpec) (NodeContainerState, error) {
	info, err := r.client.ContainerInspect(ctx, spec.Name)
	switch {
	case err == nil:
		if info.Config != nil && info.Config.Image == spec.Image && info.Config.Labels[labelNodeService] == spec.Name {
			return NodeContainerState{ID: info.ID, Running: info.State != nil && info.State.Running}, nil
		}
		if err := r.client.ContainerRemove(ctx, info.ID, container.RemoveOptions{Force: true}); err != nil && !isNotFound(err) {
			return NodeContainerState{}, fmt.Errorf("docker: replace %s: %w", spec.Name, err)
		}
	case !isNotFound(err):
		return NodeContainerState{}, fmt.Errorf("docker: inspect %s: %w", spec.Name, err)
	}

	labels := maps.Clone(spec.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[labelNodeService] = spec.Name
	created, err := r.createPulling(ctx, spec.Image, func() (container.CreateResponse, error) {
		return r.client.ContainerCreate(ctx,
			&container.Config{Image: spec.Image, User: spec.User, Cmd: slices.Clone(spec.Cmd), Labels: labels},
			&container.HostConfig{
				Binds:         slices.Clone(spec.Binds),
				NetworkMode:   container.NetworkMode(spec.Network),
				RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
			},
			nil, nil, spec.Name)
	})
	if err != nil {
		return NodeContainerState{}, fmt.Errorf("docker: create %s: %w", spec.Name, err)
	}
	return NodeContainerState{ID: created.ID, Created: true}, nil
}

// StartContainer starts a container that is not running.
func (r *DockerRuntime) StartContainer(ctx context.Context, nameOrID string) error {
	if err := r.client.ContainerStart(ctx, nameOrID, container.StartOptions{}); err != nil {
		return fmt.Errorf("docker: start %s: %w", nameOrID, err)
	}
	return nil
}

// ContainerRunning reports whether the container exists and is running.
func (r *DockerRuntime) ContainerRunning(ctx context.Context, nameOrID string) (bool, error) {
	info, err := r.client.ContainerInspect(ctx, nameOrID)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("docker: inspect %s: %w", nameOrID, err)
	}
	return info.State != nil && info.State.Running, nil
}

// RemoveContainer removes a container and its anonymous volumes. A container
// already gone is not an error.
func (r *DockerRuntime) RemoveContainer(ctx context.Context, nameOrID string) error {
	err := r.client.ContainerRemove(ctx, nameOrID, container.RemoveOptions{Force: true, RemoveVolumes: true})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("docker: remove %s: %w", nameOrID, err)
	}
	return nil
}

// EnsureProjectNetwork creates the Project's network if it does not exist,
// with the same ownership checks ReconcileProject applies. It lets a Node
// service be attached to the network before the Project's containers start.
func (r *DockerRuntime) EnsureProjectNetwork(ctx context.Context, project *v1.Project) error {
	return r.ensureNetwork(ctx, project.Namespace, project.Name, project.ObjectMeta.UID)
}

// RestartContainer stops and starts one container, for a setting that takes
// effect only at process start.
func (r *DockerRuntime) RestartContainer(ctx context.Context, nameOrID string) error {
	if err := r.client.ContainerStop(ctx, nameOrID, container.StopOptions{}); err != nil {
		return fmt.Errorf("docker: stop %s: %w", nameOrID, err)
	}
	if err := r.client.ContainerStart(ctx, nameOrID, container.StartOptions{}); err != nil {
		return fmt.Errorf("docker: start %s: %w", nameOrID, err)
	}
	return nil
}
