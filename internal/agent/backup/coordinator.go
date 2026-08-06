package backup

import (
	"fmt"
	"sync"
)

// ResourceKey identifies a Project within a namespace.
type ResourceKey struct {
	Namespace string
	Name      string
}

func (k ResourceKey) String() string {
	return k.Namespace + "/" + k.Name
}

// Operation is a mutually exclusive activity the agent performs on one
// Project. Only one may be in flight per Project at a time: a backup that
// overlapped a terminate would upload archives for a Project being torn down,
// and a restore that overlapped a backup would read a half-written volume.
type Operation string

const (
	OpBackup      Operation = "Backup"
	OpRestore     Operation = "Restore"
	OpTerminate   Operation = "Terminate"
	OpFinalBackup Operation = "FinalBackup"
	OpGC          Operation = "GC"
)

// Coordinator serialises operations per Project and lets unrelated parts of
// the agent ask whether a Project is currently busy.
//
// This is the mechanism actually responsible for correctness in CARA-59, not
// the server-side Maintenance condition. The poll loop consults IsBusy before
// dispatching, so a Project whose containers are stopped for a backup is
// never health-checked and never reported Failed. Being in-process memory it
// cannot fail on a network blip and has no propagation delay, which a status
// round-trip to the server would have. The Maintenance condition remains, but
// only for observability and as an input to the future recovery controller.
//
// A Coordinator is safe for concurrent use.
type Coordinator struct {
	mu     sync.Mutex
	claims map[ResourceKey]Operation
}

// NewCoordinator creates an empty Coordinator.
func NewCoordinator() *Coordinator {
	return &Coordinator{claims: make(map[ResourceKey]Operation)}
}

// TryClaim attempts to claim key for op. It returns a release function and
// true on success, or nil and false if another operation already holds the
// Project.
//
// It never blocks. A caller that loses the race must skip this round rather
// than queue behind the holder: a backup tick that waited would eventually
// fire against a Project that has since been terminated or moved.
//
// The returned release function is idempotent and must be called (normally
// via defer) so a panicking or early-returning caller cannot strand the
// claim.
func (c *Coordinator) TryClaim(key ResourceKey, op Operation) (release func(), ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, busy := c.claims[key]; busy {
		return nil, false
	}
	c.claims[key] = op

	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			delete(c.claims, key)
		})
	}, true
}

// IsBusy reports whether any operation currently holds key. The poll loop
// calls this before dispatching so it skips the Project entirely for that
// tick: no health check, no reconcile, no status write.
func (c *Coordinator) IsBusy(key ResourceKey) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, busy := c.claims[key]
	return busy
}

// Current returns the operation holding key, and whether one is held. Useful
// for logging and diagnostics; correctness decisions should use IsBusy or the
// result of TryClaim.
func (c *Coordinator) Current(key ResourceKey) (Operation, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	op, busy := c.claims[key]
	return op, busy
}

// ExitReason describes why a backup run is ending.
type ExitReason int

const (
	// ExitSuccess is a completed backup.
	ExitSuccess ExitReason = iota
	// ExitFailure is an error during archiving or upload.
	ExitFailure
	// ExitTimeout is the upload or overall run exceeding its deadline.
	ExitTimeout
	// ExitCancelled is context cancellation that is not an agent shutdown.
	ExitCancelled
	// ExitAgentShutdown is the agent process terminating.
	ExitAgentShutdown
)

// Ownership describes whether this node should still be running the Project
// when a backup ends. It is resolved by re-reading the Project from the
// server after the backup finishes, because the answer may have changed while
// containers were stopped.
type Ownership int

const (
	// OwnershipRetained means the Project is still assigned to this node and
	// still expected to run.
	OwnershipRetained Ownership = iota
	// OwnershipReassigned means status.nodeRef now points at another node.
	OwnershipReassigned
	// OwnershipTerminating means the Project moved to the Terminating phase.
	OwnershipTerminating
	// OwnershipLost means the assignment is gone for another reason: a
	// completed drain, or the Project no longer existing at all.
	OwnershipLost
	// OwnershipUnknown means the server could not be reached to find out.
	OwnershipUnknown
)

// ShouldRestartContainers decides whether a backup's deferred cleanup should
// restart the Project's containers.
//
// "Always restart in a defer" is wrong. While containers are stopped for a
// backup the control plane may have reassigned the Project; restarting then
// would leave two nodes running the same workload against divergent copies of
// the data. So the decision depends on why the run ended and on whether this
// node still owns the Project:
//
//	tar/upload failure, still owned      → restart
//	timeout or cancellation, still owned → restart
//	agent shutting down, still owned     → restart (best effort)
//	reassigned to another node           → do NOT restart
//	moved to Terminating                 → do NOT restart, yield to teardown
//	drain completed / assignment lost    → do NOT restart
//
// When ownership cannot be determined the containers are restarted:
//
//   - Restarting is correct in the common case (a brief blip during which
//     nothing was reassigned). If the Project really did move, the containers
//     started here are orphans — but the agent's orphan sweep now removes them
//     once the Project has been absent from ListProjectsForReconcile for the
//     grace period (see internal/agent/orphan.go), so the mistake is temporary
//     rather than permanent.
//   - Refusing to restart is wrong in the common case: it turns a transient
//     control-plane blip into a stopped service that nothing brings back,
//     because a Failed Project is not currently auto-recovered. Fixing that
//     is NT-B's job.
//
// Restarting is chosen because it is correct in the common case, and because
// the case where it is wrong is now self-healing while the alternative's
// failure is not.
func ShouldRestartContainers(reason ExitReason, ownership Ownership) bool {
	switch ownership {
	case OwnershipReassigned, OwnershipTerminating, OwnershipLost:
		return false
	}

	// Ownership is Retained or Unknown; restart unless the run succeeded, in
	// which case the normal post-backup restart path applies anyway.
	switch reason {
	case ExitSuccess, ExitFailure, ExitTimeout, ExitCancelled, ExitAgentShutdown:
		return true
	default:
		// Defensive: an unrecognised reason should still leave the service
		// running rather than silently down.
		return true
	}
}

// String renders an ExitReason for logs and conditions.
func (r ExitReason) String() string {
	switch r {
	case ExitSuccess:
		return "Success"
	case ExitFailure:
		return "Failure"
	case ExitTimeout:
		return "Timeout"
	case ExitCancelled:
		return "Cancelled"
	case ExitAgentShutdown:
		return "AgentShutdown"
	default:
		return fmt.Sprintf("ExitReason(%d)", int(r))
	}
}

// String renders an Ownership for logs and conditions.
func (o Ownership) String() string {
	switch o {
	case OwnershipRetained:
		return "Retained"
	case OwnershipReassigned:
		return "Reassigned"
	case OwnershipTerminating:
		return "Terminating"
	case OwnershipLost:
		return "Lost"
	case OwnershipUnknown:
		return "Unknown"
	default:
		return fmt.Sprintf("Ownership(%d)", int(o))
	}
}
