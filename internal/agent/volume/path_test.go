package volume

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostPath(t *testing.T) {
	tests := []struct {
		name     string
		dataRoot string
		ns       string
		project  string
		volume   string
		want     string
		wantErr  string // empty means valid
	}{
		{
			name:     "derives the documented layout",
			dataRoot: "/var/lib/cara",
			ns:       "default",
			project:  "blog",
			volume:   "db-data",
			want:     "/var/lib/cara/volumes/default/blog/db-data/data",
		},
		{
			name:     "trailing slash on data root is normalised",
			dataRoot: "/var/lib/cara/",
			ns:       "default",
			project:  "blog",
			volume:   "db-data",
			want:     "/var/lib/cara/volumes/default/blog/db-data/data",
		},
		{
			name:     "namespace is part of the path even while locked to default",
			dataRoot: "/srv/cara",
			ns:       "team-a",
			project:  "blog",
			volume:   "uploads",
			want:     "/srv/cara/volumes/team-a/blog/uploads/data",
		},
		{
			name:     "parent traversal in project is rejected",
			dataRoot: "/var/lib/cara",
			ns:       "default",
			project:  "..",
			volume:   "db-data",
			wantErr:  "project",
		},
		{
			name:     "path separator in volume is rejected",
			dataRoot: "/var/lib/cara",
			ns:       "default",
			project:  "blog",
			volume:   "../../etc/passwd",
			wantErr:  "volume",
		},
		{
			name:     "path separator in namespace is rejected",
			dataRoot: "/var/lib/cara",
			ns:       "a/b",
			project:  "blog",
			volume:   "db-data",
			wantErr:  "namespace",
		},
		{
			name:     "empty volume name is rejected",
			dataRoot: "/var/lib/cara",
			ns:       "default",
			project:  "blog",
			volume:   "",
			wantErr:  "volume",
		},
		{
			name:     "empty data root is rejected",
			dataRoot: "",
			ns:       "default",
			project:  "blog",
			volume:   "db-data",
			wantErr:  "data root",
		},
		{
			name:     "relative data root is rejected",
			dataRoot: "var/lib/cara",
			ns:       "default",
			project:  "blog",
			volume:   "db-data",
			wantErr:  "absolute path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := HostPath(tt.dataRoot, tt.ns, tt.project, tt.volume)
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestHostPathStaysInsideDataRoot(t *testing.T) {
	// Names that survive DNS validation must still land under the data root.
	got, err := HostPath("/var/lib/cara", "default", "a.b", "c.d")
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/cara/volumes/default/a.b/c.d/data", got)
}

func TestProjectDir(t *testing.T) {
	got, err := ProjectDir("/var/lib/cara", "default", "blog")
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/cara/volumes/default/blog", got)

	_, err = ProjectDir("/var/lib/cara", "default", "..")
	assert.ErrorContains(t, err, "project")
}
