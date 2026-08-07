package objectstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseEndpoint(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantHost   string
		wantSecure bool
		wantErr    string // empty means valid
	}{
		{
			name:       "https enables TLS",
			raw:        "https://minio.internal:9000",
			wantHost:   "minio.internal:9000",
			wantSecure: true,
		},
		{
			name:       "http disables TLS",
			raw:        "http://minio.internal:9000",
			wantHost:   "minio.internal:9000",
			wantSecure: false,
		},
		{
			name:    "missing scheme is rejected",
			raw:     "minio.internal:9000",
			wantErr: "must start with http",
		},
		{
			name:    "unsupported scheme is rejected",
			raw:     "s3://minio.internal:9000",
			wantErr: "must start with http",
		},
		{
			name:    "empty host is rejected",
			raw:     "http://",
			wantErr: "no host",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, secure, err := ParseEndpoint(tt.raw)
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.wantHost, host)
			assert.Equal(t, tt.wantSecure, secure)
		})
	}
}
