package project

import (
	"context"
	"encoding/json"
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

// fenceFakeStore records which write path the handler chose and lets a test
// force a store error. It embeds store.ProjectStore so unrelated methods panic.
type fenceFakeStore struct {
	store.ProjectStore

	getProject *v1.Project
	getErr     error

	fencedStatusCalls   int
	unfencedStatusCalls int
	lastRef             store.AssignmentRef

	deleteCalls      int
	terminatingCalls int
	created          *v1.Project

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
	st := v1.ProjectStatus{}
	return mutate(&st)
}

func (f *fenceFakeStore) UpdateProjectStatusWithRetry(_ context.Context, _ string, mutate func(*v1.ProjectStatus) error) error {
	f.unfencedStatusCalls++
	if f.returnErr != nil {
		return f.returnErr
	}
	st := v1.ProjectStatus{}
	return mutate(&st)
}

func (f *fenceFakeStore) CreateProject(_ context.Context, p *v1.Project) error {
	f.created = p
	return nil
}

func (f *fenceFakeStore) DeleteProject(_ context.Context, _ string) error {
	f.deleteCalls++
	return nil
}

func newFenceHandler(s store.ProjectStore, enforce bool) *Handler {
	return NewHandler(zap.NewNop(), s, problem.NewWithMapping(handler.NewProblemMapping()), enforce)
}

func doPatchStatus(h *Handler, name, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/projects/"+name+"/status", strings.NewReader(body))
	req.SetPathValue("name", name)
	rec := httptest.NewRecorder()
	h.patchStatus(rec, req)
	return rec
}

// A complete fence takes the atomic fenced write path even in compatibility
// mode: an upgraded Agent should always be validated, whatever the rollout
// state of the switch.
func TestCompleteFenceUsesFencedPath(t *testing.T) {
	s := &fenceFakeStore{}
	h := newFenceHandler(s, false)

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

// In compatibility mode a legacy Agent that omits the fence still works through
// the unfenced path.
func TestCompatModeMissingFenceUsesUnfenced(t *testing.T) {
	s := &fenceFakeStore{}
	h := newFenceHandler(s, false)

	rec := doPatchStatus(h, "demo", `{"phase":"Running"}`)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
	if s.unfencedStatusCalls != 1 || s.fencedStatusCalls != 0 {
		t.Fatalf("fenced=%d unfenced=%d, want the unfenced path", s.fencedStatusCalls, s.unfencedStatusCalls)
	}
}

// Under strict enforcement a report missing the fence is rejected as stale
// before any store write happens.
func TestStrictModeRejectsMissingFence(t *testing.T) {
	s := &fenceFakeStore{}
	h := newFenceHandler(s, true)

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
	h := newFenceHandler(s, true)

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

// A newly created Project is always NeverAssigned at generation zero, and a
// client cannot override that server-owned state.
func TestCreateInitializesNeverAssigned(t *testing.T) {
	s := &fenceFakeStore{}
	h := newFenceHandler(s, false)

	body := `{"metadata":{"name":"fresh"},"spec":{"services":[{"name":"web","image":"nginx"}]},` +
		`"status":{"assignmentGeneration":99,"assignmentHistory":"Known"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.createProject(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	if s.created == nil {
		t.Fatal("CreateProject was not called")
	}
	if s.created.Status.AssignmentHistory != v1.AssignmentHistoryNeverAssigned {
		t.Errorf("history = %q, want NeverAssigned (client value ignored)", s.created.Status.AssignmentHistory)
	}
	if s.created.Status.AssignmentGeneration != 0 {
		t.Errorf("generation = %d, want 0 (client value ignored)", s.created.Status.AssignmentGeneration)
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
		h := newFenceHandler(s, false)

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
		h := newFenceHandler(s, false)

		rec := doDelete(h, "used")

		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202 (cleanup lifecycle)", rec.Code)
		}
		if s.deleteCalls != 0 {
			t.Fatalf("deleteCalls = %d, want 0; a Known Project must not be hard-deleted", s.deleteCalls)
		}
		if s.unfencedStatusCalls != 1 {
			t.Fatalf("terminating transition not written (unfenced calls = %d)", s.unfencedStatusCalls)
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
		h := newFenceHandler(s, false)

		rec := doDelete(h, "mystery")

		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202 (cleanup lifecycle)", rec.Code)
		}
		if s.deleteCalls != 0 {
			t.Fatalf("deleteCalls = %d, want 0; an Unknown Project must not be hard-deleted", s.deleteCalls)
		}
	})
}
