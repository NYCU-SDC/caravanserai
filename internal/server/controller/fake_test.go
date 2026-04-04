package controller

import (
	"context"
	"sync"
	"time"

	"NYCU-SDC/caravanserai/internal/store"
)

// ---------------------------------------------------------------------------
// fakeClock — implements Clock for deterministic tests
// ---------------------------------------------------------------------------

// testBaseTime is a fixed reference point shared by all controller tests so
// that assertions are fully deterministic and independent of wall-clock time.
var testBaseTime = time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

// fakeClock is a test Clock whose time is controlled by the test.  Now()
// returns the value of Time, and Since(t) returns Time.Sub(t).
type fakeClock struct {
	Time time.Time
}

var _ Clock = (*fakeClock)(nil)

func (fc *fakeClock) Now() time.Time                  { return fc.Time }
func (fc *fakeClock) Since(t time.Time) time.Duration { return fc.Time.Sub(t) }

// newFakeClock returns a fakeClock pinned to testBaseTime.
func newFakeClock() *fakeClock { return &fakeClock{Time: testBaseTime} }

// ---------------------------------------------------------------------------
// Call-recording types
// ---------------------------------------------------------------------------

// setNodeStateCall records a single invocation of SetNodeState.
type setNodeStateCall struct {
	Name    string
	State   NodeState
	Reason  string
	Message string
}

// setProjectScheduledCall records a single invocation of SetProjectScheduled.
type setProjectScheduledCall struct {
	Name    string
	NodeRef string
}

// deleteProjectCall records a single invocation of DeleteProject.
type deleteProjectCall struct {
	Name string
}

// setProjectPendingCall records a single invocation of SetProjectPending.
type setProjectPendingCall struct {
	Name string
}

// setTerminatingAtCall records a single invocation of SetTerminatingAt.
type setTerminatingAtCall struct {
	Name string
	At   time.Time
}

// setNotReadyAtCall records a single invocation of SetNotReadyAt.
type setNotReadyAtCall struct {
	Name string
	At   time.Time
}

// forceTerminatedCall records a single invocation of ForceTerminated.
type forceTerminatedCall struct {
	Name string
}

// ---------------------------------------------------------------------------
// fakeNodeStore — implements NodeStore
// ---------------------------------------------------------------------------

type fakeNodeStore struct {
	mu    sync.Mutex
	nodes map[string]NodeStatusSnapshot
	errs  map[string]error // per-name error injection

	SetNodeStateCalls []setNodeStateCall
}

var _ NodeStore = (*fakeNodeStore)(nil)

func newFakeNodeStore() *fakeNodeStore {
	return &fakeNodeStore{
		nodes: make(map[string]NodeStatusSnapshot),
		errs:  make(map[string]error),
	}
}

func (f *fakeNodeStore) ListNodeNames(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	names := make([]string, 0, len(f.nodes))
	for n := range f.nodes {
		names = append(names, n)
	}
	return names, nil
}

func (f *fakeNodeStore) GetNodeStatus(_ context.Context, name string) (NodeStatusSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err, ok := f.errs[name]; ok {
		return NodeStatusSnapshot{}, err
	}
	snap, ok := f.nodes[name]
	if !ok {
		return NodeStatusSnapshot{}, store.ErrNotFound
	}
	return snap, nil
}

func (f *fakeNodeStore) SetNodeState(_ context.Context, name string, state NodeState, reason, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	// 1. Error injection.
	if err, ok := f.errs[name]; ok {
		return err
	}

	// 2. Record the call for later assertions.
	f.SetNodeStateCalls = append(f.SetNodeStateCalls, setNodeStateCall{
		Name:    name,
		State:   state,
		Reason:  reason,
		Message: message,
	})

	// 3. Update the in-memory state so subsequent reads reflect the change.
	if snap, ok := f.nodes[name]; ok {
		snap.State = state
		f.nodes[name] = snap
	}
	return nil
}

// ---------------------------------------------------------------------------
// fakeSchedulerProjectStore — implements SchedulerProjectStore
// ---------------------------------------------------------------------------

type fakeSchedulerProjectStore struct {
	mu       sync.Mutex
	projects map[string]schedulerProjectRecord
	errs     map[string]error

	SetProjectScheduledCalls []setProjectScheduledCall
}

type schedulerProjectRecord struct {
	Phase   ProjectPhase
	NodeRef string
}

var _ SchedulerProjectStore = (*fakeSchedulerProjectStore)(nil)

func newFakeSchedulerProjectStore() *fakeSchedulerProjectStore {
	return &fakeSchedulerProjectStore{
		projects: make(map[string]schedulerProjectRecord),
		errs:     make(map[string]error),
	}
}

func (f *fakeSchedulerProjectStore) ListProjectNamesByPhase(_ context.Context, phase ProjectPhase) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var names []string
	for n, r := range f.projects {
		if r.Phase == phase {
			names = append(names, n)
		}
	}
	return names, nil
}

func (f *fakeSchedulerProjectStore) GetProjectPhase(_ context.Context, name string) (ProjectPhase, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err, ok := f.errs[name]; ok {
		return "", "", err
	}
	r, ok := f.projects[name]
	if !ok {
		return "", "", store.ErrNotFound
	}
	return r.Phase, r.NodeRef, nil
}

func (f *fakeSchedulerProjectStore) SetProjectScheduled(_ context.Context, name, nodeRef string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err, ok := f.errs[name]; ok {
		return err
	}

	f.SetProjectScheduledCalls = append(f.SetProjectScheduledCalls, setProjectScheduledCall{
		Name:    name,
		NodeRef: nodeRef,
	})

	if r, ok := f.projects[name]; ok {
		r.Phase = ProjectPhaseScheduled
		r.NodeRef = nodeRef
		f.projects[name] = r
	}
	return nil
}

// ---------------------------------------------------------------------------
// fakeSchedulerNodeStore — implements SchedulerNodeStore
// ---------------------------------------------------------------------------

type fakeSchedulerNodeStore struct {
	mu         sync.Mutex
	readyNodes []string
}

var _ SchedulerNodeStore = (*fakeSchedulerNodeStore)(nil)

func newFakeSchedulerNodeStore(ready ...string) *fakeSchedulerNodeStore {
	return &fakeSchedulerNodeStore{readyNodes: ready}
}

func (f *fakeSchedulerNodeStore) ListReadyNodeNames(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, len(f.readyNodes))
	copy(out, f.readyNodes)
	return out, nil
}

// ---------------------------------------------------------------------------
// fakeTerminationProjectStore — implements TerminationProjectStore
// ---------------------------------------------------------------------------

type fakeTerminationProjectStore struct {
	mu       sync.Mutex
	projects map[string]terminationProjectRecord
	errs     map[string]error

	DeleteProjectCalls []deleteProjectCall
}

type terminationProjectRecord struct {
	Phase   ProjectPhase
	NodeRef string
}

var _ TerminationProjectStore = (*fakeTerminationProjectStore)(nil)

func newFakeTerminationProjectStore() *fakeTerminationProjectStore {
	return &fakeTerminationProjectStore{
		projects: make(map[string]terminationProjectRecord),
		errs:     make(map[string]error),
	}
}

func (f *fakeTerminationProjectStore) ListProjectNamesByPhase(_ context.Context, phase ProjectPhase) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var names []string
	for n, r := range f.projects {
		if r.Phase == phase {
			names = append(names, n)
		}
	}
	return names, nil
}

func (f *fakeTerminationProjectStore) GetProjectPhase(_ context.Context, name string) (ProjectPhase, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err, ok := f.errs[name]; ok {
		return "", "", err
	}
	r, ok := f.projects[name]
	if !ok {
		return "", "", store.ErrNotFound
	}
	return r.Phase, r.NodeRef, nil
}

func (f *fakeTerminationProjectStore) DeleteProject(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err, ok := f.errs[name]; ok {
		return err
	}

	f.DeleteProjectCalls = append(f.DeleteProjectCalls, deleteProjectCall{Name: name})

	if _, ok := f.projects[name]; !ok {
		return store.ErrNotFound
	}
	delete(f.projects, name)
	return nil
}

// ---------------------------------------------------------------------------
// fakeReschedulerProjectStore — implements ReschedulerProjectStore
// ---------------------------------------------------------------------------

type fakeReschedulerProjectStore struct {
	mu       sync.Mutex
	projects map[string]*ProjectSnapshot
	errs     map[string]error

	SetProjectPendingCalls []setProjectPendingCall
	SetTerminatingAtCalls  []setTerminatingAtCall
	SetNotReadyAtCalls     []setNotReadyAtCall
	ForceTerminatedCalls   []forceTerminatedCall
}

var _ ReschedulerProjectStore = (*fakeReschedulerProjectStore)(nil)

func newFakeReschedulerProjectStore() *fakeReschedulerProjectStore {
	return &fakeReschedulerProjectStore{
		projects: make(map[string]*ProjectSnapshot),
		errs:     make(map[string]error),
	}
}

func (f *fakeReschedulerProjectStore) ListProjectsByNodeRef(
	_ context.Context,
	nodeRef string,
	phases []ProjectPhase,
) ([]*ProjectSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	phaseSet := make(map[ProjectPhase]bool, len(phases))
	for _, ph := range phases {
		phaseSet[ph] = true
	}

	var result []*ProjectSnapshot
	for _, p := range f.projects {
		if p.NodeRef == nodeRef && phaseSet[p.Phase] {
			// Return a copy to prevent test mutations from affecting the store.
			cp := *p
			cp.Conditions = make([]ConditionSnapshot, len(p.Conditions))
			copy(cp.Conditions, p.Conditions)
			result = append(result, &cp)
		}
	}
	return result, nil
}

func (f *fakeReschedulerProjectStore) SetProjectPending(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err, ok := f.errs[name]; ok {
		return err
	}

	f.SetProjectPendingCalls = append(f.SetProjectPendingCalls, setProjectPendingCall{Name: name})

	if p, ok := f.projects[name]; ok {
		p.Phase = ProjectPhasePending
		p.NodeRef = ""
	}
	return nil
}

func (f *fakeReschedulerProjectStore) SetTerminatingAt(_ context.Context, name string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err, ok := f.errs[name]; ok {
		return err
	}

	f.SetTerminatingAtCalls = append(f.SetTerminatingAtCalls, setTerminatingAtCall{Name: name, At: at})

	if p, ok := f.projects[name]; ok {
		// Replace or add the TerminatingAt condition.
		replaced := false
		for i, c := range p.Conditions {
			if c.Type == ConditionTypeTerminatingAt {
				p.Conditions[i].LastTransitionTime = at
				replaced = true
				break
			}
		}
		if !replaced {
			p.Conditions = append(p.Conditions, ConditionSnapshot{
				Type:               ConditionTypeTerminatingAt,
				LastTransitionTime: at,
			})
		}
	}
	return nil
}

func (f *fakeReschedulerProjectStore) SetNotReadyAt(_ context.Context, name string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err, ok := f.errs[name]; ok {
		return err
	}

	f.SetNotReadyAtCalls = append(f.SetNotReadyAtCalls, setNotReadyAtCall{Name: name, At: at})

	if p, ok := f.projects[name]; ok {
		// Replace or add the NotReadyAt condition.
		replaced := false
		for i, c := range p.Conditions {
			if c.Type == ConditionTypeNotReadyAt {
				p.Conditions[i].LastTransitionTime = at
				replaced = true
				break
			}
		}
		if !replaced {
			p.Conditions = append(p.Conditions, ConditionSnapshot{
				Type:               ConditionTypeNotReadyAt,
				LastTransitionTime: at,
			})
		}
	}
	return nil
}

func (f *fakeReschedulerProjectStore) ForceTerminated(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err, ok := f.errs[name]; ok {
		return err
	}

	f.ForceTerminatedCalls = append(f.ForceTerminatedCalls, forceTerminatedCall{Name: name})

	if p, ok := f.projects[name]; ok {
		p.Phase = ProjectPhaseTerminated
	}
	return nil
}

// ---------------------------------------------------------------------------
// fakeReschedulerNodeStore — implements ReschedulerNodeStore
// ---------------------------------------------------------------------------

type fakeReschedulerNodeStore struct {
	mu    sync.Mutex
	nodes map[string]NodeStatusSnapshot
	errs  map[string]error
}

var _ ReschedulerNodeStore = (*fakeReschedulerNodeStore)(nil)

func newFakeReschedulerNodeStore() *fakeReschedulerNodeStore {
	return &fakeReschedulerNodeStore{
		nodes: make(map[string]NodeStatusSnapshot),
		errs:  make(map[string]error),
	}
}

func (f *fakeReschedulerNodeStore) GetNodeStatus(_ context.Context, name string) (NodeStatusSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err, ok := f.errs[name]; ok {
		return NodeStatusSnapshot{}, err
	}
	snap, ok := f.nodes[name]
	if !ok {
		return NodeStatusSnapshot{}, store.ErrNotFound
	}
	return snap, nil
}

func (f *fakeReschedulerNodeStore) ListNotReadyNodeNames(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var names []string
	for n, snap := range f.nodes {
		if snap.State == NodeStateNotReady {
			names = append(names, n)
		}
	}
	return names, nil
}
