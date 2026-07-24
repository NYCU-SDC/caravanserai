package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeConfigFile writes content to a temp config.yaml with the given mode and
// returns its path. os.WriteFile is subject to umask, so the mode is applied
// explicitly afterwards.
func writeConfigFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), mode))
	require.NoError(t, os.Chmod(path, mode))
	return path
}

func TestMergeS3PreservesFieldsAcrossSources(t *testing.T) {
	// configutil.Merge replaces a nested struct wholesale as soon as any part
	// of the override is non-zero. Without field-wise merging, an endpoint
	// supplied by env would wipe the bucket and credentials from file.
	base := S3Config{
		Endpoint:  "http://from-file:9000",
		Bucket:    "cara-backups",
		Region:    "us-east-1",
		AccessKey: "file-access",
		SecretKey: "file-secret",
	}
	override := S3Config{Endpoint: "https://from-env:9000"}

	got := mergeS3(base, override)

	assert.Equal(t, "https://from-env:9000", got.Endpoint, "override should win")
	assert.Equal(t, "cara-backups", got.Bucket)
	assert.Equal(t, "us-east-1", got.Region)
	assert.Equal(t, "file-access", got.AccessKey)
	assert.Equal(t, "file-secret", got.SecretKey)
}

func TestAgentFromFileMergesS3WithoutClobbering(t *testing.T) {
	path := writeConfigFile(t, `
s3:
  bucket: from-file
  secret_key: file-secret
`, 0o600)

	base := &AgentConfig{S3: S3Config{Endpoint: "http://preset:9000", AccessKey: "preset-access"}}
	got, err := AgentFromFile(path, base, NewConfigLogger())
	require.NoError(t, err)

	assert.Equal(t, "http://preset:9000", got.S3.Endpoint, "field absent from file must survive")
	assert.Equal(t, "preset-access", got.S3.AccessKey)
	assert.Equal(t, "from-file", got.S3.Bucket)
	assert.Equal(t, "file-secret", got.S3.SecretKey)
}

func TestAgentFromFileFlagsInsecureSecretFile(t *testing.T) {
	tests := []struct {
		name         string
		content      string
		mode         os.FileMode
		wantInsecure bool
	}{
		{
			name:         "secret in 0600 file is accepted",
			content:      "s3:\n  secret_key: shh\n",
			mode:         0o600,
			wantInsecure: false,
		},
		{
			name:         "secret in group-readable file is flagged",
			content:      "s3:\n  secret_key: shh\n",
			mode:         0o640,
			wantInsecure: true,
		},
		{
			name:         "secret in world-readable file is flagged",
			content:      "s3:\n  secret_key: shh\n",
			mode:         0o644,
			wantInsecure: true,
		},
		{
			name:         "no secret means permissions are not checked",
			content:      "s3:\n  bucket: cara-backups\n",
			mode:         0o644,
			wantInsecure: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfigFile(t, tt.content, tt.mode)

			got, err := AgentFromFile(path, &AgentConfig{}, NewConfigLogger())
			require.NoError(t, err)

			if tt.wantInsecure {
				assert.Equal(t, path, got.InsecureSecretFile)
			} else {
				assert.Empty(t, got.InsecureSecretFile)
			}
		})
	}
}

func TestAgentConfigValidateRejectsInsecureSecretFile(t *testing.T) {
	cfg := &AgentConfig{
		ServerURL:          "http://localhost:8080",
		AdvertiseIP:        "10.0.0.1",
		InsecureSecretFile: "/etc/cara/config.yaml",
	}

	err := cfg.Validate()

	assert.ErrorContains(t, err, "readable beyond its owner")
	assert.ErrorContains(t, err, "chmod 0600")
}

func TestS3ConfigValidate(t *testing.T) {
	complete := S3Config{
		Endpoint:  "http://minio:9000",
		Bucket:    "cara-backups",
		AccessKey: "access",
		SecretKey: "secret",
	}

	tests := []struct {
		name    string
		mutate  func(*S3Config)
		wantErr string // empty means valid
	}{
		{
			name:   "complete config is valid",
			mutate: func(*S3Config) {},
		},
		{
			name:   "empty endpoint disables backups and is valid",
			mutate: func(s *S3Config) { *s = S3Config{} },
		},
		{
			name:    "endpoint without scheme is rejected",
			mutate:  func(s *S3Config) { s.Endpoint = "minio:9000" },
			wantErr: "must start with http",
		},
		{
			name:    "non-http scheme is rejected",
			mutate:  func(s *S3Config) { s.Endpoint = "s3://minio:9000" },
			wantErr: "must start with http",
		},
		{
			name:    "missing bucket is rejected",
			mutate:  func(s *S3Config) { s.Bucket = "" },
			wantErr: "s3.bucket is required",
		},
		{
			name:    "missing access key is rejected",
			mutate:  func(s *S3Config) { s.AccessKey = "" },
			wantErr: "s3.access_key is required",
		},
		{
			name:    "missing secret key is rejected",
			mutate:  func(s *S3Config) { s.SecretKey = "" },
			wantErr: "s3.secret_key is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s3 := complete
			tt.mutate(&s3)

			err := s3.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.ErrorContains(t, err, tt.wantErr)
			}
		})
	}
}
