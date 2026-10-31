package project

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"NYCU-SDC/caravanserai/internal/server/handler"

	"github.com/NYCU-SDC/summer/pkg/problem"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// A server with no OPERATOR_TOKEN must not treat an empty credential as a match.
func TestTakeoverDisabledWithoutToken(t *testing.T) {
	h := NewHandler(zap.NewNop(), nil, problem.NewWithMapping(handler.NewProblemMapping()), true)
	for _, auth := range []string{"", "Bearer "} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/web/recovery/takeover", strings.NewReader("{}"))
		req.Header.Set("Authorization", auth)
		rec := httptest.NewRecorder()
		h.postTakeover(rec, req)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	}
}
