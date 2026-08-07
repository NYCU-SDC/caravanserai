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
	// A Secret must never be printed with real values in any format. Route it
	// through PrintSecret, which redacts before json/yaml/table output.
	if secret, ok := v.(v1.Secret); ok {
		return p.PrintSecret(secret)
	}

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

// redactedValue is printed in place of every real Secret value in CLI output.
const redactedValue = "<redacted>"

// redactSecret returns a copy of s with every data value replaced by
// redactedValue. The copy is deep over the Data slice so the caller's original
// (which still holds real values) is never mutated. Used by every Secret print
// path so no CLI output format ever emits a real value.
func redactSecret(s v1.Secret) v1.Secret {
	redacted := s
	redacted.Spec.Data = make([]v1.SecretDataItem, len(s.Spec.Data))
	for i, item := range s.Spec.Data {
		redacted.Spec.Data[i] = v1.SecretDataItem{Key: item.Key, Value: redactedValue}
	}
	return redacted
}

// PrintSecretList renders a SecretList in the configured format, always with
// values redacted.
func (p *Printer) PrintSecretList(list v1.SecretList) error {
	switch p.Format {
	case "json", "yaml":
		redacted := list
		redacted.Items = make([]v1.Secret, len(list.Items))
		for i, s := range list.Items {
			redacted.Items[i] = redactSecret(s)
		}
		if p.Format == "yaml" {
			return printYAML(p.Out, redacted)
		}
		return printJSON(p.Out, redacted)
	default:
		return p.printSecretTable(list.Items)
	}
}

// PrintSecret renders a single Secret in the configured format, always with
// values redacted.
func (p *Printer) PrintSecret(secret v1.Secret) error {
	switch p.Format {
	case "json":
		return printJSON(p.Out, redactSecret(secret))
	case "yaml":
		return printYAML(p.Out, redactSecret(secret))
	default:
		return p.printSecretTable([]v1.Secret{secret})
	}
}

// printSecretTable writes a human-readable table with columns:
// NAME  KEYS  AGE — key names only, never values.
func (p *Printer) printSecretTable(secrets []v1.Secret) error {
	w := tabwriter.NewWriter(p.Out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tKEYS\tAGE")

	for _, s := range secrets {
		keys := make([]string, len(s.Spec.Data))
		for i, item := range s.Spec.Data {
			keys[i] = item.Key
		}
		keyList := strings.Join(keys, ",")
		if keyList == "" {
			keyList = "<none>"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", s.Name, keyList, humanAge(s.CreatedAt))
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
// NAME  STATE  OVERLAY IP  AGE
func (p *Printer) printNodeTable(nodes []v1.Node) error {
	w := tabwriter.NewWriter(p.Out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATE\tOVERLAY IP\tAGE")

	for _, n := range nodes {
		name := n.Name
		state := string(n.Status.State)
		if state == "" {
			state = "<unknown>"
		}
		ip := n.Status.Network.OverlayIP
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
