//go:build e2e

package integration

import (
	"context"
	"errors"
	"net/http"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// postgresDifferentialProject is a valid Project whose only Managed volume uses
// the PostgresDifferential strategy.
func postgresDifferentialProject(name string, retainFull *int) v1.Project {
	return v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: name},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{{
				Name:         "db",
				Image:        v1.PostgresDifferentialImage,
				VolumeMounts: []v1.VolumeMount{{Name: "pgdata", MountPath: v1.PostgresDifferentialDataPath}},
			}},
			Volumes: []v1.VolumeDef{{Name: "pgdata", Type: v1.VolumeTypeManaged}},
			Backup: &v1.ProjectBackupConfig{
				Interval: "24h",
				Items: []v1.BackupItem{{
					Volume:     "pgdata",
					Strategy:   v1.BackupStrategyPostgresDifferential,
					RetainFull: retainFull,
				}},
			},
		},
	}
}

func createForCleanup(t *testing.T, project v1.Project) v1.Project {
	t.Helper()
	resp := doRequest(t, http.MethodPost, "/api/v1/projects", mustMarshal(t, project))
	require.Equal(t, http.StatusCreated, resp.StatusCode, "create %s", project.Name)
	var created v1.Project
	mustDecodeBody(t, resp, &created)
	t.Cleanup(func() {
		resp := doRequest(t, http.MethodDelete, "/api/v1/projects/"+project.Name+"?force=true", nil)
		drainBody(resp)
	})
	return created
}

func TestBackupStrategyCreateRoundTripsAndReportsNotActive(t *testing.T) {
	retain := 6
	createForCleanup(t, postgresDifferentialProject("e2e-pgdiff-create", &retain))

	got := getProject(t, "e2e-pgdiff-create")
	require.NotNil(t, got.Spec.Backup)
	require.Len(t, got.Spec.Backup.Items, 1)
	item := got.Spec.Backup.Items[0]
	assert.Equal(t, "pgdata", item.Volume)
	assert.Equal(t, v1.BackupStrategyPostgresDifferential, item.Strategy)
	require.NotNil(t, item.RetainFull)
	assert.Equal(t, 6, *item.RetainFull)

	cond, ok := findCondition(got.Status.Conditions, v1.ConditionTypeBackupActive)
	require.True(t, ok, "a PostgresDifferential Project must say nothing protects it yet")
	assert.Equal(t, v1.ConditionFalse, cond.Status)
	assert.Equal(t, v1.ReasonStrategyNotActive, cond.Reason)
}

func TestBackupStrategyOmittedItemsAreUnchanged(t *testing.T) {
	// A manifest from before strategies existed: stored exactly as sent, no
	// items invented, and no BackupActive condition.
	project := v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: "e2e-pgdiff-legacy"},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{{Name: "web", Image: "nginx:latest",
				VolumeMounts: []v1.VolumeMount{{Name: "data", MountPath: "/data"}}}},
			Volumes: []v1.VolumeDef{{Name: "data", Type: v1.VolumeTypeManaged}},
			Backup:  &v1.ProjectBackupConfig{Interval: "1h"},
		},
	}
	createForCleanup(t, project)

	got := getProject(t, "e2e-pgdiff-legacy")
	assert.Equal(t, project.Spec, got.Spec)
	assert.Equal(t, v1.BackupStrategyOffline, got.Spec.BackupStrategyFor("data"))
	_, ok := findCondition(got.Status.Conditions, v1.ConditionTypeBackupActive)
	assert.False(t, ok)
}

func TestBackupStrategyConditionIsServerOwned(t *testing.T) {
	// A client cannot create a Project already claiming to be protected...
	forged := postgresDifferentialProject("e2e-pgdiff-forged", nil)
	forged.Status.Conditions = []v1.Condition{{
		Type: v1.ConditionTypeBackupActive, Status: v1.ConditionTrue, Reason: "Forged",
	}}
	createForCleanup(t, forged)

	got := getProject(t, "e2e-pgdiff-forged")
	cond, ok := findCondition(got.Status.Conditions, v1.ConditionTypeBackupActive)
	require.True(t, ok)
	assert.Equal(t, v1.ConditionFalse, cond.Status)
	assert.Equal(t, v1.ReasonStrategyNotActive, cond.Reason)

	// ...nor can an agent flip or clear it afterwards.
	path := "/api/v1/projects/e2e-pgdiff-forged/conditions/" + string(v1.ConditionTypeBackupActive)
	resp := doRequest(t, http.MethodPatch, path, mustMarshal(t, map[string]string{"status": "True", "reason": "Forged"}))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	drainBody(resp)
	resp = doRequest(t, http.MethodDelete, path, nil)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	drainBody(resp)

	cond, ok = findCondition(getProject(t, "e2e-pgdiff-forged").Status.Conditions, v1.ConditionTypeBackupActive)
	require.True(t, ok)
	assert.Equal(t, v1.ConditionFalse, cond.Status)
}

func TestBackupStrategyItemsAreImmutable(t *testing.T) {
	const name = "e2e-pgdiff-immutable"
	createForCleanup(t, postgresDifferentialProject(name, nil))

	put := func(project v1.Project) *http.Response {
		return doRequest(t, http.MethodPut, "/api/v1/projects/"+name, mustMarshal(t, project))
	}

	// An unrelated change with the items resent unchanged is accepted.
	same := postgresDifferentialProject(name, nil)
	same.Spec.Services[0].Env = []v1.EnvVar{{Name: "TZ", Value: "Asia/Taipei"}}
	resp := put(same)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "unchanged items must not block other updates")
	drainBody(resp)

	// Changing retainFull alone is a change to the items.
	retain := 8
	resp = put(postgresDifferentialProject(name, &retain))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	drainBody(resp)

	// Dropping the items would turn the volume Offline.
	dropped := postgresDifferentialProject(name, nil)
	dropped.Spec.Backup.Items = nil
	resp = put(dropped)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	drainBody(resp)

	assert.Nil(t, getProject(t, name).Spec.Backup.Items[0].RetainFull, "rejected updates must not be stored")
}

func TestBackupStrategyItemsAreImmutableInTheStore(t *testing.T) {
	// The handler's check reads before it writes, so an update racing another
	// one could pass it. The store must refuse on its own, inside the write.
	const name = "e2e-pgdiff-store-guard"
	createForCleanup(t, postgresDifferentialProject(name, nil))

	retain := 8
	changed := postgresDifferentialProject(name, &retain)
	err := suite.store.UpdateProjectSpec(context.Background(), &changed)
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrConflictState), "got %v", err)
	assert.Contains(t, err.Error(), "spec.backup.items cannot change")

	assert.Nil(t, getProject(t, name).Spec.Backup.Items[0].RetainFull)

	// The same items written again pass the guard.
	same := postgresDifferentialProject(name, nil)
	require.NoError(t, suite.store.UpdateProjectSpec(context.Background(), &same))
}

func TestBackupStrategyDataCannotLeaveTheVolume(t *testing.T) {
	// The data must live in the volume that provenance, placement and backup
	// treat as the database. Moving PGDATA or the WAL elsewhere is refused on
	// create and on update.
	moved := func(name string, env v1.EnvVar) v1.Project {
		p := postgresDifferentialProject(name, nil)
		p.Spec.Services[0].Env = []v1.EnvVar{env}
		return p
	}

	for _, env := range []v1.EnvVar{
		{Name: "PGDATA", Value: "/tmp/other-db"},
		{Name: "POSTGRES_INITDB_WALDIR", Value: "/tmp/wal"},
		{Name: "POSTGRES_INITDB_ARGS", Value: "--waldir=/tmp/wal"},
		{Name: "POSTGRES_INITDB_ARGS", Value: "--wald=/tmp/other-wal"},
		{Name: "POSTGRES_INITDB_ARGS", Value: "--encoding=UTF8${IFS}--waldir=/tmp/wal"},
	} {
		resp := doRequest(t, http.MethodPost, "/api/v1/projects", mustMarshal(t, moved("e2e-pgdiff-moved", env)))
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "create with %s=%s", env.Name, env.Value)
		drainBody(resp)
	}

	const name = "e2e-pgdiff-moved-later"
	createForCleanup(t, postgresDifferentialProject(name, nil))
	resp := doRequest(t, http.MethodPut, "/api/v1/projects/"+name,
		mustMarshal(t, moved(name, v1.EnvVar{Name: "PGDATA", Value: "/tmp/other-db"})))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "update moving PGDATA")
	drainBody(resp)
	resp = doRequest(t, http.MethodPut, "/api/v1/projects/"+name,
		mustMarshal(t, moved(name, v1.EnvVar{Name: "POSTGRES_INITDB_ARGS", Value: "--encoding=UTF8${IFS}--waldir=/tmp/wal"})))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "update moving the WAL through shell expansion")
	drainBody(resp)
	assert.Empty(t, getProject(t, name).Spec.Services[0].Env, "the rejected update must not be stored")
}

func TestStanzaInventory(t *testing.T) {
	const name = "e2e-pgdiff-stanzas"
	forged := postgresDifferentialProject(name, nil)
	forged.Status.Backup = &v1.ProjectBackupStatus{Stanzas: []v1.StanzaRecord{{Name: "forged", Volume: "pgdata"}}}
	created := createForCleanup(t, forged)
	assert.Nil(t, getProject(t, name).Status.Backup, "the inventory is server-owned; a client cannot seed it")

	ctx := context.Background()
	require.NoError(t, suite.store.UpdateProjectStatusWithRetry(ctx, name, func(st *v1.ProjectStatus) error {
		st.Phase, st.NodeRef, st.AssignmentGeneration = v1.ProjectPhaseScheduled, "node-a", 1
		st.AssignmentHistory = v1.AssignmentHistoryKnown
		return nil
	}))

	put := func(uid, node string, gen *int64, volume string) int {
		body := map[string]any{"uid": uid, "nodeRef": node, "volume": volume}
		if gen != nil {
			body["assignmentGeneration"] = *gen
		}
		resp := doRequest(t, http.MethodPut, "/api/v1/projects/"+name+"/backup/stanzas", mustMarshal(t, body))
		drainBody(resp)
		return resp.StatusCode
	}
	one, two := int64(1), int64(2)
	uid := created.ObjectMeta.UID

	assert.Equal(t, http.StatusNoContent, put(uid, "node-a", &one, "pgdata"))
	assert.Equal(t, http.StatusNoContent, put(uid, "node-a", &one, "pgdata"), "recording again is a no-op")
	b := getProject(t, name).Status.Backup
	require.NotNil(t, b)
	require.Len(t, b.Stanzas, 1)
	assert.Equal(t, v1.StanzaName(uid, "pgdata"), b.Stanzas[0].Name)
	assert.Equal(t, "pgdata", b.Stanzas[0].Volume)

	assert.Equal(t, http.StatusConflict, put(uid, "node-a", &two, "pgdata"), "a fence that is not current")
	assert.Equal(t, http.StatusConflict, put(uid, "node-b", &one, "pgdata"), "another node")
	assert.Equal(t, http.StatusConflict, put("other-uid", "node-a", &one, "pgdata"), "another lifetime")
	assert.Equal(t, http.StatusConflict, put(uid, "node-a", nil, "pgdata"), "an incomplete fence")
	assert.Equal(t, http.StatusBadRequest, put(uid, "node-a", &one, "nope"), "not a PostgresDifferential volume")

	require.NoError(t, suite.store.UpdateProjectStatusWithRetry(ctx, name, func(st *v1.ProjectStatus) error {
		st.Phase = v1.ProjectPhaseTerminating
		return nil
	}))
	assert.Equal(t, http.StatusConflict, put(uid, "node-a", &one, "pgdata"), "not while Terminating")
	assert.Len(t, getProject(t, name).Status.Backup.Stanzas, 1)
}
