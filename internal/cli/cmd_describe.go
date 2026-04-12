// cmd_describe.go implements the "describe" subcommand tree.
//
// Routes:
//
//	caractrl describe node <name>     — detailed Node output
//	caractrl describe project <name>  — detailed Project output
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/spf13/cobra"
)

// NewDescribeCmd returns the "describe" subcommand tree.
//
// Usage:
//
//	caractrl describe node <name>
//	caractrl describe project <name>
func NewDescribeCmd() *cobra.Command {
	describeCmd := &cobra.Command{
		Use:   "describe <resource-type> <name>",
		Short: "Show detailed information about a resource",
	}

	describeCmd.AddCommand(newDescribeNodeCmd())
	describeCmd.AddCommand(newDescribeProjectCmd())
	return describeCmd
}

func newDescribeNodeCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "node <name>",
		Short:   "Show detailed information about a node",
		Aliases: []string{"nodes"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			serverURL, _ := cmd.Root().PersistentFlags().GetString("server")

			client := NewClient(serverURL)
			ctx := context.Background()

			node, err := client.GetNode(ctx, args[0])
			if err != nil {
				return fmt.Errorf("describe node %q: %w", args[0], err)
			}

			describeNode(os.Stdout, &node)
			return nil
		},
	}
}

func newDescribeProjectCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "project <name>",
		Short:   "Show detailed information about a project",
		Aliases: []string{"projects"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			serverURL, _ := cmd.Root().PersistentFlags().GetString("server")

			client := NewClient(serverURL)
			ctx := context.Background()

			project, err := client.GetProject(ctx, args[0])
			if err != nil {
				return fmt.Errorf("describe project %q: %w", args[0], err)
			}

			describeProject(os.Stdout, &project)
			return nil
		},
	}
}

// describeNode writes a kubectl-style detailed view of a Node.
func describeNode(w io.Writer, node *v1.Node) {
	// Basic info
	printField(w, "Name", node.Name)
	printField(w, "Kind", node.Kind)
	printField(w, "Created", formatTimestamp(node.CreatedAt))

	// Labels
	printMapField(w, "Labels", node.Labels)

	// Annotations
	printMapField(w, "Annotations", node.Annotations)

	// Spec
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Spec:")
	printField(w, "  Hostname", stringOrNone(node.Spec.Hostname))
	printField(w, "  Unschedulable", fmt.Sprintf("%t", node.Spec.Unschedulable))

	// Status
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Status:")
	printField(w, "  State", stringOrNone(string(node.Status.State)))
	printField(w, "  Last Heartbeat", formatTimestamp(node.Status.LastHeartbeat))

	// Info
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Info:")
	printField(w, "  Kernel Version", stringOrNone(node.Status.Info.KernelVersion))
	printField(w, "  OS Image", stringOrNone(node.Status.Info.OSImage))
	printField(w, "  Agent Version", stringOrNone(node.Status.Info.AgentVersion))

	// Network
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Network:")
	printField(w, "  IP", stringOrNone(node.Status.Network.IP))
	printField(w, "  DNS Name", stringOrNone(node.Status.Network.DNSName))
	printField(w, "  Mode", stringOrNone(string(node.Status.Network.Mode)))
	if node.Status.Network.AgentPort != 0 {
		printField(w, "  Agent Port", fmt.Sprintf("%d", node.Status.Network.AgentPort))
	} else {
		printField(w, "  Agent Port", "<none>")
	}

	// Throughput
	fmt.Fprintln(w, "  Throughput:")
	printField(w, "    Download", stringOrNone(node.Status.Network.Throughput.Download))
	printField(w, "    Upload", stringOrNone(node.Status.Network.Throughput.Upload))
	printField(w, "    Last Test", formatTimestamp(node.Status.Network.Throughput.LastTestTime))

	// Capacity
	fmt.Fprintln(w)
	printResourceList(w, "Capacity", node.Status.Capacity)

	// Allocatable
	printResourceList(w, "Allocatable", node.Status.Allocatable)

	// Conditions
	fmt.Fprintln(w)
	printConditions(w, node.Status.Conditions)
}

// describeProject writes a kubectl-style detailed view of a Project.
func describeProject(w io.Writer, project *v1.Project) {
	// Basic info
	printField(w, "Name", project.Name)
	printField(w, "Kind", project.Kind)
	printField(w, "Created", formatTimestamp(project.CreatedAt))

	// Labels
	printMapField(w, "Labels", project.Labels)

	// Annotations
	printMapField(w, "Annotations", project.Annotations)

	// Spec
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Spec:")

	// Services
	fmt.Fprintln(w, "  Services:")
	if len(project.Spec.Services) == 0 {
		fmt.Fprintln(w, "    <none>")
	} else {
		for _, svc := range project.Spec.Services {
			fmt.Fprintf(w, "    - Name:    %s\n", svc.Name)
			fmt.Fprintf(w, "      Image:   %s\n", svc.Image)

			if len(svc.Env) > 0 {
				fmt.Fprintln(w, "      Env:")
				for _, env := range svc.Env {
					fmt.Fprintf(w, "        %s=%s\n", env.Name, env.Value)
				}
			}

			if len(svc.VolumeMounts) > 0 {
				fmt.Fprintln(w, "      Volume Mounts:")
				for _, vm := range svc.VolumeMounts {
					fmt.Fprintf(w, "        %s -> %s\n", vm.Name, vm.MountPath)
				}
			}
		}
	}

	// Volumes
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Volumes:")
	if len(project.Spec.Volumes) == 0 {
		fmt.Fprintln(w, "    <none>")
	} else {
		for _, vol := range project.Spec.Volumes {
			fmt.Fprintf(w, "    - Name:   %s\n", vol.Name)
			fmt.Fprintf(w, "      Type:   %s\n", vol.Type)
		}
	}

	// Ingress
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Ingress:")
	if len(project.Spec.Ingress) == 0 {
		fmt.Fprintln(w, "    <none>")
	} else {
		for _, ing := range project.Spec.Ingress {
			fmt.Fprintf(w, "    - Name:    %s\n", ing.Name)
			fmt.Fprintf(w, "      Host:    %s\n", stringOrNone(ing.Host))
			fmt.Fprintf(w, "      Target:  %s:%d\n", ing.Target.Service, ing.Target.Port)
			fmt.Fprintf(w, "      Scope:   %s\n", stringOrNone(string(ing.Access.Scope)))
		}
	}

	// ExpireAt
	fmt.Fprintln(w)
	if project.Spec.ExpireAt != nil {
		printField(w, "  Expire At", formatTimestamp(*project.Spec.ExpireAt))
	} else {
		printField(w, "  Expire At", "<none>")
	}

	// Status
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Status:")
	printField(w, "  Phase", stringOrNone(string(project.Status.Phase)))
	printField(w, "  Node", stringOrNone(project.Status.NodeRef))

	// Conditions
	fmt.Fprintln(w)
	printConditions(w, project.Status.Conditions)
}

// --- helpers ----------------------------------------------------------------

// formatTimestamp returns "2006-01-02T15:04:05Z (3d ago)" or "<none>" for zero.
func formatTimestamp(t time.Time) string {
	if t.IsZero() {
		return "<none>"
	}
	return fmt.Sprintf("%s (%s ago)", t.UTC().Format(time.RFC3339), humanAge(t))
}

// stringOrNone returns s if non-empty, otherwise "<none>".
func stringOrNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}

// printField writes a single "Key:  value" line with consistent alignment.
func printField(w io.Writer, key, value string) {
	fmt.Fprintf(w, "%s:\t%s\n", key, value)
}

// printMapField writes a map as sorted key=value pairs, or "<none>" if empty.
func printMapField(w io.Writer, label string, m map[string]string) {
	if len(m) == 0 {
		printField(w, label, "<none>")
		return
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, m[k]))
	}
	printField(w, label, strings.Join(parts, ", "))
}

// printResourceList writes a ResourceList section (e.g. Capacity, Allocatable).
func printResourceList(w io.Writer, label string, rl v1.ResourceList) {
	fmt.Fprintf(w, "%s:\n", label)
	if len(rl) == 0 {
		fmt.Fprintln(w, "  <none>")
		return
	}

	keys := make([]string, 0, len(rl))
	for k := range rl {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		fmt.Fprintf(w, "  %s:\t%s\n", k, rl[k])
	}
}

// printConditions writes the Conditions table using tabwriter.
func printConditions(w io.Writer, conditions []v1.Condition) {
	fmt.Fprintln(w, "Conditions:")
	if len(conditions) == 0 {
		fmt.Fprintln(w, "  <none>")
		return
	}

	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "  TYPE\tSTATUS\tLAST HEARTBEAT\tLAST TRANSITION\tREASON\tMESSAGE")
	fmt.Fprintln(tw, "  ----\t------\t--------------\t---------------\t------\t-------")

	for _, c := range conditions {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n",
			c.Type,
			c.Status,
			formatConditionTime(c.LastHeartbeatTime),
			formatConditionTime(c.LastTransitionTime),
			stringOrNone(c.Reason),
			stringOrNone(c.Message),
		)
	}
	_ = tw.Flush()
}

// formatConditionTime returns a compact RFC3339 timestamp or "<none>" for zero.
func formatConditionTime(t time.Time) string {
	if t.IsZero() {
		return "<none>"
	}
	return t.UTC().Format(time.RFC3339)
}
