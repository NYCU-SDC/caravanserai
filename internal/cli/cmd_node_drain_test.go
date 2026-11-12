package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetNodeScheduling(t *testing.T) {
	tests := []struct {
		name              string
		start             v1.NodeSpec
		want              nodeScheduling
		wantUnschedulable bool
		wantDrain         bool
	}{
		{name: "cordon", want: cordonScheduling, wantUnschedulable: true},
		{name: "cordon leaves a drain in progress alone", start: v1.NodeSpec{Unschedulable: true, Drain: true},
			want: cordonScheduling, wantUnschedulable: true, wantDrain: true},
		{name: "drain cordons too", want: drainScheduling, wantUnschedulable: true, wantDrain: true},
		{name: "uncordon clears both", start: v1.NodeSpec{Unschedulable: true, Drain: true}, want: uncordonScheduling},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stored := v1.Node{
				ObjectMeta: v1.ObjectMeta{Name: "pve1", Labels: map[string]string{v1.LabelNodeTier: "primary"}},
				Spec:       tt.start,
			}
			stored.Spec.Hostname = "pve1.local"

			var put v1.Node
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(stored)
					return
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&put))
				_ = json.NewEncoder(w).Encode(put)
			}))
			defer srv.Close()

			require.NoError(t, setNodeScheduling(context.Background(), NewClient(srv.URL), "pve1", tt.want))
			assert.Equal(t, tt.wantUnschedulable, put.Spec.Unschedulable)
			assert.Equal(t, tt.wantDrain, put.Spec.Drain)
			assert.Equal(t, "pve1.local", put.Spec.Hostname, "the rest of the spec is kept")
			assert.Equal(t, "primary", put.Labels[v1.LabelNodeTier], "labels are kept")
		})
	}
}

func TestDescribeNodeDrain(t *testing.T) {
	tests := []struct {
		name string
		node v1.Node
		want []string
	}{
		{name: "none", want: []string{`(?m)^  Drain:\s+<none>$`}},
		{name: "requested, not yet picked up", node: v1.Node{Spec: v1.NodeSpec{Drain: true}}, want: []string{`(?m)^  Drain:\s+Requested$`}},
		{
			name: "incomplete lists what blocks it",
			node: v1.Node{
				Spec: v1.NodeSpec{Drain: true},
				Status: v1.NodeStatus{Drain: &v1.NodeDrainStatus{
					Phase: v1.NodeDrainIncomplete, Remaining: 2, Blocking: []string{"blog: Failed here"},
				}},
			},
			want: []string{`(?m)^  Drain:\s+Incomplete \(2 left\)$`, `(?m)^    - blog: Failed here$`},
		},
		{
			name: "drained",
			node: v1.Node{Spec: v1.NodeSpec{Drain: true}, Status: v1.NodeStatus{Drain: &v1.NodeDrainStatus{Phase: v1.NodeDrainDrained}}},
			want: []string{`(?m)^  Drain:\s+Drained$`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			describeNode(&buf, &tt.node)
			for _, re := range tt.want {
				assert.Regexp(t, regexp.MustCompile(re), buf.String())
			}
		})
	}
}
