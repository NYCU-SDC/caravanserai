//go:build e2e

package integration

import (
	"encoding/json"
	"net/http"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSecretCRUD exercises the full Secret lifecycle over HTTP:
//
//	POST   /api/v1/secrets            → 201, resourceVersion 1
//	POST   (duplicate)               → 409
//	GET    /api/v1/secrets           → list contains it
//	GET    /api/v1/secrets/{name}    → 200, values returned in plaintext (API does not redact)
//	PUT    /api/v1/secrets/{name}    → 200, value updated, resourceVersion incremented
//	DELETE /api/v1/secrets/{name}    → 204
//	GET    after delete              → 404
func TestSecretCRUD(t *testing.T) {
	const secretName = "e2e-secret-crud"

	// ── 1. Create ────────────────────────────────────────────────────────────
	createBody := mustMarshal(t, v1.Secret{
		ObjectMeta: v1.ObjectMeta{Name: secretName},
		Spec: v1.SecretSpec{
			Data: []v1.SecretDataItem{
				{Key: "db-password", Value: "hunter2"},
				{Key: "api-key", Value: "abc123"},
			},
		},
	})
	resp := doRequest(t, http.MethodPost, "/api/v1/secrets", createBody)
	require.Equal(t, http.StatusCreated, resp.StatusCode, "create secret: expected 201")

	var created v1.Secret
	mustDecodeBody(t, resp, &created)
	assert.Equal(t, secretName, created.Name)
	assert.Equal(t, "default", created.Namespace, "namespace normalized to default")
	assert.Equal(t, int64(1), created.ResourceVersion, "resource_version starts at 1")

	// ── 2. Duplicate create → 409 ────────────────────────────────────────────
	resp = doRequest(t, http.MethodPost, "/api/v1/secrets", createBody)
	assert.Equal(t, http.StatusConflict, resp.StatusCode, "duplicate create: expected 409")
	drainBody(resp)

	// ── 3. List → contains our secret ────────────────────────────────────────
	resp = doRequest(t, http.MethodGet, "/api/v1/secrets", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "list secrets: expected 200")

	var list v1.SecretList
	mustDecodeBody(t, resp, &list)
	found := false
	for _, s := range list.Items {
		if s.Name == secretName {
			found = true
		}
	}
	assert.True(t, found, "list must contain the created secret")

	// ── 4. Get by name → API returns real values (no redaction server-side) ──
	resp = doRequest(t, http.MethodGet, "/api/v1/secrets/"+secretName, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "get secret: expected 200")

	var fetched v1.Secret
	mustDecodeBody(t, resp, &fetched)
	require.Len(t, fetched.Spec.Data, 2)
	assert.Equal(t, "hunter2", fetched.Spec.Data[0].Value,
		"API GET must return the real value — the agent needs it")

	// ── 5. PUT rotation → value updated, resource_version incremented ────────
	updateBody := mustMarshal(t, v1.Secret{
		ObjectMeta: v1.ObjectMeta{Name: secretName},
		Spec: v1.SecretSpec{
			Data: []v1.SecretDataItem{
				{Key: "db-password", Value: "newpass"},
				{Key: "api-key", Value: "abc123"},
			},
		},
	})
	resp = doRequest(t, http.MethodPut, "/api/v1/secrets/"+secretName, updateBody)
	require.Equal(t, http.StatusOK, resp.StatusCode, "update secret: expected 200")

	var updated v1.Secret
	mustDecodeBody(t, resp, &updated)
	assert.Equal(t, int64(2), updated.ResourceVersion, "resource_version increments on update")

	// Confirm the rotated value is actually stored.
	resp = doRequest(t, http.MethodGet, "/api/v1/secrets/"+secretName, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var afterRotate v1.Secret
	mustDecodeBody(t, resp, &afterRotate)
	assert.Equal(t, "newpass", afterRotate.Spec.Data[0].Value, "rotated value must be persisted")

	// ── 6. Delete → 204 ──────────────────────────────────────────────────────
	resp = doRequest(t, http.MethodDelete, "/api/v1/secrets/"+secretName, nil)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode, "delete secret: expected 204")
	drainBody(resp)

	// ── 7. Get after delete → 404 ────────────────────────────────────────────
	resp = doRequest(t, http.MethodGet, "/api/v1/secrets/"+secretName, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "get after delete: expected 404")
	drainBody(resp)
}

// TestSecretValidation covers the apply-time guards on Secret specs.
func TestSecretValidation(t *testing.T) {
	type testCase struct {
		name       string
		secretName string
		namespace  string
		data       []v1.SecretDataItem
		wantStatus int
	}

	testCases := []testCase{
		{
			name:       "Valid secret returns 201",
			secretName: "e2e-secret-valid",
			data:       []v1.SecretDataItem{{Key: "k", Value: "v"}},
			wantStatus: http.StatusCreated,
		},
		{
			name:       "Empty data returns 400",
			secretName: "e2e-secret-empty",
			data:       []v1.SecretDataItem{},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "Empty key returns 400",
			secretName: "e2e-secret-emptykey",
			data:       []v1.SecretDataItem{{Key: "", Value: "v"}},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "Duplicate key returns 400",
			secretName: "e2e-secret-dupkey",
			data:       []v1.SecretDataItem{{Key: "dup", Value: "a"}, {Key: "dup", Value: "b"}},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "Non-default namespace returns 400",
			secretName: "e2e-secret-ns",
			namespace:  "blog-team",
			data:       []v1.SecretDataItem{{Key: "k", Value: "v"}},
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			body := mustMarshal(t, v1.Secret{
				ObjectMeta: v1.ObjectMeta{Name: tc.secretName, Namespace: tc.namespace},
				Spec:       v1.SecretSpec{Data: tc.data},
			})
			resp := doRequest(t, http.MethodPost, "/api/v1/secrets", body)
			require.Equal(t, tc.wantStatus, resp.StatusCode,
				"%s: expected %d", tc.name, tc.wantStatus)

			if tc.wantStatus == http.StatusBadRequest {
				var p problemResponse
				require.NoError(t, json.NewDecoder(resp.Body).Decode(&p))
				assert.Equal(t, http.StatusBadRequest, p.Status)
			} else {
				drainBody(resp)
			}
		})
	}

	// Cleanup the one secret that was successfully created.
	resp := doRequest(t, http.MethodDelete, "/api/v1/secrets/e2e-secret-valid", nil)
	assert.Contains(t, []int{http.StatusNoContent, http.StatusNotFound}, resp.StatusCode)
	drainBody(resp)
}
