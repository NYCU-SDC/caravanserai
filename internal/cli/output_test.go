package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// decodeAny unmarshals data as JSON or YAML into v depending on format.
func decodeAny(format string, data []byte, v any) error {
	switch format {
	case "json":
		return json.Unmarshal(data, v)
	case "yaml":
		return yaml.Unmarshal(data, v)
	default:
		return fmt.Errorf("decodeAny: unsupported format %q", format)
	}
}

// secretWithSecrets returns a Secret whose values are distinctive strings
// that must never appear in any CLI output.
func secretWithSecrets() v1.Secret {
	return v1.Secret{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Secret"},
		ObjectMeta: v1.ObjectMeta{Name: "db-creds", Namespace: "default"},
		Spec: v1.SecretSpec{
			Data: []v1.SecretDataItem{
				{Key: "db-password", Value: "hunter2-do-not-leak"},
				{Key: "api-key", Value: "sk-do-not-leak-either"},
			},
		},
	}
}

// forbiddenValues are the real Secret values that must never appear in output.
func forbiddenValues() []string {
	s := secretWithSecrets()
	vals := make([]string, len(s.Spec.Data))
	for i, item := range s.Spec.Data {
		vals[i] = item.Value
	}
	return vals
}

func assertNoLeak(t *testing.T, output string) {
	t.Helper()
	for _, v := range forbiddenValues() {
		assert.NotContains(t, output, v, "output must never contain a real Secret value")
	}
	// Key names should still be present, otherwise the assertion above would
	// be trivially true on empty output. Table output never prints values at
	// all (not even redacted), so this is the only invariant that holds
	// across every format.
	assert.Contains(t, output, "db-password")
	assert.Contains(t, output, "api-key")
}

func TestRedactSecret(t *testing.T) {
	original := secretWithSecrets()
	redacted := redactSecret(original)

	require.Len(t, redacted.Spec.Data, len(original.Spec.Data))
	for i, item := range redacted.Spec.Data {
		assert.Equal(t, original.Spec.Data[i].Key, item.Key, "keys must be preserved")
		assert.Equal(t, redactedValue, item.Value, "every value must be replaced")
	}

	// redactSecret must not mutate the caller's copy.
	assert.Equal(t, "hunter2-do-not-leak", original.Spec.Data[0].Value,
		"redactSecret must not mutate its argument")
}

func TestPrintSecret_NeverLeaksValue(t *testing.T) {
	for _, format := range []string{"table", "json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			p := &Printer{Format: format, Out: &buf}

			require.NoError(t, p.PrintSecret(secretWithSecrets()))
			assertNoLeak(t, buf.String())
		})
	}
}

// TestPrintSecret_StructuredFormatsAreRedacted decodes the json/yaml output
// (rather than substring-matching, which is unreliable once encoding/json's
// HTML escaping turns "<redacted>" into "<redacted>") and checks
// every value was actually replaced with redactedValue.
func TestPrintSecret_StructuredFormatsAreRedacted(t *testing.T) {
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			p := &Printer{Format: format, Out: &buf}
			require.NoError(t, p.PrintSecret(secretWithSecrets()))

			var decoded v1.Secret
			require.NoError(t, decodeAny(format, buf.Bytes(), &decoded))
			require.Len(t, decoded.Spec.Data, 2)
			for _, item := range decoded.Spec.Data {
				assert.Equal(t, redactedValue, item.Value)
			}
		})
	}
}

func TestPrintSecretList_NeverLeaksValue(t *testing.T) {
	list := v1.SecretList{Items: []v1.Secret{secretWithSecrets()}}

	for _, format := range []string{"table", "json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			p := &Printer{Format: format, Out: &buf}

			require.NoError(t, p.PrintSecretList(list))
			assertNoLeak(t, buf.String())
		})
	}
}

func TestPrintSecretList_StructuredFormatsAreRedacted(t *testing.T) {
	list := v1.SecretList{Items: []v1.Secret{secretWithSecrets()}}

	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			p := &Printer{Format: format, Out: &buf}
			require.NoError(t, p.PrintSecretList(list))

			var decoded v1.SecretList
			require.NoError(t, decodeAny(format, buf.Bytes(), &decoded))
			require.Len(t, decoded.Items, 1)
			require.Len(t, decoded.Items[0].Spec.Data, 2)
			for _, item := range decoded.Items[0].Spec.Data {
				assert.Equal(t, redactedValue, item.Value)
			}
		})
	}
}

func TestPrintAny_Secret_NeverLeaksValue(t *testing.T) {
	// PrintAny is what cmd_apply.go uses; it must route Secret through the
	// same redaction path as PrintSecret regardless of format.
	for _, format := range []string{"table", "json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			p := &Printer{Format: format, Out: &buf}

			require.NoError(t, p.PrintAny(secretWithSecrets()))
			assertNoLeak(t, buf.String())
		})
	}
}

func TestDescribeSecret_NeverLeaksValue(t *testing.T) {
	secret := secretWithSecrets()
	var buf bytes.Buffer

	describeSecret(&buf, &secret)
	assertNoLeak(t, buf.String())
}

func TestDescribeSecret_NoData(t *testing.T) {
	secret := v1.Secret{ObjectMeta: v1.ObjectMeta{Name: "empty-secret"}}
	var buf bytes.Buffer

	describeSecret(&buf, &secret)
	assert.True(t, strings.Contains(buf.String(), "<none>"),
		"describeSecret should show <none> when Spec.Data is empty")
}
