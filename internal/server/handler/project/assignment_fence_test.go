package project

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/handler"
	"NYCU-SDC/caravanserai/internal/store"

	"github.com/NYCU-SDC/summer/pkg/problem"
	"go.uber.org/zap"
)

// fenceFakeStore records which write path the handler chose and lets a test
// force a store error. It embeds store.ProjectStore so unrelated methods panic.
type fenceFakeStore struct {
	store.ProjectStore

	getProject *v1.Project
	getErr     error

	fencedStatusCalls   int
	unfencedStatusCalls int
	uidStatusCalls      int
	lastRef             store.AssignmentRef
	lastUID             string

	deleteCalls      int
	terminatingCalls int
	created          *v1.Project

	// changedBeforeDelete, when set, replaces getProject at the moment
	// FinalizeProject locks the row: a write that committed between the
	// handler's read and its delete, such as the first assignment.
	changedBeforeDelete *v1.Project
	// lastApplied is the status the last UID-pinned write produced.
	lastApplied v1.ProjectStatus

	returnErr error
}

func (f *fenceFakeStore) GetProject(_ context.Context, name string) (*v1.Project, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.getProject != nil {
		return f.getProject, nil
	}
	return &v1.Project{ObjectMeta: v1.ObjectMeta{Name: name}}, nil
}

func (f *fenceFakeStore) ReportProjectStatusFenced(_ context.Context, _ string, ref store.AssignmentRef, mutate func(*v1.ProjectStatus) error) error {
	f.fencedStatusCalls++
	f.lastRef = ref
	if f.returnErr != nil {
		return f.returnErr
	}
	st := f.reportedOn()
	return mutate(&st)
}

func (f *fenceFakeStore) UpdateProjectStatusWithRetry(_ context.Context, _ string, mutate func(*v1.ProjectStatus) error) error {
	f.unfencedStatusCalls++
	if f.returnErr != nil {
		return f.returnErr
	}
	st := f.reportedOn()
	return mutate(&st)
}

// reportedOn is the status an Agent report is applied to: getProject's when a
// test sets one, otherwise a freshly Scheduled Project, the phase an Agent
// first reports on.
func (f *fenceFakeStore) reportedOn() v1.ProjectStatus {
	if f.getProject != nil {
		return f.getProject.Status
	}
	return v1.ProjectStatus{Phase: v1.ProjectPhaseScheduled, NodeRef: "node-a", AssignmentGeneration: 3}
}

func (f *fenceFakeStore) UpdateProjectStatusForUID(_ context.Context, _ string, uid string, mutate func(*v1.ProjectStatus) error) error {
	f.uidStatusCalls++
	f.lastUID = uid
	if f.returnErr != nil {
		return f.returnErr
	}
	st := v1.ProjectStatus{}
	if f.getProject != nil {
		st = f.getProject.Status
	}
	if err := mutate(&st); err != nil {
		return err
	}
	f.lastApplied = st
	return nil
}

func (f *fenceFakeStore) CreateProject(_ context.Context, p *v1.Project) error {
	f.created = p
	return nil
}

// FinalizeProject mirrors the store: the predicate is evaluated on the row as
// it is when locked, which is changedBeforeDelete if a test set one.
func (f *fenceFakeStore) FinalizeProject(_ context.Context, name, uid string, blocker func(*v1.Project) string) error {
	if f.changedBeforeDelete != nil {
		f.getProject, f.changedBeforeDelete = f.changedBeforeDelete, nil
	}
	current, _ := f.GetProject(context.Background(), name)
	if current.ObjectMeta.UID != uid {
		return store.ErrStaleAssignment
	}
	if reason := blocker(current); reason != "" {
		return &store.FinalizationBlockedError{Name: name, Reason: reason}
	}
	f.deleteCalls++
	return nil
}

func newFenceHandler(s store.ProjectStore) *Handler {
	return NewHandler(zap.NewNop(), s, problem.NewWithMapping(handler.NewProblemMapping()))
}

func doPatchStatus(h *Handler, name, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/projects/"+name+"/status", strings.NewReader(body))
	req.SetPathValue("name", name)
	rec := httptest.NewRecorder()
	h.patchStatus(rec, req)
	return rec
}

// A complete fence takes the atomic fenced write path.
func TestCompleteFenceUsesFencedPath(t *testing.T) {
	s := &fenceFakeStore{}
	h := newFenceHandler(s)

	rec := doPatchStatus(h, "demo",
		`{"phase":"Running","uid":"uid-1","nodeRef":"node-a","assignmentGeneration":5}`)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
	if s.fencedStatusCalls != 1 || s.unfencedStatusCalls != 0 {
		t.Fatalf("fenced=%d unfenced=%d, want the fenced path", s.fencedStatusCalls, s.unfencedStatusCalls)
	}
	if s.lastRef != (store.AssignmentRef{UID: "uid-1", NodeRef: "node-a", Generation: 5}) {
		t.Fatalf("ref = %+v, want the request's fence", s.lastRef)
	}
}

// A report missing the fence is rejected as stale before any store write
// happens.
func TestMissingFenceIsRejected(t *testing.T) {
	s := &fenceFakeStore{}
	h := newFenceHandler(s)

	rec := doPatchStatus(h, "demo", `{"phase":"Running"}`)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
	if s.fencedStatusCalls != 0 || s.unfencedStatusCalls != 0 {
		t.Fatalf("store was called (fenced=%d unfenced=%d); a missing fence must be rejected before any write",
			s.fencedStatusCalls, s.unfencedStatusCalls)
	}
}

// A stale-assignment error from the store surfaces as 409 without leaking the
// underlying identity mismatch.
func TestFencedStaleReturns409(t *testing.T) {
	s := &fenceFakeStore{returnErr: store.ErrStaleAssignment}
	h := newFenceHandler(s)

	rec := doPatchStatus(h, "demo",
		`{"phase":"Running","uid":"uid-1","nodeRef":"node-a","assignmentGeneration":5}`)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
	var p struct {
		Detail string `json:"detail"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&p)
	if strings.Contains(p.Detail, "uid-1") {
		t.Fatalf("detail leaked the fence identity: %q", p.Detail)
	}
}

// Status is server-owned: whatever a client sends on create is replaced by the
// initial status, not filtered field by field. The comparison is against the
// whole value, so a status field added later is covered without editing this
// test or createProject.
func TestCreateResetsClientStatus(t *testing.T) {
	s := &fenceFakeStore{}
	h := newFenceHandler(s)

	body := `{"metadata":{"name":"fresh"},"spec":{"services":[{"name":"web","image":"nginx"}]},` +
		`"status":{"phase":"Running","nodeRef":"node-x","assignmentGeneration":99,"assignmentHistory":"Known",` +
		`"failedNodes":["node-y"],"conditions":[{"type":"Phase","status":"True","reason":"Forged"}],` +
		`"recovery":{"rescheduleAttempts":42}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.createProject(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	if s.created == nil {
		t.Fatal("CreateProject was not called")
	}
	if !reflect.DeepEqual(s.created.Status, v1.InitialProjectStatus()) {
		t.Errorf("stored status = %+v, want the initial status %+v", s.created.Status, v1.InitialProjectStatus())
	}

	var resp v1.Project
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !reflect.DeepEqual(resp.Status, v1.InitialProjectStatus()) {
		t.Errorf("response status = %+v, want the initial status", resp.Status)
	}
}

func doDelete(h *Handler, name string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/"+name, nil)
	req.SetPathValue("name", name)
	rec := httptest.NewRecorder()
	h.deleteProject(rec, req)
	return rec
}

// A NeverAssigned Pending Project has no runtime anywhere, so it is hard-deleted
// directly. A Known or Unknown Project — even one whose nodeRef is empty — must
// enter the Terminating cleanup lifecycle instead.
func TestDeleteGuardHonoursAssignmentHistory(t *testing.T) {
	t.Run("NeverAssigned is deleted immediately", func(t *testing.T) {
		s := &fenceFakeStore{getProject: &v1.Project{
			ObjectMeta: v1.ObjectMeta{Name: "fresh"},
			Status:     v1.ProjectStatus{Phase: v1.ProjectPhasePending, AssignmentHistory: v1.AssignmentHistoryNeverAssigned},
		}}
		h := newFenceHandler(s)

		rec := doDelete(h, "fresh")

		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rec.Code)
		}
		if s.deleteCalls != 1 {
			t.Fatalf("deleteCalls = %d, want 1", s.deleteCalls)
		}
	})

	t.Run("Known with empty nodeRef enters cleanup", func(t *testing.T) {
		s := &fenceFakeStore{getProject: &v1.Project{
			ObjectMeta: v1.ObjectMeta{Name: "used"},
			Status: v1.ProjectStatus{
				Phase:             v1.ProjectPhasePending,
				NodeRef:           "",
				AssignmentHistory: v1.AssignmentHistoryKnown,
			},
		}}
		h := newFenceHandler(s)

		rec := doDelete(h, "used")

		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202 (cleanup lifecycle)", rec.Code)
		}
		if s.deleteCalls != 0 {
			t.Fatalf("deleteCalls = %d, want 0; a Known Project must not be hard-deleted", s.deleteCalls)
		}
		if s.uidStatusCalls != 1 {
			t.Fatalf("terminating transition not written (UID-pinned calls = %d)", s.uidStatusCalls)
		}
	})

	t.Run("Unknown with empty nodeRef enters cleanup", func(t *testing.T) {
		s := &fenceFakeStore{getProject: &v1.Project{
			ObjectMeta: v1.ObjectMeta{Name: "mystery"},
			Status: v1.ProjectStatus{
				Phase:             v1.ProjectPhasePending,
				AssignmentHistory: v1.AssignmentHistoryUnknown,
			},
		}}
		h := newFenceHandler(s)

		rec := doDelete(h, "mystery")

		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202 (cleanup lifecycle)", rec.Code)
		}
		if s.deleteCalls != 0 {
			t.Fatalf("deleteCalls = %d, want 0; an Unknown Project must not be hard-deleted", s.deleteCalls)
		}
	})
}

// The handler's read only nominates a Project for direct deletion; the store
// decides on the row it locks. A first assignment that commits in between turns
// the request into an ordinary deletion instead of deleting a record whose
// containers are about to start.
func TestDeleteImmediateRacesFirstAssignment(t *testing.T) {
	neverAssigned := &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: "fresh", UID: "uid-1"},
		Status:     v1.InitialProjectStatus(),
	}

	t.Run("the first assignment wins and the Project enters Terminating", func(t *testing.T) {
		s := &fenceFakeStore{getProject: neverAssigned, changedBeforeDelete: &v1.Project{
			ObjectMeta: v1.ObjectMeta{Name: "fresh", UID: "uid-1"},
			Status: v1.ProjectStatus{
				Phase: v1.ProjectPhaseScheduled, NodeRef: "node-a",
				AssignmentGeneration: 1, AssignmentHistory: v1.AssignmentHistoryKnown,
			},
		}}
		h := newFenceHandler(s)

		rec := doDelete(h, "fresh")

		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202; body = %s", rec.Code, rec.Body.String())
		}
		if s.deleteCalls != 0 {
			t.Fatalf("deleteCalls = %d, want 0; the record of an assigned Project was deleted", s.deleteCalls)
		}
		if s.lastUID != "uid-1" {
			t.Fatalf("Terminating written for UID %q, want uid-1", s.lastUID)
		}
		if s.lastApplied.Phase != v1.ProjectPhaseTerminating {
			t.Fatalf("phase = %s, want Terminating", s.lastApplied.Phase)
		}
		want := v1.AssignmentIdentity{ProjectUID: "uid-1", NodeName: "node-a", AssignmentGeneration: 1}
		if s.lastApplied.Recovery.FindObligation(want) == nil {
			t.Fatalf("no cleanup obligation for %+v; obligations = %+v", want, s.lastApplied.Recovery)
		}
	})

	t.Run("a same-name Project created in between is not deleted", func(t *testing.T) {
		s := &fenceFakeStore{getProject: neverAssigned, changedBeforeDelete: &v1.Project{
			ObjectMeta: v1.ObjectMeta{Name: "fresh", UID: "uid-2"},
			Status:     v1.InitialProjectStatus(),
		}}
		h := newFenceHandler(s)

		rec := doDelete(h, "fresh")

		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
		}
		if s.deleteCalls != 0 || s.uidStatusCalls != 0 {
			t.Fatalf("deleteCalls = %d, uidStatusCalls = %d; the new lifetime was touched", s.deleteCalls, s.uidStatusCalls)
		}
	})
}
