package cli

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPIError_Error(t *testing.T) {
	tests := []struct {
		name string
		err  APIError
		want string
	}{
		{
			name: "title and detail",
			err: APIError{
				StatusCode: 409,
				Status:     "409 Conflict",
				Title:      "Conflict",
				Detail:     `cannot delete node "test-node": 1 project(s) still assigned`,
			},
			want: `Conflict: cannot delete node "test-node": 1 project(s) still assigned`,
		},
		{
			name: "title and validation errors",
			err: APIError{
				StatusCode: 400,
				Status:     "400 Bad Request",
				Title:      "Validation Error",
				Errors:     []string{"name is required", "endpoint must be a valid URL"},
			},
			want: "Validation Error: name is required; endpoint must be a valid URL",
		},
		{
			name: "title and detail take precedence over errors",
			err: APIError{
				StatusCode: 400,
				Status:     "400 Bad Request",
				Title:      "Bad Request",
				Detail:     "invalid manifest",
				Errors:     []string{"name is required"},
			},
			want: "Bad Request: invalid manifest",
		},
		{
			name: "detail only (no title)",
			err: APIError{
				StatusCode: 500,
				Status:     "500 Internal Server Error",
				Detail:     "something went wrong",
			},
			want: "something went wrong",
		},
		{
			name: "title only (no detail or errors)",
			err: APIError{
				StatusCode: 404,
				Status:     "404 Not Found",
				Title:      "Not Found",
			},
			want: "Not Found",
		},
		{
			name: "errors only (no title)",
			err: APIError{
				StatusCode: 400,
				Status:     "400 Bad Request",
				Errors:     []string{"field x is invalid"},
			},
			want: "field x is invalid",
		},
		{
			name: "fallback (no title, no detail, no errors)",
			err: APIError{
				StatusCode: 502,
				Status:     "502 Bad Gateway",
			},
			want: "server returned 502 Bad Gateway",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.err.Error())
		})
	}
}

func TestParseAPIError_ValidProblemJSON(t *testing.T) {
	body := []byte(`{"title":"Conflict","status":409,"type":"https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/409","detail":"cannot delete node \"test-node\": 1 project(s) still assigned"}`)

	apiErr := ParseAPIError(body, "409 Conflict", 409)

	assert.Equal(t, 409, apiErr.StatusCode)
	assert.Equal(t, "409 Conflict", apiErr.Status)
	assert.Equal(t, "Conflict", apiErr.Title)
	assert.Equal(t, `cannot delete node "test-node": 1 project(s) still assigned`, apiErr.Detail)
	assert.Nil(t, apiErr.Errors)
}

func TestParseAPIError_ValidationErrors(t *testing.T) {
	body := []byte(`{"title":"Validation Error","status":400,"detail":"","errors":["name is required","endpoint must be a valid URL"]}`)

	apiErr := ParseAPIError(body, "400 Bad Request", 400)

	assert.Equal(t, "Validation Error", apiErr.Title)
	assert.Empty(t, apiErr.Detail)
	assert.Equal(t, []string{"name is required", "endpoint must be a valid URL"}, apiErr.Errors)
}

func TestParseAPIError_InvalidJSON(t *testing.T) {
	body := []byte(`this is not json`)

	apiErr := ParseAPIError(body, "500 Internal Server Error", 500)

	assert.Equal(t, 500, apiErr.StatusCode)
	assert.Empty(t, apiErr.Title)
	assert.Equal(t, "server returned 500 Internal Server Error: this is not json", apiErr.Detail)
}

func TestParseAPIError_EmptyBody(t *testing.T) {
	apiErr := ParseAPIError([]byte{}, "404 Not Found", 404)

	assert.Equal(t, 404, apiErr.StatusCode)
	assert.Empty(t, apiErr.Title)
	assert.Equal(t, "server returned 404 Not Found: ", apiErr.Detail)
}

func TestParseAPIError_JSONWithoutTitle(t *testing.T) {
	// Valid JSON but missing the title field — should fall back.
	body := []byte(`{"message":"something unexpected"}`)

	apiErr := ParseAPIError(body, "500 Internal Server Error", 500)

	assert.Empty(t, apiErr.Title)
	assert.Contains(t, apiErr.Detail, "server returned 500 Internal Server Error")
}

func TestCheckStatus_Success(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.NoError(t, checkStatus(resp))
}

func TestCheckStatus_ProblemDetail(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"title":"Conflict","status":409,"detail":"resource already exists"}`))
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	checkErr := checkStatus(resp)
	require.Error(t, checkErr)

	// Should be an *APIError.
	var apiErr *APIError
	require.True(t, errors.As(checkErr, &apiErr))
	assert.Equal(t, 409, apiErr.StatusCode)
	assert.Equal(t, "Conflict", apiErr.Title)
	assert.Equal(t, "resource already exists", apiErr.Detail)

	// Error() should format cleanly.
	assert.Equal(t, "Conflict: resource already exists", apiErr.Error())
}

func TestCheckStatus_NonJSONBody(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream timeout"))
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	checkErr := checkStatus(resp)
	require.Error(t, checkErr)

	var apiErr *APIError
	require.True(t, errors.As(checkErr, &apiErr))
	assert.Equal(t, 502, apiErr.StatusCode)
	assert.Contains(t, apiErr.Error(), "upstream timeout")
}

func TestCheckStatus_NotFoundProblem(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"title":"Not Found","status":404,"detail":"node \"nonexistent\" not found"}`))
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	checkErr := checkStatus(resp)
	require.Error(t, checkErr)

	var apiErr *APIError
	require.True(t, errors.As(checkErr, &apiErr))
	assert.Equal(t, `Not Found: node "nonexistent" not found`, apiErr.Error())
}
