package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCheckAssignedSpecChange(t *testing.T) {
	base := ProjectSpec{
		Size:     ProjectSizeSmall,
		Services: []ServiceDef{{Name: "web", Image: "nginx:1.27"}},
		Volumes: []VolumeDef{
			{Name: "data", Type: VolumeTypeManaged},
			{Name: "cache", Type: VolumeTypeEphemeral},
		},
		Backup: &ProjectBackupConfig{Interval: "1h"},
	}
	edit := func(f func(*ProjectSpec)) ProjectSpec {
		s := base
		s.Services = append([]ServiceDef(nil), base.Services...)
		s.Volumes = append([]VolumeDef(nil), base.Volumes...)
		b := *base.Backup
		s.Backup = &b
		f(&s)
		return s
	}

	for _, tc := range []struct {
		name      string
		next      ProjectSpec
		wantField string
	}{
		{"same spec", base, ""},
		{"new image", edit(func(s *ProjectSpec) { s.Services[0].Image = "nginx:1.28" }), ""},
		{"added service", edit(func(s *ProjectSpec) { s.Services = append(s.Services, ServiceDef{Name: "api", Image: "api:1"}) }), ""},
		{"added volume", edit(func(s *ProjectSpec) { s.Volumes = append(s.Volumes, VolumeDef{Name: "conf", Type: VolumeTypeManaged}) }), ""},
		{"volumes reordered", edit(func(s *ProjectSpec) { s.Volumes[0], s.Volumes[1] = s.Volumes[1], s.Volumes[0] }), ""},
		{"backup interval", edit(func(s *ProjectSpec) { s.Backup.Interval = "6h" }), ""},
		{"size", edit(func(s *ProjectSpec) { s.Size = ProjectSizeLarge }), "spec.size"},
		{"removed volume", edit(func(s *ProjectSpec) { s.Volumes = s.Volumes[:1] }), "spec.volumes"},
		{"volume type", edit(func(s *ProjectSpec) { s.Volumes[1].Type = VolumeTypeManaged }), "spec.volumes"},
		{"backup items", edit(func(s *ProjectSpec) {
			s.Backup.Items = []BackupItem{{Volume: "data", Strategy: BackupStrategyOffline}}
		}), "spec.backup.items"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckAssignedSpecChange(base, tc.next)
			if tc.wantField == "" {
				assert.Nil(t, got)
				return
			}
			if assert.NotNil(t, got) {
				assert.Equal(t, tc.wantField, got.Field)
			}
		})
	}

	t.Run("writing out the default size is not a change", func(t *testing.T) {
		unset := ProjectSpec{Services: base.Services}
		explicit := ProjectSpec{Size: DefaultProjectSize, Services: base.Services}
		assert.Nil(t, CheckAssignedSpecChange(unset, explicit))
		assert.Nil(t, CheckAssignedSpecChange(explicit, unset))
	})
}
