package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/store"
)

// fakeStatusCAS stands in for the database so the retry policy can be driven
// through every branch without one. Recording the calls matters as much as the
// outcome: an implementation that resends the same stale payload three times
// returns the same errors as one that re-reads, and only the call log tells
// them apart.
type fakeStatusCAS struct {
	status  v1.ProjectStatus
	version int64

	// loadErrs and swapErrs are consumed one per call; a nil entry, or running
	// off the end, means success.
	loadErrs []error
	swapErrs []error

	loads int
	swaps int

	loadedVersions  []int64
	swappedVersions []int64
	swapped         []v1.ProjectStatus

	// beforeLoad runs before each load, letting a test simulate another writer
	// committing between attempts.
	beforeLoad func(f *fakeStatusCAS, attempt int)
}

func (f *fakeStatusCAS) loadProjectStatus(_ context.Context, _ string) (v1.ProjectStatus, int64, error) {
	if f.beforeLoad != nil {
		f.beforeLoad(f, f.loads)
	}
	f.loads++
	if len(f.loadErrs) >= f.loads {
		if err := f.loadErrs[f.loads-1]; err != nil {
			return v1.ProjectStatus{}, 0, err
		}
	}
	f.loadedVersions = append(f.loadedVersions, f.version)
	return copyProjectStatus(f.status), f.version, nil
}

func (f *fakeStatusCAS) swapProjectStatus(_ context.Context, _ string, expectedVersion int64, next v1.ProjectStatus) error {
	f.swaps++
	f.swappedVersions = append(f.swappedVersions, expectedVersion)
	f.swapped = append(f.swapped, copyProjectStatus(next))

	if len(f.swapErrs) >= f.swaps {
		if err := f.swapErrs[f.swaps-1]; err != nil {
			return err
		}
	}
	f.status = copyProjectStatus(next)
	f.version++
	return nil
}

// versionConflict mirrors what the real CAS returns, so tests exercise the same
// errors.Is path production does rather than the bare sentinel.
func versionConflict() error {
	return errors.Join(store.ErrVersionConflict, errors.New(`project "demo"`))
}

func setPhase(p v1.ProjectPhase) func(*v1.ProjectStatus) error {
	return func(s *v1.ProjectStatus) error {
		s.Phase = p
		return nil
	}
}

func countingMutate(calls *int, inner func(*v1.ProjectStatus) error) func(*v1.ProjectStatus) error {
	return func(s *v1.ProjectStatus) error {
		*calls++
		return inner(s)
	}
}

func TestRetryFirstAttemptSucceeds(t *testing.T) {
	f := &fakeStatusCAS{status: v1.ProjectStatus{Phase: v1.ProjectPhasePending}, version: 7}
	mutations := 0

	committed, err := retryProjectStatusUpdate(context.Background(), f, "demo",
		countingMutate(&mutations, setPhase(v1.ProjectPhaseRunning)))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !committed {
		t.Error("expected committed = true")
	}
	if f.loads != 1 || mutations != 1 || f.swaps != 1 {
		t.Errorf("loads=%d mutations=%d swaps=%d, want 1/1/1", f.loads, mutations, f.swaps)
	}
	if f.swappedVersions[0] != 7 {
		t.Errorf("swapped at version %d, want the version that was read (7)", f.swappedVersions[0])
	}
}

func TestRetryConflictTwiceThenSucceeds(t *testing.T) {
	f := &fakeStatusCAS{
		status:   v1.ProjectStatus{Phase: v1.ProjectPhasePending},
		version:  1,
		swapErrs: []error{versionConflict(), versionConflict()},
		// Each failed attempt leaves the row advanced by somebody else.
		beforeLoad: func(f *fakeStatusCAS, attempt int) {
			if attempt > 0 {
				f.version++
			}
		},
	}
	mutations := 0

	committed, err := retryProjectStatusUpdate(context.Background(), f, "demo",
		countingMutate(&mutations, setPhase(v1.ProjectPhaseRunning)))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !committed {
		t.Error("expected committed = true")
	}
	if mutations != 3 {
		t.Errorf("mutate called %d times, want 3 — it must re-run on each freshly read state", mutations)
	}
	for i := 1; i < len(f.swappedVersions); i++ {
		if f.swappedVersions[i] <= f.swappedVersions[i-1] {
			t.Errorf("swap versions %v are not strictly increasing; a retry resent a stale version",
				f.swappedVersions)
			break
		}
	}
}

func TestRetryExhaustsAfterThreeAttempts(t *testing.T) {
	f := &fakeStatusCAS{
		status:   v1.ProjectStatus{Phase: v1.ProjectPhasePending},
		version:  1,
		swapErrs: []error{versionConflict(), versionConflict(), versionConflict()},
	}
	mutations := 0

	committed, err := retryProjectStatusUpdate(context.Background(), f, "demo",
		countingMutate(&mutations, setPhase(v1.ProjectPhaseRunning)))

	if !errors.Is(err, store.ErrVersionConflict) {
		t.Fatalf("err = %v, want ErrVersionConflict", err)
	}
	if committed {
		t.Error("expected committed = false")
	}
	if f.loads != 3 || mutations != 3 || f.swaps != 3 {
		t.Errorf("loads=%d mutations=%d swaps=%d, want exactly 3/3/3 — the first attempt plus two retries",
			f.loads, mutations, f.swaps)
	}
}

func TestRetryStopsWhenProjectDisappears(t *testing.T) {
	f := &fakeStatusCAS{
		status:   v1.ProjectStatus{Phase: v1.ProjectPhasePending},
		version:  1,
		swapErrs: []error{versionConflict()},
		loadErrs: []error{nil, store.ErrNotFound},
	}

	committed, err := retryProjectStatusUpdate(context.Background(), f, "demo", setPhase(v1.ProjectPhaseRunning))

	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if committed {
		t.Error("expected committed = false")
	}
	if f.swaps != 1 {
		t.Errorf("swaps=%d, want 1 — a vanished row must not be retried", f.swaps)
	}
}

func TestRetryReturnsMutationErrorWithoutWriting(t *testing.T) {
	sentinel := errors.New("mutation refused")
	f := &fakeStatusCAS{status: v1.ProjectStatus{Phase: v1.ProjectPhasePending}, version: 1}

	committed, err := retryProjectStatusUpdate(context.Background(), f, "demo",
		func(*v1.ProjectStatus) error { return sentinel })

	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the mutation's own error", err)
	}
	if committed || f.swaps != 0 {
		t.Errorf("committed=%v swaps=%d, want false/0", committed, f.swaps)
	}
}

func TestRetryDoesNotRetryNonConflictErrors(t *testing.T) {
	dbErr := errors.New("connection reset by peer")

	for _, tc := range []struct {
		name string
		f    *fakeStatusCAS
	}{
		{"swap fails", &fakeStatusCAS{version: 1, swapErrs: []error{dbErr}}},
		{"load fails", &fakeStatusCAS{version: 1, loadErrs: []error{dbErr}}},
		{"context cancelled on load", &fakeStatusCAS{version: 1, loadErrs: []error{context.Canceled}}},
		{"context cancelled on swap", &fakeStatusCAS{version: 1, swapErrs: []error{context.Canceled}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			committed, err := retryProjectStatusUpdate(context.Background(), tc.f, "demo",
				setPhase(v1.ProjectPhaseRunning))

			if err == nil {
				t.Fatal("expected an error")
			}
			if errors.Is(err, store.ErrVersionConflict) {
				t.Errorf("err = %v, must not be reported as a version conflict", err)
			}
			if committed {
				t.Error("expected committed = false")
			}
			if tc.f.loads > 1 {
				t.Errorf("loads=%d, want 1 — only a version conflict may be retried", tc.f.loads)
			}
		})
	}
}

func TestRetryNoOpWritesNothing(t *testing.T) {
	f := &fakeStatusCAS{status: v1.ProjectStatus{Phase: v1.ProjectPhaseRunning}, version: 4}

	committed, err := retryProjectStatusUpdate(context.Background(), f, "demo",
		setPhase(v1.ProjectPhaseRunning))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if committed {
		t.Error("committed = true for a no-op; the caller would publish an event saying nothing changed")
	}
	if f.swaps != 0 {
		t.Errorf("swaps=%d, want 0", f.swaps)
	}
	if f.version != 4 {
		t.Errorf("version advanced to %d on a no-op", f.version)
	}
}

// The point of retrying is to re-apply the mutation to whatever is there now,
// not to resend what was there before. If another writer added a condition
// while we were losing the race, our commit must carry it.
func TestRetryPreservesAnotherWritersChange(t *testing.T) {
	other := v1.Condition{Type: "ContainersReady", Status: v1.ConditionTrue}
	f := &fakeStatusCAS{
		status:   v1.ProjectStatus{Phase: v1.ProjectPhasePending},
		version:  1,
		swapErrs: []error{versionConflict()},
		beforeLoad: func(f *fakeStatusCAS, attempt int) {
			if attempt == 1 {
				f.status.Conditions = append(f.status.Conditions, other)
				f.version++
			}
		},
	}

	if _, err := retryProjectStatusUpdate(context.Background(), f, "demo",
		setPhase(v1.ProjectPhaseRunning)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	final := f.swapped[len(f.swapped)-1]
	if final.Phase != v1.ProjectPhaseRunning {
		t.Errorf("phase = %q, want Running", final.Phase)
	}
	if len(final.Conditions) != 1 || final.Conditions[0].Type != other.Type {
		t.Errorf("conditions = %v, want the condition the other writer added", final.Conditions)
	}
}

// A copy that shares the Conditions backing array mutates the "before" snapshot
// alongside the candidate, so every in-place edit compares equal and silently
// writes nothing. Appending a condition would not catch it; editing one does.
func TestRetryDetectsInPlaceConditionEdit(t *testing.T) {
	f := &fakeStatusCAS{
		status: v1.ProjectStatus{
			Phase:      v1.ProjectPhaseRunning,
			Conditions: []v1.Condition{{Type: "Phase", Status: v1.ConditionTrue, Reason: "Started"}},
		},
		version: 2,
	}

	committed, err := retryProjectStatusUpdate(context.Background(), f, "demo",
		func(s *v1.ProjectStatus) error {
			s.Conditions[0].Reason = "Restarted"
			return nil
		})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !committed || f.swaps != 1 {
		t.Fatalf("committed=%v swaps=%d — an in-place condition edit was seen as no change, "+
			"which means the status copy aliases its Conditions slice", committed, f.swaps)
	}
}

// mutate may run more than once. Running it twice must land in the same place
// as running it once, or a retry quietly changes the outcome.
func TestMutationIsIdempotentAcrossAttempts(t *testing.T) {
	add := func(s *v1.ProjectStatus) error {
		for i := range s.Conditions {
			if s.Conditions[i].Type == "Ready" {
				s.Conditions[i].Status = v1.ConditionTrue
				return nil
			}
		}
		s.Conditions = append(s.Conditions, v1.Condition{Type: "Ready", Status: v1.ConditionTrue})
		return nil
	}

	once := v1.ProjectStatus{Phase: v1.ProjectPhaseRunning}
	if err := add(&once); err != nil {
		t.Fatal(err)
	}
	twice := copyProjectStatus(once)
	if err := add(&twice); err != nil {
		t.Fatal(err)
	}

	equal, err := statusEqual(once, twice)
	if err != nil {
		t.Fatal(err)
	}
	if !equal {
		t.Errorf("applying the mutation twice differs from applying it once:\n once  = %+v\n twice = %+v", once, twice)
	}
}

// statusEqual compares the persisted form. A status that came back from the
// database and an identical one built in-process must agree — reflect.DeepEqual
// does not, because Condition carries time.Time.
func TestStatusEqualSurvivesJSONRoundTrip(t *testing.T) {
	at := time.Date(2026, 8, 22, 10, 30, 0, 123456789, time.UTC)
	original := v1.ProjectStatus{
		Phase:   v1.ProjectPhaseRunning,
		NodeRef: "node-a",
		Conditions: []v1.Condition{{
			Type:               "Phase",
			Status:             v1.ConditionTrue,
			LastHeartbeatTime:  at,
			LastTransitionTime: at,
			Reason:             "Started",
		}},
	}

	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var roundTripped v1.ProjectStatus
	if err := json.Unmarshal(raw, &roundTripped); err != nil {
		t.Fatal(err)
	}

	equal, err := statusEqual(original, roundTripped)
	if err != nil {
		t.Fatal(err)
	}
	if !equal {
		t.Error("a status that round-tripped through JSON no longer compares equal; " +
			"the no-op guard would fire on every repeated report")
	}
}

// time.Now() carries a monotonic reading that JSON cannot represent. The guard
// must not treat that difference as a change.
func TestStatusEqualIgnoresMonotonicClock(t *testing.T) {
	withMonotonic := time.Now()
	wall := withMonotonic.Round(0)

	a := v1.ProjectStatus{Conditions: []v1.Condition{{Type: "Ready", LastTransitionTime: withMonotonic}}}
	b := v1.ProjectStatus{Conditions: []v1.Condition{{Type: "Ready", LastTransitionTime: wall}}}

	equal, err := statusEqual(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if !equal {
		t.Error("a monotonic clock reading changed the comparison; only the instant should matter")
	}
}

func TestSentinelErrorsAreDistinct(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		notIs []error
	}{
		{"version conflict", store.ErrVersionConflict, []error{store.ErrNotFound, store.ErrConflictState, store.ErrAlreadyExists}},
		{"not found", store.ErrNotFound, []error{store.ErrVersionConflict, store.ErrConflictState}},
		{"conflict state", store.ErrConflictState, []error{store.ErrVersionConflict, store.ErrNotFound}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := versionConflictLike(tc.err)
			if !errors.Is(wrapped, tc.err) {
				t.Fatalf("wrapped error no longer matches its own sentinel")
			}
			for _, other := range tc.notIs {
				if errors.Is(wrapped, other) {
					t.Errorf("%v also matches %v; retry logic could not tell them apart", tc.err, other)
				}
			}
		})
	}
}

func versionConflictLike(sentinel error) error {
	return errors.Join(sentinel, errors.New(`project "demo"`))
}
