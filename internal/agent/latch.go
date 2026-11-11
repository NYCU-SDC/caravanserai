package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/docker"

	"go.uber.org/zap"
)

// ErrWorkloadLatched is returned by a call that would start the containers of
// an assignment this node has reported stopped.
var ErrWorkloadLatched = errors.New("this node reported the assignment stopped; its containers are not started here again")

// stopLatchVersion is the format of a stop latch record.
const stopLatchVersion = 1

// StopLatches is this node's own record of the assignments it has reported
// stopped (CARA-92).
//
// Stop evidence lets the server release an assignment, after which another
// node may start the Project. From then on this node must never start that
// assignment's containers again. The server's copy of the evidence is checked
// on the way into every start path, but those checks are many and each needs
// the server to answer. This record is written before the evidence is sent,
// read on every call that can start a container, and needs no network, so
// the promise holds however a start is reached and across an Agent restart.
//
// A record is dropped when it can no longer matter: when the server refused
// the evidence, or when the Project is no longer this assignment.
type StopLatches struct {
	dir string
	mu  sync.Mutex
}

type stopLatchRecord struct {
	Version              int       `json:"version"`
	Namespace            string    `json:"namespace"`
	Project              string    `json:"project"`
	ProjectUID           string    `json:"projectUID"`
	NodeName             string    `json:"nodeName"`
	AssignmentGeneration int64     `json:"assignmentGeneration"`
	RecordedAt           time.Time `json:"recordedAt"`
}

// NewStopLatches keeps the records under {dataRoot}/stop-latches.
func NewStopLatches(dataRoot string) *StopLatches {
	return &StopLatches{dir: filepath.Join(dataRoot, "stop-latches")}
}

func (l *StopLatches) path(namespace, name string) (string, error) {
	for _, part := range []string{namespace, name} {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, `/\`) {
			return "", fmt.Errorf("stop latch: unusable project key %q/%q", namespace, name)
		}
	}
	return filepath.Join(l.dir, namespace, name+".json"), nil
}

// Record notes that p's current assignment is reported stopped. It must
// succeed before the evidence is sent: evidence the server holds with no
// record here would leave the starts the record guards unguarded. The write
// is atomic and synced, so a crash leaves either no record or a whole one.
func (l *StopLatches) Record(p *v1.Project, now time.Time) error {
	if l == nil {
		return nil
	}
	path, err := l.path(p.Namespace, p.Name)
	if err != nil {
		return err
	}
	body, err := json.MarshalIndent(stopLatchRecord{
		Version:              stopLatchVersion,
		Namespace:            p.Namespace,
		Project:              p.Name,
		ProjectUID:           p.UID,
		NodeName:             p.Status.NodeRef,
		AssignmentGeneration: p.Status.AssignmentGeneration,
		RecordedAt:           now.UTC(),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("stop latch: marshal: %w", err)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("stop latch: create %q: %w", dir, err)
	}
	tmp := path + ".tmp"
	if err := writeSynced(tmp, body); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("stop latch: write %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("stop latch: commit %q: %w", path, err)
	}
	return syncDir(dir)
}

// Holds reports whether p's current assignment is recorded stopped. A record
// that cannot be read counts as holding: refusing a start until an operator
// looks is the safe side of an unanswerable question.
func (l *StopLatches) Holds(p *v1.Project) (bool, error) {
	if l == nil {
		return false, nil
	}
	rec, err := l.read(p.Namespace, p.Name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return true, err
	}
	return rec.ProjectUID == p.UID && rec.AssignmentGeneration == p.Status.AssignmentGeneration, nil
}

// Forget drops the record for a Project, if there is one.
func (l *StopLatches) Forget(namespace, name string) error {
	if l == nil {
		return nil
	}
	path, err := l.path(namespace, name)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stop latch: remove %q: %w", path, err)
	}
	return nil
}

// Sync brings the records in line with the Projects the server lists as
// assigned to this node. It must be given a complete, successful listing.
//
//   - A record whose Project is not listed, or is listed as another lifetime
//     or generation, guards nothing any more and is dropped: this node no
//     longer holds that assignment, so nothing here starts it.
//   - A listed assignment the server holds evidence for but this node has no
//     record of — evidence sent by an Agent from before these records — is
//     recorded, so the guard covers it from now on.
func (l *StopLatches) Sync(assigned []*v1.Project, now time.Time, log *zap.Logger) {
	if l == nil {
		return
	}
	byKey := make(map[string]*v1.Project, len(assigned))
	for _, p := range assigned {
		byKey[p.Namespace+"/"+p.Name] = p
		if workloadLatched(p) {
			if held, _ := l.Holds(p); !held {
				if err := l.Record(p, now); err != nil {
					log.Warn("Could not record a reported stop locally", zap.String("project", p.Name), zap.Error(err))
				}
			}
		}
	}

	namespaces, err := os.ReadDir(l.dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Warn("Could not list stop latch records", zap.Error(err))
		}
		return
	}
	for _, ns := range namespaces {
		if !ns.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(l.dir, ns.Name()))
		if err != nil {
			log.Warn("Could not list stop latch records", zap.String("namespace", ns.Name()), zap.Error(err))
			continue
		}
		for _, f := range files {
			name, ok := strings.CutSuffix(f.Name(), ".json")
			if !ok || f.IsDir() {
				continue
			}
			rec, err := l.read(ns.Name(), name)
			if err != nil {
				// Left in place: an unreadable record keeps refusing starts
				// until an operator removes it.
				log.Warn("Unreadable stop latch record", zap.String("namespace", ns.Name()), zap.String("project", name), zap.Error(err))
				continue
			}
			p := byKey[ns.Name()+"/"+name]
			if p != nil && p.UID == rec.ProjectUID && p.Status.AssignmentGeneration == rec.AssignmentGeneration {
				continue
			}
			if err := l.Forget(ns.Name(), name); err != nil {
				log.Warn("Could not drop a stop latch record", zap.String("project", name), zap.Error(err))
				continue
			}
			log.Debug("Dropped the stop latch of an assignment this node no longer holds",
				zap.String("project", name), zap.Int64("generation", rec.AssignmentGeneration))
		}
	}
}

func (l *StopLatches) read(namespace, name string) (stopLatchRecord, error) {
	path, err := l.path(namespace, name)
	if err != nil {
		return stopLatchRecord{}, err
	}
	l.mu.Lock()
	body, err := os.ReadFile(path)
	l.mu.Unlock()
	if err != nil {
		return stopLatchRecord{}, err
	}
	var rec stopLatchRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		return stopLatchRecord{}, fmt.Errorf("stop latch: parse %q: %w", path, err)
	}
	if rec.Version != stopLatchVersion {
		return stopLatchRecord{}, fmt.Errorf("stop latch: %q has version %d, want %d", path, rec.Version, stopLatchVersion)
	}
	return rec, nil
}

// refuseStart returns ErrWorkloadLatched when p's assignment is recorded
// stopped, or its record cannot be read.
func (l *StopLatches) refuseStart(p *v1.Project) error {
	held, err := l.Holds(p)
	switch {
	case err != nil:
		return fmt.Errorf("%w (record unreadable: %v)", ErrWorkloadLatched, err)
	case held:
		return fmt.Errorf("%w: %s/%s generation %d", ErrWorkloadLatched, p.Namespace, p.Name, p.Status.AssignmentGeneration)
	}
	return nil
}

func writeSynced(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("stop latch: open %q: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("stop latch: sync %q: %w", dir, err)
	}
	return nil
}

// LatchGuardedRuntime is the Docker runtime with every call that can start a
// Project's containers refused for an assignment recorded in StopLatches. It
// embeds the concrete runtime, not the interface, so the optional
// capabilities other code asserts for (pgRuntime, pgRestoreRuntime,
// assignmentRuntime, ...) are still there.
type LatchGuardedRuntime struct {
	*docker.DockerRuntime
	latches *StopLatches
}

// GuardRuntime wraps rt so that no start reaches Docker for a latched
// assignment.
func GuardRuntime(rt *docker.DockerRuntime, latches *StopLatches) *LatchGuardedRuntime {
	return &LatchGuardedRuntime{DockerRuntime: rt, latches: latches}
}

// ReconcileProject creates and starts missing or stopped containers.
func (g *LatchGuardedRuntime) ReconcileProject(ctx context.Context, project *v1.Project) error {
	if err := g.latches.refuseStart(project); err != nil {
		return err
	}
	return g.DockerRuntime.ReconcileProject(ctx, project)
}

// StartProject starts existing containers.
func (g *LatchGuardedRuntime) StartProject(ctx context.Context, project *v1.Project) error {
	if err := g.latches.refuseStart(project); err != nil {
		return err
	}
	return g.DockerRuntime.StartProject(ctx, project)
}

// RecoverServices recreates or starts the named services.
func (g *LatchGuardedRuntime) RecoverServices(ctx context.Context, project *v1.Project, services []string) error {
	if err := g.latches.refuseStart(project); err != nil {
		return err
	}
	return g.DockerRuntime.RecoverServices(ctx, project, services)
}

// UpdateServices recreates outdated containers, which starts them.
func (g *LatchGuardedRuntime) UpdateServices(ctx context.Context, project *v1.Project, drift docker.SpecDrift) error {
	if err := g.latches.refuseStart(project); err != nil {
		return err
	}
	return g.DockerRuntime.UpdateServices(ctx, project, drift)
}

// The guard must not hide what the runtime can do: code asserts for these.
var (
	_ docker.Runtime    = (*LatchGuardedRuntime)(nil)
	_ ResourceProber    = (*LatchGuardedRuntime)(nil)
	_ pgRuntime         = (*LatchGuardedRuntime)(nil)
	_ pgRestoreRuntime  = (*LatchGuardedRuntime)(nil)
	_ assignmentRuntime = (*LatchGuardedRuntime)(nil)
	_ pgRuntimeWithLogs = (*LatchGuardedRuntime)(nil)
)
