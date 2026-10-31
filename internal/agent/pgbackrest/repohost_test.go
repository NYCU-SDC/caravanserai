package pgbackrest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"NYCU-SDC/caravanserai/internal/agent/docker"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakeRepoDocker records what the repo host asks Docker to do. The server
// answers pings unless failPings is positive, in which case that many pings
// fail first.
type fakeRepoDocker struct {
	mu        sync.Mutex
	exists    bool
	running   bool
	image     string
	networks  map[string]bool // networks the repo host is attached to
	calls     []string
	failPings int
	// startFails fails that many starts.
	startFails int
	// failSignals fails that many signals before delivering any.
	failSignals int
}

func newFakeRepoDocker() *fakeRepoDocker { return &fakeRepoDocker{networks: map[string]bool{}} }

func (f *fakeRepoDocker) record(c string) { f.calls = append(f.calls, c) }

func (f *fakeRepoDocker) RunToCompletion(context.Context, string, string, []string, []string) (docker.ExecResult, error) {
	return docker.ExecResult{}, nil
}

func (f *fakeRepoDocker) EnsureNodeNetwork(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("network:" + name)
	return nil
}

func (f *fakeRepoDocker) EnsureNodeContainer(_ context.Context, spec docker.NodeContainerSpec) (docker.NodeContainerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exists && f.image == spec.Image {
		return docker.NodeContainerState{ID: spec.Name, Running: f.running}, nil
	}
	f.record("create:" + spec.Image)
	f.exists, f.running, f.image = true, false, spec.Image
	return docker.NodeContainerState{ID: spec.Name, Created: true}, nil
}

func (f *fakeRepoDocker) StartContainer(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("start")
	if f.startFails > 0 {
		f.startFails--
		return errors.New("failed to set up container networking: network not found")
	}
	f.running = true
	return nil
}

func (f *fakeRepoDocker) ContainerRunning(context.Context, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running, nil
}

func (f *fakeRepoDocker) RemoveContainer(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("remove")
	f.exists, f.running = false, false
	f.networks = map[string]bool{}
	return nil
}

func (f *fakeRepoDocker) Signal(_ context.Context, _, sig string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSignals > 0 {
		f.failSignals--
		return errors.New("signal not delivered")
	}
	f.record("signal:" + sig)
	return nil
}

func (f *fakeRepoDocker) Exec(_ context.Context, _, _ string, cmd []string) (docker.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(cmd) > 1 && cmd[1] == "server-ping" {
		if f.failPings > 0 {
			f.failPings--
			return docker.ExecResult{ExitCode: 1, Stderr: "unable to connect"}, nil
		}
		return docker.ExecResult{}, nil
	}
	return docker.ExecResult{Stdout: "pgBackRest 2.59.3\n"}, nil
}

func (f *fakeRepoDocker) ConnectNetwork(_ context.Context, network, _ string, _ []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.networks[network] {
		f.record("connect:" + network)
	}
	f.networks[network] = true
	return nil
}

func (f *fakeRepoDocker) DisconnectNetwork(_ context.Context, network, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.networks[network] {
		f.record("disconnect:" + network)
	}
	delete(f.networks, network)
	return nil
}

func (f *fakeRepoDocker) NetworkAttached(_ context.Context, network, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.networks[network], nil
}

func (f *fakeRepoDocker) callsWith(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

const testImage = "ghcr.io/nycu-sdc/cara-postgres:16"

var testS3 = S3{Endpoint: "https://minio.local:9000", Bucket: "b", AccessKey: "ak", SecretKey: "sk"}

func newTestRepoHost(t *testing.T, d *fakeRepoDocker) *RepoHost {
	t.Helper()
	return NewRepoHost(d, Config{
		DataRoot:         t.TempDir(),
		NodeName:         "pve1",
		S3:               testS3,
		ReadinessTimeout: 2 * time.Second,
		Owner:            Owner{UID: os.Getuid(), GID: os.Getgid()},
	}, zap.NewNop())
}

func readConfig(t *testing.T, h *RepoHost) string {
	t.Helper()
	data, err := os.ReadFile(h.confPath())
	require.NoError(t, err)
	applied, err := os.ReadFile(h.appliedPath())
	require.NoError(t, err)
	require.Equal(t, string(data), string(applied), "what the server reads is confirmed served")
	return string(data)
}

// confirmed is the configuration the repo host is confirmed to serve.
func confirmed(t *testing.T, h *RepoHost) string {
	t.Helper()
	data, err := os.ReadFile(h.appliedPath())
	require.NoError(t, err)
	return string(data)
}

func authLines(conf string) []string {
	var out []string
	for _, l := range strings.Split(conf, "\n") {
		if strings.HasPrefix(l, "tls-server-auth=") {
			out = append(out, l)
		}
	}
	return out
}

func TestRenderServerConfig(t *testing.T) {
	conf, err := renderServerConfig(authorizations{
		"uid-b": {Stanzas: []string{"uid-b-pgdata"}, Network: "cara-b"},
		"uid-a": {Stanzas: []string{"uid-a-z", "uid-a-a", "uid-a-a"}, Network: "cara-a"},
	}, S3{Endpoint: "https://minio.local:9443", Bucket: "bkt", AccessKey: "ak", SecretKey: "sk", CAFile: "/x/ca.pem"})
	require.NoError(t, err)
	text := string(conf)
	assert.Equal(t, []string{"tls-server-auth=uid-a=uid-a-a,uid-a-z", "tls-server-auth=uid-b=uid-b-pgdata"}, authLines(text),
		"sorted by UID and stanza, duplicates dropped")
	assert.Contains(t, text, "repo1-s3-endpoint=minio.local\n")
	assert.Contains(t, text, "repo1-storage-port=9443\n")
	assert.Contains(t, text, "repo1-path=/cara/v2/pgbackrest\n")
	assert.Contains(t, text, "repo1-storage-ca-file=/etc/cara/pgbackrest/tls/s3-ca.crt\n")

	again, err := renderServerConfig(authorizations{
		"uid-a": {Stanzas: []string{"uid-a-a", "uid-a-z"}, Network: "cara-a"},
		"uid-b": {Stanzas: []string{"uid-b-pgdata"}, Network: "cara-b"},
	}, S3{Endpoint: "https://minio.local:9443", Bucket: "bkt", AccessKey: "ak", SecretKey: "sk", CAFile: "/x/ca.pem"})
	require.NoError(t, err)
	assert.Equal(t, conf, again, "the same state renders the same bytes")

	empty, err := renderServerConfig(authorizations{}, testS3)
	require.NoError(t, err)
	assert.Equal(t, []string{"tls-server-auth=" + noClient}, authLines(string(empty)),
		"the server needs the option even with nobody authorized")

	for _, endpoint := range []string{"http://minio.local:9000", "minio.local:9000", ""} {
		_, err := renderServerConfig(authorizations{}, S3{Endpoint: endpoint})
		assert.ErrorIs(t, err, ErrRepositoryNotHTTPS, endpoint)
	}
}

func TestAuthorizeStartsTheRepoHostAndAttachesTheNetwork(t *testing.T) {
	d := newFakeRepoDocker()
	h := newTestRepoHost(t, d)
	ctx := context.Background()

	require.NoError(t, h.Authorize(ctx, "uid-a", Authorization{Stanzas: []string{"uid-a-pgdata"}, Network: "cara-a"}, testImage))
	assert.Equal(t, []string{"create:" + testImage}, d.callsWith("create"))
	assert.Equal(t, []string{"start"}, d.callsWith("start"))
	assert.Equal(t, []string{"connect:cara-a"}, d.callsWith("connect"))
	assert.Equal(t, []string{"tls-server-auth=uid-a=uid-a-pgdata"}, authLines(readConfig(t, h)))

	// The same authorization again changes nothing and reloads nothing.
	require.NoError(t, h.Authorize(ctx, "uid-a", Authorization{Stanzas: []string{"uid-a-pgdata"}, Network: "cara-a"}, testImage))
	assert.Empty(t, d.callsWith("signal"))

	// A second Project reloads the running server instead of restarting it.
	require.NoError(t, h.Authorize(ctx, "uid-b", Authorization{Stanzas: []string{"uid-b-pgdata"}, Network: "cara-b"}, testImage))
	assert.Equal(t, []string{"signal:HUP"}, d.callsWith("signal"))
	assert.Len(t, d.callsWith("start"), 1)
	assert.Equal(t, []string{"tls-server-auth=uid-a=uid-a-pgdata", "tls-server-auth=uid-b=uid-b-pgdata"}, authLines(readConfig(t, h)))
}

func TestRevokeRemovesOnlyThatProject(t *testing.T) {
	d := newFakeRepoDocker()
	h := newTestRepoHost(t, d)
	ctx := context.Background()
	require.NoError(t, h.Authorize(ctx, "uid-a", Authorization{Stanzas: []string{"uid-a-pgdata"}, Network: "cara-a"}, testImage))
	require.NoError(t, h.Authorize(ctx, "uid-b", Authorization{Stanzas: []string{"uid-b-pgdata"}, Network: "cara-b"}, testImage))

	require.NoError(t, h.Revoke(ctx, "uid-a", "cara-a"))
	assert.Equal(t, []string{"tls-server-auth=uid-b=uid-b-pgdata"}, authLines(readConfig(t, h)))
	assert.Equal(t, []string{"disconnect:cara-a"}, d.callsWith("disconnect"))

	left, err := h.Remaining(ctx, "uid-a", "cara-a")
	require.NoError(t, err)
	assert.Empty(t, left)
	left, err = h.Remaining(ctx, "uid-b", "cara-b")
	require.NoError(t, err)
	assert.Equal(t, []string{"pgbackrest authorization uid-b"}, left)

	require.NoError(t, h.Revoke(ctx, "uid-a", "cara-a"), "revoking twice is not an error")
	require.NoError(t, h.Revoke(ctx, "uid-b", "cara-b"))
	assert.Equal(t, []string{"tls-server-auth=" + noClient}, authLines(readConfig(t, h)))
}

func TestRemainingReportsALeftoverAttachment(t *testing.T) {
	d := newFakeRepoDocker()
	h := newTestRepoHost(t, d)
	d.networks["cara-a"] = true // attached, but no longer authorized

	left, err := h.Remaining(context.Background(), "uid-a", "cara-a")
	require.NoError(t, err)
	assert.Equal(t, []string{"pgbackrest repo host on network cara-a"}, left)

	require.NoError(t, h.Revoke(context.Background(), "uid-a", "cara-a"))
	left, err = h.Remaining(context.Background(), "uid-a", "cara-a")
	require.NoError(t, err)
	assert.Empty(t, left)
}

// Authorizations for different Projects arrive concurrently; each change
// regenerates the whole configuration under one lock, so none is lost.
func TestConcurrentChangesKeepEveryAuthorization(t *testing.T) {
	d := newFakeRepoDocker()
	h := newTestRepoHost(t, d)
	ctx := context.Background()
	require.NoError(t, h.Authorize(ctx, "uid-gone", Authorization{Stanzas: []string{"uid-gone-pgdata"}, Network: "cara-gone"}, testImage))

	var wg sync.WaitGroup
	uids := []string{"uid-1", "uid-2", "uid-3", "uid-4", "uid-5", "uid-6"}
	for _, uid := range uids {
		wg.Add(1)
		go func(uid string) {
			defer wg.Done()
			assert.NoError(t, h.Authorize(ctx, uid, Authorization{Stanzas: []string{uid + "-pgdata"}, Network: "cara-" + uid}, testImage))
		}(uid)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		assert.NoError(t, h.Revoke(ctx, "uid-gone", "cara-gone"))
	}()
	wg.Wait()

	var want []string
	for _, uid := range uids {
		want = append(want, "tls-server-auth="+uid+"="+uid+"-pgdata")
	}
	assert.Equal(t, want, authLines(readConfig(t, h)))
}

func TestAConfigurationTheServerRejectsIsRolledBack(t *testing.T) {
	d := newFakeRepoDocker()
	h := newTestRepoHost(t, d)
	ctx := context.Background()
	require.NoError(t, h.Authorize(ctx, "uid-a", Authorization{Stanzas: []string{"uid-a-pgdata"}, Network: "cara-a"}, testImage))
	before := readConfig(t, h)

	// The server stops answering after the next reload, and answers again
	// once the previous configuration is back. Waiting 600ms with a ping every
	// 500ms makes three pings (at 0, 0.5s and 1s) before giving up; all three
	// fail, and the first ping after the rollback succeeds.
	h.cfg.ReadinessTimeout = 600 * time.Millisecond
	d.mu.Lock()
	d.failPings = 3
	d.mu.Unlock()

	err := h.Authorize(ctx, "uid-b", Authorization{Stanzas: []string{"uid-b-pgdata"}, Network: "cara-b"}, testImage)
	require.Error(t, err)
	assert.Equal(t, before, readConfig(t, h), "the previous configuration is what the server runs")
	assert.Equal(t, []string{"signal:HUP", "signal:HUP"}, d.callsWith("signal"), "reloaded with the new one, then with the old")
	assert.NotContains(t, d.callsWith("connect"), "connect:cara-b", "a rejected change attaches nothing")
}

func TestAuthorizeRefusesAnObjectStoreWithoutHTTPS(t *testing.T) {
	d := newFakeRepoDocker()
	h := newTestRepoHost(t, d)
	h.cfg.S3.Endpoint = "http://minio.local:9000"

	err := h.Authorize(context.Background(), "uid-a", Authorization{Stanzas: []string{"uid-a-pgdata"}, Network: "cara-a"}, testImage)
	assert.ErrorIs(t, err, ErrRepositoryNotHTTPS)
	assert.Empty(t, d.calls, "nothing is started or recorded")
	_, statErr := os.Stat(filepath.Join(h.root(), "repo-host.json"))
	assert.True(t, os.IsNotExist(statErr))
}

func TestReconcileRebuildsFromTheDurableState(t *testing.T) {
	d := newFakeRepoDocker()
	h := newTestRepoHost(t, d)
	ctx := context.Background()
	require.NoError(t, h.Authorize(ctx, "uid-a", Authorization{Stanzas: []string{"uid-a-pgdata"}, Network: "cara-a"}, testImage))

	// The Agent restarts; Docker lost the container in the meantime.
	d2 := newFakeRepoDocker()
	h2 := NewRepoHost(d2, h.cfg, zap.NewNop())
	require.NoError(t, h2.Reconcile(ctx))
	assert.Equal(t, []string{"create:" + testImage}, d2.callsWith("create"))
	assert.Equal(t, []string{"connect:cara-a"}, d2.callsWith("connect"))
}

// A stopped container that cannot start, such as one left referring to a removed
// network, is replaced rather than retried forever, and rejoins its networks.
func TestReconcileReplacesAContainerThatWillNotStart(t *testing.T) {
	d := newFakeRepoDocker()
	h := newTestRepoHost(t, d)
	ctx := context.Background()
	require.NoError(t, h.Authorize(ctx, "uid-a", Authorization{Stanzas: []string{"uid-a-pgdata"}, Network: "cara-a"}, testImage))

	d.mu.Lock()
	d.running, d.startFails, d.calls = false, 1, nil
	d.mu.Unlock()
	require.NoError(t, h.Reconcile(ctx))

	assert.Equal(t, []string{"remove"}, d.callsWith("remove"))
	assert.Equal(t, []string{"create:" + testImage}, d.callsWith("create"))
	assert.Equal(t, []string{"start", "start"}, d.callsWith("start"))
	assert.Equal(t, []string{"connect:cara-a"}, d.callsWith("connect"))
}

// A failure of the replacement itself is reported, for the next check to retry.
func TestReconcileReportsAContainerThatStillWillNotStart(t *testing.T) {
	d := newFakeRepoDocker()
	h := newTestRepoHost(t, d)
	ctx := context.Background()
	require.NoError(t, h.Authorize(ctx, "uid-a", Authorization{Stanzas: []string{"uid-a-pgdata"}, Network: "cara-a"}, testImage))

	d.mu.Lock()
	d.running, d.startFails = false, 2
	d.mu.Unlock()
	assert.Error(t, h.Reconcile(ctx))
}

// A revocation whose reload the server rejects is rolled back to a
// configuration that still authorizes the Project. Its retry must still remove
// it, and until then nothing may report the Project's authorization gone.
func TestARevocationRolledBackIsCompletedByItsRetry(t *testing.T) {
	d := newFakeRepoDocker()
	h := newTestRepoHost(t, d)
	ctx := context.Background()
	require.NoError(t, h.Authorize(ctx, "uid-a", Authorization{Stanzas: []string{"uid-a-pgdata"}, Network: "cara-a"}, testImage))
	require.NoError(t, h.Authorize(ctx, "uid-b", Authorization{Stanzas: []string{"uid-b-pgdata"}, Network: "cara-b"}, testImage))

	h.cfg.ReadinessTimeout = 600 * time.Millisecond
	d.mu.Lock()
	d.failPings = 3
	d.mu.Unlock()
	require.Error(t, h.Revoke(ctx, "uid-a", "cara-a"))
	assert.Contains(t, authLines(readConfig(t, h)), "tls-server-auth=uid-a=uid-a-pgdata", "rolled back")

	left, err := h.Remaining(ctx, "uid-a", "cara-a")
	require.NoError(t, err)
	assert.Contains(t, left, "pgbackrest authorization uid-a in the repo host configuration")

	require.NoError(t, h.Revoke(ctx, "uid-a", "cara-a"))
	assert.Equal(t, []string{"tls-server-auth=uid-b=uid-b-pgdata"}, authLines(readConfig(t, h)))
	left, err = h.Remaining(ctx, "uid-a", "cara-a")
	require.NoError(t, err)
	assert.Empty(t, left)
}

// With the repo host stopped, a revocation still rewrites its configuration:
// whatever starts it next must not serve the revoked Project.
func TestARevocationReachesAStoppedRepoHost(t *testing.T) {
	d := newFakeRepoDocker()
	h := newTestRepoHost(t, d)
	ctx := context.Background()
	require.NoError(t, h.Authorize(ctx, "uid-a", Authorization{Stanzas: []string{"uid-a-pgdata"}, Network: "cara-a"}, testImage))
	d.mu.Lock()
	d.running = false
	d.mu.Unlock()

	require.NoError(t, h.Revoke(ctx, "uid-a", "cara-a"))
	assert.Equal(t, []string{"tls-server-auth=" + noClient}, authLines(readConfig(t, h)))
	left, err := h.Remaining(ctx, "uid-a", "cara-a")
	require.NoError(t, err)
	assert.Empty(t, left)
}

// A reload that was never delivered leaves the server on the old
// configuration although the file holds the new one. Nothing may then report
// the revocation done, and both its retry and the background repair must
// deliver the reload.
func TestARevocationWhoseReloadWasNotDeliveredIsReloadedAgain(t *testing.T) {
	for _, retry := range []string{"revoke", "reconcile"} {
		t.Run(retry, func(t *testing.T) {
			d := newFakeRepoDocker()
			h := newTestRepoHost(t, d)
			ctx := context.Background()
			require.NoError(t, h.Authorize(ctx, "uid-a", Authorization{Stanzas: []string{"uid-a-pgdata"}, Network: "cara-a"}, testImage))
			require.NoError(t, h.Authorize(ctx, "uid-b", Authorization{Stanzas: []string{"uid-b-pgdata"}, Network: "cara-b"}, testImage))
			hups := len(d.callsWith("signal:HUP"))

			d.mu.Lock()
			d.failSignals = 1
			d.mu.Unlock()
			require.Error(t, h.Revoke(ctx, "uid-a", "cara-a"))
			assert.Contains(t, authLines(confirmed(t, h)), "tls-server-auth=uid-a=uid-a-pgdata")
			left, err := h.Remaining(ctx, "uid-a", "cara-a")
			require.NoError(t, err)
			assert.Contains(t, left, "pgbackrest authorization uid-a in the repo host configuration")

			if retry == "revoke" {
				require.NoError(t, h.Revoke(ctx, "uid-a", "cara-a"))
			} else {
				require.NoError(t, h.Reconcile(ctx))
			}
			assert.Len(t, d.callsWith("signal:HUP"), hups+1, "the reload is delivered")
			assert.Equal(t, []string{"tls-server-auth=uid-b=uid-b-pgdata"}, authLines(readConfig(t, h)))
			left, err = h.Remaining(ctx, "uid-a", "cara-a")
			require.NoError(t, err)
			assert.NotContains(t, left, "pgbackrest authorization uid-a in the repo host configuration")
		})
	}
}

// An Agent killed between writing the configuration and reloading the server
// reloads it when it starts again.
func TestAnAgentRestartedBeforeTheReloadReloads(t *testing.T) {
	d := newFakeRepoDocker()
	h := newTestRepoHost(t, d)
	ctx := context.Background()
	require.NoError(t, h.Authorize(ctx, "uid-a", Authorization{Stanzas: []string{"uid-a-pgdata"}, Network: "cara-a"}, testImage))
	require.NoError(t, h.Authorize(ctx, "uid-b", Authorization{Stanzas: []string{"uid-b-pgdata"}, Network: "cara-b"}, testImage))

	// The state and the file say uid-a is gone; the process died before HUP.
	st, err := h.loadState()
	require.NoError(t, err)
	delete(st.Authorizations, "uid-a")
	require.NoError(t, h.saveState(st))
	next, err := renderServerConfig(st.Authorizations, testS3)
	require.NoError(t, err)
	require.NoError(t, h.writeConfig(ctx, next, testImage))
	hups := len(d.callsWith("signal:HUP"))

	restarted := NewRepoHost(d, h.cfg, zap.NewNop())
	left, err := restarted.Remaining(ctx, "uid-a", "")
	require.NoError(t, err)
	assert.NotEmpty(t, left, "not confirmed served yet")
	require.NoError(t, restarted.Reconcile(ctx))
	assert.Len(t, d.callsWith("signal:HUP"), hups+1)
	assert.Equal(t, []string{"tls-server-auth=uid-b=uid-b-pgdata"}, authLines(readConfig(t, restarted)))
}

// A change whose reload failed, followed by one that returns to the
// configuration still served, rewrites the file and reloads: the file must
// not keep the change nobody confirmed.
func TestAFileAheadOfTheServerIsBroughtBack(t *testing.T) {
	d := newFakeRepoDocker()
	h := newTestRepoHost(t, d)
	ctx := context.Background()
	require.NoError(t, h.Authorize(ctx, "uid-a", Authorization{Stanzas: []string{"uid-a-pgdata"}, Network: "cara-a"}, testImage))
	served := confirmed(t, h)

	d.mu.Lock()
	d.failSignals = 1
	d.mu.Unlock()
	require.Error(t, h.Authorize(ctx, "uid-b", Authorization{Stanzas: []string{"uid-b-pgdata"}, Network: "cara-b"}, testImage))
	hups := len(d.callsWith("signal:HUP"))

	require.NoError(t, h.Revoke(ctx, "uid-b", "cara-b"))
	assert.Len(t, d.callsWith("signal:HUP"), hups+1)
	assert.Equal(t, served, readConfig(t, h))
}
