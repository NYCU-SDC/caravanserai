package project

import (
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
)

// specWithVolumes builds a minimal valid spec with the given volumes and mounts
// so volume rules can be tested in isolation.
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
			name: "managed volume is valid",
			spec: specWithVolumes(
				[]v1.VolumeDef{{Name: "db-data", Type: v1.VolumeTypeManaged}},
				[]v1.VolumeMount{{Name: "db-data", MountPath: "/var/lib/postgresql/data"}},
			),
		},
		{
			name: "ephemeral volume is valid",
			spec: specWithVolumes(
				[]v1.VolumeDef{{Name: "cache", Type: v1.VolumeTypeEphemeral}},
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

// specWithBackup builds a spec whose volumes are as given plus a Project-level
// backup policy.
func specWithBackup(volumes []v1.VolumeDef, backup *v1.ProjectBackupConfig) v1.ProjectSpec {
	spec := specWithVolumes(volumes, nil)
	spec.Backup = backup
	return spec
}

func TestValidateProjectSpecBackup(t *testing.T) {
	managed := []v1.VolumeDef{{Name: "db-data", Type: v1.VolumeTypeManaged}}
	ephemeral := []v1.VolumeDef{{Name: "cache", Type: v1.VolumeTypeEphemeral}}

	tests := []struct {
		name    string
		spec    v1.ProjectSpec
		wantErr string // empty means valid
	}{
		{
			name: "backup with managed volume is valid",
			spec: specWithBackup(managed, &v1.ProjectBackupConfig{
				Interval:  "168h",
				OnMissing: v1.VolumeOnMissingInitializeEmpty,
			}),
		},
		{
			name: "onMissing may be omitted",
			spec: specWithBackup(managed, &v1.ProjectBackupConfig{Interval: "1h"}),
		},
		{
			name: "managed volume without a backup policy is valid",
			spec: specWithBackup(managed, nil),
		},
		{
			name:    "backup without any managed volume is rejected",
			spec:    specWithBackup(ephemeral, &v1.ProjectBackupConfig{Interval: "168h"}),
			wantErr: "requires at least one volume with type \"Managed\"",
		},
		{
			name:    "unparsable interval is rejected",
			spec:    specWithBackup(managed, &v1.ProjectBackupConfig{Interval: "weekly"}),
			wantErr: "positive duration",
		},
		{
			name:    "zero interval is rejected",
			spec:    specWithBackup(managed, &v1.ProjectBackupConfig{Interval: "0s"}),
			wantErr: "positive duration",
		},
		{
			name:    "negative interval is rejected",
			spec:    specWithBackup(managed, &v1.ProjectBackupConfig{Interval: "-1h"}),
			wantErr: "positive duration",
		},
		{
			name: "misspelled onMissing is rejected",
			spec: specWithBackup(managed, &v1.ProjectBackupConfig{
				Interval:  "168h",
				OnMissing: "InitalizeEmpty",
			}),
			wantErr: "onMissing must be",
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
