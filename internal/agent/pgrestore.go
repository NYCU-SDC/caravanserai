package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/docker"
	"NYCU-SDC/caravanserai/internal/agent/pgbackrest"
	"NYCU-SDC/caravanserai/internal/agent/restore"
	caravolume "NYCU-SDC/caravanserai/internal/agent/volume"

	"go.uber.org/zap"
)

const (
	// pgRestoreTimeout stops a stalled restore from holding the Project's
	// coordinator slot forever. Generous, because it covers downloading the
	// database and replaying its WAL; a run cut short is retried.
	pgRestoreTimeout = 12 * time.Hour

	// pgRestoreMinBackoff and pgRestoreMaxBackoff bound the wait between
	// attempts. The same backup is tried each time: falling back to an older
	// one would lose more data with nothing to show for it but a condition.
	pgRestoreMinBackoff = time.Minute
	pgRestoreMaxBackoff = 30 * time.Minute

	// pgRestoreOwnershipCheck is how often a running restore asks whether the
	// assignment it works for still exists.
	pgRestoreOwnershipCheck = 30 * time.Second

	pgRestoreDataPath = "/var/lib/postgresql/data"
)

// pgRestoreScript runs in a throwaway container with the database's image,
// $1 the stanza and $2 the backup label. It restores the backup, replays every
// WAL segment archived after it, and shuts down cleanly once PostgreSQL has
// promoted, so the database's own container starts with nothing to recover.
//
// Replay happens here rather than in that container because it takes as long
// as the WAL does, and the placement steps that wait for PostgreSQL are bounded.
//
// Archiving stays on: promotion writes a timeline history file, and without it
// in the repository a later restore could not follow this timeline. No client
// can connect, since there is no TCP listener and the socket is in /tmp. A
// failed attempt that promoted leaves only a timeline nothing was written to.
const pgRestoreScript = `set -eu
D=` + pgRestoreDataPath + `
L=/tmp/restore.log
pgbackrest --stanza="$1" --set="$2" --pg1-path="$D" restore
if ! pg_ctl -D "$D" -l "$L" -w -t 604800 -o "-c listen_addresses= -c unix_socket_directories=/tmp -c hot_standby=off" start >/dev/null; then
	tail -n 20 "$L" >&2
	exit 1
fi
until [ "$(pg_controldata "$D" | sed -n 's/^Database cluster state: *//p')" = "in production" ]; do
	if ! pg_ctl -D "$D" status >/dev/null 2>&1; then
		tail -n 20 "$L" >&2
		echo "PostgreSQL stopped before recovery finished" >&2
		exit 1
	fi
	sleep 1
done
last="$(sed -n 's/.*last completed transaction was at log time //p' "$L" | tail -n 1)"
pg_ctl -D "$D" -m fast -w -t 600 stop >/dev/null
echo "last-transaction=$last"
`

// pgRestoreRuntime is what a restore needs from the container runtime.
type pgRestoreRuntime interface {
	pgRuntime
	RunToCompletion(ctx context.Context, image, user string, binds, cmd []string) (docker.ExecResult, error)
	RunOneShot(ctx context.Context, spec docker.OneShotSpec) (docker.ExecResult, error)
	RemoveContainer(ctx context.Context, nameOrID string) error
}

// pgRestoreAttempt is what is remembered about one assignment's restore. It
// lives only in memory: an Agent that restarts tries again at once, which costs
// one attempt and needs no state to go stale.
type pgRestoreAttempt struct {
	failures int
	next     time.Time
	lastErr  string
}

// pgRestores runs PostgresDifferential restores in the background, so a large
// database does not hold up the poll loop and every other Project on the Node.
// A running restore holds the Project's coordinator claim, which is what keeps
// later polls, backups and cleanup away from it; polls see the Project busy and
// leave its status alone.
type pgRestores struct {
	runtime   pgRestoreRuntime
	repo      pgRepository
	restorer  *restore.Restorer
	ownership backup.OwnershipResolver
	dataRoot  string
	logger    *zap.Logger

	// reportRestored records what a finished restore recovered to. Its failure
	// is logged: the data is in place either way.
	reportRestored func(ctx context.Context, p *v1.Project, message string) error

	// give makes owner the owner of dir itself, not of what is inside it.
	give func(ctx context.Context, image string, owner pgbackrest.Owner, dir string) error

	now            func() time.Time
	ownershipCheck time.Duration
	mu             sync.Mutex
	attempts       map[string]*pgRestoreAttempt
}

func newPGRestores(runtime pgRestoreRuntime, repo pgRepository, restorer *restore.Restorer, ownership backup.OwnershipResolver,
	dataRoot string, report func(ctx context.Context, p *v1.Project, message string) error, logger *zap.Logger) *pgRestores {
	return &pgRestores{
		runtime: runtime, repo: repo, restorer: restorer, ownership: ownership,
		dataRoot: dataRoot, logger: logger, reportRestored: report,
		give: func(ctx context.Context, image string, owner pgbackrest.Owner, dir string) error {
			return pgbackrest.NewChowner(runtime, image).Give(ctx, owner, dir)
		},
		now: time.Now, ownershipCheck: pgRestoreOwnershipCheck,
		attempts: map[string]*pgRestoreAttempt{},
	}
}

func pgRestoreKey(p *v1.Project) string {
	return fmt.Sprintf("%s/%d", p.ObjectMeta.UID, p.Status.AssignmentGeneration)
}

// begin starts a restore of p unless its last failure is still backing off, and
// always returns the block that describes where the Project now stands. release
// is the caller's coordinator claim. It returns true when the job took the
// claim over and will release it when finished.
func (r *pgRestores) begin(ctx context.Context, p *v1.Project, release func()) (*restore.PlacementBlockedError, bool) {
	completion := p.Status.Backup.LastCompletion()
	if completion == nil {
		// The Project has been placed before and its data is not here. Starting
		// empty would present that as a successful deployment.
		return &restore.PlacementBlockedError{
			Reason: restore.BlockNoRestoreSource,
			Detail: "this Project has been placed before, but the server has accepted no backup of it to restore from",
		}, false
	}

	key := pgRestoreKey(p)
	r.mu.Lock()
	attempt := r.attempts[key]
	if attempt != nil && r.now().Before(attempt.next) {
		blocked := &restore.PlacementBlockedError{
			Reason: restore.BlockRestoreFailed,
			Detail: fmt.Sprintf("restoring backup %s failed %d time(s), next attempt at %s: %s",
				completion.BackupID, attempt.failures, attempt.next.UTC().Format(time.RFC3339), attempt.lastErr),
		}
		r.mu.Unlock()
		return blocked, false
	}
	// Forget other assignments' attempts: only the current one can run again.
	for k := range r.attempts {
		if k != key {
			delete(r.attempts, k)
		}
	}
	r.mu.Unlock()

	go func() {
		defer release()
		r.run(ctx, p, completion)
	}()
	return &restore.PlacementBlockedError{
		Reason: restore.BlockRestoreInProgress,
		Detail: "restoring backup " + completion.BackupID + " and replaying the WAL archived after it",
	}, true
}

func (r *pgRestores) run(parent context.Context, p *v1.Project, completion *v1.BackupCompletion) {
	key := pgRestoreKey(p)
	log := r.logger.With(zap.String("project", p.Namespace+"/"+p.Name), zap.String("backupID", completion.BackupID))
	log.Info("Restoring PostgresDifferential data")
	started := r.now()

	ctx, cancel := context.WithTimeout(parent, pgRestoreTimeout)
	defer cancel()
	stopWatching := r.watchOwnership(ctx, cancel, p, log)
	defer stopWatching()

	last, err := r.restore(ctx, p, completion)
	if err == nil {
		log.Info("Restored PostgresDifferential data", zap.Duration("took", r.now().Sub(started)), zap.String("lastTransaction", last))
		r.mu.Lock()
		delete(r.attempts, key)
		r.mu.Unlock()
		message := "Restored from backup " + completion.BackupID + " and the WAL archived after it"
		if last != "" {
			message += "; last transaction at " + last
		}
		if rerr := r.reportRestored(context.WithoutCancel(parent), p, message); rerr != nil {
			log.Warn("Failed to report the restored data", zap.Error(rerr))
		}
		return
	}
	if parent.Err() != nil {
		return
	}
	log.Error("Failed to restore PostgresDifferential data", zap.Error(err))
	r.mu.Lock()
	defer r.mu.Unlock()
	attempt := r.attempts[key]
	if attempt == nil {
		attempt = &pgRestoreAttempt{}
		r.attempts[key] = attempt
	}
	attempt.failures++
	attempt.lastErr = err.Error()
	attempt.next = r.now().Add(pgRestoreBackoff(attempt.failures))
}

func pgRestoreBackoff(failures int) time.Duration {
	backoff := pgRestoreMinBackoff
	for i := 1; i < failures && backoff < pgRestoreMaxBackoff; i++ {
		backoff *= 2
	}
	return min(backoff, pgRestoreMaxBackoff)
}

// watchOwnership cancels the restore once its assignment is gone. An
// unanswerable question is not an answer: a restore outlives a server outage.
func (r *pgRestores) watchOwnership(ctx context.Context, cancel context.CancelFunc, p *v1.Project, log *zap.Logger) (stop func()) {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(r.ownershipCheck)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if r.assignmentGone(ctx, p) {
					log.Info("Stopping a restore whose assignment is gone")
					cancel()
					return
				}
			}
		}
	}()
	return func() { close(done) }
}

func (r *pgRestores) assignmentGone(ctx context.Context, p *v1.Project) bool {
	switch r.ownership.Resolve(ctx, backup.ResourceKey{Namespace: p.Namespace, Name: p.Name}, p.ObjectMeta.UID, p.Status.AssignmentGeneration) {
	case backup.OwnershipRetained, backup.OwnershipUnknown:
		return false
	default:
		return true
	}
}

// restore puts the data on disk and returns the time of the last transaction
// PostgreSQL replayed. Nothing live is touched until every database has
// recovered and shut down cleanly in staging.
func (r *pgRestores) restore(ctx context.Context, p *v1.Project, completion *v1.BackupCompletion) (string, error) {
	dbs := postgresDatabases(p)
	// The throwaway containers reach the repo host exactly as the database will.
	if err := preparePostgresDifferential(ctx, r.repo, r.runtime, p); err != nil {
		return "", err
	}
	// An Agent that died part-way left its container running. It must not carry
	// on against the staging about to be cleared, in the same stanza.
	for _, db := range dbs {
		if err := r.runtime.RemoveContainer(ctx, "cara-restore-"+db.stanza); err != nil {
			return "", err
		}
	}
	if err := r.clearStaging(ctx, p, dbs[0].image); err != nil {
		return "", err
	}
	staging, err := r.restorer.PrepareStaging(p)
	if err != nil {
		return "", err
	}
	confDir, tlsDir, err := caravolume.PgBackRestClientDirs(r.dataRoot, p.ObjectMeta.UID)
	if err != nil {
		return "", err
	}

	staged := make(map[string]string, len(dbs))
	var last string
	for _, db := range dbs {
		label := ""
		for _, item := range completion.Items {
			if item.Volume == db.volume {
				label = item.Label
			}
		}
		if label == "" {
			return "", fmt.Errorf("agent: backup %s has no item for volume %q", completion.BackupID, db.volume)
		}

		dir := filepath.Join(staging, "extracted", db.volume)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("agent: create restore directory: %w", err)
		}
		if err := r.give(ctx, db.image, pgbackrest.ImageOwner, dir); err != nil {
			return "", err
		}

		result, err := r.runtime.RunOneShot(ctx, docker.OneShotSpec{
			Name:    "cara-restore-" + db.stanza,
			Image:   db.image,
			User:    fmt.Sprintf("%d:%d", pgbackrest.ImageOwner.UID, pgbackrest.ImageOwner.GID),
			Network: docker.NetworkName(p.Name),
			Binds: []string{
				dir + ":" + pgRestoreDataPath,
				confDir + ":/etc/pgbackrest:ro",
				tlsDir + ":/etc/cara/pgbackrest/tls:ro",
			},
			Cmd: []string{"tini", "--", "sh", "-c", pgRestoreScript, "cara-restore", db.stanza, label},
		})
		if err != nil {
			return "", err
		}
		if result.ExitCode != 0 {
			return "", fmt.Errorf("agent: restore %s of %s exited %d: %s", label, db.volume, result.ExitCode, tailOf(result.Stderr, 400))
		}
		if v := lastTransaction(result.Stdout); v > last {
			last = v
		}
		staged[db.volume] = dir
	}

	// A swap for an assignment that has gone would stamp this Node's volumes for
	// a Project it no longer holds.
	if r.assignmentGone(ctx, p) {
		return "", errors.New("agent: the assignment was released during the restore")
	}
	// Moving a directory to another parent takes write permission on it, which an
	// Agent that is not root lacks on what the database's user owns. Handing over
	// only the top directory is enough, however large the database. If this stops
	// between the two handovers, the image's entrypoint takes PGDATA back at start.
	agent := pgbackrest.Owner{UID: os.Getuid(), GID: os.Getgid()}
	images := make(map[string]string, len(dbs))
	for _, db := range dbs {
		images[db.volume] = db.image
		if err := r.give(ctx, db.image, agent, staged[db.volume]); err != nil {
			return "", err
		}
	}
	if err := r.restorer.InstallStaged(p, completion.BackupID, staged); err != nil {
		return "", err
	}
	for volume, image := range images {
		live, err := caravolume.HostPath(r.dataRoot, p.Namespace, p.Name, volume)
		if err != nil {
			return "", err
		}
		if err := r.give(ctx, image, pgbackrest.ImageOwner, live); err != nil {
			return "", err
		}
	}
	return last, nil
}

// clearStaging removes what an earlier attempt left. Most of it belongs to the
// database's user, which an Agent that is not root cannot delete by itself.
func (r *pgRestores) clearStaging(ctx context.Context, p *v1.Project, image string) error {
	staging, err := restore.StagingDir(r.dataRoot, p.Namespace, p.Name)
	if err != nil {
		return err
	}
	err = os.RemoveAll(staging)
	if err == nil || !errors.Is(err, os.ErrPermission) {
		return err
	}
	result, err := r.runtime.RunToCompletion(ctx, image, "0", []string{staging + ":/target"}, []string{"find", "/target", "-mindepth", "1", "-delete"})
	if err != nil {
		return fmt.Errorf("agent: clear restore staging: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("agent: clear restore staging: find exited %d: %s", result.ExitCode, tailOf(result.Stderr, 400))
	}
	return nil
}

func lastTransaction(stdout string) string {
	for _, line := range strings.Split(stdout, "\n") {
		if v, ok := strings.CutPrefix(line, "last-transaction="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func tailOf(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}
