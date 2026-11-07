//go:build e2e

package integration

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/adapter"
	"NYCU-SDC/caravanserai/test/integration/testhelper"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func committedBackupBody(uid, nodeRef string, generation int64, backupID string) []byte {
	return []byte(fmt.Sprintf(`{"uid":%q,"nodeRef":%q,"assignmentGeneration":%d,"backupID":%q}`,
		uid, nodeRef, generation, backupID))
}

// ADR-0024 C1: once an assignment is released, its Agent cannot change where the
// next assignment restores from, however long it keeps running.
func TestCommittedBackupFollowsTheCurrentAssignment(t *testing.T) {
	const (
		name   = "e2e-committed-backup"
		first  = "20261031T000000Z-aaaaaaaa"
		stale  = "20261031T010000Z-bbbbbbbb"
		second = "20261031T020000Z-cccccccc"
	)
	uid, gen := createAssigned(t, name, "node-a", v1.ProjectPhaseRunning)
	commit := func(nodeRef string, generation int64, backupID string) int {
		t.Helper()
		return putEvidence(t, name, "committed-backup", committedBackupBody(uid, nodeRef, generation, backupID))
	}

	require.Equal(t, http.StatusNoContent, commit("node-a", gen, first))
	committed := getProject(t, name).Status.Recovery.CommittedBackup
	require.NotNil(t, committed)
	assert.Equal(t, first, committed.BackupID)
	assert.Equal(t, v1.AssignmentIdentity{ProjectUID: uid, NodeName: "node-a", AssignmentGeneration: gen}, committed.Assignment)

	t.Run("committing the same generation again changes nothing", func(t *testing.T) {
		before := getProject(t, name).ObjectMeta.ResourceVersion
		assert.Equal(t, http.StatusNoContent, commit("node-a", gen, first))
		assert.Equal(t, before, getProject(t, name).ObjectMeta.ResourceVersion)
	})

	t.Run("refused for a fence that is not current", func(t *testing.T) {
		before := getProject(t, name).ObjectMeta.ResourceVersion
		assert.Equal(t, http.StatusConflict, commit("node-a", gen-1, stale))
		assert.Equal(t, http.StatusConflict, commit("node-b", gen, stale))
		assert.Equal(t, before, getProject(t, name).ObjectMeta.ResourceVersion)
	})

	t.Run("refused without a fence", func(t *testing.T) {
		assert.Equal(t, http.StatusConflict, putEvidence(t, name, "committed-backup", []byte(fmt.Sprintf(`{"backupID":%q}`, stale))))
	})

	t.Run("refused for something that is not a generation", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, commit("node-a", gen, "../../other-project"))
	})

	testhelper.ReleaseCleanly(t, suite.store, name)

	t.Run("the released assignment can no longer commit", func(t *testing.T) {
		assert.Equal(t, http.StatusConflict, commit("node-a", gen, stale))
		assert.Equal(t, first, getProject(t, name).Status.Recovery.CommittedBackupID())
	})

	require.NoError(t, adapter.NewProjectStoreAdapter(suite.store).SetProjectScheduled(context.Background(), name, "node-b"))
	p := getProject(t, name)

	t.Run("a new assignment keeps the restore source", func(t *testing.T) {
		assert.Equal(t, first, p.Status.Recovery.CommittedBackupID())
	})

	t.Run("the old assignment still cannot commit after the new one exists", func(t *testing.T) {
		assert.Equal(t, http.StatusConflict, commit("node-a", gen, stale))
		assert.Equal(t, first, getProject(t, name).Status.Recovery.CommittedBackupID())
	})

	t.Run("the new assignment moves it forward", func(t *testing.T) {
		assert.Equal(t, http.StatusNoContent, commit("node-b", p.Status.AssignmentGeneration, second))
		assert.Equal(t, second, getProject(t, name).Status.Recovery.CommittedBackupID())
	})
}

// A Project on its way out needs no restore source.
func TestCommittedBackupRefusedWhileTerminating(t *testing.T) {
	const name = "e2e-committed-backup-terminating"
	uid, gen := createAssigned(t, name, "node-a", v1.ProjectPhaseRunning)

	resp := doRequest(t, http.MethodDelete, "/api/v1/projects/"+name, nil)
	drainBody(resp)
	require.Equal(t, v1.ProjectPhaseTerminating, getProject(t, name).Status.Phase)

	assert.Equal(t, http.StatusConflict,
		putEvidence(t, name, "committed-backup", committedBackupBody(uid, "node-a", gen, "20261031T000000Z-aaaaaaaa")))
}

// The record of a deleted lifetime does not carry over to a same-name successor.
func TestCommittedBackupDoesNotReachASuccessor(t *testing.T) {
	const name = "e2e-committed-backup-successor"
	uid, gen := createAssigned(t, name, "node-a", v1.ProjectPhaseRunning)
	require.Equal(t, http.StatusNoContent,
		putEvidence(t, name, "committed-backup", committedBackupBody(uid, "node-a", gen, "20261031T000000Z-aaaaaaaa")))

	createAssigned(t, name, "node-b", v1.ProjectPhaseRunning)

	assert.Empty(t, getProject(t, name).Status.Recovery.CommittedBackupID())
}
