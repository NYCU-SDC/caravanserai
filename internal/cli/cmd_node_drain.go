package cli

import (
	"context"
	"fmt"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/spf13/cobra"
)

// nodeScheduling is what cordon, drain and uncordon set on a Node's spec.
type nodeScheduling struct {
	unschedulable bool
	drain         bool
}

func newNodeCordonCmd() *cobra.Command {
	return newNodeSchedulingCmd("cordon", "Stop new projects from being placed on a node",
		`cordon marks a node unschedulable: no new project is placed on it, and the
projects already there keep running. Undo it with uncordon.`,
		nodeScheduling{unschedulable: true}, "cordoned")
}

func newNodeDrainCmd() *cobra.Command {
	return newNodeSchedulingCmd("drain", "Move every project off a node so it can be powered off",
		`drain cordons a node and moves every project off it, one planned move at a
time: each is stopped, backed up first if it has Managed data, and placed on
another node by the scheduler. Nothing written before the move is lost.

Follow the progress with 'caractl describe node <name>'. It reads Drained once
no project is left on the node and the node has cleaned up after them; the
node can then be powered off. Incomplete lists any project that cannot leave
without help, and why.

Undo it with uncordon.`,
		nodeScheduling{unschedulable: true, drain: true}, "draining")
}

func newNodeUncordonCmd() *cobra.Command {
	return newNodeSchedulingCmd("uncordon", "Let a node take new projects again, calling off any drain",
		`uncordon lets a node take new projects again. If it is being drained, the
drain is called off: moves that have not yet stopped their project are
cancelled and those projects keep running here; moves past that point finish.`,
		nodeScheduling{}, "uncordoned")
}

func newNodeSchedulingCmd(use, short, long string, want nodeScheduling, done string) *cobra.Command {
	return &cobra.Command{
		Use:   use + " <name>",
		Short: short,
		Long:  long,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			serverURL, _ := cmd.Root().PersistentFlags().GetString("server")
			name := args[0]
			if err := setNodeScheduling(cmd.Context(), NewClient(serverURL), name, want); err != nil {
				return fmt.Errorf("%s node %q: %w", use, name, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "node/%s %s\n", name, done)
			return nil
		},
	}
}

// setNodeScheduling reads the node, sets Unschedulable and Drain and writes it
// back. The write replaces the node's spec and labels, so it is built from the
// node as read to keep everything else — its tier, its hostname — unchanged.
func setNodeScheduling(ctx context.Context, client *Client, name string, want nodeScheduling) error {
	node, err := client.GetNode(ctx, name)
	if err != nil {
		return err
	}
	node.Spec.Unschedulable = want.unschedulable
	node.Spec.Drain = want.drain
	_, err = client.UpdateNode(ctx, node)
	return err
}

// describeNodeDrain summarises a drain in progress for describe, e.g.
// "Incomplete (2 left)".
func describeNodeDrain(spec v1.NodeSpec, status *v1.NodeDrainStatus) string {
	switch {
	case status != nil:
		s := string(status.Phase)
		if status.Remaining > 0 {
			s += fmt.Sprintf(" (%d left)", status.Remaining)
		}
		return s
	case spec.Drain:
		return "Requested"
	default:
		return "<none>"
	}
}
