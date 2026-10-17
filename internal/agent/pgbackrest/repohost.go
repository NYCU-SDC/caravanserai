package pgbackrest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"NYCU-SDC/caravanserai/internal/agent/docker"

	"go.uber.org/zap"
)

const (
	// RepoContainer is the repo host's container name on every Node.
	RepoContainer = "cara-pgbackrest-repo"
	// RepoNetwork is the repo host's primary network. Nothing else joins it, so
	// the repo host shares no network with any Project and is reachable only
	// through the Project networks it is attached to; it is not internal,
	// because the repo host must reach the object store.
	RepoNetwork = "cara-pgbackrest"

	defaultReadinessTimeout = 30 * time.Second
	readinessPoll           = 500 * time.Millisecond
)

// repoDocker is what the repo host needs from Docker; *docker.DockerRuntime
// satisfies it.
type repoDocker interface {
	helperRunner
	EnsureNodeNetwork(ctx context.Context, name string) error
	EnsureNodeContainer(ctx context.Context, spec docker.NodeContainerSpec) (docker.NodeContainerState, error)
	StartContainer(ctx context.Context, nameOrID string) error
	ContainerRunning(ctx context.Context, nameOrID string) (bool, error)
	RemoveContainer(ctx context.Context, nameOrID string) error
	Signal(ctx context.Context, containerID, sig string) error
	Exec(ctx context.Context, containerID, user string, cmd []string) (docker.ExecResult, error)
	ConnectNetwork(ctx context.Context, networkName, containerID string, aliases []string) error
	DisconnectNetwork(ctx context.Context, networkName, containerID string) error
	NetworkAttached(ctx context.Context, networkName, containerID string) (bool, error)
}

// Config is how the repo host is set up on this Node.
type Config struct {
	DataRoot string
	NodeName string
	S3       S3
	// ReadinessTimeout bounds how long a start or reload may take to answer.
	ReadinessTimeout time.Duration
	// Owner is the user pgBackRest runs as in the image; ImageOwner when
	// zero.
	Owner Owner
	// ContainerName and NetworkName default to RepoContainer and RepoNetwork;
	// tests set them so they cannot collide with a real repo host.
	ContainerName string
	NetworkName   string
}

// repoState is the durable desired state: the authorizations, and the image
// the repo host runs, so a restarted Agent can rebuild it before any Project
// asks for anything.
type repoState struct {
	Image          string         `json:"image"`
	Authorizations authorizations `json:"authorizations"`
}

// RepoHost runs the Node's pgBackRest TLS server and keeps its authorizations.
//
// Every change goes through one lock and regenerates the whole configuration
// from the durable state, so concurrent changes for different Projects cannot
// overwrite each other, and a crash between saving the state and applying it
// is repaired by the next change or Reconcile.
type RepoHost struct {
	mu      sync.Mutex
	docker  repoDocker
	chowner *Chowner
	cfg     Config
	logger  *zap.Logger
	now     func() time.Time
}

// NewRepoHost returns the repo host manager for this Node.
func NewRepoHost(d repoDocker, cfg Config, logger *zap.Logger) *RepoHost {
	if cfg.ReadinessTimeout == 0 {
		cfg.ReadinessTimeout = defaultReadinessTimeout
	}
	if cfg.Owner == (Owner{}) {
		cfg.Owner = ImageOwner
	}
	if cfg.ContainerName == "" {
		cfg.ContainerName = RepoContainer
	}
	if cfg.NetworkName == "" {
		cfg.NetworkName = RepoNetwork
	}
	return &RepoHost{docker: d, cfg: cfg, logger: logger, now: time.Now}
}

func (h *RepoHost) root() string      { return filepath.Join(h.cfg.DataRoot, "pgbackrest") }
func (h *RepoHost) statePath() string { return filepath.Join(h.root(), "repo-host.json") }
func (h *RepoHost) caDir() string     { return filepath.Join(h.root(), "ca") }
func (h *RepoHost) confDir() string   { return filepath.Join(h.root(), "server", "conf") }
func (h *RepoHost) confPath() string  { return filepath.Join(h.confDir(), "pgbackrest.conf") }

// appliedPath keeps the Agent's own copy of the configuration last written.
// The mounted file belongs to the image's user and may not be readable by an
// Agent that is not root, so comparisons and rollbacks read this copy.
func (h *RepoHost) appliedPath() string { return filepath.Join(h.root(), "server", "applied.conf") }
func (h *RepoHost) tlsDir() string      { return filepath.Join(h.root(), "server", "tls") }
func (h *RepoHost) projectDir(uid string) string {
	return filepath.Join(h.root(), "projects", uid)
}

// ClientIdentityDir is where a Project lifetime's client identity lives on the
// Node; the database container mounts it.
func (h *RepoHost) ClientIdentityDir(uid string) string { return h.projectDir(uid) }

func (h *RepoHost) loadState() (repoState, error) {
	data, err := os.ReadFile(h.statePath())
	if err != nil {
		if os.IsNotExist(err) {
			return repoState{Authorizations: authorizations{}}, nil
		}
		return repoState{}, fmt.Errorf("pgbackrest: read repo host state: %w", err)
	}
	var st repoState
	if err := json.Unmarshal(data, &st); err != nil {
		return repoState{}, fmt.Errorf("pgbackrest: parse repo host state: %w", err)
	}
	if st.Authorizations == nil {
		st.Authorizations = authorizations{}
	}
	return st, nil
}

func (h *RepoHost) saveState(st repoState) error {
	if err := os.MkdirAll(h.root(), 0o700); err != nil {
		return fmt.Errorf("pgbackrest: create %s: %w", h.root(), err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("pgbackrest: encode repo host state: %w", err)
	}
	return writeFile(h.statePath(), data, keyFileMode)
}

// IssueClientIdentity makes sure the Project lifetime has a client identity
// owned by the image's user, and returns its directory.
func (h *RepoHost) IssueClientIdentity(ctx context.Context, uid, image string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ca, err := LoadOrCreateCA(h.caDir(), h.cfg.NodeName, h.now())
	if err != nil {
		return "", err
	}
	dir := h.projectDir(uid)
	files, err := ca.IssueClient(dir, uid, h.now())
	if err != nil {
		return "", err
	}
	if err := NewChowner(h.docker, image).Give(ctx, h.cfg.Owner, files...); err != nil {
		return "", err
	}
	return dir, nil
}

// Authorize lets the Project lifetime uid use its stanzas through the repo
// host, reached over network, starting the repo host from image if needed.
// It returns once the server answers with the new configuration.
func (h *RepoHost) Authorize(ctx context.Context, uid string, a Authorization, image string) error {
	if uid == "" || a.Network == "" || len(a.Stanzas) == 0 {
		return errors.New("pgbackrest: an authorization needs a UID, a network and at least one stanza")
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	st, err := h.loadState()
	if err != nil {
		return err
	}
	st.Authorizations[uid] = a
	if image != "" {
		st.Image = image
	}
	// Rendering first refuses an object store pgBackRest cannot use before
	// anything is recorded.
	if _, err := renderServerConfig(st.Authorizations, h.cfg.S3); err != nil {
		return err
	}
	if err := h.saveState(st); err != nil {
		return err
	}
	return h.apply(ctx, st)
}

// Revoke removes the Project lifetime's authorization and detaches the repo
// host from its network. Revoking what is not authorized is not an error.
func (h *RepoHost) Revoke(ctx context.Context, uid, network string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	st, err := h.loadState()
	if err != nil {
		return err
	}
	if a, ok := st.Authorizations[uid]; ok {
		if network == "" {
			network = a.Network
		}
		delete(st.Authorizations, uid)
		if err := h.saveState(st); err != nil {
			return err
		}
		running, err := h.docker.ContainerRunning(ctx, h.cfg.ContainerName)
		if err != nil {
			return err
		}
		if running {
			if err := h.apply(ctx, st); err != nil {
				return err
			}
		}
	}
	if network != "" && !h.networkInUse(st, network) {
		if err := h.docker.DisconnectNetwork(ctx, network, h.cfg.ContainerName); err != nil {
			return err
		}
	}
	return nil
}

// networkInUse reports whether another authorization still reaches the repo
// host over network.
func (h *RepoHost) networkInUse(st repoState, network string) bool {
	for _, a := range st.Authorizations {
		if a.Network == network {
			return true
		}
	}
	return false
}

// Remaining describes what the repo host still holds for the Project lifetime
// uid on network: its authorization, or an attachment to the network.
func (h *RepoHost) Remaining(ctx context.Context, uid, network string) ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, err := h.loadState()
	if err != nil {
		return nil, err
	}
	var left []string
	if _, ok := st.Authorizations[uid]; ok {
		left = append(left, "pgbackrest authorization "+uid)
	}
	if network != "" && !h.networkInUse(st, network) {
		attached, err := h.docker.NetworkAttached(ctx, network, h.cfg.ContainerName)
		if err != nil {
			return nil, err
		}
		if attached {
			left = append(left, "pgbackrest repo host on network "+network)
		}
	}
	return left, nil
}

// Reconcile rebuilds the repo host from the durable state, for an Agent that
// restarted while Projects were authorized.
func (h *RepoHost) Reconcile(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, err := h.loadState()
	if err != nil {
		return err
	}
	if len(st.Authorizations) == 0 || st.Image == "" {
		return nil
	}
	return h.apply(ctx, st)
}

// Version returns the pgBackRest version the repo host runs.
func (h *RepoHost) Version(ctx context.Context) (string, error) {
	res, err := h.docker.Exec(ctx, h.cfg.ContainerName, h.user(), []string{"pgbackrest", "version"})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("pgbackrest: version exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return strings.TrimSpace(res.Stdout), nil
}

func (h *RepoHost) user() string { return fmt.Sprintf("%d:%d", h.cfg.Owner.UID, h.cfg.Owner.GID) }

// apply makes the repo host match st. Called with h.mu held.
func (h *RepoHost) apply(ctx context.Context, st repoState) error {
	if st.Image == "" {
		return errors.New("pgbackrest: no image to run the repo host from")
	}
	desired, err := renderServerConfig(st.Authorizations, h.cfg.S3)
	if err != nil {
		return err
	}
	if err := h.prepareServerFiles(ctx, st.Image); err != nil {
		return err
	}
	if err := h.docker.EnsureNodeNetwork(ctx, h.cfg.NetworkName); err != nil {
		return err
	}

	previous, err := os.ReadFile(h.appliedPath())
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("pgbackrest: read repo host configuration: %w", err)
	}
	changed := !bytes.Equal(previous, desired)
	if changed {
		if err := h.writeConfig(ctx, desired, st.Image); err != nil {
			return err
		}
	}

	state, err := h.docker.EnsureNodeContainer(ctx, h.containerSpec(st.Image))
	if err != nil {
		return err
	}
	switch {
	case !state.Running:
		if err := h.docker.StartContainer(ctx, h.cfg.ContainerName); err != nil {
			return err
		}
	case changed:
		if err := h.docker.Signal(ctx, h.cfg.ContainerName, "HUP"); err != nil {
			return err
		}
	}

	if err := h.waitReady(ctx); err != nil {
		h.logger.Warn("Repo host not ready after configuration change; restoring the previous configuration", zap.Error(err))
		if rerr := h.recover(ctx, previous, st.Image); rerr != nil {
			return fmt.Errorf("pgbackrest: repo host not ready (%v), and restoring it failed: %w", err, rerr)
		}
		return fmt.Errorf("pgbackrest: repo host rejected the new configuration and was restored: %w", err)
	}

	for _, uid := range sortedUIDs(st.Authorizations) {
		if err := h.docker.ConnectNetwork(ctx, st.Authorizations[uid].Network, h.cfg.ContainerName, []string{RepoAlias}); err != nil {
			return err
		}
	}
	return nil
}

// recover puts the previous configuration back and gets the server answering
// again, recreating the container as a last resort.
func (h *RepoHost) recover(ctx context.Context, previous []byte, image string) error {
	if len(previous) > 0 {
		if err := h.writeConfig(ctx, previous, image); err != nil {
			return err
		}
	}
	running, err := h.docker.ContainerRunning(ctx, h.cfg.ContainerName)
	if err != nil {
		return err
	}
	if running {
		err = h.docker.Signal(ctx, h.cfg.ContainerName, "HUP")
	} else {
		err = h.docker.StartContainer(ctx, h.cfg.ContainerName)
	}
	if err == nil && h.waitReady(ctx) == nil {
		return nil
	}
	if err := h.docker.RemoveContainer(ctx, h.cfg.ContainerName); err != nil {
		return err
	}
	if _, err := h.docker.EnsureNodeContainer(ctx, h.containerSpec(image)); err != nil {
		return err
	}
	if err := h.docker.StartContainer(ctx, h.cfg.ContainerName); err != nil {
		return err
	}
	return h.waitReady(ctx)
}

func (h *RepoHost) containerSpec(image string) docker.NodeContainerSpec {
	return docker.NodeContainerSpec{
		Name:  h.cfg.ContainerName,
		Image: image,
		User:  h.user(),
		Cmd:   []string{"pgbackrest", "server"},
		Binds: []string{
			h.confDir() + ":/etc/pgbackrest:ro",
			h.tlsDir() + ":" + serverTLSDir + ":ro",
		},
		Network: h.cfg.NetworkName,
	}
}

// prepareServerFiles makes sure the repo host's TLS identity and the object
// store's CA are in place and belong to the image's user.
func (h *RepoHost) prepareServerFiles(ctx context.Context, image string) error {
	ca, err := LoadOrCreateCA(h.caDir(), h.cfg.NodeName, h.now())
	if err != nil {
		return err
	}
	files, err := ca.IssueServer(h.tlsDir(), h.cfg.NodeName, h.now())
	if err != nil {
		return err
	}
	if h.cfg.S3.CAFile != "" {
		caPEM, err := os.ReadFile(h.cfg.S3.CAFile)
		if err != nil {
			return fmt.Errorf("pgbackrest: read object store CA: %w", err)
		}
		path := filepath.Join(h.tlsDir(), s3CAName)
		if err := writeFile(path, caPEM, certFileMode); err != nil {
			return err
		}
		files = append(files, path)
	}
	return NewChowner(h.docker, image).Give(ctx, h.cfg.Owner, files...)
}

// writeConfig replaces the configuration file. The directory, not the file, is
// mounted, so the replacement is what the running server reads on reload. It
// holds the object store's secret key, so only the image's user may read it.
func (h *RepoHost) writeConfig(ctx context.Context, data []byte, image string) error {
	if err := os.MkdirAll(h.confDir(), identityDirMode); err != nil {
		return fmt.Errorf("pgbackrest: create %s: %w", h.confDir(), err)
	}
	if err := writeFile(h.confPath(), data, keyFileMode); err != nil {
		return err
	}
	if err := NewChowner(h.docker, image).Give(ctx, h.cfg.Owner, h.confPath()); err != nil {
		return err
	}
	return writeFile(h.appliedPath(), data, keyFileMode)
}

// waitReady waits until the server answers a ping.
func (h *RepoHost) waitReady(ctx context.Context) error {
	deadline := h.now().Add(h.cfg.ReadinessTimeout)
	var last error
	for {
		res, err := h.docker.Exec(ctx, h.cfg.ContainerName, h.user(), []string{"pgbackrest", "server-ping", "127.0.0.1"})
		switch {
		case err != nil:
			last = err
		case res.ExitCode != 0:
			last = fmt.Errorf("server-ping exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
		default:
			return nil
		}
		if h.now().After(deadline) {
			return fmt.Errorf("pgbackrest: repo host did not answer within %s: %w", h.cfg.ReadinessTimeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readinessPoll):
		}
	}
}

func sortedUIDs(a authorizations) []string {
	uids := make([]string, 0, len(a))
	for uid := range a {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}
