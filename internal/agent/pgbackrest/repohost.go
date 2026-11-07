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
	caravolume "NYCU-SDC/caravanserai/internal/agent/volume"

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

// appliedPath keeps the Agent's own copy of the configuration the repo host is
// confirmed to serve. It is written only once a reload was delivered and the
// server answers again, or while no server runs, so whatever starts one next
// reads the configuration file. A configuration written but not confirmed
// (a failed reload, an Agent killed in between) leaves this copy behind, and
// the next apply reloads again. The mounted file belongs to the image's user
// and may not be readable by an Agent that is not root, so comparisons,
// rollbacks and Remaining read this copy.
func (h *RepoHost) appliedPath() string { return filepath.Join(h.root(), "server", "applied.conf") }
func (h *RepoHost) tlsDir() string      { return filepath.Join(h.root(), "server", "tls") }

// writtenPath keeps the Agent's copy of the configuration file as last
// written, which may be ahead of the confirmed one.
func (h *RepoHost) writtenPath() string { return filepath.Join(h.root(), "server", "written.conf") }

// clientDirs are where a Project lifetime's client configuration and identity
// live on the Node; the database container mounts both.
func (h *RepoHost) clientDirs(uid string) (confDir, tlsDir string, err error) {
	return caravolume.PgBackRestClientDirs(h.cfg.DataRoot, uid)
}

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
	_, dir, err := h.clientDirs(uid)
	if err != nil {
		return "", err
	}
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
	}
	// Applied on every call, not only the one that removed the record: a
	// reload that failed was rolled back to a configuration that still
	// authorizes uid, and a retry must not find the record gone and stop.
	if err := h.applyWithout(ctx, st, uid); err != nil {
		return err
	}
	if network != "" && !h.networkInUse(st, network) {
		if err := h.docker.DisconnectNetwork(ctx, network, h.cfg.ContainerName); err != nil {
			return err
		}
	}
	return nil
}

// applyWithout makes the configuration the repo host serves match st, which no
// longer authorizes uid. A running server is reloaded and checked; a stopped
// one only has its configuration file rewritten, so that whatever starts it
// next cannot serve uid. Called with h.mu held.
func (h *RepoHost) applyWithout(ctx context.Context, st repoState, uid string) error {
	served, err := h.servesUID(uid)
	if err != nil || !served {
		return err
	}
	running, err := h.docker.ContainerRunning(ctx, h.cfg.ContainerName)
	if err != nil {
		return err
	}
	if running {
		return h.apply(ctx, st)
	}
	desired, err := renderServerConfig(st.Authorizations, h.cfg.S3)
	if err != nil {
		return err
	}
	if err := h.writeConfig(ctx, desired, st.Image); err != nil {
		return err
	}
	// No server runs, so none serves the old one; the next to start reads
	// this file.
	return h.confirmApplied(desired)
}

// servesUID reports whether the repo host may serve uid: the configuration
// confirmed served authorizes it, or the file written for the next reload or
// start does.
func (h *RepoHost) servesUID(uid string) (bool, error) {
	for _, path := range []string{h.appliedPath(), h.writtenPath()} {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("pgbackrest: read repo host configuration: %w", err)
		}
		if bytes.Contains(data, []byte("\ntls-server-auth="+uid+"=")) {
			return true, nil
		}
	}
	return false, nil
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
	} else if served, err := h.servesUID(uid); err != nil {
		return nil, err
	} else if served {
		left = append(left, "pgbackrest authorization "+uid+" in the repo host configuration")
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
	written, err := os.ReadFile(h.writtenPath())
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("pgbackrest: read repo host configuration: %w", err)
	}
	// Reloaded when what is confirmed served differs, so a write whose reload
	// failed is reloaded again; and whenever the file is written, since a
	// reload whose outcome is unknown may have left the server on what the
	// file held before.
	changed := !bytes.Equal(previous, desired) || !bytes.Equal(written, desired)
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
		if err := h.startOrRecreate(ctx, st.Image); err != nil {
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
	if changed {
		if err := h.confirmApplied(desired); err != nil {
			return err
		}
	}

	for _, uid := range sortedUIDs(st.Authorizations) {
		if err := h.docker.ConnectNetwork(ctx, st.Authorizations[uid].Network, h.cfg.ContainerName, []string{RepoAlias}); err != nil {
			return err
		}
	}
	return nil
}

// startOrRecreate starts the stopped container, and replaces it if it cannot
// start. A container can be left unable to start for good: one attached to a
// Project network removed just before a power loss refers to a network that no
// longer exists, and retrying the start can never succeed. It holds no state of
// its own, and apply attaches the replacement to every authorized network.
func (h *RepoHost) startOrRecreate(ctx context.Context, image string) error {
	err := h.docker.StartContainer(ctx, h.cfg.ContainerName)
	if err == nil {
		return nil
	}
	h.logger.Warn("Repo host would not start; replacing it", zap.Error(err))
	if err := h.docker.RemoveContainer(ctx, h.cfg.ContainerName); err != nil {
		return err
	}
	if _, err := h.docker.EnsureNodeContainer(ctx, h.containerSpec(image)); err != nil {
		return err
	}
	return h.docker.StartContainer(ctx, h.cfg.ContainerName)
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
	return writeFile(h.writtenPath(), data, keyFileMode)
}

// confirmApplied records data as the configuration the repo host serves.
func (h *RepoHost) confirmApplied(data []byte) error {
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

// ClientStanza is one stanza a database container uses.
type ClientStanza struct {
	Name       string
	RetainFull int
}

// WriteClientConfig writes the pgBackRest configuration a Project lifetime's
// database mounts, and returns its directory. It names the repo host, the
// identity's paths, and each stanza with its retention: retention takes
// effect only on the side that issues the command, which is the database.
// It holds no secret, so it stays the Agent's and is readable by anyone.
func (h *RepoHost) WriteClientConfig(uid string, stanzas []ClientStanza) (string, error) {
	confDir, _, err := h.clientDirs(uid)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(confDir, identityDirMode); err != nil {
		return "", fmt.Errorf("pgbackrest: create %s: %w", confDir, err)
	}
	data := renderClientConfig(stanzas)
	path := filepath.Join(confDir, "pgbackrest.conf")
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, data) {
		return confDir, nil
	}
	if err := writeFile(path, data, certFileMode); err != nil {
		return "", err
	}
	return confDir, nil
}

// DataPath is where a PostgresDifferential database's PGDATA is mounted.
const DataPath = "/var/lib/postgresql/data"

// archivePushQueueMax bounds the WAL a database keeps while it cannot archive.
const archivePushQueueMax = "4GiB"

func renderClientConfig(stanzas []ClientStanza) []byte {
	var b strings.Builder
	line := func(k, v string) { fmt.Fprintf(&b, "%s=%s\n", k, v) }
	b.WriteString("[global]\n")
	line("repo1-host", RepoAlias)
	line("repo1-host-type", "tls")
	line("repo1-host-ca-file", serverTLSDir+"/ca.crt")
	line("repo1-host-cert-file", serverTLSDir+"/client.crt")
	line("repo1-host-key-file", serverTLSDir+"/client.key")
	line("compress-type", "zst")
	// Block incremental: a backup stores only the changed blocks of a file.
	// It needs bundling, and takes effect on the side running the backup.
	line("repo1-bundle", "y")
	line("repo1-block", "y")
	line("archive-async", "y")
	// While the repository cannot be reached, WAL waits in the spool and
	// pg_wal. Past this, pgBackRest drops it and logs that it did, rather than
	// filling the Node's disk for every Project on it; the dropped range breaks
	// point-in-time recovery across it, and the next backup is full.
	line("archive-push-queue-max", archivePushQueueMax)
	line("spool-path", "/var/spool/pgbackrest")
	line("start-fast", "y")
	line("log-level-console", "info")
	line("log-level-file", "off")

	sorted := append([]ClientStanza(nil), stanzas...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, st := range sorted {
		fmt.Fprintf(&b, "\n[%s]\n", st.Name)
		line("pg1-path", DataPath)
		if st.RetainFull > 0 {
			line("repo1-retention-full", fmt.Sprint(st.RetainFull))
		}
	}
	return []byte(b.String())
}
