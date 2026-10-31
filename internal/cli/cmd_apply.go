package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// NewApplyCmd returns the "apply" subcommand.
//
// Usage:
//
//	caractrl apply -f manifest.yaml
//	caractrl apply -f -             # read from stdin
func NewApplyCmd() *cobra.Command {
	var (
		filename string
		wait     bool
		timeout  time.Duration
	)

	applyCmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply a resource manifest from a file or stdin",
		Long: `Apply creates or updates a resource defined in a YAML or JSON manifest.
The kind field in the manifest determines which API endpoint is called.

Supported kinds: Node, Project, Secret

Examples:
  caractrl apply -f node.yaml
  caractrl apply -f project.yaml
  cat project.yaml | caractrl apply -f -
  caractrl apply -f project.yaml --wait --timeout 5m`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			serverURL, _ := cmd.Root().PersistentFlags().GetString("server")
			outputFmt, _ := cmd.Root().PersistentFlags().GetString("output")

			// Read the manifest.
			raw, err := readManifest(filename)
			if err != nil {
				return fmt.Errorf("read manifest: %w", err)
			}

			// Convert YAML → JSON so the server (which speaks JSON) accepts it.
			jsonBytes, err := yamlToJSON(raw)
			if err != nil {
				return fmt.Errorf("parse manifest: %w", err)
			}

			client := NewClient(serverURL)
			printer := &Printer{Format: outputFmt, Out: os.Stdout}

			result, err := client.ApplyResource(cmd.Context(), jsonBytes)
			if err != nil {
				return fmt.Errorf("apply: %w", err)
			}

			// Print a confirmation line in table mode; full object otherwise.
			if outputFmt == "table" {
				printApplyConfirmation(cmd, result)
			} else if err := printer.PrintAny(result.Resource); err != nil {
				return err
			}
			if !wait {
				return nil
			}
			project, ok := result.Resource.(v1.Project)
			if !ok {
				return errors.New("--wait only applies to a Project")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			return waitForProject(ctx, client, project.Name, cmd.ErrOrStderr(), projectWaitInterval)
		},
	}

	applyCmd.Flags().StringVarP(&filename, "filename", "f", "", "manifest file to apply (\"-\" for stdin)")
	applyCmd.Flags().BoolVar(&wait, "wait", false, "wait until the Project is Running")
	applyCmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "how long --wait waits")
	_ = applyCmd.MarkFlagRequired("filename")

	return applyCmd
}

// projectWaitInterval is how often --wait reads the Project.
const projectWaitInterval = 2 * time.Second

// waitForProject polls until the Project is Running, and prints each new phase
// or blocking reason so a wait that will not end explains itself. It returns an
// error if the Project reaches Failed or ctx ends first.
func waitForProject(ctx context.Context, client *Client, name string, progress io.Writer, interval time.Duration) error {
	last := ""
	for {
		project, err := client.GetProject(ctx, name)
		if err != nil {
			return fmt.Errorf("wait for project %q: %w", name, err)
		}
		state := string(project.Status.Phase)
		for _, c := range project.Status.Conditions {
			if c.Type != v1.ConditionTypePhase && c.Status == v1.ConditionTrue && c.Message != "" {
				state += fmt.Sprintf("; %s: %s", c.Type, c.Message)
			}
		}
		if state != last {
			fmt.Fprintf(progress, "project/%s %s\n", name, state)
			last = state
		}
		switch project.Status.Phase {
		case v1.ProjectPhaseRunning:
			return nil
		case v1.ProjectPhaseFailed:
			return fmt.Errorf("project %q is Failed: %s", name, state)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for project %q: %s", name, state)
		case <-time.After(interval):
		}
	}
}

// readManifest reads the manifest bytes from a file path or stdin ("-").
func readManifest(filename string) ([]byte, error) {
	if filename == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(filename)
}

// yamlToJSON converts a YAML (or JSON) document to canonical JSON bytes.
// It accepts pure JSON as-is so users can also pass .json files.
func yamlToJSON(src []byte) ([]byte, error) {
	// Decode YAML into a generic structure.
	var generic any
	if err := yaml.Unmarshal(src, &generic); err != nil {
		return nil, fmt.Errorf("yaml decode: %w", err)
	}
	// yaml.v3 decodes maps as map[string]any; JSON marshal handles that fine.
	out, err := json.Marshal(normaliseYAMLNode(generic))
	if err != nil {
		return nil, fmt.Errorf("json encode: %w", err)
	}
	return out, nil
}

// normaliseYAMLNode recursively converts map[string]any values that yaml.v3
// may emit as map[string]interface{} with interface{} key variants into the
// plain map[string]any expected by encoding/json.
func normaliseYAMLNode(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, sub := range val {
			out[k] = normaliseYAMLNode(sub)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, sub := range val {
			out[i] = normaliseYAMLNode(sub)
		}
		return out
	default:
		return val
	}
}

// printApplyConfirmation prints a short human-readable success line.
func printApplyConfirmation(cmd *cobra.Command, result ApplyResult) {
	verb := "configured"
	if result.Created {
		verb = "created"
	}

	switch res := result.Resource.(type) {
	case v1.Node:
		fmt.Fprintf(cmd.OutOrStdout(), "node/%s %s\n", res.Name, verb)
	case v1.Project:
		fmt.Fprintf(cmd.OutOrStdout(), "project/%s %s\n", res.Name, verb)
	case v1.Secret:
		fmt.Fprintf(cmd.OutOrStdout(), "secret/%s %s\n", res.Name, verb)
	default:
		fmt.Fprintln(cmd.OutOrStdout(), "resource applied")
	}
}
