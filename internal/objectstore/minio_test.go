package objectstore

import (
	"errors"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  minio.ErrorResponse
		want bool
	}{
		{name: "NoSuchKey", err: minio.ErrorResponse{Code: "NoSuchKey"}, want: true},
		{name: "NoSuchBucket", err: minio.ErrorResponse{Code: "NoSuchBucket"}, want: true},
		{name: "404 status with unrelated code", err: minio.ErrorResponse{Code: "SomethingElse", StatusCode: 404}, want: true},
		{name: "AccessDenied is not not-found", err: minio.ErrorResponse{Code: "AccessDenied", StatusCode: 403}, want: false},
		{name: "InternalError is not not-found", err: minio.ErrorResponse{Code: "InternalError", StatusCode: 500}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isNotFound(tt.err))
		})
	}
}

func TestIsNotFoundIgnoresUnrelatedErrors(t *testing.T) {
	// ToErrorResponse falls back to a zero-value ErrorResponse for errors it
	// doesn't recognise; isNotFound must not treat that zero value as a match.
	err := errors.New("connection refused")
	assert.False(t, isNotFound(err))
}

func TestToObjectMeta(t *testing.T) {
	now := time.Now()
	info := minio.ObjectInfo{
		Key:          "cara/v1/projects/default/blog/latest.json",
		ETag:         "abc123",
		Size:         42,
		LastModified: now,
	}

	got := toObjectMeta(info)

	assert.Equal(t, ObjectMeta{
		Key:          info.Key,
		ETag:         info.ETag,
		Size:         info.Size,
		LastModified: now,
	}, got)
}

func TestNewMinioStoreValidatesConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     MinioConfig
		wantErr string
	}{
		{
			name:    "empty endpoint is rejected",
			cfg:     MinioConfig{Bucket: "cara-backups"},
			wantErr: "endpoint must not be empty",
		},
		{
			name:    "empty bucket is rejected",
			cfg:     MinioConfig{Endpoint: "minio.internal:9000"},
			wantErr: "bucket must not be empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewMinioStore(tt.cfg)
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestNewMinioStoreAcceptsValidConfig(t *testing.T) {
	// Construction never dials the network — it only builds a client. A
	// well-formed config must succeed regardless of whether anything is
	// actually listening at the endpoint.
	store, err := NewMinioStore(MinioConfig{
		Endpoint:  "127.0.0.1:9000",
		Bucket:    "cara-backups",
		AccessKey: "access",
		SecretKey: "secret",
	})
	require.NoError(t, err)
	assert.NotNil(t, store)
}

func TestMinioPutOptionsSendsTheCondition(t *testing.T) {
	tests := []struct {
		name            string
		opts            PutOptions
		wantIfMatch     string
		wantIfNoneMatch string
	}{
		{name: "unconditional", opts: PutOptions{Size: 3}},
		{name: "if-match quotes the ETag", opts: PutOptions{Size: 3, IfMatch: "abc123"}, wantIfMatch: `"abc123"`},
		{name: "if-absent", opts: PutOptions{Size: 3, IfAbsent: true}, wantIfNoneMatch: "*"},
		{name: "an empty object can be conditional", opts: PutOptions{Size: 0, IfAbsent: true}, wantIfNoneMatch: "*"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := minioPutOptions(tt.opts)
			require.NoError(t, err)
			h := got.Header()
			assert.Equal(t, tt.wantIfMatch, h.Get("If-Match"))
			assert.Equal(t, tt.wantIfNoneMatch, h.Get("If-None-Match"))
		})
	}
}

func TestMinioPutOptionsRefusesWhatItCannotHonour(t *testing.T) {
	tests := []struct {
		name string
		opts PutOptions
	}{
		{name: "both conditions", opts: PutOptions{Size: 3, IfMatch: "abc", IfAbsent: true}},
		{name: "if-match with unknown size", opts: PutOptions{Size: -1, IfMatch: "abc"}},
		{name: "if-absent with unknown size", opts: PutOptions{Size: -1, IfAbsent: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := minioPutOptions(tt.opts)
			assert.ErrorIs(t, err, ErrInvalidOptions)
		})
	}
	_, err := minioPutOptions(PutOptions{Size: -1})
	assert.NoError(t, err, "an unconditional upload of unknown size streams as before")
}

func TestIsPreconditionFailed(t *testing.T) {
	tests := []struct {
		name string
		err  minio.ErrorResponse
		want bool
	}{
		{name: "412 status", err: minio.ErrorResponse{StatusCode: 412}, want: true},
		{name: "PreconditionFailed code", err: minio.ErrorResponse{Code: "PreconditionFailed"}, want: true},
		{name: "409 ConditionalRequestConflict is retryable, not a lost race", err: minio.ErrorResponse{Code: "ConditionalRequestConflict", StatusCode: 409}, want: false},
		{name: "404 is not a precondition failure", err: minio.ErrorResponse{Code: "NoSuchKey", StatusCode: 404}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isPreconditionFailed(tt.err))
		})
	}
	assert.False(t, isPreconditionFailed(errors.New("connection refused")))
}
