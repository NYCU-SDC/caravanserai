package cli

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
)

// collectFieldPaths recursively collects all leaf field paths in a struct type.
// Embedded (anonymous) structs are flattened. The paths use dot notation,
// e.g. "Spec.Hostname", "Status.Network.OverlayIP".
func collectFieldPaths(t reflect.Type, prefix string) []string {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return []string{prefix}
	}

	var paths []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)

		// Skip unexported fields.
		if !f.IsExported() {
			continue
		}

		// Build the dot-separated path.
		name := f.Name
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}

		ft := f.Type
		if ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}

		// Flatten embedded (anonymous) structs.
		if f.Anonymous && ft.Kind() == reflect.Struct {
			paths = append(paths, collectFieldPaths(ft, prefix)...)
			continue
		}

		// Recurse into non-collection struct fields.
		if ft.Kind() == reflect.Struct &&
			ft.Kind() != reflect.Map &&
			name != "CreatedAt" && name != "UpdatedAt" &&
			name != "LastHeartbeatTime" && name != "LastTransitionTime" &&
			name != "LastTestTime" && name != "LastHeartbeat" &&
			name != "ExpireAt" {
			paths = append(paths, collectFieldPaths(ft, path)...)
			continue
		}

		paths = append(paths, path)
	}
	return paths
}

// describeNodeCoveredFields lists every field path that describeNode explicitly
// handles. When a new field is added to v1.Node (or its nested structs), add
// the path here — otherwise TestDescribeNodeFieldCoverage will fail.
//
// Paths use dot notation matching the Go struct hierarchy, e.g. "Spec.Hostname".
// Embedded TypeMeta/ObjectMeta fields are flattened (no prefix).
var describeNodeCoveredFields = newStringSet(
	// TypeMeta (embedded)
	"APIVersion",
	"Kind",
	// ObjectMeta (embedded)
	"Name",
	"UID",
	"Namespace",
	"ResourceVersion",
	"Labels",
	"Annotations",
	"CreatedAt",
	"UpdatedAt",
	// Spec
	"Spec.Hostname",
	"Spec.Unschedulable",
	// Status
	"Status.State",
	"Status.LastHeartbeat",
	"Status.Conditions",
	"Status.Capacity",
	"Status.Allocatable",
	// Status.Info
	"Status.Info.KernelVersion",
	"Status.Info.OSImage",
	"Status.Info.AgentVersion",
	// Status.Network
	"Status.Network.OverlayIP",
	"Status.Network.OverlayStatus",
	"Status.Network.DNSName",
	"Status.Network.Mode",
	"Status.Network.AgentPort",
	"Status.Network.Throughput.Download",
	"Status.Network.Throughput.Upload",
	"Status.Network.Throughput.LastTestTime",
)

// describeProjectCoveredFields lists every field path that describeProject
// explicitly handles.
var describeProjectCoveredFields = newStringSet(
	// TypeMeta (embedded)
	"APIVersion",
	"Kind",
	// ObjectMeta (embedded)
	"Name",
	"UID",
	"Namespace",
	"ResourceVersion",
	"Labels",
	"Annotations",
	"CreatedAt",
	"UpdatedAt",
	// Spec
	"Spec.Services",
	"Spec.Volumes",
	"Spec.Backup.Interval",
	"Spec.Backup.OnMissing",
	"Spec.Ingress",
	"Spec.ExpireAt",
	// Status
	"Status.Phase",
	"Status.NodeRef",
	"Status.AssignmentGeneration",
	"Status.AssignmentHistory",
	"Status.Recovery.HealthySinceGeneration",
	"Status.Recovery.PreferredNode",
	"Status.Recovery.PreviousAssignment.AssignmentGeneration",
	"Status.Recovery.PreviousAssignment.NodeName",
	"Status.Recovery.PreviousAssignment.ProjectUID",
	"Status.Recovery.RescheduleAttempts",
	"Status.Recovery.ResourcesRemoved.Assignment.AssignmentGeneration",
	"Status.Recovery.ResourcesRemoved.Assignment.NodeName",
	"Status.Recovery.ResourcesRemoved.Assignment.ProjectUID",
	"Status.Recovery.WorkloadStopped.Assignment.AssignmentGeneration",
	"Status.Recovery.WorkloadStopped.Assignment.NodeName",
	"Status.Recovery.WorkloadStopped.Assignment.ProjectUID",
	"Status.Conditions",
)

// describeSecretCoveredFields lists every field path that describeSecret
// explicitly handles. "Spec.Data" covers the whole slice: describeSecret
// prints each item's Key but must never print Value, so there is no separate
// "Spec.Data.Value" path to register.
var describeSecretCoveredFields = newStringSet(
	// TypeMeta (embedded)
	"APIVersion",
	"Kind",
	// ObjectMeta (embedded)
	"Name",
	"UID",
	"Namespace",
	"ResourceVersion",
	"Labels",
	"Annotations",
	"CreatedAt",
	"UpdatedAt",
	// Spec
	"Spec.Data",
)

func TestDescribeSecretFieldCoverage(t *testing.T) {
	actual := collectFieldPaths(reflect.TypeOf(v1.Secret{}), "")
	sort.Strings(actual)

	var missing []string
	for _, path := range actual {
		if !describeSecretCoveredFields.has(path) {
			missing = append(missing, path)
		}
	}

	assert.Empty(t, missing,
		"describeSecret does not cover these Secret fields — add formatting in "+
			"cmd_describe.go and register the paths in describeSecretCoveredFields:\n  %s",
		strings.Join(missing, "\n  "))
}

func TestDescribeNodeFieldCoverage(t *testing.T) {
	actual := collectFieldPaths(reflect.TypeOf(v1.Node{}), "")
	sort.Strings(actual)

	var missing []string
	for _, path := range actual {
		if !describeNodeCoveredFields.has(path) {
			missing = append(missing, path)
		}
	}

	assert.Empty(t, missing,
		"describeNode does not cover these Node fields — add formatting in "+
			"cmd_describe.go and register the paths in describeNodeCoveredFields:\n  %s",
		strings.Join(missing, "\n  "))
}

func TestDescribeProjectFieldCoverage(t *testing.T) {
	actual := collectFieldPaths(reflect.TypeOf(v1.Project{}), "")
	sort.Strings(actual)

	var missing []string
	for _, path := range actual {
		if !describeProjectCoveredFields.has(path) {
			missing = append(missing, path)
		}
	}

	assert.Empty(t, missing,
		"describeProject does not cover these Project fields — add formatting in "+
			"cmd_describe.go and register the paths in describeProjectCoveredFields:\n  %s",
		strings.Join(missing, "\n  "))
}

// stringSet is a simple set of strings for O(1) membership checks.
type stringSet map[string]struct{}

func newStringSet(vals ...string) stringSet {
	s := make(stringSet, len(vals))
	for _, v := range vals {
		s[v] = struct{}{}
	}
	return s
}

func (s stringSet) has(v string) bool {
	_, ok := s[v]
	return ok
}

// The Recovery section appears only once recovery has recorded something, and
// a healthy window from an older assignment is labelled stale rather than shown
// as if it described the current one.
func TestDescribeProjectRecoverySection(t *testing.T) {
	var none strings.Builder
	describeProject(&none, &v1.Project{Status: v1.ProjectStatus{Phase: v1.ProjectPhaseRunning}})
	assert.NotContains(t, none.String(), "Recovery:")

	healthy := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	prev := v1.AssignmentIdentity{ProjectUID: "uid-1", NodeName: "node-a", AssignmentGeneration: 6}
	var out strings.Builder
	describeProject(&out, &v1.Project{Status: v1.ProjectStatus{
		AssignmentGeneration: 7,
		Recovery: &v1.ProjectRecoveryStatus{
			RescheduleAttempts:     2,
			HealthySince:           &healthy,
			HealthySinceGeneration: 6,
			PreviousAssignment:     &prev,
		},
	}})
	text := out.String()
	assert.Contains(t, text, "Recovery:")
	assert.Contains(t, text, "(stale: generation 6)")
	assert.Contains(t, text, "node-a generation 6 (uid uid-1)")
}
