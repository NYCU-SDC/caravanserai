//go:build e2e

package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/adapter"
	"NYCU-SDC/caravanserai/internal/server/controller"
	"NYCU-SDC/caravanserai/test/integration/controllerhelper"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dnsTestHostname stands in for the server's hostname function, which a later
// change adds. It appends a fixed suffix to the ingress host.
func dnsTestHostname(_ controller.DNSProjectSnapshot, ing v1.IngressDef) (string, error) {
	return ing.Host + ".dev.cara.test", nil
}

// createDNSNode creates a Ready Node with the given overlay IP.
func createDNSNode(t *testing.T, s *controllerhelper.Suite, name, overlayIP string) {
	t.Helper()
	require.NoError(t, s.Store.CreateNode(context.Background(), &v1.Node{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Node"},
		ObjectMeta: v1.ObjectMeta{Name: name},
		Spec:       v1.NodeSpec{Hostname: name},
		Status: v1.NodeStatus{
			State:         v1.NodeStateReady,
			LastHeartbeat: time.Now().UTC(),
			Network:       v1.NodeNetworkStatus{OverlayIP: overlayIP},
		},
	}))
}

// createDNSProject creates a Project in phase on node with one Internal
// ingress for host.
func createDNSProject(t *testing.T, s *controllerhelper.Suite, name string, phase v1.ProjectPhase, node, host string) {
	t.Helper()
	require.NoError(t, s.Store.CreateProject(context.Background(), &v1.Project{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Project"},
		ObjectMeta: v1.ObjectMeta{Name: name},
		Spec: v1.ProjectSpec{
			Size:     v1.ProjectSizeSmall,
			Services: []v1.ServiceDef{{Name: "web", Image: "nginx:1.27"}},
			Ingress: []v1.IngressDef{{
				Name:   "web",
				Host:   host,
				Target: v1.IngressTarget{Service: "web", Port: 80},
				Access: v1.IngressAccess{Scope: v1.IngressScopeInternal},
			}},
		},
		Status: v1.ProjectStatus{Phase: phase, NodeRef: node},
	}))
}

// startDNSController runs only the DNS controller against the suite's real
// store and bus, and returns the records file path.
func startDNSController(t *testing.T, s *controllerhelper.Suite, seedInterval time.Duration) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "extra-records.json")
	ctrl, err := controller.NewDNSRecordsController(s.Logger, adapter.NewDNSStoreAdapter(s.Store), s.Bus,
		path, dnsTestHostname, controller.WithSeedInterval(seedInterval))
	require.NoError(t, err)

	mgr := controller.NewManager(s.Logger, controller.WithErrorBackoff(100*time.Millisecond))
	mgr.Add(ctrl)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = mgr.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Log("Warning: DNS controller manager did not shut down within 5s")
		}
	})
	return path
}

// waitForRecordsFile waits until the records file holds exactly want.
func waitForRecordsFile(t *testing.T, path, want string) {
	t.Helper()
	controllerhelper.WaitForCondition(t, 10*time.Second, 100*time.Millisecond,
		"records file = "+want,
		func() bool {
			got, err := os.ReadFile(path)
			return err == nil && string(got) == want
		},
	)
}

const (
	dnsRecordsOnNodeA = `[
  {
    "name": "www.dev.cara.test",
    "type": "A",
    "value": "100.64.0.5"
  }
]
`
	dnsRecordsOnNodeB = `[
  {
    "name": "www.dev.cara.test",
    "type": "A",
    "value": "100.64.0.6"
  }
]
`
)

// TestDNSRecordsFollowProjectLifecycle validates, through store events alone:
//
//  1. A Running Project publishes its record; a Pending one does not.
//  2. Moving the Project to another Node rewrites the record's IP.
//  3. A Terminating Project's record is removed.
//
// The resync ticker is set to an hour, so only events can drive these steps.
func TestDNSRecordsFollowProjectLifecycle(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)
	ctx := context.Background()

	createDNSNode(t, s, "dns-node-a", "100.64.0.5")
	createDNSNode(t, s, "dns-node-b", "100.64.0.6")
	createDNSProject(t, s, "dns-blog", v1.ProjectPhaseRunning, "dns-node-a", "www")
	createDNSProject(t, s, "dns-waiting", v1.ProjectPhasePending, "", "pending")

	path := startDNSController(t, s, time.Hour)
	waitForRecordsFile(t, path, dnsRecordsOnNodeA)

	require.NoError(t, s.Store.UpdateProjectStatusWithRetry(ctx, "dns-blog", func(st *v1.ProjectStatus) error {
		st.NodeRef = "dns-node-b"
		return nil
	}))
	waitForRecordsFile(t, path, dnsRecordsOnNodeB)

	require.NoError(t, s.Store.UpdateProjectStatusWithRetry(ctx, "dns-blog", func(st *v1.ProjectStatus) error {
		st.Phase = v1.ProjectPhaseTerminating
		return nil
	}))
	waitForRecordsFile(t, path, "[]\n")
}

// TestDNSRecordsPickUpOverlayIPChangeOnResync validates that a Node overlay IP
// change, which publishes no event while the Node stays Ready, still reaches
// the file through the resync ticker.
func TestDNSRecordsPickUpOverlayIPChangeOnResync(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)
	ctx := context.Background()

	createDNSNode(t, s, "dns-node-a", "100.64.0.5")
	createDNSProject(t, s, "dns-blog", v1.ProjectPhaseRunning, "dns-node-a", "www")

	path := startDNSController(t, s, 200*time.Millisecond)
	waitForRecordsFile(t, path, dnsRecordsOnNodeA)

	node, err := s.Store.GetNode(ctx, "dns-node-a")
	require.NoError(t, err)
	status := node.Status
	status.Network.OverlayIP = "100.64.0.6"
	require.NoError(t, s.Store.UpdateNodeStatus(ctx, "dns-node-a", status))

	waitForRecordsFile(t, path, dnsRecordsOnNodeB)
}

// TestDNSRecordsOlderProjectKeepsDuplicateHostname validates the duplicate
// rule against real creation timestamps from PostgreSQL.
func TestDNSRecordsOlderProjectKeepsDuplicateHostname(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)

	createDNSNode(t, s, "dns-node-a", "100.64.0.5")
	createDNSNode(t, s, "dns-node-b", "100.64.0.6")
	createDNSProject(t, s, "dns-zz-older", v1.ProjectPhaseRunning, "dns-node-a", "www")
	createDNSProject(t, s, "dns-aa-newer", v1.ProjectPhaseRunning, "dns-node-b", "www")

	path := startDNSController(t, s, time.Hour)

	// dns-aa-newer sorts first by name, so only creation time can make
	// dns-zz-older win.
	waitForRecordsFile(t, path, dnsRecordsOnNodeA)
}

// TestDNSRecordsStableAcrossRestart validates that a fresh controller, as
// after a cara-server restart, produces a byte-identical file and does not
// rewrite it.
func TestDNSRecordsStableAcrossRestart(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)
	ctx := context.Background()

	createDNSNode(t, s, "dns-node-a", "100.64.0.5")
	createDNSNode(t, s, "dns-node-b", "100.64.0.6")
	createDNSProject(t, s, "dns-blog", v1.ProjectPhaseRunning, "dns-node-a", "www")
	createDNSProject(t, s, "dns-wiki", v1.ProjectPhaseFailed, "dns-node-b", "wiki")
	createDNSProject(t, s, "dns-api", v1.ProjectPhaseScheduled, "dns-node-a", "api")

	path := filepath.Join(t.TempDir(), "extra-records.json")
	newCtrl := func() *controller.DNSRecordsController {
		c, err := controller.NewDNSRecordsController(s.Logger, adapter.NewDNSStoreAdapter(s.Store), nil,
			path, dnsTestHostname)
		require.NoError(t, err)
		return c
	}

	_, err := newCtrl().Reconcile(ctx, "extra-records")
	require.NoError(t, err)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(path, past, past))

	_, err = newCtrl().Reconcile(ctx, "extra-records")
	require.NoError(t, err)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, info.ModTime().Equal(past), "restart rewrote an unchanged file")
	assert.JSONEq(t, `[
		{"name":"api.dev.cara.test","type":"A","value":"100.64.0.5"},
		{"name":"wiki.dev.cara.test","type":"A","value":"100.64.0.6"},
		{"name":"www.dev.cara.test","type":"A","value":"100.64.0.5"}
	]`, string(after))
}
