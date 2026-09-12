package project

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/store"
)

// conditionFakeStore records fenced condition writes on top of fenceFakeStore.
type conditionFakeStore struct {
	fenceFakeStore

	patched []v1.Condition
	cleared []v1.ConditionType
	refs    []store.AssignmentRef
}

func (f *conditionFakeStore) PatchProjectConditionFenced(_ context.Context, _ string, ref store.AssignmentRef, c v1.Condition) error {
	f.patched = append(f.patched, c)
	f.refs = append(f.refs, ref)
	return f.returnErr
}

func (f *conditionFakeStore) ClearProjectConditionFenced(_ context.Context, _ string, ref store.AssignmentRef, t v1.ConditionType) error {
	f.cleared = append(f.cleared, t)
	f.refs = append(f.refs, ref)
	return f.returnErr
}

func doPatchCondition(h *Handler, name, condType, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/projects/"+name+"/conditions/"+condType, strings.NewReader(body))
	req.SetPathValue("name", name)
	req.SetPathValue("type", condType)
	rec := httptest.NewRecorder()
	h.patchCondition(rec, req)
	return rec
}

func doDeleteCondition(h *Handler, name, condType, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/"+name+"/conditions/"+condType+"?"+query, nil)
	req.SetPathValue("name", name)
	req.SetPathValue("type", condType)
	rec := httptest.NewRecorder()
	h.deleteCondition(rec, req)
	return rec
}

// RecoveryBlocked is agent-owned: the agent is the only party that knows it
// refused a recovery, so it must be able to set and clear it — through the
// fenced path, so a node that has lost the Project cannot.
func TestRecoveryBlockedIsAgentWritableThroughTheFence(t *testing.T) {
	s := &conditionFakeStore{}
	h := newFenceHandler(s, true)

	rec := doPatchCondition(h, "demo", "RecoveryBlocked",
		`{"status":"True","reason":"VolumeUnavailable","message":"db-data missing",`+
			`"uid":"uid-1","nodeRef":"node-a","assignmentGeneration":5}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("patch status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
	if len(s.patched) != 1 || s.patched[0].Type != v1.ConditionTypeRecoveryBlocked ||
		s.patched[0].Reason != "VolumeUnavailable" {
		t.Fatalf("patched = %+v, want one RecoveryBlocked/VolumeUnavailable", s.patched)
	}

	rec = doDeleteCondition(h, "demo", "RecoveryBlocked", "uid=uid-1&nodeRef=node-a&assignmentGeneration=5")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
	if len(s.cleared) != 1 || s.cleared[0] != v1.ConditionTypeRecoveryBlocked {
		t.Fatalf("cleared = %v, want [RecoveryBlocked]", s.cleared)
	}

	want := store.AssignmentRef{UID: "uid-1", NodeRef: "node-a", Generation: 5}
	for i, ref := range s.refs {
		if ref != want {
			t.Errorf("write %d fenced with %+v, want %+v", i, ref, want)
		}
	}
}

// A stale agent's write is refused with 409, the same as a stale status write:
// a node the Project has moved away from must not be able to mark it blocked.
func TestRecoveryBlockedStaleFenceIsRejected(t *testing.T) {
	s := &conditionFakeStore{}
	s.returnErr = store.ErrStaleAssignment
	h := newFenceHandler(s, true)

	rec := doPatchCondition(h, "demo", "RecoveryBlocked",
		`{"status":"True","reason":"VolumeUnavailable","uid":"uid-1","nodeRef":"node-a","assignmentGeneration":4}`)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
}

// In strict mode an unfenced write is refused outright rather than falling
// back to the unfenced path.
func TestRecoveryBlockedWithoutFenceIsRejectedInStrictMode(t *testing.T) {
	s := &conditionFakeStore{}
	h := newFenceHandler(s, true)

	rec := doPatchCondition(h, "demo", "RecoveryBlocked", `{"status":"True","reason":"VolumeUnavailable"}`)

	if rec.Code == http.StatusNoContent {
		t.Fatalf("an unfenced write was accepted in strict mode")
	}
	if len(s.patched) != 0 {
		t.Fatalf("store was written: %+v", s.patched)
	}
}
