package project

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/auth"
	"NYCU-SDC/caravanserai/internal/server/handler"

	"github.com/NYCU-SDC/summer/pkg/problem"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// A takeover can run a workload on two Nodes, so only an admin may ask for one:
// a request with no caller is refused, and so is a Node, whatever header it
// carries.
func TestTakeoverRequiresAdmin(t *testing.T) {
	h := NewHandler(zap.NewNop(), nil, problem.NewWithMapping(handler.NewProblemMapping()))
	cases := []struct {
		name   string
		ctx    context.Context
		status int
	}{
		{"no caller", context.Background(), http.StatusUnauthorized},
		{"node", auth.WithPrincipal(context.Background(), auth.Principal{Kind: v1.APITokenKindNode, Name: "node-a"}), http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/web/recovery/takeover", strings.NewReader("{}")).WithContext(tc.ctx)
			req.Header.Set("Authorization", "Bearer anything")
			rec := httptest.NewRecorder()
			h.postTakeover(rec, req)
			assert.Equal(t, tc.status, rec.Code)
		})
	}
}
