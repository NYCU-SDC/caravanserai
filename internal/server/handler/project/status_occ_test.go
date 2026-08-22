package project

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/handler"
	"NYCU-SDC/caravanserai/internal/store"

	"github.com/NYCU-SDC/summer/pkg/problem"
	"go.uber.org/zap"
)

// fakeProjectStore embeds store.ProjectStore so only the methods these tests
// exercise need implementing. Anything else panics on a nil interface call,
// which is the intent: a handler quietly reaching for another store method
// should fail the test rather than pass through an accidental stub.
type fakeProjectStore struct {
	store.ProjectStore

	status v1.ProjectStatus

	// updateErr, when set, is what UpdateProjectStatusWithRetry returns instead
	// of applying the mutation.
	updateErr error

	// mutations counts how many times the handler's closure was applied, and
	// applied records the resulting status.
	mutations int
	applied   v1.ProjectStatus
}

func (f *fakeProjectStore) GetProject(_ context.Context, name string) (*v1.Project, error) {
	return &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: name},
		Status:     f.status,
	}, nil
}

func (f *fakeProjectStore) UpdateProjectStatusWithRetry(
	_ context.Context,
	_ string,
	mutate func(*v1.ProjectStatus) error,
) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	next := f.status
	next.Conditions = append([]v1.Condition(nil), f.status.Conditions...)
	if err := mutate(&next); err != nil {
		return err
	}
	f.mutations++
	f.applied = next
	f.status = next
	return nil
}

func newTestHandler(s store.ProjectStore) *Handler {
	return NewHandler(
		zap.NewNop(),
		s,
		problem.NewWithMapping(handler.NewProblemMapping()),
	)
}

func patchStatusRequest(t *testing.T, h *Handler, name, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/projects/"+name+"/status", strings.NewReader(body))
	req.SetPathValue("name", name)
	rec := httptest.NewRecorder()
	h.patchStatus(rec, req)
	return rec
}

// The whole point of the migration: the agent reports a phase, and everything
// it did not mention survives. Before, the handler wrote back a status it had
// read moments earlier, discarding whatever a controller committed in between.
func TestPatchStatusPreservesFieldsItDoesNotTouch(t *testing.T) {
	s := &fakeProjectStore{
		status: v1.ProjectStatus{
			Phase:   v1.ProjectPhaseScheduled,
			NodeRef: "node-a",
			Conditions: []v1.Condition{
				{Type: v1.ConditionTypeNotReadyAt, Status: v1.ConditionTrue, Reason: "NodeNotReady"},
			},
		},
	}
	h := newTestHandler(s)

	rec := patchStatusRequest(t, h, "demo", `{"phase":"Running"}`)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
	if s.applied.Phase != v1.ProjectPhaseRunning {
		t.Errorf("phase = %q, want Running", s.applied.Phase)
	}
	if s.applied.NodeRef != "node-a" {
		t.Errorf("nodeRef = %q, want it preserved as node-a", s.applied.NodeRef)
	}
	if len(s.applied.Conditions) != 1 || s.applied.Conditions[0].Type != v1.ConditionTypeNotReadyAt {
		t.Errorf("conditions = %v, want the controller-owned NotReadyAt condition preserved",
			s.applied.Conditions)
	}
}

func TestPatchStatusUpsertsPhaseConditionWithoutDroppingOthers(t *testing.T) {
	s := &fakeProjectStore{
		status: v1.ProjectStatus{
			Phase: v1.ProjectPhaseRunning,
			Conditions: []v1.Condition{
				{Type: v1.ConditionTypeMaintenance, Status: v1.ConditionTrue},
				{Type: v1.ConditionTypePhase, Status: v1.ConditionTrue, Reason: "Started"},
			},
		},
	}
	h := newTestHandler(s)

	rec := patchStatusRequest(t, h, "demo",
		`{"phase":"Failed","reason":"ContainerExited","message":"exit 1"}`)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
	if len(s.applied.Conditions) != 2 {
		t.Fatalf("conditions = %v, want the Phase condition replaced in place, not appended",
			s.applied.Conditions)
	}
	var phaseCond, maintenanceCond *v1.Condition
	for i := range s.applied.Conditions {
		switch s.applied.Conditions[i].Type {
		case v1.ConditionTypePhase:
			phaseCond = &s.applied.Conditions[i]
		case v1.ConditionTypeMaintenance:
			maintenanceCond = &s.applied.Conditions[i]
		}
	}
	if maintenanceCond == nil {
		t.Error("the Maintenance condition was dropped")
	}
	if phaseCond == nil || phaseCond.Reason != "ContainerExited" {
		t.Errorf("phase condition = %v, want reason ContainerExited", phaseCond)
	}
}

func TestPatchStatusMapsStoreErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		storeErr error
		want     int
	}{
		{"missing project", store.ErrNotFound, http.StatusNotFound},
		{"exhausted retries", store.ErrVersionConflict, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &fakeProjectStore{updateErr: tc.storeErr}
			h := newTestHandler(s)

			rec := patchStatusRequest(t, h, "demo", `{"phase":"Running"}`)

			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// A database fault must not be reported as 404. A caller told the Project is
// gone may act on that — a controller dropping it from its queue, an operator
// concluding it was deleted — and nothing records that the cause was transient.
func TestPatchStatusDoesNotReportDatabaseFaultsAsNotFound(t *testing.T) {
	s := &fakeProjectStore{updateErr: errors.New("connection reset by peer")}
	h := newTestHandler(s)

	rec := patchStatusRequest(t, h, "demo", `{"phase":"Running"}`)

	if rec.Code == http.StatusNotFound {
		t.Errorf("a database fault was reported as 404: %s", rec.Body.String())
	}
	if rec.Code < 500 {
		t.Errorf("status = %d, want a 5xx for an unclassified store failure", rec.Code)
	}
}

func TestDeleteProjectMarksTerminatingPreservingOtherFields(t *testing.T) {
	s := &fakeProjectStore{
		status: v1.ProjectStatus{
			Phase:      v1.ProjectPhaseRunning,
			NodeRef:    "node-b",
			Conditions: []v1.Condition{{Type: v1.ConditionTypePhase, Status: v1.ConditionTrue, Reason: "Started"}},
		},
	}
	h := newTestHandler(s)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/demo", nil)
	req.SetPathValue("name", "demo")
	rec := httptest.NewRecorder()
	h.deleteProject(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rec.Code, rec.Body.String())
	}
	if s.applied.Phase != v1.ProjectPhaseTerminating {
		t.Errorf("phase = %q, want Terminating", s.applied.Phase)
	}
	if s.applied.NodeRef != "node-b" {
		t.Errorf("nodeRef = %q, want it preserved", s.applied.NodeRef)
	}
	if len(s.applied.Conditions) != 1 {
		t.Errorf("conditions = %v, want them preserved untouched", s.applied.Conditions)
	}
}
