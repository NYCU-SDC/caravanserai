package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateNodeLabels(t *testing.T) {
	tests := []struct {
		name    string
		labels  map[string]string
		wantErr bool
	}{
		{name: "nil labels are valid", labels: nil},
		{name: "no tier label is valid", labels: map[string]string{"team": "sdc"}},
		{name: "primary is valid", labels: map[string]string{LabelNodeTier: "primary"}},
		{name: "backup is valid", labels: map[string]string{LabelNodeTier: "backup"}},
		{name: "other labels stay free-form", labels: map[string]string{LabelNodeTier: "backup", "zone": "Anything Goes"}},
		{name: "capitalised tier is rejected", labels: map[string]string{LabelNodeTier: "Primary"}, wantErr: true},
		{name: "unknown tier is rejected", labels: map[string]string{LabelNodeTier: "gold"}, wantErr: true},
		{name: "empty tier is rejected", labels: map[string]string{LabelNodeTier: ""}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateNodeLabels(tt.labels)
			if tt.wantErr {
				assert.ErrorContains(t, err, LabelNodeTier)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestNodeEffectiveTier(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   NodeTier
	}{
		{name: "unlabeled resolves to the default", labels: nil, want: DefaultNodeTier},
		{name: "primary", labels: map[string]string{LabelNodeTier: "primary"}, want: NodeTierPrimary},
		{name: "backup", labels: map[string]string{LabelNodeTier: "backup"}, want: NodeTierBackup},
		{name: "invalid resolves to the default", labels: map[string]string{LabelNodeTier: "gold"}, want: DefaultNodeTier},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := Node{ObjectMeta: ObjectMeta{Labels: tt.labels}}
			assert.Equal(t, tt.want, node.EffectiveTier())
		})
	}
	assert.Equal(t, NodeTierBackup, DefaultNodeTier, "unlabeled nodes default to backup")
}
