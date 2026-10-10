package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetNodeTier_KeepsSpecAndOtherLabels(t *testing.T) {
	stored := v1.Node{
		ObjectMeta: v1.ObjectMeta{Name: "pve1", Labels: map[string]string{"team": "sdc"}},
		Spec:       v1.NodeSpec{Hostname: "pve1.local", Unschedulable: true},
	}

	var methods []string
	var put v1.Node
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		assert.Equal(t, "/api/v1/nodes/pve1", r.URL.Path)
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(stored)
		case http.MethodPut:
			require.NoError(t, json.NewDecoder(r.Body).Decode(&put))
			_ = json.NewEncoder(w).Encode(put)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer srv.Close()

	err := setNodeTier(context.Background(), NewClient(srv.URL), "pve1", v1.NodeTierPrimary)
	require.NoError(t, err)

	assert.Equal(t, []string{http.MethodGet, http.MethodPut}, methods)
	assert.Equal(t, "primary", put.Labels[v1.LabelNodeTier])
	assert.Equal(t, "sdc", put.Labels["team"], "other labels are kept")
	assert.True(t, put.Spec.Unschedulable, "setting the tier must not uncordon the node")
	assert.Equal(t, "pve1.local", put.Spec.Hostname)
}

func TestSetNodeTier_UnlabeledNode(t *testing.T) {
	var put v1.Node
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(v1.Node{ObjectMeta: v1.ObjectMeta{Name: "eng-service1"}})
			return
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&put))
		_ = json.NewEncoder(w).Encode(put)
	}))
	defer srv.Close()

	err := setNodeTier(context.Background(), NewClient(srv.URL), "eng-service1", v1.NodeTierBackup)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{v1.LabelNodeTier: "backup"}, put.Labels)
}

func TestSetNodeTier_InvalidTierSendsNoRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	for _, tier := range []v1.NodeTier{"gold", "Primary", ""} {
		err := setNodeTier(context.Background(), NewClient(srv.URL), "pve1", tier)
		assert.ErrorContains(t, err, `tier must be "primary" or "backup"`, "tier %q", tier)
	}
}

func TestSetNodeTier_NodeNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected %s after failed GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"title":"Not Found","status":404,"detail":"node not found: pve9"}`))
	}))
	defer srv.Close()

	err := setNodeTier(context.Background(), NewClient(srv.URL), "pve9", v1.NodeTierPrimary)
	assert.Error(t, err)
}
