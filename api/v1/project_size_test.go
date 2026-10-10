package v1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestProjectSizeIsValid(t *testing.T) {
	tests := []struct {
		size ProjectSize
		want bool
	}{
		{size: ProjectSizeSmall, want: true},
		{size: ProjectSizeMedium, want: true},
		{size: ProjectSizeLarge, want: true},
		{size: "", want: false},
		{size: "medium", want: false},
		{size: "XL", want: false},
	}

	for _, tt := range tests {
		t.Run(string(tt.size), func(t *testing.T) {
			assert.Equal(t, tt.want, tt.size.IsValid())
		})
	}
}

func TestProjectSizeRequests(t *testing.T) {
	tests := []struct {
		name string
		size ProjectSize
		want ResourceRequests
	}{
		{name: "Small", size: ProjectSizeSmall, want: ResourceRequests{CPUMilli: 500, MemoryBytes: 512 << 20}},
		{name: "Medium", size: ProjectSizeMedium, want: ResourceRequests{CPUMilli: 1000, MemoryBytes: 2 << 30}},
		{name: "Large", size: ProjectSizeLarge, want: ResourceRequests{CPUMilli: 2000, MemoryBytes: 4 << 30}},
		{name: "empty books the default", size: "", want: ResourceRequests{CPUMilli: 1000, MemoryBytes: 2 << 30}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.size.Requests())
		})
	}
}

func TestProjectSpecEffectiveSize(t *testing.T) {
	assert.Equal(t, ProjectSizeMedium, ProjectSpec{}.EffectiveSize(), "omitted size defaults to Medium")
	assert.Equal(t, ProjectSizeLarge, ProjectSpec{Size: ProjectSizeLarge}.EffectiveSize())
}

func TestProjectSpecSize_RoundTrip(t *testing.T) {
	manifest := []byte(`
apiVersion: ` + APIVersion + `
kind: Project
metadata:
  name: demo
spec:
  size: Medium
  services:
    - name: web
      image: nginx
`)

	var p Project
	require.NoError(t, yaml.Unmarshal(manifest, &p))
	assert.Equal(t, ProjectSizeMedium, p.Spec.Size)

	raw, err := json.Marshal(p)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"size":"Medium"`)

	var decoded Project
	require.NoError(t, json.Unmarshal(raw, &decoded))
	assert.Equal(t, ProjectSizeMedium, decoded.Spec.Size)
}

// An omitted size stays omitted, so stored specs and `caractl get -o yaml`
// output for Projects that never declared one are unchanged.
func TestProjectSpecSize_OmittedWhenEmpty(t *testing.T) {
	raw, err := json.Marshal(ProjectSpec{})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "size")

	out, err := yaml.Marshal(ProjectSpec{})
	require.NoError(t, err)
	assert.NotContains(t, string(out), "size")
}
