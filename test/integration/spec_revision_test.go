//go:build e2e

package integration

import (
	"NYCU-SDC/caravanserai/test/integration/testhelper"
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/adapter"
	"NYCU-SDC/caravanserai/internal/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// putSpec sends spec as the new spec of name and returns the status code and,
// on success, the stored Project.
func putSpec(t *testing.T, name string, spec v1.ProjectSpec, meta v1.ObjectMeta) (int, v1.Project) {
	t.Helper()
	meta.Name = name
	resp := doRequest(t, http.MethodPut, "/api/v1/projects/"+name, mustMarshal(t, v1.Project{ObjectMeta: meta, Spec: spec}))
	var p v1.Project
	if resp.StatusCode == http.StatusOK {
		mustDecodeBody(t, resp, &p)
	} else {
		drainBody(resp)
	}
	return resp.StatusCode, p
}

func specAppliedBody(uid, nodeRef string, generation, revision int64, applied bool, reason string) []byte {
	return []byte(fmt.Sprintf(`{"uid":%q,"nodeRef":%q,"assignmentGeneration":%d,"specRevision":%d,"applied":%t,"reason":%q}`,
		uid, nodeRef, generation, revision, applied, reason))
}

func putSpecApplied(t *testing.T, name string, body []byte) int {
	t.Helper()
	resp := doRequest(t, http.MethodPut, "/api/v1/projects/"+name+"/status/spec-applied", body)
	defer drainBody(resp)
	return resp.StatusCode
}

// CARA-134 stage 1: specRevision counts spec content, not writes.
func TestSpecRevisionCountsSpecChanges(t *testing.T) {
	const name = "e2e-spec-revision"
	drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+name+"?force=true", nil))
	t.Cleanup(func() { drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+name+"?force=true", nil)) })

	spec := v1.ProjectSpec{Services: []v1.ServiceDef{{Name: "web", Image: "nginx:1.27"}}}
	resp := doRequest(t, http.MethodPost, "/api/v1/projects", mustMarshal(t, v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: name, SpecRevision: 7},
		Spec:       spec,
	}))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var created v1.Project
	mustDecodeBody(t, resp, &created)
	assert.Equal(t, int64(1), created.SpecRevision, "a new Project starts at 1 whatever the client sent")
	assert.Equal(t, int64(1), getProject(t, name).SpecRevision)

	t.Run("the same spec again leaves it alone", func(t *testing.T) {
		code, p := putSpec(t, name, spec, v1.ObjectMeta{})
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, int64(1), p.SpecRevision)
		_, found := findCondition(p.Status.Conditions, v1.ConditionTypeSpecApplied)
		assert.False(t, found, "nothing changed, so nothing is waiting to be applied")
	})

	t.Run("labels alone are not a spec change", func(t *testing.T) {
		code, p := putSpec(t, name, spec, v1.ObjectMeta{Labels: map[string]string{"team": "sre"}})
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, int64(1), p.SpecRevision)
	})

	t.Run("a changed spec adds one, and a client value is ignored", func(t *testing.T) {
		next := v1.ProjectSpec{Services: []v1.ServiceDef{{Name: "web", Image: "nginx:1.28"}}}
		code, p := putSpec(t, name, next, v1.ObjectMeta{SpecRevision: 99})
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, int64(2), p.SpecRevision)
		cond, found := findCondition(p.Status.Conditions, v1.ConditionTypeSpecApplied)
		require.True(t, found, "the update says in the same write that the new revision is not running")
		assert.Equal(t, v1.ConditionFalse, cond.Status)
		assert.Equal(t, v1.ReasonSpecUpdatePending, cond.Reason)
	})
}

// CARA-134 stage 1: while a Project is assigned to a Node, an update may not
// change what the Node booked or what its data and backups are tied to.
func TestAssignedSpecChangesAreRefused(t *testing.T) {
	const name = "e2e-spec-assigned"
	ctx := context.Background()
	projects := adapter.NewProjectStoreAdapter(suite.store)
	drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+name+"?force=true", nil))
	t.Cleanup(func() { drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+name+"?force=true", nil)) })

	base := v1.ProjectSpec{
		Size: v1.ProjectSizeSmall,
		Services: []v1.ServiceDef{{
			Name: "web", Image: "nginx:1.27",
			VolumeMounts: []v1.VolumeMount{{Name: "data", MountPath: "/data"}},
		}},
		Volumes: []v1.VolumeDef{
			{Name: "data", Type: v1.VolumeTypeManaged},
			{Name: "cache", Type: v1.VolumeTypeEphemeral},
		},
	}
	resp := doRequest(t, http.MethodPost, "/api/v1/projects", mustMarshal(t, v1.Project{ObjectMeta: v1.ObjectMeta{Name: name}, Spec: base}))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	drainBody(resp)

	resize := base
	resize.Size = v1.ProjectSizeLarge

	t.Run("an unassigned Project may still change size", func(t *testing.T) {
		code, p := putSpec(t, name, resize, v1.ObjectMeta{})
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, v1.ProjectSizeLarge, p.Spec.Size)
		code, _ = putSpec(t, name, base, v1.ObjectMeta{})
		require.Equal(t, http.StatusOK, code)
	})

	require.NoError(t, projects.SetProjectScheduled(ctx, name, testhelper.UID(t, suite.store, name), "node-a"))
	require.NoError(t, testhelper.ForcePhase(ctx, suite.store, name, v1.ProjectPhaseRunning, "Test", ""))
	before := getProject(t, name)
	require.Equal(t, "node-a", before.Status.NodeRef)

	removeVolume := base
	removeVolume.Volumes = base.Volumes[:1]
	retype := base
	retype.Volumes = []v1.VolumeDef{base.Volumes[0], {Name: "cache", Type: v1.VolumeTypeManaged}}

	for _, tc := range []struct {
		name string
		spec v1.ProjectSpec
	}{
		{"size", resize},
		{"removing a volume", removeVolume},
		{"changing a volume's type", retype},
	} {
		t.Run(tc.name+" is refused with a field error", func(t *testing.T) {
			code, _ := putSpec(t, name, tc.spec, v1.ObjectMeta{})
			assert.Equal(t, http.StatusBadRequest, code)
		})
		t.Run(tc.name+" is refused by the store as well", func(t *testing.T) {
			// The handler's check reads before the write; the store's is what
			// holds when an assignment lands in between.
			err := suite.store.UpdateProjectSpec(ctx, &v1.Project{ObjectMeta: v1.ObjectMeta{Name: name}, Spec: tc.spec})
			assert.True(t, errors.Is(err, store.ErrConflictState), "got %v", err)
		})
	}
	unchanged := getProject(t, name)
	assert.Equal(t, before.ResourceVersion, unchanged.ResourceVersion, "a refused update writes nothing")
	assert.Equal(t, before.SpecRevision, unchanged.SpecRevision)

	t.Run("adding a volume and changing an image are accepted without touching the assignment", func(t *testing.T) {
		next := base
		next.Services = []v1.ServiceDef{{Name: "web", Image: "nginx:1.28", VolumeMounts: base.Services[0].VolumeMounts}}
		next.Volumes = append(append([]v1.VolumeDef(nil), base.Volumes...), v1.VolumeDef{Name: "conf", Type: v1.VolumeTypeManaged})
		code, p := putSpec(t, name, next, v1.ObjectMeta{})
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, before.SpecRevision+1, p.SpecRevision)
		assert.Equal(t, before.UID, p.UID)
		assert.Equal(t, before.Status.NodeRef, p.Status.NodeRef)
		assert.Equal(t, before.Status.AssignmentGeneration, p.Status.AssignmentGeneration)
		assert.Equal(t, v1.ProjectPhaseRunning, p.Status.Phase)
	})

	t.Run("Failed with its nodeRef is still assigned", func(t *testing.T) {
		require.NoError(t, testhelper.ForcePhase(ctx, suite.store, name, v1.ProjectPhaseFailed, "Test", ""))
		code, _ := putSpec(t, name, resize, v1.ObjectMeta{})
		assert.Equal(t, http.StatusBadRequest, code)
	})
}

// CARA-134 stage 1: the assigned Agent reports a revision through its fence,
// and the server keeps observedSpecRevision and SpecApplied honest.
func TestSpecAppliedReports(t *testing.T) {
	const name = "e2e-spec-applied"
	uid, gen := createAssigned(t, name, "node-a", v1.ProjectPhaseRunning)
	report := func(nodeRef string, generation, revision int64, applied bool, reason string) int {
		t.Helper()
		return putSpecApplied(t, name, specAppliedBody(uid, nodeRef, generation, revision, applied, reason))
	}

	t.Run("revision 1 applied", func(t *testing.T) {
		require.Equal(t, http.StatusNoContent, report("node-a", gen, 1, true, ""))
		p := getProject(t, name)
		assert.Equal(t, int64(1), p.Status.ObservedSpecRevision)
		cond, found := findCondition(p.Status.Conditions, v1.ConditionTypeSpecApplied)
		require.True(t, found)
		assert.Equal(t, v1.ConditionTrue, cond.Status)
		assert.Equal(t, v1.ReasonSpecApplied, cond.Reason)
	})

	t.Run("the same report again writes nothing", func(t *testing.T) {
		before := getProject(t, name).ResourceVersion
		require.Equal(t, http.StatusNoContent, report("node-a", gen, 1, true, ""))
		assert.Equal(t, before, getProject(t, name).ResourceVersion)
	})

	t.Run("a revision the spec has not reached is refused", func(t *testing.T) {
		assert.Equal(t, http.StatusConflict, report("node-a", gen, 2, true, ""))
		assert.Equal(t, int64(1), getProject(t, name).Status.ObservedSpecRevision)
	})

	t.Run("a fence that is not current is refused", func(t *testing.T) {
		assert.Equal(t, http.StatusConflict, report("node-a", gen-1, 1, true, ""))
		assert.Equal(t, http.StatusConflict, report("node-b", gen, 1, true, ""))
		assert.Equal(t, http.StatusConflict, putSpecApplied(t, name, []byte(`{"specRevision":1,"applied":true}`)))
	})

	t.Run("a malformed report is refused", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, report("node-a", gen, 0, true, ""))
		assert.Equal(t, http.StatusBadRequest, report("node-a", gen, 1, false, ""), "a failure needs a reason")
	})

	for _, image := range []string{"nginx:1.28", "nginx:1.29"} {
		code, _ := putSpec(t, name, v1.ProjectSpec{Services: []v1.ServiceDef{{Name: "web", Image: image}}}, v1.ObjectMeta{})
		require.Equal(t, http.StatusOK, code)
	}
	require.Equal(t, int64(3), getProject(t, name).SpecRevision)

	t.Run("a superseded revision raises observed but leaves the condition to the latest", func(t *testing.T) {
		require.Equal(t, http.StatusNoContent, report("node-a", gen, 2, true, ""))
		p := getProject(t, name)
		assert.Equal(t, int64(2), p.Status.ObservedSpecRevision)
		cond, _ := findCondition(p.Status.Conditions, v1.ConditionTypeSpecApplied)
		assert.Equal(t, v1.ConditionFalse, cond.Status)
		assert.Equal(t, v1.ReasonSpecUpdatePending, cond.Reason)
	})

	t.Run("a failed latest revision says why and keeps observed", func(t *testing.T) {
		require.Equal(t, http.StatusNoContent, report("node-a", gen, 3, false, v1.ReasonImagePullFailed))
		p := getProject(t, name)
		assert.Equal(t, int64(2), p.Status.ObservedSpecRevision)
		cond, _ := findCondition(p.Status.Conditions, v1.ConditionTypeSpecApplied)
		assert.Equal(t, v1.ConditionFalse, cond.Status)
		assert.Equal(t, v1.ReasonImagePullFailed, cond.Reason)
	})

	t.Run("observed never goes back", func(t *testing.T) {
		require.Equal(t, http.StatusNoContent, report("node-a", gen, 1, true, ""))
		assert.Equal(t, int64(2), getProject(t, name).Status.ObservedSpecRevision)
	})

	t.Run("the latest revision applied", func(t *testing.T) {
		require.Equal(t, http.StatusNoContent, report("node-a", gen, 3, true, ""))
		p := getProject(t, name)
		assert.Equal(t, int64(3), p.Status.ObservedSpecRevision)
		cond, _ := findCondition(p.Status.Conditions, v1.ConditionTypeSpecApplied)
		assert.Equal(t, v1.ConditionTrue, cond.Status)
	})

	t.Run("refused once the Project is on its way out", func(t *testing.T) {
		drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+name, nil))
		require.Equal(t, v1.ProjectPhaseTerminating, getProject(t, name).Status.Phase)
		assert.Equal(t, http.StatusConflict, report("node-a", gen, 3, true, ""))
	})
}

// SpecApplied belongs to the server's report path, not the generic condition
// endpoint an Agent may write.
func TestSpecAppliedIsNotAnAgentCondition(t *testing.T) {
	const name = "e2e-spec-applied-condition"
	createAssigned(t, name, "node-a", v1.ProjectPhaseRunning)
	resp := doRequest(t, http.MethodPatch, "/api/v1/projects/"+name+"/conditions/"+string(v1.ConditionTypeSpecApplied),
		fencedCondition(t, name, map[string]any{"status": "True", "reason": "Forged"}))
	defer drainBody(resp)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	_, found := findCondition(getProject(t, name).Status.Conditions, v1.ConditionTypeSpecApplied)
	assert.False(t, found)
}

// CARA-134 stage 3: a rollout restart is a new revision that only changes
// spec.restartedAt, and a later apply of the manifest keeps it.
func TestRolloutRestart(t *testing.T) {
	const name = "e2e-spec-restart"
	createAssigned(t, name, "node-a", v1.ProjectPhaseRunning)
	before := getProject(t, name)
	require.Nil(t, before.Spec.RestartedAt)

	restart := func() (int, v1.Project) {
		t.Helper()
		resp := doRequest(t, http.MethodPost, "/api/v1/projects/"+name+"/restart", nil)
		var p v1.Project
		if resp.StatusCode == http.StatusOK {
			mustDecodeBody(t, resp, &p)
		} else {
			drainBody(resp)
		}
		return resp.StatusCode, p
	}

	code, restarted := restart()
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, restarted.Spec.RestartedAt)
	assert.Equal(t, before.SpecRevision+1, restarted.SpecRevision)
	assert.Equal(t, before.Spec.Services, restarted.Spec.Services, "nothing else in the spec changes")
	assert.Equal(t, before.Status.AssignmentGeneration, restarted.Status.AssignmentGeneration, "a restart is not a reschedule")
	cond, _ := findCondition(restarted.Status.Conditions, v1.ConditionTypeSpecApplied)
	assert.Equal(t, v1.ReasonSpecUpdatePending, cond.Reason)

	t.Run("applying the manifest again keeps the restart", func(t *testing.T) {
		code, p := putSpec(t, name, v1.ProjectSpec{Services: before.Spec.Services}, v1.ObjectMeta{})
		require.Equal(t, http.StatusOK, code)
		require.NotNil(t, p.Spec.RestartedAt)
		assert.True(t, p.Spec.RestartedAt.Equal(*restarted.Spec.RestartedAt))
		assert.Equal(t, restarted.SpecRevision, p.SpecRevision, "and so is not a new revision")
	})

	t.Run("each restart is a new revision", func(t *testing.T) {
		code, again := restart()
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, restarted.SpecRevision+1, again.SpecRevision)
	})

	t.Run("refused once the Project is on its way out, and for one that does not exist", func(t *testing.T) {
		drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+name, nil))
		code, _ := restart()
		assert.Equal(t, http.StatusConflict, code)
		resp := doRequest(t, http.MethodPost, "/api/v1/projects/no-such-project/restart", nil)
		drainBody(resp)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}
