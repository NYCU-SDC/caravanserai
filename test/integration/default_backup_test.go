//go:build e2e

package integration

import (
	"context"
	"net/http"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/adapter"
	"NYCU-SDC/caravanserai/internal/server/controller"
	"NYCU-SDC/caravanserai/test/integration/testhelper"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// managedProject is a Project with one Managed volume and the given backup
// section.
func managedProject(name string, backup *v1.ProjectBackupConfig) v1.Project {
	return v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: name},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{{Name: "web", Image: "nginx:alpine",
				VolumeMounts: []v1.VolumeMount{{Name: "data", MountPath: "/data"}}}},
			Volumes: []v1.VolumeDef{{Name: "data", Type: v1.VolumeTypeManaged}},
			Backup:  backup,
		},
	}
}

func putProject(t *testing.T, p v1.Project) *http.Response {
	t.Helper()
	return doRequest(t, http.MethodPut, "/api/v1/projects/"+p.Name, mustMarshal(t, p))
}

// TestDefaultBackupInterval: a Managed Project that says nothing about backup
// is backed up daily (ADR-0029), and the stored spec says so.
func TestDefaultBackupInterval(t *testing.T) {
	const name = "e2e-default-backup"
	drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+name+"?force=true", nil))
	created := createForCleanup(t, managedProject(name, nil))
	require.NotNil(t, created.Spec.Backup, "the create response shows what was stored")
	assert.Equal(t, "24h", created.Spec.Backup.Interval)

	got := getProject(t, name)
	require.NotNil(t, got.Spec.Backup)
	assert.Equal(t, &v1.ProjectBackupConfig{Interval: "24h"}, got.Spec.Backup)
	revision := got.SpecRevision

	// A manifest that never mentions backup applies again without changing
	// anything: the default is what it already has.
	resp := putProject(t, managedProject(name, nil))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	drainBody(resp)
	got = getProject(t, name)
	assert.Equal(t, "24h", got.Spec.Backup.Interval)
	assert.Equal(t, revision, got.SpecRevision, "re-applying the same manifest is not a spec change")

	// Its own interval wins, and removing it from the manifest goes back to
	// the default rather than keeping the old value.
	resp = putProject(t, managedProject(name, &v1.ProjectBackupConfig{Interval: "168h"}))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	drainBody(resp)
	assert.Equal(t, "168h", getProject(t, name).Spec.Backup.Interval)

	resp = putProject(t, managedProject(name, nil))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	drainBody(resp)
	assert.Equal(t, "24h", getProject(t, name).Spec.Backup.Interval)
}

// TestDefaultBackupOnlyForManagedData: nothing to back up gets no policy, and
// a Managed volume added later brings the default with it.
func TestDefaultBackupOnlyForManagedData(t *testing.T) {
	const name = "e2e-default-backup-stateless"
	drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+name+"?force=true", nil))
	stateless := v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: name},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{{Name: "web", Image: "nginx:alpine",
				VolumeMounts: []v1.VolumeMount{{Name: "cache", MountPath: "/cache"}}}},
			Volumes: []v1.VolumeDef{{Name: "cache", Type: v1.VolumeTypeEphemeral}},
		},
	}
	createForCleanup(t, stateless)
	assert.Nil(t, getProject(t, name).Spec.Backup)

	withData := stateless
	withData.Spec.Volumes = append(withData.Spec.Volumes, v1.VolumeDef{Name: "data", Type: v1.VolumeTypeManaged})
	withData.Spec.Services = []v1.ServiceDef{{Name: "web", Image: "nginx:alpine", VolumeMounts: []v1.VolumeMount{
		{Name: "cache", MountPath: "/cache"}, {Name: "data", MountPath: "/data"}}}}
	resp := putProject(t, withData)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	drainBody(resp)
	got := getProject(t, name)
	require.NotNil(t, got.Spec.Backup)
	assert.Equal(t, "24h", got.Spec.Backup.Interval)
}

// TestDisabledBackup: opting out is stored as written, and a section that
// mixes it with a schedule is refused.
func TestDisabledBackup(t *testing.T) {
	const name = "e2e-backup-disabled"
	drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+name+"?force=true", nil))
	createForCleanup(t, managedProject(name, &v1.ProjectBackupConfig{Disabled: true}))
	assert.Equal(t, &v1.ProjectBackupConfig{Disabled: true}, getProject(t, name).Spec.Backup)

	resp := putProject(t, managedProject(name, &v1.ProjectBackupConfig{Disabled: true, Interval: "24h"}))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	drainBody(resp)
	assert.Equal(t, &v1.ProjectBackupConfig{Disabled: true}, getProject(t, name).Spec.Backup)

	// Turning it back on is an ordinary update.
	resp = putProject(t, managedProject(name, nil))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	drainBody(resp)
	assert.Equal(t, &v1.ProjectBackupConfig{Interval: "24h"}, getProject(t, name).Spec.Backup)
}

// TestDisabledBackupIsNotMovedOnTheGracePeriod: a Project that turned backup
// off stays on a lost Node even though it has an old committed backup, which
// would silently discard everything written since.
func TestDisabledBackupIsNotMovedOnTheGracePeriod(t *testing.T) {
	const name = "e2e-backup-disabled-grace"
	ctx := context.Background()
	on := adapter.NewProjectStoreAdapter(suite.store).WithGraceExpiredRelease(true)

	drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+name+"?force=true", nil))
	createForCleanup(t, managedProject(name, nil))
	require.NoError(t, on.SetProjectScheduled(ctx, name, testhelper.UID(t, suite.store, name), "node-a"))
	require.NoError(t, suite.store.UpdateProjectStatusWithRetry(ctx, name, func(s *v1.ProjectStatus) error {
		s.Phase = v1.ProjectPhaseRunning
		return nil
	}))
	p := getProject(t, name)
	require.Equal(t, http.StatusNoContent, putEvidence(t, name, "committed-backup",
		committedBackupBody(p.ObjectMeta.UID, "node-a", p.Status.AssignmentGeneration, "20261108T000000Z-bbbbbbbb")))

	resp := putProject(t, managedProject(name, &v1.ProjectBackupConfig{Disabled: true}))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	drainBody(resp)

	p = getProject(t, name)
	released, err := on.ReleaseAssignment(ctx, &controller.ProjectSnapshot{
		Name: name, UID: p.ObjectMeta.UID, Phase: p.Status.Phase, NodeRef: p.Status.NodeRef, AssignmentGeneration: p.Status.AssignmentGeneration,
	})
	require.NoError(t, err)
	assert.False(t, released)
	got := getProject(t, name)
	assert.Equal(t, "node-a", got.Status.NodeRef)
	cond, ok := findCondition(got.Status.Conditions, v1.ConditionTypeTakeoverRequired)
	require.True(t, ok, "a person has to decide")
	assert.Equal(t, v1.ConditionTrue, cond.Status)
}
