package docker

import (
	"context"
	"errors"
	"fmt"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"go.uber.org/zap"
)

// Assignment names one assignment of one Project lifetime, as the Docker
// resources created for it are labelled.
//
// AllGenerations widens it to every assignment of the lifetime this daemon
// holds anything for, and Generation is then ignored. Deleting a Project uses
// it: whichever generation left a container here, it goes with the Project —
// but a same-name Project of another lifetime is never touched.
type Assignment struct {
	Namespace      string
	Project        string
	UID            string
	Generation     int64
	AllGenerations bool
}

func (a Assignment) String() string {
	if a.AllGenerations {
		return fmt.Sprintf("%s/%s (uid %s, every generation)", a.Namespace, a.Project, a.UID)
	}
	return fmt.Sprintf("%s/%s (uid %s, generation %d)", a.Namespace, a.Project, a.UID, a.Generation)
}

// RemoveAssignment removes the Docker resources this daemon holds for exactly
// one assignment: the containers labelled with its UID and generation, and —
// once no container of the same lifetime is left, of any generation — the
// lifetime's network and Ephemeral volumes. Managed volume data lives in host
// directories, not Docker volumes, and is never touched.
//
// The network and volumes carry a UID but no generation, so they belong to
// every assignment of the lifetime this daemon still holds containers for.
// Removing them while another generation's containers remain would take them
// from that generation; leaving them is what lets the two obligations be
// resolved independently, in either order.
//
// Ownership is always matched by UID: this removes resources on the strength of a cleanup obligation for
// one lifetime, and a same-name resource from any other lifetime is not that
// obligation's to remove. It is idempotent; anything already gone is not an
// error.
func (r *DockerRuntime) RemoveAssignment(ctx context.Context, a Assignment) error {
	if a.UID == "" {
		return fmt.Errorf("remove assignment %s: no UID to match resources by", a)
	}
	log := r.logger.With(zap.String("namespace", a.Namespace), zap.String("project", a.Project),
		zap.String("assignment", a.String()))

	containers, err := r.client.ContainerList(ctx, container.ListOptions{All: true, Filters: assignmentContainerFilters(a)})
	if err != nil {
		return fmt.Errorf("list containers of %s: %w", a, err)
	}
	var errs []error
	for _, c := range containers {
		timeout := stopTimeoutSeconds
		if err := r.client.ContainerStop(ctx, c.ID, container.StopOptions{Timeout: &timeout}); err != nil && !isNotFound(err) {
			errs = append(errs, fmt.Errorf("stop container %s: %w", shortID(c.ID), err))
		}
		if err := r.client.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err != nil && !isNotFound(err) {
			errs = append(errs, fmt.Errorf("remove container %s: %w", shortID(c.ID), err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("remove assignment %s: %w", a, err)
	}

	others, err := r.lifetimeHasContainers(ctx, a)
	if err != nil {
		return fmt.Errorf("remove assignment %s: %w", a, err)
	}
	if others {
		log.Info("Leaving the lifetime's network and volumes: another generation still has containers here")
		return nil
	}

	if net, ok, err := r.lifetimeNetwork(ctx, a); err != nil {
		errs = append(errs, err)
	} else if ok {
		if err := r.client.NetworkRemove(ctx, net.ID); err != nil && !isNotFound(err) {
			errs = append(errs, fmt.Errorf("remove network %q: %w", net.Name, err))
		}
	}

	vols, err := r.lifetimeVolumes(ctx, a)
	if err != nil {
		errs = append(errs, err)
	}
	for _, v := range vols {
		if err := r.client.VolumeRemove(ctx, v.Name, false); err != nil && !isNotFound(err) {
			errs = append(errs, fmt.Errorf("remove volume %s: %w", v.Name, err))
		}
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("remove assignment %s: %w", a, err)
	}
	log.Info("Removed the assignment's Docker resources")
	return nil
}

// AssignmentRemaining lists what this daemon still holds for the assignment,
// by the same rule RemoveAssignment removes by. An empty result is the
// evidence ResourcesRemoved may be reported on; it is read fresh rather than
// inferred from a removal having returned no error.
func (r *DockerRuntime) AssignmentRemaining(ctx context.Context, a Assignment) ([]string, error) {
	if a.UID == "" {
		return nil, fmt.Errorf("inspect assignment %s: no UID to match resources by", a)
	}
	containers, err := r.client.ContainerList(ctx, container.ListOptions{All: true, Filters: assignmentContainerFilters(a)})
	if err != nil {
		return nil, fmt.Errorf("list containers of %s: %w", a, err)
	}
	var remaining []string
	for _, c := range containers {
		remaining = append(remaining, "container "+firstName(c.Names))
	}

	others, err := r.lifetimeHasContainers(ctx, a)
	if err != nil {
		return nil, err
	}
	if others {
		// Shared with a generation that still has containers here; not this
		// assignment's to account for.
		return remaining, nil
	}
	net, ok, err := r.lifetimeNetwork(ctx, a)
	if err != nil {
		return nil, err
	}
	if ok {
		remaining = append(remaining, "network "+net.Name)
	}
	vols, err := r.lifetimeVolumes(ctx, a)
	if err != nil {
		return nil, err
	}
	for _, v := range vols {
		remaining = append(remaining, "volume "+v.Name)
	}
	return remaining, nil
}

func assignmentContainerFilters(a Assignment) filters.Args {
	args := containerOwnershipFilters(ProjectIdentity{Namespace: a.Namespace, Name: a.Project, UID: a.UID})
	if !a.AllGenerations {
		args.Add("label", labelGeneration+"="+generationLabel(a.Generation))
	}
	return args
}

// lifetimeHasContainers reports whether any container of a's lifetime, of a
// generation other than a's, is on this daemon. With AllGenerations there is
// no other generation: every container of the lifetime is a's own.
func (r *DockerRuntime) lifetimeHasContainers(ctx context.Context, a Assignment) (bool, error) {
	if a.AllGenerations {
		return false, nil
	}
	containers, err := r.client.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: containerOwnershipFilters(ProjectIdentity{Namespace: a.Namespace, Name: a.Project, UID: a.UID}),
	})
	if err != nil {
		return false, fmt.Errorf("list containers of lifetime %s: %w", a.UID, err)
	}
	want := generationLabel(a.Generation)
	for _, c := range containers {
		if c.Labels[labelGeneration] != want {
			return true, nil
		}
	}
	return false, nil
}

// lifetimeNetwork returns the Project's network if it carries a's UID. A
// same-named network with another UID, or none, belongs to another lifetime.
func (r *DockerRuntime) lifetimeNetwork(ctx context.Context, a Assignment) (network.Inspect, bool, error) {
	name := NetworkName(a.Project)
	net, err := r.client.NetworkInspect(ctx, name, network.InspectOptions{})
	if err != nil {
		if isNotFound(err) {
			return network.Inspect{}, false, nil
		}
		return network.Inspect{}, false, fmt.Errorf("inspect network %q: %w", name, err)
	}
	if net.Labels[labelProject] != a.Project || net.Labels[labelUID] != a.UID {
		return network.Inspect{}, false, nil
	}
	if ns := net.Labels[labelNamespace]; ns != "" && ns != a.Namespace {
		return network.Inspect{}, false, nil
	}
	if net.Name == "" {
		net.Name = name
	}
	return net, true, nil
}

func (r *DockerRuntime) lifetimeVolumes(ctx context.Context, a Assignment) ([]*volume.Volume, error) {
	vols, err := r.client.VolumeList(ctx, volume.ListOptions{
		Filters: resourceOwnershipFilters(ProjectIdentity{Namespace: a.Namespace, Name: a.Project, UID: a.UID}),
	})
	if err != nil {
		return nil, fmt.Errorf("list volumes of lifetime %s: %w", a.UID, err)
	}
	return vols.Volumes, nil
}
