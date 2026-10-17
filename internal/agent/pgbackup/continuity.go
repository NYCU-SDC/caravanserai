package pgbackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// droppedWAL is the start of the warning pgBackRest logs, through PostgreSQL,
// when archive-push-queue-max is exceeded and WAL is dropped unarchived.
const droppedWAL = "dropped WAL file"

// LogReader returns every log line a container still has, each distinct (as
// Docker's timestamp prefix makes them).
type LogReader interface {
	LogLines(ctx context.Context, containerID string) ([]string, error)
}

// latch is the durable continuity state of one database. It is bound to the
// data history and the log it was observed in: anything else is a database
// this state says nothing about.
type latch struct {
	UID         string `json:"uid"`
	Volume      string `json:"volume"`
	SystemID    string `json:"systemID"`
	Timeline    string `json:"timeline"`
	ContainerID string `json:"containerID"`

	// Version counts the events that made continuity unknown. It only grows,
	// so a full backup that started before an event cannot clear it.
	Version uint64 `json:"version"`
	// Latched is set by such an event and cleared by an accepted full backup
	// that started at the same Version.
	Latched bool `json:"latched"`

	// Lines is how many log lines have been observed, and LastHash the hash
	// of the last of them. A log whose line Lines-1 no longer hashes to it
	// was rotated or truncated.
	Lines    int    `json:"lines"`
	LastHash string `json:"lastHash,omitempty"`
}

func (l *latch) boundTo(db Database) bool {
	return l.UID == db.ProjectUID && l.Volume == db.Volume && l.SystemID == db.SystemID &&
		l.Timeline == db.Timeline && l.ContainerID == db.ContainerID
}

// FileContinuity keeps each database's latch in a file on the Node, outside
// the volume. Losing it costs one full backup, so it needs no copy elsewhere.
type FileContinuity struct {
	dir  string
	logs LogReader
	// mu serialises every read-modify-write of a latch file. A Project's runs
	// are already serial under its claim; this keeps the file sound regardless.
	mu sync.Mutex
}

// NewFileContinuity keeps latches under {dataRoot}/pgbackrest/continuity.
func NewFileContinuity(dataRoot string, logs LogReader) *FileContinuity {
	return &FileContinuity{dir: filepath.Join(dataRoot, "pgbackrest", "continuity"), logs: logs}
}

func (c *FileContinuity) path(db Database) string {
	return filepath.Join(c.dir, db.ProjectUID, db.Volume+".json")
}

// Check implements Continuity.
func (c *FileContinuity) Check(ctx context.Context, db Database) (bool, uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, err := c.observe(ctx, db)
	if l == nil {
		return false, 0, err
	}
	// With an error, l is the latch the failure set: a full that starts now
	// covers it.
	return !l.Latched, l.Version, err
}

// FullAccepted implements Continuity. The log is observed first: a gap logged
// while the full ran has a newer version, and keeps the latch.
func (c *FileContinuity) FullAccepted(ctx context.Context, db Database, version uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, err := c.observe(ctx, db)
	if err != nil {
		return err
	}
	if !l.Latched || l.Version != version {
		return nil
	}
	l.Latched = false
	return c.save(db, l)
}

// observe brings db's latch up to date with its log and saves it. Whatever
// cannot be proven continuous latches: no file, an unreadable file, another
// binding, a log that does not continue from the cursor, or a dropped-WAL
// warning. The latch is saved before the cursor it was found at is trusted.
func (c *FileContinuity) observe(ctx context.Context, db Database) (*latch, error) {
	prev, loadErr := c.load(db)

	lines, err := c.logs.LogLines(ctx, db.ContainerID)
	if err != nil {
		// Nothing was observed, so nothing may move; but continuity is
		// unknown from here until it is proven again.
		l := c.rebind(prev, db)
		l.Version++
		l.Latched = true
		if saveErr := c.save(db, l); saveErr != nil {
			return nil, saveErr
		}
		return l, fmt.Errorf("pgbackup: read the log of %s: %w", db.Service, err)
	}

	var l *latch
	switch {
	case loadErr != nil, !prev.boundTo(db), !continues(prev, lines):
		l = c.rebind(prev, db)
		l.Version++
		l.Latched = true
	default:
		l = prev
	}

	for _, line := range lines[min(l.Lines, len(lines)):] {
		if strings.Contains(line, droppedWAL) {
			l.Version++
			l.Latched = true
			break
		}
	}
	if len(lines) > 0 {
		l.Lines, l.LastHash = len(lines), hashLine(lines[len(lines)-1])
	}
	if err := c.save(db, l); err != nil {
		return nil, err
	}
	return l, nil
}

// rebind starts a latch for db, keeping only the version of the previous one
// so it never goes back.
func (c *FileContinuity) rebind(prev *latch, db Database) *latch {
	l := &latch{UID: db.ProjectUID, Volume: db.Volume, SystemID: db.SystemID, Timeline: db.Timeline, ContainerID: db.ContainerID}
	if prev != nil {
		l.Version = prev.Version
	}
	return l
}

// continues reports whether lines is the log l observed, possibly grown.
func continues(l *latch, lines []string) bool {
	if l == nil {
		return false
	}
	if l.Lines == 0 {
		return true
	}
	return len(lines) >= l.Lines && hashLine(lines[l.Lines-1]) == l.LastHash
}

func hashLine(line string) string {
	sum := sha256.Sum256([]byte(line))
	return hex.EncodeToString(sum[:])
}

func (c *FileContinuity) load(db Database) (*latch, error) {
	data, err := os.ReadFile(c.path(db))
	if err != nil {
		return nil, err
	}
	var l latch
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("pgbackup: latch %s: %w", c.path(db), err)
	}
	return &l, nil
}

// save writes the latch durably: to a temporary file that is synced, then
// renamed over the old one, then the directory synced.
func (c *FileContinuity) save(db Database, l *latch) error {
	path := c.path(db)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("pgbackup: latch dir: %w", err)
	}
	data, err := json.Marshal(l)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("pgbackup: write latch: %w", err)
	}
	_, werr := f.Write(data)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		return fmt.Errorf("pgbackup: write latch: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("pgbackup: write latch: %w", err)
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("pgbackup: sync latch dir: %w", err)
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
