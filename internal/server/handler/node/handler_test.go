package node

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	serverhandler "NYCU-SDC/caravanserai/internal/server/handler"

	"github.com/NYCU-SDC/summer/pkg/middleware"
	"github.com/NYCU-SDC/summer/pkg/problem"
	"go.uber.org/zap"
)

// labelRecordingStore records the Node passed to CreateNode / UpdateNodeSpec
// so tests can tell whether a write reached the store.
type labelRecordingStore struct {
	fakeNodeStore
	written *v1.Node
}

func (s *labelRecordingStore) CreateNode(_ context.Context, n *v1.Node) error {
	s.written = n
	return nil
}

func (s *labelRecordingStore) UpdateNodeSpec(_ context.Context, n *v1.Node) error {
	s.written = n
	return nil
}

func (s *labelRecordingStore) GetNode(_ context.Context, _ string) (*v1.Node, error) {
	return s.written, nil
}

func TestCreateAndUpdateNode_TierLabelValidation(t *testing.T) {
	tests := []struct {
		name       string
		labels     map[string]string
		wantStatus int
	}{
		{name: "no tier label", labels: nil, wantStatus: http.StatusOK},
		{name: "primary", labels: map[string]string{v1.LabelNodeTier: "primary"}, wantStatus: http.StatusOK},
		{name: "backup", labels: map[string]string{v1.LabelNodeTier: "backup"}, wantStatus: http.StatusOK},
		{name: "invalid tier", labels: map[string]string{v1.LabelNodeTier: "gold"}, wantStatus: http.StatusBadRequest},
	}
	requests := []struct {
		method, path string
		okStatus     int
	}{
		{method: http.MethodPost, path: "/api/v1/nodes", okStatus: http.StatusCreated},
		{method: http.MethodPut, path: "/api/v1/nodes/pve1", okStatus: http.StatusOK},
	}

	for _, req := range requests {
		for _, tt := range tests {
			t.Run(req.method+"/"+tt.name, func(t *testing.T) {
				st := &labelRecordingStore{}
				pw := problem.NewWithMapping(serverhandler.NewProblemMapping())
				h := NewHandler(zap.NewNop(), st, fakeProjectLister{}, nil, nil, pw)
				mux := http.NewServeMux()
				h.RegisterRoutes(mux, middleware.NewSet())
				srv := httptest.NewServer(mux)
				defer srv.Close()

				body, err := json.Marshal(v1.Node{ObjectMeta: v1.ObjectMeta{Name: "pve1", Labels: tt.labels}})
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				r, err := http.NewRequest(req.method, srv.URL+req.path, bytes.NewReader(body))
				if err != nil {
					t.Fatalf("build request: %v", err)
				}
				r.Header.Set("Content-Type", "application/json")
				resp, err := http.DefaultClient.Do(r)
				if err != nil {
					t.Fatalf("request: %v", err)
				}
				defer resp.Body.Close()

				want := tt.wantStatus
				if want == http.StatusOK {
					want = req.okStatus
				}
				if resp.StatusCode != want {
					b, _ := io.ReadAll(resp.Body)
					t.Fatalf("status = %d, want %d, body = %s", resp.StatusCode, want, b)
				}
				if want == http.StatusBadRequest {
					if st.written != nil {
						t.Errorf("invalid tier reached the store: %+v", st.written)
					}
					return
				}
				if st.written == nil {
					t.Fatal("valid node was not written to the store")
				}
				if got, want := st.written.Labels[v1.LabelNodeTier], tt.labels[v1.LabelNodeTier]; got != want {
					t.Errorf("stored tier = %q, want %q", got, want)
				}
			})
		}
	}
}
