package v1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// CARA-82: the immutable UID must survive JSON and YAML round trips so it is
// preserved across API responses, reconcile payloads, and CLI apply/get.
func TestObjectMetaUID_RoundTrip(t *testing.T) {
	const uid = "8f14e45f-ceea-467d-9c3a-7c2a1b2c3d4e"

	original := Project{
		TypeMeta:   TypeMeta{APIVersion: APIVersion, Kind: "Project"},
		ObjectMeta: ObjectMeta{Name: "nginx-demo", UID: uid, Namespace: "default"},
	}

	t.Run("json", func(t *testing.T) {
		raw, err := json.Marshal(original)
		require.NoError(t, err)
		assert.Contains(t, string(raw), `"uid":"`+uid+`"`)

		var decoded Project
		require.NoError(t, json.Unmarshal(raw, &decoded))
		assert.Equal(t, uid, decoded.ObjectMeta.UID)
	})

	t.Run("yaml", func(t *testing.T) {
		raw, err := yaml.Marshal(original)
		require.NoError(t, err)

		var decoded Project
		require.NoError(t, yaml.Unmarshal(raw, &decoded))
		assert.Equal(t, uid, decoded.ObjectMeta.UID)
	})
}

// An empty UID is omitted from the serialized form (omitempty) so pre-UID
// fixtures and hand-written manifests do not carry an empty field.
func TestObjectMetaUID_OmittedWhenEmpty(t *testing.T) {
	raw, err := json.Marshal(Project{ObjectMeta: ObjectMeta{Name: "foo"}})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "uid")
}
