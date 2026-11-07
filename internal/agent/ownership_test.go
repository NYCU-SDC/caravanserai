package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// The backup started under uid-1, generation 7, on node-a.
func TestResolveComparesAgainstTheAssignmentTheBackupStartedUnder(t *testing.T) {
	for name, tc := range map[string]struct {
		uid        string
		phase      v1.ProjectPhase
		nodeRef    string
		generation int64
		want       backup.Ownership
	}{
		"same assignment":            {"uid-1", v1.ProjectPhaseRunning, "node-a", 7, backup.OwnershipRetained},
		"moved away and back":        {"uid-1", v1.ProjectPhaseRunning, "node-a", 9, backup.OwnershipReassigned},
		"on another node":            {"uid-1", v1.ProjectPhaseRunning, "node-b", 8, backup.OwnershipReassigned},
		"released":                   {"uid-1", v1.ProjectPhasePending, "", 7, backup.OwnershipLost},
		"terminating":                {"uid-1", v1.ProjectPhaseTerminating, "node-a", 7, backup.OwnershipTerminating},
		"recreated, same node":       {"uid-2", v1.ProjectPhaseRunning, "node-a", 1, backup.OwnershipLost},
		"recreated, same generation": {"uid-2", v1.ProjectPhaseRunning, "node-a", 7, backup.OwnershipLost},
		"recreated, being deleted":   {"uid-2", v1.ProjectPhaseTerminating, "node-a", 7, backup.OwnershipLost},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				require.NoError(t, json.NewEncoder(w).Encode(v1.Project{
					ObjectMeta: v1.ObjectMeta{Name: "blog", Namespace: "default", UID: tc.uid},
					Status:     v1.ProjectStatus{Phase: tc.phase, NodeRef: tc.nodeRef, AssignmentGeneration: tc.generation},
				}))
			}))
			defer server.Close()

			resolver := NewOwnershipResolver(NewClient(zap.NewNop(), server.URL, "node-a"), "node-a", zap.NewNop())

			got := resolver.Resolve(context.Background(), backup.ResourceKey{Namespace: "default", Name: "blog"}, "uid-1", 7)

			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.want == backup.OwnershipRetained, backup.ShouldRestartContainers(backup.ExitSuccess, got),
				"only the assignment the backup started under may be restarted")
		})
	}
}

func TestResolveSeparatesAGoneProjectFromAnUnreachableServer(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	defer gone.Close()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()
	key := backup.ResourceKey{Namespace: "default", Name: "blog"}

	assert.Equal(t, backup.OwnershipLost,
		NewOwnershipResolver(NewClient(zap.NewNop(), gone.URL, "node-a"), "node-a", zap.NewNop()).Resolve(context.Background(), key, "uid-1", 7))
	assert.Equal(t, backup.OwnershipUnknown,
		NewOwnershipResolver(NewClient(zap.NewNop(), failing.URL, "node-a"), "node-a", zap.NewNop()).Resolve(context.Background(), key, "uid-1", 7))
}
