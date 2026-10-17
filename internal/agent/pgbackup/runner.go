package pgbackup

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/docker"

	"go.uber.org/zap"
)

// FullInterval is how old the newest full backup may be before the next
// backup is full again. Fixed: the Project's schedule is the only clock.
const FullInterval = 7 * 24 * time.Hour

// stopTimeout bounds how long a cancelled run waits for the backup process in
// the container to exit.
const stopTimeout = 30 * time.Second

// Runtime is what the runner needs from the container runtime.
type Runtime interface {
	InspectProject(ctx context.Context, project *v1.Project) ([]docker.ContainerState, error)
	Exec(ctx context.Context, containerID, user string, cmd []string) (docker.ExecResult, error)
}

// Database is one PostgresDifferential database of a Project, as one run sees
// it.
type Database struct {
	ProjectUID string
	Volume     string
	Service    string
	Stanza     string
	// ContainerID is the database container this run backs up from; its log
	// is what continuity is observed in.
	ContainerID string
	// SystemID and Timeline identify the data history the backup belongs to.
	SystemID string
	Timeline string
}

// Continuity says whether WAL archiving has provably been continuous.
type Continuity interface {
	// Check observes db's log since the last check and reports whether
	// continuity is known, with the gap event version at this moment.
	// Anything it cannot prove is reported as not known.
	Check(ctx context.Context, db Database) (known bool, version uint64, err error)
	// FullAccepted records that a full backup which started at version was
	// accepted. A gap seen since keeps continuity unknown.
	FullAccepted(db Database, version uint64) error
}

// Item is one confirmed backup of a run.
type Item struct {
	Volume  string
	Stanza  string
	Label   string
	Type    string
	PGMajor int
	Started time.Time
	Stopped time.Time
}

// Recorder publishes a completed run. A nil error means the server accepted
// it; only then may a full backup clear the continuity latch.
type Recorder interface {
	Record(ctx context.Context, project *v1.Project, backupID string, items []Item) error
}

// Runner takes one backup of every PostgresDifferential database of a
// Project, while it runs.
type Runner struct {
	coordinator *backup.Coordinator
	runtime     Runtime
	continuity  Continuity
	recorder    Recorder
	logger      *zap.Logger
	now         func() time.Time
}

// NewRunner creates a Runner. continuity and recorder may be nil: without
// continuity every backup is full, and without a recorder no run is accepted.
func NewRunner(coordinator *backup.Coordinator, runtime Runtime, continuity Continuity, recorder Recorder, logger *zap.Logger) *Runner {
	return &Runner{
		coordinator: coordinator, runtime: runtime, continuity: continuity, recorder: recorder,
		logger: logger, now: time.Now,
	}
}

// ErrContainerNotReady means a database container is not running under the
// current assignment, so nothing is run in it.
var ErrContainerNotReady = errors.New("pgbackup: database container is not running under this assignment")

// Run takes one backup of each PostgresDifferential database of project.
// No service is stopped. A run that finds the Project busy does nothing.
func (r *Runner) Run(ctx context.Context, project *v1.Project) error {
	key := backup.ResourceKey{Namespace: project.Namespace, Name: project.Name}
	log := r.logger.With(zap.String("project", key.String()))

	release, ok := r.coordinator.TryClaim(key, backup.OpBackup)
	if !ok {
		op, _ := r.coordinator.Current(key)
		log.Info("Skipping backup, project is busy", zap.String("operation", string(op)))
		return nil
	}
	defer release()

	dbs, err := r.databases(ctx, project)
	if err != nil {
		return err
	}
	backupID, err := backup.NewBackupID(r.now())
	if err != nil {
		return err
	}
	log = log.With(zap.String("backupID", backupID))

	items := make([]Item, 0, len(dbs))
	fulls := map[int]uint64{}
	for i := range dbs {
		item, version, err := r.backupOne(ctx, &dbs[i], log)
		if err != nil {
			return err
		}
		if item.Type == TypeFull {
			fulls[i] = version
		}
		items = append(items, item)
	}

	if r.recorder == nil {
		log.Info("Backup complete; no recorder, so it is not reported")
		return nil
	}
	if err := r.recorder.Record(ctx, project, backupID, items); err != nil {
		return fmt.Errorf("pgbackup: record %s: %w", backupID, err)
	}
	for i, version := range fulls {
		if r.continuity == nil {
			break
		}
		if err := r.continuity.FullAccepted(dbs[i], version); err != nil {
			// The latch stays set: one more full than needed, nothing lost.
			log.Warn("Failed to clear the continuity latch", zap.String("volume", dbs[i].Volume), zap.Error(err))
		}
	}
	log.Info("Backup recorded", zap.Int("items", len(items)))
	return nil
}

// databases lists the Project's PostgresDifferential databases, each in a
// container that runs under the Project's current assignment.
func (r *Runner) databases(ctx context.Context, p *v1.Project) ([]Database, error) {
	states, err := r.runtime.InspectProject(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("pgbackup: inspect: %w", err)
	}
	var dbs []Database
	for _, svc := range p.Spec.Services {
		for _, vm := range svc.VolumeMounts {
			if p.Spec.BackupStrategyFor(vm.Name) != v1.BackupStrategyPostgresDifferential {
				continue
			}
			i := slices.IndexFunc(states, func(s docker.ContainerState) bool { return s.ServiceName == svc.Name })
			if i < 0 || states[i].NotOwned != nil || states[i].Status != "running" {
				return nil, fmt.Errorf("%w: %s", ErrContainerNotReady, svc.Name)
			}
			dbs = append(dbs, Database{
				ProjectUID: p.ObjectMeta.UID, Volume: vm.Name, Service: svc.Name,
				Stanza: v1.StanzaName(p.ObjectMeta.UID, vm.Name), ContainerID: states[i].ContainerID,
			})
		}
	}
	return dbs, nil
}

// backupOne chooses the type for db, takes the backup and confirms it. It
// returns the continuity version the choice was made at.
func (r *Runner) backupOne(ctx context.Context, db *Database, log *zap.Logger) (Item, uint64, error) {
	log = log.With(zap.String("volume", db.Volume), zap.String("stanza", db.Stanza))

	var err error
	if db.SystemID, err = r.psql(ctx, db.ContainerID, "SELECT system_identifier FROM pg_control_system()"); err != nil {
		return Item{}, 0, err
	}
	if db.Timeline, err = r.psql(ctx, db.ContainerID, "SELECT substr(pg_walfile_name(pg_current_wal_lsn()), 1, 8)"); err != nil {
		return Item{}, 0, err
	}
	versionNum, err := r.psql(ctx, db.ContainerID, "SHOW server_version_num")
	if err != nil {
		return Item{}, 0, err
	}
	n, err := strconv.Atoi(versionNum)
	if err != nil {
		return Item{}, 0, fmt.Errorf("pgbackup: server_version_num %q: %w", versionNum, err)
	}

	before, err := r.info(ctx, db)
	if err != nil {
		return Item{}, 0, err
	}
	known, version := false, uint64(0)
	if r.continuity != nil {
		if known, version, err = r.continuity.Check(ctx, *db); err != nil {
			log.Warn("Cannot tell whether archiving was continuous; taking a full backup", zap.Error(err))
			known = false
		}
	}
	sel := chooseType(before, db.Timeline, known, r.now(), FullInterval)
	log = log.With(zap.String("type", sel.Type))
	log.Info("Starting PostgreSQL backup", zap.String("why", sel.Reason))

	started := r.now()
	if err := r.runBackup(ctx, db, sel.Type); err != nil {
		return Item{}, 0, err
	}
	after, err := r.info(ctx, db)
	if err != nil {
		return Item{}, 0, err
	}
	set, ok := newBackup(before, after, sel.Type)
	if !ok {
		return Item{}, 0, fmt.Errorf("pgbackup: %s: pgbackrest reported success but info lists no new %s backup", db.Stanza, sel.Type)
	}
	log.Info("PostgreSQL backup complete", zap.String("label", set.Label), zap.Duration("duration", r.now().Sub(started)))
	return Item{
		Volume: db.Volume, Stanza: db.Stanza, Label: set.Label, Type: set.Type, PGMajor: n / 10000,
		Started: set.started(), Stopped: time.Unix(set.Timestamp.Stop, 0).UTC(),
	}, version, nil
}

// newBackup finds the backup of the given type that after lists and before
// did not.
func newBackup(before, after []backupSet, typ string) (backupSet, bool) {
	seen := map[string]bool{}
	for _, b := range before {
		seen[b.Label] = true
	}
	for i := len(after) - 1; i >= 0; i-- {
		if !seen[after[i].Label] && after[i].Type == typ {
			return after[i], true
		}
	}
	return backupSet{}, false
}

func (r *Runner) info(ctx context.Context, db *Database) ([]backupSet, error) {
	out, err := r.exec(ctx, db.ContainerID, "pgbackrest", "--stanza="+db.Stanza, "--output=json", "info")
	if err != nil {
		return nil, err
	}
	return parseInfo([]byte(out), db.Stanza)
}

// runBackup runs the backup in the database container. A Docker exec keeps
// running when its caller goes away, so cancelling ctx also stops the backup
// process in the container; otherwise it would finish unobserved, holding the
// stanza lock against the next run.
func (r *Runner) runBackup(ctx context.Context, db *Database, typ string) error {
	type result struct {
		res docker.ExecResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := r.runtime.Exec(context.WithoutCancel(ctx), db.ContainerID, "postgres",
			[]string{"pgbackrest", "--stanza=" + db.Stanza, "--type=" + typ, "backup"})
		done <- result{res, err}
	}()

	select {
	case out := <-done:
		if out.err != nil {
			return fmt.Errorf("pgbackup: %s backup: %w", db.Stanza, out.err)
		}
		if out.res.ExitCode != 0 {
			return fmt.Errorf("pgbackup: %s backup exited %d: %s", db.Stanza, out.res.ExitCode, strings.TrimSpace(out.res.Stderr))
		}
		return nil
	case <-ctx.Done():
	}

	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer cancel()
	if _, err := r.runtime.Exec(stopCtx, db.ContainerID, "postgres", stopBackupCmd(db.Stanza)); err != nil {
		r.logger.Warn("Failed to stop the cancelled backup", zap.String("stanza", db.Stanza), zap.Error(err))
	}
	select {
	case <-done:
	case <-stopCtx.Done():
		r.logger.Warn("The cancelled backup did not exit in time", zap.String("stanza", db.Stanza))
	}
	return ctx.Err()
}

// stopBackupCmd terminates every pgBackRest backup process of stanza in the
// container, including the local processes the backup started. pgBackRest
// exits on SIGTERM and releases its lock; the interrupted backup is resumed
// or discarded by the next one.
func stopBackupCmd(stanza string) []string {
	return []string{"sh", "-c", `
for d in /proc/[0-9]*; do
  c=$(tr '\0' ' ' < "$d/cmdline" 2>/dev/null) || continue
  case "$c" in pgbackrest*"--stanza=$1 "*backup*) kill -TERM "${d#/proc/}" 2>/dev/null ;; esac
done`, "stop-backup", stanza}
}

func (r *Runner) psql(ctx context.Context, container, sql string) (string, error) {
	out, err := r.exec(ctx, container, "sh", "-c",
		`psql -X -qtA -U "${POSTGRES_USER:-postgres}" -d postgres -c "$1"`, "psql", sql)
	return strings.TrimSpace(out), err
}

func (r *Runner) exec(ctx context.Context, container string, cmd ...string) (string, error) {
	res, err := r.runtime.Exec(ctx, container, "postgres", cmd)
	if err != nil {
		return "", fmt.Errorf("pgbackup: %s: %w", cmd[0], err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("pgbackup: %s exited %d: %s", cmd[0], res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}
