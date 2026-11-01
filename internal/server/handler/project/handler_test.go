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

func TestValidateProjectSpecSize(t *testing.T) {
	tests := []struct {
		name    string
		size    v1.ProjectSize
		wantErr string // empty means valid
	}{
		{name: "omitted size is valid", size: ""},
		{name: "Small is valid", size: v1.ProjectSizeSmall},
		{name: "Medium is valid", size: v1.ProjectSizeMedium},
		{name: "Large is valid", size: v1.ProjectSizeLarge},
		{name: "lowercase is rejected", size: "medium", wantErr: `spec.size must be one of "Small", "Medium", "Large"`},
		{name: "unknown size is rejected", size: "XL", wantErr: `spec.size must be one of "Small", "Medium", "Large"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := specWithVolumes(nil, nil)
			spec.Size = tt.size

			err := validateProjectSpec(spec)
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.ErrorContains(t, err, tt.wantErr)
			}
		})
	}
}

// postgresSpec is a valid PostgresDifferential Project: one Managed volume,
// mounted as PGDATA by one service on the supported image.
func postgresSpec(item v1.BackupItem) v1.ProjectSpec {
	return v1.ProjectSpec{
		Services: []v1.ServiceDef{{
			Name:         "db",
			Image:        v1.PostgresDifferentialImage,
			VolumeMounts: []v1.VolumeMount{{Name: "pgdata", MountPath: v1.PostgresDifferentialDataPath}},
		}},
		Volumes: []v1.VolumeDef{{Name: "pgdata", Type: v1.VolumeTypeManaged}},
		Backup:  &v1.ProjectBackupConfig{Interval: "24h", Items: []v1.BackupItem{item}},
	}
}

func TestValidateProjectSpecBackupItems(t *testing.T) {
	pg := v1.BackupItem{Volume: "pgdata", Strategy: v1.BackupStrategyPostgresDifferential}
	retain := func(n int) *int { return &n }
	with := func(mutate func(*v1.ProjectSpec)) v1.ProjectSpec {
		spec := postgresSpec(pg)
		mutate(&spec)
		return spec
	}

	tests := []struct {
		name    string
		spec    v1.ProjectSpec
		wantErr string // empty means valid
	}{
		{name: "PostgresDifferential on the supported image and path", spec: postgresSpec(pg)},
		{
			name: "retainFull at the lower bound",
			spec: with(func(s *v1.ProjectSpec) {
				s.Backup.Items[0].RetainFull = retain(v1.MinRetainFull)
			}),
		},
		{
			name: "retainFull at the upper bound",
			spec: with(func(s *v1.ProjectSpec) {
				s.Backup.Items[0].RetainFull = retain(v1.MaxRetainFull)
			}),
		},
		{
			name: "explicit Offline item",
			spec: specWithBackup([]v1.VolumeDef{{Name: "data", Type: v1.VolumeTypeManaged}},
				&v1.ProjectBackupConfig{Interval: "1h", Items: []v1.BackupItem{{Volume: "data", Strategy: v1.BackupStrategyOffline}}}),
		},
		{
			name:    "retainFull below the lower bound",
			spec:    with(func(s *v1.ProjectSpec) { s.Backup.Items[0].RetainFull = retain(0) }),
			wantErr: "retainFull must be between 1 and 12",
		},
		{
			name:    "retainFull above the upper bound",
			spec:    with(func(s *v1.ProjectSpec) { s.Backup.Items[0].RetainFull = retain(13) }),
			wantErr: "retainFull must be between 1 and 12",
		},
		{
			name: "retainFull on an Offline item",
			spec: specWithBackup([]v1.VolumeDef{{Name: "data", Type: v1.VolumeTypeManaged}},
				&v1.ProjectBackupConfig{Interval: "1h", Items: []v1.BackupItem{{Volume: "data", Strategy: v1.BackupStrategyOffline, RetainFull: retain(4)}}}),
			wantErr: "retainFull applies only to strategy",
		},
		{
			name:    "item naming an undeclared volume",
			spec:    with(func(s *v1.ProjectSpec) { s.Backup.Items[0].Volume = "nope" }),
			wantErr: "undeclared volume nope",
		},
		{
			name:    "item without a volume",
			spec:    with(func(s *v1.ProjectSpec) { s.Backup.Items[0].Volume = "" }),
			wantErr: "must name a volume",
		},
		{
			name: "item naming an Ephemeral volume",
			spec: with(func(s *v1.ProjectSpec) {
				s.Volumes = append(s.Volumes, v1.VolumeDef{Name: "cache", Type: v1.VolumeTypeEphemeral})
				s.Backup.Items = append(s.Backup.Items, v1.BackupItem{Volume: "cache", Strategy: v1.BackupStrategyPostgresDifferential})
			}),
			wantErr: "only a Managed volume",
		},
		{
			name:    "duplicate item",
			spec:    with(func(s *v1.ProjectSpec) { s.Backup.Items = append(s.Backup.Items, pg) }),
			wantErr: "duplicate backup item",
		},
		{
			name:    "unknown strategy",
			spec:    with(func(s *v1.ProjectSpec) { s.Backup.Items[0].Strategy = "Snapshot" }),
			wantErr: "strategy must be",
		},
		{
			name:    "another image",
			spec:    with(func(s *v1.ProjectSpec) { s.Services[0].Image = "postgres:16" }),
			wantErr: "its image must be",
		},
		{
			name: "another mount path",
			spec: with(func(s *v1.ProjectSpec) {
				s.Services[0].VolumeMounts[0].MountPath = "/data"
			}),
			wantErr: "it must be mounted at",
		},
		{
			name: "mounted by two services",
			spec: with(func(s *v1.ProjectSpec) {
				s.Services = append(s.Services, v1.ServiceDef{
					Name: "replica", Image: v1.PostgresDifferentialImage,
					VolumeMounts: []v1.VolumeMount{{Name: "pgdata", MountPath: v1.PostgresDifferentialDataPath}},
				})
			}),
			wantErr: "mounted by exactly one service, found 2",
		},
		{
			name:    "not mounted at all",
			spec:    with(func(s *v1.ProjectSpec) { s.Services[0].VolumeMounts = nil }),
			wantErr: "not mounted by any service",
		},
		{
			name: "PGDATA at the fixed path",
			spec: with(func(s *v1.ProjectSpec) {
				s.Services[0].Env = []v1.EnvVar{{Name: "PGDATA", Value: v1.PostgresDifferentialDataPath}}
			}),
		},
		{
			name: "other initdb arguments",
			spec: with(func(s *v1.ProjectSpec) {
				s.Services[0].Env = []v1.EnvVar{{Name: "POSTGRES_INITDB_ARGS", Value: "--data-checksums --encoding=UTF8"}}
			}),
		},
		{
			name:    "PGDATA moved out of the volume",
			spec:    with(func(s *v1.ProjectSpec) { s.Services[0].Env = []v1.EnvVar{{Name: "PGDATA", Value: "/tmp/other-db"}} }),
			wantErr: "PGDATA must be",
		},
		{
			name: "PGDATA from a Secret",
			spec: with(func(s *v1.ProjectSpec) {
				s.Services[0].Env = []v1.EnvVar{{Name: "PGDATA", ValueFrom: &v1.EnvVarSource{SecretKeyRef: &v1.SecretKeySelector{Name: "s", Key: "k"}}}}
			}),
			wantErr: "must be a literal value",
		},
		{
			name: "WAL directory moved",
			spec: with(func(s *v1.ProjectSpec) {
				s.Services[0].Env = []v1.EnvVar{{Name: "POSTGRES_INITDB_WALDIR", Value: "/tmp/wal"}}
			}),
			wantErr: "WAL directory cannot be moved",
		},
		{
			name: "WAL directory moved through initdb arguments",
			spec: with(func(s *v1.ProjectSpec) {
				s.Services[0].Env = []v1.EnvVar{{Name: "POSTGRES_INITDB_ARGS", Value: "--data-checksums --waldir=/tmp/wal"}}
			}),
			wantErr: "POSTGRES_INITDB_ARGS may only contain",
		},
		{
			name: "allowed initdb arguments",
			spec: with(func(s *v1.ProjectSpec) {
				s.Services[0].Env = []v1.EnvVar{{Name: "POSTGRES_INITDB_ARGS", Value: "-k --encoding=UTF8 --locale=C.UTF-8 --auth-host=scram-sha-256"}}
			}),
		},
		{
			name: "WAL directory moved through an abbreviated option",
			spec: with(func(s *v1.ProjectSpec) {
				s.Services[0].Env = []v1.EnvVar{{Name: "POSTGRES_INITDB_ARGS", Value: "--wald=/tmp/other-wal"}}
			}),
			wantErr: "POSTGRES_INITDB_ARGS may only contain",
		},
		{
			name: "WAL directory smuggled through shell expansion in a value",
			spec: with(func(s *v1.ProjectSpec) {
				s.Services[0].Env = []v1.EnvVar{{Name: "POSTGRES_INITDB_ARGS", Value: "--encoding=UTF8${IFS}--waldir=/tmp/wal"}}
			}),
			wantErr: "POSTGRES_INITDB_ARGS may only contain",
		},
		{
			name: "command substitution in a value",
			spec: with(func(s *v1.ProjectSpec) {
				s.Services[0].Env = []v1.EnvVar{{Name: "POSTGRES_INITDB_ARGS", Value: "--locale=$(echo C)"}}
			}),
			wantErr: "POSTGRES_INITDB_ARGS may only contain",
		},
		{
			name: "quoted value",
			spec: with(func(s *v1.ProjectSpec) {
				s.Services[0].Env = []v1.EnvVar{{Name: "POSTGRES_INITDB_ARGS", Value: `--encoding="UTF8 --waldir=/tmp/wal"`}}
			}),
			wantErr: "POSTGRES_INITDB_ARGS may only contain",
		},
		{
			name: "initdb setting with its value as a separate word",
			spec: with(func(s *v1.ProjectSpec) {
				s.Services[0].Env = []v1.EnvVar{{Name: "POSTGRES_INITDB_ARGS", Value: "--encoding UTF8"}}
			}),
			wantErr: "POSTGRES_INITDB_ARGS may only contain",
		},
		{
			name: "unknown initdb option",
			spec: with(func(s *v1.ProjectSpec) {
				s.Services[0].Env = []v1.EnvVar{{Name: "POSTGRES_INITDB_ARGS", Value: "--pgdata=/tmp/db"}}
			}),
			wantErr: "POSTGRES_INITDB_ARGS may only contain",
		},
		{
			name: "WAL directory moved through the short flag",
			spec: with(func(s *v1.ProjectSpec) {
				s.Services[0].Env = []v1.EnvVar{{Name: "POSTGRES_INITDB_ARGS", Value: "-X /tmp/wal"}}
			}),
			wantErr: "POSTGRES_INITDB_ARGS may only contain",
		},
		{
			name: "mixed with an Offline Managed volume",
			spec: with(func(s *v1.ProjectSpec) {
				s.Volumes = append(s.Volumes, v1.VolumeDef{Name: "uploads", Type: v1.VolumeTypeManaged})
			}),
			wantErr: "cannot mix strategies",
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

// An environment that cannot pull the published image runs a copy from another
// registry; the configured reference then replaces the published one rather
// than being accepted alongside it.
func TestValidateProjectSpecConfiguredPostgresImage(t *testing.T) {
	const copied = "docker.io/example/cara-postgres:16"
	item := v1.BackupItem{Volume: "pgdata", Strategy: v1.BackupStrategyPostgresDifferential}

	onCopy := postgresSpec(item)
	onCopy.Services[0].Image = copied
	assert.NoError(t, validateProjectSpecFor(onCopy, copied))
	assert.ErrorContains(t, validateProjectSpecFor(postgresSpec(item), copied), "its image must be")

	h := NewHandler(nil, nil, nil, WithPostgresDifferentialImage(""))
	assert.Equal(t, v1.PostgresDifferentialImage, h.postgresImage, "an empty setting keeps the published image")
	h = NewHandler(nil, nil, nil, WithPostgresDifferentialImage(copied))
	assert.Equal(t, copied, h.postgresImage)
}
