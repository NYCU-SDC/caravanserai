package testhelper

import (
	"context"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	pgstore "NYCU-SDC/caravanserai/internal/store/postgres"

	"github.com/stretchr/testify/require"
)

// UID returns the named Project's UID, for writes pinned to its lifetime.
func UID(t *testing.T, s *pgstore.Store, name string) string {
	t.Helper()
	p, err := s.GetProject(context.Background(), name)
	require.NoError(t, err)
	return p.ObjectMeta.UID
}

// ForcePhase puts the named Project into phase with no writer, transition or
// assignment check, stamping the Phase condition the way a real transition
// would. It exists for tests that need a Project in a phase directly; no
// production path may write a phase this way (CARA-81). It replaces the
// adapter's SetProjectPhase, removed in CARA-81 stage 3.
func ForcePhase(ctx context.Context, s *pgstore.Store, name string, phase v1.ProjectPhase, reason, message string) error {
	// Fixed before the closure, which may run more than once.
	now := time.Now().UTC()
	return s.UpdateProjectStatusWithRetry(ctx, name, func(status *v1.ProjectStatus) error {
		transitioned := status.Phase != phase
		status.Phase = phase
		if phase == v1.ProjectPhaseFailed {
			status.Recovery.ClearHealthyWindow()
		}
		status.Conditions = v1.UpsertCondition(status.Conditions, v1.Condition{
			Type:               v1.ConditionTypePhase,
			Status:             v1.ConditionTrue,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: now,
		}, transitioned)
		return nil
	})
}
