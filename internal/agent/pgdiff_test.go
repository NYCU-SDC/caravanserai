package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/docker"
	"NYCU-SDC/caravanserai/internal/agent/pgbackrest"
	"NYCU-SDC/caravanserai/internal/agent/restore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

const pgVersion = "pgBackRest 2.59.0"

// pgProject is a Scheduled Project whose "db" service runs PostgreSQL on a
// PostgresDifferential volume, next to a "web" service.
func pgProject() *v1.Project {
	p := gateProject()
	p.Status.Phase = v1.ProjectPhaseScheduled
	p.Status.AssignmentHistory = v1.AssignmentHistoryKnown
	// The first assignment: placement starts its volumes empty.
	p.Status.AssignmentGeneration = 1
	retain := 2
	p.Spec.Volumes = []v1.VolumeDef{{Name: "pgdata", Type: v1.VolumeTypeManaged}}
	p.Spec.Services = []v1.ServiceDef{
		{Name: "db", Image: v1.PostgresDifferentialImage,
			VolumeMounts: []v1.VolumeMount{{Name: "pgdata", MountPath: v1.PostgresDifferentialDataPath}}},
		{Name: "web", Image: "nginx:alpine"},
	}
	p.Spec.Backup = &v1.ProjectBackupConfig{Items: []v1.BackupItem{
		{Volume: "pgdata", Strategy: v1.BackupStrategyPostgresDifferential, RetainFull: &retain},
	}}
	return p
}

// fakeRepo records what placement asks of the repo host.
type fakeRepo struct {
	mu         sync.Mutex
	calls      []string
	authorized pgbackrest.Authorization
	stanzas    []pgbackrest.ClientStanza
	version    string
	authErr    error
}

func (f *fakeRepo) record(c string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

func (f *fakeRepo) IssueClientIdentity(_ context.Context, uid, _ string) (string, error) {
	f.record("identity " + uid)
	return "/tls", nil
}

func (f *fakeRepo) WriteClientConfig(uid string, stanzas []pgbackrest.ClientStanza) (string, error) {
	f.record("config " + uid)
	f.stanzas = stanzas
	return "/conf", nil
}

func (f *fakeRepo) Authorize(_ context.Context, uid string, a pgbackrest.Authorization, _ string) error {
	f.record("authorize " + uid)
	f.authorized = a
	return f.authErr
}

func (f *fakeRepo) Version(context.Context) (string, error) { return f.version, nil }

// pgFakeRuntime is a gateRuntime that also runs commands in containers. The
// database it simulates starts with archiving off; ALTER SYSTEM takes effect
// at the next restart, as in PostgreSQL.
type pgFakeRuntime struct {
	*gateRuntime
	mu sync.Mutex
	// events is every runtime step, in order: "network", "reconcile",
	// "restart" and each command run, joined by spaces.
	events        []string
	clientVersion string
	archiveMode   string
	archiveCmd    string
	pendingMode   string
	pendingCmd    string
	checkFails    bool
}

func newPgFakeRuntime() *pgFakeRuntime {
	rt := &pgFakeRuntime{gateRuntime: newGateRuntime(), clientVersion: pgVersion + "\n", archiveMode: "off"}
	rt.reconcileFn = func(context.Context, *v1.Project) error {
		rt.event("reconcile")
		return nil
	}
	return rt
}

func (r *pgFakeRuntime) event(e string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *pgFakeRuntime) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *pgFakeRuntime) EnsureProjectNetwork(context.Context, *v1.Project) error {
	r.event("network")
	return nil
}

func (r *pgFakeRuntime) RestartContainer(_ context.Context, name string) error {
	r.event("restart " + name)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pendingMode != "" {
		r.archiveMode = r.pendingMode
	}
	if r.pendingCmd != "" {
		r.archiveCmd = r.pendingCmd
	}
	return nil
}

func (r *pgFakeRuntime) Exec(_ context.Context, container, _ string, cmd []string) (docker.ExecResult, error) {
	line := strings.Join(cmd, " ")
	if cmd[0] == "sh" {
		line = "psql " + cmd[len(cmd)-1]
	}
	r.event(line)
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case line == "pg_isready -q":
		return docker.ExecResult{}, nil
	case line == "pgbackrest version":
		return docker.ExecResult{Stdout: r.clientVersion}, nil
	case line == "psql SHOW archive_mode":
		return docker.ExecResult{Stdout: r.archiveMode + "\n"}, nil
	case line == "psql SHOW archive_command":
		return docker.ExecResult{Stdout: r.archiveCmd + "\n"}, nil
	case strings.HasPrefix(line, "psql ALTER SYSTEM SET archive_mode"):
		r.pendingMode = "on"
		return docker.ExecResult{}, nil
	case strings.HasPrefix(line, "psql ALTER SYSTEM SET archive_command"):
		r.pendingCmd = strings.TrimSuffix(strings.TrimPrefix(line, "psql ALTER SYSTEM SET archive_command = '"), "'")
		return docker.ExecResult{}, nil
	case strings.HasSuffix(line, " stanza-create"):
		return docker.ExecResult{}, nil
	case strings.HasSuffix(line, " check"):
		if r.checkFails {
			return docker.ExecResult{ExitCode: 82, Stderr: "WAL segment was not archived"}, nil
		}
		return docker.ExecResult{}, nil
	}
	return docker.ExecResult{ExitCode: 127, Stderr: "unexpected " + line}, nil
}

func pgBackups(t *testing.T, repo pgRepository) *BackupSupport {
	dataRoot := t.TempDir()
	return &BackupSupport{
		Coordinator: backup.NewCoordinator(),
		Restorer:    restore.NewRestorer(nil, restore.Config{DataRoot: dataRoot, NodeName: gateNode}, zap.NewNop()),
		DataRoot:    dataRoot,
		NodeName:    gateNode,
		RepoHost:    repo,
	}
}

func indexOf(events []string, prefix string) int {
	for i, e := range events {
		if strings.HasPrefix(e, prefix) {
			return i
		}
	}
	return -1
}

// A new PostgresDifferential Project is reported Running only after its
// database archives through the repo host, and the database first starts
// already holding the identity and configuration it needs.
func TestPostgresPlacementArchivesBeforeRunning(t *testing.T) {
	p := pgProject()
	gs, client := newGateServer(t, pgProject())
	rt := newPgFakeRuntime()
	repo := &fakeRepo{version: pgVersion}

	reconcileOne(t.Context(), client, rt, &recordingRoutes{}, pgBackups(t, repo), p, zap.NewNop())

	stanza := v1.StanzaName(p.UID, "pgdata")
	assert.Equal(t, []string{"identity uid-1", "config uid-1", "authorize uid-1"}, repo.calls)
	assert.Equal(t, []string{stanza}, repo.authorized.Stanzas)
	assert.Equal(t, docker.NetworkName(p.Name), repo.authorized.Network)
	assert.Equal(t, []pgbackrest.ClientStanza{{Name: stanza, RetainFull: 2}}, repo.stanzas)

	events := rt.seen()
	assert.Less(t, indexOf(events, "network"), indexOf(events, "reconcile"),
		"the repo host joins the Project network before the database starts")
	assert.Less(t, indexOf(events, "pgbackrest --stanza="+stanza+" stanza-create"),
		indexOf(events, "psql ALTER SYSTEM SET archive_mode"))
	assert.Less(t, indexOf(events, "restart "+docker.ContainerName(p.Name, "db")),
		indexOf(events, "pgbackrest --stanza="+stanza+" check"),
		"archive_mode takes effect only after a restart")
	assert.Equal(t, archiveCommand(stanza), rt.archiveCmd)

	assert.Equal(t, []string{"pgdata"}, gs.stanzas, "the stanza is in the inventory")
	require.Len(t, gs.updates, 1)
	assert.Equal(t, v1.ProjectPhaseRunning, gs.updates[0].Phase)
}

// A database that already archives with its stanza's command, as a restored
// PGDATA does, is not restarted.
func TestPostgresPlacementKeepsArchivingThatIsAlreadyOn(t *testing.T) {
	p := pgProject()
	gs, client := newGateServer(t, pgProject())
	rt := newPgFakeRuntime()
	rt.archiveMode, rt.archiveCmd = "on", archiveCommand(v1.StanzaName(p.UID, "pgdata"))

	reconcileOne(t.Context(), client, rt, &recordingRoutes{}, pgBackups(t, &fakeRepo{version: pgVersion}), p, zap.NewNop())

	assert.Equal(t, -1, indexOf(rt.seen(), "restart"))
	assert.Equal(t, -1, indexOf(rt.seen(), "psql ALTER"))
	require.Len(t, gs.updates, 1)
	assert.Equal(t, v1.ProjectPhaseRunning, gs.updates[0].Phase)
}

// Each reason placement cannot finish holds the Project in Scheduled with a
// condition saying why; none of them is Failed, which the poll loop would
// never look at again.
func TestPostgresPlacementBlocksWithAReason(t *testing.T) {
	cases := []struct {
		name    string
		repo    func() pgRepository
		setup   func(*pgFakeRuntime)
		reason  string
		started bool
	}{
		{name: "no repo host", repo: func() pgRepository { return nil }, reason: pgBlockedNoRepository},
		{name: "repository not https",
			repo:   func() pgRepository { return &fakeRepo{version: pgVersion, authErr: pgbackrest.ErrRepositoryNotHTTPS} },
			reason: pgBlockedNoRepository},
		{name: "version mismatch", repo: func() pgRepository { return &fakeRepo{version: pgVersion} },
			setup:  func(rt *pgFakeRuntime) { rt.clientVersion = "pgBackRest 2.58.0" },
			reason: pgBlockedVersionMismatch, started: true},
		{name: "archive check fails", repo: func() pgRepository { return &fakeRepo{version: pgVersion} },
			setup:  func(rt *pgFakeRuntime) { rt.checkFails = true },
			reason: pgBlockedArchiving, started: true},
		{name: "authorize fails", repo: func() pgRepository { return &fakeRepo{version: pgVersion, authErr: errors.New("reload refused")} },
			reason: pgBlockedPlacement},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := pgProject()
			gs, client := newGateServer(t, pgProject())
			rt := newPgFakeRuntime()
			if tc.setup != nil {
				tc.setup(rt)
			}
			reconcileOne(t.Context(), client, rt, &recordingRoutes{}, pgBackups(t, tc.repo()), p, zap.NewNop())

			blocks := gs.writes("patch", v1.ConditionTypeRecoveryBlocked)
			require.Len(t, blocks, 1)
			assert.Equal(t, tc.reason, blocks[0].Reason)
			assert.Empty(t, gs.updates, "blocked is not a phase change")
			assert.Equal(t, tc.started, indexOf(rt.seen(), "reconcile") >= 0)
		})
	}
}

// The containers stay up when finishing fails; the next tick sees them all
// running and finishes then, clearing the block before reporting Running.
func TestPostgresPlacementFinishesOnALaterTick(t *testing.T) {
	p := pgProject()
	gs, client := newGateServer(t, pgProject())
	rt := newPgFakeRuntime()
	rt.checkFails = true
	backups := pgBackups(t, &fakeRepo{version: pgVersion})

	reconcileOne(t.Context(), client, rt, &recordingRoutes{}, backups, p, zap.NewNop())
	require.Len(t, gs.writes("patch", v1.ConditionTypeRecoveryBlocked), 1)
	require.Empty(t, gs.updates)

	rt.checkFails = false
	rt.inspectFn = func(context.Context, *v1.Project) ([]docker.ContainerState, error) {
		return []docker.ContainerState{{ServiceName: "db", Status: "running"}, {ServiceName: "web", Status: "running"}}, nil
	}
	fresh, err := client.GetProject(t.Context(), p.Name)
	require.NoError(t, err)
	reconcileOne(t.Context(), client, rt, &recordingRoutes{}, backups, fresh, zap.NewNop())

	assert.Len(t, gs.writes("delete", v1.ConditionTypeRecoveryBlocked), 1)
	require.Len(t, gs.updates, 1)
	assert.Equal(t, v1.ProjectPhaseRunning, gs.updates[0].Phase)
	assert.Equal(t, 1, strings.Count(strings.Join(rt.seen(), "\n"), "reconcile"),
		"running containers are not reconciled again")
}

// Finishing a placement whose containers are already up waits while another
// operation holds the Project: it may restart the database.
func TestPostgresPlacementWaitsForTheClaim(t *testing.T) {
	p := pgProject()
	gs, client := newGateServer(t, pgProject())
	rt := newPgFakeRuntime()
	rt.inspectFn = func(context.Context, *v1.Project) ([]docker.ContainerState, error) {
		return []docker.ContainerState{{ServiceName: "db", Status: "running"}, {ServiceName: "web", Status: "running"}}, nil
	}
	backups := pgBackups(t, &fakeRepo{version: pgVersion})
	release, ok := backups.Coordinator.TryClaim(backup.ResourceKey{Namespace: p.Namespace, Name: p.Name}, backup.OpBackup)
	require.True(t, ok)
	defer release()

	reconcileOne(t.Context(), client, rt, &recordingRoutes{}, backups, p, zap.NewNop())

	assert.Empty(t, rt.seen())
	assert.Empty(t, gs.updates)
	assert.Empty(t, gs.conditionWrites)
}
