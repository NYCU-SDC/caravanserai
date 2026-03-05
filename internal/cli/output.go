package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"gopkg.in/yaml.v3"
)

// Printer writes resource output in the user-selected format.
type Printer struct {
	Format string // "table" | "json" | "yaml"
	Out    io.Writer
}

// PrintNodeList renders a NodeList in the configured format.
func (p *Printer) PrintNodeList(list v1.NodeList) error {
	switch p.Format {
	case "json":
		return printJSON(p.Out, list)
	case "yaml":
		return printYAML(p.Out, list)
	default:
		return p.printNodeTable(list.Items)
	}
}

// PrintNode renders a single Node in the configured format.
func (p *Printer) PrintNode(node v1.Node) error {
	switch p.Format {
	case "json":
		return printJSON(p.Out, node)
	case "yaml":
		return printYAML(p.Out, node)
	default:
		return p.printNodeTable([]v1.Node{node})
	}
}

// PrintAny renders an arbitrary value (used by apply) in the configured format.
func (p *Printer) PrintAny(v any) error {
	switch p.Format {
	case "json":
		return printJSON(p.Out, v)
	case "yaml":
		return printYAML(p.Out, v)
	default:
		// For apply, a single-line confirmation is enough in table mode.
		// Attempt to cast to known types for a nicer message.
		switch res := v.(type) {
		case v1.Node:
			return p.PrintNode(res)
		case v1.Project:
			return p.PrintProject(res)
		default:
			return printJSON(p.Out, res)
		}
	}
}

// PrintProjectList renders a ProjectList in the configured format.
func (p *Printer) PrintProjectList(list v1.ProjectList) error {
	switch p.Format {
	case "json":
		return printJSON(p.Out, list)
	case "yaml":
		return printYAML(p.Out, list)
	default:
		return p.printProjectTable(list.Items)
	}
}

// PrintProject renders a single Project in the configured format.
func (p *Printer) PrintProject(project v1.Project) error {
	switch p.Format {
	case "json":
		return printJSON(p.Out, project)
	case "yaml":
		return printYAML(p.Out, project)
	default:
		return p.printProjectTable([]v1.Project{project})
	}
}

// printProjectTable writes a human-readable table with columns:
// NAME  PHASE  NODE  CONDITIONS  AGE
func (p *Printer) printProjectTable(projects []v1.Project) error {
	w := tabwriter.NewWriter(p.Out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tPHASE\tNODE\tCONDITIONS\tAGE")

	for _, proj := range projects {
		name := proj.Name
		phase := string(proj.Status.Phase)
		if phase == "" {
			phase = "<unknown>"
		}
		node := proj.Status.NodeRef
		if node == "" {
			node = "<none>"
		}
		conditions := latestConditionReason(proj.Status.Conditions)
		age := humanAge(proj.CreatedAt)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", name, phase, node, conditions, age)
	}

	return w.Flush()
}

// latestConditionReason returns the Reason of the last condition in the slice,
// or "-" if there are none.
func latestConditionReason(conditions []v1.Condition) string {
	if len(conditions) == 0 {
		return "-"
	}
	return conditions[len(conditions)-1].Reason
}

// printNodeTable writes a human-readable table with columns:
// NAME  STATE  IP  AGE
func (p *Printer) printNodeTable(nodes []v1.Node) error {
	w := tabwriter.NewWriter(p.Out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATE\tIP\tAGE")

	for _, n := range nodes {
		name := n.Name
		state := string(n.Status.State)
		if state == "" {
			state = "<unknown>"
		}
		ip := n.Status.Network.IP
		if ip == "" {
			ip = "<none>"
		}
		age := humanAge(n.CreatedAt)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", name, state, ip, age)
	}

	return w.Flush()
}

// humanAge returns a compact human-readable duration since t.
// Returns "<unknown>" if t is the zero value.
func humanAge(t time.Time) string {
	if t.IsZero() {
		return "<unknown>"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("json encode: %w", err)
	}
	return nil
}

func printYAML(w io.Writer, v any) error {
	// Round-trip through JSON first so yaml.v3 sees plain Go maps/structs
	// rather than custom marshaler surprises.
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("json marshal: %w", err)
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return fmt.Errorf("json unmarshal: %w", err)
	}
	out, err := yaml.Marshal(generic)
	if err != nil {
		return fmt.Errorf("yaml marshal: %w", err)
	}
	_, err = fmt.Fprint(w, strings.TrimRight(string(out), "\n")+"\n")
	return err
}
