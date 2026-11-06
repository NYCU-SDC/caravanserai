package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// token list printed its table whatever -o said, so a script asking for JSON
// got text it could not parse.
func TestTokenListHonoursOutput(t *testing.T) {
	tokens := []v1.APIToken{{ID: 1, Kind: v1.APITokenKindUser, Name: "ops-agent", CreatedBy: "user:break-glass", CreatedAt: time.Now()}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(tokens)
	}))
	t.Cleanup(srv.Close)

	run := func(format string) string {
		root := &cobra.Command{Use: "caractl"}
		root.PersistentFlags().String("server", srv.URL, "")
		root.PersistentFlags().StringP("output", "o", "table", "")
		root.AddCommand(NewTokenCmd())
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs([]string{"token", "list", "-o", format})
		require.NoError(t, root.Execute())
		return out.String()
	}

	var got []v1.APIToken
	require.NoError(t, json.Unmarshal([]byte(run("json")), &got))
	assert.Equal(t, "ops-agent", got[0].Name)
	assert.Contains(t, run("yaml"), "name: ops-agent")
	assert.Contains(t, run("table"), "user:ops-agent")
}
