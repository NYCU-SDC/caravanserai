package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/docker"
	"NYCU-SDC/caravanserai/internal/agent/pgbackrest"

	"go.uber.org/zap"
)

// Reasons a PostgresDifferential placement is held back. They reach the
// Project as the RecoveryBlocked condition while it stays Scheduled; each is
// retried on the next poll.
const (
	// pgBlockedNoRepository: this Node cannot run the repo host, because no
	// object store is configured or the configured one is not HTTPS.
	pgBlockedNoRepository = "BackupRepositoryUnavailable"
	// pgBlockedVersionMismatch: the database and the repo host run different
	// pgBackRest versions, which pgBackRest's TLS protocol does not allow.
	pgBlockedVersionMismatch = "PgBackRestVersionMismatch"
	// pgBlockedArchiving: the stanza could not be created or the archive
	// check failed, so nothing proves WAL reaches the repository.
	pgBlockedArchiving = "ArchivingNotReady"
)

// pgReadyTimeout bounds how long placement waits for PostgreSQL to accept
// connections after a start or restart.
const pgReadyTimeout = 90 * time.Second

// pgBlockedError holds a placement back with a reason the user can see.
type pgBlockedError struct {
	reason, detail string
}

func (e *pgBlockedError) Error() string { return e.reason + ": " + e.detail }

// pgRuntime is what placement needs from the container runtime.
type pgRuntime interface {
	EnsureProjectNetwork(ctx context.Context, project *v1.Project) error
	Exec(ctx context.Context, containerID, user string, cmd []string) (docker.ExecResult, error)
	RestartContainer(ctx context.Context, nameOrID string) error
}

// pgRepository is the Node's pgBackRest repo host, as placement uses it. It is
// satisfied by *pgbackrest.RepoHost.
type pgRepository interface {
	IssueClientIdentity(ctx context.Context, uid, image string) (string, error)
	WriteClientConfig(uid string, stanzas []pgbackrest.ClientStanza) (string, error)
	Authorize(ctx context.Context, uid string, a pgbackrest.Authorization, image string) error
	Version(ctx context.Context) (string, error)
}

// pgDatabase is one PostgreSQL of a Project whose data volume uses
// PostgresDifferential.
type pgDatabase struct {
	service, volume, image, stanza string
	retainFull                     int
}

func postgresDatabases(p *v1.Project) []pgDatabase {
	if !p.Spec.HasPostgresDifferential() {
		return nil
	}
	retain := map[string]int{}
	for _, item := range p.Spec.Backup.Items {
		if item.Strategy == v1.BackupStrategyPostgresDifferential {
			retain[item.Volume] = item.EffectiveRetainFull()
		}
	}
	var dbs []pgDatabase
	for _, svc := range p.Spec.Services {
		for _, vm := range svc.VolumeMounts {
			if n, ok := retain[vm.Name]; ok {
				dbs = append(dbs, pgDatabase{
					service: svc.Name, volume: vm.Name, image: svc.Image,
					stanza: v1.StanzaName(p.ObjectMeta.UID, vm.Name), retainFull: n,
				})
			}
		}
	}
	return dbs
}

// preparePostgresDifferential runs before the Project's containers start. It
// gives the lifetime its identity and configuration, which the database's
// container mounts from its creation, and authorizes it on the repo host over
// the Project's network, so a PGDATA that already archives finds the repo host
// on its very first start.
func preparePostgresDifferential(ctx context.Context, repo pgRepository, rt pgRuntime, p *v1.Project) error {
	dbs := postgresDatabases(p)
	if len(dbs) == 0 {
		return nil
	}
	if repo == nil {
		return &pgBlockedError{pgBlockedNoRepository, "this Node has no object store configured, so it cannot run the pgBackRest repo host"}
	}
	uid := p.ObjectMeta.UID
	if _, err := repo.IssueClientIdentity(ctx, uid, dbs[0].image); err != nil {
		return err
	}
	stanzas := make([]pgbackrest.ClientStanza, 0, len(dbs))
	names := make([]string, 0, len(dbs))
	for _, db := range dbs {
		stanzas = append(stanzas, pgbackrest.ClientStanza{Name: db.stanza, RetainFull: db.retainFull})
		names = append(names, db.stanza)
	}
	if _, err := repo.WriteClientConfig(uid, stanzas); err != nil {
		return err
	}
	if err := rt.EnsureProjectNetwork(ctx, p); err != nil {
		return err
	}
	err := repo.Authorize(ctx, uid, pgbackrest.Authorization{Stanzas: names, Network: docker.NetworkName(p.Name)}, dbs[0].image)
	if errors.Is(err, pgbackrest.ErrRepositoryNotHTTPS) {
		return &pgBlockedError{pgBlockedNoRepository, err.Error()}
	}
	return err
}

// finishPostgresDifferential runs once the containers are up, and returns nil
// only when every database archives through the repo host. Every step is
// idempotent, so an Agent that restarts part-way simply runs it again.
func finishPostgresDifferential(ctx context.Context, client *Client, repo pgRepository, rt pgRuntime, p *v1.Project) error {
	dbs := postgresDatabases(p)
	if len(dbs) == 0 {
		return nil
	}
	if repo == nil {
		return &pgBlockedError{pgBlockedNoRepository, "this Node has no object store configured, so it cannot run the pgBackRest repo host"}
	}
	serverVersion, err := repo.Version(ctx)
	if err != nil {
		return err
	}
	for _, db := range dbs {
		name := docker.ContainerName(p.Name, db.service)
		if err := waitPostgres(ctx, rt, name); err != nil {
			return err
		}
		clientVersion, err := execOK(ctx, rt, name, "postgres", "pgbackrest", "version")
		if err != nil {
			return err
		}
		if strings.TrimSpace(clientVersion) != serverVersion {
			return &pgBlockedError{pgBlockedVersionMismatch,
				fmt.Sprintf("%s runs %q but the repo host runs %q", db.service, strings.TrimSpace(clientVersion), serverVersion)}
		}

		// Recorded before it is created: the inventory a deleted lifetime's
		// cleanup reads must never miss a stanza that may exist.
		if err := client.RecordStanza(ctx, p.Name, fenceForProject(p), db.volume); err != nil {
			return err
		}
		if out, err := execOK(ctx, rt, name, "postgres", "pgbackrest", "--stanza="+db.stanza, "stanza-create"); err != nil {
			return &pgBlockedError{pgBlockedArchiving, fmt.Sprintf("stanza-create for %s: %v %s", db.stanza, err, out)}
		}

		if err := enableArchiving(ctx, rt, name, db.stanza); err != nil {
			return err
		}
		if out, err := execOK(ctx, rt, name, "postgres", "pgbackrest", "--stanza="+db.stanza, "check"); err != nil {
			return &pgBlockedError{pgBlockedArchiving, fmt.Sprintf("archive check for %s: %v %s", db.stanza, err, out)}
		}
	}
	return nil
}

// archiveCommand is how PostgreSQL hands a WAL segment to pgBackRest.
func archiveCommand(stanza string) string {
	return "pgbackrest --stanza=" + stanza + " archive-push %p"
}

// enableArchiving turns archiving on, unless it already is with this stanza's
// command, and restarts PostgreSQL for archive_mode to take effect. The
// settings are written with ALTER SYSTEM, so they live in PGDATA and come back
// with it after a restore.
func enableArchiving(ctx context.Context, rt pgRuntime, container, stanza string) error {
	mode, err := psql(ctx, rt, container, "SHOW archive_mode")
	if err != nil {
		return err
	}
	command, err := psql(ctx, rt, container, "SHOW archive_command")
	if err != nil {
		return err
	}
	if mode == "on" && command == archiveCommand(stanza) {
		return nil
	}
	if _, err := psql(ctx, rt, container, "ALTER SYSTEM SET archive_mode = 'on'"); err != nil {
		return err
	}
	if _, err := psql(ctx, rt, container, "ALTER SYSTEM SET archive_command = '"+archiveCommand(stanza)+"'"); err != nil {
		return err
	}
	if err := rt.RestartContainer(ctx, container); err != nil {
		return err
	}
	if err := waitPostgres(ctx, rt, container); err != nil {
		return err
	}
	if mode, err = psql(ctx, rt, container, "SHOW archive_mode"); err != nil {
		return err
	}
	if mode != "on" {
		return &pgBlockedError{pgBlockedArchiving, "archive_mode is " + mode + " after the restart"}
	}
	return nil
}

// psql runs one SQL statement as the database's superuser over the local
// socket and returns its trimmed output.
func psql(ctx context.Context, rt pgRuntime, container, sql string) (string, error) {
	out, err := execOK(ctx, rt, container, "postgres", "sh", "-c",
		`psql -X -qtA -U "${POSTGRES_USER:-postgres}" -d postgres -c "$1"`, "psql", sql)
	return strings.TrimSpace(out), err
}

// waitPostgres waits until PostgreSQL in container accepts connections.
func waitPostgres(ctx context.Context, rt pgRuntime, container string) error {
	deadline := time.Now().Add(pgReadyTimeout)
	for {
		res, err := rt.Exec(ctx, container, "postgres", []string{"pg_isready", "-q"})
		if err == nil && res.ExitCode == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("agent: %s did not accept connections within %s", container, pgReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// execOK runs cmd and returns its stdout, or an error naming the exit code and
// stderr when it fails.
func execOK(ctx context.Context, rt pgRuntime, container, user string, cmd ...string) (string, error) {
	res, err := rt.Exec(ctx, container, user, cmd)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return res.Stdout, fmt.Errorf("%s exited %d: %s", cmd[0], res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

// pgBlockedPlacement: a placement step failed for a reason that may pass on
// its own, such as the server being unreachable. Retried like the others.
const pgBlockedPlacement = "PostgresPlacementPending"

// postgresPending reports whether p has a PostgresDifferential database whose
// placement has not yet been reported Running. Only then does placement run:
// a Running Project already archives, and its repairs restart containers with
// the same mounts.
func postgresPending(p *v1.Project) bool {
	return p.Status.Phase == v1.ProjectPhaseScheduled && p.Spec.HasPostgresDifferential()
}

// placePostgres places a PostgresDifferential Project under one claim:
// prepare, then start (when start is non-nil, which is how the containers are
// created), then finish. It reports true only when every database archives,
// which is what Running promises. Otherwise it records why on the Project as
// RecoveryBlocked and leaves the phase Scheduled, so the next poll retries:
// none of these is a fault of the Project's spec.
//
// The Project's other services start together with the database. They are
// not held back until archiving is proven, and may see the database restart
// once while archiving is turned on.
func placePostgres(ctx context.Context, client *Client, runtime docker.Runtime, backups *BackupSupport, p *v1.Project, start func() bool, log *zap.Logger) bool {
	if backups != nil && backups.Coordinator != nil {
		release, ok := backups.Coordinator.TryClaim(backup.ResourceKey{Namespace: p.Namespace, Name: p.Name}, backup.OpPlacement)
		if !ok {
			// Another operation holds the Project; not a fault.
			return false
		}
		defer release()
	}
	var repo pgRepository
	if backups != nil {
		repo = backups.RepoHost
	}
	rt, ok := runtime.(pgRuntime)
	if !ok {
		reportRecoveryBlocked(ctx, client, p, pgBlockedNoRepository, "this Node's container runtime cannot run commands in containers", log)
		return false
	}
	if start != nil {
		if err := preparePostgresDifferential(ctx, repo, rt, p); err != nil {
			reportPostgresBlocked(ctx, client, p, err, log)
			return false
		}
		if !start() {
			return false
		}
	}
	if err := finishPostgresDifferential(ctx, client, repo, rt, p); err != nil {
		reportPostgresBlocked(ctx, client, p, err, log)
		return false
	}
	clearRecoveryBlocked(ctx, client, p, log)
	return true
}

func reportPostgresBlocked(ctx context.Context, client *Client, p *v1.Project, err error, log *zap.Logger) {
	reason, detail := pgBlockedPlacement, err.Error()
	var blocked *pgBlockedError
	if errors.As(err, &blocked) {
		reason, detail = blocked.reason, blocked.detail
	}
	log.Warn("PostgresDifferential placement is not complete", zap.String("reason", reason), zap.String("detail", detail))
	reportRecoveryBlocked(ctx, client, p, reason, detail, log)
}
