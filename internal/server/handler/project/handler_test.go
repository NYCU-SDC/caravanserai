package project

import (
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
)

// specWithVolumes builds a minimal valid spec with the given volumes and one
// service mounting nothing, so volume-specific rules can be tested in isolation.
func specWithVolumes(volumes []v1.VolumeDef, mounts []v1.VolumeMount) v1.ProjectSpec {
	return v1.ProjectSpec{
		Services: []v1.ServiceDef{
			{Name: "app", Image: "busybox:1.36", VolumeMounts: mounts},
		},
		Volumes: volumes,
	}
}

func TestValidateProjectSpecVolumes(t *testing.T) {
	tests := []struct {
		name    string
		spec    v1.ProjectSpec
		wantErr string // empty means valid
	}{
		{
			name: "managed volume with backup is valid",
			spec: specWithVolumes(
				[]v1.VolumeDef{{
					Name: "db-data",
					Type: v1.VolumeTypeManaged,
					Backup: &v1.VolumeBackupConfig{
						Interval:  "1h",
						OnMissing: v1.VolumeOnMissingInitializeEmpty,
					},
				}},
				[]v1.VolumeMount{{Name: "db-data", MountPath: "/var/lib/postgresql/data"}},
			),
		},
		{
			name: "managed volume without backup is valid",
			spec: specWithVolumes(
				[]v1.VolumeDef{{Name: "db-data", Type: v1.VolumeTypeManaged}},
				nil,
			),
		},
		{
			name: "backup onMissing defaults to empty string and is valid",
			spec: specWithVolumes(
				[]v1.VolumeDef{{
					Name:   "db-data",
					Type:   v1.VolumeTypeManaged,
					Backup: &v1.VolumeBackupConfig{Interval: "30m"},
				}},
				nil,
			),
		},
		{
			name: "empty volume name is rejected",
			spec: specWithVolumes(
				[]v1.VolumeDef{{Name: "", Type: v1.VolumeTypeEphemeral}},
				nil,
			),
			wantErr: "non-empty name",
		},
		{
			name: "duplicate volume name is rejected",
			spec: specWithVolumes(
				[]v1.VolumeDef{
					{Name: "data", Type: v1.VolumeTypeEphemeral},
					{Name: "data", Type: v1.VolumeTypeManaged},
				},
				nil,
			),
			wantErr: "duplicate volume name",
		},
		{
			name: "unknown volume type is rejected",
			spec: specWithVolumes(
				[]v1.VolumeDef{{Name: "data", Type: "HostPath"}},
				nil,
			),
			wantErr: "type must be",
		},
		{
			name: "backup on ephemeral volume is rejected",
			spec: specWithVolumes(
				[]v1.VolumeDef{{
					Name:   "cache",
					Type:   v1.VolumeTypeEphemeral,
					Backup: &v1.VolumeBackupConfig{Interval: "1h"},
				}},
				nil,
			),
			wantErr: "only valid for Managed",
		},
		{
			name: "unparsable backup interval is rejected",
			spec: specWithVolumes(
				[]v1.VolumeDef{{
					Name:   "db-data",
					Type:   v1.VolumeTypeManaged,
					Backup: &v1.VolumeBackupConfig{Interval: "yearly"},
				}},
				nil,
			),
			wantErr: "positive duration",
		},
		{
			name: "zero backup interval is rejected",
			spec: specWithVolumes(
				[]v1.VolumeDef{{
					Name:   "db-data",
					Type:   v1.VolumeTypeManaged,
					Backup: &v1.VolumeBackupConfig{Interval: "0s"},
				}},
				nil,
			),
			wantErr: "positive duration",
		},
		{
			name: "negative backup interval is rejected",
			spec: specWithVolumes(
				[]v1.VolumeDef{{
					Name:   "db-data",
					Type:   v1.VolumeTypeManaged,
					Backup: &v1.VolumeBackupConfig{Interval: "-1h"},
				}},
				nil,
			),
			wantErr: "positive duration",
		},
		{
			name: "unknown onMissing is rejected",
			spec: specWithVolumes(
				[]v1.VolumeDef{{
					Name:   "db-data",
					Type:   v1.VolumeTypeManaged,
					Backup: &v1.VolumeBackupConfig{Interval: "1h", OnMissing: "InitalizeEmpty"},
				}},
				nil,
			),
			wantErr: "onMissing must be",
		},
		{
			name: "volumeMount referencing undeclared volume is rejected",
			spec: specWithVolumes(
				[]v1.VolumeDef{{Name: "db-data", Type: v1.VolumeTypeManaged}},
				[]v1.VolumeMount{{Name: "db-dat", MountPath: "/data"}},
			),
			wantErr: "undeclared volume",
		},
		{
			name: "volumeMount with empty mountPath is rejected",
			spec: specWithVolumes(
				[]v1.VolumeDef{{Name: "db-data", Type: v1.VolumeTypeManaged}},
				[]v1.VolumeMount{{Name: "db-data", MountPath: ""}},
			),
			wantErr: "non-empty mountPath",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateProjectSpec(tt.spec)
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.ErrorContains(t, err, tt.wantErr)
			}
		})
	}
}
