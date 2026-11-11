package v1

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestWithDefaultBackup(t *testing.T) {
	managed := []VolumeDef{{Name: "data", Type: VolumeTypeManaged}}
	ephemeral := []VolumeDef{{Name: "cache", Type: VolumeTypeEphemeral}}

	tests := []struct {
		name     string
		spec     ProjectSpec
		interval time.Duration
		want     *ProjectBackupConfig
	}{
		{
			name:     "a Managed volume without a backup section gets the default",
			spec:     ProjectSpec{Volumes: managed},
			interval: 24 * time.Hour,
			want:     &ProjectBackupConfig{Interval: "24h"},
		},
		{
			name:     "the interval is written the way people write it",
			spec:     ProjectSpec{Volumes: managed},
			interval: 90 * time.Minute,
			want:     &ProjectBackupConfig{Interval: "1h30m"},
		},
		{
			name:     "a Project's own interval wins",
			spec:     ProjectSpec{Volumes: managed, Backup: &ProjectBackupConfig{Interval: "168h"}},
			interval: 24 * time.Hour,
			want:     &ProjectBackupConfig{Interval: "168h"},
		},
		{
			name:     "a disabled backup stays disabled",
			spec:     ProjectSpec{Volumes: managed, Backup: &ProjectBackupConfig{Disabled: true}},
			interval: 24 * time.Hour,
			want:     &ProjectBackupConfig{Disabled: true},
		},
		{
			name:     "nothing to back up gets no backup section",
			spec:     ProjectSpec{Volumes: ephemeral},
			interval: 24 * time.Hour,
			want:     nil,
		},
		{
			name:     "no volumes at all gets no backup section",
			spec:     ProjectSpec{},
			interval: 24 * time.Hour,
			want:     nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.spec.WithDefaultBackup(tt.interval).Backup)
		})
	}
}

func TestWithDefaultBackupLeavesTheCallersSpecAlone(t *testing.T) {
	spec := ProjectSpec{Volumes: []VolumeDef{{Name: "data", Type: VolumeTypeManaged}}}
	_ = spec.WithDefaultBackup(24 * time.Hour)
	assert.Nil(t, spec.Backup)
}

func TestBackupDisabled(t *testing.T) {
	assert.False(t, ProjectSpec{}.BackupDisabled())
	assert.False(t, ProjectSpec{Backup: &ProjectBackupConfig{Interval: "24h"}}.BackupDisabled())
	assert.True(t, ProjectSpec{Backup: &ProjectBackupConfig{Disabled: true}}.BackupDisabled())
}
