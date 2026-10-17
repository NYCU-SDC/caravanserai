package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/pkg/stdcopy"
)

// ExecResult is what a command run inside a container produced.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// execPollInterval is how often Exec asks Docker whether a command has
// finished once its output stream has closed. The stream closing and the exit
// code becoming available are not simultaneous.
const execPollInterval = 50 * time.Millisecond

// Exec runs cmd inside the container as user and waits for it to finish.
//
// A non-zero exit code is returned in the result, not as an error: callers
// read the code and the output to decide what failed. An error means the
// command could not be run or its outcome could not be determined, which is
// never the same as success.
//
// Cancelling ctx stops waiting but does not stop the command: Docker keeps an
// exec running after its client goes away. A caller that needs the command
// gone must stop it inside the container.
func (r *DockerRuntime) Exec(ctx context.Context, containerID, user string, cmd []string) (ExecResult, error) {
	created, err := r.client.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		User:         user,
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return ExecResult{}, fmt.Errorf("docker: create exec in %s: %w", containerID, err)
	}

	attached, err := r.client.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return ExecResult{}, fmt.Errorf("docker: attach exec in %s: %w", containerID, err)
	}
	defer attached.Close()

	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, attached.Reader); err != nil {
		return ExecResult{}, fmt.Errorf("docker: read exec output in %s: %w", containerID, err)
	}

	for {
		inspected, err := r.client.ContainerExecInspect(ctx, created.ID)
		if err != nil {
			return ExecResult{}, fmt.Errorf("docker: inspect exec in %s: %w", containerID, err)
		}
		if !inspected.Running {
			return ExecResult{ExitCode: inspected.ExitCode, Stdout: stdout.String(), Stderr: stderr.String()}, nil
		}
		select {
		case <-ctx.Done():
			return ExecResult{}, fmt.Errorf("docker: wait for exec in %s: %w", containerID, ctx.Err())
		case <-time.After(execPollInterval):
		}
	}
}

// Signal sends sig (for example "HUP") to the container's main process.
func (r *DockerRuntime) Signal(ctx context.Context, containerID, sig string) error {
	if err := r.client.ContainerKill(ctx, containerID, sig); err != nil {
		return fmt.Errorf("docker: signal %s to %s: %w", sig, containerID, err)
	}
	return nil
}

// ConnectNetwork attaches the container to the network under the given
// aliases. A container already attached is left as it is.
func (r *DockerRuntime) ConnectNetwork(ctx context.Context, networkName, containerID string, aliases []string) error {
	attached, err := r.networkHasContainer(ctx, networkName, containerID)
	if err != nil {
		return err
	}
	if attached {
		return nil
	}
	err = r.client.NetworkConnect(ctx, networkName, containerID, &network.EndpointSettings{Aliases: slices.Clone(aliases)})
	if err != nil {
		return fmt.Errorf("docker: connect %s to network %s: %w", containerID, networkName, err)
	}
	return nil
}

// DisconnectNetwork detaches the container from the network. A container not
// attached, or a network that no longer exists, is not an error.
func (r *DockerRuntime) DisconnectNetwork(ctx context.Context, networkName, containerID string) error {
	attached, err := r.networkHasContainer(ctx, networkName, containerID)
	if err != nil {
		if errors.Is(err, errNetworkAbsent) {
			return nil
		}
		return err
	}
	if !attached {
		return nil
	}
	if err := r.client.NetworkDisconnect(ctx, networkName, containerID, true); err != nil && !isNotFound(err) {
		return fmt.Errorf("docker: disconnect %s from network %s: %w", containerID, networkName, err)
	}
	return nil
}

// NetworkAttached reports whether the container is attached to the network.
// A network that does not exist has nothing attached.
func (r *DockerRuntime) NetworkAttached(ctx context.Context, networkName, containerID string) (bool, error) {
	attached, err := r.networkHasContainer(ctx, networkName, containerID)
	if errors.Is(err, errNetworkAbsent) {
		return false, nil
	}
	return attached, err
}

var errNetworkAbsent = errors.New("docker: network absent")

// networkHasContainer reads the network fresh and reports whether the
// container, by ID or name, is among its endpoints.
func (r *DockerRuntime) networkHasContainer(ctx context.Context, networkName, containerID string) (bool, error) {
	inspected, err := r.client.NetworkInspect(ctx, networkName, network.InspectOptions{})
	if err != nil {
		if isNotFound(err) {
			return false, errNetworkAbsent
		}
		return false, fmt.Errorf("docker: inspect network %s: %w", networkName, err)
	}
	for id, endpoint := range inspected.Containers {
		if id == containerID || endpoint.Name == containerID {
			return true, nil
		}
	}
	return false, nil
}

// RunToCompletion runs cmd in a throwaway container from image, as user, with
// binds mounted, waits for it to exit and removes it. It is for one-off work
// the Agent cannot do as itself, such as handing files to a container user
// when the Agent is not root. The container carries no Project label, so no
// Project reconcile or sweep ever treats it as a workload.
func (r *DockerRuntime) RunToCompletion(ctx context.Context, image, user string, binds, cmd []string) (ExecResult, error) {
	if len(cmd) == 0 {
		return ExecResult{}, errors.New("docker: helper container needs a command")
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return ExecResult{}, fmt.Errorf("docker: helper name: %w", err)
	}
	name := "cara-helper-" + hex.EncodeToString(suffix)

	created, err := r.createPulling(ctx, image, func() (container.CreateResponse, error) {
		return r.client.ContainerCreate(ctx,
			&container.Config{Image: image, User: user, Entrypoint: cmd[:1], Cmd: cmd[1:], Labels: map[string]string{labelHelper: "true"}},
			&container.HostConfig{Binds: slices.Clone(binds), NetworkMode: "none"},
			nil, nil, name)
	})
	if err != nil {
		return ExecResult{}, fmt.Errorf("docker: create helper from %s: %w", image, err)
	}
	// Removed on every path, with a context of its own: a caller that gave up
	// must not leave the helper behind.
	defer func() {
		_ = r.client.ContainerRemove(context.WithoutCancel(ctx), created.ID, container.RemoveOptions{Force: true})
	}()

	waitCh, errCh := r.client.ContainerWait(ctx, created.ID, container.WaitConditionNextExit)
	if err := r.client.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return ExecResult{}, fmt.Errorf("docker: start helper from %s: %w", image, err)
	}
	var code int64
	select {
	case resp := <-waitCh:
		if resp.Error != nil {
			return ExecResult{}, fmt.Errorf("docker: helper from %s: %s", image, resp.Error.Message)
		}
		code = resp.StatusCode
	case err := <-errCh:
		return ExecResult{}, fmt.Errorf("docker: wait for helper from %s: %w", image, err)
	case <-ctx.Done():
		return ExecResult{}, fmt.Errorf("docker: wait for helper from %s: %w", image, ctx.Err())
	}

	logs, err := r.client.ContainerLogs(ctx, created.ID, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return ExecResult{}, fmt.Errorf("docker: read helper output: %w", err)
	}
	defer func() { _ = logs.Close() }()
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, logs); err != nil {
		return ExecResult{}, fmt.Errorf("docker: read helper output: %w", err)
	}
	return ExecResult{ExitCode: int(code), Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

// createPulling runs create, and when it fails because image is not on the
// Node, pulls image and runs it once more. A Node service or helper can be
// needed before any Project container has brought its image to the Node.
func (r *DockerRuntime) createPulling(ctx context.Context, image string, create func() (container.CreateResponse, error)) (container.CreateResponse, error) {
	created, err := create()
	if err == nil || !isNotFound(err) {
		return created, err
	}
	rc, pullErr := r.client.ImagePull(ctx, image, pullOptions())
	if pullErr != nil {
		return created, fmt.Errorf("pull image %q: %w", image, pullErr)
	}
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()
	return create()
}
