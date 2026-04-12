package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

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
	var filename string

	applyCmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply a resource manifest from a file or stdin",
		Long: `Apply creates or updates a resource defined in a YAML or JSON manifest.
The kind field in the manifest determines which API endpoint is called.

Supported kinds: Node, Project

Examples:
  caractrl apply -f node.yaml
  caractrl apply -f project.yaml
  cat project.yaml | caractrl apply -f -`,
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
				return nil
			}
			return printer.PrintAny(result.Resource)
		},
	}

	applyCmd.Flags().StringVarP(&filename, "filename", "f", "", "manifest file to apply (\"-\" for stdin)")
	_ = applyCmd.MarkFlagRequired("filename")

	return applyCmd
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
	default:
		fmt.Fprintln(cmd.OutOrStdout(), "resource applied")
	}
}
