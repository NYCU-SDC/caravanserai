package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The acceptance example: what explain lists is what schemas/project.json,
// read from the checkout rather than the embedded copy, says ServiceDef has.
func TestExplainMatchesTheSchemaFile(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", "project.json"))
	require.NoError(t, err)
	var doc schemaNode
	require.NoError(t, json.Unmarshal(raw, &doc))
	svc := doc.Defs["ServiceDef"]

	ex, err := explain("project.spec.services")
	require.NoError(t, err)
	assert.Equal(t, "Project", ex.Kind)
	assert.Equal(t, "spec.services", ex.Path)
	assert.Equal(t, "[]ServiceDef", ex.Type)

	var names []string
	for _, f := range ex.Fields {
		names = append(names, f.Name)
		assert.Equal(t, svc.Properties[f.Name].Description, f.Description, f.Name)
		assert.Equal(t, contains(svc.Required, f.Name), f.Required, f.Name)
	}
	assert.Equal(t, sortedKeys(svc.Properties), names)
}

func TestExplainTypes(t *testing.T) {
	for path, want := range map[string]string{
		"project.spec.services.env":    "[]EnvVar",
		"project.status.phase":         "string (ProjectPhase)",
		"project.metadata.labels":      "map[string]string",
		"node.status.capacity":         "map[string]string (ResourceList)",
		"node.status.lastHeartbeat":    "string (date-time)",
		"secret.spec.data":             "[]SecretDataItem",
		"project.spec.services.image":  "string",
		"PROJECTS.spec.services.image": "string",
	} {
		ex, err := explain(path)
		require.NoError(t, err, path)
		assert.Equal(t, want, ex.Type, path)
	}
	ex, err := explain("project.status.phase")
	require.NoError(t, err)
	assert.Contains(t, ex.Enum, "Running")
}

func TestExplainWrongPathListsTheValidChildren(t *testing.T) {
	_, err := explain("project.spec.servics")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Project.spec")
	assert.Contains(t, err.Error(), "services")

	_, err = explain("project.spec.services.image.x")
	assert.ErrorContains(t, err, "has no child fields")

	_, err = explain("vm")
	assert.ErrorContains(t, err, "valid kinds are node, project, secret")
}

// Every field of every kind can be explained: no $ref explain fails to follow.
func TestEveryFieldExplains(t *testing.T) {
	count := 0
	var walk func(path string, depth int)
	walk = func(path string, depth int) {
		ex, err := explain(path)
		require.NoError(t, err, path)
		count++
		assert.NotContains(t, ex.Type, "any", path)
		if depth > 8 {
			return
		}
		for _, f := range ex.Fields {
			walk(path+"."+f.Name, depth+1)
		}
	}
	for _, kind := range []string{"node", "project", "secret"} {
		walk(kind, 0)
	}
	assert.Greater(t, count, 100)
}

func TestExplainTextOutput(t *testing.T) {
	ex, err := explain("project.spec.services")
	require.NoError(t, err)
	var out strings.Builder
	ex.write(&out)
	assert.Contains(t, out.String(), "FIELD: spec.services <[]ServiceDef>")
	assert.Contains(t, out.String(), "  image <string> (required)")
}

func contains(list []string, s string) bool {
	return slices.Contains(list, s)
}
