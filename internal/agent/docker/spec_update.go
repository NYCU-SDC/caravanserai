package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"go.uber.org/zap"
)

// labelSpecHash records, on every service container, the hash of the service
// configuration it was built from (ADR-0026). A container of the current
// assignment whose label differs from the spec's hash is outdated and is
// recreated; one without the label is outdated too.
const labelSpecHash = "cara.spec-hash"

// ErrImagePull is wrapped by an update that could not pull an image of the new
// spec. Nothing was stopped: the old containers are still running.
var ErrImagePull = errors.New("pull image")

// serviceHashInput is what a service container is built from. It is encoded
// as JSON with omitempty, so a field added later changes no existing hash
// until a service sets it. Changing this type or its encoding changes every
// hash, and every container is recreated once.
type serviceHashInput struct {
	Image   string         `json:"image"`
	Command []string       `json:"command,omitempty"`
	Env     []envHashInput `json:"env,omitempty"`
	Mounts  []mountHash    `json:"volumeMounts,omitempty"`
}

// envHashInput names a Secret by reference, never by value: a value's hash in
// a container label could be guessed offline when the value is weak.
type envHashInput struct {
	Name      string                `json:"name"`
	Value     string                `json:"value,omitempty"`
	SecretRef *v1.SecretKeySelector `json:"secretKeyRef,omitempty"`
}

// mountHash includes the volume's type because the bind source and its
// supporting mounts are chosen by type.
type mountHash struct {
	Name      string        `json:"name"`
	MountPath string        `json:"mountPath"`
	Type      v1.VolumeType `json:"type"`
}

// ServiceSpecHash returns the hash of what svc's container is built from.
//
// An env var that comes from a Secret contributes its reference only. The
// Agent's resolved copy of a Project keeps that reference next to the value
// it fetched, so the hash of the resolved and the declared spec are the same.
// A rotated Secret therefore changes no hash; `rollout restart` is how a new
// value is picked up.
func ServiceSpecHash(svc v1.ServiceDef, spec v1.ProjectSpec) string {
	types := make(map[string]v1.VolumeType, len(spec.Volumes))
	for _, vol := range spec.Volumes {
		types[vol.Name] = vol.Type
	}
	in := serviceHashInput{Image: svc.Image, Command: svc.Command}
	for _, e := range svc.Env {
		h := envHashInput{Name: e.Name}
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			ref := *e.ValueFrom.SecretKeyRef
			h.SecretRef = &ref
		} else {
			h.Value = e.Value
		}
		in.Env = append(in.Env, h)
	}
	for _, m := range svc.VolumeMounts {
		in.Mounts = append(in.Mounts, mountHash{Name: m.Name, MountPath: m.MountPath, Type: types[m.Name]})
	}
	raw, err := json.Marshal(in)
	if err != nil {
		// Only strings and slices of them: Marshal cannot fail.
		panic(fmt.Sprintf("docker: encode service hash input: %v", err))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// SpecDrift is how a Project's containers on this Node differ from its spec.
// It only describes containers of the current assignment; a container of
// another assignment is StaleContainers' business.
type SpecDrift struct {
	// Outdated lists services, in spec order, whose container exists but was
	// built from another configuration.
	Outdated []string
	// Missing lists services, in spec order, that have no container.
	Missing []string
	// Removed lists containers of this assignment whose service is no longer
	// in the spec.
	Removed []StaleContainer
}

// Changes reports whether an update would stop or remove anything: outdated
// containers or containers of removed services. Missing containers alone are
// not a change, since a crash leaves them missing too.
func (d SpecDrift) Changes() bool { return len(d.Outdated) > 0 || len(d.Removed) > 0 }

// SpecDrift implements Runtime.
func (r *DockerRuntime) SpecDrift(ctx context.Context, project *v1.Project) (SpecDrift, error) {
	owner := ownerOf(project)
	var drift SpecDrift
	listed := make(map[string]bool, len(project.Spec.Services))
	for _, svc := range project.Spec.Services {
		listed[svc.Name] = true
		name := ContainerName(project.Name, svc.Name)
		info, err := r.client.ContainerInspect(ctx, name)
		if err != nil {
			if isNotFound(err) {
				drift.Missing = append(drift.Missing, svc.Name)
				continue
			}
			return SpecDrift{}, fmt.Errorf("inspect %q: %w", name, err)
		}
		if err := r.checkContainerOwnership(name, info, owner, svc.Name); err != nil {
			return SpecDrift{}, err
		}
		if labelsOf(info)[labelSpecHash] != ServiceSpecHash(svc, project.Spec) {
			drift.Outdated = append(drift.Outdated, svc.Name)
		}
	}

	removed, err := r.unlistedContainers(ctx, project, listed)
	if err != nil {
		return SpecDrift{}, err
	}
	drift.Removed = removed
	return drift, nil
}

// unlistedContainers finds the containers of project's current assignment
// whose service is not in listed. Only containers this assignment owns are
// returned, checked by the same labels every other path uses: one of another
// assignment under an old service name is left for whoever owns it.
func (r *DockerRuntime) unlistedContainers(ctx context.Context, project *v1.Project, listed map[string]bool) ([]StaleContainer, error) {
	f := containerOwnershipFilters(ProjectIdentity{Namespace: project.Namespace, Name: project.Name, UID: project.ObjectMeta.UID})
	containers, err := r.client.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, fmt.Errorf("list project containers: %w", err)
	}
	owner := ownerOf(project)
	var out []StaleContainer
	for _, c := range containers {
		service := c.Labels[labelService]
		if listed[service] {
			continue
		}
		name := strings.TrimPrefix(firstName(c.Names), "/")
		if r.checkOwnershipLabels(name, c.Labels, owner, service) != nil {
			continue
		}
		out = append(out, StaleContainer{Name: name, Service: service, ID: c.ID})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// UpdateServices implements Runtime.
//
// The order is what ADR-0026 asks for. Every image the update needs is pulled
// first, while the old containers still run, so an image that does not exist
// leaves the Project as it was. Then the outdated containers stop and go in
// reverse spec order, with the containers of removed services, and the new
// ones are created in spec order. Each container is checked against the
// assignment again right before it is removed.
func (r *DockerRuntime) UpdateServices(ctx context.Context, project *v1.Project, drift SpecDrift) error {
	log := r.logger.With(zap.String("project", project.Name))
	services := make(map[string]v1.ServiceDef, len(project.Spec.Services))
	for _, svc := range project.Spec.Services {
		services[svc.Name] = svc
	}

	build := slices.Concat(drift.Outdated, drift.Missing)
	for _, name := range build {
		svc, ok := services[name]
		if !ok {
			return fmt.Errorf("service %q is not in the spec", name)
		}
		if err := r.pullImage(ctx, svc.Image); err != nil {
			return fmt.Errorf("service %q: %w", name, err)
		}
	}

	owner := ownerOf(project)
	for i := len(project.Spec.Services) - 1; i >= 0; i-- {
		svc := project.Spec.Services[i]
		if !slices.Contains(drift.Outdated, svc.Name) {
			continue
		}
		if err := r.removeOwnedContainer(ctx, ContainerName(project.Name, svc.Name), owner, svc.Name); err != nil {
			return err
		}
		log.Info("Outdated container removed", zap.String("service", svc.Name))
	}
	for _, c := range drift.Removed {
		if err := r.removeOwnedContainer(ctx, c.Name, owner, c.Service); err != nil {
			return err
		}
		log.Info("Container of a removed service removed", zap.String("service", c.Service))
	}

	if err := r.ensureNetwork(ctx, project.Namespace, project.Name, project.ObjectMeta.UID); err != nil {
		return fmt.Errorf("ensure network: %w", err)
	}
	if err := r.ensureVolumes(ctx, ProjectIdentity{Namespace: project.Namespace, Name: project.Name, UID: project.ObjectMeta.UID}, project.Spec.Volumes); err != nil {
		return fmt.Errorf("ensure volumes: %w", err)
	}
	for _, svc := range project.Spec.Services {
		if !slices.Contains(build, svc.Name) {
			continue
		}
		if _, err := r.ensureContainerPulled(ctx, project.Namespace, project.Name, project.ObjectMeta.UID, project.Status.AssignmentGeneration, svc, project.Spec, true); err != nil {
			return fmt.Errorf("ensure container %q: %w", svc.Name, err)
		}
	}
	return nil
}

// removeOwnedContainer stops and removes one container after checking, on a
// fresh inspect, that it still belongs to owner. A container already gone is
// fine: a restart mid-update leaves exactly that.
func (r *DockerRuntime) removeOwnedContainer(ctx context.Context, name string, owner containerOwner, service string) error {
	info, err := r.client.ContainerInspect(ctx, name)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("inspect %q: %w", name, err)
	}
	if err := r.checkContainerOwnership(name, info, owner, service); err != nil {
		return err
	}
	if err := r.client.ContainerStop(ctx, info.ID, container.StopOptions{}); err != nil && !isNotFound(err) {
		return fmt.Errorf("stop %q: %w", name, err)
	}
	if err := r.client.ContainerRemove(ctx, info.ID, container.RemoveOptions{}); err != nil && !isNotFound(err) {
		return fmt.Errorf("remove %q: %w", name, err)
	}
	return nil
}

// pullImage pulls ref and waits for the pull to finish. An error wraps
// ErrImagePull.
func (r *DockerRuntime) pullImage(ctx context.Context, ref string) error {
	rc, err := r.client.ImagePull(ctx, ref, pullOptions())
	if err != nil {
		return fmt.Errorf("%w %q: %v", ErrImagePull, ref, err)
	}
	defer func() { _ = rc.Close() }()
	if err := drainPull(rc); err != nil {
		return fmt.Errorf("%w %q: %v", ErrImagePull, ref, err)
	}
	return nil
}

// drainPull reads a pull's progress stream to the end and returns the error
// the stream reports, if any. Docker answers a pull of a tag that does not
// exist with a stream that carries the error rather than with a failed call.
func drainPull(rc io.Reader) error {
	dec := json.NewDecoder(rc)
	for {
		var msg struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			// Not JSON: nothing to read an error from.
			_, _ = io.Copy(io.Discard, rc)
			return nil
		}
		if msg.Error != "" {
			return errors.New(msg.Error)
		}
		if msg.ErrorDetail.Message != "" {
			return errors.New(msg.ErrorDetail.Message)
		}
	}
}

// labelsOf returns the labels of an inspected container, or nil.
func labelsOf(info types.ContainerJSON) map[string]string {
	if info.Config == nil {
		return nil
	}
	return info.Config.Labels
}
